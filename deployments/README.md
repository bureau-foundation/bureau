# Deployments

Deployment configurations for Bureau infrastructure.

## Contents

### systemd Units

Service definitions for running Bureau on Linux:

- `bureau-core.service`
- `bureau-tools.service`
- `bureau-watchers.service`

### Docker

Development and testing containers:

- `Dockerfile.dev` — Development environment
- `docker-compose.yaml` — Local stack for testing
- `sandbox/` — Sandboxed execution containers

### Infrastructure

Configuration for supporting services:

- Litestream (SQLite replication)
- Matrix (Synapse) setup notes
- Reverse proxy configuration

## Environments

Bureau supports three environments:

| Environment | Purpose | Location |
|-------------|---------|----------|
| Sandbox | Local development | Your machine |
| Staging | Pre-production testing | Staging server |
| Production | Real workloads | Production server |

## Deployment Process

1. Changes merged to `main`
2. CI builds and tests
3. Deploy to staging (automatic or manual)
4. Validation in staging
5. Deploy to production (manual approval)

See `docs/operations.md` for detailed procedures.
