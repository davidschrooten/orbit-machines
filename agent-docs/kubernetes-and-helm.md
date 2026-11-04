# Kubernetes and Helm chart (`charts/orbit-machines`)

Workspace admins deploy `Orbit Machines` onto their Kubernetes cluster using our Helm chart
(`charts/orbit-machines`) and connect it to their Orbit workspace.

## Installation experience

In Orbit's workspace settings (`/:workspace/settings/machines`), the Workspace Admin clicks
**Connect Kubernetes Cluster**, which generates a cluster registration token and displays the
ready-to-run Helm command:

```sh
helm install orbit-machines oci://ghcr.io/davidschrooten/charts/orbit-machines \
  --namespace orbit-machines --create-namespace \
  --set orbit.url="https://orbit.example.com" \
  --set orbit.clusterToken="omc_live_..." \
  --set ingress.host="machines.example.com"
```

## Components deployed by the Helm chart

1. **`orbit-dispatcher` (`Deployment` + `Service` + `Ingress`)**
   - Stateless Go service that:
     - Registers with the Orbit instance (`POST /api/internal/machine_clusters/register`) and sends
       periodic heartbeats (`POST /api/internal/machine_clusters/heartbeat`).
     - Receives machine lifecycle commands from Orbit or wakes machines when a browser connects
       with a valid Orbit-signed ticket.
     - Creates, watches, and deletes ephemeral Machine Pods via the Kubernetes API.
     - Terminates direct Browser WebSocket connections (`wss://<ingress.host>/ws/terminal`) and
       streams binary frames to the target Machine Pod's `orbit-machined` (`:8080/ws`).
2. **In-cluster MinIO (`Subchart / StatefulSet`, enabled by default)**
   - Automatically provisions two buckets on install:
     - `orbit-nix-cache` — Shared Nix binary cache for all machine Pods in the cluster.
     - `orbit-user-profiles` — Encrypted user TUI configs, AI CLI auth states, and WIP git bundles.
   - Admins who prefer AWS S3, Cloudflare R2, or Google Cloud Storage can set `minio.enabled=false`
     and provide `s3.endpoint`, `s3.region`, `s3.nixCacheBucket`, `s3.profilesBucket`, and
     credentials in `values.yaml`.
3. **Nix Binary Cache Signing Key (`Secret`)**
   - Generated automatically by a Helm pre-install hook (or supplied via `values.yaml`), mounted
     into Machine Pods so `post-build-hook` can sign derivations uploaded to `orbit-nix-cache`.
4. **RBAC & Namespace Isolation**
   - `ServiceAccount` + `Role` + `RoleBinding` scoped strictly to the release namespace (can only
     manage `pods`, `pods/log`, `secrets`, and `persistentvolumeclaims` carrying label
     `app.kubernetes.io/managed-by=orbit-dispatcher`).
5. **`NetworkPolicy`**
   - Machine Pods **cannot** reach other Machine Pods (preventing lateral movement between users).
   - Machine Pods can only accept ingress from `orbit-dispatcher` on port `8080`.
   - Machine Pods can egress to the cluster DNS, the in-cluster MinIO service (`:9000`), and public
     internet (Git hosts, LLM APIs, `cache.nixos.org`), while blocking cloud metadata IPs
     (`169.254.169.254/32`) and cluster-internal RFC1918 ranges outside MinIO/Orbit.

## Resource constraints & security in `values.yaml`

The Helm chart lets the cluster administrator enforce strict resource tiers and container security:

```yaml
orbit:
  url: "https://orbit.example.com"
  clusterToken: "" # Required (or via existingSecret)

ingress:
  enabled: true
  className: "nginx"
  host: "machines.example.com"
  tls:
    enabled: true

machines:
  image:
    repository: ghcr.io/davidschrooten/orbit-machine-nixos
    tag: "latest"
  idleTimeout: "30m"
  maxConcurrentMachines: 25
  maxMachinesPerUser: 1

  # Resource profiles selectable by Workspace Admins when granting access
  tiers:
    standard:
      requests:
        cpu: "1000m"
        memory: "2Gi"
        ephemeral-storage: "15Gi"
      limits:
        cpu: "4000m"
        memory: "8Gi"
        ephemeral-storage: "30Gi"
    performance:
      requests:
        cpu: "2000m"
        memory: "4Gi"
        ephemeral-storage: "25Gi"
      limits:
        cpu: "8000m"
        memory: "16Gi"
        ephemeral-storage: "60Gi"

  security:
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    runAsNonRoot: true
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: false # /home/orbit, /nix, /workspace are writable emptyDirs
    seccompProfile:
      type: RuntimeDefault
    capabilities:
      drop: ["ALL"]
    # Optional hard sandbox runtime (e.g., "gvisor" or "kata")
    runtimeClassName: ""

minio:
  enabled: true
  persistence:
    size: "100Gi"

s3:
  # Used when minio.enabled == false
  endpoint: ""
  region: "us-east-1"
  nixCacheBucket: "orbit-nix-cache"
  profilesBucket: "orbit-user-profiles"
  existingSecret: ""
```

## Machine Pod specification

When `orbit-dispatcher` spawns a Machine Pod for `(workspace_id, user_id)`:
- **Pod Name:** `orbit-machine-<workspace-short-id>-<user-short-id>`
- **Labels:**
  - `orbit.app/workspace-id: "<workspace_id>"`
  - `orbit.app/user-id: "<user_id>"`
  - `orbit.app/tier: "standard"`
- **Volumes:**
  - `nix-store`: `emptyDir` (sized to tier `ephemeral-storage` limit, initialized from the image's
    base `/nix` store).
  - `workspace`: `emptyDir` mounted at `/workspace`.
  - `home`: `emptyDir` mounted at `/home/orbit` (hydrated on boot from `s3://orbit-user-profiles`).
- **Environment / Ephemeral Secret:**
  - Short-lived Orbit MCP token, Git credential broker token, user's encrypted S3 profile key, and
    any workspace-level LLM proxy/API keys configured by the Workspace Admin.
