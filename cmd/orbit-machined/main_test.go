package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/config"
	"github.com/davidschrooten/orbit-machines/internal/helper"
	"github.com/davidschrooten/orbit-machines/internal/tui"
)

func TestMachinedCLI_Commands(t *testing.T) {
	t.Run("version subcommand", func(t *testing.T) {
		t.Parallel()
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{"version"}, &out, &errOut, Runners{})
		if code != 0 {
			t.Fatalf("run(version) code = %d, stderr = %s", code, errOut.String())
		}
		if !strings.Contains(out.String(), "orbit-machined") {
			t.Errorf("version output = %q, want orbit-machined", out.String())
		}
	})

	t.Run("serve subcommand loads config and invokes custom runner", func(t *testing.T) {
		t.Parallel()
		var captured config.MachinedConfig
		runners := Runners{
			Serve: func(_ context.Context, cfg config.MachinedConfig) error {
				captured = cfg
				return nil
			},
		}
		var out, errOut bytes.Buffer
		args := []string{
			"serve",
			"--workspace-id=ws_99",
			"--user-id=usr_42",
			"--resource-tier=performance",
		}
		code := run(context.Background(), args, &out, &errOut, runners)
		if code != 0 {
			t.Fatalf("run(serve) code = %d, stderr = %s", code, errOut.String())
		}
		if captured.MachineID.PodName() != "machine-ws-99-usr-42" {
			t.Errorf("PodName = %q, want machine-ws-99-usr-42", captured.MachineID.PodName())
		}
	})

	t.Run("serve subcommand with default runner succeeds", func(t *testing.T) {
		var out, errOut bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		tmpHome := filepath.Join(t.TempDir(), "orbit-home")
		tmpWork := filepath.Join(tmpHome, "workspace")
		t.Setenv("ORBIT_MACHINED_HOME_DIR", tmpHome)
		t.Setenv("ORBIT_URL", "https://orbit.acme.dev")
		t.Setenv("ORBIT_WORKSPACE_SLUG", "acme")
		t.Setenv("ORBIT_MCP_TOKEN", "")
		args := []string{
			"serve",
			"--listen-addr=127.0.0.1:0",
			"--workspace-id=ws_1",
			"--user-id=usr_1",
			"--work-dir=" + tmpWork,
		}
		code := run(ctx, args, &out, &errOut, Runners{})
		if code != 0 {
			t.Fatalf("run(serve default) code = %d, stderr = %s", code, errOut.String())
		}
		// Verify default MCP configs were wired even when ORBIT_MCP_TOKEN was empty at boot
		for _, p := range []string{
			filepath.Join(tmpHome, ".claude.json"),
			filepath.Join(tmpHome, ".gemini", "config", "mcp_config.json"),
			filepath.Join(tmpHome, ".config", "opencode", "opencode.json"),
		} {
			raw, readErr := os.ReadFile(p)
			if readErr != nil || !strings.Contains(string(raw), "https://orbit.acme.dev/api/v1/workspaces/acme/mcp") {
				t.Fatalf("expected %s to contain workspace MCP URL after serve boot, got (%q, %v)", p, string(raw), readErr)
			}
		}
		t.Setenv("ORBIT_MACHINED_HOME_DIR", "")
		_ = resolveHomeDir(tmpWork)
		_ = resolveHomeDir("/")
		t.Setenv("SHELL", "")
		pane := spawnShellPane(ctx, tmpHome, "/nonexistent-work-dir", "tab-1", "Shell", 80, 24)
		_ = pane.Close()
		t.Setenv("SHELL", "/nonexistent/shell-binary")
		vp := spawnShellPane(ctx, tmpHome, tmpWork, "tab-2", "Fallback", 80, 24)
		_ = vp.Close()
		_ = applyContextUpdate(nil, tui.ContextUpdateRequest{})
		mgr, _ := helper.NewManager(helper.Config{HomeDir: tmpHome, WorkDir: tmpWork})
		_ = applyContextUpdate(mgr, tui.ContextUpdateRequest{
			OrbitURL:      "https://orbit.acme.dev",
			OrbitMCPToken: "omcp_tok_updated",
			WorkspaceSlug: "acme",
		})
		updatedClaude, _ := os.ReadFile(filepath.Join(tmpHome, ".claude.json"))
		if !strings.Contains(string(updatedClaude), "Bearer omcp_tok_updated") {
			t.Fatalf("expected applyContextUpdate to update .claude.json with Bearer token, got:\n%s", string(updatedClaude))
		}
	})

	t.Run("serve subcommand fails on missing required flags", func(t *testing.T) {
		t.Parallel()
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{"serve"}, &out, &errOut, Runners{})
		if code != 1 {
			t.Fatalf("expected exit code 1, got %d", code)
		}
	})

	t.Run("hook-upload subcommand parses store paths from args", func(t *testing.T) {
		t.Parallel()
		var gotPaths []string
		runners := Runners{
			HookUpload: func(_ context.Context, paths []string) error {
				gotPaths = paths
				return nil
			},
		}
		var out, errOut bytes.Buffer
		args := []string{"hook-upload", "/nix/store/abc-go", "/nix/store/def-ripgrep"}
		code := run(context.Background(), args, &out, &errOut, runners)
		if code != 0 {
			t.Fatalf("run(hook-upload) code = %d, stderr = %s", code, errOut.String())
		}
		want := []string{"/nix/store/abc-go", "/nix/store/def-ripgrep"}
		if !reflect.DeepEqual(gotPaths, want) {
			t.Errorf("gotPaths = %v, want %v", gotPaths, want)
		}
	})

	t.Run("hook-upload default runner and empty paths error", func(t *testing.T) {
		t.Parallel()
		var out, errOut bytes.Buffer
		if code := run(context.Background(), []string{"hook-upload", "/nix/store/abc"}, &out, &errOut, Runners{}); code != 0 {
			t.Fatalf("expected exit code 0 for default hook-upload runner, got %d", code)
		}
		if code := run(context.Background(), []string{"hook-upload"}, &out, &errOut, Runners{}); code != 1 {
			t.Fatalf("expected exit code 1 when no store paths provided, got %d", code)
		}
		errRunners := Runners{
			HookUpload: func(_ context.Context, _ []string) error {
				return errors.New("s3 upload failed")
			},
		}
		if code := run(context.Background(), []string{"hook-upload", "/nix/store/abc"}, &out, &errOut, errRunners); code != 1 {
			t.Fatalf("expected exit code 1 when hook-upload runner fails, got %d", code)
		}
	})
}

func TestMachinedCLI_HookUploadEnvOutPaths(t *testing.T) {
	t.Setenv("OUT_PATHS", "/nix/store/env1 /nix/store/env2")
	var gotPaths []string
	runners := Runners{
		HookUpload: func(_ context.Context, paths []string) error {
			gotPaths = paths
			return nil
		},
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"hook-upload", "/nix/store/arg1"}, &out, &errOut, runners)
	if code != 0 {
		t.Fatalf("run(hook-upload) code = %d, stderr = %s", code, errOut.String())
	}
	want := []string{"/nix/store/arg1", "/nix/store/env1", "/nix/store/env2"}
	if !reflect.DeepEqual(gotPaths, want) {
		t.Errorf("gotPaths = %v, want %v", gotPaths, want)
	}
}

func TestMachinedCLI_SeedNixCommand(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src-nix")
	dstDir := filepath.Join(t.TempDir(), "dst-nix")

	if err := os.MkdirAll(filepath.Join(srcDir, "store", "abc-pkg", "bin"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "store", "abc-pkg", "bin", "hello"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_ = os.Symlink("store/abc-pkg/bin/hello", filepath.Join(srcDir, "hello-link"))

	t.Setenv("ORBIT_SEED_NIX_PURE_GO", "1")
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"seed-nix", "--source=" + srcDir, "--target=" + dstDir}, &out, &errOut, Runners{})
	if code != 0 {
		t.Fatalf("seed-nix failed: code=%d stderr=%s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dstDir, "store", "abc-pkg", "bin", "hello")); err != nil {
		t.Fatalf("expected seeded store file to exist: %v", err)
	}
	if linkTarget, err := os.Readlink(filepath.Join(dstDir, "hello-link")); err != nil || linkTarget != "store/abc-pkg/bin/hello" {
		t.Fatalf("expected symlink to be preserved, got (%q, %v)", linkTarget, err)
	}
	// Second run is idempotent when .orbit-nix-seeded exists
	if code := run(context.Background(), []string{"seed-nix", "--source=" + srcDir, "--target=" + dstDir}, &out, &errOut, Runners{}); code != 0 {
		t.Fatalf("second seed-nix failed: code=%d", code)
	}
	// Empty source/target returns exit code 1
	if code := run(context.Background(), []string{"seed-nix", "--source=", "--target="}, &out, &errOut, Runners{}); code != 1 {
		t.Fatalf("expected exit code 1 for empty source/target, got %d", code)
	}
}

func TestMachinedCLI_SeedNixReadOnlyStorePermissions(t *testing.T) {
	for _, mode := range []string{"pure-go", "cp"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "pure-go" {
				t.Setenv("ORBIT_SEED_NIX_PURE_GO", "1")
			} else {
				t.Setenv("ORBIT_SEED_NIX_PURE_GO", "")
			}

			srcDir := filepath.Join(t.TempDir(), "src-nix")
			dstDir := filepath.Join(t.TempDir(), "dst-nix")
			t.Cleanup(func() {
				makeTreeWritable(srcDir)
				makeTreeWritable(dstDir)
			})

			srcPkgDir := filepath.Join(srcDir, "store", "abc-pkg")
			if err := os.MkdirAll(srcPkgDir, 0o755); err != nil {
				t.Fatalf("MkdirAll srcPkgDir: %v", err)
			}
			srcFile := filepath.Join(srcPkgDir, "hello.drv")
			if err := os.WriteFile(srcFile, []byte("updated-drv-content\n"), 0o444); err != nil {
				t.Fatalf("WriteFile srcFile: %v", err)
			}
			_ = os.Symlink("hello.drv", filepath.Join(srcPkgDir, "hello-link"))
			if err := os.Chmod(srcPkgDir, 0o555); err != nil {
				t.Fatalf("Chmod srcPkgDir: %v", err)
			}

			// Simulate a prior interrupted run leaving 0555 directory and 0444 file in dstDir without .orbit-nix-seeded
			dstPkgDir := filepath.Join(dstDir, "store", "abc-pkg")
			if err := os.MkdirAll(dstPkgDir, 0o755); err != nil {
				t.Fatalf("MkdirAll dstPkgDir: %v", err)
			}
			dstFile := filepath.Join(dstPkgDir, "hello.drv")
			if err := os.WriteFile(dstFile, []byte("stale-drv\n"), 0o444); err != nil {
				t.Fatalf("WriteFile dstFile: %v", err)
			}
			if err := os.Chmod(dstPkgDir, 0o555); err != nil {
				t.Fatalf("Chmod dstPkgDir: %v", err)
			}

			var out, errOut bytes.Buffer
			code := run(context.Background(), []string{"seed-nix", "--source=" + srcDir, "--target=" + dstDir}, &out, &errOut, Runners{})
			if code != 0 {
				t.Fatalf("seed-nix(%s) failed: code=%d stderr=%s", mode, code, errOut.String())
			}

			got, err := os.ReadFile(dstFile)
			if err != nil {
				t.Fatalf("ReadFile dstFile: %v", err)
			}
			if string(got) != "updated-drv-content\n" {
				t.Errorf("dstFile content = %q, want updated-drv-content", string(got))
			}
		})
	}
}
