// Command orbit-machined is the in-Pod PID 1 supervisor, S3 profile vault,
// Nix binary cache uploader, and 60fps Bubble Tea TUI compositor.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/davidschrooten/orbit-machines/internal/config"
	"github.com/davidschrooten/orbit-machines/internal/domain"
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
	for _, p := range strings.Fields(envOutPaths) {
		out = append(out, p)
	}
	return out
}

func defaultServeRunner(logOut io.Writer) func(context.Context, config.MachinedConfig) error {
	return func(ctx context.Context, cfg config.MachinedConfig) error {
		logger := slog.New(slog.NewJSONHandler(logOut, nil))
		logger.InfoContext(ctx, "orbit-machined configured",
			slog.String("pod", cfg.MachineID.PodName()),
			slog.String("tier", string(cfg.ResourceTier)),
			slog.String("listen_addr", cfg.ListenAddr),
		)
		return nil
	}
}

func defaultHookUploadRunner(logOut io.Writer) func(context.Context, []string) error {
	return func(ctx context.Context, storePaths []string) error {
		logger := slog.New(slog.NewJSONHandler(logOut, nil))
		logger.InfoContext(ctx, "nix post-build-hook queued paths", slog.Int("count", len(storePaths)))
		return nil
	}
}
