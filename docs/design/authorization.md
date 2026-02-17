# Authorization

[fundamentals.md](fundamentals.md) establishes that Bureau runs
untrusted code; [architecture.md](architecture.md) describes the
runtime topology of daemon, launcher, and proxy. This document defines
Bureau's authorization framework: how principals are granted
capabilities, how those capabilities are checked, how policy is
distributed, and how dynamic access requests flow through tickets.

---

## Why a Unified Framework

Authorization decisions occur throughout Bureau — observation, Matrix
operations, service discovery, credential provisioning, ticket
mutations, fleet management, artifact operations, interrupt signals.
Without a shared model, each capability develops its own policy format,
its own enforcement code, and its own configuration path. The framework
described here provides a single model that all capabilities build on:
hierarchical actions matched by glob patterns, two-sided policy
(grants and allowances), and layered resolution from machine defaults
through templates and rooms to per-principal overrides.

---

## Design Principles

**Default-deny everywhere.** A principal with no grants can do nothing
beyond what its sandbox provides (read its own filesystem, talk to its
own proxy for explicitly configured services). Every action requires an
explicit grant. This is not a convention — it is the evaluation rule:
the framework returns deny unless a matching, non-expired, non-denied
grant is found.

**Two-sided policy.** Authorization decisions have two perspectives:

- **Grants** (subject-side): what is principal A allowed to do?
  Configured on the acting principal. A grant for `matrix/join` lets
  a principal join rooms. A grant for `service/discover` lets a
  principal see services.

- **Allowances** (target-side): who is allowed to act on principal B?
  Configured on the target principal or resource. An allowance for
  `observe` from `iree/**` lets principals in the `iree` namespace
  observe this principal's terminal.

The effective permission is the intersection: A can do X to B only if
A has a grant for X *and* B has an allowance for A to do X. Either
side can deny.

Some actions are one-sided. Self-service actions (joining rooms,
discovering services) only check subject-side grants — the "target"
is infrastructure, not another principal. Cross-principal actions
(observation, interrupts) check both sides.

**Glob patterns as the universal matching language.** Bureau uses
hierarchical `/`-separated patterns with `*` (one segment), `**`
(recursive), and `?` (single character) for both localpart matching
and action matching. No regex, no separate ACL grammar. The same
pattern language works everywhere.

**Room membership as implicit grouping.** Matrix rooms already group
principals. Rather than inventing a separate role/group concept, the
framework leverages room membership: grants attached to a room apply
to all members. Combined with power levels within rooms, this gives
hierarchical authority without a separate RBAC system.

**Policy lives in Matrix.** Authorization policy is configuration.
Configuration lives in Matrix state events. The framework adds state
event types for authorization policy, which flow through the same
/sync path as everything else.

---

## The Model

### Actions

An action is something a principal can do. Actions are hierarchical
strings using `/` as separator:

```
observe                  — observe a principal's terminal (read-only)
observe/read-write       — observe with interactive access
interrupt                — send interrupt signal
interrupt/terminate      — send termination signal
credential/provision     — provision credentials
matrix/join              — join Matrix rooms
matrix/invite            — invite others to rooms
matrix/create-room       — create rooms
service/discover         — discover services in the directory
service/invoke           — invoke a service
ticket/create            — create tickets
ticket/assign            — assign tickets
ticket/close             — close tickets
artifact/store           — store artifacts
artifact/fetch           — fetch artifacts
artifact/tag             — create or update tags
artifact/pin             — pin artifacts (prevent GC)
fleet/assign             — assign principals to machines
fleet/provision          — provision/deprovision machines
```

Actions form a tree. A grant for `ticket/*` implies `ticket/create`,
`ticket/assign`, and `ticket/close`. A grant for `artifact/**` implies
all artifact operations including nested ones. A grant for `observe`
implies only read-only observation (not `observe/read-write`). The
`*` wildcard matches one level; `**` matches all descendants.

#### Service action namespaces

Every Bureau service defines its action namespace under a prefix
matching its service role — the string used for token audience, socket
path (`/run/bureau/service/<role>.sock`), and token file path
(`/run/bureau/service/token/<role>.token`). The ticket service owns `ticket/*`,
the artifact service owns `artifact/*`, the fleet controller owns
`fleet/*`.

External service connectors follow the same convention. A Forgejo
connector with role `forgejo` defines actions under `forgejo/*`. A
GitHub connector with role `github` defines actions under `github/*`.
Connectors for the same category of service (git forges) use shared
operation names so that cross-forge grants work:

```
<role>/list-repos        — list repos the caller can access
<role>/repo-info         — metadata for a specific repo
<role>/create-repo       — create a repo
<role>/delete-repo       — delete a repo (destructive)
<role>/register-webhook  — configure webhook delivery
<role>/report-status     — push commit status
<role>/create-user       — create a user (admin only)
<role>/delete-user       — remove a user (admin only)
<role>/sync-permissions  — force permission resync (admin only)
```

Because actions use `/` hierarchy and glob matching, grants can span
services naturally:

- `{"actions": ["*/report-status"]}` — report commit status on any
  forge
- `{"actions": ["forgejo/**"]}` — all Forgejo operations
- `{"actions": ["**"]}` — all actions on all services (operator-level)

### Grants

A grant gives a principal permission to perform actions, optionally
scoped to specific targets:

- **Actions** — a list of action patterns (glob syntax). The principal
  can perform any action matching any pattern.
- **Targets** — a list of localpart patterns identifying which
  principals or resources this grant applies to. Empty means the grant
  applies to non-targeted actions only (self-service operations like
  `matrix/join`, `service/discover`, `artifact/store`). For
  self-service actions, targets are ignored.
- **ExpiresAt** — optional timestamp. After this time, the grant is
  ignored during evaluation. The daemon garbage-collects expired
  grants. Omit for permanent grants.
- **Ticket** — optional ticket reference linking the grant to its
  approval. Provides audit trail for temporary and escalated access.
- **GrantedBy** and **GrantedAt** — provenance, set automatically by
  the daemon when processing grant requests.
- **Source** — identifies where the grant came from in the policy merge
  hierarchy. Set automatically by the daemon during authorization index
  rebuild: `machine-default` (from DefaultPolicy), `principal` (from
  PrincipalAssignment.Authorization), `temporal` (from a temporal grant
  state event), or `room:<room_id>` (from a room-level authorization
  policy). Denials, allowances, and allowance denials also carry Source.

Examples:

```json
// PM can interrupt and observe any agent in its project
{"actions": ["interrupt", "observe/**"], "targets": ["bureau/dev/**"]}

// Coder can create and assign tickets in its workstream
{"actions": ["ticket/create", "ticket/assign"], "targets": ["bureau/dev/workspace/**"]}

// Operator can observe and interrupt anything
{"actions": ["observe/**", "interrupt/**"], "targets": ["**"]}

// Sysadmin agent can use all service actions
{"actions": ["ticket/**", "artifact/**", "workspace/**", "fleet/**"]}

// Connector can provision specific credential keys for specific principals
{"actions": ["credential/provision/key/FORGEJO_TOKEN"], "targets": ["iree/**"]}
```

### Denials

A denial explicitly prohibits an action, overriding any matching
grants. Denials are evaluated after grants: if a denial matches, the
action is blocked regardless of how many grants allow it.

Denials exist on both sides:

- **Subject-side denials**: "this principal cannot do X, even if a
  room-level grant says it can."
- **Target-side denials**: "principal B cannot be acted on by actors
  matching pattern P, even if a room-level allowance says they can."

The key property of denials: they are inherited from parent templates
and cannot be removed by child templates. A base template can deny
`fleet/**` to establish a security boundary that no descendant template
can circumvent. This rigidity is intentional — platform administrators
define hard limits in base templates, and project-level templates
compose within those limits. A specialized template that needs fleet
access must not inherit from a base that denies it, forcing template
designers to think carefully about where denials live in the hierarchy.

Example: a workspace room grants `ticket/**` to all members, but
coders should not close tickets — only the TPM should:

```json
// Room-level grant (all members):
{"actions": ["ticket/**"]}

// Per-principal denial on coder templates:
{"actions": ["ticket/close", "ticket/reopen"]}
```

The coder inherits `ticket/**` from the room but the template-level
denial blocks `ticket/close` and `ticket/reopen`.

### Allowances

An allowance lets a principal be acted upon by specific actors for
specific actions — the target-side counterpart to grants:

- **Actions** — action patterns the actors are allowed to perform on
  this principal.
- **Actors** — localpart patterns identifying who can perform the
  allowed actions.

Allowance denials work symmetrically: they override matching allowances,
and they are also inherited and non-removable through template
composition.

Examples:

```json
// Allow the PM and any TPM to observe and interrupt this agent
{"actions": ["observe/**", "interrupt"], "actors": ["bureau/dev/pm", "bureau/dev/*/tpm"]}

// Allow the operator to do anything
{"actions": ["**"], "actors": ["bureau-admin"]}

// Allow any reviewer to observe this agent (read-only)
{"actions": ["observe"], "actors": ["bureau/dev/reviewer/**"]}
```

### Per-Principal Policy

Each principal's authorization is configured through an authorization
policy on its PrincipalAssignment, carrying four lists: grants,
denials, allowances, and allowance denials. During template
inheritance, all four lists are appended — child templates add to
parent policy. Per-principal overrides in the PrincipalAssignment are
merged on top of the resolved template policy. This follows the same
layering as other Bureau configuration: machine defaults, then
template, then instance.

### Room-Level Policy

Some authorization applies to all members of a room. A room-level
authorization policy avoids duplicating the same grants across every
principal:

- **Event type:** `m.bureau.authorization`
- **State key:** `""` (room-level singleton)

A room authorization policy carries:

- **Member grants** — grants given to every member of the room.
- **Power level grants** — additional grants keyed by Matrix power
  level. A principal with power level >= the key gets the associated
  grants on top of member grants.

Example for a workstream room:

```json
{
  "member_grants": [
    {"actions": ["ticket/create", "ticket/assign"], "targets": ["bureau/dev/workspace/**"]},
    {"actions": ["observe"], "targets": ["bureau/dev/workspace/**"]}
  ],
  "power_level_grants": {
    "50": [
      {"actions": ["interrupt", "observe/read-write"], "targets": ["bureau/dev/workspace/**"]}
    ],
    "100": [
      {"actions": ["**"], "targets": ["bureau/dev/workspace/**"]}
    ]
  }
}
```

Members can create tickets and observe workspace agents. Power level
50+ (TPM, daemon) can also interrupt and interactively observe. Power
level 100 (admin, PM) can do anything to workspace agents.

### Machine-Level Defaults

Machine-level default policy applies to all principals on the machine
unless overridden. This sets a baseline: "every principal on this
machine can discover services matching `service/**`" or "every
principal allows observation by `bureau-admin`."

Per-principal policy is additive on top of machine defaults — it adds
grants and allowances, it does not remove them. A principal cannot
have fewer permissions than the machine default. Machine defaults set
the floor (common infrastructure access); per-principal policy adds
role-specific capabilities.

---

## Authorization Evaluation

### The check

When the system checks whether an actor can perform an action on a
target:

1. **Collect the actor's grants** from all sources: machine defaults,
   room-level grants from all rooms the actor belongs to (member grants
   plus applicable power level grants), per-principal grants from the
   PrincipalAssignment, template-inherited grants, and temporal grants.
2. **Find a matching grant**: does any non-expired grant's action
   patterns match the requested action, and do its target patterns
   match the target (or is the target empty for self-service actions)?
   No match means deny (default-deny).
3. **Check the actor's denials** from all sources: if any denial
   matches (action, target), the result is deny. Denials override
   grants.
4. **For cross-principal actions, collect the target's allowances**
   from all sources: machine defaults, room-level, per-principal,
   template-inherited.
5. **Find a matching allowance**: does any allowance's action patterns
   match the action, and do its actor patterns match the actor? No
   match means deny.
6. **Check allowance denials**: if any allowance denial matches
   (action, actor), the result is deny.
7. **Authorized**: the request passed all checks.

For self-service actions (empty target), steps 4-6 are skipped.

### Policy resolution

The daemon maintains an in-memory authorization index — the resolved
policy for all principals, pre-merged from all sources. The index is
updated when machine config changes, room authorization policy changes,
principal assignments change, temporal grants are added or revoked, or
expired temporal grants are swept.

Authorization checks are constant-time lookups against this
pre-resolved index, not re-resolution on every check. This matters
because checks happen on every observation request, service discovery
query, and interrupt signal.

---

## Enforcement Points

Authorization checks happen at different layers depending on the
action:

| Action | Enforcement point |
|--------|-------------------|
| `observe`, `observe/read-write` | Daemon, before forking relay |
| `interrupt`, `interrupt/terminate` | Daemon, before forwarding signal to launcher |
| `matrix/join`, `matrix/invite`, `matrix/create-room` | Proxy, before forwarding to homeserver |
| `service/discover` | Proxy, filtering service directory response |
| `credential/provision` | Daemon, before credential bundle merge |
| `ticket/*` | Ticket service, on token grants |
| `artifact/*` | Artifact service, on token grants |
| `forgejo/*`, `github/*`, etc. | Connector service, on token grants |
| `fleet/*` | Fleet controller, on token grants |

The daemon is the primary authorization evaluator. For actions enforced
at the proxy, the daemon pushes pre-computed allowed actions to the
proxy via the admin socket. The proxy does not evaluate grants itself —
it receives resolved policy from the daemon.

For service-side enforcement, callers authenticate with service
identity tokens (see below). Services verify the token
cryptographically and check the embedded grants without calling back
to the daemon.

---

## Service Identity Tokens

Service sockets are shared — the daemon bind-mounts the same host-side
socket into every sandbox whose template declares the service as a
dependency. From the service's perspective, connections arrive from
multiple principals on the same listener with no inherent way to
distinguish callers. Unix `SO_PEERCRED` provides PID/UID, but PID
namespaces and UID mapping inside bwrap make these unreliable across
the namespace boundary.

Self-identification by the caller (putting `"principal": "..."` in the
request body) is unacceptable — a compromised sandbox could claim to be
any principal. Proxying service traffic (daemon-in-the-middle injecting
identity headers) is also wrong — services like the artifact service
move large binary payloads and need direct transport.

The answer is bearer tokens: the daemon mints a cryptographically
signed token per (principal, service) pair. The token proves identity
and carries authorization claims. Services verify tokens without a
daemon round-trip.

### Token contents

A service identity token carries:

- **Subject** — the principal's localpart (identity).
- **Machine** — the machine where the principal is running.
- **Audience** — the service role this token is scoped to. A token for
  the ticket service cannot be used against the artifact service.
- **Grants** — pre-resolved grants relevant to this service. The
  daemon filters the principal's full grant set to include only actions
  matching the service's namespace (`ticket/*` for ticket service
  tokens, `artifact/*` for artifact service tokens). The service
  checks these grants directly without a full authorization index.
- **ID** — unique identifier for revocation.
- **IssuedAt** and **ExpiresAt** — timestamps bounding validity.
  Short TTLs (default: 5 minutes) limit the revocation window.

### Wire format

The token is raw bytes: CBOR-encoded payload followed by a 64-byte
Ed25519 signature over that payload. Ed25519 signatures are always
exactly 64 bytes, so the split point is deterministic. No header, no
base64 — the algorithm is fixed and the signature size is constant.

On disk, tokens live at `/run/bureau/service/token/<service-role>.token`
inside the sandbox, in the token subdirectory under the service directory.
The agent reads the file as opaque bytes and includes them in service
requests. The agent never parses the token.

### Signing key

The daemon generates an Ed25519 keypair on first boot and persists it
in its state directory. The key is stable across restarts so in-flight
tokens remain valid. The daemon publishes the public key as a state
event in `#bureau/system` so services can discover it. Services also
receive the public key at sandbox creation time for offline
verification.

The signing keypair is separate from the age keypair held by the
launcher — the age keypair is for encryption (credential bundles), the
signing keypair is for authentication (service tokens). They serve
different purposes and live in different processes.

### Token lifecycle

**Minting**: when the daemon creates a sandbox, it mints one token per
required service, audience-scoped so a token stolen from one service
socket cannot be replayed against another.

**Delivery**: the daemon writes token files to a `token/` subdirectory
within the per-sandbox service directory on the host filesystem. The
launcher bind-mounts the service directory at `/run/bureau/service/`
inside the sandbox with sockets at the top level and the `token/`
subdirectory read-only. Tokens live in a separate subdirectory because
bwrap cannot create socket mount points inside a read-only directory —
the token directory is mounted read-only while the parent directory
needs to accommodate socket bind mounts. The directory mount means
the daemon can write new token files to the host-side `token/`
directory after sandbox creation and they appear inside the sandbox
immediately.

**Refresh**: the daemon writes a new token file before the current one
expires (at ~80% of the TTL). The write is atomic (write to temp file,
rename). The agent re-reads the token file on each service request. No
daemon-to-sandbox signaling needed.

**Temporal grant integration**: when a temporal grant arrives via
/sync, the daemon mints a new token for the affected principal with
updated grants and writes it to the token directory. The next service
request from the agent carries the expanded grants. Temporal grant
revocation triggers the same refresh — a new token without the revoked
grants.

### Revocation

Three mechanisms, in order of speed:

- **TTL expiry** (default): tokens expire after 5 minutes. A revoked
  principal's tokens become invalid within the TTL window. Sufficient
  for non-emergency revocation (permission changes, temporal grant
  expiry).
- **Token refresh** (fast): the daemon writes a new token without the
  revoked grants. Effective within seconds.
- **Emergency blacklist** (immediate): the daemon sends a revocation
  notice to the affected service via its admin channel. The service
  adds the token ID to a small in-memory blacklist. The blacklist
  auto-cleans as tokens expire. Used for sandbox destruction, policy
  changes requiring immediate effect, or machine-level emergency
  revocation (`bureau machine revoke`). During emergency shutdown the
  daemon pushes revocations for all principals to every reachable
  service before exiting.

### Service-side verification

When a service receives a request with a token:

1. Split the token bytes at `length - 64` (payload and signature).
2. Verify the Ed25519 signature against the daemon's public key.
3. Decode the CBOR payload.
4. Reject if expired.
5. Reject if the audience does not match this service.
6. Reject if the token ID is on the blacklist.
7. Extract the principal identity and grants.
8. Check the embedded grants for the requested operation.

Steps 1-6 are constant-time. Step 7-8 is a linear scan of the grants
list (small — a principal typically has fewer than 10 grants for any
given service).

### What tokens do not carry

Tokens carry grants (subject-side), not allowances (target-side).
Services where the target is the service itself (ticket CRUD, artifact
store/fetch) need only one-sided grant checks from the token.
Two-sided checks are primarily relevant for the daemon's own
enforcement (observation, interrupts) where it maintains the full
authorization index.

### Relationship to the proxy

The proxy and service tokens solve the same problem (caller
authentication) at different layers. The proxy authenticates requests
to Matrix and external APIs, holds the Matrix access token, and
injects credentials. Service tokens authenticate requests to
Bureau-internal services, are held by the agent inside the sandbox, and
are verified cryptographically by the service. The proxy never sees
service tokens (service sockets bypass the proxy). Service tokens never
leave the Bureau network. The two authentication layers are
independent.

---

## Credential Integration

### Provisioning scope as grants

Credential provisioning scope maps directly to the grant model. A
Forgejo connector that can provision `FORGEJO_TOKEN` but not
`OPENAI_API_KEY` has:

```json
{"actions": ["credential/provision/key/FORGEJO_TOKEN"], "targets": ["iree/**", "bureau/dev/**"]}
```

The connector can provision `FORGEJO_TOKEN` for principals under
`iree/` or `bureau/dev/`. It cannot provision `OPENAI_API_KEY` (no
matching action grant) and cannot provision for principals outside
those namespaces (no matching target pattern).

### Enforcement path

Credential provisioning is daemon-enforced, not service-token-enforced.
The `credential/provision` action namespace is a cross-cutting
privilege that modifies Matrix state events in config rooms. The daemon
checks `credential/provision/key/<KEY>` against the full authorization
index when a provisioning request arrives. If authorized, the daemon
handles the credential merge (decrypt via launcher IPC, insert new key,
re-encrypt, publish updated state event). If denied, the request is
rejected.

The connector does not need credential provisioning grants in any
service identity token — the daemon evaluates this on the full index.
Credential provisioning is the connector acting on its own behalf,
checked by the daemon. Socket operations are agents acting through the
connector, checked by the connector via token grants.

### RequiredCredentials

Templates declare required credential keys that must be present before
sandbox start. This is a readiness check, not an authorization check,
but it composes with the framework: whoever provisions the credentials
must have the appropriate `credential/provision/key/*` grant targeting
the principal.

Publishing credential state events requires both Matrix power level
(homeserver enforces) and a `credential/provision` grant (daemon
validates). This is the two-sided model in action: the room power level
is the allowance side, and the provisioning grant is the subject side.

---

## Connector and External Service Authorization

Bureau integrates with external systems (Forgejo, GitHub, S3, Slack)
through connector services. Each connector is a Bureau service
principal with its own unix socket, Matrix account, and credential
bundle. Every connector has two authorization surfaces.

### Surface A: agents calling the connector

Agents interact with a connector through its unix socket. The agent
authenticates with a service identity token (audience = connector's
service role). The connector checks the token's embedded grants.

The grants for connector socket operations come from the agent's
resolved policy (template grants, room-level grants, machine
defaults). A typical agent template includes:

```json
{"actions": ["forgejo/list-repos", "forgejo/repo-info", "forgejo/report-status"]}
```

Room-level grants in rooms with repository bindings can add per-room
connector access. Admin-only operations (`create-user`, `delete-user`,
`sync-permissions`) are restricted to operator-level grants.

### Surface B: connector provisioning credentials

The connector provisions per-principal credentials in external systems
(e.g., Forgejo API tokens). This uses `credential/provision/key/*`
grants, enforced by the daemon. The connector's PrincipalAssignment
includes grants like:

```json
{"actions": ["credential/provision/key/FORGEJO_TOKEN"], "targets": ["**"]}
```

The target patterns scope which principals the connector can provision
for. `"**"` means all principals. A restricted connector might use
`{"targets": ["iree/**"]}` to limit provisioning to a specific
namespace.

### Multiple connectors of the same type

Two Forgejo instances (internal and public mirrors) register as
different service principals with different roles: `forgejo/internal`
and `forgejo/public`. The action namespaces are `forgejo/internal/*`
and `forgejo/public/*`. Grants can target one or both:

- `{"actions": ["forgejo/internal/create-repo"]}` — create repos on
  internal Forgejo only
- `{"actions": ["forgejo/*/list-repos"]}` — list repos on both
- `{"actions": ["forgejo/**/report-status"]}` — report status on both

The token audience distinguishes instances: a token with audience
`"forgejo/internal"` is rejected by the `forgejo/public` service.

---

## Relationship to Matrix Power Levels

Matrix power levels are the homeserver's authorization mechanism.
Bureau's authorization framework sits on top of it:

- **Power levels** gate state event modifications at the homeserver
  level. Bureau cannot bypass them. They are the floor.
- **Bureau authorization** gates actions at the daemon/proxy/service
  level. Even if a principal has sufficient power level to modify a
  state event, Bureau can additionally require the right grant.

The `power_level_grants` in room authorization policy create a bridge:
a principal's Matrix power level translates into Bureau grants. This
lets room administrators control Bureau authorization through the
familiar power level mechanism without conflating the two systems.

The framework does not auto-sync Bureau grants with power levels. They
are separate layers. Power levels are coarse (a number) and
room-scoped. Bureau grants are fine-grained (action + target patterns)
and can span rooms. Using power levels as *input* to grant computation
gives the best of both: coarse control via Matrix-native tools,
fine-grained control via Bureau-native grants.

---

## Temporal Grants and Ticket-Backed Access Requests

### The problem

Static authorization (template-defined grants, room-level grants)
handles the common case: coders can create tickets, PMs can interrupt
agents, operators can observe everything. But some access needs are
dynamic:

- A coder debugging a database issue needs temporary observation
  access to the database service principal.
- A TPM needs temporary fleet management permissions to handle an
  incident outside its normal workstream.
- An operator grants a PM temporary credential provisioning capability
  for an emergency rotation.

These are legitimate, time-bounded access needs that should not be
hardcoded into templates. They need an approval workflow, an audit
trail, and automatic expiry.

### Ticket-backed access flow

The ticket system is the approval mechanism:

1. **Agent requests access**: creates a ticket describing what access
   is needed, why, and for how long.

2. **Approval chain**: the ticket routes through appropriate approvers
   based on the requested action and target. Low-risk requests
   (observe access to a project member) can be approved by a TPM.
   Medium-risk requests (cross-project access, credential
   provisioning) require PM approval. High-risk requests (fleet
   operations, operator-level access) require a human operator.

   The approval chain is itself authorization-gated: the approver
   must have a `grant/approve/*` action for the type of access being
   requested. The `grant/approve/*` namespace mirrors the regular
   action namespace: `grant/approve/observe` means "can approve
   observe grants." The ability to grant access is itself an
   authorized action.

3. **Grant creation**: once approved, the approver publishes a
   temporal grant as a state event (`m.bureau.temporal_grant`). The
   grant includes the actions, targets, expiry, ticket reference,
   and who approved it.

4. **Immediate effect**: the daemon sees the state event via /sync,
   merges the temporal grant into the principal's resolved policy,
   mints new service tokens with updated grants, and pushes updated
   policy to the proxy if needed. No sandbox restart.

5. **Automatic expiry**: after the specified duration, the daemon's
   expiry sweep removes the grant. The principal's access disappears.
   The ticket is updated noting the grant expired.

6. **Early revocation**: publishing a tombstone (empty content for the
   same state key) removes the grant immediately.

7. **Audit trail**: the ticket contains the full history — who
   requested, what justification, who approved, when granted, when
   expired or revoked. The ticket field on the grant links back to
   this record.

### What does not need a ticket

Static grants (template-defined, room-level, machine-default) do not
go through the ticket workflow. They are configuration. Temporal grants
are for access beyond what static policy provides — escalation, not
baseline.

---

## Capabilities Enabled by the Framework

### Interrupt authority

A PM has a grant for `interrupt` targeting `bureau/dev/**`. A coder
has an allowance for `interrupt` from `bureau/dev/pm`. Both sides
match — authorized. A coder trying to interrupt another coder has no
grant for `interrupt` — denied at the subject side.

### MCP tool filtering

The MCP server reads the principal's resolved grants, extracts service
action patterns, and exposes only matching CLI commands as MCP tools.
A coder sees ticket, artifact, and workspace tools. A sysadmin agent
additionally sees fleet tools. This is a subject-only check.

### Ticket operations

Room-level policy in the workstream room grants `ticket/create` and
`ticket/assign` to all members, and `ticket/close` and
`ticket/reopen` to power level 50+. Any room member can create
tickets; only the TPM or admin can close them.

### Operator access

An operator's authorization comes from room membership and power
levels, not from their localpart's position in a namespace hierarchy.
Power level 100 in config rooms, combined with room-level
`power_level_grants` at PL 100 granting `"**"`, gives the operator
all actions. Any allowance pattern matching the operator's localpart
completes the target side.

---

## Audit Logging

Authorization decisions need to be inspectable without SSHing into
every machine in a fleet. The daemon posts audit events to Matrix so
sysadmins can review denials and sensitive grants from any client.

### What gets logged

Not all authorization decisions are worth recording. Service directory
filtering runs on every `/v1/services` request per service entry —
logging each hidden entry would overwhelm any log with no diagnostic
value. Routine successful grants (`matrix/join`, `ticket/list`) are
similarly high-volume and low-signal.

The audit log captures two categories:

- **Denials of attempted actions.** An agent tried to do something and
  was blocked. Either a misconfiguration or an unauthorized attempt —
  both warrant investigation.
- **Successful checks of sensitive actions.** High-privilege operations
  where an audit trail matters: `credential/provision/**`,
  `interrupt/**`, `fleet/**`, `observe/read-write`, and
  `grant/approve/**`. This set is defined in code, not configuration —
  what counts as "sensitive" is a security decision, not an operational
  knob.

Grant lifecycle events (temporal grant creation, expiry, revocation)
are also logged as audit events since they represent dynamic privilege
changes.

### Where audit events live

Audit events are `m.bureau.audit` timeline events in the per-machine
config room (`#bureau/config/<machine>`). Config rooms are the right
location because:

- The audience is already correct — admin, machine daemon, fleet
  controllers.
- The authorization policy that produced the decision is in the same
  room (`m.bureau.machine_config`), so denials have full context
  without room-hopping.
- Each machine's volume scales with its own principal count and denial
  rate, not the fleet's.

Fleet-wide inspection is a client concern: a `bureau audit` command
aggregates across config rooms by enumerating machines and filtering
events.

Only the daemon posts audit events to Matrix — it is the process with
a Matrix session and access to the config room. The proxy and services
(which enforce authorization independently) log denials locally via
slog in structured format. They have no Matrix session and no need for
one: proxy denials reflect grants the daemon already computed, and
service denials reflect token grants the daemon already minted. Both
are visible through `bureau auth check` without a Matrix round-trip.

### API design

The authorization package provides result-returning variants of the
boolean check functions (`TargetCheck`, `GrantsCheck`) that return
the deny reason and matched rules alongside the decision. The daemon
uses these for observation and cross-principal authorization where
the full trace feeds the audit event. The existing boolean functions
(`TargetAllows`, `GrantsAllow`) remain for call sites that don't need
the trace, like list filtering where logging every hidden entry would
be noise.

Audit event posting is asynchronous — the authorization decision
returns immediately, and a goroutine posts the event to Matrix with
retry. If the homeserver is unavailable, the event is logged locally
and the Matrix post is dropped. Authorization is never delayed by
Matrix availability.

---

## Relationship to Other Design Documents

- **[credentials.md](credentials.md)** — credential provisioning
  scope becomes grants. The encryption model (age, TPM, machine keys)
  is orthogonal: authorization determines who can provision, encryption
  determines who can decrypt.

- **[architecture.md](architecture.md)** — the daemon is the primary
  authorization evaluator. The proxy receives pre-computed grants via
  admin socket. Services verify tokens independently.

- **[tickets.md](tickets.md)** — the ticket service uses the
  framework for mutation authorization. Room-level grants control
  ticket operations. Temporal grants flow through the ticket approval
  workflow.

- **[fleet.md](fleet.md)** — fleet operations (`fleet/assign`,
  `fleet/provision`) are gated by grants. Cross-fleet preemption is
  an authorization decision: a higher-priority fleet controller needs
  a `fleet/assign` grant targeting the lower-priority controller's
  machines.

- **[observation.md](observation.md)** — observation authorization
  uses `observe` and `observe/read-write` actions with two-sided
  policy (grants on the observer, allowances on the target).

- **[pipelines.md](pipelines.md)** — pipeline principals that create
  rooms or provision credentials need appropriate grants. Matrix
  operations use `matrix/*` action grants.

- **[information-architecture.md](information-architecture.md)** —
  room membership and power levels interact with the authorization
  layer. Room-level authorization policy is stored as state events,
  consistent with the Matrix data model.

- **[forgejo.md](forgejo.md)** — reference implementation for
  external service connectors. Demonstrates both authorization
  surfaces: agent-to-socket (token grants) and connector-to-daemon
  (credential provisioning grants).

- **[workspace.md](workspace.md)** — workspace operations map to
  `workspace/*` actions. Power level grants bridge room power levels
  to workspace authorization.

- **[agent-layering.md](agent-layering.md)** — interrupt authority,
  MCP tool filtering, and session access use the framework. Agent
  wrappers check authorization for incoming messages.
