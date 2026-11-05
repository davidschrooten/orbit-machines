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

func TestEnsureInitialized_WritesStarshipTokyoNightDirenvAndBashrc(t *testing.T) {
	t.Parallel()

	if err := shellinit.EnsureInitialized("", "/tmp/ws"); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty homeDir, got %v", err)
	}
	if err := shellinit.EnsureInitialized("/tmp/home", "   "); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty workDir, got %v", err)
	}

	homeDir := filepath.Join(t.TempDir(), "orbit-home")
	workDir := filepath.Join(homeDir, "workspace")

	if err := shellinit.EnsureInitialized(homeDir, workDir); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
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

	// Verify starship.toml contains Tokyo Night preset with Orbit logo (🪐)
	starshipPath := filepath.Join(homeDir, ".config", "starship.toml")
	starshipBytes, err := os.ReadFile(starshipPath)
	if err != nil {
		t.Fatalf("ReadFile(starship.toml): %v", err)
	}
	starshipStr := string(starshipBytes)
	for _, needle := range []string{
		"[░▒▓](#a3aed2)",
		"[ 🪐 ](bg:#a3aed2 fg:#090c0c)",
		"$directory",
		"$git_branch",
		"$nix_shell",
		"[time]",
	} {
		if !strings.Contains(starshipStr, needle) {
			t.Errorf("starship.toml missing %q", needle)
		}
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

	// Verify .bashrc and .bash_profile contain direnv hook, starship init, and env.sh sourcing
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

	// Calling EnsureInitialized a second time is idempotent and preserves custom starship.toml edits
	customStarship := "# user custom starship\n" + starshipStr
	if err := os.WriteFile(starshipPath, []byte(customStarship), 0o644); err != nil {
		t.Fatalf("WriteFile custom starship: %v", err)
	}
	if err := shellinit.EnsureInitialized(homeDir, workDir); err != nil {
		t.Fatalf("second EnsureInitialized: %v", err)
	}
	afterStarship, _ := os.ReadFile(starshipPath)
	if string(afterStarship) != customStarship {
		t.Fatalf("expected existing starship.toml to be preserved")
	}
	afterBashrc, _ := os.ReadFile(bashrcPath)
	if strings.Count(string(afterBashrc), ">>> orbit-machines shell init >>>") != 1 {
		t.Fatalf("expected .bashrc init block to appear exactly once, got:\n%s", string(afterBashrc))
	}

	// Test pre-existing .bashrc without trailing newline and default /home/orbit/workspace direnv whitelist
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

	// Test filesystem error when homeDir is a file instead of a directory
	blockingFile := filepath.Join(t.TempDir(), "file-not-dir")
	_ = os.WriteFile(blockingFile, []byte("x"), 0o644)
	if err := shellinit.EnsureInitialized(blockingFile, filepath.Join(blockingFile, "ws")); err == nil {
		t.Fatalf("expected error when homeDir is an existing regular file")
	}
}

