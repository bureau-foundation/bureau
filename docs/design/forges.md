# Forge Connectors

Bureau integrates with git forges (GitHub, Forgejo, GitLab) through a
unified connector model. To agents and services, all forges present the
same interface: subscription-based event delivery, entity mapping to
tickets and pipelines, and operations through a common socket API. Each
forge has its own connector binary that translates between the forge's
native API and Bureau's forge abstraction.

This document defines the abstraction (event types, subscription model,
entity mapping, room configuration, socket API) and the per-provider
details (authentication, identity, webhook ingestion) for each supported
forge.

Companion to credentials.md (connector-managed credential lifecycle),
tickets.md (work item integration), pipelines.md (CI integration), and
authorization.md (grant model).

---

## Design Principles

**One schema, many providers.** `lib/schema/forge/` defines
provider-agnostic event types: PushEvent, PullRequestEvent, IssueEvent,
ReviewEvent, WorkflowRunEvent. Each connector translates its forge's
native webhook payloads into these common types. Agents subscribe to
events and receive the same typed structs regardless of whether the repo
lives on GitHub or a self-hosted Forgejo instance.

**Subscription, not broadcast.** Events are delivered to subscribed
agents via CBOR streams, not broadcast to rooms as Matrix messages. Repos
at scale produce high event volume. Broadcasting creates noise.
Subscriptions let each agent see exactly what it needs. Room-level triage
subscriptions handle the "someone needs to see everything" case.

**Forges are not identity providers.** Bureau owns identity. A Bureau
agent is identified by its Matrix entity, not by a forge account. How
that identity maps to a forge depends on the forge's model:
- **Self-hosted forges** (Forgejo, GitLab): the connector creates
  per-principal accounts and provisions per-agent credentials. The forge
  has native awareness of each agent's identity.
- **Cloud forges** (GitHub): a single service account (GitHub App)
  handles all operations via the forge's REST API. The App bot is the
  commit author; agent and operator identity are encoded in
  `Co-authored-by` trailers (custom author fields are not used because
  they break the forge's auto-signing — see Two Classes of Forge
  Interaction). The forge sees one bot; Bureau's connector attributes
  actions to the correct agent via proxy attribution tracking.

The abstraction accommodates both: the socket API and event schema are
identical. The identity layer adapts per provider.

**The connector is a ticket service client.** Issue-to-ticket mapping,
PR-to-review-ticket creation, and gate resolution all flow through the
ticket service's socket API. The forge connector never stores tickets —
it translates forge events into ticket operations. This means ticket
infrastructure (indexing, readiness, search, gates, notes) is shared
across all forges without duplication.

---

## The Forge Abstraction

### Event Schema (`lib/schema/forge/`)

Provider-agnostic event types. Each connector translates its forge's
native webhooks into these types at ingestion time.

```go
// PushEvent represents a push to a repository branch.
type PushEvent struct {
    Provider  string   `cbor:"provider"`          // "github", "forgejo", "gitlab"
    Repo      string   `cbor:"repo"`              // "owner/repo"
    Ref       string   `cbor:"ref"`               // "refs/heads/main"
    Before    string   `cbor:"before"`            // previous HEAD SHA
    After     string   `cbor:"after"`             // new HEAD SHA
    Commits   []Commit `cbor:"commits"`
    Sender    string   `cbor:"sender"`            // forge username
    BureauEntity string `cbor:"bureau_entity,omitempty"` // mapped Bureau entity
    Summary   string   `cbor:"summary"`           // human-readable with links
    URL       string   `cbor:"url"`               // web URL to diff/compare
}

// PullRequestEvent represents a change to a pull request (or merge
// request on GitLab).
type PullRequestEvent struct {
    Provider     string `cbor:"provider"`
    Repo         string `cbor:"repo"`
    Number       int    `cbor:"number"`
    Action       string `cbor:"action"`       // opened, closed, merged, synchronize, review_requested, ...
    Title        string `cbor:"title"`
    Author       string `cbor:"author"`       // forge username
    BureauEntity string `cbor:"bureau_entity,omitempty"`
    HeadRef      string `cbor:"head_ref"`
    BaseRef      string `cbor:"base_ref"`
    HeadSHA      string `cbor:"head_sha"`
    Draft        bool   `cbor:"draft"`
    Summary      string `cbor:"summary"`
    URL          string `cbor:"url"`
}

// IssueEvent represents a change to an issue.
type IssueEvent struct {
    Provider     string   `cbor:"provider"`
    Repo         string   `cbor:"repo"`
    Number       int      `cbor:"number"`
    Action       string   `cbor:"action"`       // opened, closed, labeled, assigned, commented, ...
    Title        string   `cbor:"title"`
    Author       string   `cbor:"author"`
    BureauEntity string   `cbor:"bureau_entity,omitempty"`
    Labels       []string `cbor:"labels,omitempty"`
    Summary      string   `cbor:"summary"`
    URL          string   `cbor:"url"`
}

// ReviewEvent represents a code review submission.
type ReviewEvent struct {
    Provider     string `cbor:"provider"`
    Repo         string `cbor:"repo"`
    PRNumber     int    `cbor:"pr_number"`
    Reviewer     string `cbor:"reviewer"`     // forge username
    BureauEntity string `cbor:"bureau_entity,omitempty"`
    State        string `cbor:"state"`        // approved, changes_requested, commented
    Body         string `cbor:"body"`
    Summary      string `cbor:"summary"`
    URL          string `cbor:"url"`
}

// CommentEvent represents a comment on an issue or PR.
type CommentEvent struct {
    Provider     string `cbor:"provider"`
    Repo         string `cbor:"repo"`
    EntityType   string `cbor:"entity_type"`  // "issue" or "pull_request"
    EntityNumber int    `cbor:"entity_number"`
    Author       string `cbor:"author"`
    BureauEntity string `cbor:"bureau_entity,omitempty"`
    Body         string `cbor:"body"`
    Summary      string `cbor:"summary"`
    URL          string `cbor:"url"`
}

// CIStatusEvent represents a CI/CD pipeline or workflow run status
// change. Covers GitHub Actions, Forgejo Actions, GitLab CI.
type CIStatusEvent struct {
    Provider   string `cbor:"provider"`
    Repo       string `cbor:"repo"`
    RunID      string `cbor:"run_id"`         // provider-specific run identifier
    Workflow   string `cbor:"workflow"`        // workflow/pipeline name
    Status     string `cbor:"status"`          // queued, in_progress, completed
    Conclusion string `cbor:"conclusion"`      // success, failure, cancelled, ""
    HeadSHA    string `cbor:"head_sha"`
    Branch     string `cbor:"branch"`
    PRNumber   int    `cbor:"pr_number,omitempty"`
    URL        string `cbor:"url"`
    Summary    string `cbor:"summary"`
}
```

Every event carries a `Summary` field: a human-readable one-liner with
markdown links. Agents that want to post status messages to rooms or
threads can use the summary directly. Agents that need structured data
use the typed fields.

Every event carries a `BureauEntity` field: populated when the forge
username maps to a Bureau principal via the connector's identity mapping.
Empty when the actor is an external contributor with no Bureau identity.

### Repository Binding

Rooms declare their association with forge repositories via state events:

```
Event type: m.bureau.repository
State key: "<provider>/<owner>/<repo>" (e.g., "github/bureau-foundation/bureau")

Content:
{
    "provider": "github",
    "owner": "bureau-foundation",
    "repo": "bureau",
    "url": "https://github.com/bureau-foundation/bureau",
    "clone_https": "https://github.com/bureau-foundation/bureau.git",
    "clone_ssh": "git@github.com:bureau-foundation/bureau.git"
}
```

The state key encodes provider/owner/repo so a room can bind to repos on
multiple forges simultaneously. A room tracking a project on both GitHub
(public mirror) and Forgejo (internal primary) has two bindings with
different state keys.

### Room Configuration

Per-room, per-repo configuration:

```
Event type: m.bureau.forge_config
State key: "<provider>/<owner>/<repo>"

Content:
{
    "provider": "github",
    "repo": "bureau-foundation/bureau",
    "events": ["push", "pull_request", "issues", "review", "ci_status"],
    "issue_sync": "import",
    "pr_review_tickets": true,
    "ci_monitor": true,
    "triage_filter": {
        "labels": ["bug", "feature-request"],
        "event_types": ["issue_opened", "pr_opened"]
    },
    "auto_subscribe": true
}
```

**`events`**: which forge event categories to process for this room.
Events not listed are received by the connector but not dispatched to
subscribers in this room. Uses the common event category names (push,
pull_request, issues, review, ci_status, comment), not provider-specific
webhook event names.

**`issue_sync`**: `"none"` (default), `"import"`, or `"bidirectional"`.
Controls whether forge issues become Bureau tickets and whether changes
flow back. See Entity Mapping.

**`pr_review_tickets`**: create Bureau review tickets with CI and review
gates when PRs are opened. Default true.

**`ci_monitor`**: publish `m.bureau.pipeline_result` state events for
CI/CD runs. Default true.

**`triage_filter`**: filter for room subscriptions. The triage stream
delivers only events matching this filter. When empty, all events for
the configured categories are delivered.

**`auto_subscribe`**: enable webhook-driven auto-subscribe for this
repo's events. Default true.

Multiple rooms can bind to the same repo with different configs. A
development room gets push events and CI results. A review room gets PR
events. A triage room gets new issues matching specific labels.

### Socket API

All forge connectors expose the same CBOR-over-unix-socket API. The
service role name is provider-specific (`github`, `forgejo`, `gitlab`)
but the action names and request/response schemas are identical.

**Subscription actions:**
- `subscribe` — stream action. Subscribe to entity events (specific
  issue/PR/CI run) or room events (all matching events for room-bound
  repos). Returns a CBOR stream.
- `unsubscribe` — remove an entity subscription.
- `list-subscriptions` — query active subscriptions for the calling
  agent.
- `auto-subscribe-config` — configure auto-subscribe rules.

**Entity operations:**
- `create-issue` — create a forge issue.
- `create-comment` — comment on an issue or PR.
- `create-review` — submit a PR review.
- `merge-pr` — merge a pull request.
- `trigger-workflow` — dispatch a CI workflow run.
- `get-workflow-logs` — retrieve CI run logs on demand.

**Repository operations:**
- `list-repos` — repos the calling agent can access.
- `repo-info` — metadata for a specific repo.

**Status reporting:**
- `report-status` — push commit status to the forge.

All actions require service identity tokens with `<provider>/*` grants
(e.g., `github/subscribe`, `forgejo/merge-pr`). Cross-provider grants
are possible via `*/report-status` for agents that work across multiple
forges.

### Grant Naming Convention

Following the pattern from authorization.md:

```
<provider>/subscribe          — subscribe to events
<provider>/create-issue       — create issues
<provider>/create-comment     — comment on issues/PRs
<provider>/create-review      — submit code reviews
<provider>/merge-pr           — merge pull requests
<provider>/trigger-workflow   — dispatch CI runs
<provider>/get-workflow-logs  — retrieve CI logs
<provider>/list-repos         — list repositories
<provider>/repo-info          — repository metadata
<provider>/report-status      — push commit status
```

Read-only agents get `<provider>/subscribe` + `<provider>/list-repos`.
Agents that push code get the above plus `<provider>/create-review` and
`<provider>/merge-pr`. Admin agents get `<provider>/**`.

---

## Access Tiers

Agents interact with forges through three access paths, each with different
levels of mediation. The proxy enforces all three via grants — there is no
way to bypass access control regardless of which path an agent uses.

### Tier 1: Structured CBOR Socket (Default)

Most agents interact with forges exclusively through the connector's CBOR
socket API. Every operation is a typed action (`create-issue`,
`create-review`, `merge-pr`, `trigger-workflow`, etc.) with validated
parameters. The connector has full visibility into what the agent is doing
because every operation passes through the connector's action handlers.

Grants required: `<provider>/subscribe`, `<provider>/create-issue`,
etc. — the specific actions from the Grant Naming Convention section above.

This is the default and recommended access path. It gives Bureau complete
control over what agents can do, enables fine-grained audit logging, and
provides the richest integration with subscriptions and entity mapping.
Agents at this tier never touch `gh`, `tea`, or any forge CLI directly.

### Tier 2: Direct Git (Code Agents)

Every agent with code access uses `git` through the proxy. Git HTTPS
authentication is handled by the proxy's credential injection — the
agent's git client connects to the forge's HTTPS clone URL, and the proxy
injects the appropriate token.

How direct git works depends on the forge's identity model:

- **Per-principal forges** (Forgejo): each agent authenticates as its own
  forge user. Git operations are attributed natively. Agents use `git
  push` normally. Commits are signed with the principal's SSH key,
  verified by the forge.
- **Shared-account forges** (GitHub): autonomous agents do not use `git
  push` — all git operations go through the REST API so that GitHub
  auto-signs commits (see Two Classes of Forge Interaction).
  Operator-driven agents use `git push` with the operator's own
  credentials, exactly as a human developer would.

No additional grants beyond the agent's base code-access grant. Git push
and pull are implicit in having forge repository access.

### Tier 3: Full Forge CLI (Admin Agents)

Team-admin agents and operator-controlled agents may need the full
surface area of the forge's CLI tool (`gh` for GitHub, `tea` for
Forgejo). These CLIs have hundreds of operations — repository settings,
branch protection rules, webhook configuration, team management, release
management, Actions/runner administration — and wrapping each one in a
structured CBOR action would be an unbounded engineering effort for
diminishing returns.

Instead, admin agents access the forge CLI through the proxy's HTTP
service. The proxy registers a `github` (or `forgejo`) HTTP service that
forwards API requests to the forge with credential injection. The `gh`
CLI points at `localhost:<proxy-port>/v1/http/github/` and the proxy
handles authentication.

Grants required: `github/**` (or `forgejo/**`) — the wildcard grant that
permits any operation through the forge's HTTP API. This is deliberately
broad and reserved for admin agents.

The proxy records every HTTP request and response at this tier (method,
path, response status, entity references extracted from responses). This
enables:
- Audit logging of admin operations
- Attribution tracking for auto-subscribe (a `gh pr create` through the
  proxy generates the same attribution record as a CBOR `create-issue`)
- Rate limit awareness (the proxy tracks `X-RateLimit-*` headers and can
  warn or throttle before quota exhaustion)

### Multi-Forge Rooms

A single Bureau room can bind to repos on multiple forges simultaneously:
an internal Forgejo instance hosting the primary development fork, and a
public GitHub mirror for upstream collaboration. The state key on
`m.bureau.repository` and `m.bureau.forge_config` distinguishes them
(`forgejo/internal/project` vs `github/upstream/project`).

Subscriptions are fully qualified: an agent subscribes to events on a
specific provider/repo, not just a repo name. Entity operations specify
the target provider. Agents that work across both forges subscribe to
both and receive events from each with the `provider` field
distinguishing the source.

Triage agents watching a multi-forge room see events from all bound repos
and route them according to the provider. A PR from an external
contributor on the GitHub mirror and a PR from an internal agent on
Forgejo both create Bureau tickets in the same room — the ticket's
`origin` field records which forge it came from.

---

## Two Classes of Forge Interaction

Bureau agents interact with external forges in two fundamentally
different modes. The distinction is driven by a hard constraint in
GitHub's API: auto-signing and custom authorship are mutually exclusive.
Setting a custom `author` on an API-created commit causes GitHub to skip
signing entirely — the commit arrives unsigned, not with an invalid
signature.

### Operator-Driven Agents

An operator-driven agent works as an extension of a human operator. The
canonical example is a "lead developer" principal that a human uses as
their AI pair programmer — similar to how Claude Code works today.

These agents use the **operator's own forge credentials** through the
proxy. The operator's PAT or SSH key is in the proxy's credential
bundle. Commits are authored by the operator, signed with the operator's
key (already registered on their forge account), and co-authored by the
agent:

```
Author: Ben Vanik <ben.vanik@gmail.com>

feat: implement retry backoff for webhook delivery

Co-authored-by: Claude <noreply@anthropic.com>
```

"Verified" badge on GitHub because the operator's key is registered.
Normal `git push` through the proxy — no API-mediated path needed. The
agent has full git capabilities: interactive rebase, conflict resolution,
complex merges, emergency fixups.

This class is appropriate for work that requires judgment, coordination
with humans, or operations that the structured API path cannot express.

### Autonomous Agents

Autonomous agents work independently on well-scoped tasks assigned by a
PM agent or ticket system. They use the **GitHub App** (or equivalent
shared service account) for all forge operations.

On GitHub, all git operations go through the REST API (not `git push`).
The API-mediated path creates commits without custom author/committer
fields, which causes GitHub to auto-sign with its web-flow GPG key:

```
Author: bureau-forge[bot] <ID+bureau-forge[bot]@users.noreply.github.com>
Committer: GitHub <noreply@github.com>

feat: implement retry backoff for webhook delivery

Co-authored-by: code-reviewer <code-reviewer@agents.bureau.foundation>
Co-authored-by: Ben Vanik <ben.vanik@gmail.com>
```

"Verified" badge. The App bot is the author. The agent and the
responsible operator are in Co-authored-by trailers.

The API-mediated path supports: multi-file atomic commits (3 API calls:
tree → commit → ref update), force push for rebases (`PATCH refs` with
`force: true`), branch creation/deletion, nested directory creation, and
all tree modes (regular, executable, symlink, submodule). The practical
throughput ceiling is ~10-15 multi-file commits per minute with the
installation token budget (5,000 requests/hour base, scaling to 12,500
with users/repos, or 15,000 on Enterprise Cloud).

On Forgejo, autonomous agents have per-principal accounts and use normal
`git push` — the API-mediated path is GitHub-specific.

---

## Subscription Model

### Subscription Types

**Entity subscriptions** target a specific forge entity: a PR, an issue,
or a CI run. "Notify me about changes to PR #42 in owner/repo." Entity
subscriptions are created explicitly by agents or auto-created by the
auto-subscribe mechanism. Ephemeral by default: cleaned up when the agent
disconnects or the entity closes. Can be made persistent (survives
disconnection, events queued for re-delivery on reconnect).

**Room subscriptions** target event categories on all repos bound to a
room. "Notify me about all new issues in repos bound to this triage
room." Room subscriptions are always persistent. Used by triage agents
that need to see everything — new issues, new PRs, CI failures — for
routing and organization. A room subscription includes a filter so a
triage agent can watch for new issues without receiving every push event.

### Stream Protocol

The `subscribe` stream action returns CBOR frames following the same
pattern as the ticket service subscribe stream:

```go
type SubscribeFrame struct {
    Type     string          `cbor:"type"`
    Event    *forge.Event    `cbor:"event,omitempty"`    // for "event" frames
    EntityRef *EntityRef     `cbor:"entity_ref,omitempty"` // for "entity_closed"
    Message  string          `cbor:"message,omitempty"`  // for "error" frames
}
```

Frame types:
- **`event`**: a forge event occurred. Contains a typed event struct
  (PushEvent, PullRequestEvent, etc.) wrapped in a discriminated union.
- **`entity_closed`**: the entity was closed/merged. Ephemeral
  subscriptions auto-clean. Persistent subscriptions remain (the agent
  may want to see reopen events).
- **`heartbeat`**: connection liveness probe (30-second interval).
- **`resync`**: subscriber buffer overflowed. Client should clear local
  state and expect a fresh event replay.
- **`caught_up`**: initial backfill of pending events (for persistent
  subscriptions with queued events) is complete. Live events follow.
- **`error`**: terminal error, connection will close.

### Auto-Subscribe

Agents should not have to remember to subscribe to things they create or
are assigned to. The auto-subscribe mechanism makes this invisible.

**Webhook-driven auto-subscribe**: When a webhook arrives and the
connector processes it, the connector checks whether any Bureau agent
should be auto-subscribed:

1. Extract forge usernames from the event (author, assignee, reviewer,
   mentioned users).
2. Look up each username in the identity mapping to find the
   corresponding Bureau entity.
3. Check the entity's auto-subscribe rules against the event type.
4. If a rule matches, create a persistent entity subscription.
5. The agent receives events on its next `subscribe` stream connection.

**Auto-subscribe rules** (per-agent, stored as
`m.bureau.forge_auto_subscribe` state events):

```cbor
{
    "on_author": true,           // subscribe when I create an entity
    "on_assign": true,           // subscribe when I'm assigned
    "on_mention": true,          // subscribe when I'm @mentioned
    "on_review_request": true    // subscribe when I'm requested as reviewer
}
```

Default: all enabled. An agent that wants full manual control disables
all rules and subscribes explicitly.

**How identity mapping enables auto-subscribe for GitHub**: When an agent
pushes a PR through the GitHub App token, the connector knows which agent
pushed (the proxy tracks which agent's request triggered the push). The
connector records this attribution and uses it to auto-subscribe the
agent when the PR webhook arrives. Even though GitHub sees the App bot as
the pusher, Bureau's connector knows the originating agent.

---

## Entity Mapping

### Issues → Bureau Tickets

When a forge issue event arrives for a room with `issue_sync` enabled:

**Import (forge → Bureau):**
1. The connector calls the ticket service's `import` action:
   - `origin: {source: "<provider>", external_ref: "owner/repo#42"}`
   - Title, body, labels, assignee (mapped to Bureau entity if possible)
   - Priority derived from labels (e.g., `priority:critical` → P0)
2. Issue comments → ticket `add-note` with author attribution.
3. Label changes → ticket label updates.
4. Issue close → ticket close with disposition matching forge close
   reason.

**Bidirectional (Bureau ↔ forge):**
When `issue_sync` is `"bidirectional"`, the connector watches the ticket
service subscribe stream for changes to tickets with forge origins and
pushes them back:
- Ticket close → forge issue close
- Ticket note → forge issue comment
- Ticket label change → forge label update
- Ticket assignment → forge assignee update (if identity-mapped)

Conflict resolution: last-writer-wins with warnings logged. The common
case is unidirectional flow: external contributors work in the forge,
Bureau agents work in tickets.

### PRs → Bureau Review Tickets

Pull requests (or merge requests) map to Bureau tickets with type
`"review"` and gates:

1. **PR opened** → create ticket:
   - Type: `"review"`, origin: `{source: "<provider>", external_ref: "owner/repo#42"}`
   - Gates: `ci_status` (state_event type, satisfied when CI passes),
     `review_approval` (human type, satisfied when reviews approve)

2. **Review submitted** → gate resolution:
   - Approved → resolve `review_approval` gate (or increment count)
   - Changes requested → block gate with feedback
   - Commented → add note to review ticket

3. **CI status change** → gate resolution:
   - All checks passed → resolve `ci_status` gate
   - Any check failed → block gate with failure details

4. **PR merged** → close ticket, disposition `"completed"`

5. **PR closed without merge** → close ticket, disposition `"abandoned"`

6. **Review requested** → assign ticket to reviewer, auto-subscribe
   reviewer

### CI Runs → Bureau Pipeline Representation

CI/CD runs are represented in Bureau as lightweight pipeline records:

**Monitor mode** (passive): receive webhooks, publish state:
- Connector publishes `m.bureau.pipeline_result` state events in the
  bound room. State key: `<provider>/<owner>/<repo>/run/<run_id>`.
- If a review ticket exists for the PR, resolve or block the `ci_status`
  gate.
- Subscription events dispatched to agents watching the PR/branch.

**Trigger mode** (active): dispatch workflow, monitor result:
- `trigger-workflow` socket action dispatches a CI run via the forge API.
- The connector monitors the resulting run via webhooks.
- Status updates delivered via the subscription stream.

**Log access**: `get-workflow-logs` fetches logs on demand. Logs are
cached briefly (default 15 minutes) to avoid repeated API calls.

---

## Identity and Attribution

### Two Identity Modes

Forge connectors handle identity in one of two modes, depending on the
forge's model and economics. This is orthogonal to the operator-driven
vs. autonomous distinction in Two Classes of Forge Interaction — that
section describes how an *agent* operates; this section describes how the
*forge* represents identity. Per-principal forges support both
operator-driven and autonomous agents natively. Shared-account forges
require the API-mediated path for autonomous agents but support
operator-driven agents using the operator's own credentials.

**Per-principal accounts** (Forgejo, self-hosted GitLab, Gitea):
- The connector creates a forge user for each Bureau principal that needs
  git access.
- Each principal gets its own forge credentials (API token, SSH signing
  key) provisioned via the sysadmin credential flow.
- The forge has native awareness of each agent's identity: commits,
  issues, PRs are attributed to the agent's forge user.
- Permission sync: Matrix room membership → forge org/team membership.
  Bureau is authoritative; the connector overwrites drift.
- Username mapping: Bureau localpart with `/` replaced by `-`, lowercase,
  constrained to forge username limits.

**Shared service account** (GitHub, cloud-hosted forges with per-seat
pricing):
- One service account (GitHub App installation) handles all operations.
- All git operations go through the forge's REST API (not `git push`),
  so the forge auto-signs commits with its own key.
- The App bot appears as the commit author. Agent and operator identity
  are encoded in `Co-authored-by` trailers.
- PR comments and reviews are posted by the App bot, with the agent's
  Bureau identity noted in the comment body.
- No per-principal forge tokens — the proxy injects the shared
  installation token for all agents.

The forge abstraction handles both transparently. The socket API, event
types, and ticket integration are identical. What differs is the
connector's internal credential management and how it attributes actions
to agents.

### Work Identity

External git identity is decoupled from internal principal identity.
Principals are ephemeral workers — a PM agent may create a dozen
principals to complete a task, and those principals are cleaned up when
the work is done. Git history is permanent. Tying external commits to
specific principals creates dead references, leaks internal organization,
and breaks round-trip workflows when review feedback arrives for a
principal that no longer exists.

External attribution maps to the level of **accountability**, not
execution:

**Principal** (internal, ephemeral): The current worker. Has its own
signing key, its own Forgejo account (on per-principal forges), its own
commit history on the internal forge. Bureau tracks exactly what each
principal did via tickets, context bindings, and session logs. This level
of provenance stays internal.

**Work identity** (external, stable): The entity that owns the
contribution on the external forge. Persists across principal lifetimes.
Configured at the room level (per-project) with a namespace-level
fallback. When review feedback arrives, the forge connector routes it
via the subscription model — whichever principal is currently assigned
to the work handles it, regardless of who originally authored the commit.

**Operator** (external, human): Always in the `Co-authored-by` chain on
autonomous agent commits. The accountable human who authorized, reviewed,
or directed the work. The operator identity comes from the authorization
chain — whoever approved the agent's deployment or the specific task.

For operator-driven agents, the work identity is the operator
themselves — exactly like today. For autonomous agents, the work identity
is the project/team identity configured for the room:

```
Author: bureau-forge[bot] <ID+bureau-forge[bot]@users.noreply.github.com>

feat: implement retry backoff for webhook delivery

Ticket: tkt-a3f9
Co-authored-by: IREE Team <iree-team@agents.bureau.foundation>
Co-authored-by: Ben Vanik <ben.vanik@gmail.com>
```

The ticket ID in the commit message (when appropriate) bridges external
commits to internal provenance. The forge connector maintains its own
mapping (external PR → internal ticket → full provenance chain) via
Matrix state events for cases where ticket IDs should not appear in
public commits.

#### Work Identity Configuration

Work identity is configured per-room via state events:

```
Event type: m.bureau.forge_work_identity
State key: "<provider>/<owner>/<repo>" (or "" for room default)

Content:
{
    "display_name": "IREE Team",
    "email": "iree-team@agents.bureau.foundation"
}
```

When no per-repo work identity is configured, the room default applies.
When no room default exists, the namespace-level default applies. The
namespace default is configured by the operator during `bureau github
setup` (or equivalent).

#### Agent Naming on External Forges

Agent identity in Co-authored-by trailers uses a per-agent email derived
from the Bureau principal identity on a domain the operator controls:

```
<principal-slug>@agents.<operator-domain>
```

For example: `code-reviewer@agents.bureau.foundation`,
`test-runner@agents.bureau.foundation`. The principal slug is derived
from the Bureau localpart (`agent/code-reviewer` → `code-reviewer`).

These emails do not need to correspond to real mailboxes or forge
accounts. They provide stable, human-readable attribution in git history.
If an operator later registers a forge account for an agent and
associates the email, the historical commits retroactively link.

On the internal forge (Forgejo), agents have full per-principal accounts
with whatever naming convention the connector uses. Internal naming is
fine-grained; external naming is deliberate and curated.

### Identity Mapping State Events

For per-principal identity forges:

```
Event type: m.bureau.forge_identity
State key: "<provider>/<matrix_localpart>"

Content:
{
    "provider": "forgejo",
    "matrix_user": "@iree/amdgpu/pm:bureau.local",
    "forge_user": "iree-amdgpu-pm",
    "forge_user_id": 42,
    "token_provisioned": true,
    "signing_key_registered": true,
    "last_synced": "2026-02-27T10:00:00Z"
}
```

For shared-account forges (GitHub), the identity mapping records the
association between Bureau entities and their Co-authored-by metadata,
even though the connector doesn't create forge accounts. This mapping
is used for:
- Auto-subscribe (webhook event attributed to a Bureau agent via proxy
  attribution tracking → auto-subscribe the agent)
- Entity attribution (populate the `BureauEntity` field on events when
  an external contributor's GitHub username maps to a Bureau entity)
- Provenance tracing (which agent's connector operation created which
  external commit)

### Proxy Attribution Tracking

When an autonomous agent creates a commit through the connector's CBOR
socket API, the connector has full visibility: it knows which agent
requested the operation, which repo and branch are affected, and the
resulting commit SHA. Attribution is straightforward.

When an admin agent uses `gh` or direct git operations through the
proxy's HTTP service (Tier 3 access), the proxy records attribution by
inspecting request/response pairs:

- `POST /repos/{owner}/{repo}/git/commits` → response includes the new
  commit SHA. The proxy records: agent X created commit Y on repo Z.
- `PATCH /repos/{owner}/{repo}/git/refs/heads/{branch}` with a new SHA →
  the proxy records: agent X updated branch B to commit Y.
- `POST /repos/{owner}/{repo}/pulls` → response includes the PR number.
  The proxy records: agent X created PR #42 on repo Z.

The connector queries these attribution records when processing webhooks.
The webhook payload attributes the action to the App bot, but the proxy
record reveals the actual agent. This enables auto-subscribe: when the
PR webhook arrives, the connector finds the proxy attribution record and
auto-subscribes the originating agent.

Attribution records are stored in a small in-memory ring buffer with a
short retention window (default 15 minutes). This is sufficient because
webhooks arrive within seconds of the triggering action. Records that
outlive the window are dropped — they're only needed for the
webhook-to-agent correlation, not for permanent audit (which is handled
by the proxy's telemetry and the connector's Matrix-based provenance).

### Permission Mapping (Per-Principal Forges Only)

For forges with per-principal accounts, the connector syncs permissions:

| Bureau concept | Forge concept | Mechanism |
|---|---|---|
| Space | Organization | Connector creates org for spaces with repos |
| Room with repos | Team in the org | Room membership → team membership |
| Power level 100 | Org owner | Admin control |
| Power level 50 | Team with write + admin | Push, manage settings |
| Power level 0 | Team with write | Push, create PRs |
| Read-only | Team with read | Clone, comment |
| Not a member | No access | No team membership |

Permission sync is event-driven via /sync (room membership and power
level changes). Bureau is always authoritative — changes made directly in
the forge are overwritten on the next sync cycle.

Shared-account forges (GitHub App) do not need permission sync — the App
has a fixed set of permissions per installation, and all agents share
those permissions. Fine-grained access control for agents happens at the
Bureau level (grants), not the forge level.

---

## Webhook Ingestion

### Architecture

Each forge connector runs an HTTP server alongside its CBOR socket. The
HTTP server receives webhook payloads from the forge, verifies
signatures, deserializes into common event types, and dispatches to the
subscription manager and entity mapping handlers.

### Signature Verification

All major forges sign webhook payloads:
- **GitHub**: HMAC-SHA256 (`X-Hub-Signature-256` header)
- **Forgejo**: HMAC-SHA256 (same as GitHub, `X-Gitea-Signature` header)
- **GitLab**: secret token in `X-Gitlab-Token` header (direct comparison,
  not HMAC)

The connector verifies signatures using the webhook secret from its
credential bundle. Rejected payloads are logged (source IP, event type,
delivery ID) and rejected with 401. No information disclosed in the
response body.

Replay protection: track delivery IDs and reject duplicates within a
configurable window (default 1 hour).

### Deployment Topologies

**Same-machine (self-hosted forges):**
Service listens on `localhost:<port>`. Forge sends webhooks directly. No
TLS needed on a trusted network. Simplest deployment.

**Cloudflare Tunnel (public forges — GitHub, GitLab.com):**
The standard deployment for receiving webhooks from public forges. A
`cloudflared` daemon on the Bureau machine opens persistent outbound
connections to Cloudflare's edge. A stable subdomain (e.g.,
`webhooks.bureau.example.com`) routes traffic through the tunnel to the
connector's HTTP listener on localhost.

Setup:
1. Create a Cloudflare Tunnel: `cloudflared tunnel create bureau-webhooks`
2. Configure DNS: CNAME `webhooks.bureau.example.com` →
   `<tunnel-id>.cfargotunnel.com`
3. Configure the tunnel to route traffic to `localhost:<connector-port>`
4. Configure the forge's webhook URL to
   `https://webhooks.bureau.example.com/webhook/<provider>`
5. The connector verifies HMAC signatures on the forwarded requests
   (Cloudflare passes the request body through byte-for-byte — HMAC
   verification works without modification)

The tunnel handles TLS termination. No ports exposed, no firewall rules,
no certificate management. The `cloudflared` daemon runs as a systemd
service alongside the Bureau daemon. Steady-state webhook delivery
latency through the tunnel is ~1-2 seconds.

For development and testing, `cloudflared tunnel --url
http://localhost:<port>` provides an instant temporary public URL with
no account, domain, or DNS configuration required.

**Reverse proxy (alternative):**
Nginx or Caddy terminates TLS and forwards to the connector on
localhost. Standard deployment pattern for machines with public IPs.

**Sandboxed with bridge:**
When the connector runs inside a Bureau sandbox, the bridge primitive
forwards webhook traffic from outside the sandbox to the connector's
HTTP socket inside. The bridge listens on a TCP port (managed by the
daemon), forwards to a unix socket bind-mounted into the sandbox.

### Webhook Auto-Configuration

**Per-repo webhooks** (Forgejo, GitLab): when the connector detects a new
`m.bureau.repository` binding alongside a `m.bureau.forge_config` event:

1. Check if a webhook exists for this connector's endpoint URL.
2. If not, create one via the forge API.
3. Configure the webhook to send all event types — filtering is
   per-room in Bureau, not per-webhook in the forge.
4. Store the webhook ID in the config state event for cleanup.

When a room unbinds from a repo, the connector removes the webhook if no
other rooms reference the repo.

**App-level webhooks** (GitHub): GitHub App webhooks are per-App, not
per-repo. The App receives events for all repositories in its
installation. The connector's webhook URL is configured once during
`bureau github setup`, not per-repo.

GitHub's API can update an existing App webhook (`PATCH /app/hook/config`)
but cannot create one — the initial webhook must be configured through
the App settings page at `github.com/settings/apps/<name>`. The setup
command guides the operator through this step and validates that the
webhook is active and receiving events.

**Installation permission acceptance**: when the App's permissions change
(adding new scopes like `issues` or `pull_requests`), the installation
must accept the new permissions before the corresponding events start
flowing. Until acceptance, the installation retains its old permissions
and the `events` array on the installation is empty for the new event
types. The setup command checks for this mismatch (App permissions vs
installation permissions) and alerts the operator.

---

## Credential Management

### Connector Credentials

Each connector's credential bundle, provisioned by the operator:

**GitHub connector:**
```
GITHUB_APP_ID:              "12345"
GITHUB_APP_PRIVATE_KEY:     "<PEM-encoded RSA private key>"
GITHUB_INSTALLATION_ID:     "67890"
FORGE_WEBHOOK_SECRET:       "<HMAC shared secret>"
```

The connector generates short-lived installation tokens (1-hour TTL,
5,000–12,500 requests/hour depending on org size, 15,000 on Enterprise
Cloud) from the App credentials. Token rotation is
automatic — the connector generates a fresh token before the current one
expires.

**Forgejo connector:**
```
FORGEJO_ADMIN_TOKEN:        "<admin API token>"
FORGEJO_URL:                "http://localhost:3000"
FORGE_WEBHOOK_SECRET:       "<HMAC shared secret>"
```

The admin token has full API access (user management, org/team
management, webhook configuration).

**Common credential key**: `FORGE_WEBHOOK_SECRET` is the same key name
across all providers. The connector uses it for signature verification
regardless of the underlying HMAC scheme.

### Per-Agent Credentials

**Per-principal forges (Forgejo):**
The connector provisions per-agent tokens via the sysadmin credential
flow. When a principal activates with the forge in its template's
`RequiredServices`:
1. Connector creates a forge user for the principal.
2. Generates a scoped API token.
3. Provisions the token into the principal's credential bundle.
4. Publishes the identity mapping state event.

The proxy injects the per-agent token into forge API requests and git
HTTPS authentication.

**Shared-account forges (GitHub):**
No per-agent forge credentials. The proxy injects the shared GitHub App
installation token for all agents accessing GitHub. The installation
token is refreshed by the connector before expiry and pushed to the
proxy via the service directory update mechanism.

Autonomous agents do not use `git push` — all git operations go through
the REST API (create tree → create commit → update ref) so that GitHub
auto-signs commits. The connector constructs `Co-authored-by` trailers
from the agent's work identity and the responsible operator's identity.
Agents never configure git author metadata for GitHub operations.

Operator-driven agents use the operator's own forge credentials (PAT or
SSH key) injected by the proxy. They use normal `git push` and the
operator's registered signing key. No shared App token needed.

### Emergency Revocation

When `m.bureau.credential_revocation` is published for a machine:
- **Per-principal forges**: the connector revokes all forge tokens for
  affected principals and deactivates their forge accounts.
- **Shared-account forges**: the connector notes the affected principals
  and removes their auto-subscriptions. The shared token continues to
  work (it's not per-agent), but the affected agents lose their
  sandbox access and can no longer reach the proxy.

---

## Triage Workflow

A triage agent monitors incoming forge activity and routes it to the
appropriate Bureau context:

1. Operator configures a triage room with `m.bureau.forge_config` binding
   repos, `issue_sync: "import"`, and a triage filter.

2. Triage agent connects with a room subscription:
   ```cbor
   {
       "action": "subscribe",
       "token": "<service_token>",
       "room": "iree/triage",
       "event_types": ["issue_opened", "pr_opened"]
   }
   ```

3. New issue from forge → connector creates Bureau ticket (if issue_sync
   enabled) → triage subscription event delivered → triage agent
   examines content, assigns, labels, routes.

4. New PR → connector creates review ticket with gates → triage event →
   triage agent assigns reviewer → reviewer auto-subscribed.

---

## Review Workflow

Full PR lifecycle through subscriptions:

1. **Agent creates PR**: pushes code via proxy, creates PR via forge API
   or `gh`/`tea` CLI. Auto-subscribed via author rule.

2. **CI runs**: forge triggers CI. Connector receives CI status webhooks.
   Events delivered to subscription stream. CI gate on review ticket
   updated.

3. **Review requested**: reviewer assigned (by triage agent, CODEOWNERS,
   or manually). Reviewer auto-subscribed. Review ticket assigned to
   reviewer.

4. **Review submitted**: events delivered to all subscribed agents.
   Review gate on ticket resolved or blocked. Notes added to ticket.

5. **Agent responds**: addresses feedback, pushes new commits, posts
   replies via socket API (`create-comment`, `create-review`).

6. **PR merged/closed**: review ticket closed. Entity_closed frame sent.
   Ephemeral subscriptions cleaned up.

---

## Actions / CI Workflow

### Monitoring

When repos have CI (GitHub Actions, Forgejo Actions, GitLab CI), the
connector monitors execution:

1. CI event webhook → connector translates to CIStatusEvent.
2. Connector publishes `m.bureau.pipeline_result` state event in bound
   room.
3. If review ticket exists for the PR, resolve/block CI gate.
4. Subscription events dispatched to agents watching the PR/branch.

### Triggering

Agents can dispatch CI runs:
```cbor
{
    "action": "trigger-workflow",
    "token": "<service_token>",
    "repo": "owner/repo",
    "workflow_id": "ci.yml",
    "ref": "refs/heads/feature-branch",
    "inputs": {"test_suite": "full"}
}
```

The connector dispatches via forge API, monitors via webhooks, delivers
status via subscription stream.

### Log Access

`get-workflow-logs` fetches logs on demand via forge API. Cached briefly
(15 minutes) to avoid repeated API calls.

---

## CLI Plugin Architecture

Forge CLIs should not live in the `bureau` binary. The ecosystem will
grow to many connectors (forges, cloud providers, messaging platforms,
etc.), and compiling every connector CLI into the main binary creates
unsustainable bloat.

### Git-Style Plugin Resolution

1. Parse the first non-flag argument as a potential subcommand.
2. Check built-in subcommands (machine, matrix, observe, etc.).
3. If no match, search PATH for `bureau-<name>`.
4. If found, execute `bureau-<name> <remaining args...>` with Bureau
   context via environment variables.
5. If not found, standard "unknown command" error with typo suggestions.

### Context Environment Variables

Set by `bureau` before executing a plugin:

| Variable | Description |
|---|---|
| `BUREAU_SERVER_NAME` | Homeserver name |
| `BUREAU_PROXY_SOCKET` | Agent's proxy socket (if in sandbox) |
| `BUREAU_SERVICE_SOCKET_DIR` | Base directory for service sockets |
| `BUREAU_USER_ID` | Agent's Matrix user ID |
| `BUREAU_NAMESPACE` | Current namespace |
| `BUREAU_FLEET` | Current fleet (if applicable) |
| `BUREAU_CONFIG_DIR` | Bureau config directory |

### MCP Plugin Discovery

The MCP server discovers plugin capabilities by scanning PATH for
`bureau-*` binaries and invoking `bureau-<name> --mcp-schema`. The
plugin returns JSON tool descriptions. The MCP server merges these with
built-in tools.

### Per-Provider CLI Plugins

**`bureau-github`** (`cmd/bureau-github/`):
- `bureau github setup` — configure GitHub App installation
- `bureau github bind <room> <repo>` — bind repo to room
- `bureau github subscribe/unsubscribe/subscriptions` — subscription mgmt
- `bureau github issue create|show|list|close`
- `bureau github pr create|show|list|merge|review`
- `bureau github actions list|trigger|status`

**`bureau-forgejo`** (`cmd/bureau-forgejo/`):
- `bureau forgejo setup` — configure Forgejo connection
- `bureau forgejo bind <room> <repo>`
- Same subscription and entity operations as GitHub

Plugins communicate with their connector via the CBOR socket and with
the proxy via `BUREAU_PROXY_SOCKET`.

---

## Provider-Specific Details

### GitHub

**Authentication**: GitHub App with installation tokens.
- Installation tokens generated from App private key + App ID +
  Installation ID.
- 1-hour TTL, auto-rotated by the connector.
- 5,000–12,500 requests/hour per installation (scales with org
  users/repos; 15,000 on Enterprise Cloud).
- HTTPS-only for git operations (GitHub Apps do not support SSH).

**Commit attribution**: all git operations go through the REST API (not
`git push`) so GitHub auto-signs commits with its web-flow GPG key. The
App bot is the commit author. Agent and operator identity are in
`Co-authored-by` trailers:
```
feat: implement error handling for retry logic

Co-authored-by: code-reviewer <code-reviewer@agents.bureau.foundation>
Co-authored-by: Ben Vanik <ben.vanik@gmail.com>
```

Setting a custom `author` field on an API-created commit causes GitHub
to skip signing entirely. All commits must use the default App bot
authorship to get the "Verified" badge. See Two Classes of Forge
Interaction for the full model including operator-driven agents that use
their own credentials.

**Webhook delivery**: installation-scoped. One webhook configuration per
App, not per-repo. The connector receives all events for all repos in
the installation and filters per-room. GitHub delivers webhooks to the
App for events caused by the App itself (no loop prevention) — this is
essential because the connector creates commits via the REST API and
needs to receive the resulting push events for subscription delivery.

**Rate limiting**: the connector tracks `X-RateLimit-*` response headers
and backs off when approaching limits. Conditional requests (ETags)
reduce quota consumption for polling operations.

### Forgejo

**Authentication**: admin API token for connector operations, per-agent
API tokens for individual access.

**Commit attribution**: native. Each agent has its own Forgejo user. Git
operations are attributed to the agent's user.

**Webhook delivery**: per-repo. The connector configures webhooks on each
bound repo.

**Identity lifecycle**: the connector creates/deletes Forgejo users as
Bureau principals activate/deactivate. The connector manages the full
user lifecycle.

**Permission sync**: Matrix room membership → Forgejo org/team
membership. Event-driven via /sync. Bureau is authoritative.

### GitLab

**Authentication**: project/group access tokens or OAuth application.
Similar to GitHub App model but with GitLab's token scoping.

**Webhook delivery**: per-project or per-group.

**CI integration**: GitLab CI pipelines map to CIStatusEvent. Triggering
via pipeline API.

---

## Relationship to Other Design Documents

- **credentials.md** — connector-managed credential lifecycle for forge
  admin tokens and per-agent tokens. Emergency revocation. Sysadmin
  provisioning flow.
- **tickets.md** — issues map to tickets via import action. PRs map to
  review tickets with gates. The connector is a ticket service client.
- **pipelines.md** — CI results published as `m.bureau.pipeline_result`.
  Bureau's pipeline visualization and gate resolution work uniformly
  with forge CI.
- **authorization.md** — `<provider>/*` action namespace. Cross-provider
  grants via `*/<action>`.
- **architecture.md** — connectors are service principals following the
  three-tier model: proxy for credentials, Matrix for persistence,
  socket for operations.
