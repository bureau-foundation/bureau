# Bureau Fundamentals

Bureau is infrastructure for running organizations of principals —
agents, services, operators, and machines — with isolation, identity,
communication, observation, and persistence.

---

## Why Bureau Exists

The last fifty years of computing infrastructure solved a problem: how
do you run many programs on shared hardware safely? Unix gave us
processes, file permissions, pipes, and signals. Containers gave us
namespace isolation and resource limits. Orchestrators gave us
scheduling, service discovery, and rolling deployments. None of these
told you what programs to write — they made it possible to run any
program safely and compose programs freely.

Bureau solves the same problem for a world where many of your processes
are autonomous AI agents. Agents need isolation (a compromised agent
can't access other agents' data or credentials). They need identity (so
they can be addressed, authorized, and audited). They need communication
(structured messaging that persists across restarts and spans machines).
They need observation (a human or another agent can watch what's
happening in real-time). And they need persistence (nothing is lost when
a process ends or a machine reboots).

These aren't agent-specific needs — they're the same needs any
multi-process system has. Bureau's position is that agents don't need
agent infrastructure. They need general infrastructure designed for a
world where processes are autonomous and untrusted. A ticket service, a
webhook relay, a human operator's shell, and an LLM coding agent are
all the same thing to Bureau: principals with identities, in sandboxes
with proxies, communicating through rooms, observable in real-time.

This document defines Bureau's design principles, architectural
primitives, and the higher-order constructs built from them. Deep-dive
documents cover full technical detail for each subsystem; this document
is the complete conceptual model — the "why" and "what" together.

---

## What Bureau Is Not

**Bureau is not an agent framework.** Frameworks like CrewAI, AutoGen,
and LangGraph are libraries that orchestrate LLM calls within a single
process, on a single machine, with shared memory and no security
boundaries. Bureau doesn't orchestrate LLM calls — it provides the
substrate that agent frameworks run on top of. A CrewAI agent can run
inside a Bureau sandbox. So can a raw Python script or a shell script
with curl.

**Bureau is not a personal assistant.** Products like OpenClaw give one
LLM access to your filesystem, browser, and messaging apps through a
single process with all credentials available simultaneously. Bureau
doesn't talk to you through WhatsApp. It runs the sandboxed process that
does — with only the credentials that process needs, observable in
real-time, auditable after the fact, and killable without affecting
anything else.

**Bureau is not a workflow engine.** Temporal, n8n, and Airflow execute
predefined sequences of steps. Bureau's pipelines can do that, but the
interesting part is what happens between the steps — autonomous agents
making decisions, communicating with each other and with humans, using
services, and creating new work. The pipeline executor is a small piece;
the organizational substrate is the point.

**Bureau is not tied to any specific LLM or agent runtime.** A Bureau
agent is any process that can make HTTP requests to a Unix socket.
Claude Code, Codex, Gemini, a custom Python script, or a shell script
with curl — they're all first-class participants. The proxy doesn't
know or care what's behind the HTTP requests.

---

## Design Principles

These principles constrain every design decision in Bureau. They are
invariants, not aspirations. A change that violates any of these is
wrong, regardless of what else it accomplishes.

### Security is structural, not behavioral

Every other agent system relies on one of two security models: either
the LLM is trusted not to misuse its access, or the system is limited
to read-only and low-risk operations. Bureau's security model is
enforced by the kernel, not by a prompt.

Sandboxes use Linux namespaces. A sandboxed process cannot see host
processes, reach the network, or access files outside its constructed
filesystem. This is enforced by the kernel, not by convention.

Proxies hold credentials. The sandboxed process has no API keys, tokens,
or secrets — the parent environment is explicitly cleared before the
sandbox starts. A compromised agent can only use the credentials its
proxy holds, and each proxy holds credentials for exactly one principal.

The launcher (privileged, creates namespaces, decrypts credentials) has
no network access after startup. The daemon (unprivileged,
network-facing, makes lifecycle decisions) cannot create namespaces or
decrypt credentials. An attacker who compromises either one gets a
process that cannot do what the other can do.

This is the same defense-in-depth architecture that container
orchestrators use for untrusted workloads, applied to AI agents.

### Everything is a principal

Bureau does not distinguish between "agents," "services," "tools," and
"humans" at the infrastructure level. They are all principals — entities
with identities, credentials, room memberships, and observation
policies. An LLM coding agent, a Whisper STT service, a webhook relay,
a Forgejo instance, and a human operator are all principals. They all
communicate through Matrix rooms, follow the same credential and
permission models, and can be observed through the same mechanism.

This matters because real organizations aren't just agents talking to
agents. They're agents using services, services calling other services,
humans observing agents, agents escalating to humans, and all of these
communicating through shared channels with persistent history. A single
abstraction means one permission system, one naming scheme, one
observation mechanism, and one communication layer — not parallel
subsystems for each principal type.

### The sandbox filesystem is the API

A Bureau principal is any process that can open a unix socket and read a
JSON file. No SDK, no special libraries, no agent framework required.
`curl` and `jq` are sufficient. The proxy socket, the payload file, the
service sockets, the token files — they're all filesystem objects.

This is the universality guarantee. Bureau works with any agent runtime
that exists or will exist, with zero integration effort. If it can do
HTTP to localhost, it's a Bureau agent.

### Multi-machine is native

The daemon mesh is the native topology, and a single machine is a
degenerate case — not the other way around. Matrix is the coordination
layer because it works across machines. The transport layer makes remote
services look like local sockets. A sandbox doesn't know whether its
service socket connects to a local process or a tunnel across the
internet.

This isn't "we added clustering." The architecture assumes distribution
and works naturally on a single machine as a special case. There is no
single-machine mode that breaks when you add a second machine.

### Matrix is the source of truth for everything persistent

Identity, configuration, service directory, templates, conversation
history, machine presence, ticket state, artifact tags — if it needs to
survive a reboot, it lives in Matrix. The homeserver (Continuwuity,
running locally) is the single source of truth. There are no local
databases, no config files that aren't synced from Matrix, no persistent
state that lives only in memory or on local disk.

### Bureau can manage itself

Agents can administer Bureau infrastructure through the same primitives
they use for any other work. Pipelines provision workspaces and deploy
services. A sysadmin agent manages machines through Matrix commands. The
daemon reconciles configuration changes from Matrix state events.

The trust boundary is clear: the daemon makes lifecycle *decisions*
(what to start, what to stop, what to reconfigure). The launcher
*executes* them (create namespace, decrypt credentials, spawn proxy).
Agents can request operations through messaging. The daemon validates
requests against policy. The launcher enforces sandbox boundaries. No
single component has unchecked authority.

---

## Principals

A principal is anything with an identity in Bureau: an AI coding agent,
a ticket service, a human operator's shell, a machine daemon, a CI
pipeline runner, a Forgejo instance. Every principal:

- Has a Matrix account (`@iree/amdgpu/pm:bureau.local`)
- Has a unix socket path derived from its name
  (`/run/bureau/principal/iree/amdgpu/pm.sock`)
- Can be a member of rooms (scoping what it can see and do)
- Can publish and receive state events
- Can be observed (live terminal access)

Names are hierarchical, using `/` as a separator. They map directly to
filesystem paths (for socket locations), Matrix localparts (for
identity), and room aliases (for organizational structure). This
alignment across identity, filesystem, rooms, and permissions eliminates
an entire class of translation bugs — `ls` and shell globs become
discovery and permission tools. The full naming convention is documented
in [architecture.md](architecture.md).

Common principal types:

- **Agents** run in sandboxes, talk through proxies, do autonomous work.
  A coding agent, a review agent, a PM agent are all principals with
  different templates.
- **Services** are principals that listen on sockets. A Bureau-native
  ticket service written in Go, a Forgejo instance speaking HTTP, a
  Python ML inference server — all are services. Bureau mounts their
  sockets into sandboxes that need them. The service's protocol is
  between it and its consumers; Bureau provides discovery, mounting,
  and caller authentication (see [authorization.md](authorization.md)).
- **Operators** are human users. An operator's shell session is a
  principal with elevated permissions, not a special case.
- **Machines** have identities for heartbeats, fleet membership, and
  cross-machine service routing.

---

## The Five Primitives

Everything Bureau does is a composition of five primitives. An "agent"
is a process running inside a sandbox, talking to the world through a
proxy (via a bridge), observable through a terminal session, and
coordinating with others through messaging.

### Sandbox

The sandbox isolates untrusted code from the host. The trust model is
kernel-enforced: Linux namespaces (via bubblewrap) provide PID, mount,
network, IPC, and UTS isolation. Resource limits (memory, CPU, task
count) are enforced via cgroups.

A sandboxed process:

- Cannot see or signal host processes (PID namespace)
- Sees a constructed filesystem, not the host (mount namespace)
- Has no external network access (network namespace)
- Cannot exceed its resource allocation (cgroup limits)

What the sandbox deliberately exposes:

- `/run/bureau/proxy.sock` — the proxy, for all external communication
- `/run/bureau/payload.json` — configuration: what this principal is and
  what it should do
- `/run/bureau/trigger.json` — the event that started this principal
  (when launched by a StartCondition)
- `/run/bureau/tokens/<role>` — service identity tokens for
  authenticating to Bureau services
- `/run/bureau/service/<role>.sock` — direct sockets to Bureau services
- `/workspace/` — a writable git worktree (for principals that need one)
- Inherited file descriptors (stdin/stdout/stderr) — this is how
  observation works; the PTY crosses the namespace boundary via fd
  inheritance, not filesystem access

The sandbox filesystem is the entire Bureau API surface. No SDK is
required. Any process that can read a file and open a socket is a
Bureau principal.

### Proxy

The proxy is the only path from inside a sandbox to the outside world.
It provides two things: credential injection and request mediation.

Inside the sandbox, the principal has no API keys, tokens, or
credentials of any kind. The parent environment is explicitly cleared
before bubblewrap executes. When the principal needs to reach an
external service or communicate through Matrix, it makes an HTTP
request to the proxy socket:

- **Credential injection**: the proxy holds credentials and injects them
  into outgoing requests. The principal never sees API keys.
- **Matrix operations**: `/v1/matrix/*` endpoints for state events,
  messages, room resolution, identity queries.
- **External API access**: `/v1/proxy` for forwarding to configured
  external services (Anthropic, GitHub, etc.) with credentials injected.
- **Service directory**: `/v1/services` for discovering available
  services.
- **HTTP proxying**: `/http/<service>/` for proxying to service HTTP
  endpoints with credentials injected.

The proxy also enforces policy. MatrixPolicy controls whether a
principal can join rooms, invite others, or create rooms (default-deny
on all three). Service visibility controls which external services are
available to each principal.

Credentials never enter the sandbox. They live in the proxy process,
loaded from encrypted bundles that the launcher decrypts at sandbox
creation time. A compromised agent can use the credentials its proxy
holds, and no others — this is the credential isolation guarantee.

Bureau uses age encryption for credential bundles: simple file-based
encryption with multiple recipients, no key server, no protocol state.
A credential bundle encrypted to three machine public keys is three
recipient stanzas and a payload — the format is small enough to audit
by reading the source. The launcher decrypts with the machine's age
private key (held in the kernel keyring); the plaintext never touches
disk.

The alternatives carry operational weight Bureau doesn't need. HSMs and
PKCS#11 add hardware dependencies and configuration surface that would
be required on every machine in the fleet. GPG's key management and
format complexity are well-documented liabilities. age is stateless and
composable: encrypt to any set of X25519 public keys, decrypt with the
corresponding private key, done. TPM binding is a complementary
enhancement for hardware-backed key protection, not a replacement — age
provides the encryption envelope, TPM protects the private key that
opens it.

See [credentials.md](credentials.md) for the encryption and provisioning model.

### Bridge

The bridge is a TCP-to-Unix-socket forwarder that runs inside the
sandbox. It listens on `127.0.0.1:8642` and forwards to the proxy
socket.

It exists because many agent runtimes — Claude Code, Codex, and most
tools that make HTTP requests — can only target URLs, not Unix sockets.
The bridge translates. Network isolation in the sandbox blocks all
external connections, but localhost within the sandbox is fine, and the
bridge's Unix socket connection to the proxy crosses the namespace
boundary via the bind-mounted socket file.

The bridge is what makes "any process that can HTTP to localhost is a
Bureau agent" true. Without it, only processes that can open Unix
sockets directly would be first-class participants.

### Observation

Observation is live bidirectional terminal access to any running
principal. It is a first-class primitive, not a monitoring afterthought.

The core mechanism: tmux runs outside the sandbox. The PTY crosses the
namespace boundary via file descriptor inheritance, not filesystem
access. The sandboxed process doesn't know it's being observed — it
reads and writes stdin/stdout/stderr, which happen to be connected to a
PTY whose master end is held by tmux on the host.

When a process is launched inside a tmux session, tmux allocates a PTY
pair (master fd + slave fd). The child process inherits the slave fd on
stdin/stdout/stderr, then bubblewrap creates new namespaces, and the
agent command inherits the same file descriptors. File descriptors are
kernel-level references, not filesystem paths — they survive namespace
transitions.

This means:

- No tmux binary or socket inside the sandbox — not needed, not
  accessible.
- Zero additional attack surface — the sandboxed process has exactly
  what any terminal process has: three file descriptors.
- Multiple simultaneous observers — tmux supports this natively.
- SIGWINCH (window resize) works across the PID namespace boundary
  because the kernel tracks the foreground process group by kernel
  struct, not by PID number.

Observation and messaging serve different purposes and must not be
conflated. Observation is synchronous, interactive, real-time — a human
(or agent) looking through glass at a running process. Messaging is
asynchronous, persistent, structured — communication between principals
that survives restarts and spans machines. An agent does not need
observation to receive a Matrix message. A human does not need Matrix to
type into an agent's terminal.

The daemon routes observation connections, locating the target principal
(local or remote) and streaming terminal data back to the observer. See
[observation.md](observation.md) for the relay architecture, wire protocol, ring buffer,
and layout system.

### Messaging

Messaging is how principals communicate persistently and asynchronously.
All structured communication between principals — and all persistent
state — flows through Matrix.

Bureau needs a persistence layer that is also an event bus, also a
permission system, and also human-readable. Matrix provides all four.
Rooms scope access — a principal that isn't in a room can't see its
state. State events are typed, versioned, and queryable — they're the
data model, not a message queue hack. The `/sync` long-poll gives every
consumer an event stream without a separate pub-sub system. And humans
can join any room with a standard client and read the conversation
directly — the audit trail is just chat history.

The alternative is building this from components: a database for state,
a message broker for events, an ACL system for permissions, and a
separate audit log. Matrix is all of these in one protocol, with
federation for multi-machine and end-to-end encryption as a future
option. Bureau doesn't use Matrix because it's a chat server — it uses
Matrix because it's a replicated, permissioned, event-driven state
store that happens to also be a chat server.

Matrix is Bureau's persistence layer, event bus, and coordination
mechanism. The homeserver (Continuwuity, running locally) is the single
source of truth:

- **Persistence**: messages and state survive process restarts, machine
  reboots, and network partitions.
- **Distribution**: works across machines without special plumbing.
  Agents on machine A can message agents on machine B.
- **Observability**: humans can join any room and read the conversation.
  The chat log is the audit trail.
- **Rooms and threads**: natural mapping to work contexts (rooms) and
  focused discussions (threads).

**Rooms** scope visibility and access. A room is a collection of
principals who can see each other's state events and messages. Global
rooms (`#bureau/system`, `#bureau/service`, `#bureau/template`, etc.)
hold system-wide configuration. Per-workspace rooms hold project state.

**State events** are the primary data model. A state event is a typed
JSON object identified by (room, event type, state key). Bureau defines
event types for everything: templates (`m.bureau.template`), pipeline
definitions (`m.bureau.pipeline`), tickets (`m.bureau.ticket`), service
bindings (`m.bureau.room_service`), machine config, fleet definitions,
artifact tags.

**Threads** carry conversation — agent work logs, review comments,
pipeline run output, session summaries. Threads are attached to room
messages and provide structured discussion without polluting the room
timeline.

The daemon, services, and other consumers each run their own `/sync`
loop against the homeserver. State changes propagate through `/sync` to
all interested parties. This is how Bureau is event-driven: publish a
template, and the daemon's sync loop picks it up and reconciles. Close a
ticket, and the ticket service's sync loop updates its index and
evaluates gates on dependent tickets.

Agents inside sandboxes access messaging through the proxy's
`/v1/matrix/*` endpoints. The proxy authenticates on the agent's behalf
and enforces policy. See [information-architecture.md](information-architecture.md) for the full room
hierarchy, state event catalog, and data organization patterns.

### Communication tiers

The five primitives create three communication tiers, each for a
distinct purpose. Using the wrong tier for a task is a design error:

- **Proxy** — credential injection and external API access. The proxy
  is the control plane: it mediates access to external services and to
  Matrix. Nothing running within the Bureau network should use the
  proxy for internal service-to-service communication.
- **Matrix** — persistent state, event-driven coordination, audit
  trail. Everything that needs to survive a reboot, be queryable later,
  or trigger downstream reactions goes through Matrix.
- **Direct sockets** — high-frequency, low-latency Bureau-internal
  communication. Service sockets are bind-mounted into sandboxes at
  `/run/bureau/service/<role>.sock`. Templates declare required
  services; the daemon resolves service bindings at sandbox creation
  time.

---

## The Runtime

The primitives need orchestration: something to decide what sandboxes
to create, which services to mount, when to launch principals, and how
to route traffic across machines. That is the runtime.

### Daemon

The daemon (`bureau-daemon`) is the unprivileged, network-facing
process that turns Matrix state into running reality. One daemon runs
per machine.

The daemon's core loop syncs against the homeserver and reconciles
desired state (Matrix) with actual state (running processes, mounted
sockets, registered services):

- **Templates** published to `#bureau/template` define sandbox
  configurations. The daemon watches for templates assigned to its
  machine.
- **Principal assignments** in per-machine config rooms
  (`#bureau/config/<machine>`) declare what should run on this machine.
  The daemon creates sandboxes (via the launcher) for each assignment.
- **StartConditions** on assignments gate principal launch on specific
  state events. The daemon evaluates conditions on each sync tick and
  launches principals when conditions are met, passing the matched
  event content as trigger.json.
- **Service bindings** (`m.bureau.room_service`) declare which service
  instance handles each role in each room. The daemon resolves these at
  sandbox creation time and passes socket mount instructions to the
  launcher.
- **Configuration changes** (template updates, payload changes, policy
  changes) trigger re-reconciliation of affected principals.

### Launcher

The launcher (`bureau-launcher`) is the daemon's privileged
counterpart. It runs on the same machine, communicates over a unix
socket, and handles operations that require elevated privileges:
creating namespaces, decrypting credential bundles, managing sandbox
lifecycle.

The launcher has no network access after startup — the daemon tells it
what to do via IPC. This is the privilege separation that makes
structural security work: the process that can create namespaces and
decrypt credentials has zero network exposure, and the process that
faces the network cannot create namespaces or decrypt credentials. See
[credentials.md](credentials.md) for the full security analysis.

### Transport

The transport layer provides daemon-to-daemon connectivity over WebRTC
data channels, with Matrix for signaling and TURN (coturn) for NAT
traversal. Daemons form a mesh: each daemon maintains persistent
WebRTC connections to peers it communicates with, and new connections
are established on demand via Matrix-signaled SDP exchange.

From a sandbox's perspective, a service on a remote machine looks
identical to a local one — both are unix sockets. The daemon translates
between the local socket and the WebRTC data channel transparently.
SCTP streams within data channels provide independent multiplexing:
multiple sandboxes sharing a daemon-to-daemon connection get their own
streams with no head-of-line blocking.

This mesh topology supports multi-homed fleets where every machine may
be behind NAT. No machine needs a public IP. The TURN server provides
relay when direct peer-to-peer connections fail. For large deployments,
an SFU (LiveKit) can replace the mesh for group communication without
changing the sandbox interface.

WebRTC is the only production-grade protocol that provides all of this
natively. Raw TCP and gRPC require either public addresses or manual
port forwarding — untenable for a system designed to work on a
Raspberry Pi behind a home router as easily as on a cloud GPU cluster.
WireGuard provides encrypted tunnels but requires key distribution and
network configuration that scales poorly with fleet size. WebRTC's ICE
negotiation handles the hard part (discovering a working path between
two machines) and DTLS encrypts the transport without external
certificate infrastructure.

### Fleet

In multi-machine deployments, each machine runs its own daemon. Daemons
coordinate through Matrix: machine heartbeats, service registration,
cross-machine observation routing via the transport layer. The fleet
controller handles service placement — matching service definitions
(with placement constraints and labels) to available machines — along
with failover and scaling. See [fleet.md](fleet.md).

---

## Services

A Bureau service is a principal that provides a capability to other
principals via a socket. The service's implementation and protocol are
its own business — Bureau provides discovery, mounting, and
authentication.

A Bureau-native service like the ticket service speaks CBOR over a unix
socket and runs its own Matrix `/sync` loop. An external service like
Forgejo speaks HTTP and has its own authentication model. A Python
inference server speaks whatever protocol its clients expect. All three
are services. Bureau treats them identically for discovery and mounting.

### Discovery and mounting

Templates declare which services they need:

```go
RequiredServices []string `json:"required_services,omitempty"`
```

These are service *roles* (e.g., `"ticket"`, `"artifact"`, `"forgejo"`),
not specific principal names. Rooms declare which service instance
handles each role via `m.bureau.room_service` state events.

At sandbox creation time, the daemon resolves each required role to a
concrete service principal by scanning the principal's rooms for
matching bindings. The launcher bind-mounts each resolved socket into
the sandbox at `/run/bureau/service/<role>.sock`. If a required service
can't be resolved, sandbox creation fails — no silent degradation.

For services on remote machines, the daemon creates a local tunnel
socket via the WebRTC transport that forwards to the remote service.
The principal never knows the difference.

### Authentication

Callers authenticate to Bureau-internal services via service identity
tokens: CBOR-encoded payloads carrying the caller's identity and
pre-resolved grants, signed with Ed25519. The daemon mints tokens at
sandbox creation time and writes them to `/run/bureau/tokens/<role>`.
The principal reads the token file as opaque bytes and includes it in
each service request. The service verifies the signature, checks
expiry, and uses the embedded grants for authorization.

External services (Forgejo, etc.) use their own authentication models.
Bureau manages the mapping between Bureau principal identity and
service-specific credentials. See [credentials.md](credentials.md) and [forgejo.md](forgejo.md).

---

## Pipelines

Pipelines are structured automation sequences that run inside sandboxes.
A pipeline is a JSON document defining an ordered list of steps,
executed by the pipeline executor.

### Step types

- **shell**: execute a command
- **publish**: PUT a Matrix state event (via proxy)
- **healthcheck**: poll a URL until it returns success
- **guard**: conditionally skip remaining steps based on a check

### Variables

Pipelines support variable substitution with a layered resolution
chain: declarations (defaults) → trigger event fields (`EVENT_*`) →
payload values → environment variables (declared keys only). This allows
the same pipeline definition to be parameterized differently per
principal.

### Thread logging

The executor creates a Matrix thread in a configured room, posts
per-step progress (start, output, success/failure), and links a summary
message. The thread becomes the audit trail for the pipeline run.

See [pipelines.md](pipelines.md) for the full step model, variable resolution, and
execution semantics.

---

## Tickets

Tickets are Bureau's coordination primitive for tracking work. Each
ticket is a Matrix state event (`m.bureau.ticket`) in a room, managed
by the ticket service.

### Why tickets are fundamental

Tickets are how principals coordinate across time and restarts. An
agent reads its assigned ticket to know what to work on. A pipeline gate
watches for a ticket to close before proceeding. A StartCondition fires
when a ticket matches specific criteria, launching a new principal.
Tickets are the connective tissue between autonomous principals that
don't share memory or direct communication channels.

### Key concepts

- **Dependencies**: tickets block other tickets. The service computes
  transitive closure and readiness.
- **Gates**: async conditions (pipeline pass, human approval, timer
  expiry, arbitrary state event) that must be satisfied before a ticket
  is ready. Evaluated by the ticket service's `/sync` loop — no
  polling.
- **Notes**: embedded annotations that travel with the ticket — context
  for the next principal to pick it up.
- **Attachments**: references to artifacts stored outside the ticket.
- **Contention detection**: transitioning to in_progress when already
  in_progress is an error. Principals must claim work atomically before
  planning.

See [tickets.md](tickets.md) for the full data model, service architecture, gate
evaluation, and CLI.

---

## Artifacts

Artifacts are Bureau's content-addressable storage layer. Any blob —
agent conversation logs, stack traces, screenshots, compiled binaries,
model weights, dataset snapshots — can be stored, deduplicated, and
referenced by content hash.

### Key concepts

- **BLAKE3 hashing** with domain separation for chunks, containers, and
  files. References use `art-<hash>` format.
- **Content-defined chunking** (GearHash CDC) for efficient
  deduplication across similar files.
- **Three-tier caching**: per-machine local, per-network shared, and
  backing store.
- **FUSE mount** at `/var/bureau/artifact/mount/` for transparent
  filesystem access: `tag/` for named pointers, `cas/` for direct hash
  access.
- **Tags** as mutable named pointers to immutable content, with
  compare-and-swap for safe concurrent updates. Tags and all artifact
  metadata live in the artifact service's own persistent indexes, not
  in Matrix — artifact counts scale to millions.

Tickets reference artifacts via attachments. Pipelines produce artifacts
as build outputs. Agents store conversation logs as artifacts for later
retrieval and summarization.

See [artifacts.md](artifacts.md) for the chunking algorithm, container format, caching
policies, encryption model, and FUSE implementation.

---

## How They Compose

A coding agent working on a ticket:

```
Template published to #bureau/template
  → daemon reconciles, creates sandbox via launcher
  → sandbox gets proxy.sock, payload.json, tokens/, service/ticket.sock
  → agent reads payload, claims ticket via ticket service socket
  → agent reads workspace, makes changes
  → agent calls external APIs through proxy (credentials injected)
  → agent posts progress to Matrix thread via proxy
  → agent stores conversation log as artifact
  → agent updates ticket status via ticket service
  → human attaches via observation to watch or intervene
  → agent exits, daemon cleans up sandbox
```

A pipeline triggered by a state event:

```
State event published (e.g., git push webhook → Matrix)
  → daemon evaluates StartCondition, matches
  → daemon captures event content as trigger.json
  → launcher creates sandbox with pipeline executor
  → executor reads pipeline definition, runs steps
  → each step logs to Matrix thread
  → publish step PUTs result state event via proxy
  → ticket gate watches for result, fires when matched
  → downstream agent's StartCondition fires
```

A service handling queries across machines:

```
Service runs on machine A, agent runs on machine B
  → daemon B resolves service socket via room_service binding
  → daemon B creates local tunnel socket via WebRTC to daemon A
  → launcher B mounts tunnel socket as /run/bureau/service/<role>.sock
  → agent sends CBOR request to socket, reads CBOR response
  → service authenticates caller via Ed25519 service identity token
  → service processes request, writes state event to Matrix if needed
  → the agent never knows the service is on another machine
```

Every interaction flows through the same structures: Matrix for state
and events, proxy for external access and credentials, direct sockets
for service communication, observation for human oversight, transport
mesh for cross-machine connectivity. The principal doesn't need to know
whether its service socket connects to a local process or a remote
tunnel — the daemon handles routing.
