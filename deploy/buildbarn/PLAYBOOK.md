# Bureau Buildbarn Deployment Playbook

Runnable guide for deploying Buildbarn as a Bazel remote cache and
execution scheduler. Workers and runners are added separately once the
scheduler is verified.

## Prerequisites

- Docker and Docker Compose
- `curl` and `jq` (for verification steps)
- A Bazel workspace to test against (the Bureau repo works)

## What This Deploys

**bb-storage** serves three roles in a single process:

- **CAS (Content-Addressable Storage)**: stores build inputs and outputs
  keyed by SHA-256 hash. When Bazel has built a target before, the result
  lives here.
- **AC (Action Cache)**: maps (action hash, platform) to the CAS keys of
  the action's outputs. This is how Bazel knows that a build action has
  already been executed and can skip it.
- **Execute frontend**: receives Execute RPCs from Bazel and forwards
  them to bb-scheduler. This means Bazel uses a single gRPC endpoint
  for both caching and remote execution.

**bb-scheduler** queues build actions and dispatches them to workers:

- Receives forwarded Execute requests from bb-storage
- Maintains per-platform queues with fair scheduling across concurrent
  Bazel invocations
- Workers register via the Synchronize RPC and receive work assignments
- Stateless — all queue state is in-memory. Restarts drop queued actions
  but workers reconnect automatically.

Storage uses block-device-based files (allocated as sparse files that grow
on demand). The block rotation scheme provides self-cleaning — there is no
separate garbage collection. Persistence is enabled so the cache survives
container restarts.

**Ports (exposed to host):**

| Port | Protocol | Service | Purpose |
|------|----------|---------|---------|
| 8980 | gRPC | bb-storage | Bazel endpoint: cache + execute frontend |
| 9980 | HTTP | bb-storage | Diagnostics (Prometheus, pprof, active spans) |
| 7982 | HTTP | bb-scheduler | Admin web UI (build queue inspector) |

**Internal ports (docker network only):**

| Port | Protocol | Service | Purpose |
|------|----------|---------|---------|
| 8982 | gRPC | bb-scheduler | Client endpoint (bb-storage forwards here) |
| 8983 | gRPC | bb-scheduler | Worker endpoint (bb-worker registers here) |
| 8984 | gRPC | bb-scheduler | BuildQueueState (monitoring/tooling) |

## 1. Start the Stack

```bash
cd deploy/buildbarn
docker compose up -d
```

Watch the logs to verify startup:

```bash
docker compose logs -f storage scheduler
```

Data lives in Docker named volumes (`bb-storage-cas`, `bb-storage-ac`),
not in the source tree. bb-storage creates `key_location_map` and
`blocks` sparse files on first start. The CAS blocks file is declared
as 32 GB but starts near zero — it grows as build artifacts are cached.

Persistence is enabled: epoch checkpoints are written every 5 minutes
to `persistent_state` directories inside each volume, so the cache
survives container restarts.

## 2. Verify the Services

bb-storage diagnostics:

```bash
curl -sf http://localhost:9980 > /dev/null && echo "bb-storage is healthy"
```

bb-scheduler admin UI:

```bash
curl -sf http://localhost:7982 > /dev/null && echo "bb-scheduler is healthy"
```

Verify the gRPC endpoint is listening:

```bash
ss -tlnp | grep 8980 || echo "WARN: bb-storage gRPC not listening on 8980"
```

Browse the scheduler admin UI at http://localhost:7982/ to see the build
queue (empty until workers are registered and builds are submitted).

## 3. Configure Bazel

Add the remote executor to `.bazelrc.local` (not committed — per-machine).
`--remote_executor` implies `--remote_cache` at the same address, so a
single flag configures both caching and execution:

```bash
# From the Bureau repo root:
echo 'build --remote_executor=grpc://localhost:8980' >> .bazelrc.local
```

For cache-only mode (no remote execution), use `--remote_cache` instead:

```bash
echo 'build --remote_cache=grpc://localhost:8980' >> .bazelrc.local
```

Or for a single build:

```bash
bazel build //... --remote_cache=grpc://localhost:8980
```

Note: `--remote_executor` only works once workers are registered.
Until then, use `--remote_cache` for cache-only mode.

## 4. Verify Cache Hits

Build twice. The second build should be entirely cached:

```bash
# First build: everything compiles, results are uploaded to the cache.
bazel clean
bazel build //...

# Second build: after clean, all results should come from the remote cache.
bazel clean
bazel build //...
```

Look for `remote cache hit` in the build output. You can also check the
Prometheus metrics for cache hit/miss rates:

```bash
curl -s http://localhost:9980/metrics | grep -i 'blob_access'
```

## 5. Teardown

```bash
docker compose down

# To also remove cached data (Docker named volumes):
docker compose down -v
```

## Configuration Reference

### bb-storage (bb-storage.jsonnet)

**CAS block storage** (`contentAddressableStorage.backend.local`):
- `sizeBytes` on `blocks`: total CAS capacity (default 32 GB, sparse)
- `sizeBytes` on `key_location_map`: hash table index (default 400 MB)
- `oldBlocks/currentBlocks/newBlocks`: block rotation counts. Total
  blocks = old + current + new. Effective capacity is
  `sizeBytes * totalBlocks / (totalBlocks + spareBlocks)`.
- `minimumEpochInterval`: how often persistence checkpoints are written

**AC block storage** (`actionCache.backend.local`):
- Same structure as CAS but much smaller (default 20 MB). Action cache
  entries are a few KB each.

**Execute frontend** (`schedulers`):
- Maps instance name prefixes to scheduler endpoints.
- `addMetadataJmespathExpression` forwards Bazel request metadata for
  per-invocation fair scheduling.

**Authentication** (`authenticationPolicy`):
- Default is `{ allow: {} }` (open access). Appropriate when the service
  is only reachable from the local machine or a trusted network.
- For network-exposed deployments, use `jmespath` or
  `tls_client_certificate` policies.

### bb-scheduler (bb-scheduler.jsonnet)

**Action routing** (`actionRouter`):
- `platformKeyExtractor: { action: {} }` routes actions based on
  platform properties in the Action proto.
- `invocationKeyExtractors` enable fair scheduling across concurrent
  Bazel invocations.

**Timeouts**:
- `defaultExecutionTimeout`: 1800s (30 minutes)
- `maximumExecutionTimeout`: 7200s (2 hours)
- `platformQueueWithNoWorkersTimeout`: 900s (15 minutes) — queues for
  platforms with no registered workers are garbage collected after this.

## Friction Log

Record any issues encountered during the dogfood exercise. Each entry
becomes a candidate for a new bead.

| Step | Friction | Severity | Bead |
|------|----------|----------|------|
| | | | |
