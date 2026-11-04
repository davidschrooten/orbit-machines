# AGENTS.md

`orbit-machines` provisions ephemeral, Nix-powered cloud development machines on Kubernetes for
[Orbit](/home/david/Dev/ruby/orbit) workspaces. Engineers open a GCP Cloud Shell-style bottom
console inside Orbit (expandable to full screen), connect directly over WebSockets to a Go
tiling window manager running inside an unprivileged NixOS-based Pod, and work across their
workspace's repositories with Claude Code, Antigravity-CLI, and OpenCode pre-wired to Orbit's
MCP server.

This file is the entry point. Keep it short and current. Detailed specifications live in
[`agent-docs/`](agent-docs/), indexed below.

## Current state

**Phases 1–6 — Complete in `orbit-machines` (96.2% total statement coverage across all 18 packages, plus production Kubernetes Helm chart, Nix flake, Makefile, and GitHub Actions CI/Release workflows). Ready for Phases 7 & 8 in `orbit/`.**

See [`project-overview.md`](agent-docs/project-overview.md) for the eight build phases and their
entry/completion criteria.

### System summary

- **Two Go binaries in one module:**
  - `cmd/orbit-dispatcher` runs as a Kubernetes Deployment installed by the workspace admin via
    our Helm chart. It pairs with an Orbit workspace over HTTPS REST, reconciles ephemeral
    machine Pods, and terminates direct browser WebSocket streams (`xterm.js`).
  - `cmd/orbit-machined` runs as PID 1 inside each ephemeral machine Pod. It hydrates the user's
    encrypted S3 profile, manages the stateless `/nix/store` backed by the cluster's shared
    MinIO Nix binary cache, hosts the in-memory Bubble Tea + Lip Gloss tiling window manager
    (`creack/pty` + `charmbracelet/x/vt`), and snapshots uncommitted git work before idle
    shutdown.
- **Two-tier governance:** A Root Admin (`super_admin`) enables `Orbit Machines` per workspace in
  Orbit's Admin Console. The Workspace Admin connects their Kubernetes cluster via Helm and
  grants access per Member or per Orbit Team.
- **Browser-native, zero-lag terminal:** Control-plane traffic (`Orbit Rails ↔ orbit-dispatcher`)
  uses REST/JSON. Real-time terminal I/O (`Browser xterm.js ↔ orbit-dispatcher ↔ orbit-machined`)
  streams directly between the browser and Kubernetes over compressed binary WebSockets with
  60fps dirty-screen diffing and instant screen-snapshot recovery on reconnect.
- **Nix flakes everywhere:** Opening a repository in the TUI enters its `flake.nix` devshell. If a
  repository lacks a `flake.nix`, the TUI launches an interactive **Flake Init Wizard** (with
  Nixpkgs search and AI assistance) to generate one on the spot.

## Golden rules

1. **Standard-library-first Go 1.24+ with hexagonal boundaries.** Domain logic in `internal/` has
   zero Kubernetes, S3, or HTTP imports; adapters implement narrow consumer-defined interfaces.
   See [`architecture.md`](agent-docs/architecture.md).
2. **Every operation is workspace- and user-scoped.** `orbit-dispatcher` verifies Orbit-signed
   session tickets and workspace cluster tokens on every request; a Pod belongs to exactly one
   `(workspace_id, user_id)` pair.
3. **Rails stays out of the terminal hot path.** Use HTTPS REST between Orbit and
   `orbit-dispatcher` for lifecycle control; route interactive terminal WebSockets directly from
   the browser to `orbit-dispatcher`.
4. **The TUI compositor owns the screen.** Child tools (Claude Code, Antigravity-CLI, OpenCode,
   shells) run inside `creack/pty` backed by `charmbracelet/x/vt` virtual terminal buffers;
   `orbit-machined` renders dirty diffs at a capped 60fps so verbose commands never flood the
   browser WebSocket. See [`tui-and-shell.md`](agent-docs/tui-and-shell.md).
5. **Stateless `/nix/store`, shared MinIO Nix binary cache.** Pods use ephemeral storage
   (`emptyDir`) for `/nix/store` and push/pull pre-built derivations through the cluster's
   MinIO/S3 binary cache (`s3://nix-cache`). Never mount a shared RW volume across concurrent
   Pods as `/nix/store`. See [`nix-and-storage.md`](agent-docs/nix-and-storage.md).
6. **Unprivileged Pods by default.** Run Nix with `sandbox = false` inside Kubernetes Pods
   constrained by CPU, memory, ephemeral-storage limits, `RuntimeDefault` seccomp, and optional
   `runtimeClassName` (`gvisor` / `kata`). Never require `privileged: true` or `SYS_ADMIN`.
7. **Never lose user settings or uncommitted work.** Sync `~/.config/orbit-machine`, `~/.claude`,
   `~/.config/opencode`, and `~/.gemini` to the user's encrypted S3 prefix. On inactivity
   timeout, bundle dirty git working trees to S3 before Pod deletion and offer restoration on
   next boot.
8. **TDD with `-race`.** Every package is tested with `go test -race ./...`. Table-driven unit
   tests for domain logic, headless Bubble Tea model tests (`teatest`) for the TUI, and `envtest`
   / fake client-go for Kubernetes reconciliation.
9. **Feature branches, merged `--no-ff`** into `master`. One concern per branch.
10. **No `Co-authored-by` trailers** in commit messages.

## Documentation index

| Document | Read this when… |
|---|---|
| [`workflow.md`](agent-docs/workflow.md) | …starting any task. Branching, commit rules, TDD loop, linting, definition of done. |
| [`project-overview.md`](agent-docs/project-overview.md) | …checking scope, user/admin journeys, non-goals, or the 8 build phases. |
| [`architecture.md`](agent-docs/architecture.md) | …designing Go packages, REST/WebSocket protocols, concurrency, or error handling. |
| [`tui-and-shell.md`](agent-docs/tui-and-shell.md) | …working on the Bubble Tea tiling WM, VT100 emulation, leader keys, Flake Wizard, or AI Configurator. |
| [`nix-and-storage.md`](agent-docs/nix-and-storage.md) | …building the NixOS container image, MinIO binary cache hooks, S3 profile sync, or WIP git snapshots. |
| [`kubernetes-and-helm.md`](agent-docs/kubernetes-and-helm.md) | …editing the Helm chart, Pod specs, resource tiers, RBAC, NetworkPolicies, or MinIO subchart. |
| [`orbit-integration.md`](agent-docs/orbit-integration.md) | …wiring the Rails control plane, Super Admin/Workspace Admin UI, or the GCP-style bottom console in [`../orbit`](../orbit/AGENTS.md). |
| [`../agent-docs/cross-repo-contracts.md`](../agent-docs/cross-repo-contracts.md) | …checking or updating session ticket schemas, REST payloads, or binary WebSocket frames shared with `orbit`. |

## Commands

The dev shell is a Nix flake; `direnv allow` (or `nix develop`) puts Go, `golangci-lint`,
`helm`, `kubectl`, and Nix image tooling on `PATH`.

| Command | Purpose |
|---|---|
| `direnv allow` | Enter the Nix dev shell |
| `go test -race ./...` | Run all unit and integration tests with the race detector |
| `golangci-lint run` | Lint all Go packages |
| `go build ./cmd/orbit-dispatcher` | Build the Kubernetes cluster dispatcher binary |
| `go build ./cmd/orbit-machined` | Build the in-pod supervisor and TUI binary |
| `nix build .#machine-image` | Build the NixOS ephemeral container image |
| `helm lint charts/orbit-machines` | Lint the Kubernetes Helm chart |
| `helm template charts/orbit-machines` | Render and validate Helm manifests locally |
