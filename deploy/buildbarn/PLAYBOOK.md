# Bureau Buildbarn Deployment Playbook

Runnable guide for deploying Buildbarn bb-storage as a Bazel remote cache.
This is the first Buildbarn component; scheduler, workers, and runners
are added later for remote execution.

## Prerequisites

- Docker and Docker Compose
- `curl` and `jq` (for verification steps)
- A Bazel workspace to test against (the Bureau repo works)

## What This Deploys

**bb-storage** provides two services in one process:

- **CAS (Content-Addressable Storage)**: stores build inputs and outputs
  keyed by SHA-256 hash. When Bazel has built a target before, the result
  lives here.
- **AC (Action Cache)**: maps (action hash, platform) to the CAS keys of
  the action's outputs. This is how Bazel knows that a build action has
  already been executed and can skip it.

Storage uses block-device-based files (allocated as sparse files that grow
on demand). The block rotation scheme provides self-cleaning — there is no
separate garbage collection. Persistence is enabled so the cache survives
container restarts.

**Ports:**

| Port | Protocol | Purpose |
|------|----------|---------|
| 8980 | gRPC | Bazel remote cache endpoint (`--remote_cache=grpc://host:8980`) |
| 9980 | HTTP | Diagnostics (Prometheus metrics, pprof, active spans) |

## 1. Start bb-storage

```bash
cd deploy/buildbarn
docker compose up -d
```

Watch the logs to verify startup:

```bash
docker compose logs -f storage
```

Data lives in Docker named volumes (`bb-storage-cas`, `bb-storage-ac`),
not in the source tree. bb-storage creates `key_location_map` and
`blocks` sparse files on first start. The CAS blocks file is declared
as 32 GB but starts near zero — it grows as build artifacts are cached.

Persistence is enabled: epoch checkpoints are written every 5 minutes
to `persistent_state` directories inside each volume, so the cache
survives container restarts.

## 2. Verify the Service

The diagnostics HTTP server confirms bb-storage is running:

```bash
curl -sf http://localhost:9980 > /dev/null && echo "bb-storage is healthy"
```

Check Prometheus metrics:

```bash
curl -s http://localhost:9980/metrics | head -20
```

Verify the gRPC port is listening:

```bash
ss -tlnp | grep 8980 || echo "WARN: bb-storage gRPC not listening on 8980"
```

## 3. Configure Bazel

Add the remote cache to `.bazelrc.local` (not committed — per-machine):

```bash
# From the Bureau repo root:
echo 'build --remote_cache=grpc://localhost:8980' >> .bazelrc.local
```

Or to use it for a single build:

```bash
bazel build //... --remote_cache=grpc://localhost:8980
```

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

The bb-storage configuration lives in `bb-storage.jsonnet`. Key tuning
parameters:

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

**Authentication** (`authenticationPolicy`):
- Default is `{ allow: {} }` (open access). Appropriate when bb-storage
  is only reachable from the local machine or a trusted network.
- For network-exposed deployments, use `jmespath` or
  `tls_client_certificate` policies.

## Friction Log

Record any issues encountered during the dogfood exercise. Each entry
becomes a candidate for a new bead.

| Step | Friction | Severity | Bead |
|------|----------|----------|------|
| | | | |
