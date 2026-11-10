// Command orbit-helper is the interactive and flag-driven in-Pod helper CLI
// for cloning workspace repositories, initializing and auto-loading Nix flakes
// via direnv, configuring AI/developer tools (claude, agy, opencode, gh, git),
// managing Git identity and SSH keys, and persisting state across ephemeral Machine Pod restarts.
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
const Version = "0.2.0"

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
		Short:         "Orbit Machine helper for workspace repos, Nix flakes, Git/SSH keys, and AI tools",
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

	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Ensure shell, Git identity, SSH key, and workspace Nix flakes are initialized",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := mgr.EnsureGitAndSSH(); err != nil {
				return err
			}
			_, _ = mgr.AutoloadAllWorkspaceFlakes(cmd.Context())
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "✓ Initialized Orbit shell, Git identity, SSH key, and workspace flakes")
			return nil
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
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  • %s (%s)\n", r.Name, helper.ToSSHCloneURL(r.CloneURL))
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
		Use:   "flake [init|autoload] [stack]",
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
			pkgs := append([]string(nil), flakePackages...)
			if len(args) > 1 {
				pkgs = append(pkgs, args[1:]...)
			} else if action != "init" && action != "" {
				pkgs = append(pkgs, action)
			}
			if err := mgr.InitFlake(cmd.Context(), targetDir, pkgs); err != nil {
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
		Use:   "tools [tool...]",
		Short: "Install and configure multiple AI and developer tools (claude, codex, gemini, opencode, cursor, aider, agy, gh, git, or all)",
		RunE: func(cmd *cobra.Command, args []string) error {
			tools := append([]string(nil), selectedTools...)
			tools = append(tools, args...)
			if len(tools) == 0 {
				tools = []string{"all"}
			}
			configured, err := mgr.ConfigureToolsWithContext(cmd.Context(), tools, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Configured tools: %s\n", strings.Join(configured, ", "))
			return nil
		},
	}
	toolsCmd.Flags().StringSliceVar(&selectedTools, "select", nil, "Comma-separated tools to configure (claude,agy,opencode,gh,git,all)")

	var (
		gitName   string
		gitEmail  string
		gitBranch string
	)
	gitCmd := &cobra.Command{
		Use:   "git",
		Short: "Configure Git author identity (user.name, user.email) and default branch",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(gitName) == "" && strings.TrimSpace(gitEmail) == "" {
				return mgr.RunInteractiveGit(stdin, cmd.OutOrStdout())
			}
			if err := mgr.ConfigureGit(gitName, gitEmail, gitBranch); err != nil {
				return err
			}
			branch := strings.TrimSpace(gitBranch)
			if branch == "" {
				branch = "main"
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Configured Git identity: %s <%s> (defaultBranch: %s)\n", strings.TrimSpace(gitName), strings.TrimSpace(gitEmail), branch)
			return nil
		},
	}
	gitCmd.Flags().StringVar(&gitName, "name", "", "Git author name (user.name)")
	gitCmd.Flags().StringVar(&gitEmail, "email", "", "Git author email (user.email)")
	gitCmd.Flags().StringVar(&gitBranch, "branch", "main", "Git default branch (init.defaultBranch)")

	var (
		sshGenerate bool
		sshShow     bool
		sshSet      string
		sshImport   string
		sshComment  string
	)
	sshKeyCmd := &cobra.Command{
		Use:   "ssh-key",
		Short: "Generate, import, or display your Git Ed25519 SSH key",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if sshGenerate {
				cfg, err := mgr.GenerateSSHKey(sshComment)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Generated Ed25519 SSH keypair (~/.ssh/id_ed25519)\nPublic key (add to GitHub/GitLab):\n%s\n", cfg.PublicKey)
				return nil
			}
			if strings.TrimSpace(sshSet) != "" {
				cfg, err := mgr.SetSSHKey(sshSet)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Imported SSH key (~/.ssh/id_ed25519)\nPublic key:\n%s\n", cfg.PublicKey)
				return nil
			}
			if strings.TrimSpace(sshImport) != "" {
				var raw []byte
				var err error
				if strings.TrimSpace(sshImport) == "-" {
					raw, err = io.ReadAll(stdin)
				} else {
					raw, err = os.ReadFile(strings.TrimSpace(sshImport))
				}
				if err != nil {
					return err
				}
				cfg, err := mgr.SetSSHKey(string(raw))
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Imported SSH key (~/.ssh/id_ed25519)\nPublic key:\n%s\n", cfg.PublicKey)
				return nil
			}
			if sshShow {
				pub, err := mgr.PublicSSHKey()
				if err != nil {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No SSH public key found. Run 'orbit ssh-key --generate' to create one.")
					return nil
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", pub)
				return nil
			}
			return mgr.RunInteractiveSSHKey(stdin, cmd.OutOrStdout())
		},
	}
	sshKeyCmd.Flags().BoolVar(&sshGenerate, "generate", false, "Generate a new Ed25519 SSH keypair")
	sshKeyCmd.Flags().BoolVar(&sshShow, "show", false, "Print the current SSH public key")
	sshKeyCmd.Flags().StringVar(&sshSet, "set", "", "Import an SSH private key PEM string directly")
	sshKeyCmd.Flags().StringVar(&sshImport, "import", "", "Import an SSH private key from a file path (or '-' for stdin)")
	sshKeyCmd.Flags().StringVar(&sshComment, "comment", "", "Optional comment for generated SSH key")

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Display Orbit workspace context, cloned repositories, Git/SSH key, and configured tools",
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

	var (
		rebuildPrintPath  bool
		rebuildAddPkgs    []string
		rebuildRemovePkgs []string
	)
	rebuildCmd := &cobra.Command{
		Use:     "rebuild [package...]",
		Aliases: []string{"config-nix", "nix-rebuild"},
		Short:   "Apply declarative Nix system packages from ~/.config/orbit/configuration.nix",
		RunE: func(cmd *cobra.Command, args []string) error {
			if rebuildPrintPath {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), mgr.ConfigurationNixPath())
				return nil
			}
			pkgs := append([]string(nil), rebuildAddPkgs...)
			pkgs = append(pkgs, args...)
			if len(pkgs) > 0 {
				if err := mgr.AddConfigurationNixPackages(pkgs); err != nil {
					return err
				}
			}
			if len(rebuildRemovePkgs) > 0 {
				if err := mgr.RemoveConfigurationNixPackages(rebuildRemovePkgs); err != nil {
					return err
				}
			}
			if err := mgr.ApplyConfigurationNix(cmd.Context()); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Applied Nix packages from %s\n", mgr.ConfigurationNixPath())
			return nil
		},
	}
	rebuildCmd.Flags().BoolVar(&rebuildPrintPath, "path", false, "Print the path to ~/.config/orbit/configuration.nix")
	rebuildCmd.Flags().StringSliceVar(&rebuildAddPkgs, "add", nil, "Add Nixpkgs attribute names to configuration.nix before rebuilding")
	rebuildCmd.Flags().StringSliceVar(&rebuildRemovePkgs, "remove", nil, "Remove Nixpkgs attribute names from configuration.nix before rebuilding")

	packagesCmd := &cobra.Command{
		Use:     "packages [search|install|uninstall|list] [args...]",
		Aliases: []string{"pkg", "pkgs", "deps"},
		Short:   "Search nixpkgs, install, uninstall, or list system packages in ~/.config/orbit/configuration.nix",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return mgr.RunInteractivePackages(cmd.Context(), stdin, cmd.OutOrStdout())
			}
			action := strings.ToLower(strings.TrimSpace(args[0]))
			rest := args[1:]
			switch action {
			case "search", "find", "s":
				query := strings.TrimSpace(strings.Join(rest, " "))
				results, err := mgr.SearchNixPackages(cmd.Context(), query)
				if err != nil {
					return err
				}
				if len(results) == 0 {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No packages found matching %q.\n", query)
					return nil
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "📦 Nixpkgs search results for %q:\n", query)
				for _, r := range results {
					ver := ""
					if r.Version != "" {
						ver = " (" + r.Version + ")"
					}
					desc := strings.TrimSpace(r.Description)
					if desc != "" {
						desc = " — " + desc
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  • %s%s%s\n", r.Attribute, ver, desc)
				}
				return nil

			case "install", "add", "i":
				if err := mgr.InstallConfigurationNixPackages(cmd.Context(), rest); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Installed system packages: %s\n", strings.Join(rest, ", "))
				return nil

			case "uninstall", "remove", "rm", "del":
				if err := mgr.UninstallConfigurationNixPackages(cmd.Context(), rest); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Uninstalled system packages: %s\n", strings.Join(rest, ", "))
				return nil

			case "list", "ls":
				installed, err := mgr.ListConfigurationNixPackages()
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "📦 Installed System Packages (%s):\n", mgr.ConfigurationNixPath())
				for _, pkg := range installed {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  • %s\n", pkg)
				}
				return nil

			default:
				// Treat `orbit packages <pkg1> <pkg2>` as shorthand for `orbit packages install <pkg1> <pkg2>`
				if err := mgr.InstallConfigurationNixPackages(cmd.Context(), args); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Installed system packages: %s\n", strings.Join(args, ", "))
				return nil
			}
		},
	}

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the orbit-helper version",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "orbit-helper v%s\n", Version)
		},
	}

	rootCmd.AddCommand(menuCmd, initCmd, reposCmd, flakeCmd, toolsCmd, gitCmd, sshKeyCmd, statusCmd, saveCmd, rebuildCmd, packagesCmd, versionCmd)
	return rootCmd
}
