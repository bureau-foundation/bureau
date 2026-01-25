# Internal Packages

This directory contains Go packages that are internal to Bureau and not intended for external import.

## Packages

### `core/`

Core Bureau logic:

- Agent management and state
- Message routing and dispatch
- Configuration management
- Hot-reload coordination

### `matrix/`

Matrix protocol implementation:

- Client connection and sync
- Room and message handling
- State management
- Typing indicators and read receipts

### `channel/`

Channel abstraction layer:

- Interface definitions for communication backends
- Matrix implementation
- Mock implementation for testing

### `policy/`

Policy engine integration:

- OPA (Open Policy Agent) client
- Capability checking
- Approval workflow logic

### `tools/`

Tool execution framework:

- Tool registry and discovery
- Sandbox management
- Result processing

## Conventions

- Packages here are implementation details, not public API
- Import paths: `github.com/benvanik/bureau/internal/...`
- These packages can be refactored freely without breaking external users
- Keep packages focused; split when they grow beyond ~1000 lines
