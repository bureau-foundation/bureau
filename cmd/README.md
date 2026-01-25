# Command Binaries

This directory contains the `main` packages for Bureau's executable binaries.

## Binaries

### `bureau`

The primary CLI tool for interacting with Bureau. Used for:

- Administrative commands (status, reload, agent management)
- Cost and metrics queries
- Manual agent interaction
- Development utilities

### `bureau-core`

The core service daemon. Responsibilities:

- Matrix client and message routing
- Agent lifecycle management (wake/sleep)
- Configuration hot-reload
- Coordination with other services

### `bureau-tools`

The tool execution service. Responsibilities:

- Sandboxed code execution (Python, shell)
- Capability validation
- External API proxying
- Result sanitization

## Building

```bash
# Build all binaries
bazel build //cmd/...

# Build specific binary
bazel build //cmd/bureau

# Run directly
bazel run //cmd/bureau -- status
```

## Conventions

Each subdirectory contains a single `main.go` that:

- Parses flags and configuration
- Sets up logging and tracing
- Delegates to packages in `internal/` for actual logic
- Handles graceful shutdown
