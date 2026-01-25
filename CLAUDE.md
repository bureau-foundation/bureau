# Bureau Development Guide

Bureau is a personal operations system built on AI agents. This document provides
essential context for agents working on the codebase.

## Project Philosophy

- **Agents as staff, not tools**: Each agent has a defined role, limited authority, and clear escalation paths
- **Async-first**: Synchronous interaction only when truly needed
- **Observable**: All activity visible in logs; audit trail by design
- **Local-first**: Your data stays on your infrastructure
- **Capability-based security**: Agents can only do what they're explicitly allowed to do

## Repository Structure

```text
bureau/
├── cmd/                    # Go binaries (main packages)
│   ├── bureau/            # Primary CLI
│   ├── bureau-core/       # Core orchestration service
│   └── bureau-tools/      # Tool execution service
├── internal/              # Internal Go packages (not exported)
│   ├── core/             # Core logic
│   ├── channel/          # Channel abstraction (Matrix facade)
│   ├── matrix/           # Matrix client implementation
│   ├── policy/           # OPA policy integration
│   └── tools/            # Tool execution framework
├── pkg/                   # Public Go packages (exported)
│   ├── channel/          # Channel interface definitions
│   └── protocol/         # Shared protocol definitions
├── services/              # Python services
│   ├── watchers/         # External service watchers
│   ├── agents/           # Agent execution environment
│   └── shared/           # Shared Python utilities
├── config/                # Configuration templates
├── docs/                  # Documentation
├── tests/                 # Test suites
├── scripts/               # Utility scripts
└── deployments/           # Deployment configurations
```

## Code Style

### File Headers

All source files require SPDX headers:

```go
// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0
```

```python
# Copyright 2026 The Bureau Authors
# SPDX-License-Identifier: Apache-2.0
```

### Naming Conventions

- **Do NOT abbreviate variable names**: use full words
  - `len` → `length`
  - `buf` → `buffer`
  - `cnt` → `count`
  - `num` → `count` or `number`
  - `pos` → `position`
  - `str` → `string` (or more descriptive)

### Formatting

Formatting is enforced by pre-commit hooks:

- **Go**: `gofmt` (tabs, standard formatting)
- **Python**: `black` + `ruff` (4 spaces, 88 char lines)
- **Bazel**: `buildifier` (4 spaces)
- **YAML/JSON/Markdown**: 2 spaces

Run `pre-commit run --all-files` before committing.

### Comments

- Comments describe the code and reasons for behavior, never history
- Never include transient information (bead references, phase plans, etc.)
- Avoid numbered lists in comments—they drift and confuse
- BAD: `// Replaced with Foo in Bar.go`
- BAD: `// To be completed in phase 2`
- BAD: `// See bead-1234`

### Error Handling

**NO SILENT FAILURES.** Fail loud and fail fast.

- Never use fallbacks or defaults that hide problems
- Never skip handling a field—either fail because it's unhandled or handle it completely
- Check for unsupported features explicitly

## Build System

Bureau uses Bazel for hermetic, reproducible builds.

```bash
# Build everything
bazel build //...

# Run all tests
bazel test //...

# Run a specific binary
bazel run //cmd/bureau

# Update Go dependencies
bazel run //:gazelle-update-repos
```

### Disk Cache

Bazel is configured with disk caching at `~/.cache/bazel-disk-cache`. It is never
a caching issue—if you think you have stale files after a successful build, you
are wrong. Never clean/expunge without explicit approval.

## Testing

- Tests are colocated with code (Go convention)
- Integration tests live in `tests/integration/`
- Use table-driven tests for Go
- Use pytest with asyncio for Python

## Beads

Bureau uses beads (`bead-*` prefix) for issue tracking within worktrees.

- Beads are agent-specific—for active task tracking only
- Cross-agent tasks belong in GitHub Issues
- Never reference beads in code comments or commit messages
- Close beads when work is complete, don't just mark them done

## Git Safety

You may be collaborating with humans or other agents in the same worktree.

- Never assume changes you didn't make are accidental
- Never "clean up" or "revert unrelated changes" without explicit instruction
- Never run destructive git commands without explicit user instruction:
  - `git stash`, `git checkout <file>`, `git restore`
  - `git reset`, `git clean`, `git rebase`
  - `git commit --amend`, `git push --force`

## Architecture Notes

### Channel Abstraction

The `pkg/channel` package defines an abstraction over communication platforms.
Matrix is the primary implementation, but the abstraction allows for:

- Mock implementations for testing
- Potential future backends (Discord, etc.)
- Offline operation for development

### Service Isolation

Go services (bureau-core, bureau-tools) are the critical infrastructure.
Python services (watchers, agents) are isolated and can fail without bringing
down the core. This isolation is intentional—don't break it.

### Tool Layers

Tools are organized by trust level:

- Layer 0: Pure functions (no side effects)
- Layer 1: Read-only external (observation)
- Layer 2: Workspace operations (contained side effects)
- Layer 3: Compute (sandboxed execution)
- Layer 4: External actions (world-changing, requires approval)
- Layer 5: System operations (Bureau infrastructure)

## Getting Help

- Check `docs/` for detailed documentation
- Read the README.md in relevant directories
- Ask in the appropriate Matrix channel
