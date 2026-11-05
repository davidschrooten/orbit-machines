// Command orbit-helper is the interactive and flag-driven in-Pod helper CLI
// for cloning workspace repositories, initializing and auto-loading Nix flakes
// via direnv, configuring AI/developer tools (claude, agy, opencode, gh, git),
// and persisting state across ephemeral Machine Pod restarts.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/davidschrooten/orbit-machines/internal/helper"
	"github.com/spf13/cobra"
)

// Version is the semantic version of orbit-helper.
const Version = "0.1.0"

// ManagerFactory constructs a helper.Manager from CLI paths.
type ManagerFactory func(homeDir, workDir, machinedURL string) (*helper.Manager, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, nil))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, factory ManagerFactory) int {
	cmd := NewRootCmd(stdin, stdout, stderr, factory)
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		return 1
	}
	return 0
}

// NewRootCmd constructs the Cobra command tree for orbit-helper.
func NewRootCmd(stdin io.Reader, stdout, stderr io.Writer, factory ManagerFactory) *cobra.Command {
	if factory == nil {
		factory = func(homeDir, workDir, machinedURL string) (*helper.Manager, error) {
			return helper.NewManager(helper.Config{
				HomeDir:     homeDir,
				WorkDir:     workDir,
				MachinedURL: machinedURL,
			})
		}
	}

	defaultHome := strings.TrimSpace(os.Getenv("HOME"))
	if defaultHome == "" {
		defaultHome = "/home/orbit"
	}
	defaultWork := strings.TrimSpace(os.Getenv("ORBIT_MACHINED_WORK_DIR"))
	if defaultWork == "" {
		defaultWork = filepath.Join(defaultHome, "workspace")
	}

	var (
		homeDir     string
		workDir     string
		machinedURL string
		mgr         *helper.Manager
	)

	rootCmd := &cobra.Command{
		Use:           "orbit-helper",
		Short:         "Orbit Machine helper for workspace repos, Nix flakes, and AI tools",
		SilenceUsage:  true,
		SilenceErrors: false,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Name() == "version" {
				return nil
			}
			var err error
			mgr, err = factory(homeDir, workDir, machinedURL)
			return err
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return mgr.RunInteractive(cmd.Context(), stdin, cmd.OutOrStdout())
		},
	}
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)
	rootCmd.PersistentFlags().StringVar(&homeDir, "home-dir", defaultHome, "User home directory")
	rootCmd.PersistentFlags().StringVar(&workDir, "work-dir", defaultWork, "Workspace repositories root directory")
	rootCmd.PersistentFlags().StringVar(&machinedURL, "machined-url", "http://127.0.0.1:8080", "In-Pod orbit-machined HTTP endpoint")

	menuCmd := &cobra.Command{
		Use:   "menu",
		Short: "Launch the interactive Orbit Helper menu",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return mgr.RunInteractive(cmd.Context(), stdin, cmd.OutOrStdout())
		},
	}

	var (
		cloneAll  bool
		repoNames []string
		initFlake bool
	)
	reposCmd := &cobra.Command{
		Use:   "repos",
		Short: "List or clone Orbit workspace repositories and configure direnv flake auto-loading",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cloneAll && len(repoNames) == 0 {
				orbitCtx, _ := mgr.LoadContext()
				if len(orbitCtx.Repositories) == 0 {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No workspace repositories registered in Orbit context.")
					return nil
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "🪐 Orbit Workspace Repositories:")
				for _, r := range orbitCtx.Repositories {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  • %s (%s)\n", r.Name, r.CloneURL)
				}
				return nil
			}
			cloned, err := mgr.CloneRepositories(cmd.Context(), helper.CloneOptions{
				CloneAll:         cloneAll,
				NamesOrURLs:      repoNames,
				InitMissingFlake: initFlake,
			})
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Cloned and configured: %s\n", strings.Join(cloned, ", "))
			return nil
		},
	}
	reposCmd.Flags().BoolVar(&cloneAll, "clone-all", false, "Clone all repositories in the Orbit workspace")
	reposCmd.Flags().StringSliceVar(&repoNames, "repo", nil, "Repository name or Git URL to clone (can be specified multiple times)")
	reposCmd.Flags().BoolVar(&initFlake, "init-flake", true, "Generate a flake.nix if the cloned repository does not have one")

	var (
		flakeDir      string
		flakePackages []string
	)
	flakeCmd := &cobra.Command{
		Use:   "flake [init|autoload]",
		Short: "Initialize a Nix flake.nix or enable automatic flake loading via direnv",
		RunE: func(cmd *cobra.Command, args []string) error {
			action := "init"
			if len(args) > 0 {
				action = strings.ToLower(strings.TrimSpace(args[0]))
			}
			targetDir := strings.TrimSpace(flakeDir)
			if action == "autoload" {
				if targetDir != "" {
					if err := mgr.EnableFlakeAutoload(cmd.Context(), targetDir); err != nil {
						return err
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Enabled direnv flake auto-loading in %s\n", targetDir)
					return nil
				}
				enabled, err := mgr.AutoloadAllWorkspaceFlakes(cmd.Context())
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Enabled direnv flake auto-loading for: %s\n", strings.Join(enabled, ", "))
				return nil
			}
			if targetDir == "" {
				targetDir, _ = os.Getwd()
			}
			if err := mgr.InitFlake(cmd.Context(), targetDir, flakePackages); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Initialized flake.nix and direnv auto-loading in %s\n", targetDir)
			return nil
		},
	}
	flakeCmd.Flags().StringVar(&flakeDir, "dir", "", "Target repository directory")
	flakeCmd.Flags().StringSliceVar(&flakePackages, "packages", nil, "Nixpkgs attributes to include in flake.nix")

	var selectedTools []string
	toolsCmd := &cobra.Command{
		Use:   "tools",
		Short: "Configure multiple AI and developer tools (claude, agy, opencode, gh, git, or all)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(selectedTools) == 0 {
				selectedTools = []string{"all"}
			}
			configured, err := mgr.ConfigureTools(selectedTools)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Configured tools: %s\n", strings.Join(configured, ", "))
			return nil
		},
	}
	toolsCmd.Flags().StringSliceVar(&selectedTools, "select", nil, "Comma-separated tools to configure (claude,agy,opencode,gh,git,all)")

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Display Orbit workspace context, cloned repositories, and configured tools",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, _ := mgr.Status()
			_, _ = fmt.Fprint(cmd.OutOrStdout(), rep.Format())
			return nil
		},
	}

	saveCmd := &cobra.Command{
		Use:   "save",
		Short: "Persist Orbit helper state and trigger an immediate S3 profile + WIP snapshot",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_ = mgr.Save(cmd.Context())
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "✓ Saved Orbit helper state and triggered profile snapshot")
			return nil
		},
	}

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the orbit-helper version",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "orbit-helper v%s\n", Version)
		},
	}

	rootCmd.AddCommand(menuCmd, reposCmd, flakeCmd, toolsCmd, statusCmd, saveCmd, versionCmd)
	return rootCmd
}
