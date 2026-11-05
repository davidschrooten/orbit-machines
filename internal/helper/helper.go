// Package helper implements the Orbit Machine interactive helper CLI and state
// manager (repository cloning, Nix flake initialization & direnv auto-loading,
// multi-tool AI/CLI configuration, and profile persistence).
package helper

import (
	"bufio"
	"context"
	"encoding/json"
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

// State records persisted actions performed by orbit-helper in ~/.config/orbit/state.json.
type State struct {
	ClonedRepos       []string  `json:"cloned_repos"`
	FlakeEnabledRepos []string  `json:"flake_enabled_repos"`
	ConfiguredTools   []string  `json:"configured_tools"`
	UpdatedAt         time.Time `json:"updated_at"`
}

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

// Manager coordinates Orbit context persistence, repo cloning, Nix flake setup, and tool wiring.
type Manager struct {
	homeDir       string
	workDir       string
	configDir     string
	machinedURL   string
	gitCloner     GitCloner
	direnvAllower DirenvAllower
	httpClient    *http.Client
}

// NewManager validates cfg and constructs a Manager.
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

	return &Manager{
		homeDir:       homeDir,
		workDir:       workDir,
		configDir:     filepath.Join(homeDir, ".config", "orbit"),
		machinedURL:   machinedURL,
		gitCloner:     cloner,
		direnvAllower: allower,
		httpClient:    httpClient,
	}, nil
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
	out.UserEmail = fallbackStr(out.UserEmail, os.Getenv("ORBIT_USER_EMAIL"))
	if rawRepos := strings.TrimSpace(os.Getenv("ORBIT_REPOSITORIES_JSON")); rawRepos != "" && len(out.Repositories) == 0 {
		_ = json.Unmarshal([]byte(rawRepos), &out.Repositories)
	}
	return out, nil
}

// LoadState reads ~/.config/orbit/state.json.
func (m *Manager) LoadState() (State, error) {
	var st State
	if data, err := os.ReadFile(filepath.Join(m.configDir, "state.json")); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st, nil
}

// SaveState writes ~/.config/orbit/state.json.
func (m *Manager) SaveState(st State) error {
	st.UpdatedAt = time.Now().UTC()
	raw, _ := json.MarshalIndent(st, "", "  ")
	return writeConfigFile(filepath.Join(m.configDir, "state.json"), raw, 0o600)
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

// InitFlake generates a flake.nix in repoDir (auto-detecting project stacks when packages is empty)
// and enables automatic flake loading via direnv.
func (m *Manager) InitFlake(ctx context.Context, repoDir string, packages []string) error {
	repoDir = strings.TrimSpace(repoDir)
	if repoDir == "" {
		return fmt.Errorf("%w: repoDir is required", domain.ErrInvalidConfig)
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return err
	}

	pkgs := cleanSlice(packages)
	if len(pkgs) == 0 {
		wiz, _ := flakewizard.New(flakewizard.Config{
			RepoDir:  repoDir,
			RepoName: filepath.Base(repoDir),
		})
		pkgs = wiz.SelectedPackages()
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

// ConfigureTools configures one or more AI and developer CLI tools ("claude", "agy", "opencode", "gh", "git", or "all")
// and persists the selections in state.json.
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
		case "opencode":
			files[filepath.Join(m.homeDir, ".config", "opencode", "config.json")] = fileSpec{mcpBytes, 0o600}
			files[filepath.Join(m.homeDir, ".config", "opencode", "opencode.json")] = fileSpec{mcpBytes, 0o600}
		case "gh":
			ghYML := []byte("git_protocol: https\neditor: vim\nprompt: enabled\n")
			files[filepath.Join(m.homeDir, ".config", "gh", "config.yml")] = fileSpec{ghYML, 0o600}
		case "git":
			email := fallbackStr(orbitCtx.UserEmail, "orbit@orbit.local")
			name := email
			if at := strings.IndexByte(email, '@'); at > 0 {
				name = email[:at]
			}
			gitCfg := fmt.Sprintf("[user]\n\tname = %s\n\temail = %s\n[init]\n\tdefaultBranch = main\n[credential]\n\thelper = store\n", name, email)
			files[filepath.Join(m.homeDir, ".gitconfig")] = fileSpec{[]byte(gitCfg), 0o644}
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

// Status inspects the current Orbit context, helper state, and workspace directories.
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

	return StatusReport{
		Context:       orbitCtx,
		State:         st,
		WorkspaceDirs: dirs,
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
		_, _ = fmt.Fprintln(out, "  [q] Quit")
		_, _ = fmt.Fprint(out, "\nSelect an option [1-4/q]: ")

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

		case "q", "quit", "exit", "0":
			return nil
		}
	}
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
