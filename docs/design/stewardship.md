# Stewardship

[fundamentals.md](fundamentals.md) establishes that Bureau runs
organizations of principals; [authorization.md](authorization.md)
defines who is *permitted* to act; [tickets.md](tickets.md) defines
the coordination primitive for tracking work. This document defines
Bureau's governance layer: who is *responsible* for which resources,
what involvement is required before changes proceed, and how the
system routes decisions through the cheapest layer first before
escalating to humans.

---

## Why Stewardship

Bureau's authorization framework answers: "Is this principal permitted
to perform this action?" A sysadmin agent with `fleet/**` grants can
scale the GPU fleet. A coder with `ticket/create` grants can file
tickets anywhere it has room membership. But permission is not the
same as governance. The sysadmin *can* scale the fleet, but *should*
it do so without consulting the project leads whose resource
allocations will change? The coder *can* file a ticket about schema
changes, but *should* the schema stewards know about it?

Authorization is binary: permitted or denied. Governance is social:
whose involvement is required, whose input is valuable, and who
should be notified? Authorization is enforced at request time by the
daemon, proxy, and services. Governance is enforced at coordination
time by the ticket service through review gates.

Without a governance layer, the answer to "who should approve this?"
lives in human memory, agent instructions, and ad-hoc convention.
Conventions drift. New agents don't know them. Existing agents can't
discover them at runtime. The result is either everything escalates to
the operator (who drowns in noise) or nothing does (and autonomy means
uncoordinated action).

Stewardship fills this gap with a declarative, room-scoped,
machine-queryable mapping from resources to responsible principals,
with policy about what "responsible" means for each resource.

---

## Design Principles

**Stewardship complements authorization.** Grants determine what a
principal *can* do. Stewardship determines whose *involvement is
required* before the action proceeds. Both must be satisfied: a
principal without a `fleet/provision` grant is denied regardless of
stewardship, and a principal with the grant but whose ticket hasn't
cleared the stewardship review gate is authorized but gated. The two
systems are evaluated at different times by different components and
do not interfere with each other.

**Room membership is the visibility boundary.** Stewardship
declarations are Matrix state events in rooms. Principals see
stewardship policies only for rooms they are members of. Project A's
resource governance is invisible to Project B unless they share a
room. Shared resources (fleet capacity, common services) have
stewardship declared in shared rooms (`#bureau/fleet`,
`#bureau/service`). No separate ACL system is needed.

**One pattern language everywhere.** Resource patterns use the same
hierarchical `/`-separated glob syntax as principal localparts,
authorization actions, and room aliases. `fleet/gpu/**` matches all
GPU fleet resources. `workspace/lib/schema/**` matches paths within a
workspace. `service/api/ticket/**` matches the ticket service API
surface. No new syntax.

**Ticket type determines involvement mode.** The same stewardship
declaration handles both "this change needs approval" and "these
stewards should know about this." The ticket's type drives which mode
activates. A review ticket touching stewarded paths triggers a review
gate; a bug report touching the same paths triggers a notification. Stewards control their own noise level by declaring
which ticket types gate vs. notify.

**Escalation reduces operator noise.** Stewardship tiers define a
decision-making hierarchy per resource. Agent stewards are tried
first. Operators are notified only when they are the last pending
approval. Most governance decisions resolve at the agent tier. The
operator sees only what agents couldn't resolve, with full context
about who approved, who didn't, and why.

---

## The Data Model

### State Event

Stewardship declarations are Matrix state events:

- **Event type:** `m.bureau.stewardship`
- **State key:** a resource identifier (e.g., `fleet/gpu`,
  `workspace/lib/schema`, `service/api/ticket`)
- **Room:** any room with ticket management enabled

The state key is a short, human-readable identifier for the resource
being stewarded. It serves as the primary key for stewardship lookup
and appears in audit logs and ticket context. The state key need not
be globally unique -- two rooms can have a stewardship declaration
with the same state key, and each scopes to its own room's context.

### Content

A stewardship declaration carries:

- **ResourcePatterns** -- a list of glob patterns defining the
  resource scope. A ticket's `affects` entries are matched against
  these patterns. Multiple patterns allow a single stewardship
  declaration to cover related resources (e.g., both
  `workspace/lib/schema/**` and `workspace/lib/ticketindex/**` for
  data structure stewards).

- **Description** -- human-readable explanation of what this resource
  is and why it has stewards. Agents reading stewardship policies can
  use this to understand the governance context without external
  knowledge.

- **GateTypes** -- ticket types that trigger a review gate when the
  ticket affects this resource. Common entries: `review`,
  `resource_request`, `access_request`, `deployment`,
  `credential_rotation`. When a ticket with a matching type declares
  `affects` entries that match any pattern in `ResourcePatterns`, the
  ticket service auto-configures a review gate from the stewardship
  tiers.

- **NotifyTypes** -- ticket types that trigger steward notification
  without a gate. Common entries: `bug`, `question`, `investigation`,
  `discussion`. Stewards are informed but not required to approve.

- **Tiers** -- ordered list of reviewer tiers. Each tier specifies
  which principals are involved, the approval threshold, and when the
  tier activates. See [Tiers and Escalation](#tiers-and-escalation).

- **OverlapPolicy** -- how this declaration composes with other
  declarations matching the same ticket. `"independent"` (default)
  means this declaration produces its own review gate that must be
  satisfied regardless of other declarations. `"cooperative"` means
  this declaration's reviewers are pooled with other cooperative
  declarations into a single merged gate. See
  [Gating mode](#gating-mode) for the full semantics.

- **Version** -- schema version, following the standard `CanModify`
  pattern from [information-architecture.md](information-architecture.md).

Ticket types not listed in either `GateTypes` or `NotifyTypes` do not
activate the stewardship at all. This is how high-traffic resources
avoid triggering stewardship on every ticket: a stewardship
declaration for `workspace/lib/` might gate on `review` and
`deployment`, notify on `bug`, and ignore `task`, `chore`, and `docs`
entirely.

### Example

A stewardship declaration for GPU fleet resources in
`#bureau/fleet`:

```json
{
  "type": "m.bureau.stewardship",
  "state_key": "fleet/gpu",
  "content": {
    "resource_patterns": ["fleet/gpu/**"],
    "description": "GPU fleet capacity and allocation across projects",
    "gate_types": ["resource_request", "deployment"],
    "notify_types": ["bug", "question"],
    "tiers": [
      {
        "principals": ["iree/amdgpu/pm", "other-project/pm", "bureau/dev/tpm"],
        "threshold": 2,
        "escalation": "immediate"
      },
      {
        "principals": ["ben"],
        "threshold": 1,
        "escalation": "last_pending"
      }
    ],
    "version": 1
  }
}
```

A resource request ticket with `affects: ["fleet/gpu/a100"]` in a
room where the requester is a member of `#bureau/fleet` triggers the
full stewardship flow: a review gate requiring 2-of-3 project leads,
escalating to the operator only if needed.

---

## Resource Patterns

Resources are identified by hierarchical patterns using the same glob
syntax as authorization actions and principal localparts. The pattern
space is open-ended -- any `/`-separated identifier can be a resource.
Bureau defines conventions for common resource categories; projects
extend the namespace as needed.

### Conventions

- **`fleet/<partition>/**`** -- fleet capacity and machine resources.
  `fleet/gpu/a100`, `fleet/cpu/batch`, `fleet/network/egress`.
  Stewardship in fleet rooms governs scaling, rebalancing, and
  resource reallocation.

- **`service/api/<role>/**`** -- service API surfaces.
  `service/api/ticket/create`, `service/api/artifact/tag`.
  Stewardship in service rooms governs API changes that affect
  consumers.

- **`workspace/<path>/**`** -- paths within workspaces. Workspaces
  are git worktrees, but their content is not necessarily source code:
  a workspace might contain Go source, creative writing, image assets,
  configuration files, documentation, or dataset snapshots.
  `workspace/lib/schema/**`, `workspace/chapters/act-two/**`,
  `workspace/models/training-data/**`. Stewardship in workspace rooms
  governs review routing for changes to workspace content — the Bureau
  equivalent of CODEOWNERS, generalized beyond code.

- **`credential/<scope>/**`** -- credential provisioning.
  `credential/anthropic`, `credential/forgejo`. Stewardship in config
  or fleet rooms governs credential rotation approval.

- **`budget/<category>/**`** -- budget and quota.
  `budget/compute/gpu-hours`, `budget/api/anthropic-tokens`.
  Stewardship in project or fleet rooms governs spending authority.

- **`access/<action>/**`** -- access escalation requests.
  `access/observe/service/database`, `access/fleet/provision`.
  Stewardship in workspace or fleet rooms governs temporal grant
  approval chains.

### Resolution

When a ticket declares `affects: ["fleet/gpu/a100"]`, the ticket
service resolves matching stewardship policies:

- Scan `m.bureau.stewardship` state events in all rooms the ticket
  service is a member of.
- For each stewardship declaration, check whether any entry in
  `ResourcePatterns` matches any entry in the ticket's `affects`
  field.
- Collect all matching declarations. Multiple declarations can match
  -- a ticket affecting both `fleet/gpu/a100` and
  `service/api/ticket` gathers stewards from both the fleet room
  and the service room.

The resolution is room-scoped: only stewardship declarations visible
to the ticket service (via its room membership) participate. The
ticket service's scope determines the governance scope.

---

## Tiers and Escalation

A stewardship declaration's `Tiers` field is an ordered list of
reviewer groups. Each tier defines:

- **Principals** -- localpart patterns identifying the reviewers in
  this tier. Patterns match against principal localparts using the
  standard glob syntax. `bureau/dev/*/tpm` matches all TPMs across
  dev teams. `ben` matches the operator. The ticket service resolves
  patterns to concrete principal user IDs at gate configuration time
  by matching against known principals in shared rooms.

- **Threshold** -- the number of approvals required from this tier.
  An integer means "at least N of the listed principals must approve."
  The string `"all"` means every listed principal must approve.

- **Escalation** -- when this tier's reviewers are notified.
  `"immediate"` means the tier is notified as soon as the review gate
  is created. `"last_pending"` means the tier is notified only when
  it is the last unsatisfied tier -- all earlier tiers have met their
  thresholds.

### Evaluation

The review gate produced by stewardship has tier-aware evaluation:

- Each tier tracks its own approval count independently.
- A tier is *satisfied* when its threshold is met (enough principals
  in the tier have disposition `"approved"`).
- A tier is *exhausted* when all principals in the tier have set
  dispositions but the threshold is not met (some denied or the pool
  is too small after abstentions).
- The overall gate is *satisfied* when all tiers are satisfied.
- The overall gate is *blocked* when any tier is exhausted without
  meeting its threshold.

### Escalation flow

- **Tier 0** (`escalation: "immediate"`): reviewers are notified when
  the review gate is created. They see the ticket, review the
  request, set dispositions.
- **Tier 1** (`escalation: "last_pending"`): reviewers are notified
  only when Tier 0 has met its threshold and Tier 1 is the remaining
  unsatisfied tier. The notification includes the full decision
  record: who in Tier 0 approved, any comments or concerns, and the
  original request context.

This ordering means operators see only requests that agents couldn't
resolve autonomously. Each request arrives with accumulated context --
not as first-line triage, but as the final decision in a
well-documented chain.

### Why tiers matter

Without tiers, every stewardship-gated ticket notifies all reviewers
simultaneously. Operators are pinged on every resource request,
interleaved with agent reviews they could have handled. The signal
drowns in noise, and the operator either ignores notifications (making
the system advisory-only) or burns attention triaging requests that
agents should have resolved.

Tiers create a decision-making hierarchy per resource. The operator
defines which principals constitute each tier and what threshold each
tier requires. The system handles routing. Agent stewards get first
shot at every decision. Operators see only what escalates.

---

## Gating vs. Notification

The same stewardship declaration serves two modes, selected by the
ticket's type.

### Gating mode

When a ticket's type appears in the stewardship declaration's
`GateTypes`, the ticket service auto-configures a review gate on the
ticket:

- Reviewers are populated from the stewardship tiers (all principals
  across all tiers, with tier metadata preserved).
- The review gate's threshold and escalation semantics come from the
  stewardship declaration's tier configuration.
- The gate blocks the ticket: it is not ready until the review gate
  is satisfied.
- This is the mechanism for resource requests, reviews, deployments,
  credential rotations -- any ticket that *changes* a stewarded
  resource.

When a ticket matches stewardship declarations from multiple rooms
(because it affects resources governed by different stewardship
policies), the declarations must be merged into a single review gate.
How they merge depends on the relationship between the declaring rooms.

Each stewardship declaration carries an **`overlap_policy`** field
that controls how it composes with other declarations matching the
same ticket:

- **`"independent"`** (default) -- this declaration's review gate
  stands on its own. If two declarations both match with
  `overlap_policy: "independent"`, the ticket gets *separate* review
  gates, one per declaration, and both must be satisfied. This is the
  correct policy when declarations represent concurrent authority:
  both Team A and Team B steward a shared protocol, and either team
  can independently block changes. Neither team's approval satisfies
  the other's.

- **`"cooperative"`** -- this declaration's reviewers are pooled with
  other `"cooperative"` declarations into a single merged gate. The
  reviewer set is the union of all matched tiers from all cooperative
  declarations. Tier ordering is preserved: all Tier 0 reviewers
  across cooperative declarations form the first escalation tier, all
  Tier 1 reviewers the second, and so on. If cooperative declarations
  have conflicting thresholds within the same effective tier, the
  strictest threshold (highest count) applies. This is correct when
  declarations represent equivalent authority: any qualified reviewer
  from any of the declaring rooms can satisfy the review.

The distinction matters for organizational hierarchy. In a workspace
with nested subteam rooms, a parent room's stewardship over
`workspace/**` and a child room's stewardship over
`workspace/team_a/**` are not peer declarations — the parent gates
merges *into* the parent branch, the child gates merges *within* the
child's scope. These are independent: the child team cannot approve
their own merge into the parent. Conversely, two peer teams that both
declare cooperative stewardship over `workspace/shared/protocol/**`
produce a single pool of qualified reviewers — approval from either
team's steward suffices.

Independent declarations that don't overlap in resource patterns
produce separate gates trivially (each gate covers different
resources). The `overlap_policy` field only affects behavior when
multiple declarations match the same ticket.

### Notification mode

When a ticket's type appears in `NotifyTypes`, stewards are informed
but no gate is created:

- The ticket service posts a notification to the rooms where the
  matching stewardship declarations live, @-mentioning the steward
  principals.
- Stewards can participate via threads on the ticket -- reviewing,
  answering questions, flagging concerns -- but their involvement is
  not required for the ticket to proceed.

Notification is asynchronous and batched. Rather than waking stewards
immediately for each informational ticket, the ticket service posts
summary digests to workspace rooms at configurable intervals. A
steward whose workspace produces 15 bug reports touching their
resources gets one digest message listing all 15, not 15 separate
notifications. Stewards consume the digest on their own schedule.

The digest interval is configurable per room via a field on
`m.bureau.stewardship`:

- **`digest_interval`** -- duration between notification digests
  (e.g., `"1h"`, `"4h"`, `"24h"`). Zero or absent means immediate
  notification (no batching). The ticket service accumulates
  notification-mode ticket events and flushes them as a single
  summary message at the end of each interval.

### Request urgency and SLA

Gating-mode tickets can declare urgency that overrides the normal
escalation timing. A ticket's priority field already carries urgency
information (P0 through P4). The stewardship system uses priority to
compress escalation:

- **P0 (critical):** all tiers are notified immediately, regardless
  of escalation ordering. The `last_pending` delay is bypassed.
  Critical requests cannot afford to wait for agent-tier resolution.
- **P1 (high):** escalation timing is compressed. The `last_pending`
  tier is notified after a shorter delay (configurable per stewardship
  declaration) rather than waiting for full tier resolution.
- **P2-P4:** normal escalation ordering applies.

This ensures that stewardship governance doesn't become a bottleneck
for genuinely urgent requests while preserving the noise-reduction
benefits for routine work.

---

## Ticket Integration

### The `affects` field

Tickets gain an `affects` field on `TicketContent`: a list of resource
identifiers describing what the ticket impacts.

```json
{
  "type": "m.bureau.ticket",
  "state_key": "tkt-b7c3",
  "content": {
    "title": "Scale GPU fleet: add 2 A100 instances",
    "type": "resource_request",
    "affects": ["fleet/gpu/a100"],
    "status": "open",
    ...
  }
}
```

`affects` is set by the ticket creator (agent, operator, or system).
For review tickets, it can be auto-populated from the changed file
list -- the agent or review pipeline extracts changed paths within the
workspace, prefixes them with `workspace/`, and sets them as the
`affects` entries (see
[Workspace Path Stewardship](#workspace-path-stewardship)).

The field is a list of strings. Each entry is a resource identifier,
not a glob pattern -- tickets declare what they affect, not what they
might affect. The ticket service matches these concrete identifiers
against the glob patterns in stewardship declarations.

### Stewardship index

The ticket service maintains an in-memory stewardship index alongside
its ticket index. The stewardship index is built from
`m.bureau.stewardship` state events across all rooms the ticket
service is a member of, updated incrementally via the `/sync` loop.

The index maps resource patterns to stewardship policies. Lookup:
given a list of resource identifiers (from a ticket's `affects`
field), return all matching stewardship declarations with their tier
configurations.

The index is small: stewardship declarations are configuration, not
data. A deployment with 50 stewarded resources across 20 rooms
produces 50 index entries. Lookup is a linear scan with glob matching
-- sub-microsecond for any realistic deployment size.

### Auto-gate configuration

When the ticket service processes a new ticket (or an update to a
ticket's `affects` or `type` field), it resolves stewardship:

- Look up matching stewardship declarations from the index.
- If the ticket's type matches any declaration's `GateTypes`:
  populate a review gate from the merged stewardship tiers.
- If the ticket's type matches any declaration's `NotifyTypes`:
  queue a notification for the next digest flush (or send immediately
  if `digest_interval` is zero).
- If the ticket's type matches neither: no stewardship action.

The review gate is added to the ticket's gates list as a gate with
type `"review"` and an ID derived from the stewardship source (e.g.,
`"stewardship:fleet/gpu"`). The ticket's `Review` field is populated
with the reviewer entries from the matched tiers. The gate and
reviewers are written atomically as part of the ticket state event.

If the ticket already has a manually-configured review gate (set by
the creator), the stewardship-derived reviewers are *merged* into
the existing reviewer list. Stewardship adds to explicit
configuration; it does not replace it. An agent that manually adds a
specific reviewer gets both that reviewer and any stewardship-derived
reviewers.

### Review gate extensions

The existing review gate type is extended with stewardship-aware
fields on `TicketReview`:

- **`threshold`** -- approval count required. An integer means "at
  least N reviewers must approve." The string `"all"` means every
  reviewer must approve. Absent or zero means `"all"` (backward
  compatible with the current behavior where every reviewer must
  approve).

- **`tiers`** on each `ReviewerEntry` -- the tier index this reviewer
  belongs to. Used for escalation ordering. Absent means Tier 0
  (immediate notification). The ticket service uses this to determine
  notification timing: Tier 0 reviewers are notified immediately,
  higher tiers are notified according to their escalation policy.

The review gate's satisfaction condition changes from "every reviewer
approved" to "the threshold is met across all tiers." Specifically:
each tier independently checks its own threshold against its own
reviewers' dispositions. The overall gate is satisfied when all tiers
are satisfied. A tier with `threshold: 2` and three reviewers, where
two have approved and one has not yet responded, is satisfied.

This is backward compatible: a review gate with no `threshold` field
and no tier annotations behaves exactly as today -- all reviewers must
approve.

### Disposition authenticity

Review dispositions are currently state events written by principals
through the ticket service. The ticket service verifies that the
principal setting a disposition is listed as a reviewer, but the
disposition itself is not cryptographically bound to the principal —
if the daemon or ticket service is compromised, dispositions could be
forged.

A stronger model would have each proxy hold a per-principal signing
keypair. Review dispositions would carry a signature that the ticket
service (and any auditor) can verify against the principal's public
key. This makes disposition forgery require compromising the specific
principal's proxy, not just the ticket service. This hardening is a
natural extension of the proxy credential architecture described in
[credentials.md](credentials.md) and applies to the review system
broadly (see [tickets.md](tickets.md)), not only to
stewardship-derived reviews.

---

## Workspace Path Stewardship

Workspace path stewardship is Bureau's equivalent of CODEOWNERS,
generalized beyond source code: a declarative mapping from
workspace-relative paths to responsible principals, enforced through
the ticket review system.

Bureau workspaces are git worktrees, but the content is unconstrained.
A workspace might contain Go source for a compiler project, chapters
and outlines for a creative writing project, image assets for a design
project, training data for an ML pipeline, or configuration files for
infrastructure. Workspace path stewardship applies to all of these:
the mapping is from paths to principals, regardless of what the paths
contain.

### Declaration

A workspace room declares path stewardship:

```json
{
  "type": "m.bureau.stewardship",
  "state_key": "workspace/lib/schema",
  "content": {
    "resource_patterns": ["workspace/lib/schema/**", "workspace/lib/ticketindex/**"],
    "description": "Schema types and ticket index data structures",
    "gate_types": ["review"],
    "notify_types": ["bug", "question"],
    "tiers": [
      {
        "principals": ["bureau/dev/senior-dev", "bureau/dev/tpm"],
        "threshold": 1,
        "escalation": "immediate"
      }
    ],
    "version": 1
  }
}
```

The `gate_types` entry is `"review"` rather than `"code_review"` --
a review ticket for edited chapters, modified images, or updated
training data triggers the same stewardship flow as a code review.
The ticket type describes the workflow (review), not the content type.

### Changed-path extraction

When a review ticket is created, the `affects` field must be populated
with the workspace paths that changed. The mechanism depends on the
workspace content:

- **Git-tracked content** (the common case): run
  `git diff --name-only <base>..<head>` to get the list of changed
  files. Strip the worktree prefix to get repo-relative paths. Prefix
  each path with `workspace/` to place it in the resource namespace
  (e.g., `lib/schema/ticket.go` becomes
  `workspace/lib/schema/ticket.go`).
- **Non-diff-based changes**: an agent or pipeline that modifies
  workspace content directly (generating images, writing dataset
  files) populates `affects` with the paths it wrote to, prefixed
  with `workspace/`.

The ticket service matches each affected path against stewardship
resource patterns. A change to `workspace/lib/schema/ticket.go`
matches `workspace/lib/schema/**` and adds the schema stewards to
the review gate. A change to `workspace/chapters/act-two/scene-3.md`
matches `workspace/chapters/act-two/**` and adds the narrative
stewards.

The `workspace/` prefix is a namespace convention that separates
workspace-relative paths from fleet resources, service resources, and
other resource categories. It allows a stewardship declaration for
`workspace/lib/schema/**` to coexist in the same room as a
declaration for `service/api/ticket/**` without ambiguity.

### Relationship to the review pipeline

Reviews in Bureau flow through a review pipeline (described in
[dev-team.md](dev-team.md)). The review pipeline's first step can
extract changed paths and populate the ticket's `affects` field
before the review gate is evaluated. This means stewardship-derived
reviewers appear on the ticket as part of the standard review setup,
not as a separate step.

Stewardship does not replace the explicit reviewer selection in the
review pipeline. A ticket may already list specific reviewers
(assigned by a lead, selected by the PM, or configured in the review
pipeline). Stewardship adds resource-specific reviewers on top of
explicit ones. The merged reviewer list ensures both organizational
review (the lead reviews all work in the workstream) and resource
review (the schema steward reviews schema changes regardless of
workstream).

---

## Access Escalation

Stewardship enables autonomous access escalation -- principals
requesting and approving temporary elevated access without requiring
an operator in the loop for every request.

### The pattern

An agent needs access beyond its static grants (temporary observation
access to a database service, cross-project service invocation, fleet
management permissions for an incident). The flow:

- The agent creates a ticket with `type: "access_request"` and
  `affects: ["access/observe/service/database"]` describing the
  requested access, the justification, and the requested duration.
- The ticket service resolves stewardship for `access/observe/**`
  and finds a policy with agent stewards in Tier 0 and an operator in
  Tier 1 (`escalation: "last_pending"`).
- Tier 0 stewards are notified. They review the request: is the
  justification sound? Is the access scope appropriate? Is the
  duration reasonable?
- If the agent stewards approve (meeting the tier's threshold), the
  operator is never notified. The ticket proceeds: a temporal grant
  is created (per [authorization.md](authorization.md)) with the
  requested access, scoped to the requested duration.
- If the agent stewards can't reach threshold -- one approves, one
  denies with a concern, one hasn't responded -- the operator tier
  activates. The operator sees the full context: the original request,
  who approved, who denied and why. The operator makes the final call
  with accumulated context.

### What the operator defines

The operator doesn't pre-approve individual access requests. The
operator pre-approves the *governance mechanism*:

- Which resources have stewardship declarations with access escalation
  support.
- Which agent principals are in each escalation tier.
- What threshold each tier requires.
- Which actions can be escalated at all (stewardship for
  `access/fleet/**` might have only the operator in Tier 0 with no
  agent tier, meaning fleet access always requires the operator).

The governance structure is configuration. Individual decisions flow
through the structure autonomously. The operator adjusts the structure
based on observed outcomes: if a particular agent tier consistently
makes good access decisions, the operator can expand its scope. If
bad decisions emerge, the operator tightens thresholds or removes
agent tiers.

### Temporal grant creation

When a stewardship-gated access request ticket clears its review gate,
the approval must be translated into a temporal grant. The ticket
service does not mint grants itself — it is a workflow engine, not an
authorization service. Centralizing grant authority in the most
heavily-trafficked service in the system would create a privilege
accumulation risk and a single point of compromise.

Instead, the flow separates concerns across services:

- The **ticket service** detects that the access request ticket's
  review gate is satisfied. It marks the ticket as approved and emits
  a state event recording the approval: who reviewed, their
  dispositions, and the timestamp of gate clearance.
- A separate **authorization service** (described in
  [authorization.md](authorization.md)) observes the approval event.
  Before minting any grant, it validates:
  - The ticket was properly approved per its stewardship requirements
    (correct reviewers, threshold met, tiers satisfied).
  - The requested grant is within policy bounds (permitted action
    scopes, maximum duration, target restrictions).
  - The approving principals had the authority to approve this class
    of grant.
- On successful validation, the authorization service mints a temporal
  grant state event with the requested access, scoped to the requested
  duration. The grant references the ticket ID for audit.

This means a stewardship approval is necessary but not sufficient for
a temporal grant. The authorization service can reject properly-approved
requests that violate policy bounds — a stewardship gate that clears
for "grant fleet admin for 30 days" can be rejected by policy that
caps temporal fleet grants at 8 hours. The ticket service provides the
*evidence* of approval; the authorization service makes the *decision*
to grant.

This connects the stewardship system to the temporal grant system
described in [authorization.md](authorization.md): stewardship governs
*who must approve*, the review gate enforces *the approval process*,
the authorization service validates *policy compliance*, and temporal
grants implement *the access change*.

---

## Notification Batching

For notification-mode stewardship (informational tickets that don't
gate), immediate notification per ticket produces noise that
undermines the system's value. A workspace generating 20 bug reports
in an hour should not wake stewards 20 times.

### Digest messages

The ticket service accumulates notification-mode events per steward
and flushes them as digest messages at the configured interval.

A digest message is a Matrix timeline event posted to the room where
the stewardship declaration lives, @-mentioning the steward
principals. The message lists the accumulated tickets: title, type,
priority, and a reference to each ticket for drill-down.

```
@bureau/dev/senior-dev @bureau/dev/tpm — 4 new tickets
affecting stewarded resources since last digest:

- bug tkt-a3f9: "Schema validation panic on empty labels" (P2)
- question tkt-b7c3: "Should TicketContent.Version be uint32?" (P3)
- bug tkt-c1d4: "Ticket index race on concurrent gate updates" (P1)
- investigation tkt-d2e5: "Ticket service memory growth under load" (P2)
```

Stewards receive the digest on their own schedule. A steward with a
StartCondition matching @-mentions in the workspace room will wake on
the next digest. A steward on a daily schedule processes digests in
batch. The digest interval controls the granularity.

### Urgency override

High-priority notification-mode tickets (P0 or P1) bypass digest
batching and notify stewards immediately. A critical bug affecting a
stewarded resource should not wait for the next hourly digest. The
immediate notification is a direct @-mention in the room, not a
digest entry.

### Digest interval configuration

The `digest_interval` field on the stewardship declaration controls
batching for that specific resource. Different resources can have
different intervals:

- `"1h"` for actively developed workspace paths (stewards should see
  issues promptly).
- `"24h"` for stable infrastructure (daily digest is sufficient).
- `"0"` or absent for immediate notification (no batching).

The ticket service maintains a per-stewardship-declaration
accumulator that flushes at the configured interval.

---

## Relationship to Authorization

Stewardship and authorization are orthogonal systems that compose
without interference.

### Authorization checks happen at request time

When a principal invokes a service action, the service verifies the
principal's grants from its service identity token. This is immediate:
the request is permitted or denied on the spot. Stewardship is not
consulted. A principal with `fleet/provision` grants can call the
fleet controller's provisioning API directly.

### Stewardship checks happen at coordination time

When a principal creates a ticket to request fleet provisioning, the
ticket service resolves stewardship and configures review gates. The
ticket doesn't proceed until the gates clear. The actual provisioning
happens after the ticket is approved -- either the sysadmin agent
picks up the approved ticket and executes it, or a pipeline fires
on the ticket's state change.

### They compose naturally

A sysadmin agent has `fleet/**` grants (it is *permitted* to manage
the fleet). Its stewardship-gated ticket has a review gate requiring
project lead approval (governance says fleet changes need consensus).
The sysadmin doesn't use its grants until the ticket clears. The
grants ensure the sysadmin *can* act. The stewardship ensures it
*should* act. Both are required for the operation to proceed.

A principal without `fleet/**` grants cannot act regardless of
stewardship -- even if a ticket is approved, the principal's service
requests will be denied at the authorization layer. Stewardship does
not grant permissions. It governs when permissions are exercised.

### MCP tool filtering remains grant-based

The MCP server filters available tools based on the principal's
resolved grants, not stewardship. A coder sees ticket and artifact
tools because it has the grants. Stewardship doesn't hide tools -- it
governs the approval process when those tools are used for
resource-changing actions.

---

## Scaling

### Small deployments

A single-machine deployment with one operator and a handful of agents
may have few stewardship declarations -- perhaps workspace path
stewardship in one workspace room and fleet stewardship in the fleet
room. The stewardship index is trivially small. Notification batching is
unnecessary. The system works but provides marginal value over direct
operator oversight.

### Medium deployments

A small fleet with multiple projects, each with their own workspace
rooms and agents. Stewardship declarations in each workspace room
govern review routing for workspace changes. Fleet stewardship in a shared room governs resource allocation. Agent tiers handle routine approvals;
the operator handles escalations. The digest mechanism keeps
notification volume manageable.

### Large deployments

Multiple fleets, many projects, dozens of agents. Stewardship
declarations are distributed across rooms -- each room governs its own
resources. The ticket service's stewardship index scales with the
number of stewardship declarations (configuration), not the number of
tickets (data). Cross-project resource stewardship lives in shared
rooms with membership limited to the relevant project leads. Room
membership provides natural partitioning: adding a new project means
creating a new room with its own stewardship declarations, not
modifying a global configuration.

---

## Relationship to Other Design Documents

- **[authorization.md](authorization.md)** -- stewardship complements
  grants. Grants authorize actions; stewardship governs when actions
  proceed. Both must be satisfied. Temporal grants (ticket-backed
  access requests) connect to stewardship through the access
  escalation pattern: stewardship governs who approves, the ticket
  service provides evidence of approval, and a separate authorization
  service validates policy bounds before minting the temporal grant.

- **[tickets.md](tickets.md)** -- stewardship extends the review gate
  with threshold semantics and tiered escalation. The `affects` field
  on tickets connects work items to stewardship declarations. The
  ticket service maintains the stewardship index and auto-configures
  review gates. Digests are timeline events in rooms.

- **[information-architecture.md](information-architecture.md)** --
  `m.bureau.stewardship` is a new state event type in the catalog.
  It lives in any room with ticket management enabled (workspace rooms,
  fleet rooms, service rooms). Room membership provides visibility
  partitioning. The state event follows standard Bureau patterns:
  versioned content, room-scoped, daemon-independent (the ticket
  service owns evaluation).

- **[fleet.md](fleet.md)** -- fleet resource stewardship governs
  scaling, rebalancing, and machine provisioning decisions. The fleet
  controller creates tickets for capacity changes; stewardship
  policies on those tickets ensure project leads approve resource
  reallocation before the sysadmin executes.

- **[dev-team.md](dev-team.md)** -- workspace path stewardship is the
  CODEOWNERS equivalent for the review pipeline. Stewardship-derived
  reviewers are merged with explicit reviewers from the review
  pipeline configuration.

- **[credentials.md](credentials.md)** -- credential rotation
  stewardship ensures affected project leads approve before shared
  credentials are rotated. The stewardship review gate clears before
  the sysadmin or connector executes the rotation.

- **[agent-layering.md](agent-layering.md)** -- MCP tool filtering
  remains grant-based, not stewardship-based. Agents see tools based
  on what they are permitted to do. Stewardship governs the approval
  process when those tools affect stewarded resources.

- **[fundamentals.md](fundamentals.md)** -- stewardship is a
  governance layer built from existing primitives: Matrix state events
  for declarations, rooms for scoping, the ticket service for
  evaluation, review gates for enforcement. No new infrastructure
  components are required.
