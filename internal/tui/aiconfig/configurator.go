// Package aiconfig implements the built-in AI Configurator modal and structured
// agent tool handlers for orbit-machined.
package aiconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/pelletier/go-toml/v2"
)

// Tool names exposed by the AI Configurator.
const (
	ToolGetMachineConfig = "GetMachineConfig"
	ToolUpdateTUIConfig  = "UpdateTUIConfig"
	ToolSetShellProfile  = "SetShellProfile"
	ToolEditRepoFlake    = "EditRepoFlake"
	ToolSyncProfileNow   = "SyncProfileNow"
	ToolWireOrbitMCP     = "WireOrbitMCP"
)

// TUIConfig represents the persisted and live-reloadable TUI configuration in tui.toml.
type TUIConfig struct {
	Theme       string            `toml:"theme" json:"theme"`
	LeaderKey   string            `toml:"leader_key" json:"leader_key"`
	MouseMode   bool              `toml:"mouse_mode" json:"mouse_mode"`
	Keybindings map[string]string `toml:"keybindings" json:"keybindings"`
}

// ShellProfile represents the user's declarative Nix profile and shell init script.
type ShellProfile struct {
	ProfileNix string `json:"profile_nix"`
	InitZsh    string `json:"init_zsh"`
}

// MachineConfigSnapshot captures the complete machine configuration inspected by GetMachineConfig.
type MachineConfigSnapshot struct {
	TUI          TUIConfig    `json:"tui"`
	Shell        ShellProfile `json:"shell"`
	HasRepoFlake bool         `json:"has_repo_flake"`
	RepoFlake    string       `json:"repo_flake"`
}

// MCPConfig configures the Orbit MCP server URL and Bearer authentication token.
type MCPConfig struct {
	ServerURL   string `json:"server_url"`
	BearerToken string `json:"bearer_token"`
}

// ToolCall represents a structured AI tool invocation.
type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult reports the outcome of a structured AI tool invocation.
type ToolResult struct {
	ToolName string                 `json:"tool_name"`
	Success  bool                   `json:"success"`
	Summary  string                 `json:"summary"`
	Snapshot *MachineConfigSnapshot `json:"snapshot,omitempty"`
}

// Message represents an entry in the AI Configurator conversation history.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ReloadCallback is invoked immediately when UpdateTUIConfig writes a valid tui.toml.
type ReloadCallback func(cfg TUIConfig)

// FlakeValidator validates a repository's flake.nix (e.g. via nix flake check --no-build).
type FlakeValidator func(ctx context.Context, repoDir string) error

// ProfileSyncer uploads the user's home vault (profile.tar.zst) to S3/MinIO immediately.
type ProfileSyncer func(ctx context.Context) error

// Planner plans and returns structured tool calls and an assistant reply for a user prompt.
type Planner func(ctx context.Context, prompt string, state MachineConfigSnapshot) ([]ToolCall, string, error)

// Options configures the AI Configurator instance.
type Options struct {
	HomeDir        string
	ConfigDir      string
	RepoDir        string
	FlakeValidator FlakeValidator
	ProfileSyncer  ProfileSyncer
	OnReload       ReloadCallback
	Planner        Planner
}

// Configurator manages structured machine configuration tools and the AI Configurator TUI modal.
type Configurator struct {
	mu             sync.Mutex
	homeDir        string
	configDir      string
	repoDir        string
	flakeValidator FlakeValidator
	profileSyncer  ProfileSyncer
	onReload       ReloadCallback
	planner        Planner
	history        []Message
	input          string
}

// New constructs a new AI Configurator.
func New(opts Options) *Configurator {
	home := strings.TrimSpace(opts.HomeDir)
	if home == "" {
		home = "/home/orbit"
	}
	cfgDir := strings.TrimSpace(opts.ConfigDir)
	if cfgDir == "" {
		cfgDir = filepath.Join(home, ".config", "orbit")
	}
	return &Configurator{
		homeDir:        home,
		configDir:      cfgDir,
		repoDir:        strings.TrimSpace(opts.RepoDir),
		flakeValidator: opts.FlakeValidator,
		profileSyncer:  opts.ProfileSyncer,
		onReload:       opts.OnReload,
		planner:        opts.Planner,
	}
}

func defaultTUIConfig() TUIConfig {
	return TUIConfig{
		Theme:     "tokyo-night",
		LeaderKey: "ctrl+space",
		MouseMode: true,
		Keybindings: map[string]string{
			"launch_tool":      "c",
			"split_vertical":   "v",
			"split_horizontal": "s",
			"zoom":             "z",
			"close_pane":       "x",
			"repo_switcher":    "r",
			"issue_picker":     "i",
			"ai_configurator":  "a",
		},
	}
}

// GetMachineConfig returns a structured snapshot of current TUI config, shell profile, and repo flake status.
func (c *Configurator) GetMachineConfig() (MachineConfigSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getMachineConfigLocked()
}

func (c *Configurator) getMachineConfigLocked() (MachineConfigSnapshot, error) {
	snap := MachineConfigSnapshot{
		TUI: defaultTUIConfig(),
	}

	tomlPath := filepath.Join(c.configDir, "tui.toml")
	if data, err := os.ReadFile(tomlPath); err == nil {
		var parsed TUIConfig
		if err := toml.Unmarshal(data, &parsed); err != nil {
			return MachineConfigSnapshot{}, fmt.Errorf("parse tui.toml: %w", err)
		}
		if parsed.Keybindings == nil {
			parsed.Keybindings = defaultTUIConfig().Keybindings
		}
		snap.TUI = parsed
	} else if !errors.Is(err, os.ErrNotExist) {
		return MachineConfigSnapshot{}, fmt.Errorf("read tui.toml: %w", err)
	}

	if data, err := os.ReadFile(filepath.Join(c.configDir, "profile.nix")); err == nil {
		snap.Shell.ProfileNix = string(data)
	}
	if data, err := os.ReadFile(filepath.Join(c.configDir, "init.zsh")); err == nil {
		snap.Shell.InitZsh = string(data)
	}

	if c.repoDir != "" {
		flakePath := filepath.Join(c.repoDir, "flake.nix")
		if data, err := os.ReadFile(flakePath); err == nil {
			snap.HasRepoFlake = true
			snap.RepoFlake = string(data)
		}
	}

	return snap, nil
}

// UpdateTUIConfig validates and writes tui.toml and immediately triggers OnReload.
func (c *Configurator) UpdateTUIConfig(cfg TUIConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updateTUIConfigLocked(cfg)
}

func (c *Configurator) updateTUIConfigLocked(cfg TUIConfig) error {
	cfg.Theme = strings.TrimSpace(cfg.Theme)
	cfg.LeaderKey = strings.TrimSpace(cfg.LeaderKey)
	if cfg.Theme == "" {
		return errors.New("tui config theme must not be empty")
	}
	if cfg.LeaderKey == "" {
		return errors.New("tui config leader_key must not be empty")
	}
	if cfg.Keybindings == nil {
		cfg.Keybindings = defaultTUIConfig().Keybindings
	}

	encoded, _ := toml.Marshal(cfg)
	tomlPath := filepath.Join(c.configDir, "tui.toml")
	if err := writeFileAtomic(tomlPath, encoded, 0o644); err != nil {
		return fmt.Errorf("write tui.toml: %w", err)
	}

	if c.onReload != nil {
		c.onReload(cfg)
	}
	return nil
}

// SetShellProfile writes user shell/Nix profile config under ConfigDir (profile.nix and init.zsh).
func (c *Configurator) SetShellProfile(profile ShellProfile) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.setShellProfileLocked(profile)
}

func (c *Configurator) setShellProfileLocked(profile ShellProfile) error {
	if strings.TrimSpace(profile.ProfileNix) == "" && strings.TrimSpace(profile.InitZsh) == "" {
		return errors.New("shell profile must specify profile_nix or init_zsh")
	}
	if strings.TrimSpace(profile.ProfileNix) != "" {
		if err := writeFileAtomic(filepath.Join(c.configDir, "profile.nix"), []byte(profile.ProfileNix), 0o644); err != nil {
			return fmt.Errorf("write profile.nix: %w", err)
		}
	}
	if strings.TrimSpace(profile.InitZsh) != "" {
		if err := writeFileAtomic(filepath.Join(c.configDir, "init.zsh"), []byte(profile.InitZsh), 0o644); err != nil {
			return fmt.Errorf("write init.zsh: %w", err)
		}
	}
	return nil
}

// EditRepoFlake writes a candidate flake.nix in RepoDir, runs FlakeValidator, and
// atomically rolls back to the previous flake.nix contents if validation fails.
func (c *Configurator) EditRepoFlake(ctx context.Context, flakeContent string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.editRepoFlakeLocked(ctx, flakeContent)
}

func (c *Configurator) editRepoFlakeLocked(ctx context.Context, flakeContent string) error {
	if c.repoDir == "" {
		return errors.New("repoDir is not configured")
	}
	if strings.TrimSpace(flakeContent) == "" {
		return errors.New("flake.nix content must not be empty")
	}

	flakePath := filepath.Join(c.repoDir, "flake.nix")
	prevBytes, prevErr := os.ReadFile(flakePath)
	hadPrev := prevErr == nil
	if prevErr != nil && !errors.Is(prevErr, os.ErrNotExist) {
		return fmt.Errorf("read existing flake.nix: %w", prevErr)
	}

	if err := writeFileAtomic(flakePath, []byte(flakeContent), 0o644); err != nil {
		return fmt.Errorf("write candidate flake.nix: %w", err)
	}

	if c.flakeValidator != nil {
		if err := c.flakeValidator(ctx, c.repoDir); err != nil {
			if hadPrev {
				_ = writeFileAtomic(flakePath, prevBytes, 0o644)
			} else {
				_ = os.Remove(flakePath)
			}
			return fmt.Errorf("validate flake.nix (rolled back): %w", err)
		}
	}
	return nil
}

// SyncProfileNow invokes the configured ProfileSyncer to upload the user's home vault immediately.
func (c *Configurator) SyncProfileNow(ctx context.Context) error {
	c.mu.Lock()
	syncer := c.profileSyncer
	c.mu.Unlock()

	if syncer == nil {
		return errors.New("profile syncer is not configured")
	}
	if err := syncer(ctx); err != nil {
		return fmt.Errorf("sync profile now: %w", err)
	}
	return nil
}

// WireOrbitMCP writes Orbit MCP server entries (URL + Bearer session token) into
// ~/.claude.json, ~/.config/opencode/opencode.json, and ~/.gemini/settings.json.
func (c *Configurator) WireOrbitMCP(mcp MCPConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wireOrbitMCPLocked(mcp)
}

func (c *Configurator) wireOrbitMCPLocked(mcp MCPConfig) error {
	mcp.ServerURL = strings.TrimSpace(mcp.ServerURL)
	mcp.BearerToken = strings.TrimSpace(mcp.BearerToken)
	if mcp.ServerURL == "" || mcp.BearerToken == "" {
		return errors.New("mcp server_url and bearer_token are required")
	}

	payload := map[string]any{
		"mcpServers": map[string]any{
			"orbit": map[string]any{
				"type": "http",
				"url":  mcp.ServerURL,
				"headers": map[string]string{
					"Authorization": "Bearer " + mcp.BearerToken,
				},
			},
		},
	}
	encoded, _ := json.MarshalIndent(payload, "", "  ")
	targets := []string{
		filepath.Join(c.homeDir, ".claude.json"),
		filepath.Join(c.homeDir, ".config", "opencode", "opencode.json"),
		filepath.Join(c.homeDir, ".gemini", "settings.json"),
	}
	for _, target := range targets {
		if err := writeFileAtomic(target, encoded, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
	}
	return nil
}

type editFlakeArgs struct {
	FlakeNix string `json:"flake_nix"`
}

// ExecuteTool dispatches any of the 6 structured AI tools by name from JSON arguments.
func (c *Configurator) ExecuteTool(ctx context.Context, call ToolCall) (ToolResult, error) {
	switch call.Name {
	case ToolGetMachineConfig:
		snap, err := c.GetMachineConfig()
		if err != nil {
			c.appendHistory("tool", fmt.Sprintf("%s failed: %v", call.Name, err))
			return ToolResult{ToolName: call.Name, Summary: err.Error()}, err
		}
		summary := fmt.Sprintf("Loaded machine config (theme=%s, leader=%s, flake=%v)", snap.TUI.Theme, snap.TUI.LeaderKey, snap.HasRepoFlake)
		c.appendHistory("tool", fmt.Sprintf("%s: %s", call.Name, summary))
		return ToolResult{ToolName: call.Name, Success: true, Summary: summary, Snapshot: &snap}, nil

	case ToolUpdateTUIConfig:
		var cfg TUIConfig
		if err := json.Unmarshal(call.Arguments, &cfg); err != nil {
			return ToolResult{ToolName: call.Name, Summary: err.Error()}, fmt.Errorf("decode UpdateTUIConfig args: %w", err)
		}
		if err := c.UpdateTUIConfig(cfg); err != nil {
			c.appendHistory("tool", fmt.Sprintf("%s failed: %v", call.Name, err))
			return ToolResult{ToolName: call.Name, Summary: err.Error()}, err
		}
		summary := fmt.Sprintf("Updated TUI config (theme=%s, leader=%s)", cfg.Theme, cfg.LeaderKey)
		c.appendHistory("tool", fmt.Sprintf("%s: %s", call.Name, summary))
		return ToolResult{ToolName: call.Name, Success: true, Summary: summary}, nil

	case ToolSetShellProfile:
		var profile ShellProfile
		if err := json.Unmarshal(call.Arguments, &profile); err != nil {
			return ToolResult{ToolName: call.Name, Summary: err.Error()}, fmt.Errorf("decode SetShellProfile args: %w", err)
		}
		if err := c.SetShellProfile(profile); err != nil {
			c.appendHistory("tool", fmt.Sprintf("%s failed: %v", call.Name, err))
			return ToolResult{ToolName: call.Name, Summary: err.Error()}, err
		}
		summary := "Updated shell profile (profile.nix / init.zsh)"
		c.appendHistory("tool", fmt.Sprintf("%s: %s", call.Name, summary))
		return ToolResult{ToolName: call.Name, Success: true, Summary: summary}, nil

	case ToolEditRepoFlake:
		var args editFlakeArgs
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return ToolResult{ToolName: call.Name, Summary: err.Error()}, fmt.Errorf("decode EditRepoFlake args: %w", err)
		}
		if err := c.EditRepoFlake(ctx, args.FlakeNix); err != nil {
			c.appendHistory("tool", fmt.Sprintf("%s failed: %v", call.Name, err))
			return ToolResult{ToolName: call.Name, Summary: err.Error()}, err
		}
		summary := "Validated and updated repository flake.nix"
		c.appendHistory("tool", fmt.Sprintf("%s: %s", call.Name, summary))
		return ToolResult{ToolName: call.Name, Success: true, Summary: summary}, nil

	case ToolSyncProfileNow:
		if err := c.SyncProfileNow(ctx); err != nil {
			c.appendHistory("tool", fmt.Sprintf("%s failed: %v", call.Name, err))
			return ToolResult{ToolName: call.Name, Summary: err.Error()}, err
		}
		summary := "Synced user profile vault to S3/MinIO"
		c.appendHistory("tool", fmt.Sprintf("%s: %s", call.Name, summary))
		return ToolResult{ToolName: call.Name, Success: true, Summary: summary}, nil

	case ToolWireOrbitMCP:
		var mcp MCPConfig
		if err := json.Unmarshal(call.Arguments, &mcp); err != nil {
			return ToolResult{ToolName: call.Name, Summary: err.Error()}, fmt.Errorf("decode WireOrbitMCP args: %w", err)
		}
		if err := c.WireOrbitMCP(mcp); err != nil {
			c.appendHistory("tool", fmt.Sprintf("%s failed: %v", call.Name, err))
			return ToolResult{ToolName: call.Name, Summary: err.Error()}, err
		}
		summary := "Wired Orbit MCP server into Claude Code, OpenCode, and Gemini CLI"
		c.appendHistory("tool", fmt.Sprintf("%s: %s", call.Name, summary))
		return ToolResult{ToolName: call.Name, Success: true, Summary: summary}, nil

	default:
		err := fmt.Errorf("unknown AI configurator tool %q", call.Name)
		return ToolResult{ToolName: call.Name, Summary: err.Error()}, err
	}
}

// Ask sends a natural-language user prompt to the configured Planner, executes any planned
// tool calls, and records the conversation in the modal history.
func (c *Configurator) Ask(ctx context.Context, prompt string) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", errors.New("prompt must not be empty")
	}
	c.appendHistory("user", prompt)

	snap, err := c.GetMachineConfig()
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	planner := c.planner
	c.mu.Unlock()
	if planner == nil {
		return "", errors.New("ai planner is not configured")
	}

	calls, reply, err := planner(ctx, prompt, snap)
	if err != nil {
		return "", fmt.Errorf("plan configuration changes: %w", err)
	}

	for _, call := range calls {
		if _, err := c.ExecuteTool(ctx, call); err != nil {
			return "", err
		}
	}

	if strings.TrimSpace(reply) != "" {
		c.appendHistory("assistant", reply)
	}
	return reply, nil
}

// SetInput updates the current text in the modal's prompt input field.
func (c *Configurator) SetInput(input string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.input = input
}

// History returns a copy of the conversation messages recorded so far.
func (c *Configurator) History() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, len(c.history))
	copy(out, c.history)
	return out
}

// View renders the Lip Gloss AI Configurator modal overlay for the TUI compositor.
func (c *Configurator) View(width, _ int) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	boxWidth := min(max(width-8, 44), 96)
	var lines []string
	lines = append(lines, "✦ AI Configurator — Configure through an AI  [Esc to close]")
	lines = append(lines, "Tools: GetMachineConfig | UpdateTUIConfig | SetShellProfile | EditRepoFlake | SyncProfileNow | WireOrbitMCP")
	lines = append(lines, "")

	if len(c.history) == 0 {
		lines = append(lines, "Describe your desired keybindings, theme, shell profile, or flake packages.")
	} else {
		for _, msg := range c.history {
			lines = append(lines, fmt.Sprintf("[%s] %s", strings.ToUpper(msg.Role), msg.Content))
		}
	}

	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("Prompt > %s", c.input))

	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		Padding(1, 2).
		Width(boxWidth).
		Render(strings.Join(lines, "\n"))
}

func (c *Configurator) appendHistory(role, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.history = append(c.history, Message{Role: role, Content: content})
}

func writeFileAtomic(targetPath string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpPath := targetPath + ".tmp"
	if err := os.WriteFile(tmpPath, content, perm); err != nil {
		return err
	}
	return os.Rename(tmpPath, targetPath)
}
