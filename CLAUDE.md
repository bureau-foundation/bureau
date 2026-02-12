# Bureau

AI agent orchestration system. Runs untrusted agent processes in sandboxed
environments with credential isolation, live observation, and structured messaging.

## Architecture

Five primitives (see `.notes/design/FUNDAMENTALS.md`):

1. **Sandbox** — bwrap namespace isolation (`sandbox/`)
2. **Proxy** — credential injection via HTTP proxy (`proxy/`)
3. **Bridge** — TCP-to-Unix socket forwarder for sandboxed HTTP (`bridge/`)
4. **Observation** — live terminal access to sandboxed principals (`observe/`, daemon routing in `cmd/bureau-daemon/`)
5. **Messaging** — agent-to-agent communication via Matrix, accessed through proxy endpoints (`messaging/`)

## Repository layout

```
sandbox/       Primitive 1: bubblewrap isolation
proxy/         Primitive 2: credential injection proxy
bridge/        Primitive 3: TCP-to-Unix socket bridge
observe/       Primitive 4: live terminal access (relay, protocol, ring buffer, layout, dashboard)
messaging/     Primitive 5: Matrix client-server API wrapper (Client + Session)
transport/     Cross-machine service routing (TCP now, WebRTC future)
lib/           Supporting packages:
  binhash/       Binary identity hashing for self-update
  bootstrap/     First-boot homeserver setup helpers
  config/        YAML config loading
  content/       Embedded Bureau content (pipeline definitions, templates)
  git/           Git worktree operations for workspace lifecycle
  hwinfo/        Hardware fingerprinting for machine identity
  netutil/       Network utility helpers
  nix/           Nix store path and environment resolution
  pipeline/      JSONC pipeline parsing, variable substitution, step validation
  principal/     Localpart validation, Matrix ID construction, socket path mapping
  schema/        Matrix state event types and content structs (Bureau protocol)
  sealed/        age encryption/decryption for credential bundles
  secret/        Guarded memory for secret material (zero-on-close)
  template/      Template resolution and merge logic
  testutil/      Shared test helpers (SocketDir, DataBinary)
  tmux/          tmux server/session management
  version/       Build version info
  watchdog/      Binary self-update detection
cmd/           Binary entry points:
  bureau/                Unified CLI entry point
    cli/                 Shared CLI framework (command dispatch, help, typo suggestions)
    environment/         Environment subcommands (list, build, status)
    machine/             Machine subcommands
    matrix/              Matrix subcommands (setup, space, room, user, doctor)
    observe/             Observation subcommands (observe, dashboard, list)
    pipeline/            Pipeline subcommands
    quickstart/          Guided zero-to-sandbox setup
    template/            Template subcommands
    workspace/           Workspace subcommands (create, destroy, list)
  bureau-launcher/       Privileged process: keypair, credential decryption, sandbox lifecycle
  bureau-daemon/         Unprivileged process: Matrix sync, config reconciliation, service routing
  bureau-proxy/          Per-sandbox credential injection proxy
  bureau-bridge/         TCP-to-Unix socket bridge CLI
  bureau-sandbox/        Sandbox creation CLI
  bureau-credentials/    Credential provisioning and machine config CLI
  bureau-observe-relay/  Observation relay (PTY allocation, tmux attach, ring buffer)
  bureau-proxy-call/     One-shot HTTP request through a proxy socket
  bureau-pipeline-executor/  Pipeline executor for sandboxed step sequencing
  bureau-test-agent/     Minimal test agent for integration tests
integration/   End-to-end integration tests (real homeserver, full daemon+launcher stack)
deploy/        Deployment configurations (matrix/, buildbarn/, systemd/, test/)
docs/          Checked-in documentation (infra/)
script/        Dev environment setup (Nix installer, version pins)
```

Top-level directories are primitives or first-class concepts. Supporting library
code goes in `lib/`. No `internal/` or `pkg/` — this is an application, not a
public Go library.

## Dev Environment Setup

Nix provides the hermetic development environment. All tools (Go, Bazel, gazelle,
buildifier, tmux, etc.) come from the Nix dev shell — no global installs.

```bash
# Install Nix (Determinate Nix, pinned version). Requires sudo.
script/setup-nix

# Verify installation.
script/setup-nix --check

# Enter the dev shell (once flake.nix exists).
nix develop
```

The pinned Nix version lives in `script/nix-installer-version` (installer release)
and `script/nix-expected-version` (expected `nix --version` output). These are
committed to the repo. Upgrading Nix means updating both files and re-running
`script/setup-nix --force`.

See `.notes/design/NIX.md` for the full build and distribution architecture.

## Build

Bazel is the build system. All builds and tests go through Bazel.

```bash
bazel build //...
bazel test //...
```

Gazelle generates BUILD.bazel files from Go source. After adding/removing Go
files or changing imports, run:

```bash
bazel run //:gazelle
```

Test binaries needed by integration tests are declared as `data` dependencies
in BUILD.bazel and resolved at runtime via `testutil.DataBinary()` (which reads
`RUNFILES_DIR` + `$(rlocationpath ...)` env vars). Tests do not call `go build`.

## Conventions

- Go is the primary language for infrastructure code
- Pre-commit hooks enforce gofmt and go vet
- SPDX license headers on all source files
- Tests live next to the code they test (`foo_test.go` beside `foo.go`)
- BUILD.bazel files are maintained by gazelle; manual edits go in `data`, `env`,
  and `deps` sections that gazelle preserves

## Documentation map

Design documents live in `.notes/design/` and describe the target architecture
for each subsystem. Each Go package also has a `doc.go` with implementation-level
documentation.

- `OVERVIEW.md` — What Bureau is: infrastructure for running organizations of AI agents
- `FUNDAMENTALS.md` — The five primitives: sandbox, proxy, bridge, observation, messaging
- `ARCHITECTURE.md` — Runtime topology: launcher/daemon/proxy processes, IPC, sandbox contract, naming, service discovery
- `INFORMATION_ARCHITECTURE.md` — Matrix hierarchy: spaces, rooms, threads, state events, agent memory, artifacts
- `CREDENTIALS.md` — age encryption, TPM/keyring, launcher/daemon privilege separation for secrets
- `OBSERVATION.md` — Relay architecture, wire protocol, ring buffer, layouts, dashboard composition
- `PIPELINES.md` — Pipeline primitive: step types (shell, publish, healthcheck, guard), variables, execution model
- `WORKSPACE.md` — Workspace lifecycle: git worktrees, setup/teardown principals, project templates, sysadmin agent
- `NIX.md` — Build and distribution: Bazel compilation, Nix packaging, flake structure, binary cache
- `TICKETS.md` — Ticket system: Matrix-native coordination primitive, direct service sockets, CLI

<!-- br-agent-instructions-v1 -->

---

## Beads Workflow Integration

This project uses [beads_rust](https://github.com/Dicklesworthstone/beads_rust) (`br`/`bd`) for issue tracking. Issues are stored in `.beads/` and local only.
CRITICAL: NEVER MENTION BEADS IN CODE. THe beads are for your local work tracking only and do not persist. Always write proper TODOs or use github issues for long term/persistent tracking. 95% of all work you do should be tracked in beads. Think of it like a memory.

### Essential Commands

```bash
# View ready issues (unblocked, not deferred)
br ready              # or: bd ready

# List and search
br list --status=open # All open issues
br show <id>          # Full issue details with dependencies
br search "keyword"   # Full-text search

# Create and update
br create --title="..." --description="..." --type=task --priority=2
br update <id> --status=in_progress
br close <id> --reason="Completed"
br close <id1> <id2>  # Close multiple issues at once
```

### Workflow Pattern

1. **Start**: Run `br ready` to find actionable work
2. **Claim**: Use `br update <id> --status=in_progress`
3. **Work**: Implement the task
4. **Complete**: Use `br close <id>`
5. **Sync**: Always run `br sync --flush-only` at session end

### Key Concepts

- **Dependencies**: Issues can block other issues. `br ready` shows only unblocked work.
- **Priority**: P0=critical, P1=high, P2=medium, P3=low, P4=backlog (use numbers 0-4, not words)
- **Types**: task, bug, feature, epic, chore, docs, question
- **Blocking**: `br dep add <issue> <depends-on>` to add dependencies

### Best Practices

- Check `br ready` at session start to find available work
- Update status as you work (in_progress → closed)
- Create new issues with `br create` when you discover tasks
- Use descriptive titles and set appropriate priority/type
- Always sync before ending session

<!-- end-br-agent-instructions -->
