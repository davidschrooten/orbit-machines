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
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=",
		"if [[ $- == *i* ]]; then",
		"Welcome to Orbit Machine",
		"orbit repos",
		"orbit flake init [stack]",
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
}
