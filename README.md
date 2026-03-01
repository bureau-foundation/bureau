# Bureau

A workshop for software: organizational infrastructure that lets
individuals direct AI agent teams — with coordination, quality gates,
workstream management, and structural guardrails — on their own
machines. The barrier to entry is craft, not capital.

**[Build Your Own Workshop — why this exists and what it means](docs/manifesto.md)**

> Bureau is under active development by one person. This repository is
> a peek into a working workshop, not yet a blueprint for building your
> own. The code builds, the tests pass, and the system runs — but the
> path from `git clone` to running your own Bureau doesn't exist yet
> outside of the author's head. If you're here, you're looking at the
> shop floor, not the showroom.

---

Bureau provides isolation, identity, communication, observation, and
persistence for autonomous processes — the same primitives an operating
system provides to programs, designed for a world where most of your
processes are untrusted.

A Bureau agent is any process that can make HTTP requests to a Unix
socket. Claude Code, Codex, Gemini, a Python script, or `curl` — they
are all first-class participants. The infrastructure doesn't distinguish
between agents, services, and human operators. They are all principals.

## Five Primitives

Everything Bureau does is a composition of five things:

| Primitive | What it does | Implementation |
|---|---|---|
| **Sandbox** | Isolated execution for untrusted code | bubblewrap, Linux namespaces, cgroups |
| **Proxy** | Credential-isolated API and tool access | Per-sandbox HTTP server on Unix socket |
| **Observation** | Live bidirectional terminal access | tmux outside sandbox, PTY fd inheritance |
| **Connectivity** | Cross-machine service routing | WebRTC (pion), Matrix signaling |
| **Messaging** | Persistent structured communication | Matrix (Continuwuity homeserver) |

An "agent" is a process inside a sandbox, talking to the world through
a proxy, observable via a terminal session, reachable through
connectivity, and coordinating with others through messaging.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  Machine: @machine/workstation:bureau.local                      │
│                                                                  │
│  bureau-launcher           bureau-daemon                         │
│  (privileged, no network)  (unprivileged, network-facing)        │
│  Namespace creation        Matrix sync, WebRTC transport         │
│  Credential decryption     Service discovery, lifecycle          │
│  Sandbox lifecycle         Machine presence, observation         │
│                                                                  │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐            │
│  │ Proxy        │  │ Proxy        │  │ Proxy        │            │
│  │ project/pm   │  │ project/dev  │  │ service/stt  │            │
│  │ Matrix token │  │ Matrix token │  │ Matrix token │            │
│  │ + OpenAI key │  │ + GitHub PAT │  │ (no ext keys)│            │
│  ├──────────────┤  ├──────────────┤  ├──────────────┤            │
│  │ Sandbox      │  │ Sandbox      │  │ Sandbox      │            │
│  │ LLM agent    │  │ Coding agent │  │ Whisper STT  │            │
│  │ /proxy.sock  │  │ /proxy.sock  │  │ /proxy.sock  │            │
│  └──────────────┘  └──────────────┘  └──────────────┘            │
└──────────────────────────────────────────────────────────────────┘
```

Three binaries, not one. The launcher (privileged) creates namespaces
and decrypts credentials but has zero network exposure. The daemon
(unprivileged) faces the network but cannot create namespaces or
decrypt credentials. Each proxy holds credentials for exactly one
principal. A compromised sandbox can only use what its proxy holds.

## Naming

Matrix user IDs use hierarchical localparts that map 1:1 to filesystem
paths, room aliases, socket paths, and permission globs:

```
@iree/amdgpu/pm:bureau.local      → /run/bureau/iree/amdgpu/pm.sock
#iree/amdgpu/general:bureau.local → project room
iree/**                           → permission glob
```

`ls` and shell globs become discovery tools. No mapping tables, no
encoding, no translation bugs.

## Repository Layout

```
sandbox/       Primitive: bubblewrap namespace isolation
proxy/         Primitive: credential injection via HTTP on Unix socket
bridge/        Primitive: TCP-to-Unix socket forwarder for sandboxed HTTP
transport/     Primitive: cross-machine WebRTC service routing
observe/       Primitive: PTY relay, ring buffer, tmux integration
messaging/     Primitive: Matrix client-server API (Client + Session)

lib/           Supporting packages
  agentdriver/   Agent process runtime (spawning, output parsing, session logging)
  artifactstore/ CAS engine (BLAKE3, CDC chunking, compression, FUSE mount)
  authorization/ Grant/denial evaluation for Matrix operations
  bootstrap/     First-boot homeserver setup helpers
  config/        YAML configuration loading
  content/       Embedded pipeline definitions and templates
  credential/    Credential management and provisioning
  git/           Git worktree operations for workspace lifecycle
  hwinfo/        Hardware fingerprinting (CPU, GPU, memory)
  llm/           LLM client abstraction and context management
  nix/           Nix store path and environment resolution
  pipelinedef/   JSONC pipeline parsing, variable substitution, validation
  principal/     Localpart validation, Matrix ID construction, socket paths
  proxyclient/   HTTP client for proxy Unix sockets
  schema/        Bureau protocol types (Matrix state event content structs)
    agent/         Agent session, context, metrics events
    artifact/      Artifact scope configuration events
    fleet/         Fleet service, machine, HA lease, alert events
    observation/   Layout, window, pane events
    pipeline/      Pipeline definition, step, result events
    telemetry/     Telemetry data types (spans, metrics, logs)
    ticket/        Ticket content, gate, note, attachment events
    workspace/     Workspace state, worktree, project config events
  sealed/        age encryption/decryption for credential bundles
  secret/        Guarded memory for secret material (zero-on-close)
  service/       Service registration and discovery
  templatedef/   Template resolution, inheritance, merge logic
  ticketindex/   In-memory ticket index (readiness, search, dependencies)
  ticketui/      Terminal UI components for ticket display
  tmux/          tmux server/session management
  toolsearch/    MCP tool discovery and matching
  version/       Build version info
  watchdog/      Binary self-update detection

cmd/           Binaries
  bureau/                Unified CLI (agent, artifact, credential, environment,
                         fleet, machine, matrix, mcp, observe, pipeline,
                         service, template, ticket, workspace)
  bureau-launcher/       Privileged: keypair, credential decryption, sandbox lifecycle
  bureau-daemon/         Unprivileged: Matrix sync, config reconciliation, routing
  bureau-proxy/          Per-sandbox credential injection proxy
  bureau-bridge/         TCP-to-Unix socket bridge
  bureau-sandbox/        Sandbox creation
  bureau-credentials/    Credential provisioning and machine config
  bureau-observe-relay/  Observation relay (PTY allocation, ring buffer)
  bureau-agent/          Generic agent wrapper (any LLM backend)
  bureau-agent-claude/   Claude Code agent wrapper
  bureau-agent-service/  Agent lifecycle service
  bureau-artifact-service/ Content-addressable artifact storage service
  bureau-ticket-service/ Ticket index and query service
  bureau-fleet-controller/ Multi-machine fleet management
  bureau-pipeline-executor/ Sandboxed pipeline step sequencing
  bureau-telemetry-relay/  Per-machine telemetry collection
  bureau-telemetry-service/ Fleet-wide telemetry aggregation
  bureau-log-relay/      Raw output capture and CAS storage
  bureau-proxy-call/     One-shot HTTP request through a proxy socket
  bureau-state-check/    State consistency validation

integration/   End-to-end tests (real homeserver, full daemon+launcher stack)
docs/          Documentation and design documents
deploy/        Deployment (Matrix homeserver, Buildbarn, systemd)
script/        Dev environment setup (Nix installer, version pins)
```

## Design Documents

Many design documents describe the target architecture. Start with the
core architecture, then read subsystem deep-dives as needed.

See **[docs/design/README.md](docs/design/README.md)** for the full
reading guide.

**Core**: fundamentals.md (five primitives, design principles),
architecture.md (runtime topology), information-architecture.md
(Matrix data model).

**Primitives**: credentials.md, observation.md, pipelines.md, nix.md.

**Services**: tickets.md, artifacts.md, workspace.md, fleet.md,
authorization.md, stewardship.md, telemetry.md, logging.md,
agent-context.md.

**Applications**: dev-team.md (self-hosted team structure),
agent-layering.md (agent runtime integration), forges.md (forge
connectors: GitHub, Forgejo, GitLab).

## Building

Bazel is the build system. All builds and tests go through Bazel.

```bash
bazel build //...
bazel test //...
```

After adding or removing Go files:

```bash
bazel run //:gazelle
```

Integration tests require Docker (they spin up a real Continuwuity
homeserver):

```bash
sg docker -c "bazel test //integration:integration_test"
```

## Development Setup

Nix provides the hermetic dev environment. All tools (Go, Bazel,
gazelle, buildifier, tmux, etc.) come from the Nix dev shell.

```bash
script/setup-nix         # install Nix (requires sudo)
script/setup-nix --check # verify
nix develop              # enter dev shell
```

## Status

Bureau is implemented in Go. All packages build and all tests pass
(77 unit test targets, 31 integration tests against a real homeserver).

**Working**: all five primitives (sandbox, proxy, bridge, transport,
observation), messaging, launcher and daemon (full lifecycle, IPC,
self-update), CLI with 15 command groups, protocol schema (all event
types across 8 domains), templates (resolution, inheritance, merge),
pipelines (definition, executor, thread logging), workspaces (git
worktrees, setup/teardown pipelines), tickets (service, index, query,
terminal UI), artifacts (CAS, CDC chunking, FUSE mount), fleet
controller (multi-machine, health monitoring, service placement),
telemetry (relay, service, Prometheus integration), agent wrappers
(Claude, generic), authorization (grants, denials, temporal scoping),
credentials (age encryption, TPM/keyring), Nix packaging, Buildbarn
remote execution.

**Designed**: real-time channels (audio/video/data via WebRTC),
service gateways (external service connectors), stewardship
(governance and review escalation).

## License

SPDX license headers on all source files.
