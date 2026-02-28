# Bureau

AI agent orchestration system. Runs untrusted agent processes in sandboxed
environments with credential isolation, live observation, and structured messaging.

## Architecture

Five primitives (see `docs/design/fundamentals.md`):

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
  agentdriver/   Agent process runtime (spawning, output parsing, session logging)
  artifactstore/ CAS engine (BLAKE3 hashing, CDC chunking, compression, containers, cache)
  binhash/       Binary identity hashing for self-update
  bootstrap/     First-boot homeserver setup helpers
  config/        YAML config loading
  content/       Embedded Bureau content (pipeline definitions, templates)
  git/           Git worktree operations for workspace lifecycle
  hwinfo/        Hardware fingerprinting for machine identity
  netutil/       Network utility helpers
  nix/           Nix store path and environment resolution
  pipelinedef/   JSONC pipeline parsing, variable substitution, step validation
  principal/     Localpart validation, Matrix ID construction, socket path mapping
  schema/        Bureau protocol types (Matrix state event content structs)
    agent/       Agent session, context, and metrics event types
    artifact/    Artifact scope configuration event types
    fleet/       Fleet service, machine, HA lease, alert event types
    observation/ Layout, window, pane event types
    pipeline/    Pipeline definition, step, result event types
    telemetry/   Telemetry data types (spans, metrics, logs, batch wire format)
    ticket/      Ticket content, gate, note, attachment event types
    workspace/   Workspace state, worktree, project config event types
  sealed/        age encryption/decryption for credential bundles
  secret/        Guarded memory for secret material (zero-on-close)
  templatedef/   Template resolution, inheritance walking, merge logic
  testutil/      Shared test helpers (SocketDir, DataBinary)
  ticketindex/   In-memory ticket index (readiness, search, dependency tracking)
  ticketui/      Terminal UI components for ticket display
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
  bureau-telemetry-relay/  Per-machine telemetry collection and forwarding
  bureau-test-agent/     Minimal test agent for integration tests
integration/   End-to-end integration tests (real homeserver, full daemon+launcher stack)
deploy/        Deployment configurations (matrix/, buildbarn/, systemd/, test/)
docs/          Documentation (design/ for architecture, infra/ for operations)
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

See `docs/design/nix.md` for the full build and distribution architecture.

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

## Running Bureau Locally

Get all Bureau host binaries on PATH from the current worktree:

```bash
eval "$(script/dev-env)"
```

This builds `bureau-host-env` via Nix (cached after first build) and adds its
`bin/` directory to PATH. All host binaries end up in the same directory, which
is required — the launcher finds bureau-proxy, bureau-sandbox, and other
companions by looking next to its own binary.

For iterative development of a single binary, build and run directly via Bazel:

```bash
bazel build //cmd/bureau-daemon
./bazel-bin/cmd/bureau-daemon/bureau-daemon_/bureau-daemon --help
```

To deploy changes to the running systemd stack (builds, publishes a version
event, daemon picks it up via /sync and performs atomic exec() transition):

```bash
script/dev-deploy
```

## Testing

**Unit tests** run with `bazel test //...` and require no external services.

**Integration tests** are tagged `manual` and require Docker. They spin up a
real Continuwuity homeserver, run the full daemon+launcher stack, and exercise
end-to-end flows. They must be run explicitly:

```bash
# The bazel server must have docker group membership. If the server was
# started without it, shut it down first so it restarts with the right groups.
sg docker -c "bazel shutdown; bazel test //integration:integration_test"
```

Integration tests must pass before every commit. Run them after any change to:
daemon, launcher, proxy, messaging, pipeline executor, workspace handling,
observation, templates, schema, or bootstrap logic. When in doubt, run them.

## Diagnosing runtime failures

When a test fails, hangs, or produces unexpected behavior:

1. **Read the test output or log file. Not source code. The log file.**
2. In your response, write the last successful log line and identify the
   gap — what should have happened next but didn't.
3. Only then may you read the specific function identified by the gap —
   not its callers, not its callees, not "related" code.
4. If you have read more than 3 source files without a fix, you have
   left the diagnostic path. Stop, re-read the logs, and start over
   from step 1.

Reading source code to theorize about runtime behavior is not diagnosis.
It is procrastination that feels like work. Bureau is built to be
diagnosed from logs and Matrix events alone. If you can't diagnose a
failure from logs, the logging is insufficient — fix the logging first,
re-run, and read the new logs. That logging improvement is part of the
fix, not throwaway debug output.

Make small changes and run tests after each one. Do not batch large sets
of changes (new types, new methods, rewritten helpers, updated callers)
and run the test for the first time only after all of it. Each change
should be validated before the next one builds on it.

## Security principles

Bureau runs untrusted code. Every design decision must account for this.

- **Every API surface requires authentication and authorization.** Read and
  write, query and mutation, socket and HTTP — no exceptions. A reachable
  channel is not an access policy. Information disclosure is a vulnerability:
  ticket titles, dependency graphs, room membership, service registrations,
  and operational metadata are all sensitive. "It's read-only" or "only
  accessible over a unix socket" is never sufficient justification to skip
  auth. If a caller can reach an endpoint, the endpoint must verify who
  they are and whether they are permitted to see the response.

- **Default-deny.** If there is no explicit grant, the answer is no. This
  applies to Matrix operations (MatrixPolicy), service socket actions, and
  any future API surface.

- **Credentials never appear on command lines, in logs, or in error messages.**
  Use file paths, sealed inputs, or environment variables. Mask tokens in
  debug output.

## Conventions

- Go is the primary language for infrastructure code
- Lefthook pre-commit hooks enforce gofmt, go vet, and license headers
- SPDX license headers on all source files
- Tests live next to the code they test (`foo_test.go` beside `foo.go`)
- BUILD.bazel files are maintained by gazelle; manual edits go in `data`, `env`,
  and `deps` sections that gazelle preserves

## Documentation map

Design documents live in `docs/design/` (see `docs/design/README.md` for the
full reading guide). Each Go package also has a `doc.go` with implementation-level
documentation.

**Core architecture** (read first):
- `overview.md` — What Bureau is and why it exists
- `fundamentals.md` — The five primitives: sandbox, proxy, bridge, observation, messaging
- `architecture.md` — Runtime topology: launcher, daemon, proxy processes, IPC, naming
- `information-architecture.md` — Matrix data model: spaces, rooms, threads, state events

**Primitive deep-dives** (read when working on a subsystem):
- `credentials.md` — age encryption, TPM/keyring, privilege separation
- `observation.md` — Relay architecture, wire protocol, ring buffer, layouts
- `pipelines.md` — Step types, variable substitution, thread logging
- `nix.md` — Bazel compilation, Nix packaging, binary cache

**Services** (built on the primitives):
- `tickets.md` — Work items, dependencies, gates, notes, attachments
- `artifacts.md` — CAS, BLAKE3 hashing, CDC chunking, FUSE mount
- `workspace.md` — Git worktrees, setup/teardown pipelines, sysadmin agent
- `fleet.md` — Multi-machine service placement, HA, batch scheduling
- `authorization.md` — Grants, denials, allowances, credential provisioning

**Applications** (patterns built from everything above):
- `dev-team.md` — Self-hosted development team, role taxonomy, review pipeline
- `agent-layering.md` — Agent runtime integration, session management, MCP, quota
- `forgejo.md` — External service connector pattern

## Design document rules

Design documents are code. They describe the target architecture and must be
kept accurate as the system evolves.

- **When you touch a subsystem, check its design document.** If the
  implementation has diverged, update the document or file a ticket
  explaining the divergence.
- **Open questions belong in tickets, not documents.** A question buried in
  a document becomes invisible the moment someone stops reading it. File a
  bead or ticket so it stays in the queue.
- **No transient references.** No bead IDs, no "as of Feb 2026", no "phase 2"
  language. Design documents are timeless. If the architecture changes, update
  the document.
- **Cross-references use document names, not paths.** Write "fundamentals.md"
  not "docs/design/fundamentals.md". Paths change; names are stable.
- **One document per concept.** Don't split a subsystem across documents.
  Don't merge unrelated subsystems into one.

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

## Bureau Ticket System

Bureau has its own ticket system backed by Matrix state events. Tickets are
the canonical work tracking mechanism. Use `script/bt` — a convenience wrapper
that auto-discovers the ticket room and adds `--service` mode.

### Quick Reference

```bash
# See what's ready to work on
script/bt ready

# Show ticket details
script/bt show tkt-a3f9

# Create a ticket
script/bt create --title "Fix the thing" --type bug --priority 1

# Create with a body
script/bt create --title "Implement feature" --type task --priority 2 \
    --body "Description of what needs to happen"

# Create a subtask of an epic
script/bt create --title "Subtask" --type task --parent tkt-epic-id

# List open tickets
script/bt list --status open

# Search tickets
script/bt search "memory leak"
script/bt grep "proxy.*socket"

# Update a ticket
script/bt update tkt-a3f9 --status in_progress
script/bt update tkt-a3f9 --assignee '@bureau/fleet/prod/agent/code-reviewer:bureau.local'

# Close a ticket
script/bt close tkt-a3f9 --reason "Fixed in commit abc123"

# View epic health
script/bt epic-health tkt-de1b

# Stats
script/bt stats

# Dependencies
script/bt dep add tkt-child tkt-blocker
script/bt dep remove tkt-child tkt-blocker

# Show dependency closure
script/bt deps tkt-a3f9
```

### Workflow

1. Run `script/bt ready` to find actionable work (unblocked, open)
2. Claim: `script/bt update <id> --status in_progress`
3. Work on the ticket
4. Close: `script/bt close <id> --reason "description"`
5. Check `script/bt ready` for next work

### Room specification

The `--room` flag accepts three forms:
- Room ID: `!opaque:bureau.local`
- Full alias: `#prod/tickets:bureau.local`
- Bare localpart: `prod/tickets`

`script/bt` auto-discovers the room when there is exactly one ticket room.
Set `BUREAU_TICKET_ROOM` to override (any of the three forms above).

### Priority and type conventions

Same as beads: P0=critical, P1=high, P2=medium, P3=low, P4=backlog.
Types: task, bug, feature, epic, chore, docs, question.

### Epics

Epics are tickets with `--type epic`. Child tickets reference their parent
with `--parent <epic-id>`. Use `script/bt epic-health <epic-id>` to see
progress. Use `script/bt children <epic-id>` to list children.
