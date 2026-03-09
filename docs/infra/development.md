# Developing Bureau

Guide for developers working on Bureau itself. Covers environment setup,
the local dev instance, iterative development workflow, and testing.

For deploying Bureau to a real machine, see deployment.md.

---

## Prerequisites

- **Git** — clone the repo
- **Nix** — provides the hermetic build environment (Go, Bazel, tmux,
  and all other tools). Install with `script/setup-nix`.
- **Docker** — the dev homeserver runs as a container

```bash
git clone https://github.com/bureau-foundation/bureau.git
cd bureau

# Install Nix if you don't have it.
script/setup-nix

# Enter the Nix dev shell. This provides Go, Bazel, gazelle, tmux,
# golangci-lint, and everything else.
nix develop
```

If you use direnv, the `.envrc` file activates the dev shell
automatically when you enter the directory.

## Build

Bazel is the build system:

```bash
# Build everything.
bazel build //...

# Build a specific binary.
bazel build //cmd/bureau-daemon

# Run gazelle after adding/removing Go files or changing imports.
bazel run //:gazelle
```

To get all Bureau host binaries on PATH from the current worktree:

```bash
eval "$(script/dev-env)"
```

This builds `bureau-host-env` via Nix (cached after first build) and
exports the bin directory onto PATH.

## First-Time Dev Instance Setup

The dev instance runs a local Continuwuity homeserver, launcher, and
daemon with a `bureau/fleet/dev` fleet identity — completely separate
from any production installation on the same machine. No sudo required.

```bash
# Build binaries and put them on PATH.
eval "$(script/dev-env)"

# One-time setup: starts homeserver, bootstraps Bureau, provisions
# a dev machine, runs first boot.
script/dev-setup
```

This creates:
- A Continuwuity container on port 6197 (`deploy/dev/docker-compose.yaml`)
- Dev state in `~/.local/share/bureau-dev/` (session, keypair, credentials)
- Runtime sockets in `/tmp/bureau-dev-$USER/` (short paths for Unix sockets)

Configuration lives in `deploy/dev/config`. Edit it to change the fleet
name, server name, or homeserver port.

To tear down and start fresh:

```bash
script/dev-setup --reset
script/dev-setup
```

## Daily Workflow

### Start the dev instance

```bash
script/dev-up
```

This starts the launcher and daemon in a tmux session with two panes
(one per process). Attach to see their logs:

```bash
tmux -S /tmp/bureau-dev-$USER/tmux.sock attach
```

For headless or CI use:

```bash
script/dev-up --background
```

### Edit, build, deploy

The typical iteration loop:

```bash
# 1. Edit code.
vim cmd/bureau-daemon/reconcile.go

# 2. Build.
bazel build //cmd/bureau-daemon

# 3. Deploy to the running dev instance.
script/dev-deploy
```

`script/dev-deploy` builds a new `bureau-host-env` from the current
worktree and publishes a BureauVersion event. The running daemon detects
it via Matrix /sync, compares binary hashes, and performs an atomic
exec() transition. No restart needed.

When a dev instance is running, `dev-deploy` automatically targets it
(not any production installation). Use `--prod` to explicitly target the
system installation instead.

### Check status

```bash
script/dev-status
```

Reports the state of the homeserver, launcher, daemon, and sockets.

### Stop

```bash
# Stop launcher and daemon (homeserver keeps running).
script/dev-down

# Stop everything including the homeserver.
script/dev-down --all
```

## Testing

### Unit tests

```bash
bazel test //...
```

No external services needed. Runs fast.

### Integration tests

Integration tests spin up a real Continuwuity homeserver, run the full
daemon+launcher stack, and exercise end-to-end flows. They require
Docker and are tagged `manual`:

```bash
# The Bazel server needs docker group membership. If it was started
# without it, shut it down first.
sg docker -c "bazel shutdown; bazel test //integration:integration_test"
```

Integration tests must pass before every commit. Run them after changes
to: daemon, launcher, proxy, messaging, pipeline executor, workspace,
observation, templates, schema, or bootstrap logic.

### Stress testing

To hunt for flaky tests (which are always real bugs):

```bash
# Run integration tests 10 times.
script/stress-integration

# Or with bazel directly:
sg docker -c "bazel test //integration:integration_test --runs_per_test=10"
```

### Linting

```bash
# Run the full lint suite (same as CI).
script/ci-lint
```

The pre-commit hooks enforce gofmt, go vet, SPDX license headers, and
several Bureau-specific checks (no bare tmux, no real clock in tests,
no banned imports).

## Useful Commands

```bash
# Get all Bureau binaries on PATH.
eval "$(script/dev-env)"

# Talk to the dev homeserver directly.
bureau matrix doctor --credential-file ~/.local/share/bureau-dev/bureau-creds \
    --server-name dev.bureau.local

# Create a ticket room in the dev instance.
bureau matrix room create dev/tickets --name Tickets \
    --credential-file ~/.local/share/bureau-dev/bureau-creds

# Enable the ticket service.
bureau ticket enable --space dev --host local \
    --credential-file ~/.local/share/bureau-dev/bureau-creds
```

## Troubleshooting

**"bureau not found on PATH"** — Run `eval "$(script/dev-env)"` or
enter the Nix dev shell with `nix develop`.

**"Docker is not running"** — Start Docker. The dev homeserver and
integration tests both need it.

**Homeserver won't start** — Check the container logs:
`docker compose -f deploy/dev/docker-compose.yaml -p bureau-dev logs`.
Port 6197 might be in use. Edit `BUREAU_DEV_HOMESERVER_PORT` in
`deploy/dev/config`.

**Launcher exits immediately** — Check the tmux pane or log file at
`~/.local/share/bureau-dev/logs/launcher.log`. Common causes: homeserver
unreachable, state directory missing (re-run `script/dev-setup`).

**Daemon exits immediately** — The daemon needs the launcher to be
running (it reads the launcher's session.json). Check that
`/tmp/bureau-dev-$USER/launcher.sock` exists.

**"socket path too long"** — Unix sockets have a 108-character path
limit. The dev scripts use `/tmp/bureau-dev-$USER/` to keep paths short.
If your username is very long, set `BUREAU_DEV_RUN_DIR` in
`deploy/dev/config` to a shorter path.

**Integration tests hang** — Read the test output (not source code).
Find the last successful log line and identify what should have happened
next. See the project's testing philosophy in CLAUDE.md — flaky tests
are always production bugs.

**dev-deploy cycles the wrong instance** — If both a dev and prod
instance are running, `dev-deploy` targets the dev instance by default.
Use `--prod` to target the production installation explicitly.

---

## Related Documents

- deployment.md — deploying Bureau to real machines
- nix-cache.md — setting up the Nix binary cache
- `docs/design/README.md` — reading guide for all design documents
- [environment](https://github.com/bureau-foundation/environment) — infrastructure bootstrap and operator fleet configuration
