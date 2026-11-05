package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidschrooten/orbit-machines/internal/helper"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
)

func TestOrbitHelperCLI_Subcommands(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), "home")
	workDir := filepath.Join(homeDir, "workspace")

	mgr, err := helper.NewManager(helper.Config{
		HomeDir: homeDir,
		WorkDir: workDir,
		GitCloner: func(_ context.Context, _, destDir, _ string) error {
			return os.MkdirAll(filepath.Join(destDir, ".git"), 0o755)
		},
		DirenvAllower: func(_ context.Context, _ string) error { return nil },
		NixEnvRunner:  func(_ context.Context, _ string) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_ = mgr.SaveContext(helper.OrbitContext{
		OrbitURL:      "https://orbit.acme.dev",
		OrbitMCPToken: "omcp_tok_cli",
		WorkspaceID:   "ws_cli",
		WorkspaceSlug: "acme",
		UserID:        "usr_cli",
		UserEmail:     "cli@acme.dev",
		Repositories: []protocol.RepoCredential{
			{ID: "r1", Name: "orbit", CloneURL: "https://github.com/acme/orbit.git"},
		},
	})

	factory := func(_, _ string, _ string) (*helper.Manager, error) {
		return mgr, nil
	}

	execCLI := func(stdin string, args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(context.Background(), args, strings.NewReader(stdin), &out, &errOut, factory)
		return code, out.String(), errOut.String()
	}

	// 1. version
	if code, out, _ := execCLI("", "version"); code != 0 || !strings.Contains(out, "orbit-helper v") {
		t.Fatalf("version failed: code=%d out=%q", code, out)
	}

	// 2. repos (list mode)
	if code, out, _ := execCLI("", "repos"); code != 0 || !strings.Contains(out, "orbit") {
		t.Fatalf("repos list failed: code=%d out=%q", code, out)
	}

	// 3. repos --clone-all --init-flake
	if code, out, errOut := execCLI("", "repos", "--clone-all", "--init-flake"); code != 0 || !strings.Contains(out, "orbit") {
		t.Fatalf("repos --clone-all failed: code=%d out=%q err=%q", code, out, errOut)
	}

	// 4. flake init & flake autoload
	customDir := filepath.Join(workDir, "svc-a")
	if code, out, errOut := execCLI("", "flake", "init", "--dir", customDir, "--packages", "go,jq"); code != 0 || !strings.Contains(out, "svc-a") {
		t.Fatalf("flake init failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := execCLI("", "flake", "autoload"); code != 0 || !strings.Contains(out, "svc-a") {
		t.Fatalf("flake autoload failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, _, errOut := execCLI("", "flake", "autoload", "--dir", customDir); code != 0 {
		t.Fatalf("flake autoload --dir failed: code=%d err=%q", code, errOut)
	}

	// 5. tools --select claude,agy,opencode,gh,git and positional tools
	if code, out, errOut := execCLI("", "tools", "--select", "claude,agy,opencode,gh,git"); code != 0 || !strings.Contains(out, "claude") {
		t.Fatalf("tools --select failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := execCLI("", "tools", "claude", "gemini", "codex", "cursor", "aider"); code != 0 || !strings.Contains(out, "gemini") {
		t.Fatalf("tools positional failed: code=%d out=%q err=%q", code, out, errOut)
	}

	// 5b. init, git (flags & interactive), and ssh-key (--show, --generate, --set, --import, interactive)
	if code, out, errOut := execCLI("", "init"); code != 0 || !strings.Contains(out, "Initialized") {
		t.Fatalf("init failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := execCLI("", "git", "--name", "CLI User", "--email", "cli@acme.dev", "--branch", "main"); code != 0 || !strings.Contains(out, "CLI User") {
		t.Fatalf("git flags failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := execCLI("Interactive User\ninter@acme.dev\nmain\n", "git"); code != 0 || !strings.Contains(out, "Interactive User") {
		t.Fatalf("git interactive failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, _ := execCLI("", "ssh-key", "--show"); code != 0 || !strings.Contains(out, "No SSH public key") {
		t.Fatalf("ssh-key --show (empty) failed: code=%d out=%q", code, out)
	}
	if code, out, errOut := execCLI("", "ssh-key", "--generate", "--comment", "cli@acme.dev"); code != 0 || !strings.Contains(out, "ssh-ed25519") {
		t.Fatalf("ssh-key --generate failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, _ := execCLI("", "ssh-key", "--show"); code != 0 || !strings.Contains(out, "ssh-ed25519") {
		t.Fatalf("ssh-key --show (after generate) failed: code=%d out=%q", code, out)
	}
	st, _ := mgr.LoadState()
	if code, out, errOut := execCLI("", "ssh-key", "--set", st.SSHKey.PrivateKey); code != 0 || !strings.Contains(out, "Imported SSH key") {
		t.Fatalf("ssh-key --set failed: code=%d out=%q err=%q", code, out, errOut)
	}
	keyFile := filepath.Join(t.TempDir(), "import_key")
	_ = os.WriteFile(keyFile, []byte(st.SSHKey.PrivateKey), 0o600)
	if code, out, errOut := execCLI("", "ssh-key", "--import", keyFile); code != 0 || !strings.Contains(out, "Imported SSH key") {
		t.Fatalf("ssh-key --import file failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := execCLI(st.SSHKey.PrivateKey, "ssh-key", "--import", "-"); code != 0 || !strings.Contains(out, "Imported SSH key") {
		t.Fatalf("ssh-key --import stdin failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := execCLI("3\n", "ssh-key"); code != 0 || !strings.Contains(out, "ssh-ed25519") {
		t.Fatalf("ssh-key interactive show failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, _, _ := execCLI("", "flake", "go", "--dir", customDir); code != 0 {
		t.Fatalf("flake go shorthand failed: code=%d", code)
	}
	if code, _, _ := execCLI("", "flake", "init", "go", "--dir", customDir); code != 0 {
		t.Fatalf("flake init go failed: code=%d", code)
	}
	if code, _, _ := execCLI("", "ssh-key", "--import", "/nonexistent/key/path"); code != 1 {
		t.Fatalf("expected exit code 1 on missing ssh-key import file, got %d", code)
	}
	if code, _, _ := execCLI("", "ssh-key", "--set", "invalid-pem"); code != 1 {
		t.Fatalf("expected exit code 1 on invalid ssh-key --set, got %d", code)
	}
	if code, _, _ := execCLI("", "git", "--name", "OnlyName"); code != 1 {
		t.Fatalf("expected exit code 1 when git --email is missing, got %d", code)
	}

	// 6. status & save
	if code, out, _ := execCLI("", "status"); code != 0 || !strings.Contains(out, "Orbit Helper Status") {
		t.Fatalf("status failed: code=%d out=%q", code, out)
	}
	if code, out, _ := execCLI("", "save"); code != 0 || !strings.Contains(out, "Saved") {
		t.Fatalf("save failed: code=%d out=%q", code, out)
	}
	if code, out, _ := execCLI("", "rebuild", "--path"); code != 0 || !strings.Contains(out, "configuration.nix") {
		t.Fatalf("rebuild --path failed: code=%d out=%q", code, out)
	}
	if code, out, errOut := execCLI("", "rebuild", "--add", "tmux", "neovim"); code != 0 || !strings.Contains(out, "Applied Nix packages") {
		t.Fatalf("rebuild --add failed: code=%d out=%q err=%q", code, out, errOut)
	}

	// 7. default interactive menu (root command & menu subcommand)
	if code, out, _ := execCLI("q\n"); code != 0 || !strings.Contains(out, "🪐 Orbit Helper") {
		t.Fatalf("interactive root failed: code=%d out=%q", code, out)
	}
	if code, out, _ := execCLI("q\n", "menu"); code != 0 || !strings.Contains(out, "🪐 Orbit Helper") {
		t.Fatalf("interactive menu failed: code=%d out=%q", code, out)
	}

	// 8. Default factory with --home-dir and --work-dir flags, empty repos list, default tools, and error exit code
	var out, errOut bytes.Buffer
	tmpHome := filepath.Join(t.TempDir(), "def-home")
	tmpWork := filepath.Join(tmpHome, "workspace")
	if code := run(context.Background(), []string{"--home-dir", tmpHome, "--work-dir", tmpWork, "status"}, strings.NewReader(""), &out, &errOut, nil); code != 0 {
		t.Fatalf("default factory status failed: code=%d err=%q", code, errOut.String())
	}
	if code := run(context.Background(), []string{"--home-dir", tmpHome, "--work-dir", tmpWork, "repos"}, strings.NewReader(""), &out, &errOut, nil); code != 0 {
		t.Fatalf("empty repos list failed: code=%d", code)
	}
	if code := run(context.Background(), []string{"--home-dir", tmpHome, "--work-dir", tmpWork, "tools"}, strings.NewReader(""), &out, &errOut, nil); code != 0 {
		t.Fatalf("default tools failed: code=%d", code)
	}
	if code := run(context.Background(), []string{"--home-dir", tmpHome, "--work-dir", tmpWork, "flake", "--dir", tmpWork}, strings.NewReader(""), &out, &errOut, nil); code != 0 {
		t.Fatalf("default flake failed: code=%d", code)
	}
	if code := run(context.Background(), []string{"--home-dir", tmpHome, "--work-dir", tmpWork, "repos", "--repo", "/nonexistent"}, strings.NewReader(""), &out, &errOut, nil); code != 1 {
		t.Fatalf("expected exit code 1 on failing repo clone, got %d", code)
	}
	if code := run(context.Background(), []string{"--home-dir", tmpHome, "--work-dir", tmpWork, "tools", "--select", "invalid"}, strings.NewReader(""), &out, &errOut, nil); code != 1 {
		t.Fatalf("expected exit code 1 on invalid tool, got %d", code)
	}

	// Default env fallbacks when HOME and ORBIT_MACHINED_WORK_DIR are unset
	t.Setenv("HOME", "")
	t.Setenv("ORBIT_MACHINED_WORK_DIR", "")
	_ = NewRootCmd(strings.NewReader(""), &out, &errOut, nil)
}
