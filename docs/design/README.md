# Bureau Design

Bureau is infrastructure for running organizations that include AI agents
— providing the same primitives an operating system provides to programs
(isolation, identity, communication, observation, persistence), but
designed for a world where most of your processes are autonomous and
untrusted.

A Bureau agent is anything with stdin/stdout that can HTTP to localhost.
No SDK, no framework dependency, no special libraries. The sandbox
filesystem is the API.

---

## Reading Guide

### Core Architecture

Start here. These documents define what Bureau is and how its pieces
compose into a running system.

- **[fundamentals.md](fundamentals.md)** — Why Bureau exists, what it is
  not, design principles, the five primitives, and the higher-order
  constructs built from them. The complete conceptual model — read this
  first.
- **[architecture.md](architecture.md)** — Runtime topology: launcher,
  daemon, proxy processes. IPC, sandbox contract, naming convention,
  service discovery.
- **[information-architecture.md](information-architecture.md)** — How
  data is organized in Matrix: spaces, rooms, threads, state events.
  The persistent data model underneath everything.

### Primitive Deep-Dives

Each primitive's full design. Read these when working on a specific
subsystem.

- **[credentials.md](credentials.md)** — age encryption, TPM/keyring
  integration, launcher/daemon privilege separation for secrets.
- **[observation.md](observation.md)** — Live terminal access: relay
  architecture, wire protocol, ring buffer, layouts, dashboard
  composition.
- **[pipelines.md](pipelines.md)** — Structured automation: step types
  (shell, publish, healthcheck, guard), variable substitution, thread
  logging.
- **[nix.md](nix.md)** — Build and distribution: Bazel compilation, Nix
  packaging, flake structure, binary cache, remote execution.

### Services

Systems built on the primitives. Each is a Bureau service principal with
a Matrix account and a unix socket API.

- **[tickets.md](tickets.md)** — Coordination primitive: Matrix-native
  work items with dependencies, gates, notes, artifact attachments.
- **[artifacts.md](artifacts.md)** — Content-addressable storage: BLAKE3
  hashing, CDC chunking, three-tier caching, FUSE mount, encryption.
- **[workspace.md](workspace.md)** — Git worktree lifecycle: shared bare
  repos, setup/teardown pipelines, sysadmin agent.
- **[fleet.md](fleet.md)** — Multi-machine management: service
  placement, machine lifecycle, HA, batch scheduling.
- **[authorization.md](authorization.md)** — Access control: grants,
  denials, allowances, temporal scoping, credential provisioning.
- **[stewardship.md](stewardship.md)** — Governance layer: resource
  ownership, tiered review escalation, workspace path stewardship
  (CODEOWNERS equivalent), notification batching, access escalation.
- **[telemetry.md](telemetry.md)** — Fleet-wide observability: trace
  collection, metric aggregation, log correlation, Prometheus
  integration.
- **[logging.md](logging.md)** — Raw output capture: PTY interposition,
  CAS-backed log chunks, log-\* mutable indices, pipeline ticket
  attachment, rotation, CLI tail/export.

- **[agent-context.md](agent-context.md)** — Agent understanding as a
  first-class artifact: context commits, delta chains, forking,
  materialization, format versioning, review discussion resumption.

### Applications

How Bureau uses itself. These describe patterns built from everything
above — concrete team structures, agent runtime integration, and
external service connectors.

- **[dev-team.md](dev-team.md)** — Bureau's self-hosted development
  team: role taxonomy, review pipeline, agent layering, coordination.
- **[agent-layering.md](agent-layering.md)** — Agent runtime
  integration: wrapper binaries, session management, conversation
  logging, MCP, quota.
- **[forgejo.md](forgejo.md)** — External service connector pattern:
  identity mapping, permission sync, event translation, webhook ingress.

---

## Document Conventions

Each design document follows this structure:

- **Preamble**: one paragraph stating the document's scope and its
  relationship to other documents (e.g., "FUNDAMENTALS.md defines the
  primitives; this document defines the runtime topology").
- **Main content**: the design itself — data models, protocols, APIs,
  deployment patterns. When alternatives were seriously considered,
  integrate the rationale into the relevant section — explain why the
  chosen approach works, not just what it is. Do not collect design
  decisions into a standalone section; separated rationale drifts from
  the content it justifies.
- **Relationship to Other Design Documents**: how this document connects
  to others. Not every document needs this (obvious connections like
  [architecture.md](architecture.md) → [fundamentals.md](fundamentals.md) are
  stated in the preamble), but documents with many touchpoints should have it.

### What Does Not Belong in Design Documents

- **Open questions.** If something is unresolved, file a ticket (or
  bead). Design documents describe decisions, not the absence of them.
  An open question in a document becomes invisible the moment someone
  stops reading that document — a ticket stays in the queue.
- **Transient references.** No bead IDs, no "as of Feb 2026", no
  "phase 2" language. Design documents are timeless descriptions of
  the target architecture. If the architecture changes, update the
  document.
- **Implementation status.** Design documents describe what the system
  *is*, not what has been built so far. A reader should not need to
  cross-reference the codebase to know whether a section is aspirational
  or implemented. If a section describes something not yet built, it
  still describes the design — the implementation is a separate concern.

### Maintenance

Design documents are code. They rot if not tended.

- **When you touch a subsystem, check its design document.** If the
  implementation has diverged, update the document or file a ticket
  explaining the divergence.
- **When you resolve a design question, update the document.** Move it
  from an open question (which should be a ticket) into the relevant
  section of the main content, with rationale integrated alongside the
  design it justifies.
- **Cross-references use document names, not paths.** Write
  "fundamentals.md" not "docs/design/fundamentals.md". Paths change;
  names are stable within this directory.
- **One document per concept.** Don't split a subsystem across multiple
  documents. Don't merge unrelated subsystems into one document.
