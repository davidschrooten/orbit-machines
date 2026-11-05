package helper_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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
	blockingHome := filepath.Join(t.TempDir(), "not-a-dir")
	_ = os.WriteFile(blockingHome, []byte("x"), 0o644)
	if _, err := helper.NewManager(helper.Config{HomeDir: blockingHome, WorkDir: filepath.Join(blockingHome, "ws")}); err == nil {
		t.Fatalf("expected error when HomeDir is a regular file")
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
	if !strings.Contains(string(envBytes), `export ORBIT_MCP_TOKEN="omcp_secret_123"`) || !strings.Contains(string(envBytes), "export GIT_TERMINAL_PROMPT=0") || !strings.Contains(string(envBytes), "export GIT_SSH_COMMAND=") {
		t.Fatalf("env.sh missing expected exports:\n%s", string(envBytes))
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
	// Verify SSH clone URLs were constructed for both repos (never HTTPS)
	if clonedURLs[0] != "git@github.com:acme/orbit.git" || clonedURLs[1] != "git@github.com:acme/billing.git" {
		t.Fatalf("expected SSH clone URLs, got %v", clonedURLs)
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

	// 3. InitFlake with custom packages and AutoloadAllWorkspaceFlakes (with a regular file in workDir)
	_ = os.WriteFile(filepath.Join(workDir, "README.txt"), []byte("workspace notes\n"), 0o644)
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

	// Verify .claude.json contains the Orbit MCP URL and Bearer token, and gh config uses ssh protocol
	claudeBytes, _ := os.ReadFile(filepath.Join(homeDir, ".claude.json"))
	if !strings.Contains(string(claudeBytes), "https://orbit.acme.dev/mcp") || !strings.Contains(string(claudeBytes), "Bearer omcp_secret_123") {
		t.Fatalf("unexpected .claude.json:\n%s", string(claudeBytes))
	}
	ghBytes, _ := os.ReadFile(filepath.Join(homeDir, ".config", "gh", "config.yml"))
	if !strings.Contains(string(ghBytes), "git_protocol: ssh") {
		t.Fatalf("expected gh config.yml to use git_protocol: ssh, got:\n%s", string(ghBytes))
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

func TestManager_GitAndSSHKeyPersistenceAndRestoration(t *testing.T) {
	t.Parallel()

	homeDir := filepath.Join(t.TempDir(), "home")
	workDir := filepath.Join(homeDir, "workspace")

	mgr, err := helper.NewManager(helper.Config{
		HomeDir: homeDir,
		WorkDir: workDir,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// 1. Auto-initialized .gitconfig exists from NewManager -> EnsureGitAndSSH
	initialGitCfg, err := os.ReadFile(filepath.Join(homeDir, ".gitconfig"))
	if err != nil {
		t.Fatalf("ReadFile(.gitconfig): %v", err)
	}
	for _, needle := range []string{"# orbit-git-ssh: v1", "defaultBranch = main", "rebase = false", "directory = *", "sshCommand = ssh -i ", "insteadOf = https://github.com/"} {
		if !strings.Contains(string(initialGitCfg), needle) {
			t.Fatalf(".gitconfig missing %q:\n%s", needle, string(initialGitCfg))
		}
	}

	// Verify legacy .gitconfig without # orbit-git-ssh: v1 gets upgraded by EnsureGitAndSSH
	_ = os.WriteFile(filepath.Join(homeDir, ".gitconfig"), []byte("[user]\n\tname = Legacy\n[credential]\n\thelper = store\n"), 0o644)
	if err := mgr.EnsureGitAndSSH(); err != nil {
		t.Fatalf("EnsureGitAndSSH upgrade: %v", err)
	}
	upgradedGitCfg, _ := os.ReadFile(filepath.Join(homeDir, ".gitconfig"))
	if !strings.Contains(string(upgradedGitCfg), "# orbit-git-ssh: v1") || !strings.Contains(string(upgradedGitCfg), "sshCommand = ssh -i ") {
		t.Fatalf("expected legacy .gitconfig to be upgraded to SSH config, got:\n%s", string(upgradedGitCfg))
	}

	// Verify ToSSHCloneURL conversions
	if got := helper.ToSSHCloneURL("https://github.com/acme/orbit"); got != "git@github.com:acme/orbit.git" {
		t.Fatalf("ToSSHCloneURL(https without .git) = %q", got)
	}
	if got := helper.ToSSHCloneURL("http://gitlab.com/acme/repo.git"); got != "git@gitlab.com:acme/repo.git" {
		t.Fatalf("ToSSHCloneURL(http gitlab) = %q", got)
	}
	if got := helper.ToSSHCloneURL("http://127.0.0.1:8080/repo.git"); got != "http://127.0.0.1:8080/repo.git" {
		t.Fatalf("ToSSHCloneURL(localhost) = %q", got)
	}
	if got := helper.ToSSHCloneURL("https://github.com/"); got != "https://github.com/" {
		t.Fatalf("ToSSHCloneURL(empty path) = %q", got)
	}
	if got := helper.ToSSHCloneURL("git@github.com:acme/orbit.git"); got != "git@github.com:acme/orbit.git" {
		t.Fatalf("ToSSHCloneURL(ssh) = %q", got)
	}

	// 2. ConfigureGit validation and persistence
	if err := mgr.ConfigureGit("", "alice@acme.dev", "main"); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty git name, got %v", err)
	}
	if err := mgr.ConfigureGit("Alice Developer", "alice@acme.dev", ""); err != nil {
		t.Fatalf("ConfigureGit: %v", err)
	}
	gitBytes, _ := os.ReadFile(filepath.Join(homeDir, ".gitconfig"))
	if !strings.Contains(string(gitBytes), "name = Alice Developer") || !strings.Contains(string(gitBytes), "email = alice@acme.dev") {
		t.Fatalf("unexpected .gitconfig after ConfigureGit:\n%s", string(gitBytes))
	}

	// 3. Before generating SSH key, PublicSSHKey returns ErrInvalidConfig, and interactive show prints help message
	if _, err := mgr.PublicSSHKey(); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig when no SSH key configured, got %v", err)
	}
	var emptySSHOut bytes.Buffer
	if err := mgr.RunInteractiveSSHKey(strings.NewReader("3\n"), &emptySSHOut); err != nil || !strings.Contains(emptySSHOut.String(), "No SSH public key found") {
		t.Fatalf("expected interactive show before key generation to succeed with message, got (%q, %v)", emptySSHOut.String(), err)
	}

	// 4. GenerateSSHKey creates ~/.ssh/id_ed25519 (0600), id_ed25519.pub (0644), config (0600), and persists to state
	sshCfg, err := mgr.GenerateSSHKey("alice@acme.dev")
	if err != nil {
		t.Fatalf("GenerateSSHKey: %v", err)
	}
	if !strings.HasPrefix(sshCfg.PublicKey, "ssh-ed25519 ") || !strings.HasSuffix(sshCfg.PublicKey, "alice@acme.dev") {
		t.Fatalf("unexpected generated public key: %q", sshCfg.PublicKey)
	}

	privPath := filepath.Join(homeDir, ".ssh", "id_ed25519")
	privInfo, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("Stat(id_ed25519): %v", err)
	}
	if privInfo.Mode().Perm() != 0o600 {
		t.Fatalf("id_ed25519 mode = %o, want 0600", privInfo.Mode().Perm())
	}

	// If ssh-keygen is installed on the host, verify OpenSSH parses our pure-Go private key and outputs the exact same public key
	if sshKeygenPath, lookErr := exec.LookPath("ssh-keygen"); lookErr == nil {
		derivedOut, cmdErr := exec.Command(sshKeygenPath, "-y", "-f", privPath).CombinedOutput()
		if cmdErr != nil {
			t.Fatalf("ssh-keygen -y -f failed: %v (%s)", cmdErr, string(derivedOut))
		}
		wantPrefix := strings.Fields(sshCfg.PublicKey)[0] + " " + strings.Fields(sshCfg.PublicKey)[1]
		if !strings.HasPrefix(strings.TrimSpace(string(derivedOut)), wantPrefix) {
			t.Fatalf("ssh-keygen derived %q, want prefix %q", strings.TrimSpace(string(derivedOut)), wantPrefix)
		}
	}

	// 5. Simulate ephemeral Pod restart: delete ~/.gitconfig and ~/.ssh, then call EnsureGitAndSSH() -> both restored from state!
	_ = os.Remove(filepath.Join(homeDir, ".gitconfig"))
	_ = os.RemoveAll(filepath.Join(homeDir, ".ssh"))
	if err := mgr.EnsureGitAndSSH(); err != nil {
		t.Fatalf("EnsureGitAndSSH restore: %v", err)
	}
	restoredPub, err := mgr.PublicSSHKey()
	if err != nil || restoredPub != sshCfg.PublicKey {
		t.Fatalf("PublicSSHKey after restore = (%q, %v), want %q", restoredPub, err, sshCfg.PublicKey)
	}
	restoredGit, _ := os.ReadFile(filepath.Join(homeDir, ".gitconfig"))
	if !strings.Contains(string(restoredGit), "Alice Developer") {
		t.Fatalf("expected .gitconfig to be restored with Alice Developer, got:\n%s", string(restoredGit))
	}

	// 6. SetSSHKey with OpenSSH PEM and PKCS#8 Ed25519 PEM, plus invalid key errors
	if _, err := mgr.SetSSHKey(""); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty private key")
	}
	if _, err := mgr.SetSSHKey("not-a-pem-key"); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for invalid PEM")
	}
	if _, err := mgr.SetSSHKey(sshCfg.PrivateKey); err != nil {
		t.Fatalf("SetSSHKey(OpenSSH PEM): %v", err)
	}

	_, pkcs8Priv, _ := ed25519.GenerateKey(rand.Reader)
	pkcs8DER, _ := x509.MarshalPKCS8PrivateKey(pkcs8Priv)
	pkcs8PEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8DER}))
	if _, err := mgr.SetSSHKey(pkcs8PEM); err != nil {
		t.Fatalf("SetSSHKey(PKCS#8 PEM): %v", err)
	}

	if _, lookErr := exec.LookPath("ssh-keygen"); lookErr == nil {
		rsaPriv, _ := rsa.GenerateKey(rand.Reader, 2048)
		rsaDER := x509.MarshalPKCS1PrivateKey(rsaPriv)
		rsaPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: rsaDER}))
		if rsaCfg, err := mgr.SetSSHKey(rsaPEM); err != nil || !strings.HasPrefix(rsaCfg.PublicKey, "ssh-rsa ") {
			t.Fatalf("SetSSHKey(RSA PEM) = (%q, %v)", rsaCfg.PublicKey, err)
		}
	}

	// 7. Additional AI tools in ConfigureTools (gemini, codex, cursor, aider) + stack name in InitFlake
	if _, err := mgr.ConfigureTools([]string{"gemini", "codex", "cursor", "aider"}); err != nil {
		t.Fatalf("ConfigureTools additional AI tools: %v", err)
	}
	stackRepo := filepath.Join(workDir, "stack-repo")
	if err := mgr.InitFlake(context.Background(), stackRepo, []string{"go"}); err != nil {
		t.Fatalf("InitFlake(go): %v", err)
	}
	stackFlake, _ := os.ReadFile(filepath.Join(stackRepo, "flake.nix"))
	for _, pkg := range []string{"pkgs.go", "pkgs.golangci-lint", "pkgs.htop", "pkgs.curl", "pkgs.wget", "pkgs.git"} {
		if !strings.Contains(string(stackFlake), pkg) {
			t.Fatalf("InitFlake(go) missing %q:\n%s", pkg, string(stackFlake))
		}
	}

	// 8. RunInteractiveGit and RunInteractiveSSHKey (options 1, 2 paste, 3 show, and invalid option)
	var gitOut bytes.Buffer
	if err := mgr.RunInteractiveGit(strings.NewReader("Bob\nbob@acme.dev\ndevelop\n"), &gitOut); err != nil {
		t.Fatalf("RunInteractiveGit: %v", err)
	}
	var sshOut bytes.Buffer
	if err := mgr.RunInteractiveSSHKey(strings.NewReader("1\n"), &sshOut); err != nil {
		t.Fatalf("RunInteractiveSSHKey(1): %v", err)
	}
	if err := mgr.RunInteractiveSSHKey(strings.NewReader("2\n"+sshCfg.PrivateKey+"\n"), &sshOut); err != nil {
		t.Fatalf("RunInteractiveSSHKey(2): %v", err)
	}
	if err := mgr.RunInteractiveSSHKey(strings.NewReader("3\n"), &sshOut); err != nil {
		t.Fatalf("RunInteractiveSSHKey(3): %v", err)
	}
	if err := mgr.RunInteractiveSSHKey(strings.NewReader("99\n"), &sshOut); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for invalid interactive ssh-key choice, got %v", err)
	}

	// 9. PublicSSHKey fallback from state when id_ed25519.pub is removed, and LoadState fallback from helper-state.json
	_ = os.Remove(filepath.Join(homeDir, ".ssh", "id_ed25519.pub"))
	if pub, err := mgr.PublicSSHKey(); err != nil || !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("PublicSSHKey state fallback = (%q, %v)", pub, err)
	}
	_ = os.Remove(filepath.Join(homeDir, ".config", "orbit", "state.json"))
	if st, err := mgr.LoadState(); err != nil || st.Git == nil {
		t.Fatalf("LoadState helper-state.json fallback = (%+v, %v)", st, err)
	}

	// 10. Corrupt OpenSSH PEM blocks in SetSSHKey
	corruptHeaderPEM := string(pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: []byte("openssh-key-v1\x00short")}))
	if _, err := mgr.SetSSHKey(corruptHeaderPEM); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for corrupt OpenSSH header")
	}
	unknownPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("bad")}))
	if _, err := mgr.SetSSHKey(unknownPEM); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for unsupported PEM key")
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
		"1",           // 1) Clone & setup workspace repositories
		"all",         // clone all
		"y",           // init missing flake.nix
		"2",           // 2) Initialize / autoload Nix flake
		"orbit-web",   // repo name
		"go,ripgrep",  // packages
		"3",           // 3) Configure AI & developer tools
		"1,2,3,4,5",   // multi-select by numbers (claude, agy, opencode, gh, git)
		"4",           // 4) Status & save
		"5",           // 5) Configure Git identity
		"Env User",    // user.name
		"dev@env.dev", // user.email
		"main",        // default branch
		"6",           // 6) Configure Git SSH key
		"1",           // 1) Generate new Ed25519 SSH keypair
		"6",           // 6) Configure Git SSH key
		"3",           // 3) Show current SSH public key
		"q",           // Quit
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
		"6",       // ssh-key
		"99",      // triggers error print in interactive loop
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
			{Name: "cloned-from-local", CloneURL: srcRepo, DefaultBranch: "main", GitToken: "tok_for_local_path"},
		},
	})
	if _, err := emptyMgr.CloneRepositories(ctx, helper.CloneOptions{CloneAll: true}); err != nil {
		t.Fatalf("defaultGitCloner real clone: %v", err)
	}
	if _, err := emptyMgr.CloneRepositories(ctx, helper.CloneOptions{NamesOrURLs: []string{"/nonexistent/git/repo/path"}}); err == nil {
		t.Fatalf("expected error when defaultGitCloner fails")
	}
	if _, err := emptyMgr.CloneRepositories(ctx, helper.CloneOptions{NamesOrURLs: []string{"", "bare-repo-name"}}); err == nil {
		t.Fatalf("expected error for bare-repo-name")
	}

	// DirenvAllower error propagation in EnableFlakeAutoload and AutoloadAllWorkspaceFlakes
	errMgr, _ := helper.NewManager(helper.Config{
		HomeDir: emptyHome,
		WorkDir: filepath.Join(emptyHome, "workspace"),
		DirenvAllower: func(_ context.Context, _ string) error {
			return errors.New("direnv failed")
		},
	})
	_ = os.WriteFile(filepath.Join(emptyHome, "workspace", "cloned-from-local", "flake.nix"), []byte("{}"), 0o644)
	if _, err := errMgr.AutoloadAllWorkspaceFlakes(ctx); err == nil {
		t.Fatalf("expected error when DirenvAllower fails in AutoloadAllWorkspaceFlakes")
	}
	var errMenuOut bytes.Buffer
	_ = errMgr.RunInteractive(ctx, strings.NewReader("1\nnonexistent\nn\n2\n\n2\ncloned-from-local\ngo\nq\n"), &errMenuOut)
}
