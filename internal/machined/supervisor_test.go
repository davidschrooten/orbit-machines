package machined_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/gitwip"
	"github.com/davidschrooten/orbit-machines/internal/machined"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
	"github.com/davidschrooten/orbit-machines/internal/storage"
)

func initTestRepo(t *testing.T, dir string, dirty bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "Alice"},
		{"config", "user.email", "alice@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)
	for _, args := range [][]string{
		{"add", "main.go"},
		{"commit", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	if dirty {
		_ = os.WriteFile(filepath.Join(dir, "uncommitted.txt"), []byte("wip work\n"), 0o644)
	}
}

func TestSupervisor_BootIdleWarningPreShutdownAndRehydrate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	memStore := storage.NewMemoryObjectStore()
	vault, _ := storage.NewProfileVault(memStore, "orbit-user-profiles", []byte("0123456789abcdef0123456789abcdef"))
	bundler, _ := gitwip.NewBundler(vault)

	mid, _ := domain.NewMachineID("ws_01HXYZ", "usr_01HABC")
	home1 := t.TempDir()
	work1 := t.TempDir()

	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)

	if _, err := machined.NewSupervisor(machined.SupervisorConfig{}); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty SupervisorConfig, got %v", err)
	}
	if _, err := machined.NewSupervisor(machined.SupervisorConfig{MachineID: mid}); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for missing HomeDir/WorkDir, got %v", err)
	}

	sup1, err := machined.NewSupervisor(machined.SupervisorConfig{
		MachineID:   mid,
		HomeDir:     home1,
		WorkDir:     work1,
		IdleTimeout: 30 * time.Minute,
		Vault:       vault,
		Bundler:     bundler,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}

	// 1. First boot has no profile or WIP bundles.
	rep1, err := sup1.BootHydrate(ctx, []string{"orbit", "orbit-machines"})
	if err != nil {
		t.Fatalf("BootHydrate 1: %v", err)
	}
	if rep1.ProfileRestored || len(rep1.PendingWIPRepos) != 0 {
		t.Fatalf("expected clean first boot report, got %+v", rep1)
	}

	// 2. Write user config in home1 and create one dirty repo + one clean repo in work1.
	_ = os.MkdirAll(filepath.Join(home1, ".claude"), 0o755)
	_ = os.WriteFile(filepath.Join(home1, ".claude/CLAUDE.md"), []byte("# Saved on idle\n"), 0o600)
	_ = os.WriteFile(filepath.Join(work1, "README.txt"), []byte("non-dir file"), 0o644)
	_ = os.MkdirAll(filepath.Join(work1, "non-git-dir"), 0o755)
	initTestRepo(t, filepath.Join(work1, "orbit"), true)
	initTestRepo(t, filepath.Join(work1, "orbit-machines"), false)

	// 3. Advance clock to 26m (above 25m warning threshold, below 30m eviction timeout).
	now = now.Add(26 * time.Minute)
	var warningEvent *protocol.MetadataEvent
	evicted, err := sup1.CheckIdle(ctx, 25*time.Minute, func(ev protocol.MetadataEvent) {
		warningEvent = &ev
	})
	if err != nil || evicted {
		t.Fatalf("CheckIdle at 26m = (%v, %v), want (false, nil)", evicted, err)
	}
	if warningEvent == nil || warningEvent.Type != protocol.MetaEventIdleWarning {
		t.Fatalf("expected MetaEventIdleWarning at 26m, got %+v", warningEvent)
	}

	// 4. RecordActivity resets idle timer back to 0.
	sup1.RecordActivity()
	if sup1.IdleDuration() != 0 {
		t.Fatalf("expected IdleDuration=0 after RecordActivity, got %v", sup1.IdleDuration())
	}

	// 5. Advance clock by 31m -> triggers GracefulPreShutdown and returns evicted=true.
	now = now.Add(31 * time.Minute)
	evicted, err = sup1.CheckIdle(ctx, 25*time.Minute, nil)
	if err != nil || !evicted {
		t.Fatalf("CheckIdle at 31m = (%v, %v), want (true, nil)", evicted, err)
	}

	// 6. Boot a second Pod (sup2) with fresh home2 and work2 -> verify profile & pending WIP bundle!
	home2 := t.TempDir()
	work2 := t.TempDir()
	sup2, err := machined.NewSupervisor(machined.SupervisorConfig{
		MachineID:   mid,
		HomeDir:     home2,
		WorkDir:     work2,
		IdleTimeout: 30 * time.Minute,
		Vault:       vault,
		Bundler:     bundler,
	})
	if err != nil {
		t.Fatalf("NewSupervisor 2: %v", err)
	}

	rep2, err := sup2.BootHydrate(ctx, []string{"orbit", "orbit-machines"})
	if err != nil {
		t.Fatalf("BootHydrate 2: %v", err)
	}
	if !rep2.ProfileRestored {
		t.Fatalf("expected ProfileRestored=true on second boot")
	}
	if len(rep2.PendingWIPRepos) != 1 || rep2.PendingWIPRepos[0] != "orbit" {
		t.Fatalf("expected PendingWIPRepos=[orbit], got %v", rep2.PendingWIPRepos)
	}
	restoredClaude, err := os.ReadFile(filepath.Join(home2, ".claude/CLAUDE.md"))
	if err != nil || string(restoredClaude) != "# Saved on idle\n" {
		t.Fatalf("restored CLAUDE.md = %q, err=%v", restoredClaude, err)
	}
}

func TestSupervisor_EdgeAndErrorCases(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	memStore := storage.NewMemoryObjectStore()
	vault, _ := storage.NewProfileVault(memStore, "orbit-user-profiles", []byte("0123456789abcdef0123456789abcdef"))
	bundler, _ := gitwip.NewBundler(vault)
	mid, _ := domain.NewMachineID("ws_01HXYZ", "usr_01HABC")

	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	notADir := filepath.Join(t.TempDir(), "file-instead-of-dir")
	_ = os.WriteFile(notADir, []byte("x"), 0o600)

	sup, err := machined.NewSupervisor(machined.SupervisorConfig{
		MachineID: mid,
		HomeDir:   t.TempDir(),
		WorkDir:   notADir,
		Vault:     vault,
		Bundler:   bundler,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}

	// Negative clock skew clamps IdleDuration to 0.
	now = now.Add(-1 * time.Minute)
	if sup.IdleDuration() != 0 {
		t.Fatalf("expected 0 IdleDuration on backward clock skew, got %v", sup.IdleDuration())
	}

	// Corrupted profile archive causes BootHydrate to fail.
	_ = memStore.PutObject(ctx, "orbit-user-profiles", mid.ProfileObjectKey(), []byte("bad-magic"))
	if _, err := sup.BootHydrate(ctx, []string{"orbit"}); err == nil {
		t.Fatalf("expected BootHydrate error on corrupted profile archive")
	}
	_ = memStore.DeleteObject(ctx, "orbit-user-profiles", mid.ProfileObjectKey())

	// Non-directory WorkDir causes GracefulPreShutdown and CheckIdle at timeout to return an error.
	now = now.Add(35 * time.Minute)
	if _, err := sup.CheckIdle(ctx, 25*time.Minute, nil); err == nil {
		t.Fatalf("expected CheckIdle error when WorkDir is a file")
	}
}
