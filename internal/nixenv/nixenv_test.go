package nixenv_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/nixenv"
	"github.com/davidschrooten/orbit-machines/internal/storage"
)

func TestParseDevEnvJSONAndMergeEnv(t *testing.T) {
	t.Parallel()

	sampleJSON := []byte(`{
		"variables": {
			"PATH": {"type": "exported", "value": "/nix/store/abc-go-1.24/bin:/nix/store/def-git/bin"},
			"GOROOT": {"type": "exported", "value": "/nix/store/abc-go-1.24/share/go"},
			"INTERNAL_VAR": {"type": "var", "value": "ignored-non-exported"},
			"ARRAY_VAR": {"type": "array", "value": ["a", "b"]}
		}
	}`)

	vars, err := nixenv.ParseDevEnvJSON(sampleJSON)
	if err != nil {
		t.Fatalf("ParseDevEnvJSON: %v", err)
	}
	if vars["GOROOT"] != "/nix/store/abc-go-1.24/share/go" {
		t.Fatalf("unexpected GOROOT: %q", vars["GOROOT"])
	}
	if _, exists := vars["INTERNAL_VAR"]; exists {
		t.Fatalf("expected non-exported variable to be ignored")
	}

	if _, err := nixenv.ParseDevEnvJSON([]byte("{invalid-json")); err == nil {
		t.Fatalf("expected error on invalid JSON")
	}

	merged := nixenv.MergeEnv([]string{"HOME=/home/orbit", "PATH=/usr/bin:/bin", "USER=orbit"}, vars)
	envMap := make(map[string]string, len(merged))
	for _, kv := range merged {
		k, v, _ := strings.Cut(kv, "=")
		envMap[k] = v
	}
	wantPath := "/nix/store/abc-go-1.24/bin:/nix/store/def-git/bin:/usr/bin:/bin"
	if envMap["PATH"] != wantPath {
		t.Fatalf("merged PATH = %q, want %q", envMap["PATH"], wantPath)
	}
	if envMap["GOROOT"] != "/nix/store/abc-go-1.24/share/go" || envMap["HOME"] != "/home/orbit" {
		t.Fatalf("unexpected merged env: %+v", envMap)
	}
}

func TestDetectStacksAndGenerateFlakeNix(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	for _, f := range []string{"go.mod", "Gemfile", "package.json", "Cargo.toml", "pyproject.toml"} {
		if err := os.WriteFile(filepath.Join(repoDir, f), []byte(""), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", f, err)
		}
	}

	stacks, err := nixenv.DetectStacks(repoDir)
	if err != nil {
		t.Fatalf("DetectStacks: %v", err)
	}
	wantStacks := []string{"go", "nodejs", "python", "ruby", "rust"}
	if !reflect.DeepEqual(stacks, wantStacks) {
		t.Fatalf("DetectStacks = %v, want %v", stacks, wantStacks)
	}

	if nixenv.HasFlake(repoDir) {
		t.Fatalf("expected HasFlake=false before writing flake.nix")
	}

	flakeContent := nixenv.GenerateFlakeNix("Orbit workspace dev environment", []string{"go_1_24", "ruby_3_4", "nodejs_22"})
	if !strings.Contains(flakeContent, "go_1_24") || !strings.Contains(flakeContent, "ruby_3_4") || !strings.Contains(flakeContent, "mkShell") {
		t.Fatalf("unexpected generated flake.nix:\n%s", flakeContent)
	}

	if err := os.WriteFile(filepath.Join(repoDir, "flake.nix"), []byte(flakeContent), 0o644); err != nil {
		t.Fatalf("WriteFile flake.nix: %v", err)
	}
	if !nixenv.HasFlake(repoDir) {
		t.Fatalf("expected HasFlake=true after writing flake.nix")
	}
}

func TestCacheUploader_UploadStorePaths(t *testing.T) {
	t.Parallel()

	memStore := storage.NewMemoryObjectStore()
	if _, err := nixenv.NewCacheUploader(nil, "orbit-nix-cache"); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for nil store, got %v", err)
	}
	if _, err := nixenv.NewCacheUploader(memStore, ""); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty bucket, got %v", err)
	}

	uploader, err := nixenv.NewCacheUploader(memStore, "orbit-nix-cache")
	if err != nil {
		t.Fatalf("NewCacheUploader: %v", err)
	}

	ctx := context.Background()
	paths := []string{
		"/nix/store/0123456789abcdef0123456789abcdef-go-1.24.1",
		"/nix/store/fedcba9876543210fedcba9876543210-ripgrep-14.1.0",
	}
	count, err := uploader.UploadStorePaths(ctx, paths)
	if err != nil || count != 2 {
		t.Fatalf("UploadStorePaths = (%d, %v), want (2, nil)", count, err)
	}

	keys, err := memStore.ListObjects(ctx, "orbit-nix-cache", "")
	if err != nil || len(keys) != 2 {
		t.Fatalf("expected 2 narinfo objects in orbit-nix-cache, got %v (err=%v)", keys, err)
	}

	// Reject invalid store paths outside /nix/store/
	if _, err := uploader.UploadStorePaths(ctx, []string{"/tmp/not-nix-store"}); !errors.Is(err, nixenv.ErrInvalidStorePath) {
		t.Fatalf("expected ErrInvalidStorePath, got %v", err)
	}
}
