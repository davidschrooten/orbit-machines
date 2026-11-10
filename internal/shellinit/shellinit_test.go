package shellinit_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/shellinit"
)

func TestEnsureInitialized_WritesStarshipJetpackDirenvAndWelcomeBashrc(t *testing.T) {
	t.Parallel()

	if err := shellinit.EnsureConfig(""); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty homeDir in EnsureConfig, got %v", err)
	}
	if err := shellinit.EnsureInitialized("", "/tmp/ws"); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty homeDir, got %v", err)
	}
	if err := shellinit.EnsureInitialized("/tmp/home", "   "); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty workDir, got %v", err)
	}

	homeDir := filepath.Join(t.TempDir(), "orbit-home")
	workDir := filepath.Join(homeDir, "workspace")

	if err := shellinit.EnsureConfig(homeDir); err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}

	// Verify directories created
	for _, dir := range []string{
		filepath.Join(homeDir, ".config"),
		filepath.Join(homeDir, ".config", "direnv"),
		filepath.Join(homeDir, ".config", "orbit"),
		workDir,
	} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("expected directory %s to exist, err=%v", dir, err)
		}
	}

	// Verify starship.toml contains Jetpack preset
	starshipPath := filepath.Join(homeDir, ".config", "starship.toml")
	starshipBytes, err := os.ReadFile(starshipPath)
	if err != nil {
		t.Fatalf("ReadFile(starship.toml): %v", err)
	}
	starshipStr := string(starshipBytes)
	for _, needle := range []string{
		"# orbit-preset: jetpack",
		`continuation_prompt = "[▸▹ ](dimmed white)"`,
		"right_format =",
		"[character]",
		"[username]",
		"[directory]",
		"[git_branch]",
		"[git_metrics]",
		"[git_status]",
		"[nix_shell]",
	} {
		if !strings.Contains(starshipStr, needle) {
			t.Errorf("starship.toml missing %q", needle)
		}
	}

	// Verify legacy starship.toml without `# orbit-preset: jetpack` gets upgraded automatically
	if err := os.WriteFile(starshipPath, []byte("# old tokyo night config\nformat = \"$all\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile legacy starship: %v", err)
	}
	if err := shellinit.EnsureInitialized(homeDir, workDir); err != nil {
		t.Fatalf("EnsureInitialized upgrade: %v", err)
	}
	upgradedBytes, _ := os.ReadFile(starshipPath)
	if !strings.Contains(string(upgradedBytes), "# orbit-preset: jetpack") {
		t.Fatalf("expected legacy starship.toml to be upgraded to jetpack preset")
	}

	// Verify direnv.toml whitelists /home/orbit/workspace and workDir
	direnvPath := filepath.Join(homeDir, ".config", "direnv", "direnv.toml")
	direnvBytes, err := os.ReadFile(direnvPath)
	if err != nil {
		t.Fatalf("ReadFile(direnv.toml): %v", err)
	}
	direnvStr := string(direnvBytes)
	if !strings.Contains(direnvStr, "[whitelist]") || !strings.Contains(direnvStr, "/home/orbit/workspace") || !strings.Contains(direnvStr, workDir) {
		t.Errorf("unexpected direnv.toml:\n%s", direnvStr)
	}

	// Verify .bashrc and .bash_profile contain direnv hook, starship init, env.sh sourcing, and welcome banner
	bashrcPath := filepath.Join(homeDir, ".bashrc")
	bashrcBytes, err := os.ReadFile(bashrcPath)
	if err != nil {
		t.Fatalf("ReadFile(.bashrc): %v", err)
	}
	bashrcStr := string(bashrcBytes)
	for _, needle := range []string{
		".config/orbit/env.sh",
		`eval "$(direnv hook bash)"`,
		`eval "$(starship init bash)"`,
		"STARSHIP_CONFIG",
		"NPM_CONFIG_PREFIX=",
		"/.local/bin:",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=",
		"if [[ $- == *i* ]]; then",
		"Welcome to Orbit Machine",
		"orbit repos",
		"orbit flake init [stack]",
		"orbit packages",
		"orbit tools [tool...]",
		"orbit git",
		"orbit ssh-key",
		"orbit status",
	} {
		if !strings.Contains(bashrcStr, needle) {
			t.Errorf(".bashrc missing %q", needle)
		}
	}

	profilePath := filepath.Join(homeDir, ".bash_profile")
	profileBytes, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("ReadFile(.bash_profile): %v", err)
	}
	if !strings.Contains(string(profileBytes), ".bashrc") {
		t.Errorf(".bash_profile does not source .bashrc:\n%s", string(profileBytes))
	}

	// Calling EnsureInitialized a second time preserves custom starship.toml edits when # orbit-preset: jetpack is present
	customStarship := "# user custom starship\n" + starshipStr
	if err := os.WriteFile(starshipPath, []byte(customStarship), 0o644); err != nil {
		t.Fatalf("WriteFile custom starship: %v", err)
	}
	// Also test replacing an older shellinit block in .bashrc in-place
	oldBashrc := "alias ll='ls -la'\n# >>> orbit-machines shellinit >>>\nold block\n# <<< orbit-machines shellinit <<<\nexport AFTER=1\n"
	if err := os.WriteFile(bashrcPath, []byte(oldBashrc), 0o644); err != nil {
		t.Fatalf("WriteFile old .bashrc: %v", err)
	}
	if err := shellinit.EnsureInitialized(homeDir, workDir); err != nil {
		t.Fatalf("second EnsureInitialized: %v", err)
	}
	// Run once more with standard markers already present to verify in-place replacement of standard markers too
	if err := shellinit.EnsureInitialized(homeDir, workDir); err != nil {
		t.Fatalf("third EnsureInitialized: %v", err)
	}
	afterStarship, _ := os.ReadFile(starshipPath)
	if string(afterStarship) != customStarship {
		t.Fatalf("expected existing jetpack starship.toml to be preserved")
	}
	afterBashrc, _ := os.ReadFile(bashrcPath)
	if strings.Count(string(afterBashrc), ">>> orbit-machines shell init >>>") != 1 {
		t.Fatalf("expected .bashrc init block to appear exactly once, got:\n%s", string(afterBashrc))
	}
	if !strings.Contains(string(afterBashrc), "Welcome to Orbit Machine") || !strings.Contains(string(afterBashrc), "export AFTER=1") {
		t.Fatalf("expected old .bashrc block to be replaced in-place preserving surrounding lines, got:\n%s", string(afterBashrc))
	}

	// Test pre-existing .bashrc without trailing newline
	homeDir2 := filepath.Join(t.TempDir(), "home2")
	_ = os.MkdirAll(homeDir2, 0o755)
	_ = os.WriteFile(filepath.Join(homeDir2, ".bashrc"), []byte("alias ll='ls -la'"), 0o644)
	if err := shellinit.EnsureInitialized(homeDir2, filepath.Join(homeDir2, "ws")); err != nil {
		t.Fatalf("EnsureInitialized with existing .bashrc: %v", err)
	}
	b2, _ := os.ReadFile(filepath.Join(homeDir2, ".bashrc"))
	if !strings.Contains(string(b2), "alias ll='ls -la'\n\n# >>> orbit-machines shell init >>>") {
		t.Fatalf("unexpected merged .bashrc:\n%s", string(b2))
	}

	// Test filesystem error when homeDir is a file instead of a directory, or starship.toml is a directory
	blockingFile := filepath.Join(t.TempDir(), "file-not-dir")
	_ = os.WriteFile(blockingFile, []byte("x"), 0o644)
	if err := shellinit.EnsureInitialized(blockingFile, filepath.Join(blockingFile, "ws")); err == nil {
		t.Fatalf("expected error when homeDir is an existing regular file")
	}
	homeDir3 := filepath.Join(t.TempDir(), "home3")
	_ = os.MkdirAll(filepath.Join(homeDir3, ".config", "starship.toml"), 0o755)
	if err := shellinit.EnsureInitialized(homeDir3, filepath.Join(homeDir3, "ws")); err == nil {
		t.Fatalf("expected error when starship.toml is a directory")
	}
	homeDir4 := filepath.Join(t.TempDir(), "home4")
	_ = os.MkdirAll(filepath.Join(homeDir4, ".bashrc"), 0o755)
	if err := shellinit.EnsureInitialized(homeDir4, filepath.Join(homeDir4, "ws")); err == nil {
		t.Fatalf("expected error when .bashrc is a directory")
	}

	// Verify legacy configuration.nix without nodejs is upgraded automatically and pre-existing .local/bin files migrate to workspace PVC
	homeDir5 := filepath.Join(t.TempDir(), "home5")
	workDir5 := filepath.Join(homeDir5, "workspace")
	_ = os.MkdirAll(filepath.Join(homeDir5, ".config", "orbit"), 0o755)
	_ = os.WriteFile(filepath.Join(homeDir5, ".config", "orbit", "configuration.nix"), []byte("pkgs.buildEnv {\n  paths = with pkgs; [\n    git\n  ];\n}\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(homeDir5, ".local", "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(homeDir5, ".local", "bin", "agy"), []byte("#!/bin/sh\n"), 0o755)
	if err := shellinit.EnsureInitialized(homeDir5, workDir5); err != nil {
		t.Fatalf("EnsureInitialized homeDir5: %v", err)
	}
	cfgNix5, _ := os.ReadFile(filepath.Join(homeDir5, ".config", "orbit", "configuration.nix"))
	if !strings.Contains(string(cfgNix5), "nodejs") {
		t.Fatalf("expected legacy configuration.nix to be upgraded with nodejs, got:\n%s", string(cfgNix5))
	}
	if _, err := os.Stat(filepath.Join(workDir5, ".orbit", "local", "bin", "agy")); err != nil {
		t.Fatalf("expected pre-existing .local/bin/agy to be migrated to workspace .orbit/local/bin/agy: %v", err)
	}
}

func TestPersistentHomeAcrossPodRestarts_ClaudeAntigravityAndOpenCode(t *testing.T) {
	t.Parallel()

	// Persistent workspace PVC shared across Pod restarts
	workDir := filepath.Join(t.TempDir(), "workspace-pvc")

	// Pod 1 starts with ephemeral homeDir1
	homeDir1 := filepath.Join(t.TempDir(), "pod1-home")
	if err := shellinit.EnsureInitialized(homeDir1, workDir); err != nil {
		t.Fatalf("Pod 1 EnsureInitialized: %v", err)
	}

	// Verify ~/.claude.json and ~/.bash_history are pre-seeded on PVC and symlinked so realpath() resolves
	resolvedClaudeJSON, err := filepath.EvalSymlinks(filepath.Join(homeDir1, ".claude.json"))
	if err != nil || resolvedClaudeJSON != filepath.Join(workDir, ".orbit", "home", ".claude.json") {
		t.Fatalf("expected ~/.claude.json symlink target to resolve to PVC, got (%q, %v)", resolvedClaudeJSON, err)
	}

	// 1. Simulate Claude Code login and repository conversation history in Pod 1
	if err := os.MkdirAll(filepath.Join(homeDir1, ".claude", "projects", "-home-orbit-workspace-my-repo"), 0o700); err != nil {
		t.Fatalf("MkdirAll .claude/projects: %v", err)
	}
	claudeCreds := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-secret","refreshToken":"sk-ant-ort01-secret"}}`
	if err := os.WriteFile(filepath.Join(homeDir1, ".claude", ".credentials.json"), []byte(claudeCreds), 0o600); err != nil {
		t.Fatalf("WriteFile .credentials.json: %v", err)
	}
	claudeSession := "{\"type\":\"user\",\"message\":\"Fix auth bug\"}\n{\"type\":\"assistant\",\"message\":\"Done\"}\n"
	if err := os.WriteFile(filepath.Join(homeDir1, ".claude", "projects", "-home-orbit-workspace-my-repo", "session-1.jsonl"), []byte(claudeSession), 0o600); err != nil {
		t.Fatalf("WriteFile session-1.jsonl: %v", err)
	}
	// Simulate Claude Code atomic write: write .claude.json.tmp in homeDir1 and os.Rename over .claude.json (replacing the symlink!)
	claudeStateJSON := `{"oauthAccount":{"accountUuid":"acc_123","emailAddress":"dev@acme.dev"},"hasCompletedOnboarding":true,"projects":{"/home/orbit/workspace/my-repo":{"hasTrustDialogAccepted":true,"lastSessionId":"session-1"}}}`
	tmpClaudePath := filepath.Join(homeDir1, ".claude.json.tmp.999")
	if err := os.WriteFile(tmpClaudePath, []byte(claudeStateJSON), 0o600); err != nil {
		t.Fatalf("WriteFile .claude.json.tmp: %v", err)
	}
	if err := os.Rename(tmpClaudePath, filepath.Join(homeDir1, ".claude.json")); err != nil {
		t.Fatalf("Rename .claude.json: %v", err)
	}

	// 2. Simulate Antigravity CLI (agy) login, custom settings, and conversation history in Pod 1
	for _, sub := range []string{
		filepath.Join(homeDir1, ".gemini", "antigravity-cli", "conversations"),
		filepath.Join(homeDir1, ".gemini", "antigravity-cli", "brain", "conv-1"),
	} {
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatalf("MkdirAll %s: %v", sub, err)
		}
	}
	agyToken := `{"access_token":"ya29.agy-oauth-token","refresh_token":"1//agy-refresh"}`
	agySettings := `{"model":"gemini-2.5-pro","autoApproveSafeCommands":true}`
	agyConvPB := "PROTO_CONVERSATION_STATE_1"
	agyTranscript := "{\"step_index\":0,\"type\":\"USER_INPUT\",\"content\":\"Build feature\"}\n"
	_ = os.WriteFile(filepath.Join(homeDir1, ".gemini", "antigravity-cli", "antigravity-oauth-token"), []byte(agyToken), 0o600)
	_ = os.WriteFile(filepath.Join(homeDir1, ".gemini", "antigravity-cli", "settings.json"), []byte(agySettings), 0o600)
	_ = os.WriteFile(filepath.Join(homeDir1, ".gemini", "antigravity-cli", "conversations", "conv-1.pb"), []byte(agyConvPB), 0o600)
	_ = os.WriteFile(filepath.Join(homeDir1, ".gemini", "antigravity-cli", "brain", "conv-1", "transcript.jsonl"), []byte(agyTranscript), 0o600)

	// 3. Simulate OpenCode login & conversation history in ~/.local/share/opencode
	_ = os.MkdirAll(filepath.Join(homeDir1, ".local", "share", "opencode", "project", "my-repo"), 0o700)
	_ = os.WriteFile(filepath.Join(homeDir1, ".local", "share", "opencode", "auth.json"), []byte(`{"anthropic":{"key":"sk-ant-opencode"}}`), 0o600)
	_ = os.WriteFile(filepath.Join(homeDir1, ".local", "share", "opencode", "project", "my-repo", "session.json"), []byte(`{"id":"oc-sess-1"}`), 0o600)

	// SyncPersistentHomeFiles moves the atomically replaced .claude.json back to PVC and re-links it
	shellinit.SyncPersistentHomeFiles(homeDir1, workDir)

	// Destroy Pod 1 ephemeral homeDir1 completely!
	if err := os.RemoveAll(homeDir1); err != nil {
		t.Fatalf("RemoveAll(homeDir1): %v", err)
	}

	// Pod 2 boots with a brand new empty ephemeral homeDir2 and the same persistent workDir
	homeDir2 := filepath.Join(t.TempDir(), "pod2-home")
	if err := shellinit.EnsureInitialized(homeDir2, workDir); err != nil {
		t.Fatalf("Pod 2 EnsureInitialized: %v", err)
	}

	for relPath, want := range map[string]string{
		".claude/.credentials.json":                                         claudeCreds,
		".claude/projects/-home-orbit-workspace-my-repo/session-1.jsonl":    claudeSession,
		".claude.json":                                                      claudeStateJSON,
		".gemini/antigravity-cli/antigravity-oauth-token":                   agyToken,
		".gemini/antigravity-cli/settings.json":                             agySettings,
		".gemini/antigravity-cli/conversations/conv-1.pb":                   agyConvPB,
		".gemini/antigravity-cli/brain/conv-1/transcript.jsonl":             agyTranscript,
		".local/share/opencode/auth.json":                                   `{"anthropic":{"key":"sk-ant-opencode"}}`,
		".local/share/opencode/project/my-repo/session.json":                `{"id":"oc-sess-1"}`,
	} {
		gotBytes, err := os.ReadFile(filepath.Join(homeDir2, relPath))
		if err != nil {
			t.Fatalf("Pod 2 ReadFile(%s): %v", relPath, err)
		}
		if string(gotBytes) != want {
			t.Errorf("Pod 2 %s = %q, want %q", relPath, string(gotBytes), want)
		}
	}
}
