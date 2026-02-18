# Naming Conventions

Bureau uses Matrix user IDs and room aliases as the universal naming
scheme for all entities. This document defines the conventions that
make names self-documenting, structurally parseable, and consistent
across the system.

## The @→# Identity

The foundational rule: for any entity with a dedicated Matrix room,
the room alias localpart equals the user ID localpart.

```
@bureau/fleet/prod/machine/gpu-box:server
#bureau/fleet/prod/machine/gpu-box:server
```

Swap the sigil, get the room. No lookup tables. No string formatting
functions. The identity IS the address.

Not every account has a corresponding room (agents deployed on machines
may not need dedicated rooms), but every entity room alias matches an
account localpart. If an entity later needs a room, its alias is already
determined.

## Path Structure

All fleet-scoped entity names follow this structure:

```
<namespace>/fleet/<fleet-name>/<entity-type>/<entity-name>
```

| Segment | Description | Examples |
|---------|-------------|----------|
| namespace | Organizational root | `bureau`, `acme`, `company_a` |
| `fleet` | Literal segment marking the fleet boundary | always `fleet` |
| fleet-name | Human-chosen fleet identifier | `prod`, `dev`, `us-east-gpu` |
| entity-type | Category of entity | `machine`, `service`, `agent` |
| entity-name | The entity itself (may have sub-segments) | `gpu-box`, `stt/whisper` |

The `bureau` namespace is the default for Bureau's own infrastructure.
Other namespaces allow organizations to run independent Bureau
installations on a shared homeserver without collision.

## Fleet-Scoped Rooms

### Collective Rooms

Each fleet has collective rooms that aggregate information across all
entities of a type:

```
#bureau/fleet/prod                     fleet config, HA leases, service definitions
#bureau/fleet/prod/machine             machine presence (MachineInfo, MachineStatus)
#bureau/fleet/prod/service             service directory
```

These are "directories" in the naming hierarchy. Created by
`bureau fleet create`.

### Entity Rooms

Individual entities get rooms whose aliases match their account
localparts (the @→# rule):

```
#bureau/fleet/prod/machine/gpu-box     gpu-box's config room
#bureau/fleet/prod/service/fleet       fleet controller's room
```

Entity rooms are "files" in the hierarchy — children of their
collective room by naming convention. Matrix does not enforce this
hierarchy; the naming convention makes the relationship visible to
humans and parseable by tools.

### Global Rooms

Some rooms are fleet-independent. They hold definitions — descriptions
of WHAT exists — as opposed to fleet rooms which describe WHERE things
run:

```
#bureau/system                         operational messages
#bureau/template                       template definitions
#bureau/pipeline                       pipeline definitions
```

Templates describe how to run something (binary, mounts, env vars).
Pipelines describe sequences of steps. These are reusable across
fleets. A single template can be referenced by service definitions
in multiple fleets.

Template and pipeline rooms support namespace flexibility:
`bureau/template:whisper-stt` is a global Bureau template.
`acme/template:proprietary-agent` is an org-specific template.
Fleet-scoped templates (`bureau/fleet/prod/template:custom-agent`)
are possible but not required.

### Project Rooms

Projects, workspaces, and tickets use the namespace hierarchy
independently of fleets:

```
#acme/frontend/general                 project discussion
#acme/frontend/tickets                 project ticket room
```

Projects and fleets are orthogonal hierarchies. A workspace is created
on a machine in a fleet, but the room it works in is project-scoped.
The fleet determines WHERE code runs; the project determines WHAT the
code serves.

## Fleet-Relative Names

Within a fleet context, the fleet prefix is stripped for local
operations. Every entity has two name forms:

| Form | Example | Used for |
|------|---------|----------|
| Full localpart | `bureau/fleet/prod/machine/gpu-box` | Matrix operations, globally unique |
| Fleet-relative | `machine/gpu-box` | Socket paths, CLI shorthand, logs within fleet context |

Converting between them:
- Full → relative: strip `<namespace>/fleet/<fleet-name>/`
- Relative → full: prepend `<namespace>/fleet/<fleet-name>/`

## Socket Paths

The runtime directory (`--run-dir`) is fleet-scoped. Socket paths use
fleet-relative names directly — the path from name to socket is
`<run-dir>/<fleet-relative-name>.sock`:

```
--run-dir /run/bureau/fleet/prod

/run/bureau/fleet/prod/machine/gpu-box.sock              agent-facing
/run/bureau/fleet/prod/machine/gpu-box.admin.sock        daemon-only
/run/bureau/fleet/prod/service/stt/whisper.sock           agent-facing
/run/bureau/fleet/prod/service/stt/whisper.admin.sock     daemon-only
/run/bureau/fleet/prod/agent/code-reviewer.sock           agent-facing
/run/bureau/fleet/prod/launcher.sock                      launcher IPC
/run/bureau/fleet/prod/observe.sock                       observation
```

Agent-facing and daemon-only sockets share the same directory tree,
distinguished by the `.admin.sock` suffix. No parallel directory
hierarchy. Finding all sockets for an entity: `machine/gpu-box.*`.

The launcher mounts individual socket files into sandboxes (not
directory trees), so co-locating `.sock` and `.admin.sock` in the
same directory has no security implications — the `.admin.sock` is
simply never mounted.

This keeps socket paths short (well within the 108-byte Unix socket
limit) regardless of namespace or fleet name depth. The fleet prefix
lives in the run-dir, not duplicated in the socket filename.

## CLI Fleet Context

The `--fleet` flag accepts multiple forms:

| Input | Interpretation |
|-------|---------------|
| `--fleet prod` | Resolves to `bureau/fleet/prod` (assumes `bureau` namespace) |
| `--fleet acme/fleet/staging` | Used verbatim |

Default fleet from environment variable (`BUREAU_FLEET=prod`) or
config file, so day-to-day commands don't require `--fleet` on every
invocation.

## Authorization Globs

The hierarchical naming works naturally with glob-based authorization:

```
bureau/fleet/prod/**                    everything in prod
bureau/fleet/*/machine/**               all machines in all bureau fleets
bureau/fleet/prod/service/stt/*         all STT services in prod
acme/**                                 everything in acme's namespace
```

## Multi-Fleet Machines

A physical host can serve multiple fleets. Each fleet membership is a
separate Bureau machine: separate Matrix account, separate daemon,
separate launcher, separate run-dir. Bureau treats them as independent
entities that happen to share hardware.

```
Physical host: gpu-box

Fleet prod:
  @bureau/fleet/prod/machine/gpu-box         account
  #bureau/fleet/prod/machine/gpu-box         config room
  daemon with --run-dir /run/bureau/fleet/prod

Fleet dev:
  @bureau/fleet/dev/machine/gpu-box          different account
  #bureau/fleet/dev/machine/gpu-box          different config room
  daemon with --run-dir /run/bureau/fleet/dev
```

Each daemon reports the physical host's hardware via MachineInfo.
Resource allocation between fleets is a capacity planning decision
(resource quotas in fleet config), not a runtime coordination problem.

## Validation Rules

All localpart segments follow the existing validation in
`lib/principal/`:

- No `..` segments (path traversal)
- No leading `.` in any segment
- Maximum 80 characters total (Unix socket 108-byte limit)
- Valid characters: alphanumeric, `-`, `_`, `/`
- The literal segment `fleet` at position 2 marks fleet-scoped names

## Complete Example

```
Global rooms (bureau matrix setup):
  #bureau                                            Bureau space
  #bureau/system                                     ops messages
  #bureau/template                                   template definitions
  #bureau/pipeline                                   pipeline definitions

Fleet "prod" (bureau fleet create prod):
  #bureau/fleet/prod                                 fleet config, HA, service defs
  #bureau/fleet/prod/machine                         machine presence
  #bureau/fleet/prod/service                         service directory

  @bureau/fleet/prod/machine/gpu-box                 machine account
  #bureau/fleet/prod/machine/gpu-box                 machine config room (@ → #)

  @bureau/fleet/prod/service/fleet                   fleet controller account
  @bureau/fleet/prod/service/stt/whisper             service account
  @bureau/fleet/prod/agent/code-reviewer             agent account (no room)

Fleet "dev" (bureau fleet create dev):
  #bureau/fleet/dev                                  fleet config
  #bureau/fleet/dev/machine                          machine presence
  #bureau/fleet/dev/service                          service directory

  @bureau/fleet/dev/machine/gpu-box                  different account, same hardware
  #bureau/fleet/dev/machine/gpu-box                  different config room

Non-bureau namespace:
  #acme/fleet/staging                                Acme's staging fleet
  #acme/fleet/staging/machine                        their machine room
  @acme/fleet/staging/machine/k8s-node-1             their machine
  #acme/fleet/staging/machine/k8s-node-1             their config room

Projects (orthogonal to fleets):
  #acme/frontend/general                             project room
  #acme/frontend/tickets                             ticket room
```
