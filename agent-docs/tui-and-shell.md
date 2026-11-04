# TUI and shell (`orbit-machined`)

`orbit-machined` hosts a pure-Go tiling window manager built with **Bubble Tea**
(`github.com/charmbracelet/bubbletea`), **Lip Gloss** (`github.com/charmbracelet/lipgloss`),
**Bubbles** (`github.com/charmbracelet/bubbles`), **`creack/pty`**, and **`charmbracelet/x/vt`**.

## Why an in-memory VT100 compositor is required

The tools engineers run inside `Orbit Machines` — **Claude Code** (`claude`),
**Antigravity-CLI** (`agy`), **OpenCode** (`opencode`), and interactive shells — are themselves
full-screen interactive TUIs that switch to the alternate screen buffer, move the cursor with ANSI
escape sequences, and capture mouse and keyboard input.

To tile multiple full-screen TUIs inside a single browser terminal (`xterm.js`):
1. Each pane spawns its child process attached to a pseudo-terminal (`creack/pty`) with dimensions
   matching that pane's inner width and height (`TIOCSWINSZ`).
2. A background goroutine per pane reads raw ANSI bytes from the PTY master fd and feeds them into
   an in-memory virtual terminal emulator (`charmbracelet/x/vt`).
3. At a capped **60fps tick** (only when at least one pane or overlay is dirty), the Bubble Tea
   root model renders each visible pane's virtual screen buffer inside Lip Gloss borders, overlays
   any active modal (Repo Switcher, Flake Init Wizard, or AI Configurator), and emits a
   `zstd`-compressed frame over the WebSocket.
4. When a browser disconnects and reconnects (or opens from a second tab), `orbit-machined` sends
   a full screen snapshot (`0x10` frame) immediately — child processes never notice the disconnect.

## Tiling window manager UX & browser-safe keybindings

Because `Orbit Machines` runs inside a web browser, standard browser shortcuts (`Ctrl+W`, `Ctrl+T`,
`Ctrl+N`, `Alt+Tab`) are intercepted by the browser. The TUI therefore combines a **browser-safe
leader key chord** with **first-class mouse support**:

### Leader chords (default leader: `Ctrl+Space`, customizable via AI Configurator)

| Chord | Action |
|---|---|
| `Ctrl+Space` then `c` | Open Tool Launcher in a new pane (`Claude Code`, `Antigravity-CLI`, `OpenCode`, `Shell`) |
| `Ctrl+Space` then `v` / `s` | Split current pane vertically / horizontally |
| `Ctrl+Space` then `h`/`j`/`k`/`l` | Move focus left / down / up / right |
| `Ctrl+Space` then `z` | Toggle zoom (maximize focused pane within the TUI) |
| `Ctrl+Space` then `x` | Close focused pane (prompts if process is active) |
| `Ctrl+Space` then `r` | Open **Repository Switcher** (switch or open another Orbit repo tab) |
| `Ctrl+Space` then `i` | Open **Orbit Issue Picker** (switch branch / inject issue context) |
| `Ctrl+Space` then `a` | Open **AI Configurator** ("Configure through an AI") |
| `Ctrl+Space` then `1`..`9` | Switch workspace repository tab `1`..`9` |

### Mouse interactions
- **Click any pane** to focus it (forwards click into the child PTY if already focused).
- **Drag any split border** to resize panes dynamically (triggers debounced `TIOCSWINSZ` on both
  child PTYs).
- **Click top bar tabs / buttons** to switch repositories, spawn a tool (`+ Claude`, `+ Agy`,
  `+ OpenCode`, `+ Shell`), or open the AI Configurator (`✦ Configure`).

## Repository tabs & the Flake Init Wizard

A machine belongs to the user's **Orbit workspace**, not just a single repository. Users can open
any repository registered in their Orbit workspace as a tab in the TUI.

When the user selects a repository:
1. `orbit-machined` clones the repository (if not yet cloned in `/workspace/<repo-name>`) using the
   short-lived Git credential helper token minted by Orbit.
2. `orbit-machined` checks for `/workspace/<repo-name>/flake.nix`:
   - **If `flake.nix` exists:** `orbit-machined` evaluates `nix print-dev-env --json` (pulling
     pre-built store paths from the cluster's shared MinIO Nix binary cache) and launches all tool
     panes (`claude`, `agy`, `opencode`, `zsh`/`bash`) inside that hermetic flake environment.
   - **If `flake.nix` does NOT exist:** The TUI opens the **Flake Init Wizard** modal before
     spawning panes for that repository.

### The Flake Init Wizard (`internal/tui/flakewizard`)

Rather than blocking repositories without a `flake.nix` in the Orbit UI, the TUI helps the engineer
create one in seconds:
1. **Auto-detection:** Inspects the repo root (`Gemfile`, `package.json`, `go.mod`, `Cargo.toml`,
   `pyproject.toml`) and pre-selects recommended Nixpkgs packages and language toolchains.
2. **Interactive Nixpkgs Search:** Provides a live fuzzy search input backed by `nix search nixpkgs --json`
   (plus common services like `postgresql_16`, `redis`, `nodejs_22`, `go`, `ruby_4_0`, `rustup`) so
   the user can toggle packages and environment variables with `Space`.
3. **AI-Assisted Generation:** Includes an `"Ask AI to generate flake.nix"` action that inspects the
   repository's README and manifest files and writes a tailored `flake.nix`.
4. **Preview & Activate:** Shows the generated `flake.nix` diff, writes `flake.nix` + `flake.lock`,
   enters `nix develop`, and pushes the built closure to the shared MinIO Nix binary cache so no
   teammate ever has to wait for it to build again.

## Built-in AI Configurator ("Configure through an AI")

Pressing `Ctrl+Space` → `a` (or clicking `✦ Configure` in the status bar) opens the **AI
Configurator** drawer (`internal/tui/aiconfig`).

Users describe what they want in plain language — e.g.:
- *"Change my leader key to Ctrl+A, use a Tokyo Night border theme, and always open Claude Code on
  the left taking 65% width and a shell on the right."*
- *"Add `jq`, `ripgrep`, and `postgresql_16` to this repo's `flake.nix` and reload my panes."*
- *"Add a git alias `co` for checkout and enable vim mode in my shell."*

### Structured tool-calling loop

The AI Configurator uses structured tool calls against `~/.config/orbit-machine/config.json` and the
active repository:

| Tool | Effect |
|---|---|
| `GetMachineConfig` | Returns current TUI layout presets, keybindings, theme, and default tool flags |
| `UpdateTUIConfig` | Validates and applies new keybindings, theme tokens, status bar position, or default split layout, then triggers a **live hot-reload** of the Bubble Tea compositor |
| `SetShellProfile` | Updates user shell aliases, env vars, or dotfiles under `~/.config/orbit-machine/shell/` |
| `EditRepoFlake` | Adds/removes packages or shellHooks in `/workspace/<repo>/flake.nix`, runs `nix print-dev-env` to verify it builds, and offers to restart open panes with the updated environment |
| `SyncProfileNow` | Immediately encrypts and uploads the updated user profile to `s3://orbit-user-profiles` |

Every successful config change is immediately persisted to the user's S3 bucket so their next
ephemeral machine boots with the exact same layout, keybindings, and shell environment.
