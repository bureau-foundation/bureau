# Pipelines

[fundamentals.md](fundamentals.md) defines pipelines as one of Bureau's
higher-order constructs — structured automation sequences built from the
five primitives. This document describes the pipeline definition format,
the execution model, variable resolution, Matrix integration, and the
interaction between the executor, the daemon, and the launcher.

---

## What Pipelines Are

A pipeline is a named sequence of steps that runs inside a Bureau
sandbox. Steps execute sequentially: each must succeed before the next
begins. A pipeline has three possible outcomes:

- **Success** — all steps completed (or were skipped by guards).
- **Failure** — a non-optional step failed. Failure triggers
  `on_failure` steps (for cleanup or status publication), then the
  executor exits with a non-zero exit code.
- **Abort** — an `assert_state` step determined the pipeline's
  precondition is no longer valid. Abort is a clean exit (exit code 0,
  no `on_failure` steps). Nothing is broken; the work was simply no
  longer needed.

Pipelines are the automation primitive for workspace setup, service
lifecycle, maintenance, deployment, and any structured task that benefits
from observability, idempotency, and Matrix-native logging.

## Definition Format

Pipeline definitions are JSONC (JSON with `//` line comments, `/* block
comments */`, and trailing commas). On disk they have a `.jsonc`
extension. In Matrix they are stored as `m.bureau.pipeline` state events
in pipeline rooms, where the state key is the pipeline name.

A pipeline definition has four top-level sections:

```jsonc
{
  "description": "Human-readable summary of what this pipeline does",

  "variables": {
    "PROJECT": {
      "description": "Project name",
      "required": true
    },
    "MODE": {
      "description": "Removal mode",
      "default": "archive"
    }
  },

  "steps": [
    // See "Step Types" below.
  ],

  "on_failure": [
    // Steps to run when a non-optional step fails.
    // Same syntax as regular steps.
  ],

  "log": {
    "room": "${WORKSPACE_ROOM_ID}"
  }
}
```

### Variables

The `variables` map declares what inputs the pipeline expects. Each
entry has:

- **`description`** — human-readable explanation (shown by
  `bureau pipeline show`).
- **`default`** — fallback value when no source provides a value.
- **`required`** — the executor refuses to start if this variable has no
  value from any source (including the default).

Variable declarations are informational and serve validation. Actual
values come from elsewhere — see "Variable Resolution" below.

### Log

The optional `log` section configures Matrix thread logging. When
present, the executor creates a root message in the specified room at
pipeline start and posts step progress as thread replies. The `room`
field supports variable substitution.

Thread creation is a hard gate: if the executor cannot create the
thread, the pipeline does not start. Individual thread replies within a
running pipeline are best-effort — a failed reply is logged to stdout
but does not abort the pipeline.

When `log` is absent, the executor logs only to stdout (visible via
`bureau observe`).

### On-failure Steps

`on_failure` steps run after a non-optional step fails. They use the
same syntax as regular steps and have access to two additional
variables:

- **`FAILED_STEP`** — the name of the step that failed.
- **`FAILED_ERROR`** — the error message from the failed step.

On-failure steps are best-effort: if one fails, the error is logged and
execution continues with the remaining on-failure steps. This prevents
cascading failures from masking the original error.

On-failure steps do **not** run on abort (precondition mismatch). Abort
means nothing went wrong.

The primary use case for on-failure steps is publishing a failure status
via a `publish` step so that observers (other daemons, the ticket
service, operators) can detect the failure and the resource does not get
stuck in a transitional state:

```jsonc
"on_failure": [
  {
    "name": "publish-failed",
    "publish": {
      "event_type": "m.bureau.workspace",
      "room": "${WORKSPACE_ROOM_ID}",
      "content": {
        "status": "failed",
        "project": "${PROJECT}",
        "machine": "${MACHINE}"
      }
    }
  }
]
```

## Step Types

Each step must set exactly one action field: `run`, `publish`, or
`assert_state`. All steps share common fields:

- **`name`** (required) — human-readable identifier used in logs and
  status messages.
- **`when`** — guard condition command. Runs before the action; if it
  exits non-zero, the step is skipped (not failed). Valid on all step
  types.
- **`optional`** — if true, step failure does not abort the pipeline.
  The failure is logged but execution continues.
- **`timeout`** — maximum duration for the step (e.g., `"5m"`, `"30s"`,
  `"1h"`). Parsed by `time.ParseDuration`. Defaults to 5 minutes.
- **`env`** — additional environment variables for this step only.
  Step-level values take precedence over pipeline-level variables on
  conflict.

### Run

Execute a shell command via `sh -c`. The shell is resolved via `PATH`
(not hardcoded to `/bin/sh`) because inside bwrap sandboxes the Nix
environment's `bin/` directory is on `PATH` but `/bin` may not exist.

```jsonc
{
  "name": "clone-repository",
  "run": "git clone --bare ${REPOSITORY} /workspace/${PROJECT}/.bare",
  "check": "test -d /workspace/${PROJECT}/.bare/objects",
  "timeout": "10m"
}
```

Run-specific fields:

- **`check`** — post-step verification command. Runs after the main
  command succeeds; if it exits non-zero, the step is treated as failed.
  Catches cases where a command "succeeds" but does not produce the
  expected result.
- **`grace_period`** — duration between `SIGTERM` and `SIGKILL` when a
  step's timeout expires. When set, the executor sends `SIGTERM` to the
  process group first, waits up to this duration, then escalates to
  `SIGKILL`. When absent, `SIGKILL` is sent immediately. Use this for
  steps performing irreversible operations where abrupt termination
  could leave state inconsistent. Most sandbox steps should use the
  default (immediate kill) since sandbox processes are ephemeral.
- **`interactive`** — the step expects terminal interaction. The
  executor allocates a PTY and the operator interacts via
  `bureau observe` in read-write mode. Only valid with `run`.

Commands run in their own process group (`Setpgid`) so that timeout
signals reach the shell and all its children. Without process group
isolation, only the shell would receive the signal — child processes
would survive and hold open inherited file descriptors, blocking the
executor from exiting.

### Publish

Publish a Matrix state event via the proxy. The executor uses the
proxy's `/v1/matrix/state` endpoint — it never constructs Matrix API
URLs directly.

```jsonc
{
  "name": "publish-active",
  "publish": {
    "event_type": "m.bureau.workspace",
    "room": "${WORKSPACE_ROOM_ID}",
    "state_key": "${WORKTREE_PATH}",
    "content": {
      "status": "active",
      "project": "${PROJECT}",
      "machine": "${MACHINE}"
    }
  }
}
```

Publish fields:

- **`event_type`** (required) — the Matrix state event type.
- **`room`** (required) — target room alias or ID.
- **`state_key`** — the state key. Empty string is valid (for singleton
  events).
- **`content`** (required) — the event content as a JSON map. String
  values support variable substitution.

### Assert State

Read a Matrix state event and verify a field matches an expected value.
Used for precondition checks and advisory compare-and-swap patterns.

```jsonc
{
  "name": "assert-still-removing",
  "assert_state": {
    "room": "${WORKSPACE_ROOM_ID}",
    "event_type": "m.bureau.worktree",
    "state_key": "${WORKTREE_PATH}",
    "field": "status",
    "equals": "removing",
    "on_mismatch": "abort",
    "message": "worktree state is no longer 'removing', aborting deinit"
  }
}
```

Assert state fields:

- **`room`** (required) — room alias or ID.
- **`event_type`** (required) — the state event type to read.
- **`state_key`** — the state key.
- **`field`** (required) — top-level JSON field name to extract from the
  event content. The value is stringified for comparison.

Exactly one condition field must be set:

- **`equals`** — field value must equal this string.
- **`not_equals`** — field value must not equal this string.
- **`in`** — field value must be one of the listed strings.
- **`not_in`** — field value must not be any of the listed strings.

Mismatch behavior:

- **`on_mismatch`** — either `"fail"` (default) or `"abort"`.
  - `"fail"` means the step fails, the pipeline fails, and `on_failure`
    steps run. Use for error conditions.
  - `"abort"` means the pipeline exits cleanly with exit code 0 and
    `on_failure` steps do not run. Use for benign precondition
    mismatches (e.g., "someone else already handled this").
- **`message`** — human-readable explanation logged on mismatch.

## Variable Resolution

Variable substitution uses the `${NAME}` form exclusively — braces are
required. Bare `$NAME` is left for shell interpretation. Variable names
must match `[A-Za-z_][A-Za-z0-9_]*`.

### Resolution Priority

Variables resolve from four sources, in priority order (lowest to
highest):

1. **Declared defaults** — the `default` field in the pipeline's
   `variables` map.
2. **Trigger variables** — top-level fields from the state event that
   satisfied the principal's StartCondition, prefixed with `EVENT_`. For
   example, an `m.bureau.workspace` event with `"status": "teardown"`
   produces `EVENT_status=teardown`. Trigger variables provide ambient
   context from the launching event.
3. **Payload variables** — key-value pairs from the principal's
   `payload.json`. The keys `pipeline_ref` and `pipeline_inline` are
   reserved for pipeline resolution and excluded from the variable map.
   Payload is explicit per-principal configuration.
4. **Environment variables** — the process environment, but only for
   variables that are _declared_ in the pipeline's `variables` map.
   Undeclared environment variables are not included. This prevents the
   pipeline from accidentally depending on ambient environment state.

Value conversion for trigger and payload sources follows consistent
rules: strings pass through, numbers format without trailing zeros,
booleans become `"true"` / `"false"`, objects and arrays are
JSON-stringified, nulls are skipped.

### Expansion

Variable expansion is applied to all string fields in steps before
execution. Step-level `env` values are expanded first (against
pipeline-level variables only), then merged into the variable map for
expanding other step fields. This means a `run` command can reference
step-level env variables with `${NAME}`, and those values already have
their own `${REFERENCES}` resolved.

Expansion is strict: any `${NAME}` reference that cannot be resolved
produces an error, and the pipeline fails. Pipelines do not silently
produce broken commands from unresolvable references.

## Pipeline Resolution

The executor resolves its pipeline definition from three sources, tried
in priority order:

1. **CLI argument** — either a file path (absolute, relative with `./`
   or `../`, or ending in `.jsonc`/`.json`) or a pipeline ref (contains
   `:`). Pipeline refs use the format
   `<room-alias-localpart>:<pipeline-name>` (e.g.,
   `bureau/pipeline:dev-workspace-init`). Federated refs include a
   server: `room@server:name` or `room@server:port:name`.

2. **Payload `pipeline_ref`** — a string in the principal's
   `payload.json` resolved identically to a CLI ref argument.

3. **Payload `pipeline_inline`** — a complete `PipelineContent` JSON
   object embedded directly in the payload.

Pipeline ref resolution works by parsing the ref into a room alias and a
pipeline name, resolving the room alias to a room ID via the proxy's
`/v1/matrix/resolve` endpoint, then reading the `m.bureau.pipeline`
state event with the pipeline name as the state key.

When no source provides a pipeline definition, the executor fails with a
descriptive error. There is no embedded fallback — the executor requires
a proxy connection and refuses to run outside a sandbox
(`BUREAU_SANDBOX=1` must be set).

## Pipeline Storage

Pipelines are stored in two forms:

### Matrix State Events

`m.bureau.pipeline` state events in pipeline rooms. The state key is the
pipeline name. Pipeline rooms are content repositories with admin-only
write power levels — only the admin publishes pipeline definitions,
everyone else reads. The global pipeline room is
`#bureau/pipeline:<server>`. Projects use their own pipeline rooms
(e.g., `#iree/pipeline:<server>`).

Pipeline refs address a specific pipeline in a specific room:
`bureau/pipeline:dev-workspace-init` means state key
`dev-workspace-init` in room `#bureau/pipeline:<server>`.

### JSONC Files

On-disk pipeline definitions authored as `.jsonc` files. These are the
authoring format — human-readable with comments explaining each step.
JSONC extensions (comments, trailing commas) are stripped before JSON
parsing.

Bureau ships built-in pipeline definitions embedded in the binary.
These are the system pipelines for workspace
and worktree lifecycle:

- `dev-workspace-init.jsonc` — clone repository, run project init,
  publish active status.
- `dev-workspace-deinit.jsonc` — workspace teardown.
- `dev-worktree-init.jsonc` — create git worktree from bare clone.
- `dev-worktree-deinit.jsonc` — remove worktree with optional archive.

Projects extend automation by placing scripts at `.bureau/pipeline/`
within the repository. The system pipelines check for project-specific
hooks (e.g., `.bureau/pipeline/init`,
`.bureau/pipeline/worktree-deinit`) via `when` guards and run them when
present.

## Execution Model

### The Executor Binary

`bureau-pipeline-executor` runs inside a bwrap sandbox. It communicates
with Matrix exclusively through its proxy Unix socket at
`/run/bureau/proxy.sock` — it never constructs Matrix API URLs
directly. On startup it:

1. Verifies it is running inside a sandbox (`BUREAU_SANDBOX=1`).
2. Connects to the proxy and calls `whoami` to establish the server name
   (needed for alias construction in pipeline ref resolution).
3. Resolves the pipeline definition (see "Pipeline Resolution").
4. Validates the pipeline structure.
5. Loads trigger variables from `/run/bureau/trigger.json`.
6. Loads payload variables from `/run/bureau/payload.json`.
7. Resolves variables (merging declarations, trigger, payload,
   environment).
8. Creates the thread logger (if `log` is configured).
9. Opens the JSONL result log (if `BUREAU_RESULT_PATH` is set).
10. Executes steps sequentially.
11. Publishes a `m.bureau.pipeline_result` state event to the log room.

### Daemon Integration

The daemon is the orchestrator. When it receives a `pipeline.execute`
command (via Matrix), it:

1. Generates an ephemeral principal localpart
   (`pipeline/<sanitized-name>/<timestamp>`).
2. Creates a temporary file on the host for the JSONL result log.
3. Builds a sandbox spec with the executor binary as entrypoint, the
   result file bind-mounted read-write, the workspace root
   bind-mounted read-write, and the Nix environment for toolchain
   access.
4. Sends `create-sandbox` to the launcher via IPC, passing the daemon's
   own Matrix token as `DirectCredentials` for the proxy.
5. Waits for the sandbox to exit. For non-zero exits, the launcher
   returns captured terminal output from the executor's tmux pane.
6. Reads the JSONL result file for structured step-level outcomes.
7. Posts the result as a threaded Matrix reply to the original command.
   When the executor crashes (non-zero exit without a result file),
   the reply includes the captured terminal output so the operator
   can diagnose the failure.
8. Destroys the ephemeral sandbox.

The executor sandbox is fully isolated: PID namespace, new session,
die-with-parent, no-new-privs. The only communication channels are the
proxy socket (for Matrix operations) and the bind-mounted result file
(for daemon integration).

### Result Reporting

Pipeline execution produces results through two parallel channels:

**JSONL result log** (`BUREAU_RESULT_PATH`) — a line-per-event file that
the daemon reads after the executor exits. Each line is an independent
JSON object, making the log crash-safe (a `SIGKILL` mid-pipeline
preserves all completed step results) and streamable (the daemon can
tail the file for real-time progress). Entry types:

- `start` — pipeline name, step count, timestamp.
- `step` — step index, name, status, duration, error.
- `complete` — success, total duration, log thread event ID.
- `failed` — error message, failed step name, total duration.
- `aborted` — reason, aborted step name, total duration.

**`m.bureau.pipeline_result` state event** — published by the executor
to the pipeline's log room when execution finishes. This is the
Matrix-native public record that other services consume. The ticket
service watches these events to evaluate pipeline gates — a
`TicketGate` with type `"pipeline"` matches the `pipeline_ref` and
`conclusion` fields. Gate evaluation happens via `/sync`: when the
ticket service sees a pipeline result state event change, it re-checks
all pending pipeline gates in that room.

The state event includes:

- `pipeline_ref` — which pipeline was executed.
- `conclusion` — `"success"`, `"failure"`, or `"aborted"`.
- `started_at`, `completed_at`, `duration_ms` — timing.
- `step_count` — total steps in the pipeline.
- `step_results` — per-step name, status, duration, error.
- `failed_step`, `error_message` — failure details (when applicable).
- `log_event_id` — link to the thread logger's root message.

State event publication is best-effort: if the log room is not
configured or the `putState` call fails, a warning is printed but the
executor's own exit status is not affected.

### Thread Logging

When `log` is configured, the executor creates a thread in the specified
Matrix room anchored by a root message ("Pipeline X started (N steps)").
Step progress is posted as thread replies using Matrix's `m.thread`
relation type:

```
Pipeline dev-workspace-init started (4 steps)
├─ step 1/4: create-project-directory... ok (0.1s)
├─ step 2/4: clone-repository... ok (12.3s)
├─ step 3/4: run-project-init... skipped (guard condition not met)
├─ step 4/4: publish-active... ok (0.2s)
└─ Pipeline dev-workspace-init: complete (12.6s)
```

Thread logging provides human-readable observability in the same Matrix
room where the resource's state events live. An operator reviewing a
workspace room sees both the state transitions (status: creating →
active) and the detailed execution log that produced them.

## Relationship to Other Design Documents

- [fundamentals.md](fundamentals.md) defines pipelines as a higher-order
  construct and explains how they compose sandbox, proxy, and
  observation primitives.
- [architecture.md](architecture.md) describes the launcher/daemon/proxy
  process topology that pipelines execute within.
- [information-architecture.md](information-architecture.md) defines
  pipeline rooms and the `m.bureau.pipeline` / `m.bureau.pipeline_result`
  event types.
- [workspace.md](workspace.md) is the primary consumer: workspace and
  worktree lifecycle is implemented as system pipelines.
- [tickets.md](tickets.md) defines pipeline gates — the mechanism by
  which the ticket service watches `m.bureau.pipeline_result` events to
  coordinate work items.
- [observation.md](observation.md) describes the live terminal access
  that operators use to watch and interact with running pipeline steps.
