# Contributing to Bureau

Thank you for your interest in contributing to Bureau! This document provides
guidelines for contributing to the project.

## Code of Conduct

Be excellent to each other. Treat all contributors—human and AI—with respect.

## Timeless Code and Commits

All code, comments, and commit messages must be **timeless**: they describe
*what the code does and why*, never the process of creating it.

### Why This Matters

Git history is permanent. Anyone reading the code or history years from now
should understand it without knowing what issue tracking system was used,
what work items existed, or what phases were planned.

### Bead IDs Are Local State

We use beads (`bd`) for local workstream management. Bead IDs like `bead-abc123`
are **ephemeral local state** that must never appear in:

- Source code or comments
- Commit messages
- Documentation (except this file and `.pre-commit-config.yaml`)

Pre-commit hooks enforce this automatically.

### What NOT to Write

**In comments:**

```go
// BAD: References transient state
// Fixed per bead-1ar review feedback
// TODO(bead-xyz): Clean up later
// Phase 2 will add caching here
// Moved from old_file.go
```

**In commit messages:**

```text
BAD: Phase 1: Add path filtering
BAD: Part of bead-abc123 (some epic name)
BAD: WIP: checkpoint before meeting
```

### What TO Write

**In comments:**

```go
// GOOD: Describes what and why
// Use draft PRs to allow CI to run before review.
// TODO: Add retry logic for transient network failures.
```

**In commit messages:**

```text
GOOD: Add glob pattern filtering to bureau-worktree-list

Support filtering worktrees by path patterns like agent/* and
agent/alice/*. Multiple patterns use OR semantics.
```

### Pre-commit Hooks

Install both hook types:

```bash
pre-commit install
pre-commit install --hook-type commit-msg
```

The following hooks enforce these guidelines:

- `no-bead-references`: Blocks bead IDs in code files
- `no-bead-references-commit-msg`: Blocks bead IDs in commit messages

## Getting Started

### Prerequisites

- Go 1.23+
- Python 3.11+
- Bazel 8.x (via Bazelisk)
- Pre-commit

### Setup

```bash
# Clone the repository
git clone git@github.com:benvanik/bureau.git
cd bureau

# Install pre-commit hooks
pip install pre-commit
pre-commit install

# Verify the build works
bazel build //...
bazel test //...
```

## Development Workflow

### Branches

- `main` is the protected primary branch
- Feature branches: `feature/<name>`
- Bug fixes: `fix/<name>`
- Agent worktrees use their own branches

### Making Changes

1. Create a feature branch from `main`
2. Make your changes
3. Ensure pre-commit hooks pass: `pre-commit run --all-files`
4. Ensure tests pass: `bazel test //...`
5. Submit a pull request

### Commit Messages

Write clear, descriptive commit messages that are **timeless** (see above):

```text
Add channel abstraction for Matrix facade

Introduces the pkg/channel interface that abstracts over communication
platforms. Matrix is the primary implementation, but this allows for
mock implementations in tests and potential future backends.
```

- Use imperative mood ("Add" not "Added")
- First line is a summary (50 chars or less)
- Body explains what and why, not how
- Each commit should stand alone—no "Part of X" or "Phase N"
- Reference GitHub issues where relevant: `Fixes #123`
- Never reference bead IDs—they are local state

### Pull Requests

PRs should:

- Have a clear title and description
- Include tests for new functionality
- Pass all CI checks
- Have appropriate reviewers assigned

Use the PR template, which includes:

- Summary of changes
- Test plan
- Checklist of requirements

## Code Standards

### Naming Conventions

Use full words, not abbreviations:

- `length` not `len`
- `buffer` not `buf`
- `count` not `cnt` or `num`
- `position` not `pos`
- `string` (or more descriptive) not `str`

### Error Handling

- No silent failures—fail loud and fail fast
- Never use fallbacks or defaults that hide errors
- Always check for unsupported features explicitly
- If something is unhandled, it must fail, not silently pass

### Go

- Follow [Effective Go](https://go.dev/doc/effective_go)
- Use `gofmt` for formatting (enforced by pre-commit)
- Use `goimports` for import organization
- Write table-driven tests
- Handle all errors explicitly

### Python

- Follow PEP 8 (enforced by ruff)
- Use type hints
- Format with black (enforced by pre-commit)
- Use pytest for testing
- Use async/await for I/O operations

### Bazel

- Use buildifier for formatting (enforced by pre-commit)
- Keep BUILD files focused and readable
- Use `gazelle` for Go dependency management
- Document non-obvious build rules

### Documentation

- Every directory should have a README.md explaining its purpose
- Public APIs should have doc comments
- Complex logic should have inline comments explaining *why*

## File Headers

All source files must include the SPDX license header:

**Go:**

```go
// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0
```

**Python:**

```python
# Copyright 2026 The Bureau Authors
# SPDX-License-Identifier: Apache-2.0
```

**Bazel:**

```starlark
# Copyright 2026 The Bureau Authors
# SPDX-License-Identifier: Apache-2.0
```

## Testing

### Running Tests

```bash
# All tests
bazel test //...

# Specific package
bazel test //internal/core/...

# With verbose output
bazel test //... --test_output=all

# Python tests (outside Bazel)
cd services && pytest
```

### Writing Tests

- Tests should be deterministic and fast
- Use mocks for external dependencies
- Test edge cases and error conditions
- Aim for high coverage on critical paths

## Review Process

### For Humans

1. Submit PR with clear description
2. Address reviewer feedback
3. Ensure CI passes
4. Merge when approved

### For Agents

1. Create PR from your worktree branch
2. Request review from appropriate reviewer (human or agent)
3. Address feedback in new commits (don't amend)
4. Wait for approval before merging

## Security

- Never commit secrets, credentials, or API keys
- Use the credential vault for sensitive data
- Report security issues privately to maintainers
- Follow the principle of least privilege

## Questions?

- Check the documentation in `docs/`
- Open a GitHub issue for bugs or feature requests
- Use appropriate Matrix channels for discussion
