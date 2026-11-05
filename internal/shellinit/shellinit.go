// Package shellinit bootstraps the Orbit Machine interactive shell environment
// with the Tokyo Night Starship prompt preset (Orbit 🪐 logo), direnv workspace
// whitelisting, and bash startup hooks.
package shellinit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/davidschrooten/orbit-machines/internal/domain"
)

const (
	bashrcMarkerStart = "# >>> orbit-machines shell init >>>"
	bashrcMarkerEnd   = "# <<< orbit-machines shell init <<<"
)

// TokyoNightStarshipTOML is the Tokyo Night preset for Starship with the Orbit logo (🪐).
const TokyoNightStarshipTOML = `format = """
[░▒▓](#a3aed2)\
[ 🪐 ](bg:#a3aed2 fg:#090c0c)\
[](bg:#769ff0 fg:#a3aed2)\
$directory\
[](fg:#769ff0 bg:#394260)\
$git_branch\
$git_status\
[](fg:#394260 bg:#212736)\
$nodejs\
$rust\
$golang\
$php\
$ruby\
$python\
$nix_shell\
[](fg:#212736 bg:#1d2230)\
$time\
[ ](fg:#1d2230)\
\n$character"""

[directory]
style = "fg:#e3e5e5 bg:#769ff0"
format = "[ $path ]($style)"
truncation_length = 3
truncation_symbol = "…/"

[directory.substitutions]
"Documents" = "󰈙 "
"Downloads" = " "
"Music" = " "
"Pictures" = " "

[git_branch]
symbol = ""
style = "bg:#394260"
format = '[[ $symbol $branch ](fg:#769ff0 bg:#394260)]($style)'

[git_status]
style = "bg:#394260"
format = '[[($all_status$ahead_behind )](fg:#769ff0 bg:#394260)]($style)'

[nodejs]
symbol = ""
style = "bg:#212736"
format = '[[ $symbol ($version) ](fg:#769ff0 bg:#212736)]($style)'

[rust]
symbol = ""
style = "bg:#212736"
format = '[[ $symbol ($version) ](fg:#769ff0 bg:#212736)]($style)'

[golang]
symbol = ""
style = "bg:#212736"
format = '[[ $symbol ($version) ](fg:#769ff0 bg:#212736)]($style)'

[php]
symbol = ""
style = "bg:#212736"
format = '[[ $symbol ($version) ](fg:#769ff0 bg:#212736)]($style)'

[ruby]
symbol = ""
style = "bg:#212736"
format = '[[ $symbol ($version) ](fg:#769ff0 bg:#212736)]($style)'

[python]
symbol = ""
style = "bg:#212736"
format = '[[ $symbol ($version) ](fg:#769ff0 bg:#212736)]($style)'

[nix_shell]
symbol = ""
style = "bg:#212736"
format = '[[ $symbol ($state) ](fg:#769ff0 bg:#212736)]($style)'

[time]
disabled = false
time_format = "%R"
style = "bg:#1d2230"
format = '[[  $time ](fg:#a0a9cb bg:#1d2230)]($style)'
`

// EnsureInitialized creates standard Orbit Machine configuration directories,
// writes ~/.config/starship.toml and ~/.config/direnv/direnv.toml if missing,
// and ensures ~/.bashrc and ~/.bash_profile hook direnv, starship, and env.sh.
func EnsureInitialized(homeDir, workDir string) error {
	homeDir = strings.TrimSpace(homeDir)
	workDir = strings.TrimSpace(workDir)
	if homeDir == "" {
		return fmt.Errorf("%w: homeDir is required", domain.ErrInvalidConfig)
	}
	if workDir == "" {
		return fmt.Errorf("%w: workDir is required", domain.ErrInvalidConfig)
	}

	dirs := []string{
		filepath.Join(homeDir, ".config"),
		filepath.Join(homeDir, ".config", "direnv"),
		filepath.Join(homeDir, ".config", "orbit"),
		workDir,
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating directory %s: %w", dir, err)
		}
	}

	starshipPath := filepath.Join(homeDir, ".config", "starship.toml")
	if _, err := os.Stat(starshipPath); os.IsNotExist(err) {
		if err := os.WriteFile(starshipPath, []byte(TokyoNightStarshipTOML), 0o644); err != nil {
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
	if !strings.Contains(string(existingBashrc), bashrcMarkerStart) {
		block := buildBashrcBlock(homeDir)
		var updated strings.Builder
		if len(existingBashrc) > 0 {
			updated.Write(existingBashrc)
			if !strings.HasSuffix(string(existingBashrc), "\n") {
				updated.WriteByte('\n')
			}
			updated.WriteByte('\n')
		}
		updated.WriteString(block)
		if err := os.WriteFile(bashrcPath, []byte(updated.String()), 0o644); err != nil {
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

	return nil
}

func buildDirenvTOML(workDir string) string {
	const defaultWorkspace = "/home/orbit/workspace"
	if filepath.Clean(workDir) == defaultWorkspace {
		return fmt.Sprintf("[whitelist]\nprefix = [ %q ]\n", defaultWorkspace)
	}
	return fmt.Sprintf("[whitelist]\nprefix = [ %q, %q ]\n", defaultWorkspace, filepath.Clean(workDir))
}

func buildBashrcBlock(homeDir string) string {
	starshipCfg := filepath.Join(homeDir, ".config", "starship.toml")
	envSh := filepath.Join(homeDir, ".config", "orbit", "env.sh")
	return fmt.Sprintf(`%s
export STARSHIP_CONFIG=%q
export COLORTERM="truecolor"
export TERM="${TERM:-xterm-256color}"
[ -f %q ] && . %q
command -v direnv >/dev/null 2>&1 && eval "$(direnv hook bash)"
command -v starship >/dev/null 2>&1 && eval "$(starship init bash)"
%s
`, bashrcMarkerStart, starshipCfg, envSh, envSh, bashrcMarkerEnd)
}
