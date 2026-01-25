# Scripts

Utility scripts for development, deployment, and operations.

## Categories

### Development

- `bureau-worktree-init` — Initialize a new git worktree for agent work
- `bureau-worktree-deinit` — Clean up a worktree
- `setup-dev.sh` — One-time development environment setup

### Build

- Bazel wrapper scripts (see `bin/` or use bazel directly)

### Deployment

- `deploy.sh` — Deploy to an environment
- `rollback.sh` — Rollback a deployment

### Operations

- `backup.sh` — Manual backup trigger
- `health-check.sh` — Quick health verification

## Conventions

- Scripts should be idempotent where possible
- Use `set -euo pipefail` for bash scripts
- Document usage in script header comments
- Test scripts before committing

## Location

Frequently-used scripts may be symlinked or installed to `bin/` for easy access:

```bash
# After setup
bureau-worktree-init feature/my-feature
```
