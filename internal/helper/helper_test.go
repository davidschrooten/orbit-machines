package helper_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/helper"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
)

func TestManager_ContextPersistenceReposCloneFlakeToolsAndSave(t *testing.T) {
	t.Parallel()

	if _, err := helper.NewManager(helper.Config{}); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty Config, got %v", err)
	}

	homeDir := filepath.Join(t.TempDir(), "home")
	workDir := filepath.Join(homeDir, "workspace")

	var (
		clonedURLs   []string
		direnvDirs   []string
		snapshotHits int
	)

	machinedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/snapshot" {
			snapshotHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"snapshotted","repositories":["orbit"]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer machinedSrv.Close()

	mgr, err := helper.NewManager(helper.Config{
		HomeDir:     homeDir,
		WorkDir:     workDir,
		MachinedURL: machinedSrv.URL,
		GitCloner: func(_ context.Context, cloneURL, destDir, _ string) error {
			clonedURLs = append(clonedURLs, cloneURL)
			if err := os.MkdirAll(filepath.Join(destDir, ".git"), 0o755); err != nil {
				return err
			}
			// Simulate existing flake.nix in "orbit" and Go project without flake.nix in "billing"
			if filepath.Base(destDir) == "orbit" {
				return os.WriteFile(filepath.Join(destDir, "flake.nix"), []byte("{ description = \"orbit\"; }"), 0o644)
			}
			return os.WriteFile(filepath.Join(destDir, "go.mod"), []byte("module acme/billing\n"), 0o644)
		},
		DirenvAllower: func(_ context.Context, dir string) error {
			direnvDirs = append(direnvDirs, dir)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// 1. SaveContext persists context.json and env.sh
	orbitCtx := helper.OrbitContext{
		OrbitURL:      "https://orbit.acme.dev",
		OrbitMCPToken: "omcp_secret_123",
		WorkspaceID:   "ws_01",
		WorkspaceSlug: "acme",
		UserID:        "usr_01",
		UserEmail:     "alice@acme.dev",
		Repositories: []protocol.RepoCredential{
			{
				ID:            "repo_1",
				Name:          "orbit",
				CloneURL:      "https://github.com/acme/orbit.git",
				DefaultBranch: "main",
				GitUsername:   "x-access-token",
				GitToken:      "ghs_tok123",
			},
			{
				ID:            "repo_2",
				Name:          "billing",
				CloneURL:      "https://github.com/acme/billing.git",
				DefaultBranch: "main",
			},
		},
	}
	if err := mgr.SaveContext(orbitCtx); err != nil {
		t.Fatalf("SaveContext: %v", err)
	}

	loadedCtx, err := mgr.LoadContext()
	if err != nil {
		t.Fatalf("LoadContext: %v", err)
	}
	if loadedCtx.OrbitURL != "https://orbit.acme.dev" || len(loadedCtx.Repositories) != 2 {
		t.Fatalf("unexpected LoadedContext: %+v", loadedCtx)
	}

	envBytes, err := os.ReadFile(filepath.Join(homeDir, ".config", "orbit", "env.sh"))
	if err != nil {
		t.Fatalf("ReadFile(env.sh): %v", err)
	}
	if !strings.Contains(string(envBytes), `export ORBIT_MCP_TOKEN="omcp_secret_123"`) {
		t.Fatalf("env.sh missing ORBIT_MCP_TOKEN:\n%s", string(envBytes))
	}

	// 2. CloneRepositories with cloneAll=true and initMissingFlake=true
	ctx := context.Background()
	cloned, err := mgr.CloneRepositories(ctx, helper.CloneOptions{
		CloneAll:         true,
		InitMissingFlake: true,
	})
	if err != nil {
		t.Fatalf("CloneRepositories: %v", err)
	}
	if len(cloned) != 2 {
		t.Fatalf("expected 2 cloned repos, got %v", cloned)
	}
	// Verify authenticated URL was constructed for repo_1
	if !strings.Contains(clonedURLs[0], "x-access-token:ghs_tok123@github.com/acme/orbit.git") {
		t.Fatalf("expected authenticated clone URL, got %q", clonedURLs[0])
	}
	// Verify both repos have .envrc with "use flake" and billing had flake.nix generated
	for _, repoName := range []string{"orbit", "billing"} {
		repoDir := filepath.Join(workDir, repoName)
		envrcBytes, err := os.ReadFile(filepath.Join(repoDir, ".envrc"))
		if err != nil || !strings.Contains(string(envrcBytes), "use flake") {
			t.Fatalf("expected .envrc with 'use flake' in %s, err=%v", repoName, err)
		}
		if _, err := os.Stat(filepath.Join(repoDir, "flake.nix")); err != nil {
			t.Fatalf("expected flake.nix in %s: %v", repoName, err)
		}
	}
	if len(direnvDirs) < 2 {
		t.Fatalf("expected DirenvAllower to be called for both repos, got %v", direnvDirs)
	}

	// Clone explicit URL not in context
	extraCloned, err := mgr.CloneRepositories(ctx, helper.CloneOptions{
		NamesOrURLs:      []string{"https://github.com/acme/custom-tool.git"},
		InitMissingFlake: true,
	})
	if err != nil || len(extraCloned) != 1 || extraCloned[0] != "custom-tool" {
		t.Fatalf("CloneRepositories custom URL = %v, err=%v", extraCloned, err)
	}

	// 3. InitFlake with custom packages and AutoloadAllWorkspaceFlakes
	customRepo := filepath.Join(workDir, "custom-flake-repo")
	if err := mgr.InitFlake(ctx, customRepo, []string{"go", "golangci-lint", "jq"}); err != nil {
		t.Fatalf("InitFlake: %v", err)
	}
	flakeBytes, err := os.ReadFile(filepath.Join(customRepo, "flake.nix"))
	if err != nil || !strings.Contains(string(flakeBytes), "pkgs.golangci-lint") {
		t.Fatalf("unexpected generated flake.nix:\n%s", string(flakeBytes))
	}
	autoloaded, err := mgr.AutoloadAllWorkspaceFlakes(ctx)
	if err != nil || len(autoloaded) < 3 {
		t.Fatalf("AutoloadAllWorkspaceFlakes = %v, err=%v", autoloaded, err)
	}

	// 4. ConfigureTools with multiple tools selected ("claude", "agy", "opencode", "gh", "git")
	configured, err := mgr.ConfigureTools([]string{"claude", "agy", "opencode", "gh", "git"})
	if err != nil {
		t.Fatalf("ConfigureTools: %v", err)
	}
	if len(configured) != 5 {
		t.Fatalf("expected 5 configured tools, got %v", configured)
	}

	for _, cfgFile := range []string{
		filepath.Join(homeDir, ".claude.json"),
		filepath.Join(homeDir, ".config", "antigravity", "mcp.json"),
		filepath.Join(homeDir, ".gemini", "settings.json"),
		filepath.Join(homeDir, ".config", "opencode", "config.json"),
		filepath.Join(homeDir, ".config", "opencode", "opencode.json"),
		filepath.Join(homeDir, ".config", "gh", "config.yml"),
		filepath.Join(homeDir, ".gitconfig"),
	} {
		data, err := os.ReadFile(cfgFile)
		if err != nil || len(data) == 0 {
			t.Fatalf("expected non-empty tool config %s, err=%v", cfgFile, err)
		}
	}

	// Verify .claude.json contains the Orbit MCP URL and Bearer token
	claudeBytes, _ := os.ReadFile(filepath.Join(homeDir, ".claude.json"))
	if !strings.Contains(string(claudeBytes), "https://orbit.acme.dev/mcp") || !strings.Contains(string(claudeBytes), "Bearer omcp_secret_123") {
		t.Fatalf("unexpected .claude.json:\n%s", string(claudeBytes))
	}

	// 5. Status & Save
	status, err := mgr.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	formatted := status.Format()
	if !strings.Contains(formatted, "🪐 Orbit Helper Status") || !strings.Contains(formatted, "orbit") || !strings.Contains(formatted, "claude") {
		t.Fatalf("unexpected status output:\n%s", formatted)
	}

	if err := mgr.Save(ctx); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if snapshotHits != 1 {
		t.Fatalf("expected Save to POST /api/v1/snapshot once, got %d", snapshotHits)
	}
}

func TestManager_InteractiveMenuAndEnvFallbacks(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), "home")
	workDir := filepath.Join(homeDir, "workspace")

	reposJSON, _ := json.Marshal([]protocol.RepoCredential{
		{ID: "r1", Name: "orbit-web", CloneURL: "https://github.com/acme/orbit-web.git"},
	})
	t.Setenv("ORBIT_URL", "https://orbit.env.dev")
	t.Setenv("ORBIT_MCP_TOKEN", "omcp_env_tok")
	t.Setenv("ORBIT_MACHINED_WORKSPACE_ID", "ws_env")
	t.Setenv("ORBIT_WORKSPACE_SLUG", "env-slug")
	t.Setenv("ORBIT_MACHINED_USER_ID", "usr_env")
	t.Setenv("ORBIT_USER_EMAIL", "dev@env.dev")
	t.Setenv("ORBIT_REPOSITORIES_JSON", string(reposJSON))

	mgr, err := helper.NewManager(helper.Config{
		HomeDir: homeDir,
		WorkDir: workDir,
		GitCloner: func(_ context.Context, _, destDir, _ string) error {
			return os.MkdirAll(filepath.Join(destDir, ".git"), 0o755)
		},
		DirenvAllower: func(_ context.Context, _ string) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Interactive menu session exercising options 1 (repos), 2 (flake), 3 (tools), 4 (status & save), and q (quit)
	input := strings.Join([]string{
		"1",            // 1) Clone & setup workspace repositories
		"all",          // clone all
		"y",            // init missing flake.nix
		"2",            // 2) Initialize / autoload Nix flake
		"orbit-web",    // repo name
		"go,ripgrep",   // packages
		"3",            // 3) Configure AI & developer tools
		"1,2,3,4,5",    // multi-select by numbers (claude, agy, opencode, gh, git)
		"4",            // 4) Status & save
		"q",            // Quit
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := mgr.RunInteractive(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("RunInteractive: %v", err)
	}
	outputStr := out.String()
	for _, needle := range []string{
		"🪐 Orbit Helper",
		"Cloned",
		"Configured tools",
		"Orbit Helper Status",
	} {
		if !strings.Contains(outputStr, needle) {
			t.Errorf("interactive output missing %q:\n%s", needle, outputStr)
		}
	}

	// Interactive menu edge cases: numeric repo selection, blank flake autoload, invalid tool error, EOF
	input2 := strings.Join([]string{
		"1",       // repos
		"1",       // select repo #1 by number
		"n",       // do not init missing flake
		"2",       // flake
		"",        // blank -> autoload all workspace flakes
		"2",       // flake with custom path & empty packages
		"subrepo", // relative repo name
		"",        // blank packages -> auto-detect
		"3",       // tools
		"badtool", // triggers error print in interactive loop
	}, "\n") + "\n"
	var out2 bytes.Buffer
	if err := mgr.RunInteractive(context.Background(), strings.NewReader(input2), &out2); err != nil {
		t.Fatalf("RunInteractive 2: %v", err)
	}
}

func TestManager_EdgeCasesAndDefaultCloner(t *testing.T) {
	t.Parallel()

	homeDir := filepath.Join(t.TempDir(), "home")
	workDir := filepath.Join(homeDir, "workspace")

	// Default GitCloner and DirenvAllower
	mgr, err := helper.NewManager(helper.Config{
		HomeDir: homeDir,
		WorkDir: workDir,
	})
	if err != nil {
		t.Fatalf("NewManager defaults: %v", err)
	}

	ctx := context.Background()

	// Validation errors
	if _, err := mgr.CloneRepositories(ctx, helper.CloneOptions{}); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig when no repos selected, got %v", err)
	}
	if err := mgr.EnableFlakeAutoload(ctx, ""); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty repoDir in EnableFlakeAutoload")
	}
	if err := mgr.InitFlake(ctx, "", nil); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty repoDir in InitFlake")
	}
	if _, err := mgr.ConfigureTools(nil); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty tool list")
	}
	if _, err := mgr.ConfigureTools([]string{"unknown-tool"}); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for unsupported tool")
	}

	// Pre-populate .git in a destination directory so defaultGitCloner takes the pull/already-cloned path
	existingRepo := filepath.Join(workDir, "local-repo")
	_ = os.MkdirAll(filepath.Join(existingRepo, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(existingRepo, ".envrc"), []byte("export FOO=bar\n"), 0o644)
	if _, err := mgr.CloneRepositories(ctx, helper.CloneOptions{
		NamesOrURLs:      []string{"https://github.com/acme/local-repo.git"},
		InitMissingFlake: true,
	}); err != nil {
		t.Fatalf("CloneRepositories existing .git: %v", err)
	}
	envrcBytes, _ := os.ReadFile(filepath.Join(existingRepo, ".envrc"))
	if !strings.Contains(string(envrcBytes), "export FOO=bar\nuse flake\n") {
		t.Fatalf("expected .envrc to preserve existing lines and append 'use flake', got:\n%s", string(envrcBytes))
	}

	// Empty status report formatting and partial SaveContext preservation
	emptyHome := filepath.Join(t.TempDir(), "empty-home")
	emptyMgr, _ := helper.NewManager(helper.Config{
		HomeDir: emptyHome,
		WorkDir: filepath.Join(emptyHome, "workspace"),
	})
	emptyStatus, err := emptyMgr.Status()
	if err != nil {
		t.Fatalf("emptyMgr.Status: %v", err)
	}
	if !strings.Contains(emptyStatus.Format(), "(not set)") || !strings.Contains(emptyStatus.Format(), "(none)") {
		t.Fatalf("expected empty placeholders in Format:\n%s", emptyStatus.Format())
	}

	// Save full context then partial context -> existing fields preserved
	_ = emptyMgr.SaveContext(helper.OrbitContext{
		OrbitURL:      "https://orbit.full.dev",
		OrbitMCPToken: "tok_full",
		WorkspaceID:   "ws_full",
		WorkspaceSlug: "slug_full",
		UserID:        "usr_full",
		UserEmail:     "full@acme.dev",
		Repositories:  []protocol.RepoCredential{{ID: "r1", Name: "r1", CloneURL: "https://example.com/r1.git"}},
	})
	_ = emptyMgr.SaveContext(helper.OrbitContext{})
	mergedCtx, _ := emptyMgr.LoadContext()
	if mergedCtx.OrbitURL != "https://orbit.full.dev" || mergedCtx.UserEmail != "full@acme.dev" || len(mergedCtx.Repositories) != 1 {
		t.Fatalf("expected partial SaveContext to preserve existing fields, got %+v", mergedCtx)
	}

	// Real defaultGitCloner against a local git repository + failing clone URL
	srcRepo := filepath.Join(t.TempDir(), "src-repo")
	_ = os.MkdirAll(srcRepo, 0o755)
	_ = exec.Command("git", "init", "-b", "main", srcRepo).Run()
	_ = exec.Command("git", "-C", srcRepo, "config", "user.email", "test@acme.dev").Run()
	_ = exec.Command("git", "-C", srcRepo, "config", "user.name", "test").Run()
	_ = os.WriteFile(filepath.Join(srcRepo, "README.md"), []byte("# test\n"), 0o644)
	_ = exec.Command("git", "-C", srcRepo, "add", ".").Run()
	_ = exec.Command("git", "-C", srcRepo, "commit", "-m", "init").Run()

	_ = emptyMgr.SaveContext(helper.OrbitContext{
		Repositories: []protocol.RepoCredential{
			{Name: "cloned-from-local", CloneURL: srcRepo, DefaultBranch: "main"},
		},
	})
	if _, err := emptyMgr.CloneRepositories(ctx, helper.CloneOptions{CloneAll: true}); err != nil {
		t.Fatalf("defaultGitCloner real clone: %v", err)
	}
	if _, err := emptyMgr.CloneRepositories(ctx, helper.CloneOptions{NamesOrURLs: []string{"/nonexistent/git/repo/path"}}); err == nil {
		t.Fatalf("expected error when defaultGitCloner fails")
	}
	if _, err := emptyMgr.CloneRepositories(ctx, helper.CloneOptions{NamesOrURLs: []string{"bare-repo-name"}}); err == nil {
		t.Fatalf("expected error for bare-repo-name")
	}
}
