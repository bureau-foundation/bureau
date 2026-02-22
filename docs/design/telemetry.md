# Telemetry

[fundamentals.md](fundamentals.md) defines Bureau's five primitives;
[architecture.md](architecture.md) describes the runtime topology;
[fleet.md](fleet.md) covers multi-machine management. This document
describes Bureau's telemetry system: how traces, metrics, and logs are
collected from distributed Bureau processes, aggregated fleet-wide, and
made available to operators, sysadmin agents, and external monitoring
tools.

---

## Why Telemetry

Bureau runs dozens of interacting processes across multiple machines:
daemons, proxies, service principals, pipeline executors, and sandboxed
agents. When a pipeline step takes 30 seconds, when a credential
injection fails intermittently, when memory pressure builds before an
OOM — operators and sysadmin agents need visibility into what happened
and why.

Machine heartbeats (`m.bureau.machine_status`) provide coarse health
signals: CPU percent, memory usage, sandbox counts. These are adequate
for placement decisions and liveness detection but insufficient for
diagnosing latency, tracing request flow across services, or
understanding error patterns over time.

Telemetry fills this gap with three signal types:

- **Traces** — distributed request flow across services. A single agent
  action (e.g., "create workspace") generates spans in the proxy, daemon,
  launcher, and pipeline executor. Tracing connects them into a causal
  chain.
- **Metrics** — quantitative measurements over time. Request latency
  histograms, error rate counters, queue depths, connection counts. The
  statistical view of system health.
- **Logs** — structured event records with trace correlation. A log line
  from the proxy's credential injection path links to the span that
  triggered it, giving the narrative context that metrics lack.

## Design Principles

**Ephemeral machines, persistent data.** Bureau machines are cloud
instances that power down without notice. Telemetry data must survive
machine loss. Per-machine processes collect and relay telemetry; a
fleet-wide service aggregates and persists it.

**Native types at the core.** Bureau's `ref.*` types (Fleet, Machine,
Service, Agent, Entity) are the identity system. Telemetry data carries
these as first-class typed fields, not opaque string attributes.
Queries filter by `machine`, `service`, or `fleet` using the same ref
values used everywhere else in Bureau.

**Standard ingestion, native internals.** Sandboxed agents may be
written in any language. The ingestion boundary accepts
[OTLP/HTTP](https://opentelemetry.io/docs/specs/otlp/#otlphttp) —
the standard protocol every OpenTelemetry SDK speaks. Inside Bureau,
everything is CBOR: the native wire format for all service-to-service
communication.

**Separation of collection and aggregation.** The per-machine relay is
thin and stable (protocol bridge, in-memory buffer). The fleet-wide
aggregation service handles storage, queries, retention, and external
integrations. They iterate at different rates: the relay rarely changes;
the aggregation service evolves as query patterns and storage needs
emerge.

**Telemetry informs operations.** Sysadmin agents query the telemetry
service to diagnose issues, monitor trends, and make recommendations.
The fleet controller queries it for placement-relevant metrics (P99
latency, error rates). The Prometheus endpoint exposes fleet-wide
metrics to standard monitoring stacks.

## Architecture Overview

Three components, two binaries:

```
Machine (ephemeral)                         Fleet-wide (persistent)
┌──────────────────────────────┐           ┌───────────────────────────────┐
│                              │           │                               │
│  agents  ──OTLP/HTTP──►┐     │           │  bureau-telemetry-service     │
│  daemon  ──OTLP/HTTP──►│     │  service  │  ├─ ingest action (CBOR)      │
│  proxy   ──OTLP/HTTP──►│     │  socket   │  ├─ SQLite store              │
│  services──OTLP/HTTP──►│     │──────────►│  ├─ CBOR query socket         │
│              OR        │     │  (daemon  │  ├─ HTTP /metrics (Prometheus)│
│  Go svcs ──CBOR───────►│     │  routes   │  └─ retention/rotation        │
│                        │     │  cross-   │                               │
│     bureau-telemetry-relay   │  machine) │  fleet controller             │
│      ├─ OTLP/HTTP listener   │           │  ├─ queries telemetry svc     │
│      ├─ CBOR listener        │           │  │  for placement decisions   │
│      └─ in-memory buffer     │           │  └─ (no telemetry storage)    │
└──────────────────────────────┘           └───────────────────────────────┘
```

### bureau-telemetry-relay (per-machine)

A protocol bridge. Accepts telemetry from local processes via two
ingestion paths:

- **OTLP/HTTP** on a Unix socket — for sandboxed agents using any
  language's OpenTelemetry SDK, and for Bureau Go services using the
  standard OTel Go SDK exporter.
- **CBOR** on a Unix socket — for Bureau Go services that emit
  telemetry directly in Bureau-native types, avoiding the
  protobuf encode/decode round trip.

Both paths feed into a single outbound queue. The relay batches CBOR
messages and ships them to the telemetry service via the standard CBOR
service socket protocol (the daemon handles cross-machine routing
through the transport layer). When the telemetry service is temporarily
unreachable, the
relay buffers in memory with a bounded ring buffer (configurable max
size, default 64 MB). Old data drops when the buffer fills.

The relay stores nothing to disk. It is a stateless forwarder. Its
socket is bind-mounted into sandboxes at `/run/bureau/service/telemetry.sock`
via the standard service socket mechanism (templates declare
`telemetry` as a required service).

The relay registers in the fleet service directory like any other
service. The daemon resolves it for sandbox socket binding.

### bureau-telemetry-service (fleet-wide)

The aggregation and query service. Placed by the fleet controller on
persistent infrastructure. Receives CBOR telemetry batches from all
per-machine relays, stores in SQLite, serves queries.

Responsibilities:

- **Ingestion**: Accepts CBOR batches via its service socket (`ingest`
  action). Verifies service tokens (same auth as every other Bureau
  service). Writes to SQLite.
- **Storage**: Time-partitioned SQLite tables (one set per day). WAL
  mode for concurrent reads during writes. Configurable retention per
  signal type.
- **Queries**: CBOR socket API for Bureau clients (CLI, agents, fleet
  controller). Supports filtering by machine, service, time range,
  operation, trace ID, severity.
- **Prometheus**: HTTP `/metrics` endpoint exposing fleet-wide metrics
  in Prometheus exposition format. A single scrape target returns data
  for all machines, differentiated by labels.

The telemetry service uses `lib/service.Bootstrap()` with
audience `"telemetry"`, registers in the fleet service directory, and
follows the standard service lifecycle (Matrix sync for configuration
events, CBOR socket for queries, graceful shutdown with deregistration).

### Fleet controller integration

The fleet controller does not store or process telemetry directly. When
making placement decisions, it optionally queries the telemetry service
for operational metrics (P99 latency, error rates, throughput) via a
standard CBOR service socket call. If the telemetry service is
unavailable, the fleet controller falls back to the coarse heartbeat
data it already receives via Matrix.

This separation means telemetry schema changes and service upgrades
never require cycling the fleet controller.

---

## Data Model

Bureau defines its own telemetry types rather than mirroring the OTLP
protobuf structures. OTLP is designed for generality — arbitrary string
attributes, deeply nested resource/scope/span hierarchies. Bureau knows
exactly what its identity model looks like. Fleet, machine, service, and
agent are first-class typed fields with proper `ref.*` values, not
string attributes that require parsing at query time.

The relay performs the OTLP → Bureau-native conversion once at the
ingestion boundary. The telemetry service stores and queries native
types. The SQLite schema has proper indexed columns for identity fields.

### Common Fields

Every telemetry record carries:

- **Fleet** — `ref.Fleet` identifying the fleet. CBOR-serialized as
  `#localpart:server`.
- **Machine** — `ref.Machine` identifying the originating machine.
  CBOR-serialized as `@localpart:server`.
- **Source** — `ref.Entity` identifying the emitting process (a service,
  agent, or machine-level daemon). CBOR-serialized as `@localpart:server`.
- **Timestamp** — `int64`, Unix nanoseconds. CBOR handles large
  integers natively (unlike Matrix canonical JSON, which forbids
  fractional numbers).

### Span

A span represents a unit of work within a trace.

- **TraceID** — `[16]byte`. Globally unique trace identifier. Correlates
  spans across services and machines.
- **SpanID** — `[8]byte`. Unique within a trace.
- **ParentSpanID** — `[8]byte`. Zero value for root spans.
- **Operation** — `string`. The operation name, scoped by convention:
  `"socket.handle"`, `"sync.loop"`, `"proxy.forward"`,
  `"pipeline.step.shell"`.
- **StartTime** — `int64`, Unix nanoseconds.
- **Duration** — `int64`, nanoseconds.
- **Status** — `uint8`. 0 = unset, 1 = ok, 2 = error.
- **StatusMessage** — `string`. Present only when Status is error.
- **Attributes** — `map[string]any`. Bounded key-value pairs for
  operation-specific context (e.g., `"action": "list"`,
  `"room_id": "!abc:server"`, `"http.method": "POST"`). Stored as JSON
  in SQLite for queryability via `json_extract()`.
- **Events** — `[]SpanEvent`. Timestamped annotations within the span
  (e.g., "retry attempt 2", "cache miss"). Each event has a name,
  timestamp, and optional attributes.

### Metric Point

A single metric observation.

- **Name** — `string`. Follows Prometheus naming conventions:
  `bureau_proxy_request_duration_seconds`,
  `bureau_socket_request_total`,
  `bureau_sandbox_count`.
- **Labels** — `map[string]string`. Typed label pairs. Standard labels
  include `service`, `action`, `status_code`. Bureau ref fields
  (fleet, machine, source) are not in labels — they're the common
  fields above.
- **Kind** — `uint8`. Distinguishes the value interpretation:
  - 0 = gauge (instantaneous value, e.g., queue depth)
  - 1 = counter (monotonically increasing, e.g., request count)
  - 2 = histogram (distribution summary)
- **Value** — For gauge and counter: `float64`. For histogram: a
  `HistogramValue` containing bucket boundaries, bucket counts,
  sum, and count.

### HistogramValue

Distribution summary for histogram metrics.

- **Boundaries** — `[]float64`. Upper bounds of each bucket, in
  ascending order. The final implicit +Inf bucket is not stored.
- **BucketCounts** — `[]uint64`. Count per bucket. Length is
  `len(Boundaries) + 1` (the +Inf bucket is explicit in counts).
- **Sum** — `float64`. Sum of all observed values.
- **Count** — `uint64`. Total number of observations.

### Log Record

A structured log entry with trace correlation.

- **Severity** — `uint8`. Follows OpenTelemetry severity numbering:
  1–4 = TRACE, 5–8 = DEBUG, 9–12 = INFO, 13–16 = WARN, 17–20 = ERROR,
  21–24 = FATAL.
- **Body** — `string`. The log message.
- **TraceID** — `[16]byte`. Zero when not correlated with a trace.
- **SpanID** — `[8]byte`. Zero when not correlated with a span.
- **Attributes** — `map[string]any`. Structured fields from the log
  context (e.g., `"error": "connection refused"`,
  `"sandbox_id": "abc123"`).

### Telemetry Batch

The wire format for relay → telemetry service communication. A single
CBOR message containing mixed signal types from one machine.

- **Machine** — `ref.Machine`. The originating machine.
- **Fleet** — `ref.Fleet`. The fleet.
- **Spans** — `[]Span`. Zero or more spans.
- **Metrics** — `[]MetricPoint`. Zero or more metric observations.
- **Logs** — `[]LogRecord`. Zero or more log records.
- **SequenceNumber** — `uint64`. Monotonically increasing per relay
  instance. The telemetry service uses this to detect gaps (indicating
  dropped data during buffer overflow) and duplicates (during retry
  after timeout).

The batch is the unit of ingestion. The relay accumulates telemetry
from local processes and flushes batches on a timer (default 5 seconds)
or when the batch reaches a size threshold (default 256 KB of CBOR).

---

## OTLP-to-Bureau Conversion

The relay's OTLP/HTTP listener accepts standard
`ExportTraceServiceRequest`, `ExportMetricsServiceRequest`, and
`ExportLogsServiceRequest` protobuf messages on the standard OTLP paths
(`/v1/traces`, `/v1/metrics`, `/v1/logs`).

The conversion extracts Bureau identity from OTLP resource attributes:

- `bureau.fleet` → `ref.Fleet` (parsed from fleet alias)
- `bureau.machine` → `ref.Machine` (parsed from Matrix user ID)
- `bureau.source` → `ref.Entity` (parsed from Matrix user ID)

When these attributes are absent (e.g., a third-party agent using a
vanilla OTel SDK without Bureau-specific configuration), the relay falls
back to the machine identity it was configured with at startup and sets
the source to a generic entity derived from the OTLP `service.name`
resource attribute.

OTLP scope and instrumentation library metadata are flattened into span
attributes (prefixed with `otel.scope.*`). OTLP resource attributes not
consumed by the identity extraction are preserved in span/metric/log
attributes.

---

## Storage

The telemetry service stores data in SQLite with WAL mode enabled for
concurrent read/write access.

### Schema

Tables are time-partitioned by day. The telemetry service creates new
partition tables automatically and drops expired partitions based on
retention configuration.

```sql
CREATE TABLE spans_YYYYMMDD (
    trace_id     BLOB NOT NULL,        -- 16 bytes
    span_id      BLOB NOT NULL,        -- 8 bytes
    parent_span_id BLOB,               -- 8 bytes, NULL for root
    fleet        TEXT NOT NULL,        -- fleet localpart
    machine      TEXT NOT NULL,        -- machine localpart
    source       TEXT NOT NULL,        -- entity localpart
    operation    TEXT NOT NULL,
    start_time   INTEGER NOT NULL,     -- Unix nanoseconds
    duration     INTEGER NOT NULL,     -- nanoseconds
    status       INTEGER NOT NULL,     -- 0=unset, 1=ok, 2=error
    status_message TEXT,
    attributes   TEXT,                 -- JSON for json_extract() queries
    events       TEXT                  -- JSON array of span events
);

CREATE INDEX idx_spans_time ON spans_YYYYMMDD(start_time);
CREATE INDEX idx_spans_trace ON spans_YYYYMMDD(trace_id);
CREATE INDEX idx_spans_machine ON spans_YYYYMMDD(machine, start_time);
CREATE INDEX idx_spans_source ON spans_YYYYMMDD(source, start_time);
CREATE INDEX idx_spans_operation ON spans_YYYYMMDD(operation, start_time);
```

```sql
CREATE TABLE metrics_YYYYMMDD (
    fleet        TEXT NOT NULL,
    machine      TEXT NOT NULL,
    source       TEXT NOT NULL,
    name         TEXT NOT NULL,        -- metric name
    labels       TEXT,                 -- JSON for label filtering
    kind         INTEGER NOT NULL,     -- 0=gauge, 1=counter, 2=histogram
    timestamp    INTEGER NOT NULL,     -- Unix nanoseconds
    value_float  REAL,                 -- gauge/counter value
    value_hist   BLOB                  -- CBOR-encoded HistogramValue
);

CREATE INDEX idx_metrics_time ON metrics_YYYYMMDD(timestamp);
CREATE INDEX idx_metrics_name ON metrics_YYYYMMDD(name, timestamp);
CREATE INDEX idx_metrics_source ON metrics_YYYYMMDD(source, name, timestamp);
```

```sql
CREATE TABLE logs_YYYYMMDD (
    fleet        TEXT NOT NULL,
    machine      TEXT NOT NULL,
    source       TEXT NOT NULL,
    severity     INTEGER NOT NULL,     -- OTel severity (1-24)
    body         TEXT NOT NULL,
    trace_id     BLOB,                 -- 16 bytes, NULL if uncorrelated
    span_id      BLOB,                 -- 8 bytes, NULL if uncorrelated
    timestamp    INTEGER NOT NULL,     -- Unix nanoseconds
    attributes   TEXT                  -- JSON
);

CREATE INDEX idx_logs_time ON logs_YYYYMMDD(timestamp);
CREATE INDEX idx_logs_source ON logs_YYYYMMDD(source, timestamp);
CREATE INDEX idx_logs_trace ON logs_YYYYMMDD(trace_id);
CREATE INDEX idx_logs_severity ON logs_YYYYMMDD(severity, timestamp);
```

### Retention

Configurable per signal type. Defaults:

- **Spans**: 7 days
- **Metrics**: 14 days
- **Logs**: 7 days

Retention is enforced by dropping entire partition tables (`DROP TABLE
spans_YYYYMMDD`). No row-level deletion, no vacuum overhead.

### Attributes Storage

Span and log attributes are stored as JSON text, queryable via SQLite's
`json_extract()` function. Metric labels are similarly JSON text. This
trades storage efficiency for query flexibility — operators can filter
on any attribute without schema changes.

Histogram values are stored as CBOR blobs (not JSON) because they are
read as a unit, never partially queried, and CBOR is more compact for
numeric arrays.

---

## Query API

The telemetry service exposes a CBOR socket with authenticated actions.
Grants follow the pattern `telemetry/<action>`.

### Actions

#### traces

Filter and retrieve spans. Returns an array of spans sorted by start
time (newest first).

Request fields:

- **trace_id** — `[16]byte`, optional. Return all spans for this trace.
- **machine** — `string`, optional. Filter by machine localpart.
- **source** — `string`, optional. Filter by entity localpart.
- **operation** — `string`, optional. Filter by operation name (prefix
  match supported: `"proxy.*"`).
- **min_duration** — `int64`, optional. Minimum span duration in
  nanoseconds.
- **status** — `uint8`, optional. Filter by status (2 = errors only).
- **start** — `int64`, optional. Earliest start_time (Unix nanoseconds).
- **end** — `int64`, optional. Latest start_time.
- **limit** — `int`, optional. Maximum spans to return (default 100).

#### metrics

Query metric time series. Returns metric observations matching the
filter.

Request fields:

- **name** — `string`, required. Metric name (exact match).
- **machine** — `string`, optional. Filter by machine.
- **source** — `string`, optional. Filter by entity.
- **labels** — `map[string]string`, optional. Label filter (all
  specified labels must match).
- **start** — `int64`, optional. Earliest timestamp.
- **end** — `int64`, optional. Latest timestamp.
- **limit** — `int`, optional. Maximum points to return (default 1000).

#### logs

Query log records. Returns logs sorted by timestamp (newest first).

Request fields:

- **machine** — `string`, optional. Filter by machine.
- **source** — `string`, optional. Filter by entity.
- **min_severity** — `uint8`, optional. Minimum severity level.
- **trace_id** — `[16]byte`, optional. Logs correlated with this trace.
- **search** — `string`, optional. Substring match on body.
- **start** — `int64`, optional. Earliest timestamp.
- **end** — `int64`, optional. Latest timestamp.
- **limit** — `int`, optional. Maximum records to return (default 100).

#### top

Aggregated operational overview. Returns the highest-impact items across
the fleet for a time window.

Request fields:

- **window** — `int64`, required. Time window in nanoseconds (e.g.,
  3600000000000 for one hour).
- **machine** — `string`, optional. Restrict to one machine.

Response includes:

- Slowest operations (by P99 duration)
- Highest error-rate operations
- Highest throughput operations
- Machines with highest resource utilization

#### status

Unauthenticated. Returns service health: ingestion rate (batches/sec,
records/sec), storage usage (bytes per signal, partition count),
retention configuration, connected relay count.

---

## Prometheus Integration

The telemetry service exposes an HTTP `/metrics` endpoint serving
Prometheus exposition format. A single scrape returns metrics for all
machines in the fleet, differentiated by labels.

Standard labels on every metric:

- `fleet` — fleet name
- `machine` — machine name
- `service` — source service/agent name

Example metrics:

```
bureau_proxy_request_duration_seconds{fleet="prod",machine="gpu-box",service="proxy",quantile="0.5"} 0.012
bureau_proxy_request_duration_seconds{fleet="prod",machine="gpu-box",service="proxy",quantile="0.99"} 0.042
bureau_proxy_request_total{fleet="prod",machine="gpu-box",service="proxy",status="ok"} 14523
bureau_proxy_request_total{fleet="prod",machine="gpu-box",service="proxy",status="error"} 37
bureau_socket_request_duration_seconds{fleet="prod",machine="gpu-box",service="ticket",action="list",quantile="0.99"} 0.008
bureau_sandbox_count{fleet="prod",machine="gpu-box",state="running"} 7
bureau_sandbox_count{fleet="prod",machine="gpu-box",state="idle"} 2
bureau_sync_loop_duration_seconds{fleet="prod",machine="gpu-box",service="daemon",quantile="0.5"} 1.2
bureau_telemetry_ingestion_batches_total{fleet="prod",machine="gpu-box"} 8472
bureau_telemetry_storage_bytes{fleet="prod",signal="spans"} 524288000
```

Prometheus scrapes this endpoint on its standard interval. Grafana
dashboards use template variables (`$fleet`, `$machine`, `$service`) to
provide filterable fleet-wide views.

The `/metrics` endpoint is unauthenticated. It exposes aggregate
operational metrics, not raw telemetry data. Operators who need access
control on the scrape endpoint can place it behind a reverse proxy.

---

## Instrumentation

Bureau instruments its own processes using the OpenTelemetry Go SDK for
processes that already carry the dependency, and directly emitting
Bureau-native CBOR telemetry for lean binaries that benefit from
avoiding the protobuf dependency.

### Framework-Level Instrumentation

Instrumenting shared infrastructure gives telemetry to every service for
free:

- **lib/service.SocketServer** — span per request (operation =
  `"socket.handle"`, attributes include action name, auth status,
  response time). Histogram metric for request duration, counter for
  request total, both labeled by action.
- **lib/service.RunSyncLoop** — span per sync cycle. Histogram for sync
  duration. Counter for sync errors.
- **lib/service.Bootstrap** — span covering the full bootstrap sequence
  (session load, validation, registration).

### Per-Binary Instrumentation

Highest-value targets, roughly in priority order:

- **Proxy** — span per forwarded request (method, path, upstream
  latency, credential injection timing). Histogram for request
  duration, counter for requests by status. This is the universal agent
  API surface — instrumenting it gives visibility into every agent's
  behavior.
- **Daemon** — spans for reconciliation cycles, service routing
  decisions, sandbox lifecycle events. Metrics for event processing
  throughput, sync lag.
- **Pipeline executor** — span per pipeline step (step type, step name,
  duration, exit code). Metrics for step success/failure rates.
- **Launcher** — spans for sandbox creation/destruction, credential
  decryption timing (without logging credential values). Metrics for
  sandbox lifecycle duration.

### Agent Instrumentation

Sandboxed agents instrument themselves using their language's
OpenTelemetry SDK. Configuration:

- **Endpoint**: `http+unix:///run/bureau/service/telemetry.sock` (the
  relay socket bind-mounted into the sandbox)
- **Protocol**: OTLP/HTTP
- **Headers**: `Authorization: Bearer <token>` (the agent's service
  token for the telemetry audience)
- **Resource attributes**: `bureau.fleet`, `bureau.machine`,
  `bureau.source` set from environment variables injected by the proxy

Agents that do not configure telemetry produce no overhead — the relay
only processes what it receives.

---

## CLI Surface

The `bureau telemetry` command group provides operator access to the
telemetry service.

| Command | Description |
|---|---|
| `bureau telemetry traces` | Query traces with filters (machine, service, operation, duration, time range) |
| `bureau telemetry trace <trace-id>` | Display a single trace as a span tree with timing |
| `bureau telemetry metrics <name>` | Query a metric time series with filters |
| `bureau telemetry logs` | Query logs with filters (machine, service, severity, search) |
| `bureau telemetry top` | Operational overview: slowest operations, highest error rates, busiest services |
| `bureau telemetry status` | Telemetry service health: ingestion rate, storage usage, relay count |

All commands route to the telemetry service's CBOR socket via service
discovery. The `--fleet` flag selects the fleet (defaults to the
configured fleet). The `--machine` flag restricts output to a single
machine.

---

## Deployment

The telemetry relay is a required service on every machine — included in
the standard machine closure alongside the daemon, launcher, and proxy.
It starts with the daemon and stops when the daemon stops.

The telemetry service is a fleet-managed service, placed by the fleet
controller on persistent infrastructure (machines labeled
`persistent=true` or equivalent). Its fleet service definition specifies
a failover mode so the fleet controller re-places it if the hosting
machine goes offline.

The telemetry service uses the standard CBOR service socket for all
communication, including batch ingestion from relays. Cross-machine
relay→service calls are routed by the daemon through the transport
layer, the same as any other Bureau service-to-service communication.
Relays discover the telemetry service from the fleet service directory.

---

## Relationship to Other Design Documents

- [fundamentals.md](fundamentals.md) — telemetry is a service built on
  the primitives (sandbox isolation for the relay, service sockets for
  queries, Matrix for service discovery).
- [architecture.md](architecture.md) — the relay follows the per-machine
  process model (alongside daemon, launcher, proxy). The telemetry
  service follows the fleet-wide service model.
- [fleet.md](fleet.md) — the fleet controller places the telemetry
  service and optionally queries it for placement-relevant metrics.
  Relay discovery uses the fleet service directory.
- [observation.md](observation.md) — telemetry and observation are
  complementary. Observation provides live terminal access (what is
  happening now). Telemetry provides historical operational data (what
  happened, how long it took, what failed).
- [tickets.md](tickets.md) — sysadmin agents can attach telemetry
  references (trace IDs, metric queries) to tickets as evidence.
- [authorization.md](authorization.md) — telemetry query access is
  governed by service token grants (`telemetry/traces`,
  `telemetry/metrics`, `telemetry/logs`, `telemetry/top`).
