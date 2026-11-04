package flakewizard_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/tui/flakewizard"
)

func TestWizard_AutoDetectSearchToggleAIGenerateAndActivate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repoDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module example.com/app\n"), 0o644)
	_ = os.WriteFile(filepath.Join(repoDir, "package.json"), []byte(`{"name":"web"}`), 0o644)
	_ = os.WriteFile(filepath.Join(repoDir, "Gemfile"), []byte("source 'https://rubygems.org'\n"), 0o644)
	_ = os.WriteFile(filepath.Join(repoDir, "Cargo.toml"), []byte("[package]\nname = \"cli\"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(repoDir, "pyproject.toml"), []byte("[project]\nname = \"ml\"\n"), 0o644)

	if _, err := flakewizard.New(flakewizard.Config{}); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty RepoDir, got %v", err)
	}

	var activatedDir string
	wiz, err := flakewizard.New(flakewizard.Config{
		RepoDir:  repoDir,
		RepoName: "orbit-web",
		Searcher: func(_ context.Context, query string) ([]string, error) {
			if query == "postgres" {
				return []string{"postgresql_16", "pgcli", "  "}, nil
			}
			return nil, nil
		},
		AIGenerator: func(_ context.Context, _ string, stacks []string) ([]string, error) {
			if len(stacks) == 0 {
				return nil, errors.New("expected detected stacks")
			}
			return []string{"redis", "lazygit", ""}, nil
		},
		OnActivate: func(_ context.Context, dir string) error {
			activatedDir = dir
			return nil
		},
	})
	if err != nil {
		t.Fatalf("flakewizard.New: %v", err)
	}

	// 1. Auto-detected Go, Node, Ruby, Rust, and Python packages are pre-selected.
	selected := wiz.SelectedPackages()
	hasPkg := func(list []string, target string) bool {
		for _, item := range list {
			if item == target {
				return true
			}
		}
		return false
	}
	for _, expected := range []string{"go", "golangci-lint", "nodejs_22", "ruby_4_0", "bundler", "rustup", "python313", "uv"} {
		if !hasPkg(selected, expected) {
			t.Fatalf("expected auto-detected %q in SelectedPackages(), got %v", expected, selected)
		}
	}

	// 2. Search catalog + custom Searcher and toggle packages with cursor.
	if err := wiz.Search(ctx, "postgres"); err != nil {
		t.Fatalf("Search(postgres): %v", err)
	}
	filtered := wiz.FilteredPackages()
	if !hasPkg(filtered, "postgresql_16") || !hasPkg(filtered, "pgcli") {
		t.Fatalf("expected postgresql_16 and pgcli in FilteredPackages(), got %v", filtered)
	}

	wiz.ToggleCurrentPackage()
	if !hasPkg(wiz.SelectedPackages(), filtered[0]) {
		t.Fatalf("expected ToggleCurrentPackage to select %q", filtered[0])
	}
	wiz.ToggleCurrentPackage()
	if hasPkg(wiz.SelectedPackages(), filtered[0]) {
		t.Fatalf("expected second ToggleCurrentPackage to deselect %q", filtered[0])
	}

	wiz.MoveCursorDown()
	wiz.MoveCursorUp()
	wiz.TogglePackage("")
	wiz.TogglePackage("postgresql_16")
	if !hasPkg(wiz.SelectedPackages(), "postgresql_16") {
		t.Fatalf("expected postgresql_16 to be selected")
	}
	wiz.TogglePackage("postgresql_16")
	if hasPkg(wiz.SelectedPackages(), "postgresql_16") {
		t.Fatalf("expected second TogglePackage(postgresql_16) to deselect")
	}
	wiz.TogglePackage("postgresql_16")

	// 3. AI-Assisted Generation merges AI-suggested packages ("redis", "lazygit").
	if err := wiz.GenerateWithAI(ctx); err != nil {
		t.Fatalf("GenerateWithAI: %v", err)
	}
	if !hasPkg(wiz.SelectedPackages(), "redis") || !hasPkg(wiz.SelectedPackages(), "lazygit") {
		t.Fatalf("expected AI-recommended redis and lazygit in SelectedPackages(), got %v", wiz.SelectedPackages())
	}

	// 4. Preview & View render valid flake.nix and modal UI.
	preview := wiz.Preview()
	if !strings.Contains(preview, "Orbit devshell for orbit-web") || !strings.Contains(preview, "postgresql_16") || !strings.Contains(preview, "redis") {
		t.Fatalf("Preview() missing description or selected packages:\n%s", preview)
	}
	view := wiz.View(100, 30)
	if !strings.Contains(view, "Flake Init Wizard") || !strings.Contains(view, "orbit-web") {
		t.Fatalf("View() missing header:\n%s", view)
	}

	// 5. WriteAndActivate writes flake.nix to repoDir and invokes OnActivate.
	if err := wiz.WriteAndActivate(ctx); err != nil {
		t.Fatalf("WriteAndActivate: %v", err)
	}
	if activatedDir != repoDir {
		t.Fatalf("OnActivate dir = %q, want %q", activatedDir, repoDir)
	}
	written, err := os.ReadFile(filepath.Join(repoDir, "flake.nix"))
	if err != nil {
		t.Fatalf("ReadFile(flake.nix): %v", err)
	}
	if string(written) != preview {
		t.Fatalf("written flake.nix did not match Preview()")
	}
}

func TestWizard_DefaultsAndErrorPaths(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	emptyDir := t.TempDir()

	// Default RepoName from directory base, no stacks detected, nil callbacks.
	wiz, err := flakewizard.New(flakewizard.Config{
		RepoDir: emptyDir,
	})
	if err != nil {
		t.Fatalf("flakewizard.New: %v", err)
	}
	if err := wiz.GenerateWithAI(ctx); err != nil {
		t.Fatalf("GenerateWithAI with nil AIGenerator: %v", err)
	}
	if !strings.Contains(wiz.View(60, 20), "Detected stacks: none") {
		t.Fatalf("expected View to show 'Detected stacks: none'")
	}

	// Search resulting in zero matches exercises empty-list guards in cursor/toggle.
	if err := wiz.Search(ctx, "no-such-package-xyz"); err != nil {
		t.Fatalf("Search(no-such-package-xyz): %v", err)
	}
	wiz.MoveCursorDown()
	wiz.MoveCursorUp()
	wiz.ToggleCurrentPackage()

	if err := wiz.WriteAndActivate(ctx); err != nil {
		t.Fatalf("WriteAndActivate with nil OnActivate: %v", err)
	}

	// Searcher error and AIGenerator error and WriteAndActivate error.
	searchErr := errors.New("nixpkgs search failed")
	aiErr := errors.New("ai provider failed")
	badDir := filepath.Join(emptyDir, "missing-subdir")
	errWiz, err := flakewizard.New(flakewizard.Config{
		RepoDir: badDir,
		Searcher: func(_ context.Context, _ string) ([]string, error) {
			return nil, searchErr
		},
		AIGenerator: func(_ context.Context, _ string, _ []string) ([]string, error) {
			return nil, aiErr
		},
	})
	if err != nil {
		t.Fatalf("flakewizard.New: %v", err)
	}
	if err := errWiz.Search(ctx, "git"); !errors.Is(err, searchErr) {
		t.Fatalf("expected searchErr, got %v", err)
	}
	if err := errWiz.GenerateWithAI(ctx); !errors.Is(err, aiErr) {
		t.Fatalf("expected aiErr, got %v", err)
	}
	if err := errWiz.WriteAndActivate(ctx); err == nil {
		t.Fatalf("expected WriteAndActivate error for missing directory")
	}
}
