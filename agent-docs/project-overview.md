# Project overview

## What Orbit Machines is

`Orbit Machines` is an optional, self-hosted cloud development environment system for
[Orbit](/home/david/Dev/ruby/orbit) workspaces. It provisions ephemeral, Nix-powered machines on a
workspace's Kubernetes cluster and exposes them inside Orbit through a **GCP Cloud Shell-style
collapsible bottom console** (expandable to a full-screen overlay).

Inside each machine runs our custom Go tiling window manager built with **Bubble Tea** and
**Lip Gloss**, where engineers can:
- Open and switch between any of their Orbit workspace's repositories in a Nix flake (`flake.nix`)
  devshell (or initialize a new `flake.nix` via a built-in wizard if the repo does not have one).
- Spawn and tile **Claude Code**, **Antigravity-CLI**, **OpenCode**, and interactive shells with
  Orbit's MCP server and Git credentials automatically wired.
- Customize their TUI layout, keybindings, shell environment, and `flake.nix` packages through a
  **built-in AI Configurator**, with all user settings and CLI auth persisted to an encrypted S3
  profile.

## Product thesis

1. **Zero local setup, zero re-login.** An engineer clicks the console bar at the bottom of Orbit
   and lands inside a warm tiling TUI where their AI tools (`claude`, `agy`, `opencode`), personal
   keybindings, Orbit MCP connection, and Git credentials already work.
2. **Terminal speed in the browser.** Rails is never in the terminal byte path. The browser's
   `xterm.js` connects directly to the cluster's Go dispatcher (`orbit-dispatcher`), while the
   in-pod Go daemon (`orbit-machined`) composites panes in memory and streams 60fps dirty-screen
   diffs.
3. **Stateless compute, shared Nix intelligence.** Machine Pods are ephemeral (`emptyDir` for
   `/nix/store`), but every built Nix derivation is pushed to the cluster's shared MinIO Nix
   binary cache. Once one machine builds a dependency, every machine in the workspace gets it in
   seconds.
4. **Safe ephemerality.** Machines shut down gracefully after a configurable inactivity window
   (e.g. 30 or 60 minutes). Before Pod termination, `orbit-machined` syncs user settings and tool
   state to S3 and saves any uncommitted git work as a WIP bundle in S3, offering to restore it on
   next launch.
5. **Two-tier governance.** A Root Admin (`super_admin`) explicitly enables `Orbit Machines` for a
   workspace. The Workspace Admin installs the Helm chart on their cluster, defines resource
   constraints (CPU, memory, disk, security), and grants access per Member or per Orbit Team.

## End-to-end journeys

### 1. Root Admin & Workspace Admin setup
1. **Root Admin (`super_admin`)** enables `Orbit Machines` on a workspace from the Orbit Admin
   Console (`/admin/workspaces/:id`).
2. **Workspace Admin** opens `/:workspace/settings/machines`, generates a cluster registration
   token, and runs `helm install orbit-machines` on their Kubernetes cluster with their resource
   constraints, idle timeout, and ingress hostname.
3. `orbit-dispatcher` boots in Kubernetes, calls Orbit's REST endpoint to verify the token and
   register its WebSocket gateway URL and resource tiers.
4. **Workspace Admin** toggles `Orbit Machines` access for individual members or entire Orbit
   `Team`s.

### 2. Engineer daily workflow
1. An authorized member sees the collapsed **Machine Console** bar at the bottom of Orbit (like
   GCP Cloud Shell) or clicks **Open in Machine** on an Issue or Repository.
2. Clicking the bar expands the bottom drawer (`data-turbo-permanent`, staying alive across all
   Turbo navigation in Orbit) or maximizes it to full screen.
3. Orbit requests a machine session from `orbit-dispatcher` via REST and returns a signed ticket.
   The browser connects directly to `wss://<dispatcher-host>/ws/terminal` with that ticket.
4. Inside the TUI:
   - The user picks an Orbit repository (or starts in the issue's repository/branch).
   - If the repository has a `flake.nix`, `orbit-machined` enters `nix develop` (hydrating from
     the cluster's MinIO Nix binary cache).
   - If the repository has **no `flake.nix`**, the TUI displays the **Flake Init Wizard**, allowing
     the user to search Nixpkgs packages/toolchains or ask AI to generate `flake.nix` on the spot.
   - The user spawns tiled panes running **Claude Code**, **Antigravity-CLI**, **OpenCode**, or a
     shell.
   - Pressing `Ctrl+Space` → `Configure` opens the **AI Configurator** overlay to tweak TUI
     splits, theme, keybindings, dotfiles, or `flake.nix` packages, hot-reloading in place and
     saving to S3.
5. When the user closes the tab or stops typing for `idleTimeout` (e.g. 30m), `orbit-machined`
   snapshots dirty git work + tool state to S3 and exits cleanly.

## Non-goals

- **No SSH server requirement.** All terminal I/O runs through `orbit-machined` over WebSockets to
  the Orbit browser console.
- **No privileged Docker-in-Docker by default.** Machine Pods run unprivileged with Nix
  `sandbox = false` inside Kubernetes-isolated Pods (with optional `gvisor`/`kata` runtimeClass).
- **No per-user `/nix/store` PVCs.** Shared MinIO Nix binary cache + ephemeral `/nix/store` avoids
  volume lock contention and allows multiple concurrent machines when needed.
- **No gRPC dependency in Rails.** Control-plane calls between Orbit Rails and `orbit-dispatcher`
  use standard HTTPS REST/JSON; real-time terminal streaming goes directly between Browser and
  `orbit-dispatcher` over WebSockets.

## Build phases

Each phase ends on a verifiable completion criterion. Complete each phase before starting the next.

### Phase 1 — Go module foundation & shared domain types
- **Entry:** `AGENTS.md` and `agent-docs/` complete.
- **Work:** Initialize `flake.nix` devshell, `go.mod`, `Makefile`, `.golangci.yml`, and domain
  packages (`internal/domain`, `internal/ticket`, `internal/protocol`) defining machine state,
  HMAC/Ed25519 session tickets, REST payloads, and binary WebSocket frame codecs.
- **Completion:** `nix develop --command go test -race ./...` and `golangci-lint run` pass with
  100% round-trip test coverage on ticket signing and WebSocket frame encoding.

### Phase 2 — `orbit-machined` PTY supervisor & VT100 tiling compositor
- **Entry:** Phase 1 complete.
- **Work:** Build `internal/pty`, `internal/vt` (`charmbracelet/x/vt` wrapper with 60fps dirty
  diffing and snapshot serialization), and `internal/tui` (Bubble Tea + Lip Gloss tiling window
  manager with `Ctrl+Space` leader chords, mouse focus/resize, status bar, and tool launcher for
  Claude Code, Antigravity-CLI, OpenCode, and shell).
- **Completion:** Headless `teatest` suite verifies pane splitting, focus switching, mouse border
  dragging, child PTY resizing (`TIOCGWINSZ`), and screen-snapshot reconnect.

### Phase 3 — Flake Init Wizard & AI Configurator in the TUI
- **Entry:** Phase 2 complete.
- **Work:** Build the repository switcher (`internal/tui/repos`), the interactive **Flake Init
  Wizard** (`internal/tui/flakewizard`) with Nixpkgs search (`nix search --json`) for repos missing
  a `flake.nix`, and the **AI Configurator** overlay (`internal/tui/aiconfig`) with structured
  tools (`UpdateTUIConfig`, `UpdateKeybindings`, `EditFlakeNix`, `SetShellEnv`) and live hot-reload.
- **Completion:** Tests verify detecting a missing `flake.nix`, generating a valid `flake.nix`, and
  hot-reloading TUI configuration from AI tool calls.

### Phase 4 — S3 profile sync, Nix binary cache hooks & WIP git bundles
- **Entry:** Phase 3 complete.
- **Work:** Build `internal/storage` (MinIO/S3 client with envelope encryption for user profiles),
  boot-time hydration and shutdown sync of `~/.config/orbit-machine`, `~/.claude`,
  `~/.config/opencode`, and `~/.gemini`, idle activity tracker, and automatic WIP `git bundle`
  creation/restoration on idle shutdown.
- **Completion:** Integration tests against an in-memory/test S3 server verify full boot hydration,
  encrypted credential sync, and dirty git tree bundle backup + restore.

### Phase 5 — `orbit-dispatcher` Kubernetes controller & WebSocket gateway
- **Entry:** Phase 4 complete.
- **Work:** Build `cmd/orbit-dispatcher` (`internal/dispatcher`, `internal/k8s`): REST control
  plane client/server for Orbit, Kubernetes Pod reconciler (creating unprivileged machine Pods with
  resource constraints, owner references, and idle garbage collection), and the direct Browser ↔
  Pod WebSocket relay with ticket authentication.
- **Completion:** Fake `client-go` + WebSocket integration tests verify end-to-end Pod creation,
  browser ticket verification, bidirectional terminal streaming, and idle Pod cleanup.

### Phase 6 — NixOS container image & Helm chart
- **Entry:** Phase 5 complete.
- **Work:** Build `nix/machine-image.nix` (layered OCI image with Nix flakes enabled,
  `sandbox = false`, `orbit-machined` as entrypoint, `claude-code`, `antigravity-cli`, `opencode`,
  `git`, `direnv`, and MinIO post-build cache hook) and `charts/orbit-machines/` (Deployment for
  `orbit-dispatcher`, RBAC, NetworkPolicy, LimitRange/ResourceQuota, Ingress, and MinIO subchart
  with external S3 override).
- **Completion:** `nix build .#machine-image` builds a runnable OCI image and
  `helm lint charts/orbit-machines` + `helm template` pass cleanly.

### Phase 7 — Orbit Rails control plane & Admin/Workspace UI
- **Entry:** Phase 6 complete.
- **Work:** In `/home/david/Dev/ruby/orbit`: add `machines_enabled` toggle for Root Admins in
  `Admin::WorkspacesController`, add `MachineCluster` and `MachineSession` models, Workspace Admin
  settings page (`/:workspace/settings/machines`) with Helm command generator and Member/Team
  access toggles, and REST endpoints for `orbit-dispatcher` registration, ticket signing, and
  short-lived MCP/Git token minting.
- **Completion:** Minitest unit, controller, policy, and integration tests pass in `/home/david/Dev/ruby/orbit`,
  `bin/rubocop` clean.

### Phase 8 — Orbit GCP-style bottom console UI (`xterm.js` + Hotwire)
- **Entry:** Phase 7 complete.
- **Work:** In `/home/david/Dev/ruby/orbit`: build the persistent `#orbit-machine-console` bottom
  drawer (`data-turbo-permanent`) with collapsed footer bar, resizable docked split view, and
  full-screen overlay mode, powered by a Stimulus controller (`machine_console_controller.js`) and
  `xterm.js` (`@xterm/xterm`, `@xterm/addon-fit`, `@xterm/addon-webgl`) connecting directly to
  `orbit-dispatcher`. Add "Open in Machine" triggers on Issues and Repositories.
- **Completion:** Minitest + Capybara system tests verify opening, resizing, maximizing, and
  navigating across Orbit pages while keeping the terminal session live.
