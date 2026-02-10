# Bureau Buildbarn Deployment Playbook

Runnable guide for deploying Buildbarn as a Bazel remote cache and remote
execution cluster. The stack runs entirely in Docker Compose.

## Prerequisites

- Docker and Docker Compose
- `curl` (for verification steps)
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

**bb-worker** materializes action inputs and uploads outputs:

- Connects to bb-scheduler (Synchronize RPC) to receive work assignments
- Downloads action inputs from CAS (bb-storage), hardlinks them into the
  build directory
- Maintains a local file cache to avoid re-downloading inputs that appear
  across multiple actions
- After the runner executes, uploads outputs back to CAS

**bb-runner** executes build commands:

- Receives gRPC requests from bb-worker via a unix socket
- Runs the build command in the shared build directory
- Has no network access (`network_mode: none`) — all file I/O is through
  the shared volume; the worker handles all CAS transfers
- Runs on an Ubuntu 24.04 base image (provides coreutils, sh, etc. for
  build actions)

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
docker compose logs -f storage scheduler worker runner
```

Init containers (`storage-init`, `runner-installer`, `worker-init`) run
first and exit. The main services (`storage`, `scheduler`, `worker`,
`runner`) start after their dependencies complete.

Data lives in Docker named volumes, not in the source tree:
- `bb-storage-cas`, `bb-storage-ac` — CAS and AC block storage
- `bb-worker-data` — shared build directory and file cache
- `bb-runner-bin` — runner binary and tini (from installer)

The CAS blocks file is declared as 32 GB but starts near zero — it grows
as build artifacts are cached. Persistence checkpoints are written every
5 minutes, so the cache survives container restarts.

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

Check that the worker has registered with the scheduler by browsing
the admin UI at http://localhost:7982/ — you should see a worker listed
under the `linux` platform queue.

## 3. Configure Bazel

The Bureau repo ships a `remote` config in `.bazelrc`:

```
build:remote --remote_executor=grpc://localhost:8980
build:remote --jobs=64
build:remote --extra_execution_platforms=//:remote_linux_amd64
```

Activate it with `--config=remote`:

```bash
bazel build --config=remote //...
```

For cache-only mode (no remote execution), use `--remote_cache` directly:

```bash
bazel build --remote_cache=grpc://localhost:8980 //...
```

Per-machine overrides (different host, different job count) go in
`.bazelrc.local` (gitignored):

```bash
# Point at a remote Buildbarn cluster:
echo 'build:remote --remote_executor=grpc://buildserver:8980' >> .bazelrc.local
```

## 4. Verify Remote Execution

Build, clean, and build again. The second build should execute remotely
with full cache hits:

```bash
# First build: actions execute on the worker, results uploaded to cache.
bazel clean
bazel build --config=remote //...

# Second build: all results come from the remote cache.
bazel clean
bazel build --config=remote //...
```

Look for `remote cache hit` in the build output. Check the scheduler
admin UI at http://localhost:7982/ — completed actions should be visible.

Tests that depend on external tools (tmux, network access) will fail
remotely because the runner container is a minimal Ubuntu image with no
network. Run these locally or with `--test_strategy=local`:

```bash
bazel test --config=remote //... --test_strategy=local
```

This executes test actions locally while still using the remote cache for
build actions. Once the runner has access to the full Nix store (via
bind-mount), all tests can execute remotely.

You can also check Prometheus metrics:

```bash
curl -s http://localhost:9980/metrics | grep -i 'blob_access'
```

## 5. Teardown

```bash
docker compose down

# To also remove cached data (Docker named volumes):
docker compose down -v
```

## Using From Any Bazel Project

Any Bazel project can use this Buildbarn cluster — it is not
Bureau-specific. The CAS is content-addressed and deduplicates across
all projects automatically.

**Step 1: Define an execution platform.** The `exec_properties` must
match exactly what the worker registers (see bb-worker.jsonnet). A
mismatch means actions queue forever with no worker to run them.

In your project's `BUILD.bazel`:

```starlark
platform(
    name = "remote_linux_amd64",
    constraint_values = [
        "@platforms//os:linux",
        "@platforms//cpu:x86_64",
    ],
    exec_properties = {
        "OSFamily": "linux",
    },
)
```

**Step 2: Add remote config to `.bazelrc`:**

```
build:remote --remote_executor=grpc://HOST:8980
build:remote --jobs=64
build:remote --extra_execution_platforms=//:remote_linux_amd64
```

Replace `HOST` with the machine running the Buildbarn stack.

**Step 3: Build.**

```bash
bazel build --config=remote //...
```

**Action cache isolation:** Action cache keys include the instance name.
Projects sharing the same `--remote_instance_name` (default: empty string)
share AC hits. For isolation between projects, set different instance names:

```
build:remote --remote_instance_name=my-project
```

Then configure worker pools with matching `instanceNamePrefix` in
bb-worker.jsonnet.

## Compute Limiting

Several knobs control how much compute the cluster uses:

| Knob | Where | Controls |
|------|-------|----------|
| `concurrency` | bb-worker.jsonnet `runners[]` | Max parallel actions per worker. This is the primary parallelism knob. |
| `--jobs` | .bazelrc (client-side) | Max actions Bazel submits concurrently. Set >= `concurrency` so the scheduler always has work queued. |
| `inputDownloadConcurrency` | bb-worker.jsonnet | Parallel CAS blob downloads per worker |
| `outputUploadConcurrency` | bb-worker.jsonnet | Parallel CAS blob uploads per worker |
| `defaultExecutionTimeout` | bb-scheduler.jsonnet | Per-action time limit (default 1800s / 30 min) |
| `maximumExecutionTimeout` | bb-scheduler.jsonnet | Hard cap (default 7200s / 2 hours) |

**Sizing `concurrency`:** Each Go compile action uses roughly 1 core and
200–500 MB RAM. Size to approximately (total cores - 4) to leave headroom
for the infrastructure containers (storage, scheduler, worker I/O). The
default of 8 is appropriate for a 12-core machine.

**Multiple workers:** To scale beyond one machine, run additional
worker+runner pairs on other hosts. Each worker registers with the
scheduler independently. The scheduler distributes actions across all
registered workers using fair scheduling.

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

### bb-worker (bb-worker.jsonnet)

**Build directory** (`buildDirectories[0].native`):
- `buildDirectoryPath`: where action inputs are materialized
  (hardlinked from cache)
- `cacheDirectoryPath`: LRU file cache. Avoids re-downloading CAS blobs
  that appear in multiple actions.
- `maximumCacheFileCount` / `maximumCacheSizeBytes`: cache eviction limits
- `cacheReplacementPolicy`: `LEAST_RECENTLY_USED`

**Runner connection** (`runners[0]`):
- `endpoint.address`: unix socket shared with the runner container
- `concurrency`: max parallel actions (the primary parallelism knob)
- `platform.properties`: must match the execution platform Bazel sends

**I/O parallelism**:
- `inputDownloadConcurrency`: parallel CAS downloads (default 10)
- `outputUploadConcurrency`: parallel CAS uploads (default 11)

### bb-runner (bb-runner.jsonnet)

Minimal configuration:
- `buildDirectoryPath`: must match the worker's build directory
- `grpcServers[0].listenPaths`: unix socket (must match worker's runner
  endpoint)

## Friction Log

Record any issues encountered during the dogfood exercise. Each entry
becomes a candidate for a tracked issue.

| Step | Friction | Severity |
|------|----------|----------|
| bb-worker config | `actionCache` and `contentAddressableStorage` are top-level in bb-storage but nested under `blobstore` in bb-worker. The proto field names differ between components. | med |
| runner binary | `bb-runner-installer` image ships `bb_runner` but not `tini`. Use Docker Compose `init: true` instead. | low |
| @platforms dep | `platform()` rule needs `bazel_dep(name = "platforms")` in MODULE.bazel — not implicitly visible to user BUILD files under bzlmod. | med |
| tmux tests | `//observe:observe_test` and `//cmd/bureau-daemon:bureau-daemon_test` fail remotely (no tmux in runner). Need Nix bind-mount or local execution tag. | high |
| WebRTC tests | `//transport:transport_test` fails remotely (runner has `network_mode: none`, WebRTC needs loopback). Needs local execution tag. | high |
