# Naming and Identity

Bureau uses Matrix user IDs and room aliases as the universal naming
scheme for all entities. Names are self-documenting, structurally
parseable, and consistent across Matrix, the filesystem, CLI, and wire
protocols. This document is the specification for how names are
constructed, how they map across surfaces, and how they are represented
in code.

---

## Namespaces

A **namespace** is the organizational root that owns infrastructure
definitions and fleet resources. Every room in Bureau belongs to a
namespace. There are no global rooms.

`bureau matrix setup --namespace <name>` creates a namespace:

```
#my_bureau                            namespace space
#my_bureau/system                     operational messages
#my_bureau/template                   sandbox template definitions
#my_bureau/pipeline                   pipeline definitions
#my_bureau/artifact                   artifact coordination
```

The namespace name is the first path segment of every localpart in that
namespace. Different namespaces on the same homeserver are fully
independent — `#my_bureau/template` and `#iree/template` coexist
without collision.

A namespace is a trust boundary. The admin account that creates it has
power level 100 in all its rooms. Other operators are invited
explicitly. Machines, services, and agents in one namespace cannot see
rooms in another unless explicitly granted access.

### When to use multiple namespaces

A single namespace is sufficient for most Bureau installations. Multiple
namespaces are useful when:

- **Multiple organizations share a homeserver.** `#acme/` and `#globex/`
  are independent.
- **Projects need their own templates.** `#iree/template` holds
  IREE-specific sandbox templates that build on `#my_bureau/template`
  via template inheritance. A machine in `my_bureau/fleet/prod` joins
  both template rooms if its principals reference templates from both.
- **Operational isolation.** Separate namespaces for production
  infrastructure (`#infra/`) and development projects (`#dev/`).

Namespaces, fleets, and projects are orthogonal hierarchies:

- **Namespace** determines WHO owns the infrastructure definitions
- **Fleet** determines WHERE things run (which machines, what isolation)
- **Project** determines WHAT the work serves (which codebase, which team)

A fleet in `#my_bureau/fleet/prod` can run services for both `#iree/`
and `#tensorflow/` projects. Those projects share machines through the
fleet without needing to know about each other's infrastructure.

---

## Fleets

A **fleet** is an infrastructure isolation boundary within a namespace.
Each fleet has its own machines, services, and fleet controller.

`bureau fleet create <name> --namespace <namespace>` creates a fleet:

```
#my_bureau/fleet/prod                 fleet config, HA leases, service defs
#my_bureau/fleet/prod/machine         machine presence (keys, info, status)
#my_bureau/fleet/prod/service         service directory
```

Fleet rooms are children of the namespace space in the Matrix hierarchy.

### Fleet isolation

Each fleet is a complete isolation boundary:

- **Separate machines.** A machine belongs to exactly one fleet. Its
  Matrix account, daemon, launcher, and run directory are fleet-scoped.
  A physical host serving multiple fleets runs separate Bureau instances.
- **Separate service directory.** Services in `my_bureau/fleet/prod`
  are invisible to `my_bureau/fleet/dev`.
- **Separate fleet controller.** Each fleet has its own placement engine,
  failover, and rebalancing. Fleet controllers cooperate through
  cross-fleet preemption (see fleet.md).

### Multi-fleet machines

A physical host can serve multiple fleets. Each fleet membership is a
separate Bureau machine: separate Matrix account, separate daemon,
separate launcher, separate run directory.

```
Physical host: gpu-box

Fleet prod:
  @my_bureau/fleet/prod/machine/gpu-box:server
  daemon with --fleet my_bureau/fleet/prod
  run-dir: /run/bureau/fleet/prod

Fleet dev:
  @my_bureau/fleet/dev/machine/gpu-box:server
  daemon with --fleet my_bureau/fleet/dev
  run-dir: /run/bureau/fleet/dev
```

Each daemon reports the physical host's hardware via MachineInfo.
Resource allocation between fleets is a capacity planning decision
(resource quotas in fleet config), not a runtime coordination problem.

---

## Entities

An **entity** is a machine, service, or agent within a fleet. Every
entity has a Matrix account and may have a dedicated room.

### Path structure

All entity names follow this structure:

```
<namespace>/fleet/<fleet-name>/<entity-type>/<entity-name>
```

| Segment | Description | Examples |
|---------|-------------|----------|
| namespace | Organizational root | `my_bureau`, `acme`, `iree` |
| `fleet` | Literal segment marking the fleet boundary | always `fleet` |
| fleet-name | Human-chosen fleet identifier | `prod`, `dev`, `us-east-gpu` |
| entity-type | Category of entity | `machine`, `service`, `agent` |
| entity-name | The entity itself (may have sub-segments) | `gpu-box`, `stt/whisper` |

### The @ → # identity

The foundational rule: for any entity with a dedicated Matrix room,
the room alias localpart equals the user ID localpart.

```
@my_bureau/fleet/prod/machine/gpu-box:server     user ID
#my_bureau/fleet/prod/machine/gpu-box:server     config/control room
```

Swap the sigil, get the room. No lookup tables. No string formatting
functions. The identity IS the address.

Not every account has a corresponding room (agents deployed on machines
may not need dedicated rooms), but every entity room alias matches an
account localpart. If an entity later needs a room, its alias is already
determined.

### Entity rooms vs collective rooms

**Collective rooms** aggregate information across all entities of a type:

```
#my_bureau/fleet/prod/machine         all machines in prod fleet
#my_bureau/fleet/prod/service         all services in prod fleet
```

**Entity rooms** are for individual entities (the @ → # rule):

```
#my_bureau/fleet/prod/machine/gpu-box     gpu-box's config room
#my_bureau/fleet/prod/service/fleet       fleet controller's room
```

Entity rooms are "files" in the hierarchy, collective rooms are
"directories." Matrix does not enforce this hierarchy; the naming
convention makes the relationship visible to humans and parseable by
tools.

### Fleet-relative names

Within a fleet context, the fleet prefix is stripped for local
operations. Every entity has two name forms:

| Form | Example | Used for |
|------|---------|----------|
| Full localpart | `my_bureau/fleet/prod/machine/gpu-box` | Matrix operations, globally unique |
| Fleet-relative | `machine/gpu-box` | Socket paths, state keys in fleet rooms, logs within fleet context |

Converting between them:
- Full → relative: strip `<namespace>/fleet/<fleet-name>/`
- Relative → full: prepend `<namespace>/fleet/<fleet-name>/`

---

## Ref Types

Bureau uses strongly typed reference types for all identity operations.
These types are immutable value types, validated at construction, with
pre-computed canonical forms accessible via methods. Raw string
concatenation of localparts, user IDs, and room aliases is prohibited.

### Package: `lib/ref/`

The `ref` package provides:

```
ref.Namespace     server + namespace
ref.Fleet         ref.Namespace + fleet name
ref.Machine       ref.Fleet + entity name
ref.Service       ref.Fleet + entity name
ref.Agent         ref.Fleet + entity name
```

### Properties

- **Value types.** Passing by value is a copy. No heap allocation for
  the ref itself. ~120 bytes per entity ref.
- **Immutable.** All fields are unexported. No setters. Once constructed,
  a ref cannot be changed.
- **Validated.** Constructors return errors for invalid input (empty
  segments, path traversal, length limits, character whitelist).
- **Eager.** All canonical forms (localpart, user ID, room alias) are
  computed at construction time. Accessor methods return pre-computed
  strings at zero cost.

### Construction

```go
ns, err := ref.NewNamespace("bureau.local", "my_bureau")
fleet, err := ref.NewFleet(ns, "prod")
machine, err := ref.NewMachine(fleet, "gpu-box")
service, err := ref.NewService(fleet, "stt/whisper")
```

Parsing from canonical strings:

```go
fleet, err := ref.ParseFleet("my_bureau/fleet/prod", "bureau.local")
machine, err := ref.ParseMachineUserID("@my_bureau/fleet/prod/machine/gpu-box:bureau.local")
```

### Accessors

Every ref provides:

| Method | Returns | Example |
|--------|---------|---------|
| `String()` | Canonical localpart | `my_bureau/fleet/prod/machine/gpu-box` |
| `Localpart()` | Same as String | `my_bureau/fleet/prod/machine/gpu-box` |
| `UserID()` | Matrix user ID | `@my_bureau/fleet/prod/machine/gpu-box:server` |
| `RoomAlias()` | Matrix room alias (@ → #) | `#my_bureau/fleet/prod/machine/gpu-box:server` |
| `Server()` | Server name | `bureau.local` |

Entity refs additionally provide:

| Method | Returns | Example |
|--------|---------|---------|
| `Fleet()` | Parent fleet ref | (FleetRef value) |
| `Name()` | Bare entity name | `gpu-box` |
| `FleetRelativeName()` | Entity type + name | `machine/gpu-box` |
| `SocketPath(runDir)` | Agent-facing socket | `/run/bureau/fleet/prod/machine/gpu-box.sock` |
| `AdminSocketPath(runDir)` | Daemon-only socket | `/run/bureau/fleet/prod/machine/gpu-box.admin.sock` |

Fleet refs additionally provide:

| Method | Returns |
|--------|---------|
| `Namespace()` | Parent namespace ref |
| `FleetName()` | Bare fleet name |
| `MachineRoomAlias()` | `#my_bureau/fleet/prod/machine:server` |
| `ServiceRoomAlias()` | `#my_bureau/fleet/prod/service:server` |
| `RunDir(base)` | `/run/bureau/fleet/prod` |

Namespace refs provide:

| Method | Returns |
|--------|---------|
| `SpaceAlias()` | `#my_bureau:server` |
| `TemplateRoomAlias()` | `#my_bureau/template:server` |
| `PipelineRoomAlias()` | `#my_bureau/pipeline:server` |
| `SystemRoomAlias()` | `#my_bureau/system:server` |
| `ArtifactRoomAlias()` | `#my_bureau/artifact:server` |

### Serialization

The canonical external representation is the full Matrix user ID form:
`@localpart:server`. This is self-contained (includes the server name)
and unambiguous (the `@` sigil distinguishes it from a bare localpart
or room alias).

- **Bureau CBOR protocols:** refs serialize as `@localpart:server`
- **CLI JSON output:** refs serialize as `@localpart:server`
- **Bootstrap configs:** refs serialize as `@localpart:server`
- **Matrix state event keys:** bare localparts (Matrix convention, not
  Bureau's choice — state keys don't include the server)

JSON marshaling produces `"@localpart:server"`. JSON unmarshaling parses
the full form without external context.

---

## Socket Paths

The runtime directory is fleet-scoped. Socket paths use fleet-relative
names:

```
--run-dir /run/bureau/fleet/prod

/run/bureau/fleet/prod/machine/gpu-box.sock            agent-facing
/run/bureau/fleet/prod/machine/gpu-box.admin.sock      daemon-only
/run/bureau/fleet/prod/service/stt/whisper.sock         agent-facing
/run/bureau/fleet/prod/service/stt/whisper.admin.sock   daemon-only
/run/bureau/fleet/prod/agent/code-reviewer.sock         agent-facing
/run/bureau/fleet/prod/launcher.sock                    launcher IPC
/run/bureau/fleet/prod/observe.sock                     observation
```

Agent-facing and daemon-only sockets share the same directory tree,
distinguished by the `.admin.sock` suffix. The launcher mounts individual
socket files into sandboxes (not directory trees), so co-locating both
in the same directory has no security implications — the `.admin.sock`
is simply never mounted.

Socket paths stay within the 108-byte Unix socket limit because the
fleet prefix lives in the run-dir, not duplicated in the socket filename.

---

## CLI Conventions

### Full localpart references

Machine commands take the full machine localpart as a positional
argument. The entity type (`machine/`) is embedded in the localpart,
making the reference unambiguous and self-contained:

```
bureau machine provision my_bureau/fleet/prod/machine/gpu-box
  → @my_bureau/fleet/prod/machine/gpu-box:server

bureau fleet create my_bureau/fleet/prod
  → #my_bureau/fleet/prod:server
```

For federation, the `@` sigil indicates a full Matrix user ID with an
explicit server name. Without the sigil, the server is derived from
the connected admin session:

```
bureau machine provision @my_bureau/fleet/prod/machine/gpu-box:remote.server
  → @my_bureau/fleet/prod/machine/gpu-box:remote.server
```

### Entity-oriented commands

Commands are organized by entity type, not by which process handles
the request:

```
bureau machine list <fleet-localpart>
bureau machine provision <machine-ref>
bureau machine upgrade <machine-ref> --host-env <path>
bureau machine decommission <machine-ref>
bureau machine revoke <machine-ref>

bureau fleet create <fleet-localpart>
bureau fleet enable <fleet-localpart> --host <machine-ref>
bureau fleet status <fleet-localpart>

bureau matrix setup <namespace>
bureau matrix doctor <namespace>
```

Machine commands accept a full machine localpart (e.g.,
`my_bureau/fleet/prod/machine/gpu-box`) or a Matrix user ID with `@`
sigil for federation (e.g., `@.../machine/gpu-box:remote.server`).
Fleet commands accept a fleet localpart (e.g., `my_bureau/fleet/prod`).

Query commands read Matrix state directly (work without a fleet
controller). Mutation commands that require placement intelligence
(place, drain, scale) talk to the fleet controller's socket.

---

## Authorization Globs

The hierarchical naming works with glob-based authorization:

```
my_bureau/fleet/prod/**                everything in prod
my_bureau/fleet/*/machine/**           all machines in all my_bureau fleets
my_bureau/fleet/prod/service/stt/*     all STT services in prod
acme/**                                everything in acme's namespace
```

---

## Validation Rules

All localpart segments follow the same validation:

- No `..` segments (path traversal)
- No leading `.` in any segment (hidden files)
- No empty segments (double slashes)
- No leading or trailing `/`
- Maximum 80 characters total (Unix socket 108-byte limit)
- Valid characters: `a-z`, `0-9`, `.`, `_`, `=`, `-`, `/`
- Lowercase only (Matrix spec requires lowercase localparts)
- The literal segment `fleet` at position 2 marks fleet-scoped names

---

## Complete Example

```
Namespace "my_bureau" (bureau matrix setup --namespace my_bureau):
  #my_bureau                                     namespace space
  #my_bureau/system                              ops messages
  #my_bureau/template                            template definitions
  #my_bureau/pipeline                            pipeline definitions
  #my_bureau/artifact                            artifact coordination

Fleet "prod" (bureau fleet create prod --namespace my_bureau):
  #my_bureau/fleet/prod                          fleet config, HA, service defs
  #my_bureau/fleet/prod/machine                  machine presence
  #my_bureau/fleet/prod/service                  service directory

Machine (bureau machine provision my_bureau/fleet/prod/machine/gpu-box):
  @my_bureau/fleet/prod/machine/gpu-box          machine account
  #my_bureau/fleet/prod/machine/gpu-box          machine config room (@ → #)

Services:
  @my_bureau/fleet/prod/service/fleet            fleet controller account
  @my_bureau/fleet/prod/service/stt/whisper      service account
  @my_bureau/fleet/prod/agent/code-reviewer      agent account (no room needed)

Fleet "dev" (bureau fleet create dev --namespace my_bureau):
  #my_bureau/fleet/dev                           fleet config
  #my_bureau/fleet/dev/machine                   machine presence
  #my_bureau/fleet/dev/service                   service directory
  @my_bureau/fleet/dev/machine/gpu-box           different account, same hardware
  #my_bureau/fleet/dev/machine/gpu-box           different config room

Project-specific templates (bureau matrix setup --namespace iree):
  #iree                                          IREE namespace space
  #iree/template                                 IREE-specific templates

Projects (orthogonal to fleets):
  #iree/amdgpu/general                           project discussion
  #iree/amdgpu/tickets                           project tickets
```

A machine in `my_bureau/fleet/prod` is invited to `#my_bureau/template`
during provisioning. If a principal on that machine references a
template from `#iree/template`, the machine is additionally invited to
that room. Template references encode the room:
`iree/template:agent-base` inherits from `my_bureau/template:base`.

---

## Relationship to Other Documents

- **[fundamentals.md](fundamentals.md)** — the five primitives that
  naming conventions address across
- **[architecture.md](architecture.md)** — runtime topology, sandbox
  filesystem contract, service communication. Architecture.md describes
  how entities interact; this document describes how they are named.
- **[fleet.md](fleet.md)** — fleet management: placement, failover,
  rebalancing. Fleet.md defines fleet behavior; this document defines
  fleet naming.
- **[authorization.md](authorization.md)** — glob-based authorization
  uses the same hierarchical naming for permission patterns
