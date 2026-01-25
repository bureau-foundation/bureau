# Claude Code Configuration

Configuration and customization for Claude Code when working on Bureau.

## Structure

### `skills/`

Custom skills for Bureau development:

- `commit` — Commit workflow with Bureau conventions
- `review-pr` — PR review with project context
- `create-agent` — Scaffold a new Bureau agent

### `styles/`

Agent personality definitions for different roles:

- `reviewer.md` — Code review persona
- `security.md` — Security-focused analysis
- `architect.md` — Architecture and design thinking

## Files

### `settings.json`

Project-specific Claude Code settings.

### `settings.local.json`

Local overrides (gitignored).

## Root CLAUDE.md

The main `CLAUDE.md` at the repository root contains:

- Project overview and architecture
- Code style and conventions
- Common patterns and anti-patterns
- Build and test instructions

This is the primary context for Claude when working on Bureau.
