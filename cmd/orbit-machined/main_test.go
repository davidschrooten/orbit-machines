package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/config"
)

func TestMachinedCLI_Commands(t *testing.T) {
	t.Parallel()

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
		t.Parallel()
		var out, errOut bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		args := []string{
			"serve",
			"--listen-addr=127.0.0.1:0",
			"--workspace-id=ws_1",
			"--user-id=usr_1",
		}
		code := run(ctx, args, &out, &errOut, Runners{})
		if code != 0 {
			t.Fatalf("run(serve default) code = %d, stderr = %s", code, errOut.String())
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
