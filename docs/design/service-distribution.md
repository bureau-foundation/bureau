# Service Distribution

[nix.md](nix.md) describes how Bureau compiles and packages its own binaries;
[architecture.md](architecture.md) defines the launcher/daemon/proxy process
topology that runs them. This document describes how services — both in the
Bureau monorepo and in independent repositories — are built, packaged,
delivered to machines, and kept up to date.

---

## Why Out-of-Tree Services

Bureau's monorepo contains the core primitives (sandbox, proxy, bridge,
observation, messaging) and a growing set of services (tickets, artifacts,
fleet controller, model service, telemetry). Each service is a standalone
Go binary that bootstraps via the proxy, registers on a service socket, and
participates in the Matrix event mesh.

Not every service belongs in the monorepo:

- **Connectors** (Discord, Slack, Jira, GitHub) couple to external APIs that
  evolve on their own cadence. Pinning their dependencies to the monorepo's
  release cycle creates friction in both directions.
- **Inference servers** (llama.cpp, whisper.cpp, TTS engines) are large C/C++
  codebases with GPU-specific build requirements. They do not share a build
  graph with Bureau's Go code.
- **Operator-specific services** (internal tools, custom workflows, domain
  agents) are authored by Bureau operators, not the Bureau team. Requiring
  them to fork the monorepo is not viable.
- **Experimentation.** A new service should be a new repository with a
  single `flake.nix` and a `main.go`, not a branch of a large monorepo
  that requires understanding the full build system.

The goal is: a Bureau service repository is small, self-contained, and
deployable with a single command. The service author needs Bureau's wire
protocol types and bootstrap library, not its sandbox implementation or
daemon internals.

---

## The Flake-as-Package Model

Every Bureau service — in-tree or out-of-tree — is distributed as a Nix
flake. The flake is the single source of versioned truth: it produces the
binary, declares the template, and pins all dependencies.

### Flake outputs

A Bureau service flake exports two well-known outputs:

- `packages.<system>.default` — the service binary. A static Go binary
  (or any executable) built by the flake's build system.
- `bureauTemplate.<system>` — a Nix attribute set describing the Bureau
  template for this service. The attribute names and structure mirror
  `TemplateContent` fields using camelCase Nix conventions.

```nix
{
  outputs = { self, nixpkgs, bureau-sdk, ... }: let
    system = "x86_64-linux";
    pkgs = nixpkgs.legacyPackages.${system};
  in {
    packages.${system}.default = pkgs.buildGoModule {
      pname = "bureau-discord";
      version = "0.3.0";
      src = ./.;
      vendorHash = "sha256-...";
    };

    bureauTemplate.${system} = {
      inherits = ["service-base"];
      command = ["${self.packages.${system}.default}/bin/bureau-discord"];
      required_credentials = ["DISCORD_BOT_TOKEN"];
      proxy_services.discord = {
        upstream = "https://discord.com/api/v10";
        inject_headers.authorization = "Bot \${DISCORD_BOT_TOKEN}";
      };
      required_services = ["ticket"];
      health_check = { type = "socket"; action = "health"; };
    };
  };
}
```

The `bureauTemplate` output uses the same field names as the JSON wire
format (`snake_case`). This means `nix eval --json` produces output that
is directly parseable as a `TemplateContent` struct — no field name
mapping is needed. Nix string interpolation (`${self.packages...}`)
resolves store paths during evaluation, so the command field contains
the full `/nix/store/...` path.

The `command` field references the binary's Nix store path directly. Because
the flake is evaluated as a unit, the template and binary are always in
sync — there is no version skew between "template says command is X" and
"binary at path X is version Y."

### Multi-architecture

Flakes produce per-system outputs. A service that runs on both x86_64 and
aarch64 exports both `packages.x86_64-linux.default` and
`packages.aarch64-linux.default`, with corresponding
`bureauTemplate.x86_64-linux` and `bureauTemplate.aarch64-linux`.

The `bureau template publish --flake` command evaluates the flake for the
specified system (defaulting to the current machine's architecture). For
fleets with mixed architectures, the operator publishes once per
architecture, and the daemon selects the template matching its machine.

### Non-Go services

Services that are not Go programs — a llama.cpp inference server, a Python
connector — follow the same model. The flake builds whatever the service
needs and exports the binary (or wrapper script) and template:

```nix
{
  bureauTemplate.x86_64-linux = {
    inherits = ["service-base"];
    command = ["${self.packages.x86_64-linux.default}/bin/llama-server"];
    environment = "${self.packages.x86_64-linux.runtime-env}";
    resources = { memory_limit_mb = 32768; };
  };
}
```

The entrypoint reads `/run/bureau/payload.json` for configuration (model
path, quantization, context size) and exposes an HTTP endpoint. Bureau
routes traffic through proxy service discovery. The service does not need
to import any Bureau code — it interacts entirely through documented
filesystem and HTTP contracts.

---

## Template Publishing and Updates

### Publishing from a flake

```
bureau template publish --flake github:bureau-foundation/bureau-discord/v1.2.0
```

This command:

1. Evaluates the flake for the target system.
2. Reads the `bureauTemplate.<system>` output.
3. Converts the Nix attribute set to a `TemplateContent` struct.
4. Records the origin: flake reference, resolved git revision, content hash.
5. Publishes an `m.bureau.template` state event to the template room.

The template state event's content includes an `origin` field:

```json
{
  "description": "Discord connector service",
  "command": ["/nix/store/abc-bureau-discord-0.3.0/bin/bureau-discord"],
  "origin": {
    "flake_ref": "github:bureau-foundation/bureau-discord/v1.2.0",
    "resolved_rev": "a1b2c3d4e5f6...",
    "content_hash": "sha256:..."
  }
}
```

The `origin` field is metadata only — it does not affect template resolution
or sandbox creation. It exists so that `bureau template update` can find and
fetch newer versions.

### Publishing from other sources

Templates can also be published from local files or URLs:

```
bureau template publish --file ./template.jsonc
bureau template publish --url https://raw.githubusercontent.com/.../template.jsonc
```

The `--file` path works as `bureau template push` does today. The `--url`
path fetches the content, validates it, and publishes with the URL recorded
as origin. Both paths produce templates without Nix store paths in the
command — the operator is responsible for ensuring the binary is available
on target machines.

The `--flake` path is the recommended method for Bureau-native services
because it produces a complete, self-contained template with resolved
binary paths.

### Checking for updates

```
bureau template update [--dry-run]
```

Iterates all templates in the template room that have an `origin` field:

- **Flake origins:** re-evaluates the flake reference (fetches latest from
  the remote). If the resolved revision or content hash differs from the
  published template, reports the difference. With `--dry-run`, shows the
  diff without publishing. Without `--dry-run`, prompts for confirmation
  before publishing each update.

- **URL origins:** re-fetches the URL. If the content hash differs, reports
  the change.

For flake origins that use a version tag (`/v1.2.0`), the update command
does not automatically advance to a newer tag — that would change the flake
reference itself. The operator explicitly publishes with the new tag. For
flake origins that track a branch (`/main`), the update command re-evaluates
against the branch HEAD and reports any changes.

### Binary delivery

When the daemon processes a template (via /sync), the template's `command`
and `environment` fields may reference Nix store paths that are not yet on
the local machine. The daemon prefetches these paths from configured binary
caches before creating a sandbox that uses them.

Each service repository's CI pushes its Nix build outputs to a binary cache.
Bureau machines list trusted caches in their Nix configuration. The trust
model is explicit: an operator chooses which caches to trust, and Nix
verifies signatures on cached paths.

For Bureau-foundation services, a shared binary cache serves all official
service builds. Community and operator-specific services publish to their
own caches (cachix, self-hosted, or any HTTP-accessible Nix binary cache).

---

## The Go SDK

Bureau-native services (Go, CBOR socket protocol) import a Go SDK that
provides the service bootstrap, socket server, schema types, and Matrix
client. The SDK is a subset of the monorepo — the packages a service needs
at runtime, without the packages that manage service lifecycles (daemon,
launcher, sandbox).

### SDK boundary

The SDK contains:

| Package | Purpose |
|---------|---------|
| `clock` | Time abstraction, fake clock for testing |
| `codec` | CBOR encoder configuration (deterministic encoding) |
| `ref` | Identity types: Namespace, Fleet, Machine, Service, Agent, UserID, RoomID |
| `schema` | Wire types: all Matrix state event content structs, sub-packages per domain |
| `secret` | Guarded memory for sensitive material |
| `netutil` | HTTP response helpers, bounded reads |
| `cron` | Cron expression parsing (used by ticket gates) |
| `servicetoken` | Ed25519 service token creation and verification |
| `principal` | Localpart validation, socket path computation |
| `proxyclient` | HTTP client for the Bureau proxy API |
| `messaging` | Matrix client-server API: Client (unauthenticated) + Session (authenticated) |
| `service` | Bootstrap, socket server, sync loop, registration, health reporting |
| `testutil` | Test helpers: SocketDir, DataBinary |

The SDK does not contain:

- Sandbox creation, bwrap, namespace management
- Proxy implementation (services are consumers, not implementors)
- Launcher, daemon internals, IPC protocol
- Pipeline executor, workspace management
- Observation relay, tmux management
- Artifact store engine (services use the artifact service, not the store)
- Bridge implementation

The boundary principle: **the SDK contains everything a service process
needs at runtime.** It does not contain anything about how that process
gets created, deployed, or managed.

### Extraction mechanism

The SDK packages live in the monorepo at their canonical locations
(`lib/service/`, `lib/schema/`, `messaging/`, etc.). A script
(`script/sync-sdk`) copies these packages into an `sdk/` directory with
a separate `go.mod`, rewriting import paths from
`github.com/bureau-foundation/bureau/lib/<pkg>` to
`github.com/bureau-foundation/bureau/sdk/<pkg>`.

The `sdk/` directory is committed to the repository. External consumers
import `github.com/bureau-foundation/bureau/sdk` and get only the SDK
dependencies. During monorepo development, a `go.work` file resolves
the SDK module locally.

A manifest file (`sdk/PACKAGES`) lists the source-to-SDK package
mappings. Adding a package to the SDK is adding a line to this file and
running the sync script. Lefthook runs the sync as a pre-commit hook,
and CI verifies the `sdk/` directory matches the source packages.

### Boundary enforcement

A CI check (`script/check-sdk-boundary`) parses the imports of every
SDK-eligible package and verifies they only import other SDK packages,
the standard library, or approved third-party modules. If an SDK package
gains an import of a non-SDK package (e.g., `sandbox/` or `observe/`),
CI fails. This prevents the clean cut from degrading over time.

---

## The Service Template Repository

A Bureau Go service repository follows a standard structure:

```
bureau-discord/
  flake.nix           Nix packaging: binary + bureauTemplate output
  flake.lock          Pinned dependencies
  MODULE.bazel        Bazel module definition
  BUILD.bazel         Bazel build targets (generated by gazelle)
  go.mod              Go module, imports bureau SDK
  go.sum
  main.go             Service entry point
  handlers.go         Socket action handlers
  handlers_test.go    Tests
  .lefthook.yml       Pre-commit hooks (from shared conventions)
```

The `go.mod` imports the SDK, not the full monorepo:

```
module github.com/bureau-foundation/bureau-discord

require (
    github.com/bureau-foundation/bureau/sdk v0.3.0
)
```

The `flake.nix` uses `gomod2nix` (matching the monorepo pattern) for
hermetic Go builds, exports the binary and template, and optionally
provides a dev shell with Bazel, Go, and Bureau tools.

### Bazel

Service repositories use Bazel for compilation. This gives them access
to Buildbarn remote execution and caching — the same infrastructure the
monorepo uses. Gazelle generates BUILD files from Go source. The Bazel
configuration is minimal: `MODULE.bazel` declares the Go SDK dependency
and rules_go/gazelle versions.

For iterative development, `bazel build //:bureau-discord` compiles
the service. For testing, `bazel test //...` runs unit tests. Integration
tests that need a running Bureau stack are run against a dev deployment.

### CI

Service repositories use GitHub Actions (or equivalent) with two jobs:

- **Build and test:** Bazel build + test in a Nix dev shell. Pushes
  build outputs to the binary cache on success.
- **SDK compatibility:** Verifies the service builds against the latest
  SDK release and the SDK's main branch. Catches breaking changes early.

### Shared conventions

Bureau service repositories share pre-commit hooks, linting rules, and
style enforcement through a conventions repository
(`bureau-foundation/bureau-conventions`). This repository contains:

- Lefthook configuration for Go services (gofmt, go vet, SPDX headers,
  buildifier)
- golangci-lint configuration
- SPDX header templates
- CI workflow templates

Service repositories reference the conventions via a Nix flake input
or git submodule, keeping them in sync without copy-paste.

---

## Service Protocol Tiers

Bureau supports two levels of service integration:

### CBOR-native services (Bureau SDK, Go)

The primary integration path. Services import the Go SDK, bootstrap via
proxy, register CBOR socket actions, and participate fully in the Matrix
event mesh. This path provides:

- Type-safe request/response with deterministic CBOR encoding
- Ed25519 service token verification (stateless, no daemon round-trip)
- Incremental sync loop for event-driven state updates
- Health reporting with degradation signaling
- Telemetry instrumentation (per-action spans)

All Bureau-authored services use this path.

### HTTP services (any language)

Services that expose HTTP endpoints can integrate through Bureau's proxy
service discovery without importing any Bureau code. The service:

1. Listens on a Unix socket at the path specified by
   `BUREAU_SERVICE_SOCKET`.
2. Reads identity and configuration from `/run/bureau/payload.json` and
   the `BUREAU_PRINCIPAL_NAME` environment variable.
3. Receives requests with a `Bureau-Token` header containing a signed
   service token.
4. Validates the token (by calling a validation endpoint on the proxy,
   or by implementing Ed25519 verification locally).

This path is suitable for non-Go services, services wrapping existing
HTTP APIs, and rapid prototyping. It trades the performance and type
safety of CBOR sockets for universal language compatibility.

---

## Version Stability

The contracts that external services depend on, ordered by stability:

**Filesystem contracts** — paths like `/run/bureau/proxy.sock`,
`/run/bureau/service/<role>.sock`, `/run/bureau/payload.json`, and
environment variables like `BUREAU_PROXY_SOCKET`. These are extremely
stable. Changing them breaks everything simultaneously.

**Proxy HTTP API** — versioned URLs (`/v1/identity`, `/v1/grants`,
`/v1/services`). Standard HTTP API versioning. New endpoints get new
version prefixes.

**Service socket envelope** — the CBOR request/response format:
`{action, token, ...}` → `{ok, error, data}`. The envelope is simple
and unlikely to change. Individual action request/response shapes evolve
with their respective services.

**Schema types** — Matrix state event content structs. Each type carries
a `version` field and a `CanModify()` method that rejects unknown future
versions. A service built against schema version N works with daemons
running version N+1 as long as it does not encounter events with version
> N. Forward compatibility is handled: unknown fields are preserved
through round-trips by the JSON and CBOR codecs.

**Go SDK API** — the bootstrap, socket server, and sync loop interfaces.
These change more frequently than wire protocols as ergonomics improve.
The SDK follows semver: breaking changes require a major version bump.
The SDK version and the wire protocol version are independent — an SDK
update that only changes helper functions does not require redeploying
services.

The stability principle: **the wire protocol is the compatibility
boundary, not the Go module version.** If a service speaks the right
CBOR over the right socket, it works regardless of which SDK version
built it.

---

## Relationship to Other Documents

- [nix.md](nix.md) describes the build and packaging system that this
  document's distribution model builds on. The flake-as-package model
  extends the patterns established there (per-binary derivations,
  gomod2nix, binary cache) to out-of-tree services.
- [architecture.md](architecture.md) defines the daemon's role in
  prefetching store paths and creating sandboxes. Template publishing
  feeds into the daemon's existing reconciliation loop.
- [fleet.md](fleet.md) describes service placement across machines.
  Out-of-tree services participate in fleet management identically to
  in-tree services — the fleet controller sees templates, not source
  repositories.
- [authorization.md](authorization.md) covers the grant model that
  service tokens carry. The SDK's token verification implements the
  authorization checks described there.
