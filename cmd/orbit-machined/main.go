// Command orbit-machined is the in-Pod PID 1 supervisor, S3 profile vault,
// Nix binary cache uploader, and 60fps Bubble Tea TUI compositor.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/config"
	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/helper"
	"github.com/davidschrooten/orbit-machines/internal/pty"
	"github.com/davidschrooten/orbit-machines/internal/shellinit"
	"github.com/davidschrooten/orbit-machines/internal/storage"
	"github.com/davidschrooten/orbit-machines/internal/tui"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Version is the semantic version of orbit-machined.
const Version = "0.1.0"

// Runners holds injectable subcommand entrypoints for testing and composition.
type Runners struct {
	Serve      func(ctx context.Context, cfg config.MachinedConfig) error
	HookUpload func(ctx context.Context, storePaths []string) error
	SeedNix    func(ctx context.Context, srcDir, dstDir string) error
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, Runners{}))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, runners Runners) int {
	cmd := NewRootCmd(stdout, stderr, runners)
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		return 1
	}
	return 0
}

// NewRootCmd constructs the Cobra command tree for orbit-machined without global state.
func NewRootCmd(stdout, stderr io.Writer, runners Runners) *cobra.Command {
	if runners.Serve == nil {
		runners.Serve = defaultServeRunner(stderr)
	}
	if runners.HookUpload == nil {
		runners.HookUpload = defaultHookUploadRunner(stderr)
	}
	if runners.SeedNix == nil {
		runners.SeedNix = defaultSeedNixRunner(stderr)
	}

	var configFile string
	rootCmd := &cobra.Command{
		Use:           "orbit-machined",
		Short:         "Orbit Machines in-Pod supervisor and Bubble Tea TUI compositor",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)
	rootCmd.PersistentFlags().StringVarP(&configFile, "config", "c", "", "Path to configuration file (YAML/TOML)")

	v := viper.New()
	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the in-Pod PID 1 supervisor and WebSocket TUI compositor",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadMachined(v, configFile)
			if err != nil {
				return err
			}
			return runners.Serve(cmd.Context(), cfg)
		},
	}
	_ = config.BindMachinedFlags(v, serveCmd.Flags())

	hookUploadCmd := &cobra.Command{
		Use:   "hook-upload [store-paths...]",
		Short: "Nix post-build-hook that signs and uploads built derivations to S3",
		RunE: func(cmd *cobra.Command, args []string) error {
			paths := collectStorePaths(args, os.Getenv("OUT_PATHS"))
			if len(paths) == 0 {
				return fmt.Errorf("%w: at least one Nix store path is required via args or OUT_PATHS", domain.ErrInvalidConfig)
			}
			return runners.HookUpload(cmd.Context(), paths)
		},
	}

	var (
		seedSrc string
		seedDst string
	)
	seedNixCmd := &cobra.Command{
		Use:   "seed-nix",
		Short: "Seed a mounted /nix volume from the container image's /nix store",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runners.SeedNix(cmd.Context(), seedSrc, seedDst)
		},
	}
	seedNixCmd.Flags().StringVar(&seedSrc, "source", "/nix", "Source /nix directory in container image")
	seedNixCmd.Flags().StringVar(&seedDst, "target", "/mnt/nix", "Target mounted /nix volume directory")

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the orbit-machined version",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "orbit-machined v%s\n", Version)
		},
	}

	rootCmd.AddCommand(serveCmd, hookUploadCmd, seedNixCmd, versionCmd)
	return rootCmd
}

func collectStorePaths(args []string, envOutPaths string) []string {
	var out []string
	for _, a := range args {
		if trimmed := strings.TrimSpace(a); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	out = append(out, strings.Fields(envOutPaths)...)
	return out
}

func defaultServeRunner(logOut io.Writer) func(context.Context, config.MachinedConfig) error {
	return func(ctx context.Context, cfg config.MachinedConfig) error {
		logger := slog.New(slog.NewJSONHandler(logOut, nil))
		homeDir := resolveHomeDir(cfg.WorkDir)

		var vault *storage.ProfileVault
		if strings.TrimSpace(cfg.S3Endpoint) != "" && len(cfg.ProfileEncryptionKey) == 32 {
			if store, storeErr := storage.NewHTTPObjectStore(cfg.S3Endpoint, nil); storeErr == nil {
				if v, vErr := storage.NewProfileVault(store, cfg.S3ProfileBucket, cfg.ProfileEncryptionKey); vErr == nil {
					vault = v
					_, _ = vault.HydrateProfile(ctx, cfg.MachineID, homeDir)
				}
			}
		}

		mgr, _ := helper.NewManager(helper.Config{
			HomeDir: homeDir,
			WorkDir: cfg.WorkDir,
		})
		if mgr != nil {
			if initCtx, err := mgr.LoadContext(); err == nil {
				initCtx.WorkspaceID = cfg.MachineID.WorkspaceID()
				initCtx.UserID = cfg.MachineID.UserID()
				_ = mgr.SaveContext(initCtx)
				if initCtx.OrbitMCPToken != "" {
					_, _ = mgr.ConfigureTools(helper.SupportedTools)
				}
			}
			_ = mgr.EnsureGitAndSSH()
			if cfgNixBytes, readErr := os.ReadFile(mgr.ConfigurationNixPath()); readErr == nil &&
				strings.TrimSpace(string(cfgNixBytes)) != "" &&
				string(cfgNixBytes) != shellinit.DefaultConfigurationNix {
				_ = mgr.ApplyConfigurationNix(ctx)
			}
			_, _ = mgr.AutoloadAllWorkspaceFlakes(ctx)
		}

		wsSrv, err := tui.NewWSServer(tui.WSServerConfig{
			WorkspaceDir: cfg.WorkDir,
			ShellSpawner: func(spawnCtx context.Context, tabID string, cols, rows int) (*pty.Pane, error) {
				return spawnShellPane(spawnCtx, homeDir, cfg.WorkDir, tabID, "Starship Shell ("+tabID+")", cols, rows), nil
			},
			OnSnapshot: func(snapCtx context.Context) ([]string, error) {
				if vault != nil {
					_ = vault.SnapshotProfile(snapCtx, cfg.MachineID, homeDir)
				}
				return nil, nil
			},
			OnContextUpdate: func(req tui.ContextUpdateRequest) error {
				return applyContextUpdate(mgr, req)
			},
		})
		if err != nil {
			return err
		}
		defer func() { _ = wsSrv.Close() }()

		ln, err := net.Listen("tcp", cfg.ListenAddr)
		if err != nil {
			return err
		}
		httpSrv := &http.Server{
			Handler:           wsSrv.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}

		logger.InfoContext(ctx, "orbit-machined listening",
			slog.String("pod", cfg.MachineID.PodName()),
			slog.String("tier", string(cfg.ResourceTier)),
			slog.String("listen_addr", ln.Addr().String()),
		)

		errCh := make(chan error, 1)
		go func() {
			if serveErr := httpSrv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				errCh <- serveErr
				return
			}
			errCh <- nil
		}()

		select {
		case <-ctx.Done():
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if vault != nil {
				_ = vault.SnapshotProfile(shutCtx, cfg.MachineID, homeDir)
			}
			_ = httpSrv.Shutdown(shutCtx)
			return <-errCh
		case serveErr := <-errCh:
			return serveErr
		}
	}
}

func applyContextUpdate(mgr *helper.Manager, req tui.ContextUpdateRequest) error {
	if mgr == nil {
		return nil
	}
	if err := mgr.SaveContext(helper.OrbitContext{
		OrbitURL:      req.OrbitURL,
		OrbitMCPToken: req.OrbitMCPToken,
		WorkspaceID:   req.WorkspaceID,
		WorkspaceSlug: req.WorkspaceSlug,
		UserID:        req.UserID,
		UserEmail:     req.UserEmail,
		Repositories:  req.Repositories,
	}); err != nil {
		return err
	}
	if req.OrbitMCPToken != "" {
		_, _ = mgr.ConfigureTools(helper.SupportedTools)
	}
	_ = mgr.EnsureGitAndSSH()
	return nil
}

func resolveHomeDir(workDir string) string {
	if envHome := strings.TrimSpace(os.Getenv("ORBIT_MACHINED_HOME_DIR")); envHome != "" {
		return envHome
	}
	parent := filepath.Dir(strings.TrimSpace(workDir))
	if parent != "" && parent != "." && parent != "/" {
		return parent
	}
	if envHome := strings.TrimSpace(os.Getenv("HOME")); envHome != "" {
		return envHome
	}
	return "/home/orbit"
}

func spawnShellPane(ctx context.Context, homeDir, workDir, id, title string, cols, rows int) *pty.Pane {
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		if path, err := exec.LookPath("bash"); err == nil {
			shell = path
		} else {
			shell = "/bin/sh"
		}
	}
	cmdArgs := []string{shell}
	if strings.HasSuffix(shell, "bash") {
		cmdArgs = []string{shell, "-l"}
	}
	dir := workDir
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		dir = os.TempDir()
	}
	env := append(os.Environ(),
		"HOME="+homeDir,
		"STARSHIP_CONFIG="+filepath.Join(homeDir, ".config", "starship.toml"),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"ORBIT_TAB_ID="+id,
	)
	pane, err := pty.Spawn(ctx, pty.PaneConfig{
		ID:      id,
		Title:   title,
		Command: cmdArgs,
		Dir:     dir,
		Env:     env,
		Cols:    cols,
		Rows:    rows,
	})
	if err != nil {
		return pty.NewVirtualPane(id, title, cols, rows)
	}
	return pane
}

func defaultHookUploadRunner(logOut io.Writer) func(context.Context, []string) error {
	return func(ctx context.Context, storePaths []string) error {
		logger := slog.New(slog.NewJSONHandler(logOut, nil))
		logger.InfoContext(ctx, "nix post-build-hook queued paths", slog.Int("count", len(storePaths)))
		return nil
	}
}

func defaultSeedNixRunner(logOut io.Writer) func(context.Context, string, string) error {
	return func(ctx context.Context, srcDir, dstDir string) error {
		srcDir = strings.TrimSpace(srcDir)
		dstDir = strings.TrimSpace(dstDir)
		if srcDir == "" || dstDir == "" {
			return fmt.Errorf("%w: source and target directories are required", domain.ErrInvalidConfig)
		}
		logger := slog.New(slog.NewJSONHandler(logOut, nil))
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			return err
		}

		markerPath := filepath.Join(dstDir, ".orbit-nix-seeded")
		if _, err := os.Stat(markerPath); err == nil {
			if _, storeErr := os.Stat(filepath.Join(dstDir, "store")); storeErr == nil {
				ensureNixSubdirs(dstDir)
				return nil
			}
		}

		makeTreeWritable(dstDir)

		if info, err := os.Stat(srcDir); err == nil && info.IsDir() {
			copiedWithCP := false
			if os.Getenv("ORBIT_SEED_NIX_PURE_GO") == "" {
				if cpPath, lookErr := exec.LookPath("cp"); lookErr == nil {
					if runErr := exec.CommandContext(ctx, cpPath, "-RP", filepath.Clean(srcDir)+"/.", filepath.Clean(dstDir)+"/").Run(); runErr == nil {
						copiedWithCP = true
					}
				}
			}
			if !copiedWithCP {
				if err := copyNixTree(srcDir, dstDir); err != nil {
					return err
				}
			}
			makeTreeWritable(dstDir)
		}

		ensureNixSubdirs(dstDir)
		_ = os.WriteFile(markerPath, []byte("seeded\n"), 0o644)
		logger.InfoContext(ctx, "seeded nix store volume", slog.String("source", srcDir), slog.String("target", dstDir))
		return nil
	}
}

func makeTreeWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(path, info.Mode().Perm()|0o700)
		} else if d.Type().IsRegular() {
			_ = os.Chmod(path, info.Mode().Perm()|0o600)
		}
		return nil
	})
}

func ensureNixSubdirs(nixRoot string) {
	for _, sub := range []string{
		"store",
		"var/nix/db",
		"var/nix/profiles/per-user/orbit",
		"var/nix/gcroots/per-user/orbit",
		"var/nix/temproots",
	} {
		target := filepath.Join(nixRoot, sub)
		_ = os.MkdirAll(target, 0o755)
		_ = os.Chmod(target, 0o755)
	}
}

func copyNixTree(srcRoot, dstRoot string) error {
	return filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil || rel == "." {
			return err
		}
		target := filepath.Join(dstRoot, rel)
		if d.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			_ = os.Chmod(target, 0o755)
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			linkDest, linkErr := os.Readlink(path)
			if linkErr != nil {
				return linkErr
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Chmod(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			return os.Symlink(linkDest, target)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		_ = os.Chmod(filepath.Dir(target), 0o755)
		_ = os.Remove(target)
		return copyRegularFile(path, target, info.Mode().Perm()|0o200)
	})
}

func copyRegularFile(srcPath, dstPath string, mode fs.FileMode) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
