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
messaging/     Primitive 5: message storage and routing (next up)
lib/           Supporting packages (config, version, etc.)
cmd/           Binary entry points
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
