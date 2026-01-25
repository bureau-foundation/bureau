# Documentation

Bureau documentation, organized for both humans and agents.

## Structure

### For Users

- `getting-started.md` — First-time setup guide
- `concepts.md` — Core concepts and vocabulary
- `operations.md` — Day-to-day operations guide

### For Developers

- `architecture.md` — System architecture overview
- `contributing.md` — Development guidelines (symlink to root)
- `style-guide.md` — Code style and conventions

### Reference

- `protocols.md` — Message formats and protocols
- `tools.md` — Available tools and capabilities
- `api/` — API documentation (generated)

## Agent-Friendly Documentation

Documentation should be:

- **Navigable**: Clear structure, cross-references
- **Precise**: Avoid ambiguity that confuses agents
- **Current**: Updated when code changes
- **Searchable**: Good headings and keywords

## Building Docs

```bash
# Generate API docs
bazel run //docs:generate

# Serve locally
bazel run //docs:serve
```
