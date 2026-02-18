# Bureau

Infrastructure for running organizations that include AI agents.

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
sandbox/       Sandbox: bubblewrap namespace isolation
proxy/         Proxy: credential injection via HTTP on Unix socket
bridge/        Bridge: TCP-to-Unix socket forwarder for sandboxed HTTP
transport/     Connectivity: cross-machine WebRTC service routing
observe/       Observation: PTY relay, ring buffer, tmux integration
messaging/     Messaging: Matrix client-server API (Client + Session)
lib/           Supporting packages
  config/        YAML configuration loading
  principal/     Localpart validation, Matrix ID construction
  schema/        Bureau protocol (Matrix state event types)
  sealed/        age encryption/decryption for credential bundles
  secret/        In-memory secret handling
  version/       Build version info
  watchdog/      Binary update success/failure detection
  testutil/      Test helpers (socket dirs, runfiles)
cmd/           Binaries
  bureau/              Unified CLI (observe, matrix, dashboard, environment)
  bureau-launcher/     Privileged: keypair, credentials, sandbox lifecycle
  bureau-daemon/       Unprivileged: Matrix sync, config, service routing
  bureau-proxy/        Per-sandbox credential injection
  bureau-bridge/       TCP-to-Unix socket bridge
  bureau-sandbox/      Sandbox creation
  bureau-credentials/  Credential provisioning
  bureau-proxy-call/   One-shot proxied HTTP request
deploy/        Deployment (Matrix homeserver, Buildbarn)
script/        Dev setup (Nix installer, version pins)
```

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

## Development Setup

Nix provides the hermetic dev environment. All tools (Go, Bazel,
gazelle, buildifier, tmux, etc.) come from the Nix dev shell.

```bash
script/setup-nix        # install Nix (requires sudo)
script/setup-nix --check # verify
nix develop              # enter dev shell
```

## Status

Bureau is implemented in Go. All packages build and all tests pass.

**Working**: sandbox isolation, proxy credential injection, bridge,
observation (relay, dashboard, layouts), messaging library, launcher
(IPC, lifecycle, credential decryption, self-update), daemon (Matrix
sync, config reconciliation, health monitoring, self-update), CLI,
protocol schema, naming/validation, WebRTC transport, Bazel build,
Nix packaging, Buildbarn remote execution.

**In progress**: templates (schema done, daemon resolution in progress),
workspaces (convention defined, commands partial), pipelines (designed,
executor pending).

**Designed**: real-time channels (audio/video/data), LiveKit SFU, SSH
certificates with on-demand tunnels.

## License

SPDX license headers on all source files.
