// Package flakewizard implements the interactive TUI Flake Init Wizard modal
// for repositories that do not yet have a flake.nix.
package flakewizard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/nixenv"
)

// DefaultCatalog lists commonly used Nixpkgs toolchains and services available
// for instant fuzzy search inside the Flake Init Wizard.
var DefaultCatalog = []string{
	"git",
	"htop",
	"curl",
	"wget",
	"ripgrep",
	"jq",
	"fd",
	"lazygit",
	"gh",
	"go",
	"golangci-lint",
	"nodejs_22",
	"pnpm",
	"bun",
	"ruby_4_0",
	"bundler",
	"rustup",
	"python313",
	"uv",
	"postgresql_16",
	"redis",
	"sqlite",
	"kubectl",
	"kubernetes-helm",
}

// BasePackages are always included in generated Orbit devshells.
var BasePackages = []string{
	"git",
	"htop",
	"curl",
	"wget",
	"ripgrep",
	"jq",
}

var stackRecommendedPackages = map[string][]string{
	"go":     {"go", "golangci-lint", "htop", "curl", "wget", "git"},
	"nodejs": {"nodejs_22", "htop", "curl", "wget", "git"},
	"node":   {"nodejs_22", "htop", "curl", "wget", "git"},
	"ruby":   {"ruby_4_0", "bundler", "htop", "curl", "wget", "git"},
	"rust":   {"rustup", "htop", "curl", "wget", "git"},
	"python": {"python313", "uv", "htop", "curl", "wget", "git"},
}

// StackPackages returns the recommended Nixpkgs packages for a named stack
// (including base utilities htop, curl, wget, git).
func StackPackages(stack string) []string {
	key := strings.ToLower(strings.TrimSpace(stack))
	if pkgs, ok := stackRecommendedPackages[key]; ok {
		out := make([]string, len(pkgs))
		copy(out, pkgs)
		return out
	}
	return nil
}

// PackageSearcher queries Nixpkgs for additional package attributes matching query.
type PackageSearcher func(ctx context.Context, query string) ([]string, error)

// AIGenerator inspects the repository and detected stacks to suggest tailored Nixpkgs attributes.
type AIGenerator func(ctx context.Context, repoDir string, stacks []string) ([]string, error)

// Config configures a Flake Init Wizard instance.
type Config struct {
	RepoDir     string
	RepoName    string
	Searcher    PackageSearcher
	AIGenerator AIGenerator
	OnActivate  func(ctx context.Context, repoDir string) error
}

// Wizard manages stack detection, Nixpkgs search, AI generation, and flake.nix activation.
type Wizard struct {
	mu          sync.Mutex
	repoDir     string
	repoName    string
	stacks      []string
	catalog     []string
	filtered    []string
	selected    map[string]bool
	cursor      int
	searchQuery string
	searcher    PackageSearcher
	aiGen       AIGenerator
	onActivate  func(ctx context.Context, repoDir string) error
}

// New validates cfg, auto-detects stacks in cfg.RepoDir, and pre-selects recommended Nixpkgs.
func New(cfg Config) (*Wizard, error) {
	dir := strings.TrimSpace(cfg.RepoDir)
	if dir == "" {
		return nil, fmt.Errorf("%w: RepoDir is required", domain.ErrInvalidConfig)
	}
	name := strings.TrimSpace(cfg.RepoName)
	if name == "" {
		name = filepath.Base(dir)
	}

	stacks, _ := nixenv.DetectStacks(dir)
	selected := make(map[string]bool, len(BasePackages))
	for _, pkg := range BasePackages {
		selected[pkg] = true
	}
	catalogSet := make(map[string]bool, len(DefaultCatalog))
	for _, pkg := range DefaultCatalog {
		catalogSet[pkg] = true
	}
	for _, st := range stacks {
		for _, pkg := range stackRecommendedPackages[st] {
			selected[pkg] = true
			catalogSet[pkg] = true
		}
	}

	catalog := sortedKeys(catalogSet)
	return &Wizard{
		repoDir:    dir,
		repoName:   name,
		stacks:     stacks,
		catalog:    catalog,
		filtered:   catalog,
		selected:   selected,
		searcher:   cfg.Searcher,
		aiGen:      cfg.AIGenerator,
		onActivate: cfg.OnActivate,
	}, nil
}

// SelectedPackages returns all currently selected Nixpkgs attributes in sorted order.
func (w *Wizard) SelectedPackages() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return sortedKeys(w.selected)
}

// FilteredPackages returns the packages matching the current search query.
func (w *Wizard) FilteredPackages() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.filtered))
	copy(out, w.filtered)
	return out
}

// Search filters the package catalog by query and merges any results from Searcher.
func (w *Wizard) Search(ctx context.Context, query string) error {
	q := strings.ToLower(strings.TrimSpace(query))
	var extra []string
	if q != "" && w.searcher != nil {
		res, err := w.searcher(ctx, q)
		if err != nil {
			return err
		}
		extra = res
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.searchQuery = q

	catSet := make(map[string]bool, len(w.catalog)+len(extra))
	for _, p := range w.catalog {
		catSet[p] = true
	}
	for _, p := range extra {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			catSet[trimmed] = true
		}
	}
	w.catalog = sortedKeys(catSet)

	var matches []string
	for _, p := range w.catalog {
		if q == "" || strings.Contains(strings.ToLower(p), q) {
			matches = append(matches, p)
		}
	}
	for _, p := range extra {
		if trimmed := strings.TrimSpace(p); trimmed != "" && !containsString(matches, trimmed) {
			matches = append(matches, trimmed)
		}
	}
	w.filtered = matches
	w.cursor = 0
	return nil
}

// MoveCursorDown advances the package selection cursor.
func (w *Wizard) MoveCursorDown() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.filtered) > 0 {
		w.cursor = (w.cursor + 1) % len(w.filtered)
	}
}

// MoveCursorUp moves the package selection cursor up.
func (w *Wizard) MoveCursorUp() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.filtered) > 0 {
		w.cursor = (w.cursor - 1 + len(w.filtered)) % len(w.filtered)
	}
}

// ToggleCurrentPackage toggles selection of the package under the cursor.
func (w *Wizard) ToggleCurrentPackage() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.filtered) == 0 {
		return
	}
	pkg := w.filtered[w.cursor]
	if w.selected[pkg] {
		delete(w.selected, pkg)
	} else {
		w.selected[pkg] = true
	}
}

// TogglePackage toggles selection of a named Nixpkgs attribute.
func (w *Wizard) TogglePackage(pkg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	pkg = strings.TrimSpace(pkg)
	if pkg == "" {
		return
	}
	if w.selected[pkg] {
		delete(w.selected, pkg)
	} else {
		w.selected[pkg] = true
	}
}

// GenerateWithAI invokes the configured AIGenerator (or stack heuristics) and adds recommended packages.
func (w *Wizard) GenerateWithAI(ctx context.Context) error {
	w.mu.Lock()
	aiFn := w.aiGen
	dir := w.repoDir
	stacks := w.stacks
	w.mu.Unlock()

	if aiFn == nil {
		return nil
	}
	suggested, err := aiFn(ctx, dir, stacks)
	if err != nil {
		return err
	}

	w.mu.Lock()
	for _, pkg := range suggested {
		if trimmed := strings.TrimSpace(pkg); trimmed != "" {
			w.selected[trimmed] = true
		}
	}
	w.mu.Unlock()
	return nil
}

// Preview renders the flake.nix content for the currently selected packages.
func (w *Wizard) Preview() string {
	return nixenv.GenerateFlakeNix(fmt.Sprintf("Orbit devshell for %s", w.repoName), w.SelectedPackages())
}

// WriteAndActivate writes flake.nix to RepoDir and runs OnActivate if configured.
func (w *Wizard) WriteAndActivate(ctx context.Context) error {
	flakePath := filepath.Join(w.repoDir, "flake.nix")
	if err := os.WriteFile(flakePath, []byte(w.Preview()), 0o644); err != nil {
		return err
	}
	if w.onActivate != nil {
		return w.onActivate(ctx, w.repoDir)
	}
	return nil
}

// View renders the interactive Flake Init Wizard modal overlay.
func (w *Wizard) View(width, _ int) string {
	w.mu.Lock()
	defer w.mu.Unlock()

	detectedStr := "none"
	if len(w.stacks) > 0 {
		detectedStr = strings.Join(w.stacks, ", ")
	}

	var rows []string
	limit := min(len(w.filtered), 8)
	for i := 0; i < limit; i++ {
		pkg := w.filtered[i]
		cursor := "  "
		if i == w.cursor {
			cursor = "> "
		}
		check := "[ ]"
		if w.selected[pkg] {
			check = "[x]"
		}
		rows = append(rows, fmt.Sprintf("%s%s %s", cursor, check, pkg))
	}

	content := fmt.Sprintf(
		"Flake Init Wizard — %s\nDetected stacks: %s\nSearch: %s\n\n%s\n\n[Space] Toggle  [a] Ask AI  [Enter] Write flake.nix & Activate  [Esc] Cancel",
		w.repoName,
		detectedStr,
		w.searchQuery,
		strings.Join(rows, "\n"),
	)
	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		Padding(1, 2).
		Width(min(max(width-8, 44), 92)).
		Render(content)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, enabled := range m {
		if enabled {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func containsString(slice []string, target string) bool {
	for _, s := range slice {
		if s == target {
			return true
		}
	}
	return false
}
