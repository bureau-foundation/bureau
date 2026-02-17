# Bureau Architecture

[fundamentals.md](fundamentals.md) defines the design principles and primitives.
This document defines the runtime contracts: what processes run, how they
communicate, what a sandbox sees, and how naming, configuration,
services, and lifecycle work in practice.

---

## Naming Convention

Bureau uses hierarchical names that map 1:1 across Matrix user IDs,
filesystem paths, room aliases, and permission globs. This alignment
is deliberate — it eliminates encoding, lookup tables, and translation
code. `ls` and shell globs become discovery and permission tools.

### Identity hierarchy

Matrix user IDs have the form `@localpart:domain`. The localpart allows
`a-z`, `0-9`, `.`, `_`, `=`, `-`, and `/`. Bureau uses `/` to create
hierarchical identities:

```
Operators:
  @ben:bureau.local

Machines:
  @machine/workstation:bureau.local
  @machine/pi-kitchen:bureau.local
  @machine/cloud-gpu-1:bureau.local

Project agents:
  @iree/amdgpu/pm:bureau.local
  @iree/amdgpu/codegen:bureau.local
  @iree/vulkan/test-runner:bureau.local

Infrastructure agents:
  @bureau/sysadmin:bureau.local
  @bureau/watchdog:bureau.local

Services:
  @service/stt/whisper:bureau.local
  @service/tts/piper:bureau.local
  @service/llm/local:bureau.local

Home/personal:
  @home/receptionist:bureau.local
  @home/kitchen:bureau.local
```

The hierarchy reflects organizational structure — which project, which
team, which role — not functional type. An STT service that serves many
projects lives under `service/stt/`, not under a specific project. The
functional type is metadata (display name, service registration). The
namespace reflects where the principal belongs.

### Filesystem mapping

The localpart maps directly to a socket path with no encoding or
escaping:

```
@iree/amdgpu/pm:bureau.local
  → /run/bureau/principal/iree/amdgpu/pm.sock

@service/stt/whisper:bureau.local
  → /run/bureau/principal/service/stt/whisper.sock
```

Standard unix tools work as discovery mechanisms:
`ls /run/bureau/principal/iree/` lists all IREE principals.
`ls /run/bureau/principal/iree/amdgpu/` narrows to the AMDGPU team.
Shell globs match permission patterns.

### Room aliases

Room aliases mirror the principal namespace:

```
#iree/amdgpu/general:bureau.local    — workstream chat
#iree/amdgpu/ci:bureau.local         — CI notifications
#bureau/service:bureau.local          — service directory
#bureau/template:bureau.local         — sandbox templates
#bureau/config/workstation:bureau.local — per-machine config
#home/kitchen:bureau.local            — kitchen automations
```

Creating a new project means creating a namespace: principal accounts,
rooms, permissions, and service directory entries, all rooted under the
same path prefix.

### Validation rules

Localparts are validated at registration time:

- **Non-empty.** A zero-length localpart is meaningless.
- **Maximum 80 characters.** Unix socket paths are limited to 108 bytes
  (`sun_path`). With the `/run/bureau/principal/` prefix (21 bytes) and
  `.sock` suffix (5 bytes), 80 bytes of localpart keeps the total under
  108 with 2 bytes of margin.
- **Lowercase only.** Matrix spec requires lowercase localparts. Bureau
  enforces this to prevent case-sensitivity mismatches between Matrix
  (case-folded) and the filesystem (case-sensitive).
- **No `..` segments.** A localpart containing `..` would allow path
  traversal when mapped to the filesystem. Splitting on `/` must not
  produce a `..` segment.
- **No leading `.` in any segment.** Segments starting with `.` create
  hidden files, invisible to `ls` without `-a`.
- **No empty segments.** Double slashes (`//`) are disallowed.
- **No leading or trailing `/`.** The localpart is a relative path, not
  absolute.
- **Character whitelist.** Only `a-z`, `0-9`, `.`, `_`, `=`, `-`, `/`.

These same rules apply to workspace names and worktree paths — the
validation is shared because the namespaces are shared.

---

## Runtime Topology

Each machine runs a launcher (privileged, minimal) and daemon
(unprivileged, network-facing), plus N sandbox instances with their
proxies. [fundamentals.md](fundamentals.md) explains why the architecture uses
three binaries (structural security via privilege separation, fault isolation,
independent updates). This section describes their responsibilities and
interactions.

```
┌──────────────────────────────────────────────────────────────────────┐
│  Machine: @machine/workstation:bureau.local                          │
│                                                                      │
│  ┌───────────────────────┐   ┌──────────────────────────────────┐    │
│  │  bureau-launcher      │   │  bureau-daemon                   │    │
│  │  (privileged, tiny)   │   │  (unprivileged, network-facing)  │    │
│  │                       │   │                                  │    │
│  │  Machine private key  │   │  Matrix sync connection          │    │
│  │  Namespace creation   │   │  WebRTC transport                │    │
│  │  Credential decrypt   │◄──│  Service discovery               │    │
│  │  Sandbox lifecycle    │   │  Lifecycle decisions             │    │
│  │                       │   │  Machine presence/stats          │    │
│  │  IPC only, no network │   │                                  │    │
│  └───────────┬───────────┘   └──────────────────────────────────┘    │
│              │                                                       │
│    ┌─────────┴───────────┬─────────────────────┐                     │
│    │                     │                     │                     │
│  ┌─▼────────────────┐  ┌─▼────────────────┐  ┌─▼──────────────────┐  │
│  │  Proxy           │  │  Proxy           │  │  Proxy             │  │
│  │  iree/amdgpu/pm  │  │  iree/amdgpu/dev │  │  service/tts/vibe  │  │
│  │                  │  │                  │  │                    │  │
│  │  Matrix token    │  │  Matrix token    │  │  Matrix token      │  │
│  │  Anthropic key   │  │  GitHub PAT      │  │  (no ext keys)     │  │
│  └────────┬─────────┘  └────────┬─────────┘  └────────┬───────────┘  │
│           │                     │                     │              │
│  ┌────────▼──────────┐  ┌───────▼────────┐   ┌────────▼───────────┐  │
│  │  Sandbox (bwrap)  │  │  Sandbox       │   │  Sandbox           │  │
│  │  LLM PM agent     │  │  Coding agent  │   │  Whisper STT       │  │
│  │                   │  │                │   │                    │  │
│  │  /run/bureau/     │  │  /run/bureau/  │   │  /run/bureau/      │  │
│  │    proxy.sock     │  │    proxy.sock  │   │    proxy.sock      │  │
│  │    payload.json   │  │    payload.json│   │    payload.json    │  │
│  │    service/       │  │    service/    │   │    service/        │  │
│  └───────────────────┘  └────────────────┘   └────────────────────┘  │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘
```

**bureau-launcher** (privileged, minimal, no network access):

- Holds the machine's age private key in the Linux kernel keyring
- Creates/destroys sandboxes (namespace creation requires
  CAP_SYS_ADMIN)
- Decrypts credential bundles and pipes plaintext to proxy stdin
- Validates every lifecycle request from the daemon
- Communicates via unix socket only (`/run/bureau/launcher.sock`)

**bureau-daemon** (unprivileged, network-facing):

- Matrix sync connection (config changes, messages, signaling)
- WebRTC transport (daemon-to-daemon connections)
- Service discovery (cache from Matrix, route to best instance)
- Lifecycle decisions (what to start/stop/reconfigure, forwarded to
  launcher)
- Machine presence and stats
- Observation routing (locate principal, stream terminal data)
- Cannot decrypt credentials, cannot create namespaces

**bureau-proxy** (one per sandbox, holds that sandbox's secrets):

- Reads credentials from stdin at startup, holds in memory only
- Creates `proxy.sock` (Matrix gateway + credential injection)
- Injects credentials into outbound requests
- Enforces MatrixPolicy and service visibility
- A sandbox escape reaches one proxy, compromising one principal's keys

**Fault isolation properties**: a proxy crash affects one sandbox. A
daemon crash doesn't kill running sandboxes — they and their proxies
keep running. The launcher restarts the daemon; running proxies are
unaffected. Proxy binaries can be cycled one at a time for
zero-downtime upgrades. The daemon can be updated without restarting
the launcher or any sandboxes.

---

## Sandbox Filesystem Contract

From inside the sandbox, the Bureau API is a set of files and sockets
under `/run/bureau/`:

```
/run/bureau/
├── proxy.sock                  Credential injection proxy (HTTP)
├── payload.json                Principle configuration (read-only)
├── trigger.json                Triggering event content (read-only, conditional)
└── service/
    ├── <role>.sock             One socket per required service
    ├── ...
    └── token/
        ├── <role>.token        Identity token per required service
        └── ...
```

Plus the workspace at `/workspace/` (writable git worktree, if the
template includes one) and inherited file descriptors (stdin/stdout/
stderr — how observation works).

### proxy.sock

HTTP server on a unix socket. The proxy injects the principal's Matrix
access token and credentials for external APIs. This is the only path
from inside the sandbox to the outside world.

The proxy restricts the Matrix API surface: principals cannot resolve
aliases, search the user directory, or access rooms they haven't been
invited to. MatrixPolicy (from the principal's assignment) controls
whether the principal can join rooms, invite others, or create rooms —
default-deny on all three.

### payload.json

Read-only JSON file describing what this principal should do. The
payload is the merge of the template's `DefaultPayload` with the
principal assignment's `Payload` (assignment values override template
defaults).

The structure is an arbitrary JSON object — there is no fixed schema.
By convention, top-level keys are feature namespaces and values are
configuration objects. The principle process reads this at startup to determine
its task, configuration, and context.

The daemon can update payload.json while the sandbox is running (via
the `update-payload` IPC action). The principle process receives SIGHUP when the
payload changes and can reload.

### trigger.json

Read-only JSON file containing the full content of the state event that
caused this principal to launch. Only present when the principal was
launched by a StartCondition match — omitted for principals that start
unconditionally.

The pipeline executor exposes top-level keys from trigger.json as
`EVENT_*` environment variables (e.g., a `status` field becomes
`EVENT_status`). This connects the triggering event to pipeline
variable substitution.

### service/

Unix sockets and identity tokens for Bureau services, grouped under the
service directory. Sockets live at the top level
(`/run/bureau/service/<role>.sock`) and identity tokens live in a `token/`
subdirectory (`/run/bureau/service/token/<role>.token`). The template's
`RequiredServices` list determines which entries appear.

Each token file contains raw bytes: a CBOR-encoded token payload with
the principal's identity and pre-resolved grants, followed by a
64-byte Ed25519 signature. The principal reads the appropriate token
file as opaque bytes before each service request and includes it in
the request. The service verifies the signature, checks expiry, and
uses the embedded grants for authorization.

The daemon refreshes tokens before they expire by writing new files to
the host-side service directory (directory-bind-mounted, so updates
appear immediately inside the sandbox).

Bureau-internal services use CBOR request-response. External service
sockets (proxied) use HTTP. The principle doesn't need to know which
category a service is in — both are sockets carrying their native protocol.

Service sockets are bind-mounted from the host. For local services
(same machine), the socket connects directly to the service principal's
socket. For remote services (different machine), the daemon creates a
local tunnel socket via the WebRTC transport. The principle never knows the
difference.

---

## Launcher IPC Protocol

The daemon communicates with the launcher over a unix socket at
`/run/bureau/launcher.sock`. The protocol is CBOR-encoded
request-response: one request per connection, one response, then close
(except for `wait-*` actions which block until the target exits).

### Actions

- **create-sandbox** — create a new sandbox with proxy. Takes the
  principal localpart, sandbox spec (resolved from template), encrypted
  or direct credentials, MatrixPolicy, service mounts, payload, and
  optional trigger content. Returns the proxy PID.
- **destroy-sandbox** — tear down a sandbox. Kills the tmux session and
  proxy, cleans up the sandbox filesystem.
- **update-payload** — rewrite `/run/bureau/payload.json` inside a
  running sandbox. The principle receives SIGHUP to trigger reload.
- **list-sandboxes** — return all running sandboxes. Used by the daemon
  on restart to adopt pre-existing sandboxes.
- **wait-sandbox** — block until a sandbox's tmux session exits. Returns
  the exit code and, for non-zero exits, captured terminal output from
  the tmux pane (last 500 lines). Long-lived connection.
- **wait-proxy** — block until a sandbox's proxy process exits. Returns
  the exit code. Long-lived connection.
- **update-proxy-binary** — validate a new proxy binary and use it for
  future sandbox creation.
- **exec-update** — the launcher validates a new binary, writes state,
  and calls `exec()` to replace itself. Used for self-update.
- **status** — returns the launcher's binary hash and current proxy
  binary path.

### Timeouts

Quick actions (create, destroy, update, status, list) use a 30-second
deadline set per-connection by the launcher. Wait operations have no
read deadline — the connection stays open until the target process
exits or the context is cancelled.

### Sandbox lifecycle boundary

The tmux session is the lifecycle boundary for a sandbox, not the
command running inside it or the proxy. The tmux server is started with
`remain-on-exit on` so that panes survive process exit — this allows
the launcher to capture terminal output via `capture-pane` before
destroying the session. When the sandboxed process exits, the launcher
detects it via the exit-code file (polling every 250ms), captures the
pane content for non-zero exits, kills the tmux session, cleans up
the proxy, and returns the exit code and captured output to the
daemon's `wait-sandbox` call.

The daemon posts sandbox exit notifications to the config room. For
non-zero exits, the notification includes the last 50 lines of
captured terminal output (the full 500-line capture is in the daemon's
structured log). The config room is the correct destination because its
membership is restricted to the admin, machine daemon, and fleet
controllers — parties who already have access to the sandbox's
credentials and configuration. Captured output is never posted to
workspace rooms or any room where sandbox principals are members.

---

## Templates and Configuration

### Templates

Templates define the shape of a principal's sandbox. They are Matrix
state events (`m.bureau.template`) published to template rooms (e.g.,
`#bureau/template`).

A template specifies:

- **Command**: the entrypoint command and arguments
- **Environment**: Nix store path providing the execution environment
- **EnvironmentVariables**: key-value pairs with `${VAR}` expansion
- **Filesystem**: mount points (source, dest, mode, type, optional)
- **Namespaces**: which Linux namespaces to create
- **Resources**: cgroup limits (memory, CPU, tasks)
- **Security**: bwrap security options
- **CreateDirs**: directories to create inside the sandbox
- **Roles**: named command sets for tmux layout (e.g., main pane runs
  the principle, side pane runs a tool)
- **RequiredCredentials**: credential names required at launch
- **RequiredServices**: service roles that must be resolved and mounted
- **DefaultPayload**: default principle configuration (overridden per
  instance)
- **HealthCheck**: optional health monitoring configuration

### Template inheritance

Templates can inherit from a parent via the `Inherits` field (formatted
as `room-alias:template-name`). The daemon walks the full inheritance
chain and merges fields:

- **Slices** (Filesystem, CreateDirs) append — child entries come after
  parent entries.
- **Maps** (EnvironmentVariables, Roles, DefaultPayload) merge — child
  values override parent values on key conflict.
- **Scalars** (Command, Environment) replace — non-zero child value
  replaces parent.

This allows a base template to define common sandbox configuration
(Nix environment, standard mounts, cgroup limits) while child templates
add project-specific or role-specific overrides.

### Principal assignments

Principal assignments declare what should run on a specific machine.
They are published as `m.bureau.machine_config` state events in
per-machine config rooms (`#bureau/config/<machine>`).

A `MachineConfig` event contains:

- **Principals**: list of `PrincipalAssignment` entries
- **DefaultObservePolicy**: fallback observation policy for principals
  without an explicit one
- **BureauVersion**: desired versions for daemon, launcher, and proxy
  binaries

Each `PrincipalAssignment` specifies:

- **Localpart**: the principal's validated localpart
- **Template**: reference to a template (`room-alias:template-name`)
- **AutoStart**: whether to launch at daemon boot
- **Labels**: free-form metadata (role, team, tier)
- **MatrixPolicy**: per-principal policy for Matrix operations
- **ObservePolicy**: per-principal observation access control
- **ServiceVisibility**: glob patterns filtering which external services
  are available
- **CommandOverride**: replace the template's command for this instance
- **EnvironmentOverride**: replace the template's Nix environment
- **ExtraEnvironmentVariables**: merged over template variables
- **Payload**: instance-specific payload (merged over template defaults)
- **StartCondition**: gate launch on a specific state event match

The daemon reconciles these assignments against running state on every
sync tick: creating sandboxes for new assignments, tearing down removed
ones, and updating configuration for changed ones.

---

## Service Communication

Bureau has three communication tiers, each with a distinct role:

- **Proxy** — credential injection, external API access, Matrix
  operations. Control-plane only. The proxy holds secrets and injects
  them into outbound requests. Agents use it for Matrix messaging,
  external API calls, and anything that requires credentials the agent
  must not see in plaintext.
- **Matrix** — logged operations, event-driven coordination, persistent
  state. Everything that needs an audit trail or must survive process
  restarts flows through Matrix state events and timeline messages.
  Configuration, service registration, ticket state, pipeline results.
- **Direct sockets** — high-frequency, low-latency agent-to-service
  and service-to-service communication. Ticket queries, embeddings
  lookups, artifact fetches — anything chatty that doesn't need
  credential injection or per-call audit logging.

Nothing running within the Bureau network uses the proxy for service
communication. The proxy is for credentials and the control plane.
Bureau-native services are accessed via Unix sockets bind-mounted
directly into sandboxes (see the sandbox filesystem contract above).

The proxy routes service *discovery* (GET /v1/services) and external
API calls. Direct sockets carry the data. Matrix carries the state.

---

## Service Discovery

### Registration

Services register in `#bureau/service` via `m.bureau.service` state
events. The state key is the principal's localpart:

```json
{
  "type": "m.bureau.service",
  "state_key": "service/stt/whisper",
  "content": {
    "principal": "@service/stt/whisper:bureau.local",
    "machine": "@machine/cloud-gpu-1:bureau.local",
    "capabilities": ["streaming", "speaker-diarization"],
    "protocol": "http",
    "description": "Whisper Large V3 streaming STT"
  }
}
```

Each daemon syncs this room and maintains a local cache of available
services. Multiple replicas are natural: three machines each run a
whisper-stt sandbox, each registers in the service room. The daemon
picks the closest instance based on latency, load, and locality.

### Room binding

Room-level service bindings declare which service instance handles each
role in each room, via `m.bureau.room_service` state events:

```json
{
  "type": "m.bureau.room_service",
  "state_key": "ticket",
  "content": {
    "principal": "@service/ticket:bureau.local"
  }
}
```

The state key is the service role name (e.g., `ticket`, `artifact`,
`rag`). This decouples service consumers (who refer to roles) from
service instances (who register by principal name).

### Resolution flow

At sandbox creation time, the daemon resolves each entry in the
template's `RequiredServices` list:

1. Scan the principal's rooms for `m.bureau.room_service` state events
   matching the required role.
2. Look up the bound principal in the `#bureau/service` directory to
   find which machine hosts it.
3. For local services (same machine): use the service principal's socket
   path directly (`/run/bureau/principal/<localpart>.sock`).
4. For remote services (different machine): create a local tunnel socket
   via the WebRTC transport.
5. Pass all resolved sockets to the launcher as `ServiceMount` entries
   (role name + host-side socket path).
6. The launcher bind-mounts each socket into the sandbox at
   `/run/bureau/service/<role>.sock`.

If any required service can't be resolved, sandbox creation fails — no
silent degradation.

### Service implementation pattern

Bureau services share a common startup sequence:

1. Load Matrix session from `session.json` (written by launcher)
2. Validate session (WhoAmI)
3. Resolve and join `#bureau/service`
4. Publish `m.bureau.service` state event
5. Perform initial `/sync` (full state snapshot for joined rooms)
6. Build service-specific in-memory indexes from the sync response
7. Start the unix socket server (CBOR action dispatch)
8. Start the incremental `/sync` loop
9. On shutdown: deregister from `#bureau/service`, drain connections

A shared service infrastructure library provides session management,
registration, sync loops, invite handling, and socket serving. Services
compose these building blocks and add domain-specific logic: index
structures, action handlers, event processing.

### Service wire protocol

Service sockets use CBOR with one connection per
request-response cycle:

1. Client connects to the Unix socket
2. Client writes a single CBOR item. CBOR is self-delimiting — each
   item's length is encoded in its header, so no framing protocol is
   needed beyond what CBOR provides. The request includes an `action`
   field (string identifying the operation) and a `token` field (raw
   bytes from `/run/bureau/service/token/<role>.token`, for authenticated operations)
3. Server decodes the request, dispatches on `action`, verifies the
   token if the action requires authentication
4. Server writes a single CBOR item as the response
5. Connection closes

Responses use a flat envelope: `{ok: true, ...result}` on success,
`{ok: false, error: "..."}` on failure. When the handler returns a map,
its entries are merged directly into the response alongside `ok: true`
— no nesting. Non-map results (arrays, scalars) are wrapped in a
`data` field.

Protocol constraints:

- 30-second read timeout (client must send promptly after connecting)
- 10-second write timeout (server must respond promptly)
- 1 MB maximum request size

CBOR's value for Bureau is three properties: self-delimiting messages
(each item's length is in its header, so no framing protocol is needed
beyond the encoding itself), deterministic encoding (sorted keys and
canonical integer representation give a single valid byte sequence for
any value, which is required when token payloads are signed), and
native binary data (service identity tokens, Ed25519 signatures, and
content hashes are raw bytes, not base64-encoded strings inside JSON).

JSON would work for the message structure but forces base64 encoding
for every binary field — adding encoding overhead and a class of bugs
(wrong base64 variant, padding errors, double-encoding). Protocol
Buffers require schema compilation and code generation, making the wire
format opaque to inspection and the build more complex. MessagePack
lacks a deterministic encoding mode, which is required when token
payloads must serialize identically on every machine for signature
verification.

CBOR is Bureau's standard internal serialization, configured for Core
Deterministic Encoding: sorted keys, smallest integer encoding, no
indefinite-length items. JSON is used for external interfaces: the
Matrix Client-Server API, proxy HTTP endpoints, and the sandbox
filesystem contract (payload.json, trigger.json, identity.json). The
boundary is clear — JSON crosses network boundaries and enters the
sandbox filesystem, CBOR stays inside Bureau's internal protocols.

---

## Machine Bootstrap and Lifecycle

### Initial setup

A machine joins Bureau through the launcher:

```bash
bureau-launcher \
    --homeserver https://matrix.bureau.local \
    --registration-token-file /path/to/token \
    --machine-name machine/cloud-gpu-1
```

The registration token is read from a file, never from a CLI argument
(secrets must not appear in `/proc/*/cmdline`). The launcher:

- Generates the machine's age keypair
- Registers `@machine/cloud-gpu-1:bureau.local` (or logs in if the
  account exists)
- Publishes the machine's public key to `#bureau/machine`
- Starts the unprivileged bureau-daemon

This pattern works for cloud VMs (spin up, launch, use, shut down) and
permanent infrastructure alike.

### Daemon startup sequence

The daemon starts with a defined sequence:

1. Load Matrix session from `session.json` (written by launcher)
2. Validate session (WhoAmI)
3. Ensure per-machine config room exists
4. Join global rooms (`#bureau/machine`, `#bureau/service`)
5. Check daemon watchdog (detect prior exec-update success/failure)
6. Publish machine info (static hardware inventory: CPU, RAM, GPUs)
7. Start WebRTC transport
8. Start observation socket listener
9. Adopt pre-existing sandboxes (query launcher for running processes
   from a previous daemon instance — prevents duplicate creation)
10. Perform initial sync (full state snapshot, first reconciliation)
11. Start incremental sync loop (long-poll with reconciliation)
12. Start status loop (periodic heartbeat publish)

### Steady state

The daemon maintains a persistent Matrix sync connection. On each sync
response, it reconciles desired state (Matrix) against actual state
(running processes):

- New principal assignments → create sandboxes
- Removed assignments → destroy sandboxes
- Changed templates/payloads/policies → update or recreate affected
  sandboxes
- StartCondition matches → launch gated principals with trigger content
- Service binding changes → resolve and mount updated sockets

### Shutdown

Normal shutdown is via SIGTERM or SIGINT — the daemon destroys all
running sandboxes, publishes a final status update, and exits cleanly.

**Emergency shutdown** triggers when the daemon detects that its Matrix
account has been deactivated or its access tokens invalidated. The sync
loop receives `M_UNKNOWN_TOKEN` from `/sync`, at which point the daemon:

1. Logs the auth failure at ERROR level
2. Acquires the reconciliation lock
3. Destroys every running sandbox via launcher IPC (best-effort — IPC
   failures are logged but don't stop the shutdown)
4. Cancels the daemon's top-level context, unblocking the main run loop
5. Exits

This is Layer 1 of the emergency credential revocation defense (see
credentials.md). No cooperation from the compromised machine is needed —
deactivating its Matrix account is sufficient to trigger sandbox
destruction and daemon exit within one sync cycle.

All local state is ephemeral: sandbox rootfs is built from Nix,
persistent data lives in Matrix, credentials are in-memory only. A
machine can be wiped and nothing is lost.

### Machine presence

The daemon publishes periodic status events to `#bureau/machine`:

```json
{
  "type": "m.bureau.machine_status",
  "state_key": "machine/cloud-gpu-1",
  "content": {
    "principal": "@machine/cloud-gpu-1:bureau.local",
    "cpu_percent": 42,
    "memory_used_mb": 12300,
    "gpu_stats": [
      {
        "pci_slot": "0000:01:00.0",
        "utilization_percent": 87,
        "vram_used_bytes": 8589934592,
        "temperature_millidegrees": 55000,
        "power_draw_watts": 250
      }
    ],
    "sandboxes": {"running": 5, "idle": 2},
    "uptime_seconds": 86400,
    "last_activity_at": "2026-02-12T14:30:22Z",
    "transport_address": "@machine/cloud-gpu-1:bureau.local"
  }
}
```

Static hardware inventory (CPU topology, RAM, GPU models, board
identity) is published once at startup as a separate
`m.bureau.machine_info` event, updated only if hardware changes.

---

## Machine-Level Cache

`/var/bureau/cache/` is the machine-level tool and model cache. It
holds infrastructure that principals use: installed tool binaries,
model weights, and package manager caches. It is separate from
workspace storage (workspaces have their own lifecycle).

```
/var/bureau/cache/
  npm/              npm global cache
  pip/              pip package cache
  bin/              installed tool binaries (claude, codex, etc.)
  hf/               HuggingFace model cache
  go/               Go module cache
  nix/              Nix store subset for principles needing nix-built tools
```

### Access tiers

Templates reference the cache using the `${CACHE_ROOT}` variable.
Three tiers control access:

**Tier 1 — sysadmin (read-write).** The sysadmin principal's template
mounts `${CACHE_ROOT}` at `/cache` with mode `rw`. It can install
tools, download models, and populate package caches. This is the only
principal that modifies the cache.

**Tier 2 — cache-backed (read-only).** Principle templates mount specific
subdirectories read-only. An principle can use cached tools but not modify
them. Mounts are optional so missing subdirectories don't block sandbox
creation.

```json
{"source": "${CACHE_ROOT}/bin", "dest": "/usr/local/bin", "mode": "ro", "optional": true}
```

When a principle needs a tool or model that isn't cached, it messages the
sysadmin in a Matrix room. The sysadmin installs it into the read-write
cache. The principle sees it appear in its read-only mount.

**Tier 3 — hermetic (no cache mount).** Templates that need complete
reproducibility omit cache mounts entirely. Everything comes from the
Nix closure.

### Lifecycle

- Created by the launcher at boot (top-level directory only)
- Subdirectories created by the sysadmin or curator principals
- Persists across sandbox restarts (warm cache is the purpose)
- Not replicated across machines (each machine has its own; use the
  artifact system for cross-machine distribution)
- Not backed up with workspace data (separate lifecycle)
