# Architecture

## System topology

`Orbit Machines` separates **low-frequency control-plane coordination** from **high-frequency
terminal I/O** so that Ruby/Rails never sits in the real-time byte stream and terminal latency stays
minimal.

```
┌────────────────────────────────────────────────────────────────────────────┐
│ User's Browser (Orbit Web UI)                                              │
│  ┌──────────────────────────────┐     ┌──────────────────────────────────┐ │
│  │ Hotwire / Turbo Pages        │     │ #orbit-machine-console           │ │
│  │ (Issues, Cycles, Epics, etc) │     │ (data-turbo-permanent, xterm.js) │ │
│  └──────────────┬───────────────┘     └─────────────────┬────────────────┘ │
└─────────────────┼───────────────────────────────────────┼──────────────────┘
                  │ 1. HTTPS (Launch / Ticket)            │ 3. Direct WSS (60fps binary
                  ▼                                       │    frames, resize, input)
┌─────────────────────────────────────────┐               │
│ Orbit Server (Rails 8 Monolith)         │               │
│  - Root Admin workspace toggle          │               │
│  - Workspace Admin Member/Team access   │               │
│  - Mints Ed25519/HMAC session tickets   │               │
│  - Mints short-lived MCP & Git tokens   │               │
└─────────────────┬───────────────────────┘               │
                  │ 2. HTTPS REST/JSON Control Plane      │
                  ▼    (Register, Provision, Stop, Sync)  ▼
┌────────────────────────────────────────────────────────────────────────────┐
│ Kubernetes Cluster (Installed via `charts/orbit-machines` Helm Chart)      │
│                                                                            │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │ orbit-dispatcher (Go Deployment)                                     │  │
│  │  - Verifies Orbit REST commands & browser session tickets            │  │
│  │  - Reconciles Machine Pods via K8s API (CPU/RAM/Disk/Security)       │  │
│  │  - Relays Browser WebSocket <-> Pod `:8080/ws` stream                │  │
│  └──────────────────┬───────────────────────────────────┬───────────────┘  │
│                     │ K8s Pod Lifecycle                 │ Pod WS Relay     │
│                     ▼                                   ▼                  │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │ Machine Pod (`ws-<workspace>-usr-<user>`, Unprivileged NixOS Image)  │  │
│  │  ┌────────────────────────────────────────────────────────────────┐  │  │
│  │  │ orbit-machined (Go PID 1 Supervisor & TUI Compositor)          │  │  │
│  │  │  - Hydrates & syncs encrypted user profile from S3             │  │  │
│  │  │  - Configures Orbit MCP & Git credential helper                │  │  │
│  │  │  - Bubble Tea + Lip Gloss Tiling WM + Flake Wizard + AI Config │  │  │
│  │  │  - PTY manager (`creack/pty`) + VT100 buffers (`x/vt`)         │  │  │
│  │  │    ├── Pane 1: Claude Code (`nix develop` env)                 │  │  │
│  │  │    ├── Pane 2: Antigravity-CLI (`nix develop` env)             │  │  │
│  │  │    └── Pane 3: OpenCode / Shell (`nix develop` env)            │  │  │
│  │  └───────────────┬────────────────────────────────┬───────────────┘  │  │
│  └──────────────────┼────────────────────────────────┼──────────────────┘  │
│                     │ s3://user-profiles             │ s3://nix-cache      │
│                     ▼                                ▼                     │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │ In-Cluster MinIO (or External S3 / R2 / GCS via Helm values.yaml)    │  │
│  │  - Bucket 1: `orbit-user-profiles` (encrypted settings, CLI auth,    │  │
│  │              WIP git bundles on idle shutdown)                       │  │
│  │  - Bucket 2: `orbit-nix-cache` (shared Nix binary cache for all      │  │
│  │              machines in the workspace)                              │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────────────────┘
```

## Why REST + WebSockets (and no gRPC)

Because the user's browser connects **directly** to `orbit-dispatcher` in Kubernetes for the
real-time terminal stream, the connection between **Orbit (Rails)** and **`orbit-dispatcher` (Go)**
only carries low-frequency control events:
- Cluster registration & periodic health/capacity heartbeats.
- Provisioning or waking a user's machine Pod and returning a signed browser connection ticket.
- Minting short-lived Orbit MCP and Git credentials for a booting Pod.
- Reporting machine state transitions (`starting`, `running`, `stopping`, `stopped`) and idle
  shutdowns.

Plain **HTTPS REST with JSON** (authenticated via HMAC-SHA256 / Bearer cluster tokens) is strictly
better here than gRPC:
- Zero `grpc` C-extension dependencies in Orbit's Ruby 4.0 Alpine container.
- Native `net/http` (`http.ServeMux` in Go 1.24+) on the Go side and standard `ActionController` +
  `Net::HTTP` on the Rails side.
- Trivially inspectable and testable with `httptest` and `curl`.

## Go repository layout

```text
orbit-machines/
├── AGENTS.md
├── agent-docs/
├── flake.nix                        # Dev shell + NixOS machine image build output
├── go.mod                           # module github.com/davidschrooten/orbit-machines
├── Makefile
├── .golangci.yml
├── cmd/
│   ├── orbit-dispatcher/            # Cluster-level K8s controller & WebSocket gateway
│   │   └── main.go
│   └── orbit-machined/              # In-pod PID 1 supervisor & Bubble Tea TUI host
│       └── main.go
├── internal/
│   ├── config/                      # Viper-backed typed configuration loaders & validation
│   ├── domain/                      # Pure domain types (Machine, Workspace, ResourceTier, Repo)
│   ├── ticket/                      # HMAC/Ed25519 browser session ticket signing & verification
│   ├── protocol/                    # REST DTOs and binary WebSocket frame codec
│   ├── dispatcher/                  # Dispatcher service orchestration (ports & use cases)
│   ├── k8s/                         # client-go adapter: Pod builder, reconciler, garbage collector
│   ├── machined/                    # In-pod lifecycle: boot hydration, idle timer, graceful stop
│   ├── storage/                     # S3/MinIO adapter, AES-GCM envelope encryption, tar.zst sync
│   ├── gitwip/                      # Dirty git working tree detection, `git bundle` save/restore
│   ├── nixenv/                      # `nix develop` env extraction, `nix search`, Flake generator
│   ├── pty/                         # `creack/pty` process lifecycle & signal forwarding
│   ├── vt/                          # `charmbracelet/x/vt` emulator, 60fps dirty-diff compositor
│   └── tui/                         # Bubble Tea + Lip Gloss tiling WM, Flake Wizard, AI Config
│       ├── wm/                      # Tiling layout tree (splits, tabs, focus, mouse border drag)
│       ├── statusbar/               # Top/bottom Chrome, repo/issue badge, idle timer
│       ├── launcher/                # Tool picker (Claude Code, Antigravity-CLI, OpenCode, Shell)
│       ├── flakewizard/             # Interactive `flake.nix` creator for repos without flakes
│       └── aiconfig/                # Built-in AI Configurator overlay & structured tool handlers
├── nix/
│   └── machine-image.nix            # Reproducible NixOS container image definition
└── charts/
    └── orbit-machines/              # Kubernetes Helm chart
        ├── Chart.yaml
        ├── values.yaml
        └── templates/
```

## Go design patterns & engineering standards

1. **Hexagonal Architecture (Ports and Adapters):**
   - Core packages (`internal/dispatcher`, `internal/machined`, `internal/tui`) define narrow
     interfaces at the point of use (e.g., `PodOrchestrator`, `ProfileStore`, `NixRunner`).
   - Infrastructure packages (`internal/k8s`, `internal/storage`, `internal/nixenv`) implement
     those interfaces.
2. **Structured Concurrency (`golang.org/x/sync/errgroup`):**
   - Every background loop (heartbeat reporter, idle monitor, PTY reader, 60fps frame ticker) takes
     a `context.Context` and runs inside an `errgroup.Group` so cancellation and shutdown propagate
     deterministically without leaked goroutines.
3. **Functional Options for Complex Construction:**
   - Use `With...` functional options when configuring `dispatcher.Server`, `machined.Daemon`, or
     `tui.Compositor` with sensible defaults.
4. **Structured Logging (`log/slog`):**
   - Pass `*slog.Logger` explicitly with contextual attributes (`workspace_id`, `user_id`,
     `machine_id`, `pod_name`). Never log inside the TUI's stdout stream; `orbit-machined` writes
     structured JSON logs to file/stderr (collected by Kubernetes container logs) while reserving
     the PTY channel strictly for TUI frames.
5. **Explicit Error Wrapping:**
   - Wrap errors with context (`fmt.Errorf("hydrating user profile: %w", err)`) and define sentinel
     domain errors (`domain.ErrMachineNotFound`, `domain.ErrTicketExpired`, `domain.ErrNoFlake`)
     checked via `errors.Is`.
6. **CLI & 12-Factor Configuration (`spf13/cobra` + `spf13/viper`):**
   - Both `cmd/orbit-dispatcher` and `cmd/orbit-machined` construct their command trees using
     `github.com/spf13/cobra` via pure factory functions (`NewRootCmd()`) with no package-level
     global state so CLI commands can be tested in parallel.
   - Configuration in `internal/config` uses dedicated `viper.New()` instances (bound to Cobra flags,
     environment variables with `ORBIT_DISPATCHER_` / `ORBIT_MACHINED_` prefixes, and optional
     YAML/TOML config files) unmarshaled into validated, immutable Go structs.

## Binary WebSocket terminal protocol

The WebSocket connection (`Browser <-> orbit-dispatcher <-> orbit-machined`) uses binary frames
with a 1-byte frame type header so keystrokes, resizes, and compressed screen updates share a single
connection with minimal overhead:

| Byte `0` | Direction | Payload | Purpose |
|---|---|---|---|
| `0x01` | Browser → Pod | Raw UTF-8 / ANSI input bytes | Keyboard & mouse X10/SGR events |
| `0x02` | Browser → Pod | `uint16 cols, uint16 rows` (big-endian) | Browser terminal resize (`xterm.js` fit) |
| `0x03` | Browser → Pod | `uint64 client_timestamp_ms` | Latency ping & user activity heartbeat |
| `0x10` | Pod → Browser | `zstd`-compressed ANSI full snapshot | Sent immediately on connect/reconnect |
| `0x11` | Pod → Browser | `zstd`-compressed ANSI dirty diff | 60fps incremental frame update |
| `0x12` | Pod → Browser | `uint64 client_timestamp_ms` | Pong response (displayed as ms RTT in UI) |
| `0x13` | Pod → Browser | JSON metadata (`repo`, `issue`, `idle_secs`, `status`) | Updates the Orbit console header bar |
