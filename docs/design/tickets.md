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
- **Status** — lifecycle state: `open`, `in_progress`, `review`,
  `blocked`, `closed`. The ticket service computes derived readiness
  from status, dependency graph, and gate satisfaction, but `blocked`
  is also a valid explicit status for agents to signal blockage not
  tracked as ticket dependencies. See [Review](#review) for the review
  status semantics.
- **Priority** — 0 (critical) through 4 (backlog).
- **Type** — `task`, `bug`, `feature`, `epic`, `chore`, `docs`,
  `question`, `pipeline`, `review_finding`. Types with dedicated
  semantics carry type-specific content (see
  [Type-Specific Content](#type-specific-content)).
  A `review_finding` is an actionable review comment — always a child
  of the ticket under review. Closing all review_finding children of a
  ticket contributes to satisfying that ticket's review gate.
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
- **Review** — reviewer list and review scope. See [Review](#review).
- **Notes** — embedded annotations. See [Notes](#notes).
- **Attachments** — artifact references. See [Attachments](#attachments).
- **Deadline** — optional target completion time (RFC 3339 UTC). The
  ticket service monitors deadlines and emits warnings when tickets
  remain open past their deadline. Deadlines are informational — they
  do not affect readiness. See
  [Deadlines](#deadlines).
- **Origin** — provenance tracking for imported tickets (source system,
  external ID, source room if moved).
- **Timestamps** — created_at, updated_at, closed_at (ISO 8601).
- **Version** — schema version for forward compatibility. Code that
  modifies a ticket checks `CanModify()` first; if the version exceeds
  what the code understands, modification is refused to prevent silent
  field loss.

### Ticket IDs

IDs use a type-derived prefix followed by a short hash derived from
room ID, creation timestamp, and title. The prefix provides visual
disambiguation — agents and operators identify a ticket's nature from
its ID alone. See [Ticket ID Prefixes](#ticket-id-prefixes) for the
type-to-prefix mapping.

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

- **`timer`** — time-based gate. Fires when the current time reaches
  the gate's target. Supports one-shot delays, absolute target times,
  and recurring schedules. See [Scheduling and Recurrence](#scheduling-and-recurrence).

- **`review`** — watches the ticket's own review field and any
  `review_finding` children. Satisfied when every reviewer in
  `TicketReview.Reviewers` has disposition `"approved"` AND all
  children of type `review_finding` have status `"closed"`. Any
  pending, changes_requested, or commented reviewer blocks
  satisfaction; any open review_finding blocks satisfaction. No
  type-specific fields — the gate implicitly watches the ticket it is
  attached to. Use for making review completion (including resolution
  of all actionable findings) a blocking readiness condition. See
  [Review](#review).

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

**Composition model:** Gates within a ticket are AND'd — all must be
satisfied for the ticket to be ready. There is no ordering between
gates on the same ticket, and gates do not gate other gates. If work
requires sequential phases where each phase has its own conditions,
model each phase as a separate ticket with `blocked_by` dependencies.
The dependency graph provides ordering; gates provide conditions within
each phase. This keeps the gate model flat and composable — a ticket
is a unit of work, not a workflow engine.

### Scheduling and Recurrence

Timer gates extend tickets into a scheduling primitive. A ticket with a
timer gate is work that should happen at a specific time or on a
recurring schedule. This replaces external cron jobs, agent sleep loops,
and ad-hoc polling with a mechanism that composes with everything tickets
already provide: dependencies, assignments, priorities, notes, and
event-driven triggers.

**Use cases:**

- **Deferral** — "come back to this Monday." A timer gate targeting
  Monday 9am. The ticket disappears from the ready set and reappears
  when the target arrives.
- **Delayed execution** — "retry in 4 hours." A timer gate targeting
  now + 4h. The agent creates the ticket and goes idle; the system
  handles the wake-up.
- **Recurring tasks** — "check system health daily at 7am." A timer
  gate with a cron schedule. The ticket cycles through
  ready → claimed → closed → re-armed automatically.
- **Maintenance windows** — "drain and update at 3am if no agents are
  active." A timer gate (schedule) combined with a state_event gate
  (activity check). Both must be satisfied.
- **Agent sleep replacement** — instead of `sleep 90`, an agent creates
  a ticket with a 90-second timer gate and goes idle. The intent is
  documented, the timer survives crashes, and the agent releases its
  sandbox while waiting.

#### Timer Gate Fields

Timer gates support both one-shot and recurring schedules through these
fields on `TicketGate`:

- **`target`** (string, RFC 3339 UTC) — absolute time at which the gate
  fires. This is the evaluation field: the service fires the gate when
  `now >= target`. When the caller provides `duration` without `target`,
  the service computes `target = base_time + duration` at creation time.
  For recurring gates, `target` is updated on each re-arm to the next
  occurrence.

- **`duration`** (string, Go duration) — relative delay. `"24h"`,
  `"30m"`, `"90s"`. Converted to an absolute `target` at gate creation
  time, using the base time determined by the `base` field.

- **`base`** (string) — what time the `duration` is measured from.
  `"created"` (default): the gate's `created_at` timestamp. `"unblocked"`:
  the time when all of the ticket's `blocked_by` dependencies were
  satisfied. The `unblocked` base is useful for soak periods that should
  start after a prerequisite completes, not from when the ticket was
  created. When `base` is `"unblocked"` and the ticket still has open
  blockers, the timer has not started — its target is undefined and it
  cannot fire.

- **`schedule`** (string, cron expression) — fixed-calendar recurrence.
  `"0 7 * * *"` = daily at 7am UTC. Standard five-field cron syntax.
  After the gate fires and the ticket is closed, the service computes
  the next occurrence from the schedule, updates `target`, and re-arms
  the gate.

- **`interval`** (string, Go duration) — drift-based recurrence.
  `"4h"` = 4 hours after the ticket is closed. Unlike `schedule`, the
  next occurrence is relative to when the current cycle completes, not
  anchored to wall-clock time.

- **`last_fired_at`** (string, RFC 3339 UTC) — when the gate last
  transitioned to `satisfied`. Set by the service on each fire.

- **`fire_count`** (int) — total number of times this gate has fired.
  Incremented on each satisfaction. Useful for observability and
  max-occurrence limits.

- **`max_occurrences`** (int) — stop recurring after this many fires.
  After the Nth fire, the gate stays satisfied and does not re-arm.
  The ticket can then be closed normally. Zero means unlimited.

`schedule` and `interval` are mutually exclusive. At least one of
`target` or `duration` must be set. A timer gate with neither `schedule`
nor `interval` is one-shot.

The service enforces a minimum recurrence period (30 seconds) to prevent
runaway loops. Anything more frequent than that is a service loop, not a
scheduled task.

#### Auto-Rearm on Close

The agent should never need to do anything special for recurrence.
Closing the ticket is the only signal the system needs.

When the ticket service processes a `close` action on a ticket with a
recurring timer gate (`schedule` or `interval` set), it performs an
atomic re-arm instead of a normal close:

1. Compute the next `target` from the schedule or interval.
2. Reset the timer gate's status to `pending`.
3. Clear `satisfied_at` and `satisfied_by`.
4. Set the ticket's status to `open` (not `closed`).
5. Clear the assignee.
6. Update `updated_at`.
7. Write the entire ticket as a single state event (`putWithEcho`).

The agent called "close" and gets back an open ticket with a pending
timer gate. From the agent's perspective, the work is done. The system
handles the recurrence. The response includes the full ticket content,
so the caller can see the re-armed state if it cares.

**Atomicity.** Because gates are embedded in the ticket content and the
entire ticket is written as a single Matrix state event, there is no
window where a watcher sees an inconsistent intermediate state (ticket
reopened but gate not yet reset, or gate reset but status still closed).
The state event that watchers receive already has both the reset gate
and the open status.

**Permanently stopping recurrence.** Remove the recurring gate first
(`remove-gate`), then close the ticket normally. The CLI provides
`bureau ticket close ID --end-recurrence` as shorthand: it removes
any recurring timer gates and closes the ticket in a single operation.

**If nobody picks it up.** For cron-style recurrence, the schedule
advances regardless of whether anyone claimed the ticket. When the next
scheduled time arrives and the ticket is already ready (still unclaimed
from the previous occurrence), the service updates the metadata
(`fire_count`, `last_fired_at`, new `target`) but leaves the gate
satisfied. The ticket stays ready. The metadata tells observers that
occurrences were missed — a ticket with `fire_count: 5` that has been
ready since Monday has missed several cycles, which is a signal for
escalation.

For interval-style recurrence, the interval cannot restart because
the previous occurrence was never completed. The ticket stays ready
indefinitely. This is correct — "4 hours between completions" means
the timer doesn't start until the current cycle finishes.

#### Evaluation: Min-Heap Timer

Timer gates are evaluated by a dedicated timer goroutine, not the /sync
loop. The service maintains a min-heap (priority queue) of pending timer
gates ordered by `target`:

- **Heap entry:** `(target, room_id, ticket_id, gate_id)`
- **Timer:** a single `clock.AfterFunc` set to the heap's minimum
  target. When it fires, the service pops the entry, verifies the gate
  is still pending, satisfies it, and resets the timer to the new
  minimum.
- **Insertion:** when a ticket is created or updated with a timer gate,
  insert into the heap. If the new entry is earlier than the current
  minimum, reset the timer.
- **Startup:** rebuild the heap from all pending timer gates across all
  rooms. Fire anything overdue immediately (crash recovery).
- **Missed cron fires:** when a recurring cron gate's target has passed
  and the ticket is already ready, advance to the next scheduled
  occurrence. Do not burst-fire missed occurrences — if the service was
  down for 3 days and the cron was daily, fire once and set the next
  target to the next future occurrence.

This gives O(log n) insertion and O(log n) fire, with zero CPU between
fires. A system with 10,000 scheduled tickets consumes no resources
until the next one is due. The heap also provides an ordered view of
upcoming fires for UI display.

#### Deferral

Deferral is syntactic sugar for a one-shot timer gate. The CLI command:

```
bureau ticket defer ID --until 2026-02-24T09:00:00Z
bureau ticket defer ID --for 3d
```

adds a timer gate with ID `"defer"` and the computed target. If a
deferral gate already exists, it updates the target. The ticket
disappears from the ready set until the target arrives.

To un-defer (make the ticket ready immediately), resolve the gate:
`bureau ticket gate resolve ID defer`.

#### Deadlines

Deadlines are a separate concept from timer gates. A timer gate says
"do this at time X." A deadline says "this should be done by time X."

The `deadline` field on `TicketContent` (RFC 3339 UTC) is the time by
which the ticket should be closed. The ticket service monitors open
tickets with deadlines: when a deadline passes with the ticket still
open, the service emits a warning (note on the ticket, log event).
Deadlines do not affect readiness — they are informational alerts, not
blocking conditions.

Deadlines compose with timer gates: a recurring ticket can have a
deadline per-occurrence (though this requires the re-arm logic to also
manage the deadline, which is a refinement beyond the initial
implementation).

### Review

Review is a ticket lifecycle status that signals "the author's work is
done; reviewers should examine it." Three use cases share the same
schema and service mechanics:

- **Code review.** A developer works in a worktree, transitions the
  ticket to `review` when ready. Reviewers examine the diff and set
  dispositions. The author iterates if changes are requested.
- **Document review.** A PM creates a ticket with a document or
  question, transitions to `review`. Engineers provide feedback via
  notes and set dispositions.
- **Agent review.** An operator asks an agent to examine a config or
  artifact. The agent reviews the scope, posts findings, and approves.

#### Schema

A ticket in review carries a `TicketReview` struct:

- **`reviewers`** — list of `ReviewerEntry` objects, each with a
  `user_id`, `disposition`, and optional `updated_at`. Disposition
  values: `"pending"` (initial), `"approved"`, `"changes_requested"`,
  `"commented"`. The reviewers list must be non-empty when the ticket
  is in review status.
- **`scope`** — optional `ReviewScope` describing what to review.
  Fields are use-case-dependent:
  - `base`, `head` — commit range for code review (git SHA or ref).
  - `worktree` — worktree path containing the changes.
  - `files` — specific files to focus on (empty means entire diff).
  - `artifact_ref` — stored artifact reference for document review.
  - `description` — freeform context for unstructured review requests.

  Callers populate whichever fields apply. No field is required — the
  scope might be "just read the ticket body."

The `Review` field lives on `TicketContent` and is present at schema
version 3. The `CanModify` guard prevents older code from silently
dropping the review field during read-modify-write.

#### Status Transitions

| From | To | Allowed | Notes |
|------|-----|---------|-------|
| open | review | yes | Direct review request |
| in_progress | review | yes | Author requests review |
| review | in_progress | yes | Author iterates after feedback |
| review | open | yes | Drop review, release to pool |
| review | closed | yes | Approved and done |
| review | blocked | yes | External blocker during review |
| review | review | rejected | Same contention rule as in_progress |
| blocked | review | yes | Resume directly into review |
| closed | review | no | Must reopen first (closed → open only) |

#### Assignee Semantics

The assignee is the work author — the person who did the work and who
will iterate if changes are requested.

- `in_progress → review` preserves the assignee.
- `review → in_progress` preserves the assignee (going back to fix).
- `review → open` clears the assignee (releasing the ticket).
- `review → closed` clears the assignee.
- Review does not require an assignee: `open → review` is valid
  without one (the PM use case where the creator is the requester, not
  the worker).

#### Dispositions

The `set-disposition` action on the ticket service requires a distinct
`ticket/review` grant. This separates review authority from edit
authority: not everyone who can edit a ticket should set review
dispositions, and not every reviewer should edit the ticket.

The service validates that the caller is in the reviewers list, that
the ticket is in review status, and that the disposition is one of the
three active values (approved, changes_requested, commented). Setting
disposition to "pending" is rejected — that is the initial state, not
something a reviewer actively chooses.

#### No Automatic Transitions

The ticket stays in "review" regardless of individual dispositions.
The author reads all feedback and decides when to go back to
in_progress (iterate) or close (ship). Automatic transitions would
take agency from the author and break cases where the author wants to
read all reviews before acting or where a post-review step follows.

#### Relationship to Gates

Gates and review serve different coordination patterns. Gates are
binary conditions: a gate is pending or satisfied, evaluated
automatically or resolved manually. Review is structured multi-reviewer
feedback with individual dispositions and iterative cycles.

A "review" gate type bridges the two. It is satisfied when all
reviewers have disposition "approved" AND all `review_finding` children
of the ticket are status "closed". This makes review completion —
including resolution of all actionable findings — a prerequisite for
downstream work. Gates and review remain orthogonal: a ticket can
have both gates and reviewers, and their satisfaction is evaluated
independently.

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

### Type-Specific Content

Some ticket types carry structured data beyond the universal lifecycle
fields. This uses a tagged-pointer pattern on `TicketContent`: optional
pointer fields whose interpretation is determined by the `Type` field.
At most one type-specific field is non-nil, and `Validate()` enforces
consistency between the `Type` value and which field is populated.

This pattern works identically in JSON (Matrix state events) and CBOR
(service socket protocol) — both encoders omit nil pointers with
`omitempty` and ignore absent fields during decode. The `CanModify`
version guard protects against field loss when new type-specific
variants are added: old code that encounters a ticket with an unknown
type-specific field harmlessly ignores it on read, and refuses to
modify it.

**Pipeline execution content** (type `"pipeline"`):

- **`pipeline_ref`** — identifies the pipeline definition to execute
  (e.g., `"bureau/pipeline:dev-workspace-init"`). Resolved by the
  executor against a pipeline room.
- **`variables`** — concrete variable values for this execution.
  Override the pipeline definition's defaults.
- **`current_step`** — 1-indexed step currently executing. Zero before
  execution starts. Updated by the executor alongside step notes.
- **`total_steps`** — total steps in the pipeline definition. Set when
  the executor resolves the pipeline.
- **`current_step_name`** — human-readable name of the current step.
- **`conclusion`** — terminal outcome: `"success"`, `"failure"`,
  `"aborted"`, or `"cancelled"`. Set when the executor closes the
  ticket. Mirrors the same field on `m.bureau.pipeline_result` for
  consumers that read the ticket directly.

Type-specific content for future ticket types (agent context, etc.)
follows the same pattern: a dedicated struct behind an optional pointer
field on `TicketContent`.

### Ticket ID Prefixes

Ticket IDs use a prefix derived from the ticket type, followed by a
short hash. The prefix mapping is a system-level convention:

- `pipeline` → `pip-`
- `context` → `ctx-`
- All other types → the room's configured prefix (default `tkt-`)

The prefix provides visual disambiguation: agents and operators can
identify a ticket's nature from its ID alone (`pip-a3f2` is a pipeline
execution, `tkt-b7c3` is a work item).

### Room Configuration

Rooms opt into ticket management via a configuration state event:

- **Event type:** `m.bureau.ticket_config`
- **State key:** `""` (singleton per room)
- **Content:** default prefix, allowed types, default labels

Configuration fields:

- **`prefix`** — default ID prefix for generic ticket types (default
  `tkt`). Types with dedicated prefixes (`pipeline` → `pip`,
  `context` → `ctx`) use their own prefix regardless of this field.
- **`allowed_types`** — which ticket types can be created in this
  room. When empty, all types are allowed. When set, only listed
  types are accepted. A workspace room might allow
  `["task", "bug", "pipeline"]`. A dedicated pipeline execution room
  might allow only `["pipeline"]`.
- **`default_labels`** — labels applied to new tickets that don't
  explicitly specify labels.

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
- Timer heap: min-heap of pending timer gates ordered by target time,
  driving the timer goroutine's `clock.AfterFunc` wakeups

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
- `in_progress -> review`: allowed (author requests review, preserves
  assignee, requires reviewers)
- `review -> in_progress`: allowed (author iterates, preserves
  assignee)
- `review -> review`: **rejected with conflict error** (same
  contention rule as in_progress)
- `review -> open`: allowed (drop review, clears assignee)
- `review -> closed`: allowed (approved and done)
- `review -> blocked`: allowed (external blocker)
- `open -> review`: allowed (direct review request, no assignee
  required, requires reviewers)
- `blocked -> review`: allowed (resume into review)
- `closed -> review`: rejected (must reopen first)
- `closed -> open`: allowed (reopen)
- `open -> closed`: allowed (duplicate, wontfix)

See [Review](#review) for the full review lifecycle semantics.

Assignee and `in_progress` status must be set atomically in the same
mutation. The service rejects `in_progress` without an assignee and
rejects setting an assignee on a ticket that is not `in_progress` or
`review`. Review may have an assignee (preserved from in_progress) but
does not require one.

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
- `upcoming-gates` — pending timer gates ordered by target time, with
  optional room filter. Returns gate metadata, ticket title, and time
  until fire. Used by UIs and PM agents to see the room's schedule.
- `status` — service health (uptime, room count, ticket count)

**Mutations:**
- `create` — new ticket, returns generated ID. Accepts `schedule`,
  `interval`, `defer_until`, and `defer_for` as top-level convenience
  fields that produce the appropriate timer gate automatically.
- `update` — modify fields on existing ticket
- `close` — close with reason. For tickets with recurring timer gates,
  performs atomic re-arm (see [Auto-Rearm on Close](#auto-rearm-on-close)).
  Accepts `end_recurrence` flag to remove recurring gates before closing.
- `reopen` — reopen closed ticket
- `defer` — add or update a deferral timer gate on a ticket. Accepts
  `until` (absolute time) or `for` (duration).
- `batch-create` — create a DAG of tickets with symbolic refs, resolved
  to real IDs server-side

**Notes:**
- `add-note` — append a note
- `remove-note` — remove a note by ID

**Gates:**
- `add-gate` — append a gate
- `remove-gate` — remove a gate by ID
- `resolve-gate` — manually satisfy a gate (for `human` type)

**Review:**
- `set-disposition` — set a reviewer's disposition on a ticket in
  review status. Requires `ticket/review` grant. The caller must be in
  the ticket's reviewer list.

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
                       [--schedule CRON] [--interval DUR]
                       [--defer-until TIME] [--defer-for DUR]
                       [--deadline TIME]
bureau ticket list     [--status S] [--priority N] [--label L] [--assignee A]
                       [--parent ID] [--room ROOM]
bureau ticket show     ID
bureau ticket update   ID [--status S] [--priority N] [--assign A]
                       [--label-add L] [--label-remove L] [--parent ID]
                       [--deadline TIME] [--reviewer USER-ID]
bureau ticket close    ID [--reason "..."] [--end-recurrence]
bureau ticket reopen   ID
bureau ticket ready    [--room ROOM]
bureau ticket blocked  [--room ROOM]
bureau ticket children ID
bureau ticket grep     PATTERN [--room ROOM]
bureau ticket upcoming [--room ROOM]
bureau ticket review   ID --approve | --request-changes | --comment

bureau ticket defer      ID --until TIME | --for DUR
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

`--schedule`, `--interval`, `--defer-until`, and `--defer-for` on
`create` are convenience flags that produce the appropriate timer gate
automatically. `--schedule` and `--interval` create a recurring gate;
`--defer-until` and `--defer-for` create a one-shot deferral gate.
`--deadline` sets the ticket-level deadline field.

`bureau ticket defer` adds or updates a deferral timer gate on an
existing ticket. If the ticket already has a gate with ID `"defer"`,
the target is updated. Otherwise a new gate is created.

`bureau ticket upcoming` lists pending timer gates across the room,
ordered by target time. Shows gate metadata, ticket title, and time
until fire. Useful for PM agents and operators reviewing the room's
schedule.

`bureau ticket close --end-recurrence` removes any recurring timer
gates from the ticket before closing. Without this flag, closing a
ticket with a recurring gate performs auto-rearm.

`--reviewer` on `update` populates the ticket's review field with
the specified reviewer user IDs (each with disposition "pending"). When
combined with `--status review`, this is the common one-step pattern
for requesting review. The flag is repeatable for multiple reviewers.

`bureau ticket review` sets the caller's disposition on a ticket in
review status. Exactly one of `--approve`, `--request-changes`, or
`--comment` must be specified. The caller must be in the ticket's
reviewer list.

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

## Pipeline Execution

Pipeline executions are tickets. A pipeline definition
(`m.bureau.pipeline` state event in a pipeline room) is a template.
Each invocation of that pipeline is a ticket of type `"pipeline"` with
a `pip-` prefixed ID, carrying the pipeline ref and variables in its
type-specific content. The ticket's lifecycle IS the execution's
lifecycle.

### Execution Flow

1. Something creates a `pip-` ticket: a human via the CLI, an agent,
   another pipeline's publish step, or the ticket service's auto-rearm
   on a recurring schedule. The ticket has `type: "pipeline"`, status
   `open`, and pipeline-specific content (ref, variables).

2. The ticket service validates and indexes the ticket. Gate evaluation
   runs. When all gates are satisfied and no blockers remain, the
   ticket is ready.

3. The ticket state event update flows through /sync. A pipeline
   executor template on the target machine has a StartCondition
   matching ready pipeline tickets (type, status, room).

4. The daemon fires the trigger, starts a sandbox with the executor
   binary, and passes the ticket content as `trigger.json`.

5. The executor reads the ticket content, transitions it to
   `in_progress` (atomic claim with contention detection — if another
   executor on another machine got there first, the transition fails
   and this executor exits).

6. The executor resolves the pipeline definition, runs steps
   sequentially, and posts each step's outcome as a note on the
   ticket. The `current_step` and `current_step_name` fields in the
   type-specific content are updated alongside each note.

7. On success: the executor sets `conclusion: "success"` in the
   pipeline content and closes the ticket. On failure: sets
   `conclusion: "failure"` and closes. On abort (precondition
   mismatch): sets `conclusion: "aborted"` and closes.

8. The executor publishes an `m.bureau.pipeline_result` state event
   (keyed by the ticket ID) with the detailed step-level results.
   This companion event is the machine-readable record that pipeline
   gates evaluate against.

No new components are required. The ticket service manages ticket
lifecycle (which it already does). The daemon's StartCondition
mechanism triggers execution (which it already does). Pipeline
execution emerges from their composition.

### Cancellation

Closing a pipeline ticket externally is a cancellation signal:

- **Pre-execution**: the `pip-` ticket is still `open`. Closing it
  prevents execution — no executor will claim a closed ticket.

- **In-progress**: the executor watches its ticket via /sync. When it
  sees the ticket closed externally, it kills the current step's
  process group, sets `conclusion: "cancelled"` (distinct from
  `"aborted"`, which means a precondition mismatch), publishes the
  result event, and exits. The close reason on the ticket carries
  context ("superseded by pip-b7c3", "operator requested stop").

### Recurring Pipelines

Scheduled pipeline execution uses timer gates on pipeline tickets.
A pipeline ticket with a recurring timer gate cycles through
executions:

1. Timer gate fires → ticket is ready.
2. Executor claims it → `in_progress`.
3. Executor completes → closes with result.
4. Auto-rearm on close → ticket reopens with a new timer target.

Each cycle is an execution. The Matrix timeline preserves every version
of the ticket state event, providing a complete history of every run.
The contention detection on `open → in_progress` prevents
double-execution if the timer fires while a previous run is still
going.

For use cases that require per-run identity (parallel executions,
per-run dependency chains, explicit audit), a scheduler principal
creates child instance tickets. The schedule ticket is the parent;
each instance ticket has `parent: <schedule-ticket-id>`. The
`bureau ticket children` query lists all runs of a schedule.

### Relationship to Pipeline Results

The `m.bureau.pipeline_result` state event continues to exist as a
companion to the pipeline ticket. The ticket is the lifecycle
container: scheduling, assignment, dependencies, human-readable
progress. The result event is the machine-readable output: per-step
timing, outputs, error details, log thread link.

The result event gains a `ticket_id` field referencing the `pip-`
ticket. This creates a bidirectional link:

- Ticket → result: the pipeline ref and ticket ID locate the result.
- Result → ticket: the `ticket_id` field links back.

Pipeline gates can match on result events (unchanged) or use ticket
gates ("wait for this pipeline ticket to close"). Both mechanisms
compose with the existing gate evaluation model.

### Storage Evolution

The ticket service is the mandatory read/write path for all ticket
operations. Consumers interact with tickets exclusively through the
service's socket API. This positions the system for a future storage
migration: the ticket service can move ticket storage out of Matrix
state events (into a local database, a dedicated store, or a
content-addressed system) while keeping room-level references for
authorization and discovery. Because all consumers already go through
the service, this migration is transparent to agents and the CLI.

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
- [pipelines.md](pipelines.md) — pipeline definitions are templates;
  pipeline executions are tickets. Pipeline `publish` steps can create
  or update tickets. Pipeline gates watch for
  `m.bureau.pipeline_result` events. See
  [Pipeline Execution](#pipeline-execution) for the unified model.
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
- [stewardship.md](stewardship.md) — stewardship extends review gates
  with threshold semantics and tiered escalation. The `affects` field
  on tickets connects work items to stewardship declarations. The
  ticket service maintains a stewardship index alongside its ticket
  index, auto-configures review gates from matched stewardship
  policies, and posts digest notifications for informational tickets.
