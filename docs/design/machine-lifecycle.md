# Machine Lifecycle

How machines join, operate, update, and leave a Bureau fleet. Covers the
privilege model, operator personas, diagnostic tooling, and the path
from manual operations to agent-driven fleet management.

Related documents:

- nix.md — build system, binary distribution, Nix store mechanics
- fleet.md — fleet controller, placement, HA, machine provisioning
- architecture.md — runtime topology, launcher/daemon/proxy processes
- credentials.md — age encryption, TPM/keyring, credential provisioning
- authorization.md — grants, denials, allowances, service tokens

---

## Personas

Three personas interact with machines at different levels:

**Developer** — has the git repo, builds from source, iterates on Bureau
itself. Uses `nix build` from a worktree to produce binaries. May run a
local Bureau instance for testing. Deploys to their own machine or a
dev fleet.

**Operator** — installs Bureau from a Nix flake or package. Does not
have (or need) the git repo. Manages machines, observes agents, files
tickets, approves grants. Interacts entirely through the `bureau` CLI
and Matrix.

**Agent (sysadmin)** — a Bureau agent running inside a sandbox,
teleoperating machines via the daemon and fleet controller. Performs the
same operations as an operator, but programmatically: monitoring health,
approving rollouts, diagnosing failures, filing tickets. The agent's
authorization comes from its Matrix identity and grants, same as a human
operator.

The distinction between developer and operator is primarily about where
binaries come from (worktree build vs. binary cache) and what
diagnostics are available (source code vs. logs and events). Once
binaries are built, the deployment and management workflows are
identical.

---

## Privilege Model

### The One-Sudo Principle

Machine setup requires root exactly once. Every operation after that —
deploying code, managing principals, observing agents, restarting
services, diagnosing failures — is fully unprivileged.

Root is needed for:

- Creating the `bureau` system user and `bureau-operators` group
- Creating system directories (`/run/bureau`, `/var/lib/bureau`,
  `/etc/bureau`, `/var/bureau/{workspace,cache}`)
- Installing and enabling systemd units
- Adding operators to the `bureau-operators` group

### System User and Group

**`bureau`** (system user) — runs the launcher and daemon. Owns state
directories and sockets. Has no login shell, no home directory beyond
what systemd provides. This user is the security boundary: it holds
the machine's age keypair, Matrix credentials, and token signing key.

**`bureau-operators`** (system group) — any Unix user who needs CLI
access to Bureau services on this machine. Membership grants access
to the daemon's operator-facing sockets. Authorization beyond socket
access is governed by the operator's Matrix identity and grants —
the group just gets them past the filesystem permission check.

The `bureau` user is a member of `bureau-operators` (it creates the
sockets with group ownership). The `docker` group is required if
sandboxes use container environments.

### Socket Permissions

All sockets live under `/run/bureau/`:

| Socket | Owner | Mode | Purpose |
|---|---|---|---|
| `launcher.sock` | `bureau:bureau` | 0660 | Daemon ↔ launcher IPC |
| `observe.sock` | `bureau:bureau-operators` | 0660 | Operator CLI, token minting |
| `relay.sock` | `bureau:bureau` | 0660 | Cross-machine transport |
| `tmux.sock` | `bureau:bureau` | 0700 | Bureau tmux server (private) |
| `credential.sock` | `bureau:bureau` | 0755 | Principal credential access |
| `fleet/.../prod.sock` | `bureau:bureau-operators` | 0660 | Service CBOR endpoints |
| `fleet/.../prod.proxy.sock` | `bureau:bureau` | 0660 | Per-principal proxy |
| `fleet/.../prod.admin.sock` | `bureau:bureau` | 0660 | Daemon-only proxy admin |

Key principle: operator-facing sockets (`observe.sock`, service
`.sock` endpoints) are group-readable by `bureau-operators`. Internal
sockets (launcher, relay, proxy admin) are restricted to the `bureau`
user.

### Directory Ownership

| Path | Owner | Mode | Contents |
|---|---|---|---|
| `/etc/bureau/` | `root:root` | 0755 | `machine.conf` |
| `/run/bureau/` | `bureau:bureau-operators` | 0750 | Sockets, tmux |
| `/var/lib/bureau/` | `bureau:bureau` | 0700 | Keypair, session, state |
| `/var/bureau/workspace/` | `bureau:bureau` | 0755 | Git worktrees |
| `/var/bureau/cache/` | `bureau:bureau` | 0755 | Nix store cache |

### Proxy Config Directories

Per-sandbox proxy configuration (config files, listen directories for
service CBOR sockets) is created under the fleet run directory at
`/run/bureau/fleet/{fleet}/sandbox/` instead of `/tmp`. This
eliminates the need for `PrivateTmp=false` on the launcher — both
launcher and daemon use `PrivateTmp=true`. Fleet-scoping prevents
collisions when multiple fleets share a machine.

The listen directory for a service principal is:

    /run/bureau/fleet/{fleet}/sandbox/{sanitized-localpart}/listen/service.sock

A symlink from the canonical `ServiceSocketPath()` (under
`/run/bureau/fleet/{fleet}/service/...`) points to this location.
Both paths are under `/run/bureau/`, so no cross-mount-namespace
visibility issues arise.

Operator access to service sockets requires traversal through the
sandbox config directory. The launcher sets directory permissions to
enable this without exposing proxy configuration:

- `sandbox/` — mode 0755 (traversable; gated by `/run/bureau/` 0750)
- `{sanitized-localpart}/` — mode 0711 for services (traverse-only,
  no listing of proxy config files)
- `listen/` — group `bureau-operators` with SGID bit, so sockets
  created by the proxy process inherit operator-accessible group
  ownership

---

## Workflows

### 1. Bootstrap a New Machine

**Who**: Developer or operator with sudo access.

**Starting state**: A Linux machine with network access to the
homeserver. May or may not have Nix installed.

```
# Install Nix if needed (requires sudo).
curl -fsSL https://install.determinate.systems/nix | sh

# Get Bureau binaries. Developer builds from source:
nix build github:bureau-foundation/bureau#bureau-host-env --print-out-paths

# Or operator fetches from the binary cache (same command, Nix
# substitutes from the cache if available).

# Bootstrap the machine. This is the one-sudo moment.
bureau machine doctor --fix \
    --homeserver http://matrix.internal:6167 \
    --machine-name bureau/fleet/prod/machine/sharkbox \
    --server-name bureau.local \
    --fleet bureau/fleet/prod

# What --fix does (with sudo):
#   - Creates bureau user and bureau-operators group
#   - Adds current user to bureau-operators
#   - Creates system directories with correct ownership
#   - Writes /etc/bureau/machine.conf
#   - Installs systemd units
#   - Starts bureau-launcher (generates keypair, registers on Matrix)
#   - Starts bureau-daemon (connects to Matrix, begins sync)
```

After bootstrap, the machine is live. An admin publishes a
`MachineConfig` state event describing what principals to run, and the
daemon converges to that state.

### 2. Upgrade the Fleet

**Who**: Developer (from worktree) or operator (from binary cache).

**Starting state**: Fleet is running an older version. New code is
committed and ready.

```
# Developer: build from worktree.
HOST_ENV=$(nix build .#bureau-host-env --print-out-paths --no-link)

# Operator: build from flake reference (binary cache substitutes).
HOST_ENV=$(nix build github:bureau-foundation/bureau#bureau-host-env \
    --print-out-paths --no-link)

# Publish the upgrade event. The daemon picks it up via /sync,
# prefetches store paths, and exec()s into the new binary.
bureau machine upgrade --local --host-env "$HOST_ENV"

# For a remote machine:
nix copy --to ssh://user@host "$HOST_ENV"
bureau machine upgrade bureau/fleet/prod/machine/worker-01 \
    --host-env "$HOST_ENV"
```

No sudo. No restart. No downtime. The daemon exec()s atomically.
The watchdog protocol (nix.md) detects and reports failed transitions.

For fleet-wide rollouts, repeat for each machine — or let the fleet
controller coordinate a rolling upgrade via placement events.

### 3. Upgrade a Single Service

**Who**: Developer or operator.

**Starting state**: Fleet binaries are fine, but a specific service
(e.g., `bureau-agent-claude`) needs a new version.

Service binaries live in Nix environment closures, not the host-env.
Updating a single service means publishing a new environment version
for that service's template, without touching the daemon/launcher
binaries.

```
# Build the updated environment.
ENV=$(nix build .#bureau-claude-env --print-out-paths --no-link)

# Update the template to reference the new environment.
bureau template update bureau/template:claude-agent \
    --environment "$ENV"

# The daemon reconciles: sees the template changed, tears down the
# old sandbox, creates a new one with the updated environment.
```

No fleet cycle. Only principals using that template are restarted.

### 4. Agent-Driven Rollout (North Star)

**Who**: A development agent and a sysadmin agent, coordinating via
tickets.

**Starting state**: A dev agent has committed and pushed changes.

The workflow:

- Dev agent creates a ticket requesting deployment, referencing the
  commit.
- A pipeline runs: builds the Nix closure, runs the full test suite
  (unit tests, integration tests, stress tests, benchmarks), produces
  a build report.
- Pipeline publishes build artifacts (the host-env store path and test
  results) and attaches them to the ticket.
- Ticket gate: requires sysadmin review. The ticket appears in the
  sysadmin room's ready queue.
- Sysadmin agent (or human) reviews the build report, approves the
  gate.
- A deployment pipeline triggers: for each machine in the fleet,
  publishes the `BureauVersion` event with the built store path.
  Monitors watchdog results. Reports success or failure to the ticket.
- On failure: sysadmin agent reads daemon logs via observation,
  diagnoses the issue, files a bug ticket, and (if safe) rolls back
  by publishing the previous version.

The sysadmin agent can do all of this because:

- It runs as a Bureau principal with Matrix credentials
- It has grants for `fleet/**`, `observe/**`, `command/machine/**`
- It can read logs via the observation protocol
- It can publish state events via proxy
- It can file and manage tickets
- It can trigger pipelines

No sudo. No SSH. No shell access. Everything through Bureau's
authenticated, audited channels.

### 5. Enable a New Machine (Headless/Cloud)

**Who**: Operator or sysadmin agent.

**Starting state**: A fresh Linux VM or bare-metal host with SSH
access.

```
# From the operator's workstation:
bureau machine deploy ssh://root@new-host.internal \
    --homeserver http://matrix.internal:6167 \
    --machine-name bureau/fleet/prod/machine/new-host \
    --server-name bureau.local \
    --fleet bureau/fleet/prod
```

This command:

- SSHs to the host
- Installs Nix (if needed)
- Copies the Bureau host-env closure
- Runs `bureau machine doctor --fix` on the remote host
- Waits for the machine to appear in the fleet

After this, the machine is fully managed via Matrix. SSH is no longer
needed for normal operations (though it remains available for
debugging).

### 6. Diagnose and Repair

**Who**: Operator, or sysadmin agent.

```
# Check machine health.
bureau machine doctor

# On a remote machine (future):
bureau machine doctor ssh://user@host

# Fix issues (requires sudo for system-level fixes).
bureau machine doctor --fix
```

The doctor checks (non-exhaustive):

- **System**: bureau user exists, bureau-operators group exists,
  current user is in the group
- **Directories**: /run/bureau, /var/lib/bureau, /etc/bureau exist
  with correct ownership and permissions
- **Sockets**: observe.sock, launcher.sock present and connectable
- **Systemd**: units installed, enabled, and running; unit files match
  expected version
- **Configuration**: machine.conf present and valid
- **Matrix**: homeserver reachable, machine registered, config room
  exists, session valid
- **Services**: expected services running, sockets responsive
- **Versions**: running binary version matches expected version

Checks that fail are reported with specific fix descriptions. `--fix`
applies all fixes that can be applied automatically. Fixes requiring
sudo prompt once at the start.

### 7. Decommission a Machine

**Who**: Operator or sysadmin agent.

```
bureau machine decommission bureau/fleet/prod/machine/old-host
```

This:

- Drains principals (graceful shutdown with configurable grace period)
- Removes the machine from the fleet controller
- Publishes a decommission event to the machine's config room
- The daemon on that machine sees the event, stops all principals,
  and exits

To fully remove Bureau from the host:

```
# On the host:
bureau machine doctor --uninstall

# Stops services, removes systemd units, removes system directories,
# optionally removes the bureau user and group.
```

### 8. Stop and Start

**Who**: Operator.

```
# Stop Bureau on this machine (graceful drain).
sudo systemctl stop bureau-daemon bureau-launcher

# Start Bureau on this machine.
sudo systemctl start bureau-launcher
# (daemon starts automatically via Requires=)
```

Stopping and starting is a systemd operation (requires sudo). For
individual principals, use Matrix state events — remove the principal
assignment from the machine config, and the daemon tears it down. No
sudo needed for per-principal lifecycle management.

---

## Developer vs. Operator: What Differs

| Concern | Developer | Operator |
|---|---|---|
| Binary source | `nix build .#bureau-host-env` (worktree) | `nix build github:...#bureau-host-env` (cache) |
| Build step | Yes (local Bazel/Nix) | No (Nix substitutes from cache) |
| Source code | Available (git clone) | Not needed |
| Diagnostics | Logs + source code + tests | Logs + Matrix events + doctor |
| Test suite | `bazel test //...`, integration tests | Doctor checks, service health |
| Iteration speed | Edit → build → deploy (seconds) | Fetch → deploy (seconds) |
| Machine access | Usually local (their workstation) | May be remote (SSH for bootstrap) |

After binaries are built, the workflows converge. `bureau machine
upgrade` is the same command regardless of where the binaries came from.

---

## Agent Autonomy

The design goal: once a machine is bootstrapped (the one-sudo moment),
an agent running as a Bureau principal with appropriate grants can
perform every operational task without root access.

**What agents can do (unprivileged, via Matrix and sockets):**

- Deploy new versions (`bureau machine upgrade`)
- Manage principals (publish MachineConfig state events)
- Observe agents (observation protocol via observe.sock)
- Manage tickets (ticket service via service socket)
- Trigger pipelines (pipeline events)
- Read logs (telemetry service)
- Approve grants (temporal grant events)
- Diagnose failures (doctor checks, log analysis, Matrix events)
- Coordinate fleet operations (fleet controller via service socket)
- File and resolve tickets for issues found

**What still requires root:**

- First-time machine bootstrap
- Systemd unit file changes (rare — binary updates use exec(), not
  unit changes)
- Adding new Unix users to bureau-operators
- OS-level changes (kernel, packages, network)

The agent-as-sysadmin model works because Bureau's control plane is
Matrix. Everything the daemon can do, it does in response to Matrix
state events. An agent that can publish state events can drive the
system. The authorization layer ensures agents can only do what their
grants permit — a dev agent cannot approve its own deployment, a
sysadmin agent cannot modify code. Separation of concerns is enforced
by the grant structure, not by Unix permissions.

---

## Relationship to Other Design Documents

- **nix.md** covers the build pipeline (Bazel → Nix derivations →
  binary cache) and the version management protocol (BureauVersion
  state events, watchdog, exec()). This document describes the
  operator-facing workflows that use those mechanisms.
- **fleet.md** covers fleet-level coordination: the fleet controller's
  placement algorithm, HA leases, machine provisioning at the service
  mesh level. This document describes the Unix-level machine setup
  that fleet.md assumes is already done.
- **architecture.md** defines the runtime topology (launcher, daemon,
  proxy) and IPC protocol. This document describes how that topology
  is installed, configured, and maintained.
- **credentials.md** covers how secrets are encrypted, stored, and
  provisioned. This document covers the Unix user/group model that
  determines who can access the processes holding those secrets.
- **authorization.md** covers Matrix-level access control (grants,
  denials). This document covers the Unix-level access control (user,
  group, socket permissions) that gates access to the authorization
  system itself.
