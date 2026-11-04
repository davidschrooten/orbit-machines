// Package machined implements the in-Pod supervisor lifecycle: boot profile
// hydration, terminal activity tracking, idle warnings, and pre-shutdown WIP
// repository snapshotting.
package machined

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/gitwip"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
	"github.com/davidschrooten/orbit-machines/internal/storage"
)

// SupervisorConfig configures the in-Pod Supervisor.
type SupervisorConfig struct {
	MachineID   domain.MachineID
	HomeDir     string
	WorkDir     string
	IdleTimeout time.Duration
	Vault       *storage.ProfileVault
	Bundler     *gitwip.Bundler
	Now         func() time.Time
}

// BootReport summarizes the state restored during BootHydrate.
type BootReport struct {
	ProfileRestored bool
	PendingWIPRepos []string
}

// Supervisor coordinates boot hydration, idle tracking, and pre-shutdown snapshots.
type Supervisor struct {
	mid         domain.MachineID
	homeDir     string
	workDir     string
	idleTimeout time.Duration
	vault       *storage.ProfileVault
	bundler     *gitwip.Bundler
	now         func() time.Time

	mu           sync.Mutex
	lastActivity time.Time
}

// NewSupervisor validates cfg and constructs a Supervisor.
func NewSupervisor(cfg SupervisorConfig) (*Supervisor, error) {
	if cfg.MachineID.WorkspaceID() == "" || cfg.MachineID.UserID() == "" {
		return nil, fmt.Errorf("%w: MachineID is required", domain.ErrInvalidConfig)
	}
	if strings.TrimSpace(cfg.HomeDir) == "" || strings.TrimSpace(cfg.WorkDir) == "" {
		return nil, fmt.Errorf("%w: HomeDir and WorkDir are required", domain.ErrInvalidConfig)
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Minute
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	return &Supervisor{
		mid:          cfg.MachineID,
		homeDir:      cfg.HomeDir,
		workDir:      cfg.WorkDir,
		idleTimeout:  idleTimeout,
		vault:        cfg.Vault,
		bundler:      cfg.Bundler,
		now:          nowFn,
		lastActivity: nowFn(),
	}, nil
}

// BootHydrate restores the user's encrypted S3 profile into HomeDir and checks
// which repositories in repoIDs have pending WIP git bundles.
func (s *Supervisor) BootHydrate(ctx context.Context, repoIDs []string) (BootReport, error) {
	var report BootReport
	if s.vault != nil {
		restored, err := s.vault.HydrateProfile(ctx, s.mid, s.homeDir)
		if err != nil {
			return BootReport{}, err
		}
		report.ProfileRestored = restored
	}
	if s.bundler != nil {
		for _, repoID := range repoIDs {
			hasPending, err := s.bundler.HasPendingBundle(ctx, s.mid, repoID)
			if err != nil {
				return BootReport{}, err
			}
			if hasPending {
				report.PendingWIPRepos = append(report.PendingWIPRepos, repoID)
			}
		}
	}
	return report, nil
}

// RecordActivity resets the idle timer to the current timestamp.
func (s *Supervisor) RecordActivity() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActivity = s.now()
}

// IdleDuration returns how long the machine has been inactive.
func (s *Supervisor) IdleDuration() time.Duration {
	s.mu.Lock()
	last := s.lastActivity
	s.mu.Unlock()
	d := s.now().Sub(last)
	if d < 0 {
		return 0
	}
	return d
}

// CheckIdle evaluates current inactivity against warnThreshold and IdleTimeout.
// When IdleDuration >= IdleTimeout, it runs GracefulPreShutdown and returns (true, nil).
func (s *Supervisor) CheckIdle(ctx context.Context, warnThreshold time.Duration, onWarning func(protocol.MetadataEvent)) (bool, error) {
	idle := s.IdleDuration()
	if idle >= s.idleTimeout {
		if _, err := s.GracefulPreShutdown(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	if warnThreshold > 0 && idle >= warnThreshold && onWarning != nil {
		remaining := s.idleTimeout - idle
		onWarning(protocol.MetadataEvent{
			Type:        protocol.MetaEventIdleWarning,
			IdleSeconds: int(idle.Seconds()),
			Message:     fmt.Sprintf("Machine idle; snapshotting and stopping in %s", remaining.Round(time.Second)),
		})
	}
	return false, nil
}

// GracefulPreShutdown snapshots the user's profile and any dirty git working trees
// under WorkDir within a bounded 30s timeout budget.
func (s *Supervisor) GracefulPreShutdown(ctx context.Context) ([]string, error) {
	shutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if s.vault != nil {
		if err := s.vault.SnapshotProfile(shutCtx, s.mid, s.homeDir); err != nil {
			return nil, err
		}
	}

	var snapshotted []string
	if s.bundler != nil {
		entries, err := os.ReadDir(s.workDir)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			repoPath := filepath.Join(s.workDir, entry.Name())
			if _, statErr := os.Stat(filepath.Join(repoPath, ".git")); statErr != nil {
				continue
			}
			created, err := s.bundler.SnapshotRepo(shutCtx, s.mid, entry.Name(), repoPath)
			if err != nil {
				return snapshotted, err
			}
			if created {
				snapshotted = append(snapshotted, entry.Name())
			}
		}
	}
	return snapshotted, nil
}
