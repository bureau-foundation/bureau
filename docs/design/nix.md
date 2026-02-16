# Build and Distribution

[fundamentals.md](fundamentals.md) establishes that Bureau's primitives
compose into a running system; [architecture.md](architecture.md)
defines the launcher/daemon/proxy process topology. This document
describes the build system that compiles Bureau, the packaging system
that distributes it, and the update mechanisms that keep it running
across machines — the layers beneath the runtime.

---

## The Stack

```
Matrix          runtime config, state, credentials, messaging
  daemon          reads Matrix config, reconciles services and sandboxes
    launcher        privilege separation, credential decryption, process lifecycle
      systemd         starts launcher + daemon, restarts on failure
        Nix             hermetic environments, content-addressed binary distribution
          Bazel           compilation, per-target dependency tracking, remote execution
```

Each layer does one thing:

- **Bazel** knows which source files affect which binary. It compiles
  only what changed. It shares compilation across agents, machines, and
  CI through Buildbarn (remote execution and caching). It handles Go,
  and eventually other languages for services.

- **Nix** provides hermetic environments (dev shells, sandbox runtimes)
  and binary distribution. Every binary and environment closure lives in
  `/nix/store/`. The binary cache is required infrastructure.

- **systemd** keeps the launcher and daemon alive. Every other process
  is managed by the launcher, not by systemd.

- **The daemon** is the service manager. It reads MachineConfig from
  Matrix, prefetches store paths from the binary cache, starts/stops
  sandboxes, monitors health, and performs rolling updates. Config
  changes arrive via Matrix sync in real-time.

- **Matrix** is the configuration store, the messaging layer, and the
  audit trail. MachineConfig state events describe the desired state of
  each machine.

---

## Bazel

### Why Bazel

`go build` has no concept of per-file dependencies at the package level.
Change any `.go` file and it recompiles everything downstream. It has no
remote cache and no remote execution. Its output locations are
uncontrollable without explicit `-o` flags. Its module system makes
reproducible builds difficult without vendoring.

Bazel tracks per-file dependencies through its action graph. Change
`proxy/handler.go` and Bazel recompiles `bureau-proxy` and nothing else
(assuming nothing else imports `proxy/`). The result is cached
content-addressably: same inputs produce the same output and the same
cache key. This granularity is what makes "change one line, only one
binary changes" possible.

### Build Configuration

Bazel uses bzlmod for dependency management (`MODULE.bazel`). The Go
SDK version is read from `go.mod` so the Bazel build and direct Go
tooling always agree. Static linking with `CGO_ENABLED=0` is the
default.

`.bazelrc` configures three modes:

- **Default** — strict action environment, static Go linking.
- **CI** — disk cache for CI persistence.
- **Remote** — Buildbarn remote execution with extended PATH for
  remote test actions to find Nix-provided tools in the runner
  container.

BUILD files are maintained by gazelle and formatted by buildifier.
Adding a new package or import is: edit Go code, run `gazelle`, commit.

### Buildbarn: Remote Execution and Caching

Buildbarn provides both remote caching and remote execution.

**Remote caching** means "build locally, share the result." That helps
when the same target has already been built, but when 20 agents on the
same machine each need to build different targets, they all compile
locally at full parallelism and oversubscribe the machine. **Remote
execution** means agents send build actions to Buildbarn's scheduler,
which queues them and dispatches to workers with controlled parallelism.
One machine with 64 cores serves 20 agents without any of them
competing for CPU.

This is also what makes cross-machine builds work: an agent on a cloud
VM with 1 CPU sends its build actions to the execution cluster. The
cluster compiles at full speed, results flow back through the cache. The
agent never runs a compiler.

The Buildbarn deployment (`deploy/buildbarn/`) has four components:
content-addressable storage (CAS and action cache), a scheduler (action
queue and dispatch), workers (execution environment setup), and runners
(actual command execution). Runners have `/nix/store` bind-mounted
read-only and a Bureau runner environment at `/usr/local/bin` for test
toolchain access. Runners have no network access — build inputs and the
Go SDK arrive via CAS, not via network downloads.

Agents in sandboxes access Buildbarn through the proxy socket. The Bazel
client in the sandbox is configured with `--remote_executor` pointing to
the scheduler endpoint (routed through the proxy). Build actions leave
the sandbox, execute on the cluster, and results come back through the
CAS. The agent never needs a Go compiler, a C toolchain, or any build
dependencies beyond the Bazel client itself.

---

## Nix

### What Nix Does

- **Dev shell** — hermetic development environment with everything
  needed to build, test, and develop Bureau. `nix develop` enters the
  shell. No global installs, no version conflicts. The Go version is
  pinned to match `go.mod` so dev shell builds and Bazel builds use
  the same compiler.

- **Binary packaging** — each Bureau binary gets its own Nix derivation.
  The daemon compares binary content hashes to determine what actually
  changed and needs restarting.

- **Binary cache** — machines pull pre-built binaries and environment
  closures from the Bureau binary cache (R2-backed). The flake declares
  it as an extra substituter so `nix build` and `nix-store --realise`
  fetch from it automatically.

- **Sandbox environments** — Nix provides the runtime closure for
  sandboxed agents and services. Each environment is a `buildEnv`
  derivation containing the tools that sandbox needs. Environments are
  defined and deployed independently of Bureau core.

- **Machine bootstrap** — a new machine installs Nix, configures the
  binary cache, pulls Bureau binaries, writes two systemd units, and
  starts the launcher and daemon. From that point on, Matrix drives
  everything.

### What Nix Does Not Do

- Runtime configuration (Matrix state events)
- Secrets management (age encryption + launcher)
- Service orchestration (daemon reconciliation)
- Process supervision beyond launcher and daemon (systemd for those two,
  launcher for everything else)
- Compilation (Bazel, via Buildbarn)

### Per-Binary Derivations

Each Bureau binary is a separate Nix derivation. When a source change
affects only one binary, only that derivation gets a new store path
after rebuild. The Go compiler version is pinned in the flake to match
`go.mod`, and all binaries are statically linked (`CGO_ENABLED=0`).

Because Bureau does not use Nix content-addressed (CA) derivations
(they remain experimental), a source change that does not affect a
binary's actual output still produces a new Nix store path (Nix hashes
inputs, not outputs). The daemon handles this by comparing SHA256
hashes of the actual binary content (`lib/binhash`). If the binary is
byte-identical to what is running, no restart occurs — regardless of
whether the store path changed. `CompareBureauVersion` produces a
`VersionDiff` with per-component changed flags.

### Flake Structure

The flake provides three categories of outputs:

**Library functions** (`self.lib`) — environment composition
infrastructure that lives outside the per-system wrapper so callers can
apply it with their own `pkgs`:

- **Modules** (`lib.modules`) — atomic building blocks, each a function
  `pkgs -> [derivation]`. Organized by category (foundation, developer,
  runtime, sysadmin). Modules are the unit of reuse — external
  environment repositories compose from these.
- **Presets** (`lib.presets`) — composed module groups that form a
  layered progression. Each level builds on the previous: minimal shell,
  standard foundation tools, developer tools, sysadmin tools.
- **`bureauRuntime`** — bubblewrap and tmux, the two host tools needed
  for sandbox creation and observation relay.

**Packages** (`packages.<system>`) — Bureau binaries plus environment
derivations. Environment derivations are pre-composed `buildEnv`
closures for common roles (CI runner, integration test, coding agent,
sysadmin agent).

**Dev shell** (`devShells.default`) — the development environment with
build tools, Go tooling, testing dependencies, code quality checks, and
the pre-commit hook. The shell hook fixes TMPDIR (Nix stdenv sets it to
an empty string in dev shells).

---

## Sandbox Environments

Sandbox environments are the "distro packages" of Bureau. They define
what tools are available inside a sandbox — cmake, ninja, Go, Python,
curl, whatever the agent or service needs.

### How They Work

An environment is a Nix `buildEnv` derivation: a store path containing
`bin/`, `lib/`, etc. with symlinks to all the packages in the
environment's closure. When the launcher creates a sandbox with an
`EnvironmentPath`:

1. It bind-mounts `/nix/store` read-only (making the entire transitive
   closure accessible).
2. It prepends `EnvironmentPath/bin` to `PATH`.

Fifty agents sharing the same environment share one store path via hard
links in the Nix store — no per-agent disk duplication.

### Composition Model

The flake exports `lib.modules` and `lib.presets` so that external
environment repositories can compose environments from atomic building
blocks without duplicating package lists. The
`bureau-foundation/environment` repository composes on top of these —
project-specific tools go there, not in the core flake.

The composition is purely functional: modules are `pkgs -> [derivation]`
functions, `applyModules` flattens an attribute set of modules into a
single package list, and presets compose modules with `++`. A project
environment might be:

```nix
pkgs.buildEnv {
  name = "iree-dev-env";
  paths =
    bureau.lib.presets.developer pkgs
    ++ bureau.lib.modules.runtime.python pkgs
    ++ [ pkgs.cmake pkgs.ninja pkgs.clang ];
}
```

### Independence from Bureau Core

Environments are defined and deployed independently. Changing an
environment never rebuilds Bureau. Changing Bureau never rebuilds
environments. They share the binary cache for distribution and
MachineConfig for deployment, but their build and release cycles are
separate.

### Environment CLI

The `bureau environment` command manages sandbox environments:

- **`bureau environment list`** — queries a flake for available
  environment profiles using `nix flake show --json`, filtered to the
  current system architecture.
- **`bureau environment build <profile>`** — builds a profile via
  `nix build`, producing a store path. Default output link at
  `/var/bureau/environment/<profile>`. Supports `--override-input` for
  development (e.g., `--override-input bureau=path:/home/user/src/bureau`
  to test local flake changes).
- **`bureau environment status`** — scans `/var/bureau/environment/` and
  `deploy/buildbarn/runner-env` for symlinks into `/nix/store`, showing
  which environments are installed and their store paths.

### Template Integration

Templates declare an `Environment` field (a Nix store path). The daemon
resolves this when building sandbox specs: `specToProfile` copies
`template.Environment` to `spec.EnvironmentPath`. Per-principal
`EnvironmentOverride` in the machine config takes precedence, allowing
a specific agent to use a different environment than its template
specifies.

Before creating a sandbox, the daemon prefetches the environment store
path from the binary cache via `nix-store --realise`. A fast-path
`os.Stat` check avoids redundant `nix-store` invocations on subsequent
reconcile cycles — Nix store paths are content-addressed and immutable,
so presence guarantees closure integrity.

---

## Bureau Version Management

The daemon, launcher, and proxy are long-running processes that need
coordinated updates without disrupting running sandboxes. MachineConfig
includes a `BureauVersion` section specifying desired Nix store paths
for each component:

```json
{
  "bureau_version": {
    "daemon_store_path": "/nix/store/abc123-bureau-daemon/bin/bureau-daemon",
    "launcher_store_path": "/nix/store/def456-bureau-launcher/bin/bureau-launcher",
    "proxy_store_path": "/nix/store/ghi789-bureau-proxy/bin/bureau-proxy"
  }
}
```

Only three process types are tracked: daemon (runs continuously),
launcher (runs continuously), and proxy (spawned per-sandbox by the
launcher). Other Bureau binaries (bridge, sandbox, credentials,
proxy-call, observe-relay) are short-lived utilities resolved from PATH
or the Nix environment at invocation time.

### Update Flow

When the daemon sees a new `BureauVersion` in MachineConfig:

1. **Prefetch** — `nix-store --realise` for each component's store
   path. The binary cache delivers pre-built binaries; this is a
   download, not a compilation.

2. **Compare binary content hashes** — SHA256 of the actual binary file,
   not the Nix store path. `CompareBureauVersion` produces a
   `VersionDiff` indicating which components actually changed. A
   dependency bump that produces a new store path but an identical binary
   is detected and skipped.

3. **Apply updates** (in order):
   - **Proxy** — send `update-proxy-binary` IPC to the launcher. The
     launcher updates its binary path for future sandbox creation.
     Existing proxy processes are unaffected — they continue running
     their current binary until their sandbox is recycled.
   - **Daemon** — `exec()` the new binary. The process replaces itself
     atomically, reconnects to Matrix, and resumes reconciliation.
   - **Launcher** — send `exec-update` IPC. The launcher persists
     sandbox state, responds OK, then `exec()`s itself. The new
     launcher reconnects to surviving sandbox processes.

4. **Report** the update result to Matrix (the machine's config room).

### Watchdog

The daemon self-update via `exec()` is inherently risky: if the new
binary crashes, the old binary must be able to detect and report the
failure. The `lib/watchdog` package provides this:

1. Before `exec()`: write a watchdog state file (component name,
   previous binary path, new binary path, timestamp). The write is
   atomic (temp file, fsync, rename, fsync parent directory).
2. `exec()` the new binary.
3. On startup, the new binary checks the watchdog:
   - If its own path matches `NewBinary`: the update succeeded. Clear
     the watchdog, report success to Matrix.
   - If its own path matches `PreviousBinary`: the new binary crashed
     and systemd restarted the old one. Report failure to Matrix, add
     the failed path to a blocklist (no retry during this process
     lifetime), clear the watchdog.
   - If neither matches: unrelated restart. Clear the stale watchdog.

Watchdog files older than 5 minutes are treated as stale and silently
cleared, preventing action on ancient files from unrelated restarts.

### The Critical Property

A one-line change in `proxy/handler.go` rebuilds only `bureau-proxy`
(Bazel's action graph). The Nix derivation for `bureau-proxy` gets a new
store path. The daemon prefetches the new store path, hashes the binary,
finds only the proxy actually changed, and tells the launcher to use the
new path for future sandboxes. The daemon, launcher, and every existing
sandbox continue running without interruption.

---

## Machine Bootstrap

A new machine goes from bare Linux to a running Bureau node:

1. **Install Nix** — `script/setup-nix` installs Nix at a pinned
   version (`script/nix-installer-version`), with flakes enabled by
   default.
2. **Configure binary cache** — the flake's `nixConfig` declares the
   Bureau binary cache as an extra substituter with its public key.
3. **Pull Bureau binaries** — `nix build` or `nix-store --realise` for
   the daemon, launcher, and proxy store paths.
4. **Write systemd units** — `deploy/systemd/bureau-launcher.service`
   and `deploy/systemd/bureau-daemon.service`, both reading
   `/etc/bureau/machine.conf` for homeserver URL, machine name, and
   server name.
5. **Start the launcher** — generates the machine's age keypair,
   registers on Matrix.
6. **Start the daemon** — connects to Matrix, begins reconciliation.

After step 6, the machine is managed entirely through Matrix. The admin
writes a MachineConfig state event describing what principals to run,
what environments to provision, and the daemon converges to that state
— pulling additional binaries and environment closures from the binary
cache as needed.

### Systemd Units

Two units, both reading `/etc/bureau/machine.conf`:

- **bureau-launcher.service** — privileged (needs user namespace
  creation via bubblewrap). `ProtectSystem=strict` with write access to
  `/var/lib/bureau`, `/run/bureau`, `/var/bureau/workspace`,
  `/var/bureau/cache`, and `/tmp`. `NoNewPrivileges=no` because
  bubblewrap needs privilege transitions.
- **bureau-daemon.service** — fully unprivileged.
  `NoNewPrivileges=yes`, `ProtectSystem=strict`, write access only to
  `/var/lib/bureau` and `/run/bureau`. Depends on and requires the
  launcher.

The machine config is three environment variables:

```bash
BUREAU_HOMESERVER_URL=http://matrix.internal:6167
BUREAU_MACHINE_NAME=machine/worker-01
BUREAU_SERVER_NAME=bureau.local
```

After bootstrap, SSH remains available for debugging but is not part of
normal operations. Binary updates arrive via the cache, environment
updates arrive via the cache, service config changes arrive via Matrix,
credential provisioning happens via Matrix + age encryption, and
observation happens via the observation protocol.

---

## Infrastructure Services

The base deployment stack includes:

| Role | Purpose |
|------|---------|
| Matrix homeserver | Config store, messaging, state |
| TURN server | WebRTC NAT traversal for cross-machine connectivity |
| Binary cache | Nix binary distribution (pre-built binaries, environments) |
| Remote execution cluster | Buildbarn: CAS, action cache, scheduler, workers |

The homeserver is the brain. The binary cache distributes binaries and
environment closures. The remote execution cluster compiles code with
controlled parallelism. Together they form the infrastructure that makes
lightweight, ephemeral agents possible — each agent is small because the
heavy work (compilation, storage, state) lives in shared services.

---

## Agent Environments

Agents are lightweight. An agent's sandbox contains:

- A git worktree (persistent across sessions)
- A proxy socket (credential injection, access to Matrix, Buildbarn,
  and external APIs)
- A Nix environment closure with the tools the agent needs

The agent does not contain:

- A full compilation toolchain (builds go through Buildbarn remote
  execution)
- Build artifacts (those live in Buildbarn's CAS and the Nix store)
- Large caches or intermediate state (ephemeral, reconstructed from
  cache)

When an agent needs to build something, it invokes Bazel with
`--remote_executor` pointing to Buildbarn (routed through the proxy
socket). Buildbarn's scheduler queues the action and dispatches it to
an available worker. The agent never runs a compiler. This is what
prevents 20 agents from all running full-parallelism builds
simultaneously — the scheduler controls parallelism, not the agents.

The Nix environment mounted into the sandbox is role-specific. A coding
agent gets git, a Bazel client, and language runtimes. A sysadmin agent
gets remote access tools, network debugging, and Nix tooling. A
monitoring agent gets curl and jq. Environments are shared via hard
links in the Nix store — fifty agents with the same environment consume
no additional disk space beyond one copy.

## Relationship to Other Design Documents

- [fundamentals.md](fundamentals.md) establishes the five primitives
  that the build system packages and the daemon deploys.
- [architecture.md](architecture.md) describes the launcher/daemon/proxy
  process topology, IPC protocol, and the MachineConfig reconciliation
  loop that drives version updates and environment prefetching.
- [information-architecture.md](information-architecture.md) defines
  MachineConfig state events, including the `BureauVersion` and
  per-principal `EnvironmentOverride` fields.
- [pipelines.md](pipelines.md) describes how the pipeline executor runs
  inside a sandbox with a Nix environment providing the toolchain.
- [fleet.md](fleet.md) extends version management to multi-machine
  deployments — fleet-wide service definitions reference Nix store paths
  for placement across machines.
