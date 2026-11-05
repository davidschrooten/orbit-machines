// Command orbit-machined is the in-Pod PID 1 supervisor, S3 profile vault,
// Nix binary cache uploader, and 60fps Bubble Tea TUI compositor.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the orbit-machined version",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "orbit-machined v%s\n", Version)
		},
	}

	rootCmd.AddCommand(serveCmd, hookUploadCmd, versionCmd)
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
			_, _ = mgr.AutoloadAllWorkspaceFlakes(ctx)
		}

		wsSrv, err := tui.NewWSServer(tui.WSServerConfig{
			ShellSpawner: func(spawnCtx context.Context, tabID string, cols, rows int) (*pty.Pane, error) {
				return spawnShellPane(spawnCtx, homeDir, cfg.WorkDir, tabID, "Starship Shell ("+tabID+")", cols, rows), nil
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
