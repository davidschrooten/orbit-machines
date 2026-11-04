# Orbit integration (`/home/david/Dev/ruby/orbit`)

How `orbit-machines` integrates with the Rails 8 application in `/home/david/Dev/ruby/orbit`.

## 1. Two-tier governance & permissions

### Tier 1: Root Admin (`super_admin`) enables Orbit Machines for a Workspace
- Stored on `Workspace` (`settings["machines_enabled"]`, exposed as `workspace.machines_enabled?`).
- Toggled by a Root Admin (`super_admin`) in the Orbit Admin Console's Workspace side panel
  (`Admin::WorkspacesController#update_settings`), alongside `time_tracking_enabled` and
  `keep_agent_traces`.
- Every toggle is recorded in `AuditEvent` (`workspace.settings_update`).
- If `workspace.machines_enabled?` is `false`, no Machine UI, settings, or API endpoints are
  accessible in that workspace.

### Tier 2: Workspace Admin connects the cluster & grants Member/Team access
- Only Workspace Owners and Admins (`membership.at_least?(:admin)`) can manage
  `/:workspace/settings/machines`.
- **Cluster Pairing (`MachineCluster` model, belongs to `Workspace`):**
  - Stores `name`, `token_digest` (and `token_prefix`), `dispatcher_url` (reported by
    `orbit-dispatcher` on registration), `websocket_url`, `status` (`pending`, `connected`,
    `offline`), `last_heartbeat_at`, `available_tiers` (`jsonb`), and `idle_timeout_minutes`.
- **Who can use Orbit Machines:**
  - Workspace Admins can enable access:
    - **Per Member:** `memberships.machines_enabled` (`boolean`, default `false`) and
      `memberships.machine_tier` (`string`, default `"standard"`).
    - **Per Team:** `teams.machines_enabled` (`boolean`, default `false`) and
      `teams.machine_tier` (`string`, default `"standard"`).
  - A user is authorized (`MachinePolicy#use?`) when:
    1. `workspace.machines_enabled?` is `true`, AND
    2. `workspace.machine_cluster&.connected?` is `true`, AND
    3. `membership.at_least?(:admin) || membership.machines_enabled? || user.teams.where(workspace: workspace, machines_enabled: true).exists?`.

## 2. Control-plane REST API between Orbit and `orbit-dispatcher`

### Cluster → Orbit (`Api::Internal::MachineClustersController`)
Authenticated via `Authorization: Bearer omc_live_<cluster_token>`:

| Endpoint | Purpose |
|---|---|
| `POST /api/internal/machine_clusters/register` | Called on `orbit-dispatcher` startup. Reports `websocket_url`, `tiers`, `idle_timeout_minutes`, and cluster version. Returns workspace metadata and the HMAC/Ed25519 ticket verification public key. |
| `POST /api/internal/machine_clusters/heartbeat` | Called every 30s with active machine statuses (`user_id`, `pod_name`, `state`, `cpu_Millicores`, `memory_mb`, `active_repo`, `idle_seconds`). |
| `POST /api/internal/machine_clusters/credentials` | Called by `orbit-dispatcher` when booting or refreshing a Pod for `user_id`. Returns a short-lived Orbit MCP token, the list of workspace `Repository` records (name, clone URL, provider), a short-lived Git clone/push token for those repositories, and the user's git author name/email. |

### Browser → Orbit (`MachinesController`, workspace-scoped)
Authenticated via normal Devise session + Pundit `MachinePolicy`:

| Endpoint | Purpose |
|---|---|
| `POST /:workspace_slug/machines/session` | Authorizes the user (`MachinePolicy#use?`), mints a short-lived (5-minute) signed connection ticket encoding `(workspace_id, user_id, tier, idle_timeout, initial_repo, initial_issue_identifier)`, and returns `{ websocket_url, ticket }`. |
| `DELETE /:workspace_slug/machines/session` | Sends a stop request to `orbit-dispatcher` to gracefully snapshot and terminate the user's running machine Pod. |

When the browser connects to `wss://<dispatcher-host>/ws/terminal?ticket=<signed-ticket>`,
`orbit-dispatcher` verifies the ticket signature locally, ensures the user's Pod is running (spawning
it if needed and streaming boot progress events to the console), and attaches the WebSocket to
`orbit-machined`.

## 3. GCP Cloud Shell-style bottom console in Orbit's UI

In Orbit (`app/views/layouts/application.html.erb`), authorized members get a persistent bottom
console container marked `data-turbo-permanent`:

```erb
<% if current_workspace && policy(:machine).use? %>
  <%= render "machines/console_drawer" %>
<% end %>
```

### Three visual states (controlled by `machine_console_controller.js`)

1. **Collapsed Footer Bar (default, `h-8` at bottom of viewport or triggered from toolbar):**
   - Shows a subtle status pill: `● Machine Ready` (or `● Running: orbit-core (CO-142) · 24m idle`),
     click to expand.
2. **Docked Bottom Split Drawer (GCP Cloud Shell mode, resizable height `240px`–`75vh`):**
   - Sits anchored to the bottom of the screen while the user browses Issues, Cycles, Epics, and
     Context documents above it. Because `#orbit-machine-console` has `data-turbo-permanent`,
     clicking around Orbit with Turbo Drive **never interrupts or reloads** the `xterm.js`
     WebSocket connection or the Bubble Tea TUI!
   - Compact header bar (`h-8`) with:
     - Connection status & RTT ping badge (`12ms`)
     - Active repository & issue badge
     - Idle countdown indicator
     - **Minimize (`_`)**, **Maximize to Full Screen (`⤢`)**, and **Stop Machine (`■`)** buttons.
3. **Full-Screen Overlay (`fixed inset-0 z-50`):**
   - Covers the entire Orbit interface so the Bubble Tea tiling window manager has 100% of the
     browser viewport for side-by-side Claude Code / Antigravity-CLI / OpenCode panes.
   - Pressing `Esc` in the header bar (or clicking the restore icon) drops it back down to the
     docked bottom drawer without losing focus.

### Contextual "Open in Machine" actions
- On any **Issue side panel** or **Repository row**, an authorized user can click **Open in Machine**,
  which dispatches a browser custom event (`orbit:machine:open`) with `{ repo, issue }` to
  `machine_console_controller.js`, expanding the bottom drawer and instructing `orbit-machined` to
  open that repository tab and branch!
