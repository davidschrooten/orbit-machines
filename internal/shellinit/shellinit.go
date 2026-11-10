// Package shellinit bootstraps the Orbit Machine interactive shell environment
// with the Starship Jetpack prompt preset, direnv workspace whitelisting, and
// bash startup hooks with an interactive welcome banner.
package shellinit

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/davidschrooten/orbit-machines/internal/domain"
)

const (
	bashrcMarkerStart    = "# >>> orbit-machines shell init >>>"
	bashrcMarkerEnd      = "# <<< orbit-machines shell init <<<"
	altBashrcMarkerStart = "# >>> orbit-machines shellinit >>>"
	altBashrcMarkerEnd   = "# <<< orbit-machines shellinit <<<"
	jetpackPresetHeader  = "# orbit-preset: jetpack"
)

// JetpackStarshipTOML is the official Starship Jetpack preset configured for Orbit Machines.
const JetpackStarshipTOML = `# orbit-preset: jetpack
# version: 1.0.0
"$schema" = 'https://starship.rs/config-schema.json'

add_newline = true
continuation_prompt = "[▸▹ ](dimmed white)"

format = """($nix_shell$container$fill$git_metrics\n)$cmd_duration\
$hostname\
$localip\
$shlvl\
$shell\
$env_var\
$jobs\
$sudo\
$username\
$character"""

right_format = """
$singularity\
$kubernetes\
$directory\
$vcsh\
$fossil_branch\
$git_branch\
$git_commit\
$git_state\
$git_status\
$hg_branch\
$pijul_channel\
$docker_context\
$package\
$c\
$cpp\
$cmake\
$cobol\
$daml\
$dart\
$deno\
$dotnet\
$elixir\
$elm\
$erlang\
$fennel\
$fortran\
$golang\
$guix_shell\
$haskell\
$haxe\
$helm\
$java\
$julia\
$kotlin\
$gradle\
$lua\
$maven\
$nim\
$nodejs\
$bun\
$ocaml\
$opa\
$perl\
$php\
$pulumi\
$purescript\
$python\
$raku\
$rlang\
$red\
$ruby\
$rust\
$scala\
$solidity\
$swift\
$terraform\
$vlang\
$vagrant\
$xmake\
$zig\
$buf\
$conda\
$pixi\
$meson\
$spack\
$memory_usage\
$aws\
$gcloud\
$openstack\
$azure\
$crystal\
$custom\
$status\
$os\
$battery\
$time"""

[fill]
symbol = ' '

[character]
format = "$symbol "
success_symbol = "[◎](bold italic bright-yellow)"
error_symbol = "[○](italic purple)"
vimcmd_symbol = "[■](italic dimmed green)"
# not supported in zsh
vimcmd_replace_one_symbol = "◌"
vimcmd_replace_symbol = "□"
vimcmd_visual_symbol = "▼"

[env_var.VIMSHELL]
format = "[$env_value]($style)"
style = 'green italic'

[sudo]
format = "[$symbol]($style)"
style = "bold italic bright-purple"
symbol = "⋈┈"
disabled = false

[username]
style_user = "bright-yellow bold italic"
style_root = "purple bold italic"
format = "[⭘ $user]($style) "
disabled = false
show_always = false

[directory]
home_symbol = "⌂"
truncation_length = 2
truncation_symbol = "□ "
read_only = " ◈"
use_os_path_sep = true
style = "italic blue"
format = '[$path]($style)[$read_only]($read_only_style)'
repo_root_style = 'bold blue'
repo_root_format = '[$before_root_path]($before_repo_root_style)[$repo_root]($repo_root_style)[$path]($style)[$read_only]($read_only_style) [△](bold bright-blue)'

[cmd_duration]
format = "[◄ $duration ](italic white)"

[jobs]
format = "[$symbol$number]($style) "
style = "white"
symbol = "[▶](blue italic)"

[localip]
ssh_only = true
format = " ◯[$localipv4](bold magenta)"
disabled = false

[time]
disabled = false
format = "[ $time]($style)"
time_format = "%R"
utc_time_offset = "local"
style = "italic dimmed white"

[battery]
format = "[ $percentage $symbol]($style)"
full_symbol = "█"
charging_symbol = "[↑](italic bold green)"
discharging_symbol = "↓"
unknown_symbol = "░"
empty_symbol = "▃"

[[battery.display]]
threshold = 20
style = "italic bold red"

[[battery.display]]
threshold = 60
style = "italic dimmed bright-purple"

[[battery.display]]
threshold = 70
style = "italic dimmed yellow"

[git_branch]
format = " [$branch(:$remote_branch)]($style)"
symbol = "[△](bold italic bright-blue)"
style = "italic bright-blue"
truncation_symbol = "⋯"
truncation_length = 11
ignore_branches = ["main", "master"]
only_attached = true

[git_metrics]
format = '([▴$added]($added_style))([▿$deleted]($deleted_style))'
added_style = 'italic dimmed green'
deleted_style = 'italic dimmed red'
ignore_submodules = true
disabled = false

[git_status]
style = "bold italic bright-blue"
format = "([⎪$ahead_behind$staged$modified$untracked$renamed$deleted$conflicted$stashed⎥]($style))"
conflicted = "[◪◦](italic bright-magenta)"
ahead = "[▴│[${count}](bold white)│](italic green)"
behind = "[▿│[${count}](bold white)│](italic red)"
diverged = "[◇ ▴┤[${ahead_count}](regular white)│▿┤[${behind_count}](regular white)│](italic bright-magenta)"
untracked = "[◌◦](italic bright-yellow)"
stashed = "[◃◈](italic white)"
modified = "[●◦](italic yellow)"
staged = "[▪┤[$count](bold white)│](italic bright-cyan)"
renamed = "[◎◦](italic bright-blue)"
deleted = "[✕](italic red)"

[deno]
format = " [deno](italic) [∫ $version](green bold)"
version_format = "${raw}"

[lua]
format = " [lua](italic) [${symbol}${version}]($style)"
version_format = "${raw}"
symbol = "⨀ "
style = "bold bright-yellow"

[nodejs]
format = " [node](italic) [◫ ($version)](bold bright-green)"
version_format = "${raw}"
detect_files = ["package-lock.json", "yarn.lock"]
detect_folders = ["node_modules"]
detect_extensions = []

[bun]
format = " [bun](italic) [◫ ($version)](bold bright-green)"
version_format = "${raw}"

[python]
format = " [py](italic) [${symbol}${version}]($style)"
symbol = "[⌉](bold bright-blue)⌊ "
version_format = "${raw}"
style = "bold bright-yellow"

[ruby]
format = " [rb](italic) [${symbol}${version}]($style)"
symbol = "◆ "
version_format = "${raw}"
style = "bold red"

[rust]
format = " [rs](italic) [$symbol$version]($style)"
symbol = "⊃ "
version_format = "${raw}"
style = "bold red"

[package]
format = " [pkg](italic dimmed) [$symbol$version]($style)"
version_format = "${raw}"
symbol = "◨ "
style = "dimmed yellow italic bold"

[swift]
format = " [sw](italic) [${symbol}${version}]($style)"
symbol = "◁ "
style = "bold bright-red"
version_format = "${raw}"

[aws]
disabled = true
format = " [aws](italic) [$symbol $profile $region]($style)"
style = "bold blue"
symbol = "▲ "

[buf]
symbol = "■ "
format = " [buf](italic) [$symbol $version $buf_version]($style)"

[c]
symbol = "ℂ "
format = " [$symbol($version(-$name))]($style)"

[cpp]
symbol = "ℂ "
format = " [$symbol($version(-$name))]($style)"

[conda]
symbol = "◯ "
format = " conda [$symbol$environment]($style)"

[pixi]
symbol = "■ "
format = " pixi [$symbol$version ($environment )]($style)"

[dart]
symbol = "◁◅ "
format = " dart [$symbol($version )]($style)"

[docker_context]
symbol = "◧ "
format = " docker [$symbol$context]($style)"

[elixir]
symbol = "△ "
format = " exs [$symbol $version OTP $otp_version ]($style)"

[elm]
symbol = "◩ "
format = " elm [$symbol($version )]($style)"

[golang]
symbol = "∩ "
format = " go [$symbol($version )]($style)"

[haskell]
symbol = "❯λ "
format = " hs [$symbol($version )]($style)"

[java]
symbol = "∪ "
format = " java [${symbol}(${version} )]($style)"

[julia]
symbol = "◎ "
format = " jl [$symbol($version )]($style)"

[memory_usage]
symbol = "▪▫▪ "
format = " mem [${ram}( ${swap})]($style)"

[nim]
symbol = "▴▲▴ "
format = " nim [$symbol($version )]($style)"

[nix_shell]
style = 'bold italic dimmed blue'
symbol = '✶'
format = '[$symbol nix⎪$state⎪]($style) [$name](italic dimmed white)'
impure_msg = '[⌽](bold dimmed red)'
pure_msg = '[⌾](bold dimmed green)'
unknown_msg = '[◌](bold dimmed yellow)'

[spack]
symbol = "◇ "
format = " spack [$symbol$environment]($style)"
`

// TokyoNightStarshipTOML is a deprecated alias for JetpackStarshipTOML.
const TokyoNightStarshipTOML = JetpackStarshipTOML

// DefaultConfigurationNix is the default declarative Nix package environment
// written to ~/.config/orbit/configuration.nix and persisted across Pod restarts.
const DefaultConfigurationNix = `{ pkgs ? import <nixpkgs> {} }:

pkgs.buildEnv {
  name = "orbit-machine-env";
  paths = with pkgs; [
    nix
    bashInteractive
    coreutils
    findutils
    gnugrep
    gnused
    gawk
    gnutar
    gzip
    xz
    zstd
    which
    procps
    less
    ncurses
    cacert
    openssl
    pkg-config
    git
    gh
    nodejs
    openssh
    htop
    curl
    wget
    jq
    ripgrep
    starship
    direnv
    nix-direnv
  ];
}
`

// EnsureConfig initializes Orbit Machine shell configuration under homeDir using
// <homeDir>/workspace as the default workspace directory.
func EnsureConfig(homeDir string) error {
	trimmed := strings.TrimSpace(homeDir)
	if trimmed == "" {
		return fmt.Errorf("%w: homeDir is required", domain.ErrInvalidConfig)
	}
	return EnsureInitialized(trimmed, filepath.Join(trimmed, "workspace"))
}

// EnsureInitialized creates standard Orbit Machine configuration directories,
// links AI tool state and user dotfiles in homeDir to <workDir>/.orbit/home on the
// persistent workspace PVC, writes ~/.config/starship.toml and ~/.config/direnv/direnv.toml
// if missing, and ensures ~/.bashrc and ~/.bash_profile hook direnv, starship, env.sh,
// and the interactive welcome banner.
func EnsureInitialized(homeDir, workDir string) error {
	homeDir = strings.TrimSpace(homeDir)
	workDir = strings.TrimSpace(workDir)
	if homeDir == "" {
		return fmt.Errorf("%w: homeDir is required", domain.ErrInvalidConfig)
	}
	if workDir == "" {
		return fmt.Errorf("%w: workDir is required", domain.ErrInvalidConfig)
	}

	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", homeDir, err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", workDir, err)
	}

	ensurePersistentLocalDir(homeDir, workDir)
	ensurePersistentHome(homeDir, workDir)
	SyncPersistentHomeFiles(homeDir, workDir)

	dirs := []string{
		filepath.Join(homeDir, ".config"),
		filepath.Join(homeDir, ".config", "direnv"),
		filepath.Join(homeDir, ".config", "nix"),
		filepath.Join(homeDir, ".config", "orbit"),
		workDir,
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating directory %s: %w", dir, err)
		}
	}

	nixConfPath := filepath.Join(homeDir, ".config", "nix", "nix.conf")
	if _, err := os.Stat(nixConfPath); os.IsNotExist(err) {
		_ = os.WriteFile(nixConfPath, []byte("experimental-features = nix-command flakes\nsandbox = false\nbuild-users-group =\nfilter-syscalls = false\n"), 0o644)
	}

	configNixPath := filepath.Join(homeDir, ".config", "orbit", "configuration.nix")
	if existingCfgNix, err := os.ReadFile(configNixPath); os.IsNotExist(err) {
		if err := os.WriteFile(configNixPath, []byte(DefaultConfigurationNix), 0o644); err != nil {
			return fmt.Errorf("writing configuration.nix: %w", err)
		}
	} else if err == nil {
		content := string(existingCfgNix)
		if !strings.Contains(content, "nodejs") {
			if idx := strings.LastIndex(content, "];"); idx >= 0 {
				upgraded := content[:idx] + "    nodejs\n  " + strings.TrimLeft(content[idx:], " \t")
				_ = os.WriteFile(configNixPath, []byte(upgraded), 0o644)
			}
		}
	}

	nixProfileLink := filepath.Join(homeDir, ".nix-profile")
	if _, err := os.Lstat(nixProfileLink); os.IsNotExist(err) {
		if _, statErr := os.Stat("/nix/var/nix/profiles/per-user/orbit/profile"); statErr == nil {
			_ = os.Symlink("/nix/var/nix/profiles/per-user/orbit/profile", nixProfileLink)
		}
	}

	starshipPath := filepath.Join(homeDir, ".config", "starship.toml")
	existingStarship, starshipErr := os.ReadFile(starshipPath)
	if starshipErr != nil || !strings.Contains(string(existingStarship), jetpackPresetHeader) {
		if err := os.WriteFile(starshipPath, []byte(JetpackStarshipTOML), 0o644); err != nil {
			return fmt.Errorf("writing starship.toml: %w", err)
		}
	}

	direnvPath := filepath.Join(homeDir, ".config", "direnv", "direnv.toml")
	if _, err := os.Stat(direnvPath); os.IsNotExist(err) {
		direnvTOML := buildDirenvTOML(workDir)
		if err := os.WriteFile(direnvPath, []byte(direnvTOML), 0o644); err != nil {
			return fmt.Errorf("writing direnv.toml: %w", err)
		}
	}

	bashrcPath := filepath.Join(homeDir, ".bashrc")
	existingBashrc, _ := os.ReadFile(bashrcPath)
	block := buildBashrcBlock(homeDir)
	updatedBashrc := upsertBashrcBlock(string(existingBashrc), block)
	if updatedBashrc != string(existingBashrc) {
		if err := os.WriteFile(bashrcPath, []byte(updatedBashrc), 0o644); err != nil {
			return fmt.Errorf("writing .bashrc: %w", err)
		}
	}

	profilePath := filepath.Join(homeDir, ".bash_profile")
	existingProfile, _ := os.ReadFile(profilePath)
	if !strings.Contains(string(existingProfile), ".bashrc") {
		profileContent := fmt.Sprintf("%s[ -f %q ] && . %q\n", string(existingProfile), bashrcPath, bashrcPath)
		if err := os.WriteFile(profilePath, []byte(profileContent), 0o644); err != nil {
			return fmt.Errorf("writing .bash_profile: %w", err)
		}
	}

	SyncPersistentHomeFiles(homeDir, workDir)
	return nil
}

type persistentDirSpec struct {
	name string
	perm os.FileMode
}

var persistentHomeDirs = []persistentDirSpec{
	{name: ".config", perm: 0o755},
	{name: ".claude", perm: 0o700},
	{name: ".gemini", perm: 0o700},
	{name: ".antigravity", perm: 0o700},
	{name: ".antigravity-cli", perm: 0o700},
	{name: ".opencode", perm: 0o700},
	{name: ".codex", perm: 0o700},
	{name: ".cursor", perm: 0o700},
	{name: ".aider", perm: 0o700},
	{name: ".ssh", perm: 0o700},
	{name: ".cache", perm: 0o755},
	{name: ".npm", perm: 0o755},
	{name: ".pki", perm: 0o700},
	{name: ".gnupg", perm: 0o700},
}

var persistentHomeFiles = []string{
	".claude.json",
	".claude.json.backup",
	".gitconfig",
	".git-credentials",
	".aider.conf.yml",
	".bash_history",
	".npmrc",
	".bashrc",
	".bash_profile",
	".profile",
}

func isKnownHomeFile(name string) bool {
	for _, f := range persistentHomeFiles {
		if f == name {
			return true
		}
	}
	return false
}

func shouldIgnoreHomeEntry(name string) bool {
	if name == "" || name == "." || name == ".." {
		return true
	}
	switch name {
	case "workspace", ".orbit", ".nix-profile", ".nix-defexpr", ".local":
		return true
	}
	if strings.Contains(name, ".tmp") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".orbit-sync-tmp") {
		return true
	}
	return false
}

func canLinkHomePath(homeDir, workDir, homePath string) bool {
	cleanHome := filepath.Clean(strings.TrimSpace(homeDir))
	cleanWork := filepath.Clean(strings.TrimSpace(workDir))
	cleanHomePath := filepath.Clean(strings.TrimSpace(homePath))
	if cleanHome == "" || cleanWork == "" || cleanHome == cleanWork {
		return false
	}
	if cleanWork == cleanHomePath || strings.HasPrefix(cleanWork, cleanHomePath+string(os.PathSeparator)) {
		return false
	}
	return true
}

func ensurePersistentHome(homeDir, workDir string) {
	cleanHome := filepath.Clean(strings.TrimSpace(homeDir))
	cleanWork := filepath.Clean(strings.TrimSpace(workDir))
	if cleanHome == "" || cleanWork == "" || cleanHome == cleanWork {
		return
	}
	pvcHome := filepath.Join(workDir, ".orbit", "home")
	_ = os.MkdirAll(pvcHome, 0o755)

	// Seed <workDir>/.orbit/home/.ssh from legacy <workDir>/.orbit/ssh if present.
	legacySSHKey := filepath.Join(workDir, ".orbit", "ssh", "id_ed25519")
	pvcSSHDir := filepath.Join(pvcHome, ".ssh")
	pvcSSHKey := filepath.Join(pvcSSHDir, "id_ed25519")
	if _, err := os.Stat(legacySSHKey); err == nil {
		if _, dstErr := os.Stat(pvcSSHKey); os.IsNotExist(dstErr) {
			_ = os.MkdirAll(pvcSSHDir, 0o700)
			_ = os.Chmod(pvcSSHDir, 0o700)
			if privData, rErr := os.ReadFile(legacySSHKey); rErr == nil {
				_ = os.WriteFile(pvcSSHKey, privData, 0o600)
			}
			if pubData, rErr := os.ReadFile(legacySSHKey + ".pub"); rErr == nil {
				_ = os.WriteFile(pvcSSHKey+".pub", pubData, 0o644)
			}
		}
	}

	for _, spec := range persistentHomeDirs {
		homePath := filepath.Join(homeDir, spec.name)
		targetPath := filepath.Join(pvcHome, spec.name)
		if !canLinkHomePath(homeDir, workDir, homePath) {
			continue
		}
		ensurePersistentDirLink(homePath, targetPath, spec.perm)
	}

	// Link any additional directories or files already persisted in <workDir>/.orbit/home from prior sessions.
	if entries, err := os.ReadDir(pvcHome); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if shouldIgnoreHomeEntry(name) {
				continue
			}
			homePath := filepath.Join(homeDir, name)
			targetPath := filepath.Join(pvcHome, name)
			if !canLinkHomePath(homeDir, workDir, homePath) {
				continue
			}
			if entry.IsDir() && !isKnownHomeFile(name) {
				ensurePersistentDirLink(homePath, targetPath, 0o700)
			} else if !entry.IsDir() {
				if _, lerr := os.Lstat(homePath); os.IsNotExist(lerr) {
					_ = os.Symlink(targetPath, homePath)
				}
			}
		}
	}
}

func ensurePersistentDirLink(homePath, targetPath string, perm os.FileMode) {
	if perm == 0 {
		perm = 0o700
	}
	_ = os.MkdirAll(targetPath, perm)
	if perm == 0o700 {
		_ = os.Chmod(targetPath, 0o700)
	}

	fi, err := os.Lstat(homePath)
	if os.IsNotExist(err) {
		_ = os.Symlink(targetPath, homePath)
		return
	}
	if err != nil {
		return
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		dest, lerr := os.Readlink(homePath)
		if lerr != nil || filepath.Clean(dest) != filepath.Clean(targetPath) {
			_ = os.Remove(homePath)
			_ = os.Symlink(targetPath, homePath)
		}
		return
	}
	if fi.IsDir() {
		mergeDirWithoutOverwrite(homePath, targetPath)
		_ = os.RemoveAll(homePath)
		_ = os.Symlink(targetPath, homePath)
	}
}

func mergeDirWithoutOverwrite(srcDir, dstDir string) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		srcPath := filepath.Join(srcDir, entry.Name())
		dstPath := filepath.Join(dstDir, entry.Name())
		info, lerr := os.Lstat(srcPath)
		if lerr != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if _, dstErr := os.Lstat(dstPath); os.IsNotExist(dstErr) {
				if linkTarget, rerr := os.Readlink(srcPath); rerr == nil {
					_ = os.Symlink(linkTarget, dstPath)
				}
			}
			continue
		}
		if info.IsDir() {
			perm := info.Mode().Perm()
			if perm == 0 {
				perm = 0o755
			}
			_ = os.MkdirAll(dstPath, perm)
			mergeDirWithoutOverwrite(srcPath, dstPath)
			continue
		}
		if info.Mode().IsRegular() {
			if _, dstErr := os.Lstat(dstPath); os.IsNotExist(dstErr) {
				if data, rerr := os.ReadFile(srcPath); rerr == nil {
					perm := info.Mode().Perm()
					if perm == 0 {
						perm = 0o600
					}
					_ = os.WriteFile(dstPath, data, perm)
				}
			}
		}
	}
}

// SyncPersistentHomeFiles ensures top-level dotfiles and dot-directories in homeDir
// (such as ~/.claude.json, ~/.gitconfig, ~/.bash_history, ~/.aider.conf.yml, and any
// atomically renamed config files) are synced to <workDir>/.orbit/home and linked back into homeDir.
func SyncPersistentHomeFiles(homeDir, workDir string) {
	homeDir = strings.TrimSpace(homeDir)
	workDir = strings.TrimSpace(workDir)
	if homeDir == "" || workDir == "" {
		return
	}
	if filepath.Clean(homeDir) == filepath.Clean(workDir) {
		return
	}
	if hInfo, err := os.Stat(homeDir); err != nil || !hInfo.IsDir() {
		return
	}

	pvcHome := filepath.Join(workDir, ".orbit", "home")
	if err := os.MkdirAll(pvcHome, 0o755); err != nil {
		return
	}

	// Pre-seed .claude.json and .bash_history in pvcHome when neither pvcHome nor homeDir has them yet,
	// so realpath() on their symlinks always resolves to an existing regular file on disk.
	pvcClaudeJSON := filepath.Join(pvcHome, ".claude.json")
	homeClaudeJSON := filepath.Join(homeDir, ".claude.json")
	if _, err := os.Stat(pvcClaudeJSON); os.IsNotExist(err) {
		if _, hErr := os.Lstat(homeClaudeJSON); os.IsNotExist(hErr) {
			_ = os.WriteFile(pvcClaudeJSON, []byte("{\"hasCompletedOnboarding\": true}\n"), 0o600)
		}
	}
	pvcBashHistory := filepath.Join(pvcHome, ".bash_history")
	homeBashHistory := filepath.Join(homeDir, ".bash_history")
	if _, err := os.Stat(pvcBashHistory); os.IsNotExist(err) {
		if _, hErr := os.Lstat(homeBashHistory); os.IsNotExist(hErr) {
			_ = os.WriteFile(pvcBashHistory, []byte(""), 0o600)
		}
	}

	candidateSet := make(map[string]bool, len(persistentHomeFiles)+16)
	var candidates []string
	addCandidate := func(name string) {
		if shouldIgnoreHomeEntry(name) || !strings.HasPrefix(name, ".") {
			return
		}
		if !candidateSet[name] {
			candidateSet[name] = true
			candidates = append(candidates, name)
		}
	}

	for _, name := range persistentHomeFiles {
		addCandidate(name)
	}
	if entries, err := os.ReadDir(homeDir); err == nil {
		for _, entry := range entries {
			addCandidate(entry.Name())
		}
	}
	if entries, err := os.ReadDir(pvcHome); err == nil {
		for _, entry := range entries {
			addCandidate(entry.Name())
		}
	}

	for _, name := range candidates {
		homeFile := filepath.Join(homeDir, name)
		pvcFile := filepath.Join(pvcHome, name)
		if !canLinkHomePath(homeDir, workDir, homeFile) {
			continue
		}

		fi, err := os.Lstat(homeFile)
		if os.IsNotExist(err) {
			if pInfo, pErr := os.Lstat(pvcFile); pErr == nil {
				if pInfo.IsDir() && !isKnownHomeFile(name) {
					ensurePersistentDirLink(homeFile, pvcFile, 0o700)
				} else if !pInfo.IsDir() {
					_ = os.Symlink(pvcFile, homeFile)
				}
			}
			continue
		}
		if err != nil {
			continue
		}

		if fi.Mode()&os.ModeSymlink != 0 {
			dest, lerr := os.Readlink(homeFile)
			if lerr != nil || filepath.Clean(dest) != filepath.Clean(pvcFile) {
				if _, pErr := os.Lstat(pvcFile); pErr == nil {
					_ = os.Remove(homeFile)
					_ = os.Symlink(pvcFile, homeFile)
				}
			}
			continue
		}

		if fi.IsDir() {
			if !isKnownHomeFile(name) {
				ensurePersistentDirLink(homeFile, pvcFile, fi.Mode().Perm()|0o700)
			}
			continue
		}

		if fi.Mode().IsRegular() {
			data, rErr := os.ReadFile(homeFile)
			if rErr == nil {
				_, pErr := os.Stat(pvcFile)
				if os.IsNotExist(pErr) || len(bytes.TrimSpace(data)) > 0 {
					perm := fi.Mode().Perm()
					if perm == 0 {
						perm = 0o600
					}
					tmpFile := pvcFile + ".orbit-sync-tmp"
					if wErr := os.WriteFile(tmpFile, data, perm); wErr == nil {
						_ = os.Chmod(tmpFile, perm)
						_ = os.Rename(tmpFile, pvcFile)
					}
				}
			}
			if _, pErr := os.Stat(pvcFile); pErr == nil {
				_ = os.Remove(homeFile)
				_ = os.Symlink(pvcFile, homeFile)
			}
		}
	}
}

func upsertBashrcBlock(existing, block string) string {
	for _, markers := range [][2]string{
		{bashrcMarkerStart, bashrcMarkerEnd},
		{altBashrcMarkerStart, altBashrcMarkerEnd},
	} {
		startIdx := strings.Index(existing, markers[0])
		endIdx := strings.Index(existing, markers[1])
		if startIdx >= 0 && endIdx > startIdx {
			afterEnd := endIdx + len(markers[1])
			if afterEnd < len(existing) && existing[afterEnd] == '\n' {
				afterEnd++
			}
			return existing[:startIdx] + block + existing[afterEnd:]
		}
	}

	var updated strings.Builder
	if len(existing) > 0 {
		updated.WriteString(existing)
		if !strings.HasSuffix(existing, "\n") {
			updated.WriteByte('\n')
		}
		updated.WriteByte('\n')
	}
	updated.WriteString(block)
	return updated.String()
}

func ensurePersistentLocalDir(homeDir, workDir string) {
	wsLocal := filepath.Join(workDir, ".orbit", "local")
	for _, sub := range []string{"bin", "lib", "share", "state"} {
		_ = os.MkdirAll(filepath.Join(wsLocal, sub), 0o755)
	}
	homeLocal := filepath.Join(homeDir, ".local")
	if canLinkHomePath(homeDir, workDir, homeLocal) {
		ensurePersistentDirLink(homeLocal, wsLocal, 0o755)
		return
	}
	_ = os.MkdirAll(filepath.Join(homeLocal, "bin"), 0o755)
}

func buildDirenvTOML(workDir string) string {
	const defaultWorkspace = "/home/orbit/workspace"
	return fmt.Sprintf("[whitelist]\nprefix = [ %q, %q ]\n", defaultWorkspace, filepath.Clean(workDir))
}

func buildBashrcBlock(homeDir string) string {
	starshipCfg := filepath.Join(homeDir, ".config", "starship.toml")
	envSh := filepath.Join(homeDir, ".config", "orbit", "env.sh")
	sshKeyPath := filepath.Join(homeDir, ".ssh", "id_ed25519")
	knownHostsPath := filepath.Join(homeDir, ".ssh", "known_hosts")
	sshCmd := fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s", sshKeyPath, knownHostsPath)
	return fmt.Sprintf(`%s
export XDG_CONFIG_HOME="%s/.config"
export XDG_DATA_HOME="%s/.local/share"
export XDG_STATE_HOME="%s/.local/state"
export XDG_CACHE_HOME="%s/.cache"
export HISTFILE="%s/.bash_history"
export HISTSIZE=10000
export HISTFILESIZE=20000
shopt -s histappend 2>/dev/null || true
export NPM_CONFIG_PREFIX="%s/.local"
export PATH="%s/.local/bin:%s/.nix-profile/bin:/nix/var/nix/profiles/per-user/orbit/profile/bin:/nix/var/nix/profiles/orbit/bin:/usr/local/bin:$PATH"
export STARSHIP_CONFIG=%q
export COLORTERM="truecolor"
export TERM="${TERM:-xterm-256color}"
export GIT_TERMINAL_PROMPT=0
export GIT_SSH_COMMAND=%q
[ -f %q ] && . %q
command -v direnv >/dev/null 2>&1 && eval "$(direnv hook bash)"
command -v starship >/dev/null 2>&1 && eval "$(starship init bash)"
if [[ $- == *i* ]]; then
  printf '\n\033[1;36m🪐 Welcome to Orbit Machine\033[0m\n'
  printf '\033[2m──────────────────────────────────────────────────────────────────────\033[0m\n'
  printf '  \033[1;33morbit\033[0m                    Launch interactive Orbit workspace & tool assistant\n'
  printf '  \033[1;33morbit repos\033[0m              Fetch & clone workspace repositories\n'
  printf '  \033[1;33morbit flake init [stack]\033[0m Initialize Nix flake + direnv .envrc\n'
  printf '  \033[1;33morbit packages\033[0m           Search nixpkgs, install, or uninstall system packages\n'
  printf '  \033[1;33morbit rebuild\033[0m            Apply ~/.config/orbit/configuration.nix packages\n'
  printf '  \033[1;33morbit tools [tool...]\033[0m    Install & configure AI coding tools (claude, agy, opencode, codex, gemini)\n'
  printf '  \033[1;33morbit git\033[0m                Configure Git author identity & defaults\n'
  printf '  \033[1;33morbit ssh-key\033[0m            Generate or import your Git SSH key\n'
  printf '  \033[1;33morbit status\033[0m             Show configured tools, Git, SSH key, and workspace status\n'
  printf '\033[2m──────────────────────────────────────────────────────────────────────\033[0m\n\n'
fi
%s
`, bashrcMarkerStart, homeDir, homeDir, homeDir, homeDir, homeDir, homeDir, homeDir, homeDir, starshipCfg, sshCmd, envSh, envSh, bashrcMarkerEnd)
}
