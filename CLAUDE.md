# Bureau

AI agent orchestration system. Runs untrusted agent processes in sandboxed
environments with credential isolation, live observation, and structured messaging.

## Architecture

Five primitives (see `.notes/design/FUNDAMENTALS.md`):

1. **Sandbox** — bwrap namespace isolation (`sandbox/`)
2. **Proxy** — credential injection via HTTP proxy (`proxy/`)
3. **Bridge** — TCP-to-Unix socket forwarder for sandboxed HTTP (`bridge/`)
4. **Observation** — not yet implemented; depends on messaging for cross-machine support
5. **Messaging** — agent-to-agent communication via Matrix, accessed through proxy endpoints

## Repository layout

```
sandbox/       Primitive 1: bubblewrap isolation
proxy/         Primitive 2: credential injection proxy
bridge/        Primitive 3: TCP-to-Unix socket bridge
transport/     Cross-machine service routing (TCP now, WebRTC future)
messaging/     Primitive 5: Matrix client-server API wrapper (Client + Session)
lib/           Supporting packages:
  config/        YAML config loading
  principal/     Localpart validation, Matrix ID construction, socket path mapping
  schema/        Matrix state event types and content structs (Bureau protocol)
  sealed/        age encryption/decryption for credential bundles
  version/       Build version info
cmd/           Binary entry points:
  bureau-launcher/     Privileged process: keypair, credential decryption, sandbox lifecycle
  bureau-daemon/       Unprivileged process: Matrix sync, config reconciliation, service routing
  bureau-proxy/        Per-sandbox credential injection proxy
  bureau-bridge/       TCP-to-Unix socket bridge CLI
  bureau-sandbox/      Sandbox creation CLI
  bureau-credentials/  Credential provisioning and machine config CLI
  bureau-proxy-call/   One-shot HTTP request through a proxy socket
  bureau-matrix-setup/ Homeserver bootstrap (admin account, spaces, rooms)
deploy/        Deployment configurations (matrix/, etc.)
```

Top-level directories are primitives or first-class concepts. Supporting library
code goes in `lib/`. No `internal/` or `pkg/` — this is an application, not a
public Go library.

## Build

```bash
go build ./...
go test ./...
```

No build system beyond `go build`. No Bazel. No Makefiles unless they add value.

## Conventions

- Go is the primary language for infrastructure code
- Pre-commit hooks enforce gofmt and go vet
- SPDX license headers on all source files
- Tests live next to the code they test (`foo_test.go` beside `foo.go`)

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
