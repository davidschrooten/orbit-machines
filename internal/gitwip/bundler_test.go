package gitwip_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/gitwip"
	"github.com/davidschrooten/orbit-machines/internal/storage"
	"github.com/klauspost/compress/zstd"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Alice",
		"GIT_AUTHOR_EMAIL=alice@example.com",
		"GIT_COMMITTER_NAME=Alice",
		"GIT_COMMITTER_EMAIL=alice@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}

func TestBundler_CleanDirtySnapshotAndRestoreLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	memStore := storage.NewMemoryObjectStore()
	vault, err := storage.NewProfileVault(memStore, "orbit-user-profiles", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewProfileVault: %v", err)
	}

	if _, err := gitwip.NewBundler(nil); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for nil vault, got %v", err)
	}
	bundler, err := gitwip.NewBundler(vault)
	if err != nil {
		t.Fatalf("NewBundler: %v", err)
	}

	mid, _ := domain.NewMachineID("ws_01HXYZ", "usr_01HABC")
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Orbit\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "Initial commit")

	// 1. Clean working tree is not dirty and does not create a bundle.
	dirty, err := bundler.IsDirty(ctx, repoDir)
	if err != nil {
		t.Fatalf("IsDirty clean: %v", err)
	}
	if dirty {
		t.Fatalf("expected clean repo to report dirty=false")
	}
	created, err := bundler.SnapshotRepo(ctx, mid, "repo_01", repoDir)
	if err != nil || created {
		t.Fatalf("expected SnapshotRepo on clean repo to return (false, nil), got (%v, %v)", created, err)
	}

	// 2. Make local commit on a feature branch + uncommitted modifications + untracked file.
	runGit(t, repoDir, "checkout", "-b", "feat/wip-work")
	if err := os.WriteFile(filepath.Join(repoDir, "committed.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile committed.go: %v", err)
	}
	runGit(t, repoDir, "add", "committed.go")
	runGit(t, repoDir, "commit", "-m", "Add committed.go")

	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Orbit Modified\n"), 0o644); err != nil {
		t.Fatalf("WriteFile README.md: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "internal/pkg"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "internal/pkg/untracked.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatalf("WriteFile untracked.go: %v", err)
	}
	_ = os.Symlink(filepath.Join(repoDir, "README.md"), filepath.Join(repoDir, "link-ignored"))

	dirty, err = bundler.IsDirty(ctx, repoDir)
	if err != nil || !dirty {
		t.Fatalf("expected dirty=true, got (%v, %v)", dirty, err)
	}

	// 3. Snapshot dirty repo to S3.
	created, err = bundler.SnapshotRepo(ctx, mid, "repo_01", repoDir)
	if err != nil || !created {
		t.Fatalf("SnapshotRepo dirty = (%v, %v), want (true, nil)", created, err)
	}

	hasPending, err := bundler.HasPendingBundle(ctx, mid, "repo_01")
	if err != nil || !hasPending {
		t.Fatalf("HasPendingBundle = (%v, %v), want (true, nil)", hasPending, err)
	}

	// 4. Restore into a fresh clone/empty repoDir on next Pod boot.
	restoredDir := t.TempDir()
	runGit(t, restoredDir, "init", "-b", "main")
	restored, err := bundler.RestoreRepo(ctx, mid, "repo_01", restoredDir)
	if err != nil || !restored {
		t.Fatalf("RestoreRepo = (%v, %v), want (true, nil)", restored, err)
	}

	// Verify committed file from git bundle, modified tracked file, and untracked file are all present.
	for rel, want := range map[string]string{
		"committed.go":              "package main\n",
		"README.md":                 "# Orbit Modified\n",
		"internal/pkg/untracked.go": "package pkg\n",
	} {
		got, err := os.ReadFile(filepath.Join(restoredDir, rel))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("restored %s = %q, want %q", rel, got, want)
		}
	}

	// Verify bundle was deleted from S3 after successful restore, and second RestoreRepo returns (false, nil).
	hasPending, err = bundler.HasPendingBundle(ctx, mid, "repo_01")
	if err != nil || hasPending {
		t.Fatalf("expected HasPendingBundle=false after restore, got (%v, %v)", hasPending, err)
	}
	restoredAgain, err := bundler.RestoreRepo(ctx, mid, "repo_01", restoredDir)
	if err != nil || restoredAgain {
		t.Fatalf("expected RestoreRepo with no bundle to return (false, nil), got (%v, %v)", restoredAgain, err)
	}

	// 5. Error paths: non-git directory and corrupt encrypted WIP bundle.
	if _, err := bundler.IsDirty(ctx, t.TempDir()); err == nil {
		t.Fatalf("expected error running IsDirty on non-git directory")
	}
	_ = memStore.PutObject(ctx, "orbit-user-profiles", mid.WIPBundleObjectKey("repo_bad"), []byte("corrupt-ciphertext"))
	if _, err := bundler.RestoreRepo(ctx, mid, "repo_bad", restoredDir); !errors.Is(err, storage.ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed on corrupt WIP bundle, got %v", err)
	}

	// Corrupt zstd inside valid encrypted bundle.
	badZstdKey := mid.WIPBundleObjectKey("repo_badzstd")
	badZstdCipher, _ := vault.EncryptBlob([]byte("not-zstd"), badZstdKey)
	_ = memStore.PutObject(ctx, "orbit-user-profiles", badZstdKey, badZstdCipher)
	if _, err := bundler.RestoreRepo(ctx, mid, "repo_badzstd", restoredDir); err == nil {
		t.Fatalf("expected error on corrupt zstd in RestoreRepo")
	}

	// Zip-slip inside valid tar.zst WIP bundle.
	var evilTar bytes.Buffer
	tw := tar.NewWriter(&evilTar)
	_ = tw.WriteHeader(&tar.Header{Name: "worktree/", Typeflag: tar.TypeDir})
	_ = tw.WriteHeader(&tar.Header{Name: "worktree/../escape.txt", Mode: 0, Size: 3, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("bad"))
	_ = tw.Close()
	enc, _ := zstd.NewWriter(nil)
	evilKey := mid.WIPBundleObjectKey("repo_zipslip")
	evilCipher, _ := vault.EncryptBlob(enc.EncodeAll(evilTar.Bytes(), nil), evilKey)
	_ = memStore.PutObject(ctx, "orbit-user-profiles", evilKey, evilCipher)
	if _, err := bundler.RestoreRepo(ctx, mid, "repo_zipslip", restoredDir); !errors.Is(err, storage.ErrUnsafeArchiveEntry) {
		t.Fatalf("expected ErrUnsafeArchiveEntry on zip-slip WIP bundle, got %v", err)
	}
}
