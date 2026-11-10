# Changelog

All notable changes to **Orbit Machines** (`orbit-dispatcher`, `orbit-machined`, `orbit-helper`, and the `charts/orbit-machines` Helm chart) are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## 0.2.0 — 2026-11-10

### Added

- **Cross-Device & Reload Persistent Multi-Tab PTY Sessions (`internal/tui`, `internal/protocol`, `internal/pty`)**:
  - Extended `0x13 FrameMetadata` (`MetadataEvent`) with `active_tabs` (`[]string`) and `closed_tab_id` (`string`) for real-time tab synchronization across browser reloads and multiple connected devices.
  - Added `WSServer.ActiveTabIDs()` and `GET /api/v1/tabs` returning numerically sorted active tab sessions (`tab-1`, `tab-2`, ..., `tab-10`).
  - Persisted active tab metadata to `<workspaceDir>/.orbit/tabs.json` and per-tab scrollback buffers (up to 256 KiB per tab via `Pane.PrependScrollback`) to `<workspaceDir>/.orbit/tabs/<tabID>.scrollback` so multi-tab sessions survive browser reloads, device switches, and Pod restarts on the workspace PVC.
  - Broadcast `0x13` `session_state` frames with `active_tabs` upon WebSocket connection and new tab creation, and broadcast `closed_tab_id` when a tab is closed so all connected devices stay synchronized.
- **Zero-Touch Orbit MCP Wiring for Claude Code, OpenCode & Antigravity CLI (`internal/helper`, `cmd/orbit-machined`)**:
  - Automatic `SyncToolMCPConfigs` execution at `orbit-machined` startup, during `POST /api/v1/context` refresh, and inside `orbit-helper tools`.
  - Streamable HTTP MCP endpoint resolution (`/api/v1/workspaces/<slug>/mcp`) with fallback to `ORBIT_URL`, `ORBIT_WORKSPACE_SLUG`, and `ORBIT_MCP_TOKEN` environment variables.
  - **Claude Code**: Pre-populates `~/.claude.json` (`mcpServers.orbit` with `"type": "http"`, `"url"`, and `"headers"` plus per-project `projects[dir]` approval entries) and `~/.claude/settings.json` (`enableAllProjectMcpServers: true`, `enabledMcpjsonServers: ["orbit"]`), and automatically repairs legacy workspace `.mcp.json` files lacking `"type": "http"`.
  - **OpenCode**: Generates `~/.config/opencode/opencode.json` using OpenCode's native `"$schema": "https://opencode.ai/config.json"` and `"mcp": { "orbit": { "type": "remote", "url": "...", "enabled": true, "headers": { ... } } }` schema, automatically migrating legacy `"mcpServers"` keys.
  - **Antigravity CLI (`agy`)**: Pre-populates `~/.gemini/config/mcp_config.json` with both `"url"` and `"serverUrl"` fields and `"headers"` for immediate MCP tool discovery.
- **Session Ticket Claims & Credential Refresh (`internal/ticket`, `internal/dispatcher`, `internal/k8s`)**:
  - Added `orbit_url` and `mcp_token` fields to HMAC-SHA256 `SessionClaims` so `orbit-dispatcher` can inject the exact Orbit instance URL and user MCP token directly into Pod manifests and live `POST /api/v1/context` refreshes even when the control-plane credential minting endpoint is unreachable.
- **Persistent Home & Per-Repository AI State Across Pod Restarts (`internal/shellinit`, `internal/helper`, `internal/k8s`)**:
  - Bi-directional persistence (`SyncPersistentHomeFiles`) between `$HOME` (`/home/orbit`) and the 20 GiB workspace PVC (`/home/orbit/workspace/.orbit/home`) for `~/.claude`, `~/.claude.json`, `~/.gemini`, `~/.config`, `~/.local`, `~/.ssh`, `~/.gitconfig`, and `~/.bash_history`.
  - Automatic per-repository symlink persistence (`EnsureRepoToolPersistence`) for `.claude`, `.gemini`, `.opencode`, `.aider.conf.yml`, and `.aider.chat.history.md` under `/home/orbit/workspace/.orbit/repos/<repo>/`.
- **Interactive `orbit-helper` (`orbit`) TUI & CLI (`internal/helper`, `cmd/orbit-helper`)**:
  - Full-screen Bubble Tea TUI (`orbit` / `orbit-helper`) for multi-select AI & developer tool installation (`claude`, `codex`, `gemini`, `opencode`, `cursor`, `aider`, `agy`, `gh`, `git`), live `nixpkgs` package search/install/uninstall (`~/.config/orbit/configuration.nix`), Nix flake + `direnv` initialization, Ed25519 SSH key generation/import, and workspace repository cloning.
- **Full-Screen TUI Application Support & File Uploads (`internal/pty`, `internal/tui`, `internal/protocol`)**:
  - Added `RawPassthrough` mode on `pty.Pane` and `SIGWINCH` process-group signaling (`NotifyWinch` / `Redraw`) so full-screen terminal applications (`claude`, `opencode`, `nano`, `nvim`, `htop`) render without blocking on terminal queries (`DSR`, `DA1/DA2`, `OSC 10/11`, `DECRQM`).
  - Added `0x04 FrameFileUpload` binary WebSocket framing and `POST /api/v1/upload` (JSON and `multipart/form-data`) for drag-and-drop file uploads into `/home/orbit/workspace`.

### Fixed

- **Nix Store Volume Seeding (`cmd/orbit-machined`, `internal/k8s`)**:
  - Added `orbit-machined seed-nix` initContainer that seeds mounted `/nix` PVCs from the container image's `/nix` store while ensuring writable directory permissions (`0755`) before copying read-only Nix store paths (`0555`/`0444`).
- **Immediate Session Pod Termination (`internal/dispatcher`, `internal/k8s`)**:
  - Added `DELETE /api/v1/sessions/{sub}` on `orbit-dispatcher` and removed redundant pre-stop snapshot bottlenecks so stopping a machine session from Orbit terminates the Kubernetes Pod immediately.

---

## 0.1.0 — 2026-11-04

### Added

- Initial release of **Orbit Machines** across 18 Go packages (`internal/domain`, `internal/config`, `internal/ticket`, `internal/protocol`, `internal/k8s`, `internal/orbitclient`, `internal/dispatcher`, `internal/storage`, `internal/gitwip`, `internal/nixenv`, `internal/machined`, `internal/pty`, `internal/tui`, `internal/tui/flakewizard`, `internal/tui/aiconfig`, `internal/helmchart`, `cmd/orbit-dispatcher`, and `cmd/orbit-machined`).
- **`orbit-dispatcher`**: Stateless Kubernetes HTTP/WebSocket gateway, HMAC-SHA256 session ticket verifier, Pod/PVC reconciler, and Orbit heartbeat/registration client.
- **`orbit-machined`**: In-Pod PID 1 supervisor, AES-256-GCM S3/MinIO profile vault, Git WIP bundle manager, Nix flake evaluator + S3 binary cache uploader, and zstd-compressed binary WebSocket (`0x01`–`0x13`) terminal server.
- **Production Helm Chart (`charts/orbit-machines`) & Nix Flake (`flake.nix`)**: Zero-ClusterRole namespaced RBAC, egress NetworkPolicy blocking cloud metadata (`169.254.169.254/32`) and RFC1918 ranges, optional in-cluster MinIO StatefulSet, and multi-arch Nix/Docker build pipelines.
