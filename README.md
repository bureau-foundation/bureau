# Bureau

> *A well-designed bureaucracy is a machine for making good decisions consistently.*

Bureau is a personal operations system built on AI agents. It manages your life's complexity through a team of specialized agents who coordinate, delegate, and escalate—just like a well-run organization.

## Philosophy

- **Agents as staff, not tools**: Each agent has a defined role, limited authority, and clear reporting lines
- **Async-first**: Synchronous interaction only when truly needed; everything else flows through queues
- **Observable**: All agent activity visible in chat logs; audit trail by design
- **Local-first**: Your data stays on your infrastructure; external APIs are tools, not dependencies
- **Capability-based security**: Agents can only do what they're explicitly allowed to do

## Status

🚧 **Early Development** — Building the foundation.

## Architecture

```text
┌─────────────────────────────────────────────────────────────────────┐
│                              YOU                                     │
│                         (The Boss)                                   │
└─────────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────────┐
│                        CHIEF OF STAFF                                │
│              Routes, delegates, escalates, summarizes                │
└─────────────────────────────────────────────────────────────────────┘
                                │
                ┌───────────────┼───────────────┐
                ▼               ▼               ▼
         ┌──────────┐    ┌──────────┐    ┌──────────┐
         │ Personal │    │   Work   │    │  System  │
         │  Domain  │    │  Domain  │    │  Agents  │
         └──────────┘    └──────────┘    └──────────┘
```

Agents communicate via Matrix, providing:

- Persistent, searchable history
- Threads for focused work
- Reactions for quick acknowledgments
- Room state for live data
- End-to-end encryption for sensitive domains
- Federation for future expansion

## Repository Structure

```text
bureau/
├── cmd/                    # Go binaries (bureau, bureau-core, etc.)
├── internal/               # Internal Go packages
├── pkg/                    # Public Go packages
├── services/               # Python services (watchers, agent runner)
├── config/                 # Configuration templates
├── docs/                   # Documentation
├── tests/                  # Test suites
├── scripts/                # Utility scripts
├── deployments/            # Deployment configurations
└── .claude/                # Claude Code configuration and skills
```

## Development

### Prerequisites

- Go 1.22+
- Python 3.11+
- Bazel 7.x+ (via Bazelisk recommended)
- A Matrix account for testing

### Quick Start

```bash
# Clone
git clone git@github.com:benvanik/bureau.git
cd bureau

# Install pre-commit hooks
pip install pre-commit
pre-commit install

# Build everything
bazel build //...

# Run tests
bazel test //...
```

### Development Workflow

This project uses:

- **Bazel** for hermetic, reproducible builds
- **Pre-commit** for automated formatting and linting
- **GitHub Actions** for CI
- **Beads** (`bead-*` prefix) for issue tracking within worktrees

See [CONTRIBUTING.md](CONTRIBUTING.md) for detailed development guidelines.

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.

## Acknowledgments

Bureau is built with assistance from Claude, Anthropic's AI assistant, as a collaboration partner in both design and implementation.
