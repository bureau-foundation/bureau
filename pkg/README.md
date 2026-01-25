# Public Packages

This directory contains Go packages that are part of Bureau's public API and may be imported by external projects or tools.

## Packages

### `protocol/`

Protocol definitions and types:

- Message formats (HANDOFF, ESCALATION, DECISION, etc.)
- Agent manifest schema
- Tool definitions
- Shared constants

### `channel/`

Channel abstraction interfaces:

- `Channel`, `Thread`, `State` interfaces
- Types that external implementations need

## Conventions

- Packages here are public API; changes require careful consideration
- Maintain backward compatibility where possible
- Document all exported types and functions
- Keep dependencies minimal to reduce import burden
- Version with semantic versioning once stable

## Importing

External projects can import these packages:

```go
import (
    "github.com/benvanik/bureau/pkg/protocol"
    "github.com/benvanik/bureau/pkg/channel"
)
```
