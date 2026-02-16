# Fleet

[fundamentals.md](fundamentals.md) establishes Bureau's primitives;
[architecture.md](architecture.md) defines the runtime topology of
launcher, daemon, and proxy processes. This document describes Bureau's
fleet management layer: how services are placed across machines, how
machines are provisioned and deprovisioned, how failover works, and how
multiple fleet controllers coexist.

---

## Why Fleet Management

Bureau's daemon reconciliation model is declarative: an admin writes
PrincipalAssignment events to a machine config room, and the daemon on
that machine reconciles to match. This works for a single machine or a
small, manually managed set. For a fleet — local machines, ephemeral
cloud GPU instances, spare hardware that sleeps until needed — manual
placement creates friction:

- **No automatic failover.** If a machine goes offline, its services
  stop. No other daemon knows to pick them up. A human has to notice,
  decide where to re-place, and write new assignments.
- **No resource awareness.** The daemon does not know that a machine is
  at 90% CPU when it starts another service. Placement decisions require
  checking machine status manually.
- **No machine lifecycle.** Spare machines that could serve burst
  workloads sit powered off. Cloud instances that have been idle for
  hours keep billing. Nobody provisions or deprovisions automatically.
- **No batch scheduling.** Training jobs that should run overnight when
  GPUs are idle have no mechanism to express "run me when it's cheap"
  or "preempt me when something more important arrives."
- **No fleet-wide view.** Each daemon sees only its own machine.
  Questions like "where is the whisper service running?" or "which
  machine has the most headroom?" require querying multiple daemons.

The fleet controller fills this gap. It operates at the fleet level,
watches all machines, and writes PrincipalAssignment events to machine
config rooms. Daemons continue to do exactly what they do today — the
fleet controller is a layer above them, not a replacement.

---

## Design Principles

**Intent over placement.** Users declare "I need a whisper STT instance
with a GPU" and the fleet controller decides where. Direct
PrincipalAssignment still works for manual overrides, bootstrapping,
and special cases. Both models coexist — the fleet controller writes
to the same config rooms a human would, not replacing manual placement
but complementing it. PrincipalAssignment is the interface because it
keeps the daemon's reconciliation model unchanged, works when the
target daemon is temporarily offline (the assignment waits in Matrix),
allows manual and fleet-managed assignments to coexist, and makes
every placement decision an auditable record.

**Labels are the universal constraint language.** Machine capabilities,
fleet membership, scheduling preferences, and placement constraints all
use the same label system. "Must run on a persistent local machine with
a GPU" is `requires: ["persistent", "local", "gpu"]`. There is no
separate concept of fleet membership — a machine belongs to a fleet
because it has labels matching the fleet controller's constraints.
Adding a machine to a fleet is adding a label. A machine in multiple
fleets has labels matching multiple fleets' constraints. The same
label system handles hardware capabilities (`gpu`, `h100`),
organizational tags (`persistent`, `ephemeral`), and deployment
concerns (`production`, `sandbox`) uniformly.

**The fleet controller is a Bureau service.** It has a Matrix account,
an in-memory model, a /sync loop, and a unix socket API. It runs in a
sandbox and uses the proxy for external API calls (cloud provisioning).
It never has direct access to credentials.

**Daemons are unchanged.** The per-machine reconciliation model is not
modified. Daemons read PrincipalAssignment events from their config
room and reconcile. They do not know or care whether a human or a fleet
controller wrote those events.

**Machine definitions mirror service definitions.** Services declare
what they need ("1 replica, requires GPU, failover: migrate"). Machines
declare what they provide ("H100 GPU, 340 GB RAM, provisionable via
GCloud API"). The fleet controller matches supply to demand.

**Multiple fleet controllers coexist.** A prod fleet controller manages
production services. A sandbox fleet controller manages test
environments. Each is scoped by its own localpart and only manages
assignments it created. Fleet controllers share machines cooperatively
— each sees total machine utilization and avoids overloading — rather
than getting hard resource partitions. For small fleets (3-10 machines),
rigid partitioning would waste capacity.

**Critical services self-heal.** Services marked `ha_class: "critical"`
get daemon-level failover independent of the fleet controller. This
solves the bootstrap problem (who manages the fleet controller's own
failover?) and provides a foundation for any service that the fleet
cannot afford to lose.

**Conservative defaults.** Operators must explicitly enable automatic
behavior. Rebalancing defaults to alert-only (propose, don't move).
Failover has no default (must be specified). Machine provisioning
defaults to manual. Auto-scaling and batch scheduling are opt-in. The
fleet controller is safe by default.

---

## Data Model

Fleet management uses state events in the `#bureau/fleet` room.

### Fleet Service Definition

A fleet service definition declares the desired state of a service
across the fleet. It is the fleet analog of a PrincipalAssignment: it
says what should exist without specifying which machine.

- **Event type:** `m.bureau.fleet_service`
- **State key:** service localpart (e.g., `service/stt/whisper`)
- **Room:** `#bureau/fleet`

A fleet service carries:

- **Template** — the sandbox template reference for this service.
- **Replicas** — minimum and maximum instance counts. Min is the floor
  the fleet controller maintains. Max is the ceiling for auto-scaling.
  When max equals min (or is omitted), no auto-scaling occurs.
- **Resources** — CPU, memory, and GPU requirements for one instance.
  The placement algorithm uses these to determine which machines have
  sufficient capacity.
- **Placement constraints** — which machines are eligible:
  - *Requires:* labels a machine must have (AND semantics).
  - *Preferred machines:* ordered list to try first before scoring.
  - *Allowed machines:* glob patterns limiting candidates.
  - *Anti-affinity:* other fleet services this one should avoid sharing
    a machine with (for spreading replicas or avoiding GPU contention).
  - *Co-location:* other fleet services this one prefers to share a
    machine with (for locality-sensitive service pairs).
- **Failover** — what happens when the hosting machine goes offline:
  `migrate` (fleet controller re-places automatically), `alert`
  (publish an alert and optionally create a ticket; a human or sysadmin
  agent decides), or `none` (pinned, no automatic action).
- **HA class** — `"critical"` enables daemon-level failover via the
  watchdog protocol (see [High Availability](#high-availability)).
  Empty means normal fleet-controller-managed failover.
- **Priority** — placement order when services compete for the same
  resources. Lower numbers are higher priority. 0 = critical
  infrastructure, 10 = production, 50 = development, 100 =
  batch/background.
- **Scheduling** — controls when the service runs (see
  [Batch Scheduling](#batch-scheduling)). Nil means always running.
- **Fleet** — the localpart of the fleet controller that manages this
  service. If empty, any controller may claim it.
- **Service rooms** — room alias glob patterns where the fleet
  controller ensures the service is bound via `m.bureau.room_service`.
- **Payload** — per-service configuration passed to the template,
  merged with the PrincipalAssignment payload.

### Machine Definition

A machine definition declares a provisionable machine — either a
physical machine woken via wake-on-LAN, or a cloud instance created
via API. Machine definitions enable the fleet controller to bring
machines online when demand exceeds current capacity and shut them down
when idle.

Machines that are always on (permanently running workstations, NAS
boxes) do not need machine definitions. They register at boot and the
fleet controller discovers them via heartbeat. Machine definitions are
for machines whose lifecycle is managed.

- **Event type:** `m.bureau.machine_definition`
- **State key:** pool name or machine localpart (e.g.,
  `gpu-cloud-pool` or `machine/spare-workstation`)
- **Room:** `#bureau/fleet`

A machine definition carries:

- **Provider** — how this machine is provisioned: `local` (WoL or SSH
  wake), cloud providers (`gcloud`, `aws`, `azure`), or `manual`
  (human intervention, fleet controller publishes a capacity request).
- **Labels** — what labels instances from this definition will have
  when they register. The fleet controller uses these to match
  definitions to service constraints before the machine is online.
- **Resources** — CPU cores, memory, GPU model, GPU count, GPU memory,
  disk. Used to predict whether a provisioned machine will satisfy a
  pending service's requirements.
- **Provisioning** — provider-specific configuration: cloud instance
  type, region, zone, boot image, startup script, credential name
  (for proxy-injected cloud API keys), MAC address (for WoL), IP
  address (for SSH), and cost per hour.
- **Scaling** — min and max instance counts. For local machines, min
  and max are both 0 or 1. For cloud pools, this bounds how many
  instances the fleet controller can create.
- **Lifecycle** — when to provision (`demand`, `always`, `manual`),
  when to deprovision (`idle`, `manual`, `never`), idle timeout, wake
  method (`wol`, `ssh`, `api`, `manual`), suspend method
  (`ssh_command`, `api`, `manual`), and estimated wake latency.

### Fleet Configuration

Global settings for a fleet controller instance.

- **Event type:** `m.bureau.fleet_config`
- **State key:** fleet controller localpart
- **Room:** `#bureau/fleet`

Controls rebalancing policy (`auto` vs `alert`), pressure thresholds
for CPU, memory, and GPU, sustained duration before acting, rebalancing
cooldown, heartbeat interval, and which other fleet controllers may
preempt this fleet's services.

### HA Lease

Used by the daemon watchdog protocol for critical service failover.

- **Event type:** `m.bureau.ha_lease`
- **State key:** service localpart
- **Room:** `#bureau/fleet`

Records which machine holds the lease, when it expires, and when it
was acquired. Daemons compete to claim expired leases for critical
services whose host has gone offline.

### Machine Status

Each daemon publishes periodic heartbeat events to `#bureau/machine`
with machine-level resource utilization (CPU, memory, GPU) and a
per-principal breakdown. The per-principal breakdown reports CPU,
memory, GPU utilization, and lifecycle state for each sandbox, giving
the fleet controller a complete picture of machine utilization and
per-service resource consumption without additional queries.

- **Event type:** `m.bureau.machine_status`
- **State key:** machine localpart
- **Room:** `#bureau/machine`

### Service Status

Application-level metrics published by services that want smart
scaling. The daemon cannot observe these from outside the sandbox —
queue depth, latency, error rate are internal to the service. Services
that want fleet-aware scaling publish these through their proxy to
`#bureau/service`.

- **Event type:** `m.bureau.service_status`
- **State key:** service localpart
- **Room:** `#bureau/service`

Reports machine, queue depth, average latency, request throughput,
error rate, and health status.

---

## The Fleet Controller

The fleet controller is a Bureau service principal with a Matrix
account, a unix socket, and a registration in `#bureau/service`.

### In-Memory Model

The fleet controller maintains:

- **Machines:** state of every registered machine — hardware info,
  resource utilization from heartbeats, labels, online/offline status.
- **Services:** fleet service definitions and their current instances
  (which machine, health, resource usage).
- **Definitions:** machine definitions available for provisioning.
- **Assignments:** current PrincipalAssignment events across all
  machine config rooms, tracked to distinguish fleet-managed from
  manual assignments.
- **Leases:** HA lease state for critical services.

### Rooms

| Room | What it reads |
|------|--------------|
| `#bureau/fleet` | Fleet service definitions, machine definitions, fleet config, HA leases |
| `#bureau/machine` | Machine keys, info (labels, hardware), status heartbeats |
| `#bureau/service` | Service registrations, service status metrics |
| `#bureau/config/*` | Current PrincipalAssignment events (to track what is already placed) |

The fleet controller is invited to all machine config rooms so it can
both read current assignments and write new ones. Its power level in
config rooms must be sufficient to write PrincipalAssignment events
(PL 50, same as the machine account).

### Placement Algorithm

When the fleet controller needs to place a service instance, it scores
each candidate machine:

```
score(machine, service_definition) -> int or INELIGIBLE

INELIGIBLE if:
  - machine is cordoned
  - machine lacks any required label
  - machine localpart does not match any allowed_machines glob
  - available memory < service memory requirement
  - service needs GPU and machine has no GPU
  - available GPU memory < service GPU memory requirement
  - anti-affinity violation (machine hosts a service in the anti-affinity list)

Score components (normalized 0-100, then weighted):
  + available_memory_headroom       weight: 25
  + available_cpu_headroom          weight: 25
  + available_gpu_headroom          weight: 20 (if GPU required, else 0)
  + preferred_machine_bonus         weight: 15 (first = 15, second = 10, third = 5)
  + co_locate_bonus                 weight: 10
  - current_utilization_penalty     weight: 5
```

For batch services with preferred time windows, the score is multiplied
by a time-of-day factor: 1.0 during preferred windows, 0.3 outside
(still eligible but strongly disfavored).

The fleet controller picks the highest-scoring machine. Ties are broken
by machine localpart (deterministic). The placement plan can be
previewed via the CLI before committing.

---

## Lifecycle Operations

**Place.** The fleet controller writes a PrincipalAssignment to the
chosen machine's config room. The assignment includes the service
template reference, a label `fleet_managed: "<fleet-controller-localpart>"`
to identify it as fleet-managed, any payload from the fleet service
definition, and ServiceRooms for room_service bindings. The daemon
reconciles and starts the sandbox.

**Remove.** The fleet controller removes (blanks) the
PrincipalAssignment from the machine's config room. The daemon
reconciles and destroys the sandbox.

**Drain.** For a machine drain, the fleet controller iterates all
fleet-managed assignments on that machine, finds alternative placements
for each (re-running the placement algorithm excluding the drained
machine), and executes place-then-remove pairs. Non-fleet-managed
assignments are not touched — draining only affects services the fleet
controller manages.

**Cordon and uncordon.** The fleet controller adds or removes a
`cordoned` label on the machine. Cordoned machines score INELIGIBLE
for new placements but services already running continue.

**Failover.** When a machine heartbeat stops (3x the heartbeat interval
with no update), the fleet controller:

1. Marks the machine as offline in its model.
2. For each fleet-managed service on that machine:
   - `failover: "migrate"` — run placement algorithm, place on a new
     machine.
   - `failover: "alert"` — publish an alert event in `#bureau/fleet`
     and (if the ticket service is available) create a ticket.
   - `failover: "none"` — do nothing (service stays unplaced until the
     machine returns).
3. Remove stale PrincipalAssignment from the offline machine's config
   room.

The stale assignment removal in step 3 prevents split-brain: if the
fleet controller moves a service from machine A to machine B while A
is offline, and A later comes back, A's daemon must not restart the
service. Removing the assignment from A's config room prevents this.
Matrix state events can be written to rooms even when the target
machine's daemon is offline — the homeserver accepts the event, and
the daemon sees the removal when it reconnects.

---

## Rebalancing

The fleet controller maintains a sliding window of machine utilization
from heartbeat events. When a machine exceeds a pressure threshold for
the sustained duration:

1. Identify fleet-managed services on the pressured machine, sorted by
   priority (lowest priority first — batch and background services move
   before production ones).
2. For each candidate service (in priority order):
   a. Check cooldown: was this service placed less than cooldown_seconds
      ago? If so, skip.
   b. Run placement algorithm excluding the current machine.
   c. If the target machine would remain below 60% utilization after the
      move: execute the move.
   d. If the target would exceed 60%: skip to next candidate.
3. Stop when the pressured machine's projected utilization drops below
   the target threshold (pressure threshold minus 15 percentage points).

Hysteresis prevents oscillation:

- **Trigger threshold** (default 85% CPU): start evaluating.
- **Target after move** (60%): do not overload the destination.
- **Cooldown** (default 10 minutes): do not re-move recently placed
  services.
- **Sustained duration** (default 5 minutes): ignore transient spikes.
- **Minimum gap** (25 percentage points between trigger and target):
  only move when imbalance is significant.

When `rebalance_policy` is `"alert"`, the fleet controller skips the
actual move and instead publishes an `m.bureau.fleet_alert` event with
the proposed rebalancing plan. A sysadmin agent or human reviews and
approves via the CLI.

---

## Auto-Scaling

When a fleet service definition has max replicas greater than min,
the fleet controller evaluates scaling triggers.

**Scale up** when any of:
- All current instances report unhealthy in service status.
- Average queue depth across instances exceeds a threshold
  (configurable per service, or a global default of 10).
- Current replicas are below min (a machine went down and failover is
  still in progress).

**Scale down** when all of:
- All instances report healthy and queue depth is low.
- Current replicas exceed min.
- The least-loaded instance has been at <10% utilization for 2x the
  cooldown period.
- Removing one instance would not push the remaining instances above
  70% utilization.

Scale-down is conservative: the fleet controller waits longer and
checks more conditions before removing a replica than before adding
one. Adding a replica is cheap (start a sandbox). Removing one drops
in-flight requests if the service does not support graceful drain.

Batch services (`scheduling.class: "batch"`) do not auto-scale. They
get exactly their minimum replica count, placed during preferred
windows.

---

## Machine Provisioning

When the fleet controller needs to place a service but no running
machine satisfies constraints:

1. Score machine definitions against the service's requirements (same
   label matching and resource checking as machine scoring, but using
   the definition's declared resources rather than live metrics).
2. If a definition matches:
   a. Check scaling limits (current instances from this definition are
      below max).
   b. Provision based on provider:
      - **WoL:** send wake signal (delegated to a daemon on the same
        LAN segment via Matrix command message; the fleet controller
        does not need direct network access).
      - **Cloud API:** make provisioning call through the proxy (e.g.,
        `POST /v1/external/gcloud/compute/instances` — the proxy
        injects cloud credentials). The request includes boot image,
        startup script (which runs bureau-launcher), and instance
        configuration.
      - **Manual:** publish a capacity request event in `#bureau/fleet`
        and optionally create a ticket. Wait for a human or sysadmin
        agent.
   c. Wait for the machine to register (heartbeat appears in
      `#bureau/machine`). Timeout after 2x the definition's wake
      latency (or 300 seconds default for cloud).
   d. Once registered, run the placement algorithm with the new machine
      as a candidate.
3. If no definition matches: publish a capacity request. The fleet
   controller cannot conjure machines that do not match any definition —
   this signals that the operator needs to add capacity or define new
   machine pools.

### Machine Deprovisioning

The fleet controller tracks idle time for machines with
`deprovision_on: "idle"` in their definition:

1. A machine has zero fleet-managed services and no non-fleet-managed
   PrincipalAssignment events in its config room.
2. The idle timer starts.
3. After idle_timeout_seconds: the fleet controller deprovisions:
   - **WoL/SSH:** send suspend command via Matrix command message to
     the machine's daemon (the daemon executes `systemctl suspend` or
     equivalent, then goes offline naturally).
   - **Cloud API:** terminate or stop the instance via proxy API call.
   - **Manual:** publish a deprovisioning suggestion event.
4. The fleet controller cleans up stale assignments and updates its
   model.

Before deprovisioning, the fleet controller checks whether any batch
service has a preferred window starting within the next hour. If so,
it defers deprovisioning to avoid paying the wake latency right before
the machine would be needed.

---

## Batch Scheduling

Batch services (`scheduling.class: "batch"`) get special treatment.

**Placement timing.** The fleet controller evaluates batch placements
on a schedule (every 15 minutes) rather than immediately. During
evaluation:
1. Is a preferred time window active? If not, defer unless the service
   has been waiting longer than 24 hours (starvation prevention).
2. Is there a machine with sufficient resources and low utilization?
   Batch services target machines at <40% utilization to avoid
   competing with interactive workloads.
3. Place with the service's priority (typically 100 = batch/background)
   so the daemon schedules it after all other principals.

**Preemption.** When a higher-priority service needs resources on a
machine running a batch service:
1. Fleet controller sends the batch service's checkpoint signal (via
   the daemon, which signals the sandbox process).
2. Waits drain_grace_seconds.
3. Removes the PrincipalAssignment (daemon stops the sandbox).
4. Places the higher-priority service.
5. The batch service remains in the fleet controller's pending queue,
   to be re-placed when resources are available.

Batch services must handle preemption: save state on checkpoint signal,
resume from saved state on next start. This is a contract between the
service and its template.

**Cost awareness.** If a batch service has a cost budget and the fleet
controller is considering placing it on a machine with a known hourly
cost (from the machine definition), the fleet controller skips machines
that exceed the budget. This prevents expensive cloud GPUs from being
used for background work that could wait for a cheaper slot.

---

## Multi-Fleet Controllers

Multiple fleet controllers coexist in the same Bureau deployment, each
managing a distinct scope of services and machines.

### Scoping

Each fleet controller is identified by its localpart (e.g.,
`service/fleet/prod`, `service/fleet/sandbox`). Fleet service
definitions are scoped to a controller via the `fleet` field. A
controller only manages services with its own localpart in the `fleet`
field (or unscoped services, which any controller may claim by writing
its localpart to the `managed_by` field).

### Assignment Tagging

Every PrincipalAssignment written by a fleet controller includes a
label `fleet_managed: "<fleet-controller-localpart>"`. Fleet
controllers only modify assignments bearing their own tag. Manual
assignments (no `fleet_managed` label) are never touched. This
prevents fleet controllers from interfering with each other or with
manually placed principals.

### Resource Visibility

Each fleet controller sees total machine utilization in the heartbeat —
including services managed by other controllers and manual assignments.
When the prod controller scores a machine, it sees that the sandbox
controller's test service is using 15% CPU and accounts for it. No
controller gets an isolated view.

Fleet controllers cooperate implicitly: each avoids overloading machines
because it sees the full picture. Explicit cooperation happens through
priority and preemption.

### Priority and Preemption

A fleet controller's config lists which other controllers may preempt
its services (via `preemptible_by`). When the prod controller needs to
place a service and the best machine is loaded with sandbox services:

1. Prod controller publishes a preemption request in `#bureau/fleet`.
2. Sandbox controller reads the request (via /sync).
3. Sandbox controller identifies its lowest-priority service on the
   target machine.
4. Sandbox controller moves (or checkpoints and removes) its service.
5. Sandbox controller acknowledges the preemption.
6. Prod controller places its service.

This is cooperative: the sandbox controller does the actual removal of
its own assignments. The prod controller does not write to
sandbox-tagged assignments. If the sandbox controller is offline,
preemption does not happen — the prod controller must find another
machine or wait. Manual intervention by an operator is the escape hatch
when cooperative preemption fails.

---

## High Availability

The fleet controller manages failover for normal services. But who
manages failover for the fleet controller itself? And for other
services that the fleet cannot lose (ticket service, messaging relay)?

Services marked `ha_class: "critical"` get daemon-level failover:
every daemon participates in a watchdog protocol that ensures the
service is restarted on another machine if its host goes down.

### The Daemon Watchdog Protocol

Every daemon syncs `#bureau/fleet`. For each `ha_class: "critical"`
fleet service definition, the daemon:

1. **Monitors the service's machine.** Watches the heartbeat of the
   machine hosting the critical service and the service's registration
   in `#bureau/service`.

2. **Detects failure.** If the hosting machine's heartbeat is missing
   for 3x the heartbeat interval (default: 90 seconds), the daemon
   considers the service potentially down.

3. **Evaluates eligibility.** Checks whether its own machine satisfies
   the critical service's placement constraints (required labels,
   allowed_machines globs, sufficient resources). If not, it does
   nothing.

4. **Enters the claim race.** Eligible daemons compete to acquire the
   HA lease:
   a. Wait a random delay between 1 and 10 seconds. Machines in the
      service's preferred_machines list get shorter delays (1-3s),
      others get longer (4-10s). This biases the race toward preferred
      machines without hard-coding a leader.
   b. Read the `m.bureau.ha_lease` state event for this service.
   c. If the lease is held by the offline machine (expired or holder
      matches the dead machine): write a new lease claiming this
      machine as holder.
   d. Wait 2 seconds, read the lease back.
   e. If this machine is still the holder: proceed to step 5.
   f. If another machine overwrote the lease: back off.

5. **Hosts the service.** The winning daemon writes a
   PrincipalAssignment for the critical service to its own machine
   config room. Its reconciliation loop starts the service.

6. **Renews the lease.** The daemon periodically updates the lease's
   expiry. If the lease expires without renewal, other daemons may
   claim it.

### Lease Mechanics

The HA lease is a Matrix state event. Matrix state events are
last-writer-wins, which is not ideal for leader election. The random
backoff plus verify pattern compensates:

- With 3-10 machines and random delays of 1-10 seconds, the
  probability of two daemons writing within the same 2-second
  verification window is low.
- If two daemons collide: both verify, one sees it has been
  overwritten, and backs off. The other proceeds. At most one duplicate
  sandbox starts briefly (the losing daemon's reconciliation loop
  destroys it when the assignment is cleaned up).
- This is not Raft. It does not provide linearizable consensus. It
  provides "one machine picks it up within 15 seconds" — sufficient
  for a failure mode where the service was already down for 90 seconds
  of missed heartbeats.

### Bootstrap

The fleet controller itself is defined as a fleet service with
`ha_class: "critical"`. On initial deployment:

```
bureau fleet enable --name prod --host workstation
```

This command:
1. Registers `@service/fleet/prod:bureau.local` on the homeserver.
2. Publishes a fleet service definition with `ha_class: "critical"`.
3. Writes an HA lease with the specified host as holder.
4. Writes a PrincipalAssignment to the host machine's config room.
5. The daemon reconciles and starts the fleet controller.

From this point, if the host goes down, the daemon watchdog on other
eligible machines detects the failure, claims the lease, and starts
the fleet controller elsewhere.

### Matrix Outage

If the homeserver goes down, the entire coordination layer stops.
Daemons cannot sync, fleet controllers cannot observe, watchdogs
cannot detect failures.

- **Running services continue.** The launcher and proxies do not
  depend on Matrix. Sandboxes keep running.
- **No new placements, failover, or rebalancing.** No data to drive
  decisions.
- **Daemons keep their last-known config.** They continue running
  whatever they were running when the homeserver went down.

When Matrix recovers, all daemons and fleet controllers reconnect via
/sync, receive accumulated state changes, and reconcile. The fleet
controller evaluates all services against current machine state and
corrects any drift that accumulated during the outage.

---

## Ticket Integration

The fleet controller handles mechanical operations: placement,
failover, rebalancing. Decisions that require judgment go to the
sysadmin agent via tickets.

### When the Fleet Controller Creates Tickets

- **`failover: "alert"`** — machine down, service needs re-placement,
  but the service definition wants human review before moving.
- **Capacity exhaustion** — no machine or machine definition can
  satisfy a service's placement constraints. The ticket includes the
  constraints that could not be met and suggested remedies (add a
  machine definition, cordon a lower-priority service to free
  resources).
- **`rebalance_policy: "alert"`** — machine under pressure, the fleet
  controller has a rebalancing plan and wants approval before
  executing.
- **`provision_on: "manual"`** — a machine definition requires human
  provisioning.
- **Cost threshold** — a placement would exceed a budget limit.

Tickets are created in the appropriate room: service-specific rooms
for service issues, `#bureau/system` for fleet-wide issues. Each
ticket includes structured data (the proposed action, affected
services, machine utilization) so the sysadmin agent can evaluate
without additional queries.

### The Sysadmin Agent as Fleet Operator

The sysadmin agent has access to the fleet controller's socket API.
It picks up tickets, queries the fleet controller for context, makes
decisions, and executes actions — the same workflow a human operator
would follow through the CLI. The fleet controller does not
distinguish between a human, an agent, or a script.

A capacity-review agent (or the sysadmin agent on a schedule) can run
periodically to evaluate fleet utilization trends, identify patterns
(rising queue depth, declining headroom), and create tickets with
scaling recommendations. This emerges from Bureau primitives: a
scheduled agent that reads data and creates tickets. The fleet
controller provides the data (via its API) and executes the decisions
(via service definition updates).

---

## CLI Surface

Fleet management adds commands to the `bureau` CLI. All talk to the
fleet controller's unix socket.

### Machine Commands

| Command | Description |
|---------|-------------|
| `bureau machine list [--fleet F] [--label L]` | List machines with status, labels, running principals, utilization |
| `bureau machine show <machine>` | Detailed info: hardware, utilization, per-sandbox resource usage, labels |
| `bureau machine drain <machine> [--fleet F]` | Migrate fleet-managed services off the machine |
| `bureau machine cordon <machine>` | Add `cordoned` label, preventing new placements |
| `bureau machine uncordon <machine>` | Remove `cordoned` label |
| `bureau machine label <machine> <k>=<v> ...` | Add or update labels |
| `bureau machine wake <machine>` | Send wake signal (WoL magic packet or cloud provision) |
| `bureau machine suspend <machine>` | Send suspend/deprovision signal |
| `bureau machine revoke <machine> [flags]` | Emergency credential revocation: deactivate account, clear state, publish revocation event |

### Service Commands

| Command | Description |
|---------|-------------|
| `bureau service define <localpart> --template <ref> [flags]` | Create a fleet service definition |
| `bureau service list [--fleet F]` | List definitions with instance counts, machines, health |
| `bureau service show <localpart>` | Full definition plus instance details |
| `bureau service instances <localpart>` | Per-instance details: machine, resource usage, uptime, health |
| `bureau service plan <localpart>` | Dry-run placement: show scores for each machine without placing |
| `bureau service scale <localpart> --replicas N` | Update replica count |
| `bureau service place <localpart> --machine <m>` | Manual override: place on a specific machine |
| `bureau service unplace <localpart> --machine <m>` | Remove a specific instance |
| `bureau service delete <localpart>` | Remove definition and all fleet-managed assignments |

### Fleet Commands

| Command | Description |
|---------|-------------|
| `bureau fleet enable --name <name> --host <machine>` | Bootstrap a fleet controller |
| `bureau fleet status [--name <name>]` | Health: uptime, managed services, machines, pending placements |
| `bureau fleet config <name> [flags]` | Update fleet configuration |

---

## Worked Example

Starting state: three local machines, one spare that sleeps, and a
cloud GPU pool.

**Always-on machines:**
- `machine/workstation`: 16 cores, 64 GB RAM, RTX 4090 (24 GB VRAM).
  Labels: `persistent`, `local`, `gpu`, `rtx4090`.
- `machine/pi-kitchen`: 4 cores, 8 GB RAM, no GPU.
  Labels: `persistent`, `local`, `arm`.
- `machine/nas`: 8 cores, 32 GB RAM, no GPU, 48 TB storage.
  Labels: `persistent`, `local`, `storage`.

**Machine definitions:**
- `spare-workstation`: local provider, WoL wake, SSH suspend. 12 cores,
  32 GB RAM, RTX 3080 (10 GB VRAM). Provision on demand, deprovision
  when idle for 1 hour.
- `gpu-cloud-pool`: GCloud provider, `a3-highgpu-1g` instances. 96
  cores, 340 GB RAM, H100 (80 GB VRAM). $3.50/hr. 0-2 instances,
  provision on demand, deprovision when idle for 1 hour.

### Day 0: Bootstrap and Initial Services

```
$ bureau fleet enable --name prod --host workstation
Fleet controller running on machine/workstation.

$ bureau service define service/stt/whisper \
    --template bureau/templates:whisper-stt \
    --replicas 1 --requires gpu --failover migrate \
    --rooms "#iree/**" --fleet service/fleet/prod
  machine/workstation    score: 92  (24 GB VRAM, 12% CPU)
  machine/pi-kitchen     INELIGIBLE (no GPU)
  machine/nas            INELIGIBLE (no GPU)
Placed on machine/workstation.

$ bureau service define service/ticket/bureau \
    --template bureau/templates:ticket-service \
    --replicas 1 --requires persistent --failover migrate \
    --ha-class critical --fleet service/fleet/prod
  machine/workstation    score: 78  (24% CPU after whisper)
  machine/pi-kitchen     score: 85  (4% CPU, lightest load)
  machine/nas            score: 82  (8% CPU)
Placed on machine/pi-kitchen.
```

### Week 1: On-Demand GPU for Testing

A developer agent creates a ticket requesting a GPU sandbox for
compiler testing. The sysadmin agent picks it up:

```
Query: bureau service plan iree/test/compiler --requires gpu
  machine/workstation    score: 65  (GPU at 72% from whisper)
  spare-workstation      score: 88  (offline, WoL, 30s wake)
  gpu-cloud-pool         score: 75  (offline, ~60s provision, $3.50/hr)

Decision: wake spare workstation (free, fast, local).
Action: bureau machine wake spare-workstation
  Registered in 28 seconds.
Action: bureau service place iree/test/compiler --machine spare-workstation
  Placed.
```

Two hours later, testing completes. Spare workstation has no
fleet-managed services. After one hour idle: fleet controller sends
suspend command via Matrix. Spare workstation sleeps.

### Month 1: Overnight Training

```
$ bureau service define ml/training/iree-compile \
    --template bureau/templates:ml-training \
    --replicas 1 --requires gpu \
    --scheduling batch --preemptible \
    --preferred-hours 22-06 \
    --failover none --fleet service/fleet/prod
```

10 PM: fleet controller evaluates batch placements. Workstation at 15%
CPU (whisper idle). Places training job on workstation.

8 AM: whisper traffic spikes. Workstation CPU reaches 60% — training
continues. At 85% sustained for 5 minutes: fleet controller sends
SIGUSR1 to training sandbox (checkpoint signal), waits 300 seconds
drain grace, removes the assignment. Training checkpoints and exits.
Whisper gets full GPU.

10 PM next day: fleet controller re-places training. Training resumes
from checkpoint.

### Month 2: Capacity Pressure

A capacity-review agent runs weekly analysis and creates a ticket:

> whisper average queue depth: 34 (up from 12 last week). Peak: 127.
> Average GPU utilization: 81%. Recommendation: scale to 2 replicas.
>
> Options: (a) second instance on spare-workstation (free, RTX 3080),
> (b) provision cloud H100 ($3.50/hr), (c) accept current latency.

Human approves option (a). Sysadmin agent adjusts the max replicas.
Fleet controller now wakes spare-workstation when whisper queue depth
exceeds threshold, places a second instance, and suspends it when
demand drops.

---

## Relationship to Other Design Documents

- **[architecture.md](architecture.md)** — the fleet controller is a
  service principal, discoverable via `#bureau/service`, reachable via
  unix socket, running in a sandbox with proxy access. Machine status
  heartbeats are part of the machine presence model defined there.

- **[information-architecture.md](information-architecture.md)** —
  fleet service definitions and machine definitions are room-level
  state in `#bureau/fleet`. Machine status heartbeats are room-level
  state in `#bureau/machine`. Placement decisions are logged as
  timeline messages in `#bureau/fleet` for audit.

- **[tickets.md](tickets.md)** — the fleet controller creates tickets
  for decisions requiring judgment. Ticket-driven workflow is the
  primary interface between fleet management and sysadmin agents.

- **[workspace.md](workspace.md)** — workspace lifecycle (create,
  migrate, destroy) can trigger fleet actions. Creating a workspace on
  a specific machine might require waking that machine. Migrating a
  workspace triggers service re-placement.

- **[credentials.md](credentials.md)** — cloud provider API keys for
  machine provisioning are stored in encrypted credential bundles. The
  fleet controller's proxy injects them into API calls. The fleet
  controller never sees raw credentials.

- **[pipelines.md](pipelines.md)** — pipeline steps can interact with
  the fleet controller via the unix socket (requesting placement,
  checking machine status). A `publish` step can create fleet service
  definitions as state events, enabling pipeline-driven service
  deployment.

- **[authorization.md](authorization.md)** — cross-fleet preemption
  is an authorization decision: a higher-priority fleet controller
  needs a `fleet/assign` grant with targets matching the lower-priority
  controller's machines. Temporal grants can scope this to specific
  preemption windows.
