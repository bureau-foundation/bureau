# Bureau Information Architecture

[fundamentals.md](fundamentals.md) defines the design principles and primitives.
[architecture.md](architecture.md) defines the runtime contracts. This document defines the
persistent data model: how Bureau organizes information in Matrix,
what lives where, and the patterns for reading and writing it.

---

## Matrix as Data Model

Matrix provides three primitives that Bureau maps to three levels of
data:

**State events** are typed, keyed, queryable records. Each state event
is identified by a triple: (room, event type, state key). Setting a
state event replaces the previous value for that triple. State events
are the primary data model — everything that needs to survive a reboot
and be queryable by key is a state event: templates, machine config,
service registrations, tickets, workspace lifecycle.

**Timeline events** are ordered, append-only messages. They carry
operational data: commands, results, progress updates, conversation.
Timeline events are historical — you read them in order, not by key.

**Threads** are sequences of timeline events linked to a root message
via `m.thread` relations. They scope conversation within a room without
polluting the room-level timeline.

The distinction matters for Bureau because it maps directly to how the
system uses data:

- **Configuration, identity, and state** → state events. The daemon
  reads machine config, templates, and service bindings as state events.
  The ticket service reads ticket state events. These are "what is the
  current value of X?" queries.
- **Operations and conversation** → timeline events. CLI commands,
  pipeline step logs, agent status updates. These are "what happened,
  in order?" queries.
- **Focused work sessions** → threads. A pipeline run, an agent work
  session, a human-agent conversation. These are bounded in time and
  scope, nested within a room.

Bureau uses Matrix state events as the data model rather than a
separate database (SQLite, Postgres, etcd). The advantage is a single
source of truth with no sync layer: automatic replication across the
federation, query via standard Matrix API, room membership as access
control, and event history as audit trail.

---

## The Hierarchy

Bureau's information architecture has three levels, each backed by a
Matrix primitive:

```
Bureau root space (#bureau)
  └── Rooms (work contexts, registries, config)
        └── Threads (conversations, pipeline runs, sessions)
```

### Spaces

The Bureau root space `#bureau:bureau.local` is the organizational
container. All standard Bureau rooms are children of this space, linked
via `m.space.child` state events.

Spaces are discovery mechanisms, not access hierarchies. Matrix space
membership does not cascade to child rooms — a member of the space is
not automatically a member of its child rooms. The daemon manages room
membership independently, based on principal assignments and invitation
logic.

As deployments grow, organizational sub-spaces (e.g., `#iree`,
`#service`) can nest under the root space. The `m.space.child`
relationship is the only structural link — everything else (membership,
permissions, triggers) is per-room.

### Rooms

Rooms are the primary unit of organization. Each room is a durable
context where principals participate, state accumulates, and history
persists.

A room provides:

- **State events** — typed, keyed configuration and data
- **Membership** — who can see and act in this context
- **Power levels** — who can do what (send messages, set state, invite)
- **Lifecycle** — rooms are created, used, and can be archived
- **Identity** — a room alias in the namespace hierarchy
- **History** — all messages and state changes are persistent

Rooms are invite-only by default. The homeserver enforces room
membership as the primary security boundary. Power levels then gate
operations within rooms.

### Threads

Threads are conversations within rooms. They have no independent
lifecycle, no state events, no membership control. A thread is a root
message plus all replies linked via `m.thread` relation type.

Threads are where most real-time communication happens, but they inherit
everything from their parent room: membership, power levels, visibility.
A thread cannot have its own triggers, its own credentials, or its own
service bindings.

When work outgrows a thread — when it needs its own state, membership,
or lifecycle — it becomes a room. This graduation is explicit: a new
room is created, context is migrated, and the space hierarchy is
updated.

### Room or thread?

The decision reduces to: does this work need room features?

| Feature | Room | Thread |
|---------|------|--------|
| State events | Yes | No |
| Independent membership | Yes | No (inherits room) |
| Power levels | Yes | No (inherits room) |
| Lifecycle control | Yes | No (lives with room) |
| Alias in the namespace | Yes | No |
| StartCondition triggers | Yes | No |
| Credential scoping | Yes | No |
| Workspace binding | Yes | No |

A feature branch that needs its own agents, its own workspace, and a
trigger that fires on push → room. A single coding task within that
feature → thread.

---

## Room Architecture

### Standard rooms

`bureau matrix setup` creates six standard rooms, all children of the
`#bureau` space. Each has specific state event types, power levels, and
membership:

**`#bureau/system`** — Operational messages. No custom state event
types. Used for informational messages, daemon announcements, and
system-wide notifications.

**`#bureau/machine`** — Machine inventory and transport signaling.

- State events: `m.bureau.machine_key` (age public key per machine),
  `m.bureau.machine_info` (static hardware inventory: CPU, RAM, GPUs),
  `m.bureau.machine_status` (periodic heartbeat with resource metrics),
  `m.bureau.webrtc_offer` and `m.bureau.webrtc_answer` (SDP signaling
  for daemon-to-daemon connections)
- Power levels: any member can set machine events (these are
  self-reported by each machine's daemon)
- Members: all machine daemons

**`#bureau/service`** — Service directory. Every discoverable service
registers here.

- State events: `m.bureau.service` (one per service principal, keyed by
  localpart)
- Power levels: any member can register services
- Members: service principals, machine daemons

**`#bureau/template`** — Built-in sandbox templates.

- State events: `m.bureau.template` (one per template, keyed by template
  name)
- Power levels: admin-only writes (templates define security boundaries
  and must be tightly controlled)
- Members: admin, machine daemons (read-only)

**`#bureau/pipeline`** — Built-in pipeline definitions.

- State events: `m.bureau.pipeline` (one per pipeline, keyed by pipeline
  name)
- Power levels: admin-only writes (`events_default: 100`)
- Members: admin, machine daemons (read-only)

**`#bureau/artifact`** — Artifact service coordination.

- No per-artifact or per-tag state events (those live in the artifact
  service's own persistent store — see [artifacts.md](artifacts.md))
- Used for artifact service registration and fleet-level coordination
- Power levels: admin for room metadata
- Members: artifact service instances, machine daemons

All standard rooms are invite-only with admin-only invite power. This
means global rooms are daemon-only — sandbox principals cannot join
them. The proxy's MatrixPolicy default-deny blocks `JoinRoom`, and
even if a principal had join permission, no one would invite them.
Global infrastructure rooms are not for principals; they are for the
daemons that serve principals.

### Per-machine config rooms

Each machine gets a config room at `#bureau/config/<machine-localpart>`.
Created during machine bootstrap, these rooms hold the machine-specific
desired state:

- `m.bureau.machine_config` — which principals should run on this
  machine, with templates, policies, payloads, and start conditions
- `m.bureau.credentials` — age-encrypted credential bundles, one per
  principal assigned to this machine

The daemon also posts operational messages to the config room: sandbox
lifecycle notifications (start, exit with code, restart), service
directory updates, and command results. For non-zero sandbox exits,
the notification includes captured terminal output (last 50 lines of
the tmux pane). This is the only path for post-mortem error capture
from sandboxed processes — the config room audience (admin, machine
daemon, fleet controllers) is the correct set of parties because they
already control the sandbox's credentials and configuration. Captured
output is never posted to workspace rooms or any room where sandbox
principals are members.

Power levels: admin and machine daemon (100) for credentials, config,
room management, and fleet controller access grants. Fleet controllers
(50) for MachineConfig writes for service placement. Default (0) for
messages only.

The config room has `events_default: 100` and `state_default: 100` —
strict lockdown. Only the admin can set new state event types.

### Workspace rooms

Each workspace has a room, typically aliased as
`#<project>/<workspace>`. These rooms are the collaboration context for
principals working in the workspace:

- `m.bureau.project` — git repo URL, worktree definitions, directory
  structure
- `m.bureau.workspace` — lifecycle status
  (pending → active → teardown → archived | removed)
- `m.bureau.worktree` — individual git worktree lifecycle
  (creating → active → removing → archived | removed)
- `m.bureau.layout` — tmux session structure for observation
- `m.bureau.ticket` and `m.bureau.ticket_config` — when ticket
  management is enabled for the room
- `m.bureau.room_service` — service bindings (ticket, artifact, etc.)
- `m.bureau.pipeline_result` — pipeline execution outcomes

Power levels: admin (100) for room metadata. Machine daemon (50) for
invites. Members (0) for messages and state events.

The workspace room uses `state_default: 0` — any member can post new
state event types. This is the extensibility mechanism: new Bureau
features (tickets, artifacts, future services) work in workspace rooms
without requiring power level updates. Config rooms lock this down;
workspace rooms leave it open.

### Project-specific template and pipeline rooms

Projects can have their own template and pipeline rooms (e.g.,
`#iree/template`, `#iree/pipeline`). Templates in project rooms can
inherit from `#bureau/template` via the `Inherits` field. This allows
project-specific specialization while sharing base configurations.

---

## State Event Catalog

Bureau defines custom state event types (`m.bureau.*`), organized here
by function. Each entry lists: the event type, what the state key
represents, which room(s) it lives in, and what it carries. Note that
artifact metadata and tags are NOT state events — they live in the
artifact service's own persistent store (see Artifacts subsection
below).

### Machine infrastructure

| Event type | State key | Room | Purpose |
|---|---|---|---|
| `m.bureau.machine_key` | machine localpart | `#bureau/machine` | age public key for credential encryption |
| `m.bureau.machine_info` | machine localpart | `#bureau/machine` | static hardware inventory (CPU, RAM, GPUs, board) |
| `m.bureau.machine_status` | machine localpart | `#bureau/machine` | periodic heartbeat (CPU%, memory, GPU stats, uptime) |
| `m.bureau.credential_revocation` | machine localpart | `#bureau/machine` | fleet-wide notification of emergency credential revocation |

`machine_info` is published once at startup (or when hardware changes).
`machine_status` is published periodically. Both use the machine
localpart as state key, so each machine has exactly one of each.

### Transport signaling

| Event type | State key | Room | Purpose |
|---|---|---|---|
| `m.bureau.webrtc_offer` | `<offerer>\|<target>` | `#bureau/machine` | SDP offer with ICE candidates |
| `m.bureau.webrtc_answer` | `<offerer>\|<target>` | `#bureau/machine` | SDP answer with ICE candidates |

The pipe character (`|`) separates machine localparts in the state key.
It is not valid in Matrix localparts, so it unambiguously separates the
two identities. Vanilla ICE: all candidates are gathered before
publishing.

### Machine configuration

| Event type | State key | Room | Purpose |
|---|---|---|---|
| `m.bureau.machine_config` | machine localpart | `#bureau/config/<machine>` | principal assignments, policies, Bureau version |
| `m.bureau.credentials` | principal localpart | `#bureau/config/<machine>` | age-encrypted credential bundle per principal |

The config room is the daemon's source of truth. When the daemon syncs
and sees changes here, it reconciles — creating, destroying, or updating
sandboxes to match.

### Service discovery

| Event type | State key | Room | Purpose |
|---|---|---|---|
| `m.bureau.service` | service localpart | `#bureau/service` | service directory entry (principal, machine, capabilities, protocol) |
| `m.bureau.room_service` | service role name | any room | binds a service role to a concrete service principal |

`m.bureau.service` is the global registry — what services exist and
where they run. `m.bureau.room_service` is the local binding — which
service instance handles a role in this specific room. The daemon
resolves `RequiredServices` by joining these two: find the room
binding, look up the service, locate its socket.

### Templates and pipelines

| Event type | State key | Room | Purpose |
|---|---|---|---|
| `m.bureau.template` | template name | template rooms | sandbox specification (command, environment, mounts, services, etc.) |
| `m.bureau.pipeline` | pipeline name | pipeline rooms | step sequence (shell, publish, healthcheck, guard) |
| `m.bureau.pipeline_result` | pipeline name | pipeline's log room | execution outcome (success, failure, abort) |

Templates and pipelines are definitions — they describe what to build
or what to run. `pipeline_result` is the outcome of a pipeline
execution, published to the room specified in the pipeline's log
configuration. The ticket service watches `pipeline_result` events to
evaluate pipeline gates.

### Workspace lifecycle

| Event type | State key | Room | Purpose |
|---|---|---|---|
| `m.bureau.project` | project name | workspace room | git repo URL, worktree layout |
| `m.bureau.workspace` | `""` (singleton) | workspace room | lifecycle status (pending → active → teardown → archived) |
| `m.bureau.worktree` | worktree path | workspace room | per-worktree lifecycle status |

Workspace and worktree status transitions are the trigger mechanism for
principal lifecycle. Agent principals match `{"status": "active"}` via
StartCondition — they launch when setup completes and stop when
teardown begins. Teardown principals match `{"status": "teardown"}`.
The daemon re-evaluates conditions every reconcile cycle, so status
transitions drive principal lifecycle automatically.

### Observation

| Event type | State key | Room | Purpose |
|---|---|---|---|
| `m.bureau.layout` | principal localpart or `""` | workspace/config rooms | tmux session structure (windows, panes, role mapping) |

For principal-specific layouts, the state key is the principal's
localpart. For room-level layouts, the state key is empty. The daemon
reads these events to create and reconcile tmux sessions, and syncs
live changes back as state event updates.

### Tickets

| Event type | State key | Room | Purpose |
|---|---|---|---|
| `m.bureau.ticket` | ticket ID (e.g., `tkt-a3f9`) | any room with tickets enabled | work item: title, body, status, priority, gates, notes, attachments |
| `m.bureau.ticket_config` | `""` (singleton) | any room wanting tickets | enables and configures ticket management |

`ticket_config` is the opt-in mechanism: a room without it will not
accept ticket operations from the ticket service. Tickets are state
events, so querying "all open tickets in this room" is a state
read, not a timeline scan.

### Artifacts

Artifact metadata and tags do NOT live in Matrix. The artifact service
is a standalone CAS with its own persistent storage, indexes, and query
capabilities. At fleet scale, artifacts and tags number in the hundreds
of thousands to millions — far beyond what Matrix state events can
handle (every state event in a room is downloaded on initial sync).

The artifact service registers in `#bureau/service` like any other
service. Rooms that work with artifacts declare a scope:

| Event type | State key | Room | Purpose |
|---|---|---|---|
| `m.bureau.artifact_scope` | `""` (singleton) | any room | tag glob patterns this room's principals care about |

The scope event is configuration — "principals in this room work with
artifacts under these tag prefixes." The actual tag-to-ref mappings,
per-artifact metadata (hash, content type, size, labels), and query
indexes all live in the artifact service's local store and are served
over its socket. See [artifacts.md](artifacts.md) for the full data model, caching
architecture, and service API.

---

## Timeline Event Patterns

Bureau uses two custom message types (not state events, but
`m.room.message` events with custom `msgtype` fields):

**`m.bureau.command`** — a structured command from the CLI. The CLI
posts commands to workspace rooms or config rooms as regular messages
with structured fields (command name, parameters, target). The
human-readable `body` field allows the command to display legibly in
any Matrix client.

**`m.bureau.command_result`** — the daemon's reply. Posted as a
threaded reply to the original command message. Contains structured
result data alongside a human-readable body.

### Thread patterns

**Command-result threading**: the CLI posts a command message, the
daemon posts the result as a thread reply. The thread groups
request and response together without cluttering the room timeline.

**Pipeline step logging**: the pipeline executor creates a thread
at pipeline start (root message: "Pipeline X started (N steps)").
Each step logs as a thread reply with timing and output. The final
reply summarizes the overall result. The thread becomes the audit
trail for the pipeline run.

**Agent work sessions**: when a principal starts a work session, it
opens a thread in its room. Progress updates, questions, and results
are thread replies. When the session ends, a summary message in the
room (not in a thread — a top-level room message) captures the
outcome with a link to the thread.

---

## Power Level Model

Matrix power levels control who can do what within a room. Bureau
defines four power level structures for different room types, plus a
set of admin-protected events that are locked at power level 100 in
every room.

### Admin-protected events (all rooms)

These Matrix-native event types are always locked to admin (100):

- Room metadata: `m.room.avatar`, `m.room.canonical_alias`,
  `m.room.name`, `m.room.topic`
- Security: `m.room.encryption`, `m.room.history_visibility`,
  `m.room.join_rules`, `m.room.power_levels`, `m.room.server_acl`
- Lifecycle: `m.room.tombstone`
- Hierarchy: `m.space.child`

### Config room power levels

Strict lockdown. `events_default: 100`, `state_default: 100`.

- Admin (100): can set all state events, including `credentials` and
  `m.room.power_levels`. The admin is the only user at PL 100, keeping
  the trust boundary tight. Admin can kick machines (100 > 50) during
  decommission.
- Machine daemon and fleet controllers (50): can write `machine_config`
  for HA hosting and service placement, publish layouts, invite users,
  and send messages. Cannot write credentials, power levels, or room
  metadata. Fleet controllers are granted PL 50 by the admin during
  provisioning.
- Default (0): messages only.

No new state event types can be introduced without explicitly adding
them to the power level overrides. This is deliberate: config rooms
define security boundaries (credentials, policies, assignments), and
unexpected state events could confuse the daemon's reconciliation.

The config room membership model matters for security: the daemon posts
sandbox exit notifications (including captured terminal output for
non-zero exits) here. Command lines, error tracebacks, and environment
details in crash output can contain sensitive information. The config
room restricts this to parties who already have full access to the
sandbox's credentials and configuration — no sandbox principals are
ever invited to the config room.

### Workspace room power levels

Open for extensibility. `events_default: 0`, `state_default: 0`.

- Admin (100): room metadata, hierarchy, power levels
- Machine daemon (50): invites
- Members (0): can send messages and set any state event type

The `state_default: 0` setting is the extensibility mechanism. When a
new Bureau feature introduces a new state event type (tickets, artifact
scopes, future services), it works immediately in workspace rooms
without updating power levels. Config rooms lock this down because they define
security boundaries; workspace rooms leave it open because they are
collaboration contexts.

### Pipeline room power levels

Admin-only writes. `events_default: 100`.

Templates and pipelines define what code runs and how sandboxes are
configured — they are security-critical definitions. Only the admin can
publish or modify them.

### Artifact room power levels

Admin-only. The artifact room is a coordination space, not a data
store. Artifact metadata and tags live in the artifact service's own
persistent indexes, not as Matrix state events.

---

## Schema Versioning

Mutable state events that participate in read-modify-write cycles
include a `Version` field. This prevents silent data loss during
rolling upgrades where different service instances run different code
versions.

The pattern:

- Each event type constant defines the current schema version
  (e.g., `TicketContentVersion = 1`)
- Before modifying an event, callers invoke `CanModify()` which checks
  the event's version against the code's version
- If the event was written by newer code (`event.Version > code version`),
  modification is refused — the older code might silently drop fields
  it doesn't know about
- Reading is always safe: JSON unmarshaling ignores unknown fields, so
  older code can process newer events without loss
- Version is incremented when adding fields that older code must not
  silently drop during a write-back

This is forward-compatible reading with backward-compatible writing.
New fields added by newer code are preserved by newer code and ignored
(but not destroyed) by older code — as long as older code never writes
the event back.

---

## Sync and Reconciliation

### Daemon sync filter

The daemon's `/sync` connection uses a filter that restricts the
response to event types it cares about:

- **State section**: `machine_config`, `credentials`, `machine_status`,
  `service`, `layout`, `project`, `workspace`, `worktree`
- **Timeline section**: the same state types plus `m.room.message`
  (for command messages)
- **Excluded**: presence, account data, typing notifications, ephemeral
  events

State events can appear in both the state and timeline sections of a
sync response. During incremental syncs, state changes arrive as
timeline events with a non-nil `state_key`. The daemon treats any
change in a monitored room as a trigger to re-reconcile, regardless
of which section it appeared in.

A single sync filter covering all event types (rather than multiple
sync connections or per-event-type dispatch) works because the
daemon's handlers read current room state independently via
`GetStateEvent` and `GetRoomState`. The sync response is a
notification that something changed in a room, not the primary data
source. Handlers are idempotent — calling them redundantly is safe.

### Four monitored room categories

The daemon categorizes rooms by function:

1. **Config room** (`#bureau/config/<machine>`) — `machine_config` and
   `credentials` changes trigger full reconciliation (sandbox
   creation, destruction, updates)
2. **Machine room** (`#bureau/machine`) — `machine_status` changes
   update peer transport addresses
3. **Service room** (`#bureau/service`) — `service` changes update the
   local service directory cache
4. **Workspace rooms** (joined dynamically) — `workspace`, `project`,
   and `worktree` changes trigger re-reconciliation for principals with
   StartConditions that match those events

### Services run independent sync loops

Each Bureau service (ticket service, artifact service, future services)
runs its own `/sync` connection against the homeserver. Services use
`lib/service` infrastructure: `InitialSync` for the full state
snapshot, `RunSyncLoop` for incremental long-polling with backoff,
`AcceptInvites` for auto-joining rooms the service is invited to.

This means services are event-driven at the Matrix level. When a ticket
is updated, the ticket service's sync loop receives the state change
and updates its in-memory index. When a pipeline publishes a result,
the ticket service's sync loop receives it and evaluates pipeline gates
on dependent tickets.

### Invite handling

The daemon accepts invites automatically. It is invited to workspace
rooms by `bureau workspace create` and must join to read workspace
state events needed for StartCondition evaluation. Services similarly
accept invites to rooms they are asked to manage.

---

## Information Flow

Information flows upward through summaries, downward through links.
Each level of the hierarchy provides appropriate compression of what
happened below it.

### Thread → Room

When a work session or pipeline run completes (thread ends), a summary
is posted as a top-level room message (not in any thread):

> **auth-refactor session complete**: refactored auth handler (3 files,
> +180/-40). Tests green. [Session thread →]

The permalink points back to the thread root for drill-down. Room
participants see outcomes at the right granularity without being buried
in step-by-step detail.

### Room → Parent context

Significant milestones in a workspace room surface to team rooms,
either automated (a pipeline that fires on workspace state changes)
or agent-driven (a PM agent posting updates to a coordination room):

> **auth-refactor update**: implementation complete, in review.
> 3 files changed, tests passing. [Feature room →]

### Practical effect

Room-level participants see summaries with links. Thread-level
participants see the full conversation. The same data serves both
granularities without duplication — the summary is a message, and
the detail is a thread the summary points to.

---

## Principal Storage and Context

### Store — key-value state

Principals need durable key-value storage that survives sandbox
restarts: working state, configuration, learned preferences, cached
computations. The proxy provides this via state events in the
principal's rooms.

```
PUT  /bureau/store/{key}     → write a value
GET  /bureau/store/{key}     → read the current value
DELETE /bureau/store/{key}   → remove a value
GET  /bureau/store/          → list keys
```

Backed by `m.bureau.store` state events with the state key prefixed
by the principal's localpart:

```json
{
  "type": "m.bureau.store",
  "state_key": "iree/amdgpu/pm:working-state",
  "content": {
    "value": { ... }
  }
}
```

The proxy constructs the state key from its own identity — the sandbox
only specifies the key name. A principal cannot access another
principal's stored data through the Bureau API.

### Context — session history

Principals need append-only session records: what happened in previous
sessions, where work left off, what decisions were made. The proxy
provides this via messages in a designated thread.

```
POST /bureau/context         → archive current session summary
GET  /bureau/context         → retrieve latest session summary
GET  /bureau/context?count=5 → retrieve last 5 session summaries
```

The thread root is recorded as a state event
(`m.bureau.context_thread`, state key = principal localpart) so the
proxy can find it across sandbox restarts. Each session appends to the
same thread, building an append-only narrative.

On session start, the principal (or its adapter) reads the latest
context summary to reconstruct where it left off. This is resume
information — not the full conversation history (that lives in session
threads), but a structured summary the principal can act on.

### Privacy model

From inside the sandbox, `/bureau/store/` and `/bureau/context` access
only the calling principal's data. The proxy enforces this by
constructing state keys and thread lookups scoped to its identity.

But the data lives in Matrix rooms. State events and thread messages
are visible to all room members through the standard Matrix API. This
means:

- A principal's stored data is **private through the Bureau API**
- But **visible to any room member through Matrix directly**
- Sharing is a consequence of room membership, which the daemon manages

This is deliberate. Room membership is the sharing mechanism. If two
principals are in the same room, they can read each other's stored
state through Matrix. If they are in different rooms, they cannot.
No cross-principal storage API is needed — room topology provides
sharing for free.

---

## Artifact References

All blob data flows through the artifact service. Matrix is never used
as a blob store — no MXC uploads, no media repository, no inline binary
content in state events or messages. Matrix carries structured data
(JSON state events and text messages). Everything else is an artifact.

When a Matrix message needs to reference binary content (a build log,
a screenshot, a model checkpoint), it carries an artifact reference:

```json
{
  "msgtype": "m.text",
  "body": "Build log: art-a3f9b2c1e7d4",
  "m.bureau.artifact_ref": {
    "ref": "art-a3f9b2c1e7d4",
    "label": "build-and-test run 47",
    "content_type": "text/plain"
  }
}
```

The `art-<hash>` reference is opaque to Matrix — it's a string in a
message. Fetching the actual content requires the artifact service
socket. Authorization is two-layered: the principal must be in the room
to see the message containing the reference, and must have artifact
service access (via `RequiredServices`) to fetch the content.

Small structured data (ticket content, template specs, workspace
config) lives directly in Matrix state events. Large or binary data
lives in the artifact service and is referenced by hash. There is no
middle tier. See [artifacts.md](artifacts.md) for the full CAS design, caching
architecture, and service API.
