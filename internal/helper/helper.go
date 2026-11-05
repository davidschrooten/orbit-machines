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
	"testing"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/nixenv"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
	"github.com/davidschrooten/orbit-machines/internal/shellinit"
	"github.com/davidschrooten/orbit-machines/internal/tui/flakewizard"
)

// SupportedTools lists all developer and AI tools configurable by orbit-helper.
var SupportedTools = []string{"claude", "agy", "opencode", "gh", "git"}

const gitSSHMarker = "# orbit-git-ssh: v1"

// GitCloner clones or updates a Git repository into destDir.
type GitCloner func(ctx context.Context, cloneURL, destDir, branch string) error

// DirenvAllower marks a repository directory's .envrc as trusted in direnv.
type DirenvAllower func(ctx context.Context, dir string) error

// NixEnvRunner installs the declarative Nix package environment from configNixPath.
type NixEnvRunner func(ctx context.Context, configNixPath string) error

// ToolInstaller installs a CLI tool binary into homeDir (e.g. ~/.local/bin) when missing.
type ToolInstaller func(ctx context.Context, homeDir, tool string, out io.Writer) error

// NoopToolInstaller is a ToolInstaller that performs no binary installation (used when only syncing config files).
func NoopToolInstaller(_ context.Context, _, _ string, _ io.Writer) error { return nil }

// NixPackageResult describes a package found in Nixpkgs.
type NixPackageResult struct {
	Attribute   string `json:"attribute"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// NixSearcher searches Nixpkgs for packages matching query.
type NixSearcher func(ctx context.Context, query string) ([]NixPackageResult, error)

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
	NixEnvRunner  NixEnvRunner
	ToolInstaller ToolInstaller
	NixSearcher   NixSearcher
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
	nixEnvRunner  NixEnvRunner
	toolInstaller ToolInstaller
	nixSearcher   NixSearcher
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
		cloner = func(ctx context.Context, cloneURL, destDir, branch string) error {
			return defaultGitClonerWithHome(ctx, homeDir, cloneURL, destDir, branch)
		}
	}
	allower := cfg.DirenvAllower
	if allower == nil {
		allower = defaultDirenvAllower
	}
	nixRunner := cfg.NixEnvRunner
	if nixRunner == nil {
		nixRunner = func(ctx context.Context, configNixPath string) error {
			return defaultNixEnvRunner(ctx, homeDir, configNixPath)
		}
	}
	installer := cfg.ToolInstaller
	if installer == nil {
		installer = func(ctx context.Context, hDir, tool string, out io.Writer) error {
			return defaultToolInstaller(ctx, hDir, tool, out)
		}
	}
	searcher := cfg.NixSearcher
	if searcher == nil {
		searcher = func(ctx context.Context, query string) ([]NixPackageResult, error) {
			return defaultNixSearcher(ctx, homeDir, query)
		}
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
		nixEnvRunner:  nixRunner,
		toolInstaller: installer,
		nixSearcher:   searcher,
		httpClient:    httpClient,
	}
	_ = m.EnsureGitAndSSH()
	return m, nil
}

// SaveContext writes context.json and env.sh under ~/.config/orbit and mirrors context.json into <workDir>/.orbit.
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
	_ = writeConfigFile(filepath.Join(m.workDir, ".orbit", "context.json"), raw, 0o600)
	return m.writeEnvSh(ctxData)
}

// LoadContext reads ~/.config/orbit/context.json (falling back to <workDir>/.orbit/context.json) and merges any ORBIT_* environment variables.
func (m *Manager) LoadContext() (OrbitContext, error) {
	var out OrbitContext
	if data, err := os.ReadFile(filepath.Join(m.configDir, "context.json")); err == nil {
		_ = json.Unmarshal(data, &out)
	}
	if wsData, err := os.ReadFile(filepath.Join(m.workDir, ".orbit", "context.json")); err == nil {
		var wsCtx OrbitContext
		if json.Unmarshal(wsData, &wsCtx) == nil {
			out.OrbitURL = fallbackStr(out.OrbitURL, wsCtx.OrbitURL)
			out.OrbitMCPToken = fallbackStr(out.OrbitMCPToken, wsCtx.OrbitMCPToken)
			out.WorkspaceID = fallbackStr(out.WorkspaceID, wsCtx.WorkspaceID)
			out.WorkspaceSlug = fallbackStr(out.WorkspaceSlug, wsCtx.WorkspaceSlug)
			out.UserID = fallbackStr(out.UserID, wsCtx.UserID)
			out.UserEmail = fallbackStr(out.UserEmail, wsCtx.UserEmail)
			if len(out.Repositories) == 0 {
				out.Repositories = wsCtx.Repositories
			}
		}
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

// LoadState reads ~/.config/orbit/state.json (falling back to helper-state.json and merging <workDir>/.orbit/state.json).
func (m *Manager) LoadState() (State, error) {
	var st State
	if data, err := os.ReadFile(filepath.Join(m.configDir, "state.json")); err == nil {
		_ = json.Unmarshal(data, &st)
	} else if data, err := os.ReadFile(filepath.Join(m.configDir, "helper-state.json")); err == nil {
		_ = json.Unmarshal(data, &st)
	}

	if wsData, err := os.ReadFile(filepath.Join(m.workDir, ".orbit", "state.json")); err == nil {
		var wsSt State
		if json.Unmarshal(wsData, &wsSt) == nil {
			st = mergeStates(st, wsSt)
		}
	}
	return st, nil
}

func mergeStates(primary, secondary State) State {
	out := primary
	for _, r := range secondary.ClonedRepos {
		out.ClonedRepos = appendUnique(out.ClonedRepos, r)
	}
	for _, r := range secondary.FlakeEnabledRepos {
		out.FlakeEnabledRepos = appendUnique(out.FlakeEnabledRepos, r)
	}
	for _, t := range secondary.ConfiguredTools {
		out.ConfiguredTools = appendUnique(out.ConfiguredTools, t)
	}
	if (out.SSHKey == nil || strings.TrimSpace(out.SSHKey.PrivateKey) == "") && secondary.SSHKey != nil && strings.TrimSpace(secondary.SSHKey.PrivateKey) != "" {
		out.SSHKey = secondary.SSHKey
	}
	if out.Git == nil && secondary.Git != nil {
		out.Git = secondary.Git
	}
	if secondary.UpdatedAt.After(out.UpdatedAt) {
		out.UpdatedAt = secondary.UpdatedAt
		if secondary.SSHKey != nil && strings.TrimSpace(secondary.SSHKey.PrivateKey) != "" {
			out.SSHKey = secondary.SSHKey
		}
		if secondary.Git != nil {
			out.Git = secondary.Git
		}
	}
	return out
}

// SaveState writes ~/.config/orbit/state.json, ~/.config/orbit/helper-state.json, and <workDir>/.orbit/state.json.
func (m *Manager) SaveState(st State) error {
	st.UpdatedAt = time.Now().UTC()
	raw, _ := json.MarshalIndent(st, "", "  ")
	_ = writeConfigFile(filepath.Join(m.configDir, "helper-state.json"), raw, 0o600)
	_ = writeConfigFile(filepath.Join(m.workDir, ".orbit", "state.json"), raw, 0o600)
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

	gitCfg := formatGitConfig(m.homeDir, name, email, defaultBranch)
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
	if err := m.SaveState(st); err != nil {
		return err
	}
	m.triggerSnapshot()
	return nil
}

// EnsureGitAndSSH ensures ~/.gitconfig is initialized with SSH key rules (restoring from State.Git or Orbit user defaults)
// and restores ~/.ssh/id_ed25519, ~/.ssh/id_ed25519.pub, and ~/.ssh/config if saved in State.SSHKey or workspace backup.
func (m *Manager) EnsureGitAndSSH() error {
	st, _ := m.LoadState()
	orbitCtx, _ := m.LoadContext()
	stateChanged := false

	if st.SSHKey == nil || strings.TrimSpace(st.SSHKey.PrivateKey) == "" {
		if diskKey, ok := m.discoverSSHKeyFromDisk(); ok {
			st.SSHKey = &diskKey
			stateChanged = true
		}
	}

	gitConfigPath := filepath.Join(m.homeDir, ".gitconfig")
	existingGit, gitErr := os.ReadFile(gitConfigPath)
	needGitWrite := gitErr != nil || !strings.Contains(string(existingGit), gitSSHMarker)
	if !needGitWrite && st.Git != nil && strings.TrimSpace(st.Git.UserName) != "" {
		if !strings.Contains(string(existingGit), "name = "+strings.TrimSpace(st.Git.UserName)) {
			needGitWrite = true
		}
	}
	if needGitWrite {
		name, email, branch := m.defaultGitIdentity(st, orbitCtx)
		gitCfg := formatGitConfig(m.homeDir, name, email, branch)
		if err := writeConfigFile(gitConfigPath, []byte(gitCfg), 0o644); err != nil {
			return err
		}
		if st.Git == nil {
			st.Git = &GitConfig{
				UserName:      name,
				UserEmail:     email,
				DefaultBranch: branch,
			}
			stateChanged = true
		}
	}

	if st.SSHKey != nil && strings.TrimSpace(st.SSHKey.PrivateKey) != "" {
		if strings.TrimSpace(st.SSHKey.PublicKey) == "" {
			if derived, err := deriveSSHPublicKey(st.SSHKey.PrivateKey); err == nil {
				st.SSHKey.PublicKey = strings.TrimSpace(derived)
				stateChanged = true
			}
		}
		if err := m.writeSSHFiles(st.SSHKey.PrivateKey, st.SSHKey.PublicKey); err != nil {
			return err
		}
	}

	if _, err := os.Stat(filepath.Join(m.configDir, "state.json")); err != nil && (st.SSHKey != nil || st.Git != nil) {
		stateChanged = true
	}
	if stateChanged {
		_ = m.SaveState(st)
	}
	m.syncConfigurationNix()
	return nil
}

// ConfigurationNixPath returns the path to ~/.config/orbit/configuration.nix.
func (m *Manager) ConfigurationNixPath() string {
	return filepath.Join(m.configDir, "configuration.nix")
}

func (m *Manager) syncConfigurationNix() {
	homeCfgPath := m.ConfigurationNixPath()
	wsCfgPath := filepath.Join(m.workDir, ".orbit", "configuration.nix")

	homeData, homeErr := os.ReadFile(homeCfgPath)
	wsData, wsErr := os.ReadFile(wsCfgPath)

	if wsErr == nil && strings.TrimSpace(string(wsData)) != "" {
		if homeErr != nil || strings.TrimSpace(string(homeData)) == "" || (string(homeData) == shellinit.DefaultConfigurationNix && string(wsData) != shellinit.DefaultConfigurationNix) {
			_ = writeConfigFile(homeCfgPath, wsData, 0o644)
			return
		}
	}
	if homeErr == nil && strings.TrimSpace(string(homeData)) != "" {
		_ = writeConfigFile(wsCfgPath, homeData, 0o644)
		return
	}
	_ = writeConfigFile(homeCfgPath, []byte(shellinit.DefaultConfigurationNix), 0o644)
	_ = writeConfigFile(wsCfgPath, []byte(shellinit.DefaultConfigurationNix), 0o644)
}

// ListConfigurationNixPackages parses ~/.config/orbit/configuration.nix and returns the list
// of package attributes inside `paths = with pkgs; [ ... ];`.
func (m *Manager) ListConfigurationNixPackages() ([]string, error) {
	m.syncConfigurationNix()
	homeCfgPath := m.ConfigurationNixPath()
	data, err := os.ReadFile(homeCfgPath)
	if err != nil {
		data = []byte(shellinit.DefaultConfigurationNix)
	}
	return parseConfigurationNixPackages(string(data))
}

func parseConfigurationNixPackages(content string) ([]string, error) {
	endIdx := strings.LastIndex(content, "];")
	if endIdx < 0 {
		return nil, fmt.Errorf("%w: invalid configuration.nix format (missing '];')", domain.ErrInvalidConfig)
	}
	startIdx := strings.LastIndex(content[:endIdx], "[")
	if startIdx < 0 {
		return nil, fmt.Errorf("%w: invalid configuration.nix format (missing '[')", domain.ErrInvalidConfig)
	}
	body := content[startIdx+1 : endIdx]
	var pkgs []string
	for _, line := range strings.Split(body, "\n") {
		if commentIdx := strings.Index(line, "#"); commentIdx >= 0 {
			line = line[:commentIdx]
		}
		for _, tok := range strings.Fields(line) {
			tok = strings.TrimPrefix(strings.TrimSpace(tok), "pkgs.")
			if tok != "" {
				pkgs = appendUnique(pkgs, tok)
			}
		}
	}
	return pkgs, nil
}

// AddConfigurationNixPackages adds the given Nixpkgs package attribute names to ~/.config/orbit/configuration.nix
// (if not already present) and mirrors the updated file into <workDir>/.orbit/configuration.nix.
func (m *Manager) AddConfigurationNixPackages(packages []string) error {
	pkgs := cleanPackageSlice(packages)
	if len(pkgs) == 0 {
		return nil
	}
	m.syncConfigurationNix()
	homeCfgPath := m.ConfigurationNixPath()
	data, err := os.ReadFile(homeCfgPath)
	if err != nil {
		data = []byte(shellinit.DefaultConfigurationNix)
	}
	content := string(data)
	existing, parseErr := parseConfigurationNixPackages(content)
	if parseErr != nil {
		return parseErr
	}
	existingSet := make(map[string]bool, len(existing))
	for _, e := range existing {
		existingSet[e] = true
	}
	idx := strings.LastIndex(content, "];")
	var addLines strings.Builder
	for _, pkg := range pkgs {
		pkg = strings.TrimPrefix(pkg, "pkgs.")
		pkg = strings.TrimPrefix(pkg, "nixpkgs.")
		if pkg != "" && !existingSet[pkg] {
			existingSet[pkg] = true
			addLines.WriteString("    " + pkg + "\n")
		}
	}
	updated := content[:idx] + addLines.String() + content[idx:]
	if err := writeConfigFile(homeCfgPath, []byte(updated), 0o644); err != nil {
		return err
	}
	return writeConfigFile(filepath.Join(m.workDir, ".orbit", "configuration.nix"), []byte(updated), 0o644)
}

// RemoveConfigurationNixPackages removes the given Nixpkgs package attribute names from ~/.config/orbit/configuration.nix
// and mirrors the updated file into <workDir>/.orbit/configuration.nix.
func (m *Manager) RemoveConfigurationNixPackages(packages []string) error {
	pkgs := cleanPackageSlice(packages)
	if len(pkgs) == 0 {
		return nil
	}
	removeSet := make(map[string]bool, len(pkgs))
	for _, pkg := range pkgs {
		pkg = strings.TrimPrefix(pkg, "pkgs.")
		pkg = strings.TrimPrefix(pkg, "nixpkgs.")
		if pkg != "" {
			removeSet[pkg] = true
		}
	}
	m.syncConfigurationNix()
	homeCfgPath := m.ConfigurationNixPath()
	data, err := os.ReadFile(homeCfgPath)
	if err != nil {
		data = []byte(shellinit.DefaultConfigurationNix)
	}
	content := string(data)
	existing, parseErr := parseConfigurationNixPackages(content)
	if parseErr != nil {
		return parseErr
	}
	endIdx := strings.LastIndex(content, "];")
	startIdx := strings.LastIndex(content[:endIdx], "[")
	var body strings.Builder
	body.WriteByte('\n')
	for _, pkg := range existing {
		if !removeSet[pkg] {
			body.WriteString("    " + pkg + "\n")
		}
	}
	body.WriteString("  ")
	updated := content[:startIdx+1] + body.String() + content[endIdx:]
	if err := writeConfigFile(homeCfgPath, []byte(updated), 0o644); err != nil {
		return err
	}
	return writeConfigFile(filepath.Join(m.workDir, ".orbit", "configuration.nix"), []byte(updated), 0o644)
}

// InstallConfigurationNixPackages adds the given Nixpkgs package attributes to ~/.config/orbit/configuration.nix
// and applies the updated configuration immediately.
func (m *Manager) InstallConfigurationNixPackages(ctx context.Context, packages []string) error {
	pkgs := cleanPackageSlice(packages)
	if len(pkgs) == 0 {
		return fmt.Errorf("%w: at least one package name is required", domain.ErrInvalidConfig)
	}
	if err := m.AddConfigurationNixPackages(pkgs); err != nil {
		return err
	}
	return m.ApplyConfigurationNix(ctx)
}

// UninstallConfigurationNixPackages removes the given Nixpkgs package attributes from ~/.config/orbit/configuration.nix
// and rebuilds the system profile environment.
func (m *Manager) UninstallConfigurationNixPackages(ctx context.Context, packages []string) error {
	pkgs := cleanPackageSlice(packages)
	if len(pkgs) == 0 {
		return fmt.Errorf("%w: at least one package name is required", domain.ErrInvalidConfig)
	}
	if err := m.RemoveConfigurationNixPackages(pkgs); err != nil {
		return err
	}
	return m.ApplyConfigurationNix(ctx)
}

// SearchNixPackages searches Nixpkgs for packages matching query.
func (m *Manager) SearchNixPackages(ctx context.Context, query string) ([]NixPackageResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("%w: package search query is required", domain.ErrInvalidConfig)
	}
	return m.nixSearcher(ctx, query)
}

// ApplyConfigurationNix mirrors ~/.config/orbit/configuration.nix into <workDir>/.orbit/configuration.nix,
// invokes nix-env -i -f ~/.config/orbit/configuration.nix, and triggers a profile snapshot.
func (m *Manager) ApplyConfigurationNix(ctx context.Context) error {
	m.syncConfigurationNix()
	homeCfgPath := m.ConfigurationNixPath()
	if data, err := os.ReadFile(homeCfgPath); err == nil && strings.TrimSpace(string(data)) != "" {
		_ = writeConfigFile(filepath.Join(m.workDir, ".orbit", "configuration.nix"), data, 0o644)
	}
	if err := m.nixEnvRunner(ctx, homeCfgPath); err != nil {
		return err
	}
	m.triggerSnapshot()
	return nil
}

func (m *Manager) discoverSSHKeyFromDisk() (SSHKeyConfig, bool) {
	candidates := []string{
		filepath.Join(m.homeDir, ".ssh", "id_ed25519"),
		filepath.Join(m.workDir, ".orbit", "ssh", "id_ed25519"),
	}
	for _, privPath := range candidates {
		privBytes, err := os.ReadFile(privPath)
		if err != nil || strings.TrimSpace(string(privBytes)) == "" {
			continue
		}
		privPEM := strings.TrimSpace(string(privBytes)) + "\n"
		pubAuth := ""
		if pubBytes, pubErr := os.ReadFile(privPath + ".pub"); pubErr == nil && strings.TrimSpace(string(pubBytes)) != "" {
			pubAuth = strings.TrimSpace(string(pubBytes))
		} else if derived, derErr := deriveSSHPublicKey(privPEM); derErr == nil {
			pubAuth = strings.TrimSpace(derived)
		}
		if pubAuth != "" {
			return SSHKeyConfig{
				PrivateKey: privPEM,
				PublicKey:  pubAuth,
			}, true
		}
	}
	return SSHKeyConfig{}, false
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
	_ = m.EnsureGitAndSSH()
	m.triggerSnapshot()
	return cfg, nil
}

// SetSSHKey validates an existing SSH private key PEM, derives its public key, writes
// ~/.ssh/id_ed25519, ~/.ssh/id_ed25519.pub, and ~/.ssh/config, and persists it in State.
func (m *Manager) SetSSHKey(privateKeyPEM string) (SSHKeyConfig, error) {
	cleaned := strings.ReplaceAll(privateKeyPEM, "\r\n", "\n")
	cleaned = strings.ReplaceAll(cleaned, "\r", "\n")
	trimmed := strings.TrimSpace(cleaned)
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
	_ = m.EnsureGitAndSSH()
	m.triggerSnapshot()
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
		if strings.TrimSpace(st.SSHKey.PrivateKey) != "" {
			_ = m.writeSSHFiles(st.SSHKey.PrivateKey, st.SSHKey.PublicKey)
		}
		return strings.TrimSpace(st.SSHKey.PublicKey), nil
	}
	return "", fmt.Errorf("%w: no SSH key configured; run 'orbit ssh-key --generate'", domain.ErrInvalidConfig)
}

func (m *Manager) writeSSHFiles(privPEM, pubAuth string) error {
	sshDir := filepath.Join(m.homeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("creating ~/.ssh: %w", err)
	}
	_ = os.Chmod(sshDir, 0o700)
	privContent := []byte(strings.TrimSpace(privPEM) + "\n")
	privPath := filepath.Join(sshDir, "id_ed25519")
	if err := os.WriteFile(privPath, privContent, 0o600); err != nil {
		return fmt.Errorf("writing id_ed25519: %w", err)
	}
	_ = os.Chmod(privPath, 0o600)
	if strings.TrimSpace(pubAuth) != "" {
		pubContent := []byte(strings.TrimSpace(pubAuth) + "\n")
		pubPath := filepath.Join(sshDir, "id_ed25519.pub")
		_ = os.WriteFile(pubPath, pubContent, 0o644)
		_ = os.Chmod(pubPath, 0o644)
		_ = writeConfigFile(filepath.Join(m.workDir, ".orbit", "ssh", "id_ed25519.pub"), pubContent, 0o644)
	}
	_ = writeConfigFile(filepath.Join(m.workDir, ".orbit", "ssh", "id_ed25519"), privContent, 0o600)

	knownHostsPath := filepath.Join(sshDir, "known_hosts")
	sshConfigPath := filepath.Join(sshDir, "config")
	sshConfig := fmt.Sprintf("Host *\n  IdentityFile %s\n  IdentitiesOnly yes\n  StrictHostKeyChecking accept-new\n  UserKnownHostsFile %s\n", privPath, knownHostsPath)
	if err := os.WriteFile(sshConfigPath, []byte(sshConfig), 0o600); err != nil {
		return err
	}
	_ = os.Chmod(sshConfigPath, 0o600)
	return nil
}

func (m *Manager) triggerSnapshot() {
	if strings.TrimSpace(m.machinedURL) == "" || m.httpClient == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.machinedURL+"/api/v1/snapshot", nil)
	if err == nil {
		if resp, doErr := m.httpClient.Do(req); doErr == nil {
			_ = resp.Body.Close()
		}
	}
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

func formatGitConfig(homeDir, name, email, branch string) string {
	branch = fallbackStr(branch, "main")
	sshKeyPath := filepath.Join(homeDir, ".ssh", "id_ed25519")
	knownHostsPath := filepath.Join(homeDir, ".ssh", "known_hosts")
	return fmt.Sprintf(`%s
[user]
	name = %s
	email = %s
[init]
	defaultBranch = %s
[pull]
	rebase = false
[safe]
	directory = *
[core]
	sshCommand = ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s
[url "git@github.com:"]
	insteadOf = https://github.com/
	insteadOf = http://github.com/
[url "git@gitlab.com:"]
	insteadOf = https://gitlab.com/
	insteadOf = http://gitlab.com/
[url "git@bitbucket.org:"]
	insteadOf = https://bitbucket.org/
	insteadOf = http://bitbucket.org/
`, gitSSHMarker, name, email, branch, sshKeyPath, knownHostsPath)
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

// ConfigureTools configures one or more AI and developer CLI tools ("claude", "agy", "opencode",
// "codex", "gemini", "cursor", "aider", "gh", "git", or "all") and persists the selections in state.json.
func (m *Manager) ConfigureTools(selected []string) ([]string, error) {
	return m.ConfigureToolsWithContext(context.Background(), selected, io.Discard)
}

// ConfigureToolsWithContext installs missing tool binaries (via ToolInstaller) and writes/merges
// MCP and CLI configuration files for the selected tools.
func (m *Manager) ConfigureToolsWithContext(ctx context.Context, selected []string, out io.Writer) ([]string, error) {
	tools := normalizeToolSelection(selected)
	if len(tools) == 0 {
		return nil, fmt.Errorf("%w: at least one tool must be selected (%s)", domain.ErrInvalidConfig, strings.Join(SupportedTools, ", "))
	}
	for _, tool := range tools {
		if !isSupportedToolName(tool) {
			return nil, fmt.Errorf("%w: unsupported tool %q", domain.ErrInvalidConfig, tool)
		}
	}

	orbitCtx, _ := m.LoadContext()
	mcpServerURL := fallbackStr(strings.TrimRight(orbitCtx.OrbitURL, "/"), "http://localhost:3000") + "/mcp"

	orbitServerSpec := map[string]any{
		"type": "http",
		"url":  mcpServerURL,
		"headers": map[string]string{
			"Authorization": "Bearer " + orbitCtx.OrbitMCPToken,
		},
	}

	st, _ := m.LoadState()
	var configured []string

	for _, tool := range tools {
		if m.toolInstaller != nil {
			if err := m.toolInstaller(ctx, m.homeDir, tool, out); err != nil {
				return configured, fmt.Errorf("installing %s: %w", tool, err)
			}
		}

		switch tool {
		case "claude":
			if err := mergeMCPServerIntoJSONFile(filepath.Join(m.homeDir, ".claude.json"), orbitServerSpec, map[string]any{
				"hasCompletedOnboarding": true,
			}); err != nil {
				return configured, err
			}
			if err := mergeMCPServerIntoJSONFile(filepath.Join(m.homeDir, ".claude", "settings.json"), orbitServerSpec, nil); err != nil {
				return configured, err
			}
		case "agy":
			for _, p := range []string{
				filepath.Join(m.homeDir, ".gemini", "config", "mcp_config.json"),
				filepath.Join(m.homeDir, ".gemini", "antigravity-cli", "settings.json"),
				filepath.Join(m.homeDir, ".gemini", "settings.json"),
				filepath.Join(m.homeDir, ".config", "antigravity", "mcp.json"),
			} {
				if err := mergeMCPServerIntoJSONFile(p, orbitServerSpec, nil); err != nil {
					return configured, err
				}
			}
		case "gemini":
			if err := mergeMCPServerIntoJSONFile(filepath.Join(m.homeDir, ".gemini", "settings.json"), orbitServerSpec, nil); err != nil {
				return configured, err
			}
		case "opencode":
			for _, p := range []string{
				filepath.Join(m.homeDir, ".config", "opencode", "config.json"),
				filepath.Join(m.homeDir, ".config", "opencode", "opencode.json"),
			} {
				if err := mergeMCPServerIntoJSONFile(p, orbitServerSpec, nil); err != nil {
					return configured, err
				}
			}
		case "codex":
			if err := mergeMCPServerIntoJSONFile(filepath.Join(m.homeDir, ".codex", "config.json"), orbitServerSpec, nil); err != nil {
				return configured, err
			}
		case "cursor":
			if err := mergeMCPServerIntoJSONFile(filepath.Join(m.homeDir, ".cursor", "mcp.json"), orbitServerSpec, nil); err != nil {
				return configured, err
			}
		case "aider":
			if err := writeConfigFile(filepath.Join(m.homeDir, ".aider.conf.yml"), []byte("auto-commits: false\n"), 0o600); err != nil {
				return configured, err
			}
		case "gh":
			ghYML := []byte("git_protocol: ssh\neditor: vim\nprompt: enabled\n")
			if err := writeConfigFile(filepath.Join(m.homeDir, ".config", "gh", "config.yml"), ghYML, 0o600); err != nil {
				return configured, err
			}
		case "git":
			name, email, branch := m.defaultGitIdentity(st, orbitCtx)
			gitCfg := formatGitConfig(m.homeDir, name, email, branch)
			if err := writeConfigFile(filepath.Join(m.homeDir, ".gitconfig"), []byte(gitCfg), 0o644); err != nil {
				return configured, err
			}
			st.Git = &GitConfig{
				UserName:      name,
				UserEmail:     email,
				DefaultBranch: branch,
			}
		}
		configured = append(configured, tool)
		st.ConfiguredTools = appendUnique(st.ConfiguredTools, tool)
	}

	_ = m.SaveState(st)
	_ = m.writeEnvSh(orbitCtx)
	m.triggerSnapshot()
	return configured, nil
}

func isSupportedToolName(tool string) bool {
	switch tool {
	case "claude", "agy", "opencode", "codex", "gemini", "cursor", "aider", "gh", "git":
		return true
	default:
		return false
	}
}

func mergeMCPServerIntoJSONFile(path string, orbitServerSpec map[string]any, defaults map[string]any) error {
	doc := make(map[string]any)
	if existing, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(existing)) > 0 {
		_ = json.Unmarshal(existing, &doc)
	}
	for k, v := range defaults {
		if _, exists := doc[k]; !exists {
			doc[k] = v
		}
	}
	mcpServers, ok := doc["mcpServers"].(map[string]any)
	if !ok || mcpServers == nil {
		mcpServers = make(map[string]any)
	}
	mcpServers["orbit"] = orbitServerSpec
	doc["mcpServers"] = mcpServers

	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeConfigFile(path, append(encoded, '\n'), 0o600)
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
			line := strings.ReplaceAll(scanner.Text(), "\r", "")
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

// RunInteractivePackages runs an interactive submenu for searching Nixpkgs, installing,
// uninstalling, or listing system packages in ~/.config/orbit/configuration.nix.
func (m *Manager) RunInteractivePackages(ctx context.Context, in io.Reader, out io.Writer) error {
	return m.runInteractivePackagesWithScanner(ctx, bufio.NewScanner(in), out)
}

func (m *Manager) runInteractivePackagesWithScanner(ctx context.Context, scanner *bufio.Scanner, out io.Writer) error {
	_, _ = fmt.Fprintf(out, "\n📦 System Packages (%s)\n", m.ConfigurationNixPath())
	_, _ = fmt.Fprintln(out, "  [1] Search nixpkgs & install packages")
	_, _ = fmt.Fprintln(out, "  [2] Install package(s) by attribute name")
	_, _ = fmt.Fprintln(out, "  [3] Uninstall package(s)")
	_, _ = fmt.Fprintln(out, "  [4] List installed system packages")
	_, _ = fmt.Fprintln(out, "  [b] Back")
	_, _ = fmt.Fprint(out, "Select an option [1-4/b]: ")

	choice := "1"
	if scanner.Scan() && strings.TrimSpace(scanner.Text()) != "" {
		choice = strings.ToLower(strings.TrimSpace(scanner.Text()))
	}

	switch choice {
	case "1", "search", "s":
		_, _ = fmt.Fprint(out, "Search nixpkgs query: ")
		query := ""
		if scanner.Scan() {
			query = strings.TrimSpace(scanner.Text())
		}
		if query == "" {
			return nil
		}
		_, _ = fmt.Fprintf(out, "⠋ Searching nixpkgs for %q...\n", query)
		results, err := m.SearchNixPackages(ctx, query)
		if err != nil {
			return err
		}
		if len(results) == 0 {
			_, _ = fmt.Fprintf(out, "No packages found matching %q.\n", query)
			return nil
		}
		_, _ = fmt.Fprintf(out, "\nFound %d package(s):\n", len(results))
		for i, r := range results {
			ver := ""
			if r.Version != "" {
				ver = " (" + r.Version + ")"
			}
			desc := strings.TrimSpace(r.Description)
			if desc != "" {
				desc = " — " + desc
			}
			_, _ = fmt.Fprintf(out, "  [%d] %s%s%s\n", i+1, r.Attribute, ver, desc)
		}
		_, _ = fmt.Fprint(out, "Enter package numbers or attribute names to install (comma-separated, or blank to cancel): ")
		rawSel := ""
		if scanner.Scan() {
			rawSel = strings.TrimSpace(scanner.Text())
		}
		if rawSel == "" {
			return nil
		}
		var toInstall []string
		for _, tok := range cleanPackageSlice([]string{rawSel}) {
			if idx, convErr := strconv.Atoi(tok); convErr == nil && idx >= 1 && idx <= len(results) {
				toInstall = appendUnique(toInstall, results[idx-1].Attribute)
			} else {
				toInstall = appendUnique(toInstall, tok)
			}
		}
		if len(toInstall) == 0 {
			return nil
		}
		_, _ = fmt.Fprintf(out, "⠋ Installing system packages: %s...\n", strings.Join(toInstall, ", "))
		if err := m.InstallConfigurationNixPackages(ctx, toInstall); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "✓ Installed system packages: %s\n", strings.Join(toInstall, ", "))
		return nil

	case "2", "install", "add", "i":
		_, _ = fmt.Fprint(out, "Enter Nixpkgs attribute names to install (comma/space-separated): ")
		raw := ""
		if scanner.Scan() {
			raw = strings.TrimSpace(scanner.Text())
		}
		pkgs := cleanPackageSlice([]string{raw})
		if len(pkgs) == 0 {
			return nil
		}
		_, _ = fmt.Fprintf(out, "⠋ Installing system packages: %s...\n", strings.Join(pkgs, ", "))
		if err := m.InstallConfigurationNixPackages(ctx, pkgs); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "✓ Installed system packages: %s\n", strings.Join(pkgs, ", "))
		return nil

	case "3", "uninstall", "remove", "rm", "del":
		installed, err := m.ListConfigurationNixPackages()
		if err != nil {
			return err
		}
		if len(installed) == 0 {
			_, _ = fmt.Fprintln(out, "No packages found in configuration.nix.")
			return nil
		}
		_, _ = fmt.Fprintf(out, "\nConfigured system packages (%s):\n", m.ConfigurationNixPath())
		for i, pkg := range installed {
			_, _ = fmt.Fprintf(out, "  [%d] %s\n", i+1, pkg)
		}
		_, _ = fmt.Fprint(out, "Enter package numbers or attribute names to uninstall (comma-separated, or blank to cancel): ")
		rawSel := ""
		if scanner.Scan() {
			rawSel = strings.TrimSpace(scanner.Text())
		}
		if rawSel == "" {
			return nil
		}
		var toRemove []string
		for _, tok := range cleanPackageSlice([]string{rawSel}) {
			if idx, convErr := strconv.Atoi(tok); convErr == nil && idx >= 1 && idx <= len(installed) {
				toRemove = appendUnique(toRemove, installed[idx-1])
			} else {
				toRemove = appendUnique(toRemove, tok)
			}
		}
		if len(toRemove) == 0 {
			return nil
		}
		_, _ = fmt.Fprintf(out, "⠋ Uninstalling system packages: %s...\n", strings.Join(toRemove, ", "))
		if err := m.UninstallConfigurationNixPackages(ctx, toRemove); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "✓ Uninstalled system packages: %s\n", strings.Join(toRemove, ", "))
		return nil

	case "4", "list", "ls":
		installed, err := m.ListConfigurationNixPackages()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "\n📦 Installed System Packages (%s):\n", m.ConfigurationNixPath())
		for _, pkg := range installed {
			_, _ = fmt.Fprintf(out, "  • %s\n", pkg)
		}
		return nil

	case "b", "back", "q", "quit", "0":
		return nil

	default:
		return fmt.Errorf("%w: invalid packages option %q", domain.ErrInvalidConfig, choice)
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
		_, _ = fmt.Fprintln(out, "  [3] Install & configure AI / developer tools (claude, agy, opencode, codex, gemini, gh, git)")
		_, _ = fmt.Fprintln(out, "  [4] Show status & save profile snapshot")
		_, _ = fmt.Fprintln(out, "  [5] Configure Git identity (user.name, user.email, defaultBranch)")
		_, _ = fmt.Fprintln(out, "  [6] Configure Git SSH key (generate Ed25519 or import key)")
		_, _ = fmt.Fprintln(out, "  [7] Manage system packages — search nixpkgs, install, or uninstall (~/.config/orbit/configuration.nix)")
		_, _ = fmt.Fprintln(out, "  [8] Rebuild Nix system packages (~/.config/orbit/configuration.nix)")
		_, _ = fmt.Fprintln(out, "  [q] Quit")
		_, _ = fmt.Fprint(out, "\nSelect an option [1-8/q]: ")

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
					_, _ = fmt.Fprintf(out, "  [%d] %s (%s)\n", i+1, r.Name, ToSSHCloneURL(r.CloneURL))
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
			_, _ = fmt.Fprintln(out, "  [1] claude    — Claude Code CLI (installed & wired to Orbit MCP)")
			_, _ = fmt.Fprintln(out, "  [2] agy       — Antigravity CLI (installed & wired to Orbit MCP)")
			_, _ = fmt.Fprintln(out, "  [3] opencode  — OpenCode CLI (installed & wired to Orbit MCP)")
			_, _ = fmt.Fprintln(out, "  [4] gh        — GitHub CLI configuration")
			_, _ = fmt.Fprintln(out, "  [5] git       — Git identity & SSH configuration")
			_, _ = fmt.Fprintln(out, "  [6] codex     — OpenAI Codex CLI (installed & wired to Orbit MCP)")
			_, _ = fmt.Fprintln(out, "  [7] gemini    — Gemini CLI (installed & wired to Orbit MCP)")
			_, _ = fmt.Fprintln(out, "  [8] aider     — Aider CLI configuration")
			_, _ = fmt.Fprintln(out, "  [9] cursor    — Cursor MCP configuration")
			_, _ = fmt.Fprint(out, "Select tools [all]: ")
			rawSel := "all"
			if scanner.Scan() && strings.TrimSpace(scanner.Text()) != "" {
				rawSel = strings.TrimSpace(scanner.Text())
			}
			configured, err := m.ConfigureToolsWithContext(ctx, strings.Split(rawSel, ","), out)
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

		case "7", "packages", "pkg", "pkgs", "deps":
			if err := m.runInteractivePackagesWithScanner(ctx, scanner, out); err != nil {
				_, _ = fmt.Fprintf(out, "Error: %v\n", err)
			}

		case "8", "rebuild", "config-nix":
			_, _ = fmt.Fprintf(out, "\n📦 Declarative Nix Configuration: %s\n", m.ConfigurationNixPath())
			_, _ = fmt.Fprint(out, "Extra Nixpkgs packages to add (comma-separated, or blank to apply current file): ")
			if scanner.Scan() && strings.TrimSpace(scanner.Text()) != "" {
				_ = m.AddConfigurationNixPackages(strings.Split(scanner.Text(), ","))
			}
			if err := m.ApplyConfigurationNix(ctx); err != nil {
				_, _ = fmt.Fprintf(out, "Error: %v\n", err)
			} else {
				_, _ = fmt.Fprintf(out, "✓ Applied Nix packages from %s\n", m.ConfigurationNixPath())
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

	// 3. Fallback to ssh-keygen -y -f for RSA, ECDSA, and other OpenSSH-supported private key formats
	if sshKeygenPath, lookErr := exec.LookPath("ssh-keygen"); lookErr == nil {
		if tmpFile, tmpErr := os.CreateTemp("", "orbit-ssh-key-*"); tmpErr == nil {
			tmpPath := tmpFile.Name()
			defer func() { _ = os.Remove(tmpPath) }()
			_ = tmpFile.Chmod(0o600)
			_, _ = tmpFile.WriteString(strings.TrimSpace(privPEM) + "\n")
			_ = tmpFile.Close()
			if out, cmdErr := exec.Command(sshKeygenPath, "-P", "", "-y", "-f", tmpPath).Output(); cmdErr == nil && strings.TrimSpace(string(out)) != "" {
				return strings.TrimSpace(string(out)) + "\n", nil
			}
		}
	}

	return "", fmt.Errorf("%w: unsupported SSH private key format", domain.ErrInvalidConfig)
}

func (m *Manager) writeEnvSh(ctxData OrbitContext) error {
	var b strings.Builder
	b.WriteString("# Generated by orbit-helper — sourced by ~/.bashrc\n")
	sshKeyPath := filepath.Join(m.homeDir, ".ssh", "id_ed25519")
	knownHostsPath := filepath.Join(m.homeDir, ".ssh", "known_hosts")
	sshCmd := fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s", sshKeyPath, knownHostsPath)
	b.WriteString("export GIT_TERMINAL_PROMPT=0\n")
	_, _ = fmt.Fprintf(&b, "export GIT_SSH_COMMAND=%q\n", sshCmd)
	_, _ = fmt.Fprintf(&b, "export NPM_CONFIG_PREFIX=%q\n", filepath.Join(m.homeDir, ".local"))
	_, _ = fmt.Fprintf(&b, "export PATH=\"%s/.local/bin:%s/.nix-profile/bin:$PATH\"\n", m.homeDir, m.homeDir)
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
		"1":               "claude",
		"2":               "agy",
		"3":               "opencode",
		"4":               "gh",
		"5":               "git",
		"6":               "codex",
		"7":               "gemini",
		"8":               "aider",
		"9":               "cursor",
		"antigravity":     "agy",
		"antigravity-cli": "agy",
		"claude-code":     "claude",
		"opencode-ai":     "opencode",
		"gemini-cli":      "gemini",
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
		if strings.EqualFold(r.Name, query) || strings.EqualFold(r.ID, query) || strings.EqualFold(r.CloneURL, query) || strings.EqualFold(ToSSHCloneURL(r.CloneURL), query) {
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

// ToSSHCloneURL converts remote HTTP(S) Git repository URLs (e.g. https://github.com/owner/repo.git)
// to SSH clone URLs (e.g. git@github.com:owner/repo.git) so Git always authenticates via SSH keys
// and never prompts for an HTTPS username/password. Local filesystem paths and SSH URLs are returned as-is.
func ToSSHCloneURL(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed == nil {
		return trimmed
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return trimmed
	}
	repoPath := strings.Trim(parsed.Path, "/")
	if repoPath == "" {
		return trimmed
	}
	if !strings.HasSuffix(repoPath, ".git") {
		repoPath += ".git"
	}
	return fmt.Sprintf("git@%s:%s", parsed.Hostname(), repoPath)
}

func buildAuthenticatedCloneURL(repo protocol.RepoCredential) string {
	return ToSSHCloneURL(repo.CloneURL)
}

func gitEnvForHome(homeDir string) []string {
	env := append([]string(nil), os.Environ()...)
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	homeDir = strings.TrimSpace(homeDir)
	if homeDir != "" {
		sshKeyPath := filepath.Join(homeDir, ".ssh", "id_ed25519")
		knownHostsPath := filepath.Join(homeDir, ".ssh", "known_hosts")
		sshCmd := fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s", sshKeyPath, knownHostsPath)
		env = append(env, "GIT_SSH_COMMAND="+sshCmd)
	}
	return env
}

func defaultGitClonerWithHome(ctx context.Context, homeDir, cloneURL, destDir, branch string) error {
	cloneURL = ToSSHCloneURL(cloneURL)
	gitEnv := gitEnvForHome(homeDir)
	if _, err := os.Stat(filepath.Join(destDir, ".git")); err == nil {
		if cloneURL != "" {
			setURLCmd := exec.CommandContext(ctx, "git", "-C", destDir, "remote", "set-url", "origin", cloneURL)
			setURLCmd.Env = gitEnv
			_ = setURLCmd.Run()
		}
		cmd := exec.CommandContext(ctx, "git", "-C", destDir, "pull", "--ff-only")
		cmd.Env = gitEnv
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
	cmd.Env = gitEnv
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

func defaultNixEnvRunner(ctx context.Context, homeDir, configNixPath string) error {
	path, err := exec.LookPath("nix-env")
	if err != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, path, "-i", "-f", configNixPath)
	cmd.Env = append(gitEnvForHome(homeDir), "HOME="+homeDir)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return fmt.Errorf("nix-env -i -f %s failed: %w (%s)", configNixPath, runErr, strings.TrimSpace(string(out)))
	}
	return nil
}

func toolInstallEnv(homeDir string) []string {
	env := gitEnvForHome(homeDir)
	localDir := filepath.Join(homeDir, ".local")
	localBin := filepath.Join(localDir, "bin")
	nixBin := filepath.Join(homeDir, ".nix-profile", "bin")
	extraPath := strings.Join([]string{
		localBin,
		nixBin,
		"/nix/var/nix/profiles/per-user/orbit/profile/bin",
		"/nix/var/nix/profiles/orbit/bin",
		"/root/.nix-profile/bin",
		"/nix/var/nix/profiles/default/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		os.Getenv("PATH"),
	}, ":")
	env = append(env,
		"HOME="+homeDir,
		"NPM_CONFIG_PREFIX="+localDir,
		"PATH="+extraPath,
	)
	return env
}

func lookToolBinary(homeDir, binName string) (string, bool) {
	for _, candidate := range []string{
		filepath.Join(homeDir, ".local", "bin", binName),
		filepath.Join(homeDir, ".nix-profile", "bin", binName),
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	isRunnerBin := binName == "npm" || binName == "bash" || binName == "curl" || binName == "nix" || binName == "nix-env"
	if os.Getenv("ORBIT_TEST_REAL_TOOL_INSTALLER") == "" || isRunnerBin {
		if p, err := exec.LookPath(binName); err == nil {
			return p, true
		}
	}
	if os.Getenv("ORBIT_TEST_REAL_TOOL_INSTALLER") == "" {
		for _, sysCandidate := range []string{
			filepath.Join("/nix/var/nix/profiles/per-user/orbit/profile/bin", binName),
			filepath.Join("/nix/var/nix/profiles/orbit/bin", binName),
		} {
			if info, err := os.Stat(sysCandidate); err == nil && !info.IsDir() {
				return sysCandidate, true
			}
		}
	}
	return "", false
}

func defaultToolInstaller(ctx context.Context, homeDir, tool string, out io.Writer) error {
	if testing.Testing() && os.Getenv("ORBIT_TEST_REAL_TOOL_INSTALLER") == "" {
		return nil
	}
	localBin := filepath.Join(homeDir, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", localBin, err)
	}
	if out == nil {
		out = io.Discard
	}

	switch tool {
	case "claude":
		if _, ok := lookToolBinary(homeDir, "claude"); ok {
			return nil
		}
		_, _ = fmt.Fprintln(out, "⠋ Installing Claude Code CLI (@anthropic-ai/claude-code)...")
		if err := installNPMGlobalTool(ctx, homeDir, "@anthropic-ai/claude-code", "claude-code"); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, "✓ Installed Claude Code CLI")
		return nil

	case "agy":
		if _, ok := lookToolBinary(homeDir, "agy"); !ok {
			_, _ = fmt.Fprintln(out, "⠋ Installing Antigravity CLI (agy)...")
			if err := installAntigravityCLI(ctx, homeDir, localBin); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(out, "✓ Installed Antigravity CLI (agy)")
		}
		agyBin := filepath.Join(localBin, "agy")
		if _, err := os.Stat(agyBin); err == nil {
			aliasLink := filepath.Join(localBin, "antigravity-cli")
			_ = os.Remove(aliasLink)
			_ = os.Symlink(agyBin, aliasLink)
		}
		return nil

	case "opencode":
		if _, ok := lookToolBinary(homeDir, "opencode"); ok {
			return nil
		}
		_, _ = fmt.Fprintln(out, "⠋ Installing OpenCode CLI (opencode-ai)...")
		if err := installNPMGlobalTool(ctx, homeDir, "opencode-ai", "opencode"); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, "✓ Installed OpenCode CLI")
		return nil

	case "codex":
		if _, ok := lookToolBinary(homeDir, "codex"); ok {
			return nil
		}
		_, _ = fmt.Fprintln(out, "⠋ Installing OpenAI Codex CLI (@openai/codex)...")
		if err := installNPMGlobalTool(ctx, homeDir, "@openai/codex", ""); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, "✓ Installed OpenAI Codex CLI")
		return nil

	case "gemini":
		if _, ok := lookToolBinary(homeDir, "gemini"); ok {
			return nil
		}
		_, _ = fmt.Fprintln(out, "⠋ Installing Gemini CLI (@google/gemini-cli)...")
		if err := installNPMGlobalTool(ctx, homeDir, "@google/gemini-cli", "gemini-cli"); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, "✓ Installed Gemini CLI")
		return nil

	case "aider":
		if _, ok := lookToolBinary(homeDir, "aider"); ok {
			return nil
		}
		_, _ = fmt.Fprintln(out, "⠋ Installing Aider CLI (nixpkgs.aider-chat)...")
		if err := installNixEnvAttr(ctx, homeDir, "nixpkgs.aider-chat"); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, "✓ Installed Aider CLI")
		return nil

	case "gh":
		if _, ok := lookToolBinary(homeDir, "gh"); ok {
			return nil
		}
		_, _ = fmt.Fprintln(out, "⠋ Installing GitHub CLI (nixpkgs.gh)...")
		if err := installNixEnvAttr(ctx, homeDir, "nixpkgs.gh"); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, "✓ Installed GitHub CLI")
		return nil

	default:
		return nil
	}
}

func installNPMGlobalTool(ctx context.Context, homeDir, npmPkg, nixFallbackAttr string) error {
	if npmPath, ok := lookToolBinary(homeDir, "npm"); ok {
		prefix := filepath.Join(homeDir, ".local")
		cmd := exec.CommandContext(ctx, npmPath, "install", "-g", "--prefix", prefix, npmPkg)
		cmd.Env = toolInstallEnv(homeDir)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		if nixFallbackAttr == "" {
			return fmt.Errorf("npm install -g %s failed: %w (%s)", npmPkg, err, strings.TrimSpace(string(out)))
		}
	}
	if nixFallbackAttr != "" {
		return installNixEnvAttr(ctx, homeDir, "nixpkgs."+nixFallbackAttr)
	}
	return fmt.Errorf("npm not found in PATH to install %s", npmPkg)
}

func installAntigravityCLI(ctx context.Context, homeDir, localBin string) error {
	bashPath, ok := lookToolBinary(homeDir, "bash")
	if !ok {
		bashPath = "/bin/sh"
	}
	script := "curl -fsSL https://antigravity.google/cli/install.sh | bash -s -- --dir \"$1\""
	cmd := exec.CommandContext(ctx, bashPath, "-c", script, "--", localBin)
	cmd.Env = toolInstallEnv(homeDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("antigravity installer failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func installNixEnvAttr(ctx context.Context, homeDir, attr string) error {
	nixEnvPath, ok := lookToolBinary(homeDir, "nix-env")
	if !ok {
		return nil
	}
	cmd := exec.CommandContext(ctx, nixEnvPath, "-iA", attr)
	cmd.Env = toolInstallEnv(homeDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nix-env -iA %s failed: %w (%s)", attr, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func defaultNixSearcher(ctx context.Context, homeDir, query string) ([]NixPackageResult, error) {
	env := toolInstallEnv(homeDir)
	if nixPath, ok := lookToolBinary(homeDir, "nix"); ok {
		for _, args := range [][]string{
			{"--extra-experimental-features", "nix-command flakes", "search", "--impure", "--expr", "import <nixpkgs> {}", query, "--json"},
			{"--extra-experimental-features", "nix-command flakes", "search", "nixpkgs", query, "--json"},
		} {
			cmd := exec.CommandContext(ctx, nixPath, args...)
			cmd.Env = env
			if out, err := cmd.Output(); err == nil && len(bytes.TrimSpace(out)) > 0 {
				if results, parseErr := ParseNixSearchJSON(out, query); parseErr == nil && len(results) > 0 {
					return results, nil
				}
			}
		}
	}
	if nixEnvPath, ok := lookToolBinary(homeDir, "nix-env"); ok {
		cmd := exec.CommandContext(ctx, nixEnvPath, "-qaP", "--description", ".*"+query+".*")
		cmd.Env = env
		if out, err := cmd.Output(); err == nil && len(bytes.TrimSpace(out)) > 0 {
			return ParseNixEnvSearchOutput(string(out), query), nil
		}
	}
	return nil, nil
}

// ParseNixSearchJSON parses the JSON output of `nix search nixpkgs <query> --json` and sorts results by relevance.
func ParseNixSearchJSON(raw []byte, query string) ([]NixPackageResult, error) {
	type rawEntry struct {
		Pname       string `json:"pname"`
		Version     string `json:"version"`
		Description string `json:"description"`
	}
	var parsed map[string]rawEntry
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	var results []NixPackageResult
	seen := make(map[string]bool, len(parsed))
	for fullAttr, entry := range parsed {
		attr := cleanNixSearchAttr(fullAttr)
		if attr == "" || seen[attr] {
			continue
		}
		seen[attr] = true
		results = append(results, NixPackageResult{
			Attribute:   attr,
			Name:        fallbackStr(entry.Pname, attr),
			Version:     entry.Version,
			Description: entry.Description,
		})
	}
	return sortAndLimitNixResults(results, query, 25), nil
}

// ParseNixEnvSearchOutput parses the text output of `nix-env -qaP --description <query>`.
func ParseNixEnvSearchOutput(raw, query string) []NixPackageResult {
	var results []NixPackageResult
	seen := make(map[string]bool)
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		attr := cleanNixSearchAttr(fields[0])
		if attr == "" || seen[attr] {
			continue
		}
		seen[attr] = true
		nameVer := fields[1]
		desc := ""
		if idx := strings.Index(line, fields[1]); idx >= 0 {
			desc = strings.TrimSpace(line[idx+len(fields[1]):])
		}
		results = append(results, NixPackageResult{
			Attribute:   attr,
			Name:        nameVer,
			Description: desc,
		})
	}
	return sortAndLimitNixResults(results, query, 25)
}

func cleanNixSearchAttr(fullAttr string) string {
	trimmed := strings.TrimSpace(fullAttr)
	parts := strings.Split(trimmed, ".")
	if len(parts) >= 3 && (parts[0] == "legacyPackages" || parts[0] == "packages") {
		return strings.Join(parts[2:], ".")
	}
	if len(parts) >= 2 && (parts[0] == "nixpkgs" || parts[0] == "pkgs") {
		return strings.Join(parts[1:], ".")
	}
	return trimmed
}

func sortAndLimitNixResults(results []NixPackageResult, query string, limit int) []NixPackageResult {
	q := strings.ToLower(strings.TrimSpace(query))
	rank := func(r NixPackageResult) int {
		attr := strings.ToLower(r.Attribute)
		name := strings.ToLower(r.Name)
		switch {
		case attr == q || name == q:
			return 0
		case strings.HasPrefix(attr, q) || strings.HasPrefix(name, q):
			return 1
		case strings.Contains(attr, q) || strings.Contains(name, q):
			return 2
		default:
			return 3
		}
	}
	sort.Slice(results, func(i, j int) bool {
		ri, rj := rank(results[i]), rank(results[j])
		if ri != rj {
			return ri < rj
		}
		if len(results[i].Attribute) != len(results[j].Attribute) {
			return len(results[i].Attribute) < len(results[j].Attribute)
		}
		return results[i].Attribute < results[j].Attribute
	})
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results
}

func cleanPackageSlice(in []string) []string {
	var out []string
	for _, s := range in {
		for _, part := range strings.FieldsFunc(s, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n'
		}) {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = appendUnique(out, trimmed)
			}
		}
	}
	return out
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
