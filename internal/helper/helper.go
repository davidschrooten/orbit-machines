// Package helper implements the Orbit Machine interactive helper CLI and state
// manager (repository cloning, Nix flake initialization & direnv auto-loading,
// multi-tool AI/CLI configuration, Git identity & SSH key management, and profile persistence).
package helper

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/nixenv"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
	"github.com/davidschrooten/orbit-machines/internal/shellinit"
	"github.com/davidschrooten/orbit-machines/internal/tui/flakewizard"
)

// SupportedTools lists all developer and AI tools configurable by orbit-helper.
var SupportedTools = []string{"claude", "agy", "opencode", "gh", "git"}

// GitCloner clones or updates a Git repository into destDir.
type GitCloner func(ctx context.Context, cloneURL, destDir, branch string) error

// DirenvAllower marks a repository directory's .envrc as trusted in direnv.
type DirenvAllower func(ctx context.Context, dir string) error

// OrbitContext holds workspace, user, MCP, and repository metadata synced from Orbit.
type OrbitContext struct {
	OrbitURL      string                    `json:"orbit_url"`
	OrbitMCPToken string                    `json:"orbit_mcp_token"`
	WorkspaceID   string                    `json:"workspace_id"`
	WorkspaceSlug string                    `json:"workspace_slug"`
	UserID        string                    `json:"user_id"`
	UserEmail     string                    `json:"user_email"`
	Repositories  []protocol.RepoCredential `json:"repositories"`
}

// GitConfig stores persisted Git author and branch configuration.
type GitConfig struct {
	UserName      string `json:"user_name,omitempty"`
	UserEmail     string `json:"user_email,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

// SSHKeyConfig stores the user's persisted Ed25519 SSH keypair across Pod restarts.
type SSHKeyConfig struct {
	PrivateKey string `json:"private_key,omitempty"`
	PublicKey  string `json:"public_key,omitempty"`
}

// State records persisted actions performed by orbit-helper in ~/.config/orbit/state.json.
type State struct {
	ClonedRepos       []string      `json:"cloned_repos"`
	FlakeEnabledRepos []string      `json:"flake_enabled_repos"`
	ConfiguredTools   []string      `json:"configured_tools"`
	Git               *GitConfig    `json:"git,omitempty"`
	SSHKey            *SSHKeyConfig `json:"ssh_key,omitempty"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

// PersistedState is an alias for State.
type PersistedState = State

// Helper is an alias for Manager.
type Helper = Manager

// CloneOptions configures repository cloning and flake setup.
type CloneOptions struct {
	CloneAll         bool
	NamesOrURLs      []string
	InitMissingFlake bool
}

// StatusReport summarizes the current Orbit Machine helper state.
type StatusReport struct {
	Context       OrbitContext `json:"context"`
	State         State        `json:"state"`
	WorkspaceDirs []string     `json:"workspace_dirs"`
	SSHPublicKey  string       `json:"ssh_public_key,omitempty"`
}

// Format renders a human-readable summary of the StatusReport.
func (r StatusReport) Format() string {
	var b strings.Builder
	b.WriteString("🪐 Orbit Helper Status\n")
	b.WriteString("────────────────────────────────────────────────────\n")
	_, _ = fmt.Fprintf(&b, "  Orbit URL:        %s\n", fallbackStr(r.Context.OrbitURL, "(not set)"))
	_, _ = fmt.Fprintf(&b, "  Workspace:        %s (%s)\n", fallbackStr(r.Context.WorkspaceSlug, "default"), fallbackStr(r.Context.WorkspaceID, "-"))
	_, _ = fmt.Fprintf(&b, "  User:             %s (%s)\n", fallbackStr(r.Context.UserEmail, "orbit"), fallbackStr(r.Context.UserID, "-"))
	mcpStatus := "not configured"
	if strings.TrimSpace(r.Context.OrbitMCPToken) != "" {
		mcpStatus = "connected (token active)"
	}
	_, _ = fmt.Fprintf(&b, "  Orbit MCP:        %s\n", mcpStatus)
	gitSummary := "(default)"
	if r.State.Git != nil && r.State.Git.UserName != "" {
		branch := fallbackStr(r.State.Git.DefaultBranch, "main")
		gitSummary = fmt.Sprintf("%s <%s> (branch: %s)", r.State.Git.UserName, r.State.Git.UserEmail, branch)
	}
	_, _ = fmt.Fprintf(&b, "  Git Identity:     %s\n", gitSummary)
	sshSummary := "(not configured — run 'orbit ssh-key')"
	if pub := strings.TrimSpace(r.SSHPublicKey); pub != "" {
		sshSummary = pub
	}
	_, _ = fmt.Fprintf(&b, "  Git SSH Key:      %s\n", sshSummary)
	_, _ = fmt.Fprintf(&b, "  Orbit Repos:      %d available\n", len(r.Context.Repositories))
	_, _ = fmt.Fprintf(&b, "  Cloned in WS:     %s\n", fallbackSlice(r.WorkspaceDirs, "(none)"))
	_, _ = fmt.Fprintf(&b, "  Flake Auto-Load:  %s\n", fallbackSlice(r.State.FlakeEnabledRepos, "(none)"))
	_, _ = fmt.Fprintf(&b, "  Configured Tools: %s\n", fallbackSlice(r.State.ConfiguredTools, "(none)"))
	return b.String()
}

// Config configures a helper Manager.
type Config struct {
	HomeDir       string
	WorkDir       string
	MachinedURL   string
	GitCloner     GitCloner
	DirenvAllower DirenvAllower
	HTTPClient    *http.Client
}

// Manager coordinates Orbit context persistence, repo cloning, Nix flake setup, Git/SSH keys, and tool wiring.
type Manager struct {
	homeDir       string
	workDir       string
	configDir     string
	machinedURL   string
	gitCloner     GitCloner
	direnvAllower DirenvAllower
	httpClient    *http.Client
}

// NewManager validates cfg, initializes shell and Git/SSH configuration, and constructs a Manager.
func NewManager(cfg Config) (*Manager, error) {
	homeDir := strings.TrimSpace(cfg.HomeDir)
	workDir := strings.TrimSpace(cfg.WorkDir)
	if homeDir == "" || workDir == "" {
		return nil, fmt.Errorf("%w: HomeDir and WorkDir are required", domain.ErrInvalidConfig)
	}
	if err := shellinit.EnsureInitialized(homeDir, workDir); err != nil {
		return nil, err
	}

	machinedURL := strings.TrimRight(strings.TrimSpace(cfg.MachinedURL), "/")
	if machinedURL == "" {
		machinedURL = "http://127.0.0.1:8080"
	}
	cloner := cfg.GitCloner
	if cloner == nil {
		cloner = defaultGitCloner
	}
	allower := cfg.DirenvAllower
	if allower == nil {
		allower = defaultDirenvAllower
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	m := &Manager{
		homeDir:       homeDir,
		workDir:       workDir,
		configDir:     filepath.Join(homeDir, ".config", "orbit"),
		machinedURL:   machinedURL,
		gitCloner:     cloner,
		direnvAllower: allower,
		httpClient:    httpClient,
	}
	_ = m.EnsureGitAndSSH()
	return m, nil
}

// SaveContext writes context.json and env.sh under ~/.config/orbit.
func (m *Manager) SaveContext(ctxData OrbitContext) error {
	existing, _ := m.LoadContext()
	ctxData.OrbitURL = fallbackStr(ctxData.OrbitURL, existing.OrbitURL)
	ctxData.OrbitMCPToken = fallbackStr(ctxData.OrbitMCPToken, existing.OrbitMCPToken)
	ctxData.WorkspaceID = fallbackStr(ctxData.WorkspaceID, existing.WorkspaceID)
	ctxData.WorkspaceSlug = fallbackStr(ctxData.WorkspaceSlug, existing.WorkspaceSlug)
	ctxData.UserID = fallbackStr(ctxData.UserID, existing.UserID)
	ctxData.UserEmail = fallbackStr(ctxData.UserEmail, existing.UserEmail)
	if len(ctxData.Repositories) == 0 {
		ctxData.Repositories = existing.Repositories
	}

	raw, _ := json.MarshalIndent(ctxData, "", "  ")
	if err := writeConfigFile(filepath.Join(m.configDir, "context.json"), raw, 0o600); err != nil {
		return err
	}
	return m.writeEnvSh(ctxData)
}

// LoadContext reads ~/.config/orbit/context.json and merges any ORBIT_* environment variables.
func (m *Manager) LoadContext() (OrbitContext, error) {
	var out OrbitContext
	if data, err := os.ReadFile(filepath.Join(m.configDir, "context.json")); err == nil {
		_ = json.Unmarshal(data, &out)
	}

	out.OrbitURL = fallbackStr(out.OrbitURL, os.Getenv("ORBIT_URL"))
	out.OrbitMCPToken = fallbackStr(out.OrbitMCPToken, os.Getenv("ORBIT_MCP_TOKEN"))
	out.WorkspaceID = fallbackStr(out.WorkspaceID, os.Getenv("ORBIT_MACHINED_WORKSPACE_ID"))
	out.WorkspaceSlug = fallbackStr(out.WorkspaceSlug, os.Getenv("ORBIT_WORKSPACE_SLUG"))
	out.UserID = fallbackStr(out.UserID, os.Getenv("ORBIT_MACHINED_USER_ID"))
	out.UserEmail = fallbackStr(out.UserEmail, fallbackStr(os.Getenv("ORBIT_USER_EMAIL"), os.Getenv("ORBIT_USER_LOGIN")))
	if rawRepos := strings.TrimSpace(os.Getenv("ORBIT_REPOSITORIES_JSON")); rawRepos != "" && len(out.Repositories) == 0 {
		_ = json.Unmarshal([]byte(rawRepos), &out.Repositories)
	}
	return out, nil
}

// LoadState reads ~/.config/orbit/state.json (falling back to helper-state.json).
func (m *Manager) LoadState() (State, error) {
	var st State
	if data, err := os.ReadFile(filepath.Join(m.configDir, "state.json")); err == nil {
		_ = json.Unmarshal(data, &st)
		return st, nil
	}
	if data, err := os.ReadFile(filepath.Join(m.configDir, "helper-state.json")); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st, nil
}

// SaveState writes ~/.config/orbit/state.json and ~/.config/orbit/helper-state.json.
func (m *Manager) SaveState(st State) error {
	st.UpdatedAt = time.Now().UTC()
	raw, _ := json.MarshalIndent(st, "", "  ")
	_ = writeConfigFile(filepath.Join(m.configDir, "helper-state.json"), raw, 0o600)
	return writeConfigFile(filepath.Join(m.configDir, "state.json"), raw, 0o600)
}

// ConfigureGit writes ~/.gitconfig with the given identity and default branch and persists it in State.
func (m *Manager) ConfigureGit(name, email, defaultBranch string) error {
	name = strings.TrimSpace(name)
	email = strings.TrimSpace(email)
	defaultBranch = strings.TrimSpace(defaultBranch)
	if name == "" || email == "" {
		return fmt.Errorf("%w: git user.name and user.email are required", domain.ErrInvalidConfig)
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	gitCfg := formatGitConfig(name, email, defaultBranch)
	if err := writeConfigFile(filepath.Join(m.homeDir, ".gitconfig"), []byte(gitCfg), 0o644); err != nil {
		return err
	}

	st, _ := m.LoadState()
	st.Git = &GitConfig{
		UserName:      name,
		UserEmail:     email,
		DefaultBranch: defaultBranch,
	}
	st.ConfiguredTools = appendUnique(st.ConfiguredTools, "git")
	return m.SaveState(st)
}

// EnsureGitAndSSH ensures ~/.gitconfig is initialized (restoring from State.Git or Orbit user defaults)
// and restores ~/.ssh/id_ed25519, ~/.ssh/id_ed25519.pub, and ~/.ssh/config if saved in State.SSHKey.
func (m *Manager) EnsureGitAndSSH() error {
	st, _ := m.LoadState()
	orbitCtx, _ := m.LoadContext()

	gitConfigPath := filepath.Join(m.homeDir, ".gitconfig")
	if _, err := os.Stat(gitConfigPath); os.IsNotExist(err) {
		name, email, branch := m.defaultGitIdentity(st, orbitCtx)
		gitCfg := formatGitConfig(name, email, branch)
		if err := writeConfigFile(gitConfigPath, []byte(gitCfg), 0o644); err != nil {
			return err
		}
		if st.Git == nil {
			st.Git = &GitConfig{
				UserName:      name,
				UserEmail:     email,
				DefaultBranch: branch,
			}
			_ = m.SaveState(st)
		}
	}

	if st.SSHKey != nil && strings.TrimSpace(st.SSHKey.PrivateKey) != "" {
		privPath := filepath.Join(m.homeDir, ".ssh", "id_ed25519")
		if _, err := os.Stat(privPath); os.IsNotExist(err) {
			if err := m.writeSSHFiles(st.SSHKey.PrivateKey, st.SSHKey.PublicKey); err != nil {
				return err
			}
		}
	}
	return nil
}

// GenerateSSHKey generates a fresh Ed25519 SSH keypair in pure Go, writes ~/.ssh/id_ed25519,
// ~/.ssh/id_ed25519.pub, and ~/.ssh/config, and persists the keypair in State.
func (m *Manager) GenerateSSHKey(comment string) (SSHKeyConfig, error) {
	comment = strings.TrimSpace(comment)
	if comment == "" {
		orbitCtx, _ := m.LoadContext()
		_, email, _ := m.defaultGitIdentity(State{}, orbitCtx)
		comment = email
	}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	privPEM := marshalOpenSSHEd25519PrivateKey(priv, comment)
	pubAuth := formatOpenSSHEd25519PublicKey(pub, comment)

	if err := m.writeSSHFiles(privPEM, pubAuth); err != nil {
		return SSHKeyConfig{}, err
	}

	cfg := SSHKeyConfig{
		PrivateKey: privPEM,
		PublicKey:  strings.TrimSpace(pubAuth),
	}
	st, _ := m.LoadState()
	st.SSHKey = &cfg
	_ = m.SaveState(st)
	return cfg, nil
}

// SetSSHKey validates an existing SSH private key PEM, derives its public key, writes
// ~/.ssh/id_ed25519, ~/.ssh/id_ed25519.pub, and ~/.ssh/config, and persists it in State.
func (m *Manager) SetSSHKey(privateKeyPEM string) (SSHKeyConfig, error) {
	trimmed := strings.TrimSpace(privateKeyPEM)
	if trimmed == "" {
		return SSHKeyConfig{}, fmt.Errorf("%w: SSH private key is required", domain.ErrInvalidConfig)
	}
	normalizedPriv := trimmed + "\n"

	pubAuth, err := deriveSSHPublicKey(normalizedPriv)
	if err != nil {
		return SSHKeyConfig{}, err
	}

	if err := m.writeSSHFiles(normalizedPriv, pubAuth); err != nil {
		return SSHKeyConfig{}, err
	}

	cfg := SSHKeyConfig{
		PrivateKey: normalizedPriv,
		PublicKey:  strings.TrimSpace(pubAuth),
	}
	st, _ := m.LoadState()
	st.SSHKey = &cfg
	_ = m.SaveState(st)
	return cfg, nil
}

// PublicSSHKey returns the current SSH public key from ~/.ssh/id_ed25519.pub or State.
func (m *Manager) PublicSSHKey() (string, error) {
	pubPath := filepath.Join(m.homeDir, ".ssh", "id_ed25519.pub")
	if data, err := os.ReadFile(pubPath); err == nil && strings.TrimSpace(string(data)) != "" {
		return strings.TrimSpace(string(data)), nil
	}
	st, _ := m.LoadState()
	if st.SSHKey != nil && strings.TrimSpace(st.SSHKey.PublicKey) != "" {
		return strings.TrimSpace(st.SSHKey.PublicKey), nil
	}
	return "", fmt.Errorf("%w: no SSH key configured; run 'orbit ssh-key --generate'", domain.ErrInvalidConfig)
}

func (m *Manager) writeSSHFiles(privPEM, pubAuth string) error {
	sshDir := filepath.Join(m.homeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("creating ~/.ssh: %w", err)
	}
	privPath := filepath.Join(sshDir, "id_ed25519")
	if err := os.WriteFile(privPath, []byte(strings.TrimSpace(privPEM)+"\n"), 0o600); err != nil {
		return fmt.Errorf("writing id_ed25519: %w", err)
	}
	if strings.TrimSpace(pubAuth) != "" {
		pubPath := filepath.Join(sshDir, "id_ed25519.pub")
		_ = os.WriteFile(pubPath, []byte(strings.TrimSpace(pubAuth)+"\n"), 0o644)
	}
	sshConfigPath := filepath.Join(sshDir, "config")
	sshConfig := fmt.Sprintf("Host *\n  IdentityFile %s\n  IdentitiesOnly yes\n  StrictHostKeyChecking accept-new\n", privPath)
	return os.WriteFile(sshConfigPath, []byte(sshConfig), 0o600)
}

func (m *Manager) defaultGitIdentity(st State, orbitCtx OrbitContext) (string, string, string) {
	if st.Git != nil && strings.TrimSpace(st.Git.UserName) != "" && strings.TrimSpace(st.Git.UserEmail) != "" {
		return strings.TrimSpace(st.Git.UserName), strings.TrimSpace(st.Git.UserEmail), fallbackStr(st.Git.DefaultBranch, "main")
	}
	rawUser := strings.TrimSpace(fallbackStr(os.Getenv("ORBIT_USER_LOGIN"), orbitCtx.UserEmail))
	rawEmail := strings.TrimSpace(fallbackStr(orbitCtx.UserEmail, os.Getenv("ORBIT_USER_EMAIL")))
	if strings.Contains(rawEmail, "@") {
		name := rawUser
		if name == "" || strings.Contains(name, "@") {
			name = rawEmail[:strings.IndexByte(rawEmail, '@')]
		}
		return fallbackStr(name, "orbit"), rawEmail, "main"
	}
	login := fallbackStr(rawUser, fallbackStr(orbitCtx.UserID, "orbit"))
	return login, login + "@users.noreply.orbit.local", "main"
}

func formatGitConfig(name, email, branch string) string {
	branch = fallbackStr(branch, "main")
	return fmt.Sprintf("[user]\n\tname = %s\n\temail = %s\n[init]\n\tdefaultBranch = %s\n[pull]\n\trebase = false\n[safe]\n\tdirectory = *\n[credential]\n\thelper = store\n", name, email, branch)
}

// CloneRepositories clones selected (or all) repositories into WorkDir and configures Nix flake auto-loading.
func (m *Manager) CloneRepositories(ctx context.Context, opts CloneOptions) ([]string, error) {
	orbitCtx, _ := m.LoadContext()

	var targets []protocol.RepoCredential
	if opts.CloneAll {
		targets = append(targets, orbitCtx.Repositories...)
	} else {
		for _, sel := range opts.NamesOrURLs {
			sel = strings.TrimSpace(sel)
			if sel == "" {
				continue
			}
			if matched, ok := matchRepo(orbitCtx.Repositories, sel); ok {
				targets = append(targets, matched)
			} else {
				targets = append(targets, protocol.RepoCredential{
					Name:     repoNameFromURL(sel),
					CloneURL: sel,
				})
			}
		}
	}

	if len(targets) == 0 {
		return nil, fmt.Errorf("%w: no repositories selected to clone", domain.ErrInvalidConfig)
	}

	st, _ := m.LoadState()
	var clonedNames []string

	for _, repo := range targets {
		name := fallbackStr(repo.Name, repoNameFromURL(repo.CloneURL))
		destDir := filepath.Join(m.workDir, name)
		authURL := buildAuthenticatedCloneURL(repo)

		if err := m.gitCloner(ctx, authURL, destDir, repo.DefaultBranch); err != nil {
			return clonedNames, fmt.Errorf("cloning %s: %w", name, err)
		}
		clonedNames = append(clonedNames, name)
		st.ClonedRepos = appendUnique(st.ClonedRepos, name)

		if nixenv.HasFlake(destDir) {
			if err := m.EnableFlakeAutoload(ctx, destDir); err != nil {
				return clonedNames, err
			}
			st.FlakeEnabledRepos = appendUnique(st.FlakeEnabledRepos, name)
		} else if opts.InitMissingFlake {
			if err := m.InitFlake(ctx, destDir, nil); err != nil {
				return clonedNames, err
			}
			st.FlakeEnabledRepos = appendUnique(st.FlakeEnabledRepos, name)
		}
	}

	_ = m.SaveState(st)
	return clonedNames, nil
}

// EnableFlakeAutoload writes `use flake` to <repoDir>/.envrc and runs `direnv allow`.
func (m *Manager) EnableFlakeAutoload(ctx context.Context, repoDir string) error {
	repoDir = strings.TrimSpace(repoDir)
	if repoDir == "" {
		return fmt.Errorf("%w: repoDir is required", domain.ErrInvalidConfig)
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return err
	}

	envrcPath := filepath.Join(repoDir, ".envrc")
	existing, _ := os.ReadFile(envrcPath)
	if !strings.Contains(string(existing), "use flake") {
		content := "use flake\n"
		if len(existing) > 0 {
			content = strings.TrimRight(string(existing), "\n") + "\nuse flake\n"
		}
		if err := os.WriteFile(envrcPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("writing .envrc: %w", err)
		}
	}

	if err := m.direnvAllower(ctx, repoDir); err != nil {
		return fmt.Errorf("direnv allow %s: %w", repoDir, err)
	}

	st, _ := m.LoadState()
	st.FlakeEnabledRepos = appendUnique(st.FlakeEnabledRepos, filepath.Base(repoDir))
	_ = m.SaveState(st)
	return nil
}

// InitFlake generates a flake.nix in repoDir (auto-detecting project stacks when packages is empty,
// and always including git, htop, curl, wget) and enables automatic flake loading via direnv.
func (m *Manager) InitFlake(ctx context.Context, repoDir string, packages []string) error {
	repoDir = strings.TrimSpace(repoDir)
	if repoDir == "" {
		return fmt.Errorf("%w: repoDir is required", domain.ErrInvalidConfig)
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return err
	}

	rawPkgs := cleanSlice(packages)
	var pkgs []string
	for _, p := range rawPkgs {
		if stackPkgs := flakewizard.StackPackages(p); len(stackPkgs) > 0 {
			for _, sp := range stackPkgs {
				pkgs = appendUnique(pkgs, sp)
			}
		} else {
			pkgs = appendUnique(pkgs, p)
		}
	}
	if len(pkgs) == 0 {
		wiz, _ := flakewizard.New(flakewizard.Config{
			RepoDir:  repoDir,
			RepoName: filepath.Base(repoDir),
		})
		pkgs = wiz.SelectedPackages()
	}
	for _, basePkg := range []string{"git", "htop", "curl", "wget"} {
		pkgs = appendUnique(pkgs, basePkg)
	}

	flakeContent := nixenv.GenerateFlakeNix(fmt.Sprintf("Orbit devshell for %s", filepath.Base(repoDir)), pkgs)
	if err := os.WriteFile(filepath.Join(repoDir, "flake.nix"), []byte(flakeContent), 0o644); err != nil {
		return fmt.Errorf("writing flake.nix: %w", err)
	}
	return m.EnableFlakeAutoload(ctx, repoDir)
}

// AutoloadAllWorkspaceFlakes scans WorkDir and enables direnv flake auto-loading for every repo with a flake.nix.
func (m *Manager) AutoloadAllWorkspaceFlakes(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(m.workDir)
	if err != nil {
		return nil, nil
	}

	var enabled []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		repoDir := filepath.Join(m.workDir, entry.Name())
		if nixenv.HasFlake(repoDir) {
			if err := m.EnableFlakeAutoload(ctx, repoDir); err != nil {
				return enabled, err
			}
			enabled = append(enabled, entry.Name())
		}
	}
	return enabled, nil
}

type fileSpec struct {
	data []byte
	perm os.FileMode
}

// ConfigureTools configures one or more AI and developer CLI tools ("claude", "agy", "opencode",
// "codex", "gemini", "cursor", "aider", "gh", "git", or "all") and persists the selections in state.json.
func (m *Manager) ConfigureTools(selected []string) ([]string, error) {
	tools := normalizeToolSelection(selected)
	if len(tools) == 0 {
		return nil, fmt.Errorf("%w: at least one tool must be selected (%s)", domain.ErrInvalidConfig, strings.Join(SupportedTools, ", "))
	}

	orbitCtx, _ := m.LoadContext()
	mcpServerURL := fallbackStr(strings.TrimRight(orbitCtx.OrbitURL, "/"), "http://localhost:3000") + "/mcp"

	mcpDoc := map[string]any{
		"mcpServers": map[string]any{
			"orbit": map[string]any{
				"type": "http",
				"url":  mcpServerURL,
				"headers": map[string]string{
					"Authorization": "Bearer " + orbitCtx.OrbitMCPToken,
				},
			},
		},
	}
	mcpBytes, _ := json.MarshalIndent(mcpDoc, "", "  ")

	st, _ := m.LoadState()
	var configured []string
	files := make(map[string]fileSpec)

	for _, tool := range tools {
		switch tool {
		case "claude":
			files[filepath.Join(m.homeDir, ".claude.json")] = fileSpec{mcpBytes, 0o600}
		case "agy":
			files[filepath.Join(m.homeDir, ".config", "antigravity", "mcp.json")] = fileSpec{mcpBytes, 0o600}
			files[filepath.Join(m.homeDir, ".gemini", "settings.json")] = fileSpec{mcpBytes, 0o600}
		case "gemini":
			files[filepath.Join(m.homeDir, ".gemini", "settings.json")] = fileSpec{mcpBytes, 0o600}
		case "opencode":
			files[filepath.Join(m.homeDir, ".config", "opencode", "config.json")] = fileSpec{mcpBytes, 0o600}
			files[filepath.Join(m.homeDir, ".config", "opencode", "opencode.json")] = fileSpec{mcpBytes, 0o600}
		case "codex":
			files[filepath.Join(m.homeDir, ".codex", "config.json")] = fileSpec{mcpBytes, 0o600}
		case "cursor":
			files[filepath.Join(m.homeDir, ".cursor", "mcp.json")] = fileSpec{mcpBytes, 0o600}
		case "aider":
			files[filepath.Join(m.homeDir, ".aider.conf.yml")] = fileSpec{[]byte("auto-commits: false\n"), 0o600}
		case "gh":
			ghYML := []byte("git_protocol: https\neditor: vim\nprompt: enabled\n")
			files[filepath.Join(m.homeDir, ".config", "gh", "config.yml")] = fileSpec{ghYML, 0o600}
		case "git":
			name, email, branch := m.defaultGitIdentity(st, orbitCtx)
			gitCfg := formatGitConfig(name, email, branch)
			files[filepath.Join(m.homeDir, ".gitconfig")] = fileSpec{[]byte(gitCfg), 0o644}
			st.Git = &GitConfig{
				UserName:      name,
				UserEmail:     email,
				DefaultBranch: branch,
			}
		default:
			return configured, fmt.Errorf("%w: unsupported tool %q", domain.ErrInvalidConfig, tool)
		}
		configured = append(configured, tool)
		st.ConfiguredTools = appendUnique(st.ConfiguredTools, tool)
	}

	for path, spec := range files {
		if err := writeConfigFile(path, spec.data, spec.perm); err != nil {
			return configured, err
		}
	}

	_ = m.SaveState(st)
	_ = m.writeEnvSh(orbitCtx)
	return configured, nil
}

// Status inspects the current Orbit context, helper state, SSH key, and workspace directories.
func (m *Manager) Status() (StatusReport, error) {
	orbitCtx, _ := m.LoadContext()
	st, _ := m.LoadState()

	var dirs []string
	if entries, err := os.ReadDir(m.workDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, e.Name())
			}
		}
	}
	sort.Strings(dirs)

	pubKey, _ := m.PublicSSHKey()
	return StatusReport{
		Context:       orbitCtx,
		State:         st,
		WorkspaceDirs: dirs,
		SSHPublicKey:  pubKey,
	}, nil
}

// Save persists context.json, state.json, env.sh, and triggers an immediate profile/WIP snapshot on orbit-machined.
func (m *Manager) Save(ctx context.Context) error {
	orbitCtx, _ := m.LoadContext()
	_ = m.SaveContext(orbitCtx)
	st, _ := m.LoadState()
	_ = m.SaveState(st)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.machinedURL+"/api/v1/snapshot", nil)
	if err == nil {
		if resp, doErr := m.httpClient.Do(req); doErr == nil {
			_ = resp.Body.Close()
		}
	}
	return nil
}

// RunInteractiveGit prompts the user interactively for Git user.name, user.email, and defaultBranch.
func (m *Manager) RunInteractiveGit(in io.Reader, out io.Writer) error {
	return m.runInteractiveGitWithScanner(bufio.NewScanner(in), out)
}

func (m *Manager) runInteractiveGitWithScanner(scanner *bufio.Scanner, out io.Writer) error {
	st, _ := m.LoadState()
	orbitCtx, _ := m.LoadContext()
	defName, defEmail, defBranch := m.defaultGitIdentity(st, orbitCtx)

	_, _ = fmt.Fprintf(out, "Git user.name [%s]: ", defName)
	name := defName
	if scanner.Scan() && strings.TrimSpace(scanner.Text()) != "" {
		name = strings.TrimSpace(scanner.Text())
	}

	_, _ = fmt.Fprintf(out, "Git user.email [%s]: ", defEmail)
	email := defEmail
	if scanner.Scan() && strings.TrimSpace(scanner.Text()) != "" {
		email = strings.TrimSpace(scanner.Text())
	}

	_, _ = fmt.Fprintf(out, "Git default branch [%s]: ", defBranch)
	branch := defBranch
	if scanner.Scan() && strings.TrimSpace(scanner.Text()) != "" {
		branch = strings.TrimSpace(scanner.Text())
	}

	if err := m.ConfigureGit(name, email, branch); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "✓ Configured Git identity: %s <%s> (defaultBranch: %s)\n", name, email, branch)
	return nil
}

// RunInteractiveSSHKey runs an interactive prompt to generate, import, or display the user's Git SSH key.
func (m *Manager) RunInteractiveSSHKey(in io.Reader, out io.Writer) error {
	return m.runInteractiveSSHKeyWithScanner(bufio.NewScanner(in), out)
}

func (m *Manager) runInteractiveSSHKeyWithScanner(scanner *bufio.Scanner, out io.Writer) error {
	_, _ = fmt.Fprintln(out, "\n🔑 Git SSH Key Configuration")
	_, _ = fmt.Fprintln(out, "  [1] Generate new Ed25519 SSH keypair")
	_, _ = fmt.Fprintln(out, "  [2] Paste/import existing SSH private key")
	_, _ = fmt.Fprintln(out, "  [3] Show current SSH public key")
	_, _ = fmt.Fprint(out, "Select an option [1-3]: ")

	choice := "1"
	if scanner.Scan() && strings.TrimSpace(scanner.Text()) != "" {
		choice = strings.TrimSpace(scanner.Text())
	}

	switch choice {
	case "1", "generate":
		cfg, err := m.GenerateSSHKey("")
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "✓ Generated Ed25519 SSH keypair (~/.ssh/id_ed25519)\nPublic key (add to GitHub/GitLab):\n%s\n", cfg.PublicKey)
		return nil
	case "2", "import", "paste":
		_, _ = fmt.Fprintln(out, "Paste your SSH private key (end with -----END ... PRIVATE KEY-----):")
		var lines []string
		for scanner.Scan() {
			line := scanner.Text()
			lines = append(lines, line)
			if strings.Contains(line, "-----END ") && strings.Contains(line, "PRIVATE KEY-----") {
				break
			}
		}
		cfg, err := m.SetSSHKey(strings.Join(lines, "\n"))
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "✓ Imported SSH key (~/.ssh/id_ed25519)\nPublic key:\n%s\n", cfg.PublicKey)
		return nil
	case "3", "show":
		pub, err := m.PublicSSHKey()
		if err != nil {
			_, _ = fmt.Fprintln(out, "No SSH public key found. Run 'orbit ssh-key --generate' to create one.")
			return nil
		}
		_, _ = fmt.Fprintf(out, "%s\n", pub)
		return nil
	default:
		return fmt.Errorf("%w: invalid ssh-key option %q", domain.ErrInvalidConfig, choice)
	}
}

// RunInteractive runs an interactive terminal menu for orbit-helper.
func (m *Manager) RunInteractive(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	for {
		_, _ = fmt.Fprintln(out, "")
		_, _ = fmt.Fprintln(out, "🪐 Orbit Helper — Workspace, Nix Flake & AI Tool Assistant")
		_, _ = fmt.Fprintln(out, "──────────────────────────────────────────────────────────")
		_, _ = fmt.Fprintln(out, "  [1] Clone & setup workspace repositories (with direnv + Nix flake)")
		_, _ = fmt.Fprintln(out, "  [2] Initialize or auto-load Nix flake.nix in a repository")
		_, _ = fmt.Fprintln(out, "  [3] Configure AI & developer tools (multi-select: claude, agy, opencode, gh, git)")
		_, _ = fmt.Fprintln(out, "  [4] Show status & save profile snapshot")
		_, _ = fmt.Fprintln(out, "  [5] Configure Git identity (user.name, user.email, defaultBranch)")
		_, _ = fmt.Fprintln(out, "  [6] Configure Git SSH key (generate Ed25519 or import key)")
		_, _ = fmt.Fprintln(out, "  [q] Quit")
		_, _ = fmt.Fprint(out, "\nSelect an option [1-6/q]: ")

		if !scanner.Scan() {
			return nil
		}
		choice := strings.ToLower(strings.TrimSpace(scanner.Text()))
		switch choice {
		case "1", "repos":
			orbitCtx, _ := m.LoadContext()
			if len(orbitCtx.Repositories) > 0 {
				_, _ = fmt.Fprintln(out, "\nAvailable workspace repositories:")
				for i, r := range orbitCtx.Repositories {
					_, _ = fmt.Fprintf(out, "  [%d] %s (%s)\n", i+1, r.Name, r.CloneURL)
				}
			}
			_, _ = fmt.Fprint(out, "Enter repo name/URL, comma-separated numbers, or 'all' [all]: ")
			sel := "all"
			if scanner.Scan() && strings.TrimSpace(scanner.Text()) != "" {
				sel = strings.TrimSpace(scanner.Text())
			}
			_, _ = fmt.Fprint(out, "Generate flake.nix automatically if missing? [Y/n]: ")
			initFlake := true
			if scanner.Scan() {
				ans := strings.ToLower(strings.TrimSpace(scanner.Text()))
				if ans == "n" || ans == "no" {
					initFlake = false
				}
			}
			opts := resolveInteractiveCloneOptions(orbitCtx.Repositories, sel, initFlake)
			cloned, err := m.CloneRepositories(ctx, opts)
			if err != nil {
				_, _ = fmt.Fprintf(out, "Error: %v\n", err)
			} else {
				_, _ = fmt.Fprintf(out, "✓ Cloned and configured: %s\n", strings.Join(cloned, ", "))
			}

		case "2", "flake":
			_, _ = fmt.Fprint(out, "Repository directory or name in workspace (leave blank to auto-load all): ")
			target := ""
			if scanner.Scan() {
				target = strings.TrimSpace(scanner.Text())
			}
			if target == "" {
				enabled, err := m.AutoloadAllWorkspaceFlakes(ctx)
				if err != nil {
					_, _ = fmt.Fprintf(out, "Error: %v\n", err)
				} else {
					_, _ = fmt.Fprintf(out, "✓ Enabled flake auto-loading for: %s\n", fallbackSlice(enabled, "(none found)"))
				}
				continue
			}
			repoDir := target
			if !filepath.IsAbs(repoDir) {
				repoDir = filepath.Join(m.workDir, repoDir)
			}
			_, _ = fmt.Fprint(out, "Nixpkgs packages (comma-separated, or blank for auto-detected stack defaults): ")
			var pkgs []string
			if scanner.Scan() && strings.TrimSpace(scanner.Text()) != "" {
				pkgs = strings.Split(scanner.Text(), ",")
			}
			if err := m.InitFlake(ctx, repoDir, pkgs); err != nil {
				_, _ = fmt.Fprintf(out, "Error: %v\n", err)
			} else {
				_, _ = fmt.Fprintf(out, "✓ Initialized flake.nix and direnv in %s\n", repoDir)
			}

		case "3", "tools":
			_, _ = fmt.Fprintln(out, "\nAvailable tools (select multiple with comma-separated numbers or names, or 'all'):")
			_, _ = fmt.Fprintln(out, "  [1] claude    — Claude Code CLI (wired to Orbit MCP)")
			_, _ = fmt.Fprintln(out, "  [2] agy       — Antigravity CLI (wired to Orbit MCP)")
			_, _ = fmt.Fprintln(out, "  [3] opencode  — OpenCode CLI (wired to Orbit MCP)")
			_, _ = fmt.Fprintln(out, "  [4] gh        — GitHub CLI configuration")
			_, _ = fmt.Fprintln(out, "  [5] git       — Git identity & credential helper")
			_, _ = fmt.Fprint(out, "Select tools [all]: ")
			rawSel := "all"
			if scanner.Scan() && strings.TrimSpace(scanner.Text()) != "" {
				rawSel = strings.TrimSpace(scanner.Text())
			}
			configured, err := m.ConfigureTools(strings.Split(rawSel, ","))
			if err != nil {
				_, _ = fmt.Fprintf(out, "Error: %v\n", err)
			} else {
				_, _ = fmt.Fprintf(out, "✓ Configured tools: %s\n", strings.Join(configured, ", "))
			}

		case "4", "status", "save":
			_ = m.Save(ctx)
			rep, _ := m.Status()
			_, _ = fmt.Fprintln(out, "")
			_, _ = fmt.Fprint(out, rep.Format())

		case "5", "git":
			if err := m.runInteractiveGitWithScanner(scanner, out); err != nil {
				_, _ = fmt.Fprintf(out, "Error: %v\n", err)
			}

		case "6", "ssh-key", "ssh":
			if err := m.runInteractiveSSHKeyWithScanner(scanner, out); err != nil {
				_, _ = fmt.Fprintf(out, "Error: %v\n", err)
			}

		case "q", "quit", "exit", "0":
			return nil
		}
	}
}

func encodeSSHString(b *bytes.Buffer, data []byte) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	_, _ = b.Write(lenBuf[:])
	_, _ = b.Write(data)
}

func readSSHString(data []byte) ([]byte, []byte, bool) {
	if len(data) < 4 {
		return nil, nil, false
	}
	n := int(binary.BigEndian.Uint32(data[:4]))
	if n < 0 || len(data) < 4+n {
		return nil, nil, false
	}
	return data[4 : 4+n], data[4+n:], true
}

func encodeEd25519PublicKeyWire(pub ed25519.PublicKey) []byte {
	var b bytes.Buffer
	encodeSSHString(&b, []byte("ssh-ed25519"))
	encodeSSHString(&b, pub)
	return b.Bytes()
}

func formatOpenSSHEd25519PublicKey(pub ed25519.PublicKey, comment string) string {
	wire := encodeEd25519PublicKeyWire(pub)
	b64 := base64.StdEncoding.EncodeToString(wire)
	if strings.TrimSpace(comment) == "" {
		return fmt.Sprintf("ssh-ed25519 %s\n", b64)
	}
	return fmt.Sprintf("ssh-ed25519 %s %s\n", b64, strings.TrimSpace(comment))
}

func marshalOpenSSHEd25519PrivateKey(priv ed25519.PrivateKey, comment string) string {
	pub := priv.Public().(ed25519.PublicKey)
	pubWire := encodeEd25519PublicKeyWire(pub)

	var checkBytes [4]byte
	_, _ = rand.Read(checkBytes[:])

	var privBlock bytes.Buffer
	_, _ = privBlock.Write(checkBytes[:])
	_, _ = privBlock.Write(checkBytes[:])
	encodeSSHString(&privBlock, []byte("ssh-ed25519"))
	encodeSSHString(&privBlock, pub)
	encodeSSHString(&privBlock, priv)
	encodeSSHString(&privBlock, []byte(comment))

	for i := 1; privBlock.Len()%8 != 0; i++ {
		_ = privBlock.WriteByte(byte(i & 0xff))
	}

	var out bytes.Buffer
	_, _ = out.WriteString("openssh-key-v1\x00")
	encodeSSHString(&out, []byte("none"))
	encodeSSHString(&out, []byte("none"))
	encodeSSHString(&out, nil)

	var numKeys [4]byte
	binary.BigEndian.PutUint32(numKeys[:], 1)
	_, _ = out.Write(numKeys[:])

	encodeSSHString(&out, pubWire)
	encodeSSHString(&out, privBlock.Bytes())

	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "OPENSSH PRIVATE KEY",
		Bytes: out.Bytes(),
	})
	return string(pemBytes)
}

func deriveSSHPublicKey(privPEM string) (string, error) {
	block, _ := pem.Decode([]byte(privPEM))
	if block == nil || !strings.Contains(block.Type, "PRIVATE KEY") {
		return "", fmt.Errorf("%w: invalid PEM private key", domain.ErrInvalidConfig)
	}

	// 1. Parse OpenSSH private key format (openssh-key-v1)
	const magic = "openssh-key-v1\x00"
	if bytes.HasPrefix(block.Bytes, []byte(magic)) {
		rest := block.Bytes[len(magic):]
		_, rest, ok1 := readSSHString(rest) // ciphername
		_, rest, ok2 := readSSHString(rest) // kdfname
		_, rest, ok3 := readSSHString(rest) // kdfoptions
		if !ok1 || !ok2 || !ok3 || len(rest) < 4 {
			return "", fmt.Errorf("%w: corrupt OpenSSH private key header", domain.ErrInvalidConfig)
		}
		numKeys := binary.BigEndian.Uint32(rest[:4])
		rest = rest[4:]
		pubWire, _, okPub := readSSHString(rest)
		if numKeys < 1 || !okPub || len(pubWire) == 0 {
			return "", fmt.Errorf("%w: corrupt OpenSSH public key blob", domain.ErrInvalidConfig)
		}
		keyTypeBytes, _, okType := readSSHString(pubWire)
		if !okType || len(keyTypeBytes) == 0 {
			return "", fmt.Errorf("%w: corrupt OpenSSH key type", domain.ErrInvalidConfig)
		}
		return fmt.Sprintf("%s %s\n", string(keyTypeBytes), base64.StdEncoding.EncodeToString(pubWire)), nil
	}

	// 2. Parse PKCS#8 Ed25519 private key
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if edPriv, ok := parsed.(ed25519.PrivateKey); ok {
			pub := edPriv.Public().(ed25519.PublicKey)
			return formatOpenSSHEd25519PublicKey(pub, ""), nil
		}
	}

	return "", fmt.Errorf("%w: unsupported SSH private key format", domain.ErrInvalidConfig)
}

func (m *Manager) writeEnvSh(ctxData OrbitContext) error {
	var b strings.Builder
	b.WriteString("# Generated by orbit-helper — sourced by ~/.bashrc\n")
	for _, kv := range [][2]string{
		{"ORBIT_URL", ctxData.OrbitURL},
		{"ORBIT_MCP_TOKEN", ctxData.OrbitMCPToken},
		{"ORBIT_WORKSPACE_ID", ctxData.WorkspaceID},
		{"ORBIT_WORKSPACE_SLUG", ctxData.WorkspaceSlug},
		{"ORBIT_USER_ID", ctxData.UserID},
		{"ORBIT_USER_EMAIL", ctxData.UserEmail},
	} {
		if kv[1] != "" {
			_, _ = fmt.Fprintf(&b, "export %s=%q\n", kv[0], kv[1])
		}
	}
	return os.WriteFile(filepath.Join(m.configDir, "env.sh"), []byte(b.String()), 0o600)
}

func resolveInteractiveCloneOptions(repos []protocol.RepoCredential, rawSel string, initFlake bool) CloneOptions {
	if strings.EqualFold(rawSel, "all") {
		return CloneOptions{CloneAll: true, InitMissingFlake: initFlake}
	}
	var names []string
	for _, part := range strings.Split(rawSel, ",") {
		part = strings.TrimSpace(part)
		if idx, err := strconv.Atoi(part); err == nil && idx >= 1 && idx <= len(repos) {
			names = append(names, repos[idx-1].Name)
		} else if part != "" {
			names = append(names, part)
		}
	}
	return CloneOptions{NamesOrURLs: names, InitMissingFlake: initFlake}
}

func normalizeToolSelection(raw []string) []string {
	NumberToTool := map[string]string{
		"1": "claude",
		"2": "agy",
		"3": "opencode",
		"4": "gh",
		"5": "git",
	}
	var out []string
	for _, item := range raw {
		for _, tok := range strings.Split(item, ",") {
			tok = strings.ToLower(strings.TrimSpace(tok))
			if tok == "" {
				continue
			}
			if tok == "all" {
				return append([]string(nil), SupportedTools...)
			}
			if mapped, ok := NumberToTool[tok]; ok {
				tok = mapped
			}
			out = appendUnique(out, tok)
		}
	}
	return out
}

func matchRepo(repos []protocol.RepoCredential, query string) (protocol.RepoCredential, bool) {
	for _, r := range repos {
		if strings.EqualFold(r.Name, query) || strings.EqualFold(r.ID, query) || strings.EqualFold(r.CloneURL, query) {
			return r, true
		}
	}
	return protocol.RepoCredential{}, false
}

func repoNameFromURL(rawURL string) string {
	cleaned := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(rawURL), "/"), ".git")
	if idx := strings.LastIndexAny(cleaned, "/:"); idx >= 0 && idx+1 < len(cleaned) {
		return cleaned[idx+1:]
	}
	if cleaned == "" {
		return "repo"
	}
	return cleaned
}

func buildAuthenticatedCloneURL(repo protocol.RepoCredential) string {
	rawURL := strings.TrimSpace(repo.CloneURL)
	token := strings.TrimSpace(repo.GitToken)
	if token == "" {
		return rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return rawURL
	}
	user := strings.TrimSpace(repo.GitUsername)
	if user == "" {
		user = "x-access-token"
	}
	parsed.User = url.UserPassword(user, token)
	return parsed.String()
}

func defaultGitCloner(ctx context.Context, cloneURL, destDir, branch string) error {
	if _, err := os.Stat(filepath.Join(destDir, ".git")); err == nil {
		cmd := exec.CommandContext(ctx, "git", "-C", destDir, "pull", "--ff-only")
		_ = cmd.Run()
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return err
	}
	args := []string{"clone"}
	if strings.TrimSpace(branch) != "" {
		args = append(args, "--branch", strings.TrimSpace(branch))
	}
	args = append(args, cloneURL, destDir)
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func defaultDirenvAllower(ctx context.Context, dir string) error {
	path, err := exec.LookPath("direnv")
	if err != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, path, "allow", dir)
	_ = cmd.Run()
	return nil
}

func writeConfigFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

func appendUnique(slice []string, val string) []string {
	for _, existing := range slice {
		if existing == val {
			return slice
		}
	}
	return append(slice, val)
}

func cleanSlice(in []string) []string {
	var out []string
	for _, s := range in {
		for _, part := range strings.Split(s, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = appendUnique(out, trimmed)
			}
		}
	}
	return out
}

func fallbackStr(val, def string) string {
	if strings.TrimSpace(val) == "" {
		return def
	}
	return val
}

func fallbackSlice(vals []string, def string) string {
	if len(vals) == 0 {
		return def
	}
	return strings.Join(vals, ", ")
}
