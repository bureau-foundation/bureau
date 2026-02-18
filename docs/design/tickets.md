# Tickets

[fundamentals.md](fundamentals.md) establishes Bureau's primitives;
[information-architecture.md](information-architecture.md) defines the
Matrix data model of spaces, rooms, and state events. This document
describes Bureau's coordination primitive for tracking work across
agents, services, and humans — a Matrix-native ticket system that
integrates directly with triggers, pipelines, service discovery, and
sandboxed execution.

---

## Why Tickets

Bureau agents need work items. External issue trackers (GitHub Issues,
Jira, local file-based trackers) create friction in an agent-driven
system:

- **Sharing is manual.** File-based trackers require explicit
  synchronization. Bureau already has a real-time persistence layer
  (Matrix) with scoped access control.
- **No workflow integration.** External trackers have no concept of
  Bureau triggers, start conditions, or agent lifecycle. Bureau has all
  of these — tickets should participate in the same event-driven model.
- **Agent ergonomics are poor.** CLI flags and command surfaces vary
  across trackers. Agents frequently fail to use them correctly.
- **Performance.** File-based trackers re-serialize the entire dataset
  on every mutation. Bureau needs sub-millisecond reads for agents
  querying work in tight loops.

Bureau tickets store work items as Matrix state events, queried through
a dedicated service, with a CLI and proxy-accessible socket that agents
use directly. The persistence is Matrix. The query layer is in-memory.
The access pattern is a Unix socket per sandbox.

---

## Data Model

### State Event

Each ticket is a Matrix state event in a room:

- **Event type:** `m.bureau.ticket`
- **State key:** ticket ID (e.g., `tkt-a3f9`)
- **Room:** any room with ticket management enabled

The state key is the ticket's identity. Matrix guarantees that for any
(room_id, event_type, state_key) tuple, only the current version is
returned by state queries. Historical versions are preserved in the room
timeline, providing a complete audit trail of every field change.

### Content

A ticket carries:

- **Title** and **body** (markdown) — the work description.
- **Status** — lifecycle state: `open`, `in_progress`, `blocked`,
  `closed`. The ticket service computes derived readiness from status,
  dependency graph, and gate satisfaction, but `blocked` is also a valid
  explicit status for agents to signal blockage not tracked as ticket
  dependencies.
- **Priority** — 0 (critical) through 4 (backlog).
- **Type** — `task`, `bug`, `feature`, `epic`, `chore`, `docs`,
  `question`.
- **Labels** — free-form tags for filtering and grouping.
- **Assignee** — single Matrix user ID. Bureau agents are principals
  with unique identities; one agent works one ticket. Multiple people
  on something means sub-tickets (parent-child). "Watchers" or
  "participants" are room membership — everyone in the room sees all
  tickets.
- **Parent** — single ticket ID for epic-to-subtask hierarchy. The
  ticket service computes children sets from the reverse mapping.
- **BlockedBy** — ticket IDs that must close before this ticket is
  ready. The ticket service computes the transitive closure for
  dependency queries and detects cycles on mutation.
- **Gates** — async coordination conditions. See [Gates](#gates).
- **Notes** — embedded annotations. See [Notes](#notes).
- **Attachments** — artifact references. See [Attachments](#attachments).
- **Origin** — provenance tracking for imported tickets (source system,
  external ID, source room if moved).
- **Timestamps** — created_at, updated_at, closed_at (ISO 8601).
- **Version** — schema version for forward compatibility. Code that
  modifies a ticket checks `CanModify()` first; if the version exceeds
  what the code understands, modification is refused to prevent silent
  field loss.

### Ticket IDs

IDs use the prefix `tkt-` followed by a short hash derived from room
ID, creation timestamp, and title. The prefix is unambiguous: agents
won't confuse `tkt-a3f9` with a commit hash or a GitHub issue number.
The prefix is configurable per room via the room's ticket config state
event.

### Dependencies and Parent Hierarchy

Tickets have two orthogonal relationship types, both embedded in the
ticket content:

- **`blocked_by`** — "must finish before." Ticket B lists ticket A in
  its `blocked_by`; B is not ready until A is closed. The ticket service
  computes the transitive closure and detects cycles on mutation.
- **`parent`** — "is subtask of." Ticket B has parent ticket A (an
  epic). The ticket service computes children sets from the reverse
  mapping for progress tracking ("5 of 8 subtasks closed").

The semantic distinction matters: `blocked_by` is a sequencing
constraint, `parent` is an organizational hierarchy. A subtask can be
blocked by unrelated tickets without changing its parent.

Single parent (not an array) because a subtask belongs to one epic.
Cross-cutting relationships use `blocked_by`.

### Readiness

A ticket is "ready" when:

- Its status is `open`
- All tickets in its `blocked_by` list have status `closed`
- All gates have status `satisfied`

Missing blockers (IDs not present in the index) are treated as
unresolved — the ticket is not ready. A dangling reference means
something is wrong, not that the dependency is satisfied.

### Gates

Gates are async coordination conditions embedded in the ticket. They
extend the readiness model beyond ticket dependencies to external
events: pipeline completions, human approvals, timer expirations, and
arbitrary Matrix state event matches.

**Why embedded?** One state event = complete ticket truth. The ticket
service writes the whole ticket atomically when a gate clears. Queries
return complete state without joins. Separate state events per gate
would allow gate updates without rewriting the ticket, but creates race
windows between gate satisfaction and ticket readiness, requires join
queries for complete state, and adds /sync event proliferation. Since
the ticket service is the authoritative write path, atomic rewrite of
the whole ticket is the natural operation.

**Gate types:**

- **`human`** — no automatic evaluation. Resolved explicitly via the
  CLI or socket API. The service records who approved and when. Use for
  approval workflows, manual verification, sign-off checkpoints.

- **`pipeline`** — watches for `m.bureau.pipeline_result` state events
  matching `pipeline_ref` and `conclusion`. Syntactic sugar for a
  `state_event` gate. Use for CI checks, build verification, deployment
  readiness.

- **`state_event`** — the general-purpose gate. Watches for any Matrix
  state event matching `event_type` + `state_key` + `room_alias` +
  `content_match`. Same condition semantics as StartCondition. Pipeline
  and ticket gates are special cases. Use for webhook deliveries, service
  health signals, custom approval flows.

- **`ticket`** — watches for another ticket (by ID, same room)
  transitioning to `closed`. Syntactic sugar for a `state_event` gate.
  Use for milestone gates.

- **`timer`** — checks whether current time exceeds `created_at +
  duration` on each /sync tick. No external event needed. Use for soak
  periods, cooldown intervals, scheduled reviews.

**Evaluation flow:** The ticket service's /sync loop delivers state
event updates. The service checks all pending gates across all tickets
in the affected room. For each matching gate, it transitions status to
`satisfied`, records when and what satisfied it, re-PUTs the ticket
with updated gate state. The ticket state event change flows through
/sync to all consumers. StartConditions on other principals that watch
ticket events fire normally.

This is the key difference from polling-based systems. Bureau's /sync
loop is the evaluator. Gate satisfaction is event-driven end-to-end:
something happens, a state event appears, the ticket service sees it,
the gate clears, the ticket updates, downstream agents trigger.

### Notes

Notes are short annotations that travel with the ticket: warnings,
references, analysis results, review findings. An agent reading a
ticket gets all actionable context in a single read, not by discovering
and paginating through a thread.

**Why embedded, not Matrix threads?** Three properties:

- **Single-read completeness.** The ticket state event contains all
  notes. No second round trip to fetch a thread.
- **No wake-up noise.** State events are deduplicated by state key in
  /sync. A security scanner adding notes to 40 tickets produces 40
  state event updates, not 40 x N timeline events. Agents with
  StartConditions won't re-trigger because their ContentMatch fields
  (status, labels) haven't changed.
- **Append-only simplicity.** Notes can be added and removed but not
  edited. The ticket service assigns sequential IDs and timestamps.

Matrix threads remain available for unstructured conversation about a
ticket. Notes are for annotations that agents should see when they pick
up work.

**Size budget:** Ticket state events are well within Matrix's 65KB
state event limit even with heavy annotation. For content that exceeds
a few hundred bytes — full stack traces, log excerpts, long analysis —
store it as an artifact and reference it from a note.

### Attachments

Attachments are references to content stored outside the ticket state
event. The ticket holds structured references (artifact ref, label,
MIME type); the content lives in the artifact service. Agents fetch
attachment content on demand via the artifact service socket.

All blob data flows through the artifact service — Matrix is never
used as a blob store. See [artifacts.md](artifacts.md).

### Room Configuration

Rooms opt into ticket management via a configuration state event:

- **Event type:** `m.bureau.ticket_config`
- **State key:** `""` (singleton per room)
- **Content:** ID prefix (default `tkt`), default labels

The ticket service checks for this event to determine whether a room
has ticket management enabled. No event means no tickets in that room.

---

## Room Service Binding

Rooms declare which service instance manages their tickets via a
general-purpose state event:

- **Event type:** `m.bureau.room_service`
- **State key:** service role (e.g., `"ticket"`)
- **Content:** the principal that provides the service for this room

This is not ticket-specific. The same mechanism works for any
room-scoped service (artifact cache, RAG, CI, code review). The state
key is the service role; different rooms can point to different service
instances for the same role.

Any room member can read this state event to discover which service
handles tickets for that room. The daemon reads it when setting up
sandboxes to determine which service sockets to bind-mount.

---

## The Ticket Service

`bureau-ticket-service` is a standalone Bureau service principal — it
has a Matrix account, a Unix socket, and registers in `#bureau/service`.
It owns the full ticket lifecycle for its scope.

### Scope

The ticket service's scope is defined by its Matrix room membership.
It watches rooms it's been invited to and indexes tickets in those
rooms. It doesn't know or care about spaces, machines, or
organizational hierarchy — it just has rooms.

Common deployment patterns:

- **Per space:** a ticket service invited to all rooms in a space sees
  all tickets for that project.
- **Per operator:** a ticket service invited to rooms across multiple
  spaces provides a unified cross-project view.
- **Small deployment:** one instance invited to everything.

The service doesn't enforce which pattern is used. Room membership
defines scope.

### Architecture

```
Matrix homeserver
    ^ /sync (filtered to ticket + service events)
    |
bureau-ticket-service
    |-- In-memory ticket index (per-room maps, dependency graph, ready set)
    |-- Gate evaluator (watches /sync for gate condition matches)
    |-- Matrix write path (PUT state events for mutations)
    '-- Unix socket server (CBOR request-response)
         |
         '-- /run/bureau/service/ticket/<scope>.sock
```

The service has its own Matrix account and runs its own /sync loop. It
does not depend on the daemon for event delivery — Matrix is the event
bus. The daemon and the ticket service independently consume events from
the same rooms.

### Write Path

Agents and the CLI send mutations to the ticket service, not directly
to Matrix. The service:

1. Validates (dependency cycles, field constraints, ticket existence,
   schema version compatibility)
2. Generates IDs for new tickets
3. Enriches (auto-timestamps)
4. Writes to Matrix (PUT state event — the authoritative persistence)
5. Updates its own cache immediately (the creator sees the result
   without waiting for a /sync round-trip)

Making the ticket service the authoritative write path means it can
enforce invariants and batch operations atomically from the caller's
perspective.

### Read Path

All queries go through the service. The in-memory index serves
structured queries (list, ready, blocked, grep) in microseconds. For
a room with 5,000 tickets, the index is roughly 10-20 MB and queries
are in-memory lookups.

### In-Memory Index

The service maintains per-room indexed views, rebuilt incrementally on
each /sync update:

- Primary store: ticket ID to content
- Secondary indexes by status, priority, label, assignee, type
- Parent-child hierarchy: parent to children reverse map
- Dependency graph: forward (`blocked_by`) and reverse (`blocks`) edges
- Ready set: open tickets with no open blockers and all gates satisfied
- Gate watch map: pending gates mapped to the state event conditions
  they watch for

When /sync delivers an `m.bureau.ticket` state event, the index
updates: parse the content, update the primary store, update affected
secondary indexes, recompute readiness if dependencies, gates, parent,
or status changed.

When /sync delivers any other state event in a watched room, the
service checks the gate watch map for pending gates whose conditions
match the event. For each match, it updates the gate status and re-PUTs
the ticket.

Full-text search (`grep`) is a linear scan with regex matching over
title, body, and note bodies. Sub-millisecond for thousands of tickets.
An inverted index is not needed until hundreds of thousands of tickets.

### Status Transition Contention

The ticket service rejects `open -> in_progress` transitions when the
ticket is already `in_progress`. The error response includes the
current assignee so the caller knows who claimed the ticket first.

This is a contention detection mechanism. Without it, two agents can
both read a ticket as `open`, both begin planning (which may take
minutes of LLM time and significant cost), and only discover the
conflict when one tries to push results. The wasted work scales with
how long the planning phase takes.

The operational rule this enforces: **claim before you plan.** An agent
must transition the ticket to `in_progress` (with itself as assignee)
as its very first action, before reading the ticket body, before
exploring the codebase, before any planning. If the transition fails,
the agent knows immediately that someone else is working on it and can
move to the next ready ticket.

Specific transition rules:

- `open -> in_progress`: allowed, must include assignee
- `in_progress -> in_progress`: **rejected with conflict error**
  (includes current assignee)
- `in_progress -> open`: allowed (unclaim, clears assignee)
- `in_progress -> closed`: allowed (agent finished)
- `in_progress -> blocked`: allowed (agent hit a blocker)
- `closed -> open`: allowed (reopen)
- `open -> closed`: allowed (duplicate, wontfix)

Assignee and `in_progress` status must be set atomically in the same
mutation. The service rejects `in_progress` without an assignee and
rejects setting an assignee on a ticket that is not `in_progress`.

### Service API

Over the Unix socket, CBOR request-response (see
[Wire Protocol](#wire-protocol)):

**Queries:**
- `list` — filtered query (status, priority, labels, assignee, type,
  parent, room)
- `ready` — open tickets with no open blockers and all gates satisfied
- `blocked` — open tickets with open blockers or unsatisfied gates
- `show` — single ticket by ID (includes notes, gates, attachments)
- `children` — subtasks of a parent ticket (with progress summary)
- `grep` — full-text search across title, body, and notes
- `stats` — counts by status, priority, type
- `deps` — dependency graph for a ticket (transitive closure)
- `status` — service health (uptime, room count, ticket count)

**Mutations:**
- `create` — new ticket, returns generated ID
- `update` — modify fields on existing ticket
- `close` — close with reason
- `reopen` — reopen closed ticket
- `batch-create` — create a DAG of tickets with symbolic refs, resolved
  to real IDs server-side

**Notes:**
- `add-note` — append a note
- `remove-note` — remove a note by ID

**Gates:**
- `add-gate` — append a gate
- `remove-gate` — remove a gate by ID
- `resolve-gate` — manually satisfy a gate (for `human` type)

**Attachments:**
- `attach` — add an artifact reference
- `detach` — remove an attachment by ref

### Batch Creation

The batch endpoint accepts a list of tickets with symbolic
back-references:

```json
[
  {"title": "Set up auth", "type": "task", "ref": "a"},
  {"title": "Implement login", "type": "task", "ref": "b", "blocked_by": ["a"]},
  {"title": "Write tests", "type": "task", "ref": "c", "blocked_by": ["b"]}
]
```

The service resolves symbolic refs to real IDs, sends all state events,
and returns a mapping of symbolic refs to generated ticket IDs. One call
from the agent's perspective, regardless of how many tickets or
dependencies are in the batch.

---

## Service Access

Agents access the ticket service through a Unix socket bind-mounted
into the sandbox at `/run/bureau/service/ticket.sock`. The template
declares `ticket` in its `required_services`; the daemon resolves the
role to a concrete service principal via `m.bureau.room_service` state
events and mounts the socket at sandbox creation time. See
[architecture.md](architecture.md) for the general service socket
pattern, resolution mechanism, and wire protocol.

---

## CLI

`bureau ticket` subcommands follow `gh`-like flag conventions — agents
familiar with `gh issue` should get the flags right on the first
attempt:

```
bureau ticket create   --title "..." --body "..." [--priority N] [--type TYPE]
                       [--label L] [--parent ID] [--room ROOM]
bureau ticket list     [--status S] [--priority N] [--label L] [--assignee A]
                       [--parent ID] [--room ROOM]
bureau ticket show     ID
bureau ticket update   ID [--status S] [--priority N] [--assign A]
                       [--label-add L] [--label-remove L] [--parent ID]
bureau ticket close    ID [--reason "..."]
bureau ticket reopen   ID
bureau ticket ready    [--room ROOM]
bureau ticket blocked  [--room ROOM]
bureau ticket children ID
bureau ticket grep     PATTERN [--room ROOM]

bureau ticket dep add    ID DEPENDS-ON-ID
bureau ticket dep remove ID DEPENDS-ON-ID

bureau ticket note add    ID --body "..."
bureau ticket note list   ID
bureau ticket note remove ID NOTE-ID

bureau ticket gate add     ID --type TYPE --id GATE-ID [type-specific flags]
bureau ticket gate list    ID
bureau ticket gate resolve ID GATE-ID [--reason "..."]
bureau ticket gate remove  ID GATE-ID

bureau ticket attach  ID FILE [--label "..."]
bureau ticket attach  ID --ref REF [--label "..."]
bureau ticket detach  ID REF

bureau ticket batch   --file FILE
bureau ticket import  --jsonl FILE [--room ROOM]
bureau ticket export  --jsonl FILE [--room ROOM]
```

The CLI talks to the ticket service's Unix socket. The `--room` flag
scopes to a specific room; without it, the CLI infers the room from
workspace context or configuration.

---

## Workflow Integration

### Tickets as StartCondition Triggers

Bureau's StartCondition mechanism works with tickets without
modification. A principal assignment can gate on ticket events:

```json
{
  "start_condition": {
    "event_type": "m.bureau.ticket",
    "room_alias": "#project/triage:bureau.local",
    "content_match": {
      "status": "open",
      "labels": "needs-triage"
    }
  }
}
```

When a ticket matching the condition appears, the daemon fires the
trigger and starts the principal with the ticket content as
`trigger.json`. The agent reads `EVENT_title`, `EVENT_body`,
`EVENT_priority`, does its work, and updates the ticket via the ticket
service socket.

ContentMatch supports array containment: `{"labels": "needs-triage"}`
means "the labels array contains the string 'needs-triage'." This is
the general semantics for matching against array fields — if the actual
value is an array, the criterion matches if any element satisfies it.

### Event-Driven Agent Lifecycle

Tickets enable an event-driven pattern where agents don't poll or stay
alive waiting:

1. Agent creates a ticket with `labels: ["waiting-ci"]` and exits
2. A CI watcher service updates the ticket when CI completes
3. A review agent has a StartCondition matching tickets with the
   updated status
4. The daemon starts the review agent with the ticket as trigger
5. The review agent does its work, updates the ticket, exits

No agent runs during the CI wait. The daemon watches. Triggers fire.
Agents start fresh with context from the ticket.

### Tickets as Persistent Agent Context

A ticket is a resumable work context:

- **What am I working on?** Read my assigned ticket.
- **What's my next step?** It's in the ticket body or a note.
- **What happened before I started?** The ticket's Matrix timeline
  shows every previous state event version.
- **Who else is involved?** Room membership and thread messages.

An agent that crashes and restarts reads its assigned ticket and
resumes. An agent handing off to another updates the ticket with its
findings; the next agent starts with accumulated context.

### Workflow Graphs

A workflow like triage, reproduce, fix, test, review, commit emerges
from StartConditions across principals. Each transition is a ticket
status or label change. Each agent reads the ticket's accumulated
context. The graph isn't hardcoded — it emerges from the conditions
configured across principal templates.

---

## Fleet Deployment

The ticket service is a principal. It gets a `PrincipalAssignment` in
a machine config room, exactly like any other principal. An operator
command sets it up:

```
bureau ticket enable --space iree --host workstation
```

This registers the service's Matrix account, publishes a
PrincipalAssignment to the appropriate machine config room, sets
`m.bureau.room_service` state events in the target rooms, and invites
the service to those rooms.

Only one machine's daemon sees the assignment — machine affinity
prevents racing between daemons. The daemon manages room membership
for the service the same way it manages it for any principal: when new
rooms are created in a space, the daemon invites services with matching
scope.

---

## Migration

The ticket service supports import from external trackers. The primary
import format is JSONL:

```
bureau ticket import --jsonl issues.jsonl --room ROOM
```

This reads the JSONL, maps fields to ticket content, generates
`tkt-`-prefixed IDs, preserves dependency relationships (mapping old
IDs to new), and creates state events in the target room. The `origin`
field on imported tickets records the source system and external ID for
traceability.

Export is the reverse:

```
bureau ticket export --jsonl tickets.jsonl --room ROOM
```

---

## Relationship to Other Design Documents

- [information-architecture.md](information-architecture.md) — tickets
  are room-level state (Level 2). Thread messages within ticket rooms
  carry agent session history and discussion (Level 3).
- [architecture.md](architecture.md) — the ticket service is a service
  principal, discoverable via `#bureau/service`, reachable via Unix
  socket. Service resolution flows through `m.bureau.room_service`
  state events and the daemon's sandbox creation path.
- [pipelines.md](pipelines.md) — pipeline `publish` steps can create
  or update tickets. Pipeline gates watch for
  `m.bureau.pipeline_result` events.
- [artifacts.md](artifacts.md) — ticket attachments reference content
  stored in the artifact service. Large blobs live there, not in the
  ticket state event.
- [workspace.md](workspace.md) — workspace rooms can have tickets. The
  workspace lifecycle can trigger ticket operations (auto-close on
  teardown, create setup tickets on workspace creation).
- [authorization.md](authorization.md) — the ticket service uses
  Bureau's authorization framework. Room power levels control which
  state events the service can write; Bureau grants control which
  principals can perform ticket operations. Service identity tokens
  authenticate callers on every socket request.
