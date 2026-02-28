# Logging

[telemetry.md](telemetry.md) describes Bureau's structured telemetry system:
traces, metrics, and structured log records emitted by Bureau processes
via the OpenTelemetry SDK or native CBOR. [observation.md](observation.md)
describes live bidirectional terminal access to running principals. This
document describes a third signal type: raw output capture from sandboxed
processes, stored as immutable artifacts, queryable and tailable through
the telemetry service.

---

## Three Signal Types

Bureau has three distinct visibility mechanisms, each serving a different
need:

**Structured telemetry** (telemetry.md) — traces, metrics, and structured
log records. Emitted explicitly by Bureau Go processes via `slog` or the
OTel SDK. The process decides what to log, at what severity, with what
attributes. These are the daemon's, proxy's, and service's own
perspective on what happened. Stored in SQLite with full query support.

**Raw output capture** (this document) — stdout/stderr byte streams from
sandboxed processes. The process doesn't know or care that it's being
captured. Pipeline build logs, agent stderr, service stdout. Unstructured
bytes with timestamps and source identity. Stored as CAS artifacts with
mutable `log-*` artifact tags as the metadata index.

**Live terminal observation** (observation.md) — bidirectional, interactive
terminal access to a running tmux session. For inhabiting a session:
watching an agent work, typing in its shell, scrolling its TUI. Not
recorded, not stored — ephemeral by design.

These overlap in some areas (you can observe a pipeline executor live
while its output is also being captured) but serve fundamentally different
use cases. Structured telemetry answers "what did the system do and why."
Output capture answers "what did the process print." Live observation
answers "what is happening right now, and can I interact with it."

---

## Architecture

Raw output capture rides on the existing telemetry pipeline. The
per-machine telemetry relay already accepts CBOR messages from local
processes and ships them to the fleet-wide telemetry service. Output
capture adds a new CBOR message type (output delta) to this pipeline
rather than building a parallel ingestion path.

```
Machine                                      Fleet-wide
+-------------------------------------------+   +-----------------------------+
|                                           |   |                             |
|  sandbox process                          |   |  bureau-telemetry-service   |
|       |                                   |   |  +- stores output deltas    |
|       | stdout/stderr (PTY)               |   |  +- manages log metadata    |
|       v                                   |   |  +- artifact write-through  |
|  bureau-log-relay (PTY interposition)     |   |  +- serves tail/query       |
|       |               |                   |   |  +- rotation/eviction       |
|       | stdout         | CBOR output      |   |                             |
|       | (to tmux)      | delta msgs       |   +-----------------------------+
|       v                v                  |          ^
|  tmux session    telemetry relay  --------+----------+
|  (observation)   (existing CBOR path)     |   service socket
|                                           |
+-------------------------------------------+
```

### bureau-log-relay

A new binary that interposes on the PTY between a sandboxed process and
tmux. The launcher inserts it into the tmux session command when the
principal's template opts in to output capture.

Without capture:
```
tmux new-session -d -s bureau/agent/foo "/path/to/sandbox-script.sh"
```

With capture:
```
tmux new-session -d -s bureau/agent/foo \
  "bureau-log-relay \
   --relay=/run/bureau/telemetry.sock \
   --token=/run/bureau/telemetry-token \
   --fleet='#bureau/fleet/prod:bureau.local' \
   --machine='@bureau/fleet/prod/machine/workstation:bureau.local' \
   --source='@bureau/fleet/prod/agent/foo:bureau.local' \
   --session-id=sess-abc123 \
   -- /path/to/sandbox-script.sh"
```

The log relay:

- Allocates a PTY pair.
- Forks the sandbox command on the PTY slave side.
- Reads the PTY master (raw process output).
- Writes every byte to its own stdout (which tmux receives, since tmux
  allocated the outer PTY for the session command).
- Simultaneously buffers output and periodically flushes CBOR output
  delta messages to the telemetry relay socket.
- Forwards stdin from tmux to the PTY slave (keyboard input, resize
  signals pass through unmodified).
- Exits when the child process exits, with the child's exit code.

The log relay runs outside the sandbox (like the observe relay), so it
cannot be tampered with by the sandboxed process. It holds no secrets,
no Matrix session, no credentials. It's a PTY bridge with a side channel.

#### Buffering and flush strategy

The log relay accumulates output bytes in a memory buffer and flushes on
two conditions:

- **Size threshold**: flush when the buffer reaches 64 KB. This keeps
  individual CBOR messages and CAS artifacts at a reasonable granularity
  for streaming and storage.
- **Time threshold**: flush every 1 second if the buffer is non-empty.
  This ensures low-latency tailing even for processes that produce
  output slowly.

Each flush produces a CBOR SubmitRequest sent to the telemetry relay
socket. The request contains fleet, machine, and source identity at
the envelope level (deduped across records) and an OutputDelta with a
monotonically increasing sequence number, a stream identifier (combined
for PTY output), a session ID, a nanosecond timestamp, and the raw
bytes.

#### PTY semantics

The log relay's PTY interposition preserves normal terminal behavior:

- **Window resize**: tmux sends SIGWINCH to the log relay (its direct
  child). The log relay applies TIOCSWINSZ to the inner PTY master,
  which propagates SIGWINCH to the sandbox process. Resize flows
  through transparently.
- **Signals**: SIGINT, SIGTERM, etc. from tmux reach the log relay,
  which forwards them to the sandbox process.
- **Job control**: the sandbox process can use job control normally;
  the PTY layer handles SIGTSTP/SIGCONT.
- **Raw mode**: TUI applications that set the terminal to raw mode do
  so on the inner PTY. The log relay captures the raw bytes they write
  (including escape sequences). For TUI-heavy processes, output capture
  can be disabled via template configuration to avoid storing
  unstructured terminal escape sequences.

### Telemetry relay changes

The per-machine telemetry relay (bureau-telemetry-relay) already accepts
CBOR messages and batches them for the fleet-wide telemetry service.
Output delta messages are a new CBOR message type in the existing
pipeline. The relay treats them like any other signal: buffer, batch,
ship. No special handling needed.

### Telemetry service changes

The fleet-wide telemetry service gains three responsibilities:

- **Output delta ingestion**: receives batched output deltas from relays.
  Stores the raw bytes as CAS artifacts via the artifact service. Creates
  or updates the corresponding log metadata artifact via a mutable tag
  (`log/<source-localpart>/<session-id>`).
- **Tail/query API**: new CBOR socket actions for streaming live output
  and querying historical output. Subscribers receive new chunks as they
  arrive (push via the event subscription mechanism). Historical queries
  return chunk references with byte ranges for random access.
- **Rotation and eviction**: configurable per-log retention policy (time
  or size). The eviction loop drops old chunks from the `log-*` entity
  and deletes the corresponding CAS artifacts.

---

## Data Model

### Output Delta (wire format)

The CBOR message sent from bureau-log-relay to the telemetry relay. Part
of the telemetry batch alongside spans, metrics, and structured log
records.

- **Source** — `ref.Entity`. The principal producing the output.
- **SessionID** — `string`. Identifies the specific process invocation
  (matches the agent session ID or pipeline execution ID). Distinguishes
  output from successive runs of the same principal.
- **Sequence** — `uint64`. Monotonically increasing per session. Enables
  ordering reconstruction and gap detection.
- **Stream** — `uint8`. 0 = combined stdout/stderr, 1 = stdout only,
  2 = stderr only. When PTY interposition captures a single merged
  stream (the common case), this is 0.
- **Timestamp** — `int64`. Unix nanoseconds at flush time.
- **Data** — `[]byte`. The raw output bytes.

### Log Metadata (artifact tag)

A CBOR-encoded `LogContent` struct stored as a CAS artifact with a
mutable tag. The tag name follows the pattern
`log/<source-localpart>/<session-id>`, enabling hierarchical listing
by source principal. The telemetry service creates the metadata
artifact when the first output delta arrives for a session and updates
it (via tag overwrite) as chunks are stored.

- **SessionID** — `string`. The process invocation this log belongs to.
- **Source** — `ref.Entity`. The principal.
- **Format** — `string`. Always `"raw"` for now. Future formats could
  include `"structured"` for pre-parsed log lines.
- **Status** — `string`. `"active"` while the process is running,
  `"complete"` after it exits, `"rotating"` for long-lived services
  with active eviction.
- **TotalBytes** — `int64`. Sum of all chunk sizes. Updated with each
  new chunk.
- **Chunks** — flat list of chunk references:
  - **Ref** — `string`. CAS artifact reference.
  - **Sequence** — `uint64`. First sequence number in this chunk.
  - **Size** — `int64`. Chunk size in bytes.
  - **Timestamp** — `int64`. Timestamp of the first delta in this chunk.

The chunk list is append-only during active capture. The telemetry
service's eviction loop removes entries from the front when rotation
is needed. At 1 MB per chunk, a 1 GB build log produces ~1000 entries
in the metadata artifact. For multi-GB logs from long-lived services,
the telemetry service consolidates old chunk entries into larger
combined artifacts to keep the index manageable.

Using artifact tags instead of Matrix state events avoids coupling
log metadata persistence to Matrix room resolution (which requires
the telemetry service to know which config room belongs to which
machine). Tags are addressed by name, with last-writer-wins
semantics — a natural fit for metadata that is only written by
one service and updated in place.

### Why a flat list, not a chain

Context commits (ctx-\*) use a parent-chain because context has branching
semantics: compaction creates new roots, resumed sessions fork from
midpoints, materialization walks parent pointers to assemble the right
prefix. Logs have none of that. They are append-only, time-ordered, and
the only operations are "append chunk," "tail from offset," and "evict
old chunks." A flat list gives O(1) append and O(1) tail-from-end with
no chain-walking overhead.

---

## Template Configuration

Output capture is opt-in via the principal's template. Not every
principal needs capture — interactive human shells don't, TUI-heavy
monitoring tools don't. Pipeline executors and agents generally do.

```json
{
  "command": ["/path/to/agent"],
  "output_capture": {
    "enabled": true,
    "retention": "7d",
    "max_size": "1GB"
  }
}
```

When `output_capture.enabled` is true, the launcher inserts
`bureau-log-relay` into the tmux session command. When false or absent,
the process runs directly in tmux with no capture overhead.

`retention` and `max_size` are hints to the telemetry service's eviction
policy. The telemetry service may enforce fleet-wide limits that override
per-principal settings.

---

## Pipeline Integration

Pipelines execute as tickets. Each pipeline step runs in a sandbox.
Output capture integrates with the ticket system:

**Pre-creation.** When the pipeline executor schedules a step, the
telemetry service creates the `log-*` metadata artifact with status
`"active"` when the first output delta arrives. `bureau log tail
pip-1234` can attach immediately and will see output as soon as the
process starts writing.

**Ticket attachment.** The pipeline executor attaches the `log-*`
identifier to the pipeline step's ticket as a note or structured
reference. The ticket viewer can display or link to the log directly.
Clicking a log reference in the ticket TUI opens the log viewer.

**Telemetry correlation.** The pipeline executor sets the pipeline ticket
ID as an attribute on all telemetry events (structured logs, spans,
metrics) emitted during the step. A query for "everything related to
pip-1234" returns both the structured telemetry and the raw output log
— different signal types, same correlation key.

**Completion.** When the sandbox exits, the log relay exits, the last
output delta flushes, and the telemetry service updates the `log-*`
status to `"complete"` with final byte counts. The ticket's log
reference now points to a complete, immutable log.

---

## Service Instance Logs

Long-lived services (Forgejo, Matrix homeserver, custom services) use
the same mechanism with rotation enabled. The `log-*` entity for a
service instance has status `"rotating"` — the telemetry service
actively evicts old chunks based on the retention policy while new
chunks continue to arrive.

The retention policy is per-principal, configured via the template's
`output_capture` section. A service configured with `retention: "3d"`
and `max_size: "500MB"` keeps a rolling window of the most recent 3
days or 500 MB of output, whichever is smaller.

Rotation is chunk-granular. The telemetry service drops the oldest
chunk, removes it from the `log-*` chunk list, and deletes the CAS
artifact. The `log-*` entity always reflects the current window.

---

## CLI Surface

| Command | Description |
|---|---|
| `bureau log tail <principal-or-ticket>` | Stream live output. For a principal, tails the active log. For a ticket ID, finds the attached log and tails it. Attaches to the log-\* subscription and prints chunks as they arrive. Works before the process starts (waits for first output). |
| `bureau log show <log-id>` | Display the full log content, paged. Fetches all chunks from the artifact service and concatenates. |
| `bureau log list [--principal <p>] [--ticket <t>]` | List log entities, filtered by principal or ticket. Shows status, size, chunk count, timestamps. |
| `bureau log export <log-id> [--output <path>]` | Export the full log to a file. Fetches all chunks and writes them concatenated. |

`bureau log tail` is the primary operator interface. When given a ticket
ID like `pip-1234`, it resolves the ticket, finds the attached log-\*
reference, and subscribes. If the pipeline step hasn't started yet, it
prints a status line and waits. When output arrives, it streams to the
terminal. Ctrl-C detaches.

For the future unified TUI: clicking a log-\* reference in the ticket
viewer opens an embedded log viewer pane. The viewer supports search,
scroll, and live tail mode.

---

## Integration Test Support

### Telemetry mock

Integration tests deploy a `bureau-telemetry-mock` service that
implements the telemetry relay's CBOR socket protocol. It accepts all
telemetry signals (spans, metrics, structured logs, output deltas) and
stores them in memory. Tests query it via the same CBOR socket actions
the real telemetry service exposes.

This gives tests programmatic access to every structured log line
emitted by every Bureau process in the test. When a checkpoint fails,
the agent service's error log about why it failed is queryable:

```go
logs := queryTelemetryMock(t, mock, telemetryQuery{
    Source:      agentServiceEntity,
    MinSeverity: severityError,
})
for _, log := range logs {
    t.Logf("agent service: %s", log.Body)
}
```

### Verbose test output

When running with `-test.v`, the telemetry mock forwards all received
log records to `t.Log()`, interleaved with Go test output in
chronological order. This provides the unified log view that integration
test debugging requires — daemon logs, agent service logs, launcher
logs, and agent stderr all visible in one stream without needing to
attach to tmux sessions or read separate log files.

### Output capture in tests

Tests that exercise pipeline execution or agent sessions can query the
telemetry mock for output deltas and verify the captured content:

```go
deltas := queryOutputDeltas(t, mock, principal)
combined := concatenateDeltas(deltas)
if !strings.Contains(combined, "expected output") {
    t.Errorf("output capture missing expected content")
}
```

---

## Security

### Access control

Raw output may contain PII, credentials in error messages, or other
sensitive data. Access follows the existing telemetry grant model:

- **Querying logs** requires a `telemetry/logs` grant (same as querying
  structured log records). The telemetry service enforces this on all
  log-related actions.
- **Tailing live output** requires a `telemetry/logs` grant for the
  target principal. The subscription mechanism verifies grants before
  streaming.
- **CAS artifact access** is scoped by the artifact service's own
  permission model. Log chunk artifacts are stored with labels linking
  them to the source principal. The artifact service checks that the
  requester has appropriate access before serving content.

Output capture data never appears in Matrix messages. The log metadata
artifact contains only chunk references, sizes, and timestamps — the
actual output bytes are in CAS artifacts behind the artifact service's
access control.

### Disabling capture

Templates can set `output_capture.enabled: false` to prevent output
capture entirely. This is appropriate for:

- Principals that handle sensitive data where even stderr might leak
  secrets.
- TUI applications where captured output is escape-sequence noise with
  no diagnostic value.
- Interactive human sessions where capture would be a privacy concern.

When capture is disabled, no `bureau-log-relay` process is started, no
output deltas are sent, and no `log-*` entity is created. The principal
runs directly in tmux with the same behavior as today.

---

## Relationship to Other Design Documents

- [telemetry.md](telemetry.md) — output capture is a new signal type in
  the telemetry pipeline. The relay and service gain output delta handling
  alongside the existing span, metric, and structured log record types.
  The ingestion, batching, and transport mechanisms are shared.
- [observation.md](observation.md) — output capture and live observation
  are complementary. Observation gives interactive terminal access;
  capture gives persistent, queryable logs. Both can be active
  simultaneously for the same principal. The `bureau-log-relay` sits
  between the process and tmux; the observe relay attaches to tmux. They
  don't interact.
- [tickets.md](tickets.md) — pipeline step logs are attached to tickets
  as `log-*` references. The ticket viewer can display or link to logs.
  Telemetry correlation via ticket ID enables cross-signal queries.
- [artifacts.md](artifacts.md) — log chunks are stored as CAS artifacts.
  Log metadata is also stored as CAS artifacts with mutable tags. The
  telemetry service acts as the write path (agents and services never
  write log artifacts directly). The artifact service provides storage,
  deduplication, and access control.
- [pipelines.md](pipelines.md) — pipeline executors create `log-*`
  entities for each step and attach them to the step's ticket. Output
  capture is the pipeline equivalent of a CI build log.
- [authorization.md](authorization.md) — log access uses the
  `telemetry/logs` grant. Output capture data is never exposed through
  channels that bypass grant verification.
