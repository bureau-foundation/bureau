# Python Services

This directory contains Python services that complement the Go core.

## Services

### `watchers/`

External service watchers:

- Gmail watcher (polls for new emails)
- Calendar watcher (monitors upcoming events)
- GitHub watcher (PRs, issues, notifications)

Watchers are lightweight, long-running processes that:

- Poll external APIs on configurable intervals
- Post events to Matrix channels
- Handle rate limiting and backoff
- Recover gracefully from API failures

### `agents/`

Agent execution environment:

- Agent runner (manages LLM conversations)
- Front desk implementation (cheap, fast routing)
- Context management and refresh
- Tool call handling

### `shared/`

Shared Python utilities:

- Matrix client wrapper
- LLM client abstraction (LiteLLM)
- Configuration loading
- Logging setup

## Development

```bash
# Install dependencies
cd services
pip install -e ".[dev]"

# Run tests
pytest

# Run a specific watcher
python -m watchers.gmail
```

## Why Python?

Python services handle the rapidly-iterating, less-critical components:

- Fast to write and modify
- Rich ecosystem for API clients
- Sandboxed from core infrastructure
- Can be rewritten in Go once stable if needed

The isolation means a Python service crash doesn't affect bureau-core.
