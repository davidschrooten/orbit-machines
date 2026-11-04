# Nix and storage

## 1. Custom NixOS-based container image (`nix/machine-image.nix`)

Every ephemeral machine Pod runs a reproducible OCI container image built via Nix flakes
(`pkgs.dockerTools.buildLayeredImage` or `nixos-generators` container rootfs).

### What the base image contains
- **PID 1 Supervisor:** `/bin/orbit-machined`
- **Nix with Flakes enabled:** `/etc/nix/nix.conf` configured with:
  ```ini
  experimental-features = nix-command flakes
  sandbox = false
  accept-flake-config = true
  substituters = http://orbit-machines-minio:9000/orbit-nix-cache?compression=zstd https://cache.nixos.org
  trusted-public-keys = orbit-cluster-cache-1:<CLUSTER_PUBKEY> cache.nixos.org-1:6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY=
  post-build-hook = /etc/nix/upload-to-cache.sh
  ```
- **Pre-installed AI Coding Agents & CLI tools:**
  - `claude-code` (`claude`)
  - `antigravity-cli` (`agy`)
  - `opencode` (`opencode`)
  - `git`, `gh`, `direnv`, `nix-direnv`, `ripgrep`, `fd`, `jq`, `curl`, `zstd`, `zsh`, `bash`, `neovim`
- **Non-root User (`orbit`, UID `1000`):** Runs unprivileged inside the Kubernetes Pod with
  single-user Nix store ownership (`/nix` owned by UID `1000`) so `nix develop` and `nix build`
  require zero root privileges or Linux `SYS_ADMIN` capabilities.

## 2. Stateless `/nix/store` + Shared MinIO Nix Binary Cache

Mounting a network volume as `/nix/store` read-write across multiple Pods corrupts Nix's SQLite
database (`/nix/var/nix/db`). Instead, `Orbit Machines` uses a **stateless Pod store backed by a
cluster-wide MinIO Nix binary cache**:

1. **Per-Pod Ephemeral Volume (`emptyDir`):**
   - `/nix` and `/workspace` live on fast node-local ephemeral storage (bounded by the Helm chart's
     `ephemeral-storage` limit, e.g., `20Gi`).
   - Base image layers live in the container runtime's overlayfs, so common tools (`git`, `claude`,
     `agy`, `opencode`, `nix`) take 0ms and 0 bytes of extra disk on boot.
2. **Cluster-Wide Nix Binary Cache (`s3://orbit-nix-cache` on MinIO):**
   - When the Helm chart is installed, it generates a cluster Nix signing keypair
     (`orbit-cluster-cache-1`) stored in a Kubernetes Secret.
   - Whenever any user's machine builds a new flake derivation, `/etc/nix/upload-to-cache.sh`
     (`post-build-hook`) signs the output paths and uploads them asynchronously (`nix copy --to
     's3://orbit-nix-cache?...'`) to the in-cluster MinIO (or external S3).
   - When any subsequent machine opens the same repository, `nix develop` downloads pre-built
     zstd-compressed `.nar` closures over the cluster LAN at 10Gbps+ in a few seconds instead of
     recompiling.

## 3. User settings & tool state persistence in S3 (`s3://orbit-user-profiles`)

Each user has an isolated prefix in the `orbit-user-profiles` bucket:
`s3://orbit-user-profiles/workspaces/<workspace-id>/users/<user-id>/`

### What is persisted in `profile.tar.zst.enc`
- `~/.config/orbit-machine/` — TUI layout presets, custom leader keybindings, Lip Gloss theme,
  shell aliases, and AI Configurator history.
- `~/.claude/` (excluding large transient conversation caches) — Claude Code auth & settings.
- `~/.config/opencode/` & `~/.local/share/opencode/auth.json` — OpenCode auth & provider config.
- `~/.gemini/` — Antigravity-CLI auth & configuration.
- `~/.gitconfig.local` — Any custom git preferences beyond Orbit's automatic identity wiring.

### Encryption & sync lifecycle
- **Envelope Encryption:** `orbit-machined` encrypts `profile.tar.zst` with AES-256-GCM using a
  per-user key derived via HKDF from the workspace cluster secret and user ID before uploading to
  S3.
- **On Boot:** Before starting the Bubble Tea TUI, `orbit-machined` downloads and extracts
  `profile.tar.zst.enc` into `/home/orbit` (~200ms).
- **Live Debounced Sync:** Whenever the AI Configurator updates settings or a tool modifies its auth
  file (monitored via `fsnotify`), `orbit-machined` schedules a debounced background upload (every
  30s on change).
- **On Graceful Shutdown:** Performs a final synchronous flush of `profile.tar.zst.enc` before
  exiting PID 1.

## 4. Inactivity timeout & automatic WIP Git bundles

Machines shut down automatically after `idleTimeout` (configured in Helm values or workspace
settings, e.g. `30m` or `60m`) of no user input or active foreground agent execution.

To guarantee engineers never lose uncommitted code when stepping away:
1. Before shutting down on idle timeout (or `SIGTERM` from Kubernetes), `orbit-machined` scans
   every cloned repository under `/workspace/*`.
2. For each repository where `git status --porcelain` is non-empty or unpushed commits exist on the
   current branch:
   - `orbit-machined` creates a temporary commit on a detached ref and packs a full snapshot
     (`git bundle create`) plus a working-tree diff patch, encrypting and uploading it to:
     `s3://orbit-user-profiles/workspaces/<ws>/users/<usr>/wip/<repo-name>-<timestamp>.bundle.enc`
3. Next time the user boots a machine and opens that repository, the TUI detects the saved WIP
   snapshot and shows a banner:
   > *"Found uncommitted changes saved before your last machine went idle (14 minutes ago).
   > `[R] Restore changes`  `[D] Discard`"*
