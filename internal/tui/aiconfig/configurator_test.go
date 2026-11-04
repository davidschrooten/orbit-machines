package aiconfig_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidschrooten/orbit-machines/internal/tui/aiconfig"
)

func TestGetAndUpdateTUIConfig_WritesTomlAndTriggersLiveReload(t *testing.T) {
	t.Parallel()
	homeDir := t.TempDir()
	repoDir := t.TempDir()

	var reloaded []aiconfig.TUIConfig
	cfg := aiconfig.New(aiconfig.Options{
		HomeDir: homeDir,
		RepoDir: repoDir,
		OnReload: func(c aiconfig.TUIConfig) {
			reloaded = append(reloaded, c)
		},
	})

	snap, err := cfg.GetMachineConfig()
	if err != nil {
		t.Fatalf("GetMachineConfig default failed: %v", err)
	}
	if snap.TUI.Theme != "tokyo-night" || snap.TUI.LeaderKey != "ctrl+space" || !snap.TUI.MouseMode {
		t.Fatalf("unexpected default TUI config: %+v", snap.TUI)
	}
	if snap.HasRepoFlake {
		t.Fatalf("expected HasRepoFlake=false for empty repoDir")
	}

	updated := aiconfig.TUIConfig{
		Theme:     "catppuccin",
		LeaderKey: "ctrl+a",
		MouseMode: true,
		Keybindings: map[string]string{
			"split_vertical":   "v",
			"split_horizontal": "s",
		},
	}
	if err := cfg.UpdateTUIConfig(updated); err != nil {
		t.Fatalf("UpdateTUIConfig failed: %v", err)
	}

	if len(reloaded) != 1 || reloaded[0].LeaderKey != "ctrl+a" || reloaded[0].Theme != "catppuccin" {
		t.Fatalf("expected OnReload to be called once with updated config, got %+v", reloaded)
	}

	tomlPath := filepath.Join(homeDir, ".config", "orbit", "tui.toml")
	if _, err := os.Stat(tomlPath); err != nil {
		t.Fatalf("expected tui.toml to be written at %s: %v", tomlPath, err)
	}

	snapAfter, err := cfg.GetMachineConfig()
	if err != nil {
		t.Fatalf("GetMachineConfig after update failed: %v", err)
	}
	if snapAfter.TUI.Theme != "catppuccin" || snapAfter.TUI.LeaderKey != "ctrl+a" {
		t.Fatalf("expected persisted TUI config, got %+v", snapAfter.TUI)
	}
	if snapAfter.TUI.Keybindings["split_vertical"] != "v" {
		t.Fatalf("expected persisted keybindings, got %+v", snapAfter.TUI.Keybindings)
	}

	// Invalid leader_key or theme must be rejected without triggering OnReload.
	if err := cfg.UpdateTUIConfig(aiconfig.TUIConfig{Theme: "", LeaderKey: "ctrl+a"}); err == nil {
		t.Fatalf("expected empty theme to fail validation")
	}
	if err := cfg.UpdateTUIConfig(aiconfig.TUIConfig{Theme: "dracula", LeaderKey: ""}); err == nil {
		t.Fatalf("expected empty leader_key to fail validation")
	}
	if len(reloaded) != 1 {
		t.Fatalf("expected OnReload not to fire on invalid config")
	}
}

func TestSetShellProfileAndSyncProfileNow(t *testing.T) {
	t.Parallel()
	homeDir := t.TempDir()
	synced := 0

	cfg := aiconfig.New(aiconfig.Options{
		HomeDir: homeDir,
		ProfileSyncer: func(_ context.Context) error {
			synced++
			return nil
		},
	})

	if err := cfg.SetShellProfile(aiconfig.ShellProfile{}); err == nil {
		t.Fatalf("expected empty ShellProfile to fail validation")
	}

	profile := aiconfig.ShellProfile{
		ProfileNix: "{ pkgs }: [ pkgs.jq pkgs.ripgrep ]\n",
		InitZsh:    "alias co='git checkout'\nbindkey -v\n",
	}
	if err := cfg.SetShellProfile(profile); err != nil {
		t.Fatalf("SetShellProfile failed: %v", err)
	}

	snap, err := cfg.GetMachineConfig()
	if err != nil {
		t.Fatalf("GetMachineConfig failed: %v", err)
	}
	if snap.Shell.ProfileNix != profile.ProfileNix || snap.Shell.InitZsh != profile.InitZsh {
		t.Fatalf("unexpected shell profile snapshot: %+v", snap.Shell)
	}

	if err := cfg.SyncProfileNow(context.Background()); err != nil {
		t.Fatalf("SyncProfileNow failed: %v", err)
	}
	if synced != 1 {
		t.Fatalf("expected ProfileSyncer to be called once, got %d", synced)
	}
}

func TestEditRepoFlake_ValidatesAndAtomicallyRollsBackOnError(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	flakePath := filepath.Join(repoDir, "flake.nix")

	validatorErr := errors.New("error: syntax error, unexpected '}', expecting ';'")
	shouldFail := true

	cfg := aiconfig.New(aiconfig.Options{
		HomeDir: t.TempDir(),
		RepoDir: repoDir,
		FlakeValidator: func(_ context.Context, dir string) error {
			data, err := os.ReadFile(filepath.Join(dir, "flake.nix"))
			if err != nil {
				return err
			}
			if shouldFail || strings.Contains(string(data), "BROKEN") {
				return validatorErr
			}
			return nil
		},
	})

	// 1. When flake.nix did not exist before and validation fails, flake.nix must be removed.
	if err := cfg.EditRepoFlake(context.Background(), "{ BROKEN "); err == nil {
		t.Fatalf("expected broken flake edit to fail")
	}
	if _, err := os.Stat(flakePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected flake.nix to be removed on rollback when it did not previously exist, stat err: %v", err)
	}

	// 2. Valid flake.nix is persisted.
	shouldFail = false
	goodFlake := `{ description = "valid flake"; outputs = { self }: {}; }`
	if err := cfg.EditRepoFlake(context.Background(), goodFlake); err != nil {
		t.Fatalf("expected valid flake edit to succeed, got: %v", err)
	}

	snap, err := cfg.GetMachineConfig()
	if err != nil {
		t.Fatalf("GetMachineConfig failed: %v", err)
	}
	if !snap.HasRepoFlake || snap.RepoFlake != goodFlake {
		t.Fatalf("expected goodFlake in snapshot, got HasRepoFlake=%v content=%q", snap.HasRepoFlake, snap.RepoFlake)
	}

	// 3. Subsequent invalid edit atomically restores goodFlake.
	if err := cfg.EditRepoFlake(context.Background(), "{ BROKEN_UPDATE "); err == nil {
		t.Fatalf("expected broken update to fail validation")
	}
	restoredBytes, err := os.ReadFile(flakePath)
	if err != nil {
		t.Fatalf("read flake.nix after rollback: %v", err)
	}
	if string(restoredBytes) != goodFlake {
		t.Fatalf("expected atomic rollback to %q, got %q", goodFlake, string(restoredBytes))
	}
}

func TestWireOrbitMCP_WritesClaudeOpenCodeAndGeminiConfigs(t *testing.T) {
	t.Parallel()
	homeDir := t.TempDir()
	cfg := aiconfig.New(aiconfig.Options{HomeDir: homeDir})

	if err := cfg.WireOrbitMCP(aiconfig.MCPConfig{}); err == nil {
		t.Fatalf("expected empty MCPConfig to fail validation")
	}

	mcp := aiconfig.MCPConfig{
		ServerURL:   "https://orbit.example.com/mcp",
		BearerToken: "obt_secret_token_123",
	}
	if err := cfg.WireOrbitMCP(mcp); err != nil {
		t.Fatalf("WireOrbitMCP failed: %v", err)
	}

	expectedPaths := []string{
		filepath.Join(homeDir, ".claude.json"),
		filepath.Join(homeDir, ".config", "opencode", "opencode.json"),
		filepath.Join(homeDir, ".gemini", "settings.json"),
	}
	for _, p := range expectedPaths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("expected MCP config file %s: %v", p, err)
		}
		text := string(raw)
		if !strings.Contains(text, mcp.ServerURL) || !strings.Contains(text, mcp.BearerToken) {
			t.Fatalf("expected %s to contain URL and token, got: %s", p, text)
		}
	}
}

func TestExecuteTool_DispatchesAllSixToolsAndHandlesErrors(t *testing.T) {
	t.Parallel()
	homeDir := t.TempDir()
	repoDir := t.TempDir()
	synced := 0

	cfg := aiconfig.New(aiconfig.Options{
		HomeDir: homeDir,
		RepoDir: repoDir,
		FlakeValidator: func(_ context.Context, _ string) error {
			return nil
		},
		ProfileSyncer: func(_ context.Context) error {
			synced++
			return nil
		},
	})

	ctx := context.Background()

	// 1. GetMachineConfig
	res, err := cfg.ExecuteTool(ctx, aiconfig.ToolCall{
		Name:      aiconfig.ToolGetMachineConfig,
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil || !res.Success || res.Snapshot == nil {
		t.Fatalf("GetMachineConfig tool failed: res=%+v err=%v", res, err)
	}

	// 2. UpdateTUIConfig
	res, err = cfg.ExecuteTool(ctx, aiconfig.ToolCall{
		Name:      aiconfig.ToolUpdateTUIConfig,
		Arguments: json.RawMessage(`{"theme":"dracula","leader_key":"ctrl+b","mouse_mode":true}`),
	})
	if err != nil || !res.Success {
		t.Fatalf("UpdateTUIConfig tool failed: res=%+v err=%v", res, err)
	}

	// 3. SetShellProfile
	res, err = cfg.ExecuteTool(ctx, aiconfig.ToolCall{
		Name:      aiconfig.ToolSetShellProfile,
		Arguments: json.RawMessage(`{"profile_nix":"{ pkgs }: [ pkgs.fd ]","init_zsh":"export EDITOR=nvim"}`),
	})
	if err != nil || !res.Success {
		t.Fatalf("SetShellProfile tool failed: res=%+v err=%v", res, err)
	}

	// 4. EditRepoFlake
	res, err = cfg.ExecuteTool(ctx, aiconfig.ToolCall{
		Name:      aiconfig.ToolEditRepoFlake,
		Arguments: json.RawMessage(`{"flake_nix":"{ description = \"repo\"; outputs = { self }: {}; }"}`),
	})
	if err != nil || !res.Success {
		t.Fatalf("EditRepoFlake tool failed: res=%+v err=%v", res, err)
	}

	// 5. SyncProfileNow
	res, err = cfg.ExecuteTool(ctx, aiconfig.ToolCall{
		Name:      aiconfig.ToolSyncProfileNow,
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil || !res.Success || synced != 1 {
		t.Fatalf("SyncProfileNow tool failed: res=%+v err=%v synced=%d", res, err, synced)
	}

	// 6. WireOrbitMCP
	res, err = cfg.ExecuteTool(ctx, aiconfig.ToolCall{
		Name:      aiconfig.ToolWireOrbitMCP,
		Arguments: json.RawMessage(`{"server_url":"https://orbit.test/mcp","bearer_token":"tok_abc"}`),
	})
	if err != nil || !res.Success {
		t.Fatalf("WireOrbitMCP tool failed: res=%+v err=%v", res, err)
	}

	// Unknown tool & malformed JSON cases
	if _, err := cfg.ExecuteTool(ctx, aiconfig.ToolCall{Name: "UnknownTool"}); err == nil {
		t.Fatalf("expected error for unknown tool")
	}
	for _, toolName := range []string{
		aiconfig.ToolUpdateTUIConfig,
		aiconfig.ToolSetShellProfile,
		aiconfig.ToolEditRepoFlake,
		aiconfig.ToolWireOrbitMCP,
	} {
		if _, err := cfg.ExecuteTool(ctx, aiconfig.ToolCall{
			Name:      toolName,
			Arguments: json.RawMessage(`{invalid`),
		}); err == nil {
			t.Fatalf("expected JSON error for %s", toolName)
		}
	}
}

func TestAskAndView_ExecutesPlannerToolsAndRendersModal(t *testing.T) {
	t.Parallel()
	homeDir := t.TempDir()
	repoDir := t.TempDir()

	cfg := aiconfig.New(aiconfig.Options{
		HomeDir: homeDir,
		RepoDir: repoDir,
		Planner: func(_ context.Context, _ string, _ aiconfig.MachineConfigSnapshot) ([]aiconfig.ToolCall, string, error) {
			return []aiconfig.ToolCall{
				{
					Name:      aiconfig.ToolUpdateTUIConfig,
					Arguments: json.RawMessage(`{"theme":"gruvbox","leader_key":"ctrl+a","mouse_mode":true}`),
				},
			}, "Updated your theme to gruvbox and leader key to Ctrl+A.", nil
		},
	})

	cfg.SetInput("Switch theme to gruvbox and leader to Ctrl+A")
	reply, err := cfg.Ask(context.Background(), "Switch theme to gruvbox and leader to Ctrl+A")
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if !strings.Contains(reply, "gruvbox") {
		t.Fatalf("unexpected reply: %s", reply)
	}

	history := cfg.History()
	if len(history) < 3 {
		t.Fatalf("expected user, tool, and assistant messages in history, got %d", len(history))
	}

	rendered := cfg.View(100, 30)
	for _, snippet := range []string{
		"AI Configurator",
		"Switch theme to gruvbox",
		"UpdateTUIConfig",
		"Updated your theme to gruvbox",
	} {
		if !strings.Contains(rendered, snippet) {
			t.Fatalf("expected View output to contain %q, got:\n%s", snippet, rendered)
		}
	}
}

func TestErrorPathsAndEdgeCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Default options (empty HomeDir, no RepoDir, no Syncer, no Planner) and empty View
	defaultCfg := aiconfig.New(aiconfig.Options{
		ConfigDir: t.TempDir(),
	})
	if v := defaultCfg.View(80, 24); !strings.Contains(v, "Describe your desired") {
		t.Fatalf("expected empty history prompt in View, got: %s", v)
	}
	if _, err := defaultCfg.Ask(ctx, "   "); err == nil {
		t.Fatalf("expected empty prompt error")
	}
	if _, err := defaultCfg.Ask(ctx, "configure theme"); err == nil {
		t.Fatalf("expected missing planner error")
	}
	if err := defaultCfg.SyncProfileNow(ctx); err == nil {
		t.Fatalf("expected missing profile syncer error")
	}
	if err := defaultCfg.EditRepoFlake(ctx, "{ outputs = {}; }"); err == nil {
		t.Fatalf("expected missing repoDir error")
	}

	repoDir := t.TempDir()
	cfgDir := t.TempDir()
	failingCfg := aiconfig.New(aiconfig.Options{
		HomeDir:   t.TempDir(),
		ConfigDir: cfgDir,
		RepoDir:   repoDir,
		FlakeValidator: func(_ context.Context, _ string) error {
			return errors.New("nix check failed")
		},
		ProfileSyncer: func(_ context.Context) error {
			return errors.New("s3 upload failed")
		},
		Planner: func(_ context.Context, prompt string, _ aiconfig.MachineConfigSnapshot) ([]aiconfig.ToolCall, string, error) {
			if prompt == "plan-error" {
				return nil, "", errors.New("llm unavailable")
			}
			return []aiconfig.ToolCall{
				{Name: aiconfig.ToolSyncProfileNow, Arguments: json.RawMessage(`{}`)},
			}, "should fail on tool", nil
		},
	})

	if err := failingCfg.EditRepoFlake(ctx, "   "); err == nil {
		t.Fatalf("expected empty flake content error")
	}

	// Tool execution error paths inside ExecuteTool
	for _, call := range []aiconfig.ToolCall{
		{Name: aiconfig.ToolUpdateTUIConfig, Arguments: json.RawMessage(`{"theme":"","leader_key":""}`)},
		{Name: aiconfig.ToolSetShellProfile, Arguments: json.RawMessage(`{}`)},
		{Name: aiconfig.ToolEditRepoFlake, Arguments: json.RawMessage(`{"flake_nix":"{ bad }"}`)},
		{Name: aiconfig.ToolSyncProfileNow, Arguments: json.RawMessage(`{}`)},
		{Name: aiconfig.ToolWireOrbitMCP, Arguments: json.RawMessage(`{}`)},
	} {
		res, err := failingCfg.ExecuteTool(ctx, call)
		if err == nil || res.Success {
			t.Fatalf("expected tool %s to fail, got res=%+v err=%v", call.Name, res, err)
		}
	}

	if _, err := failingCfg.Ask(ctx, "plan-error"); err == nil {
		t.Fatalf("expected planner error from Ask")
	}
	if _, err := failingCfg.Ask(ctx, "tool-error"); err == nil {
		t.Fatalf("expected tool execution error from Ask")
	}

	// Corrupt tui.toml triggers GetMachineConfig error
	if err := os.WriteFile(filepath.Join(cfgDir, "tui.toml"), []byte("not = [valid toml"), 0o644); err != nil {
		t.Fatalf("write corrupt tui.toml: %v", err)
	}
	if _, err := failingCfg.ExecuteTool(ctx, aiconfig.ToolCall{Name: aiconfig.ToolGetMachineConfig}); err == nil {
		t.Fatalf("expected GetMachineConfig to fail on corrupt tui.toml")
	}
	if _, err := failingCfg.Ask(ctx, "any prompt"); err == nil {
		t.Fatalf("expected Ask to fail when GetMachineConfig fails")
	}
}
