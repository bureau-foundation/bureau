# Deploying Bureau

Operational guide for deploying Bureau to one or more machines. Covers
infrastructure setup, machine provisioning, and day-2 operations.

For architecture and design rationale, see the design documents:
machine-lifecycle.md, nix.md, credentials.md, fleet.md.

---

## Prerequisites

- **Nix** — Bureau binaries are distributed as Nix derivations. Install
  with `script/setup-nix` (from a git checkout) or from
  [Determinate Systems](https://docs.determinate.systems/determinate-nix/).

- **Docker and Docker Compose** — the Continuwuity homeserver runs as a
  container. The homeserver can also run natively, but Docker is the
  supported path.

- **A machine with network access** — the operator workstation needs to
  reach the homeserver. Target machines need to reach the homeserver.

## 1. Bootstrap Infrastructure

Bureau requires a Continuwuity homeserver (Matrix), an attic binary cache
(Nix), and optionally a coturn TURN server (WebRTC NAT traversal). These
run as Docker containers managed by the
[environment](https://github.com/bureau-foundation/environment) repository.

Fork the environment repository and run the bootstrap script:

```bash
# Clone your fork of the environment repo.
git clone https://github.com/$YOUR_ORG/environment.git
cd environment

# Run the bootstrap (idempotent — safe to re-run).
nix run .#bootstrap
```

The bootstrap script:
- Generates secrets (registration token, TURN secret, attic JWT)
- Starts Docker containers (Continuwuity, attic, coturn)
- Waits for health checks
- Runs `bureau matrix setup` to create the admin account and Bureau rooms
- Configures the binary cache
- Writes a credential file for subsequent commands

After bootstrap, verify with:

```bash
bureau matrix doctor --credential-file ./bureau-creds
```

See the environment repository README for full details, customization,
and updating.

## 2. Create a Fleet

A fleet is a group of machines managed together. Fleet rooms hold
machine registrations, service bindings, and configuration.

```bash
bureau fleet create bureau/fleet/prod \
    --credential-file ./bureau-creds
```

## 3. Provision a Machine

Provisioning creates the machine's Matrix account, per-machine config
room, and a bootstrap config file that contains a one-time password.

Run this on the operator workstation (the machine with the credential
file):

```bash
bureau machine provision bureau/fleet/prod/machine/worker-01 \
    --credential-file ./bureau-creds \
    --output bootstrap.json
```

Transfer `bootstrap.json` to the target machine. The one-time password
in the file is rotated on first boot, so it becomes useless after the
machine starts.

## 4. Deploy to the Target Machine

On the target machine, run the unified deploy command. This handles
system user/group creation, directory setup, binary installation,
systemd unit installation, first-boot registration, and service start
in one step.

```bash
# Get Bureau binaries on the target machine.
# From the binary cache:
nix build github:bureau-foundation/bureau#bureau-host-env --print-out-paths

# Or from a local checkout:
nix build .#bureau-host-env --print-out-paths

# Deploy (requires sudo for first-time system setup).
sudo bureau machine deploy local --bootstrap-file bootstrap.json
```

What `bureau machine deploy local` does:

1. Runs `bureau machine doctor --fix` to create the `bureau` user,
   `bureau-operators` group, system directories, install binaries as
   symlinks to Nix store paths, write `/etc/bureau/machine.conf`, and
   install systemd units.
2. Runs the launcher in first-boot mode: logs in with the one-time
   password, rotates it, generates an age keypair, publishes the
   public key to the fleet.
3. Starts `bureau-launcher` and `bureau-daemon` via systemd.

After deploy, the machine is live and managed entirely through Matrix.

## 5. Verify

On the target machine:

```bash
bureau machine doctor
```

This checks system prerequisites, directories, binaries, systemd units,
sockets, and Matrix connectivity. All checks should pass.

From the operator workstation:

```bash
bureau machine list --credential-file ./bureau-creds
```

The new machine should appear in the list.

## 6. Add an Operator

Create a Matrix account for each human operator:

```bash
bureau matrix user create alice \
    --credential-file ./bureau-creds \
    --operator
```

The `--operator` flag invites the user to all Bureau infrastructure
rooms. The operator can then log in:

```bash
bureau login alice
# Enter password when prompted. Saves session to ~/.config/bureau/session.json.
```

After login, CLI commands auto-detect the session and no longer need
`--credential-file`.

---

## Day-2 Operations

### Upgrade Binaries

Build a new host environment from the current code:

```bash
HOST_ENV=$(nix build .#bureau-host-env --print-out-paths --no-link)
```

Publish an upgrade event. The daemon detects it via Matrix /sync,
prefetches the Nix store paths, and performs an atomic exec() transition.
No restart, no downtime.

```bash
# Local machine.
bureau machine upgrade --local --host-env "$HOST_ENV"

# Remote machine.
bureau machine upgrade bureau/fleet/prod/machine/worker-01 \
    --host-env "$HOST_ENV"
```

For developer iteration, `script/dev-deploy` wraps the build and
upgrade into one command.

### Diagnose and Repair

```bash
# Check health (no changes).
bureau machine doctor

# Fix issues (may need sudo for system-level fixes).
sudo bureau machine doctor --fix

# Preview what --fix would do.
sudo bureau machine doctor --fix --dry-run
```

### Decommission a Machine

Drain principals and remove the machine from the fleet:

```bash
bureau machine decommission bureau/fleet/prod/machine/worker-01
```

Then on the machine itself, remove Bureau:

```bash
# Preview what would be removed.
sudo bureau machine uninstall --dry-run

# Remove everything (services, units, binaries, directories).
sudo bureau machine uninstall

# Also remove the bureau user and group.
sudo bureau machine uninstall --remove-user
```

### Stop and Start

```bash
# Stop Bureau on this machine.
sudo systemctl stop bureau-daemon bureau-launcher

# Start Bureau on this machine (launcher starts first, daemon follows).
sudo systemctl start bureau-launcher
```

For individual principals, use Matrix state events to add or remove
assignments from the machine config. No sudo needed.

---

## Deploying a Claude Code Agent

The `bureau-agent-claude` template is published from an external Nix flake
repository ([bureau-foundation/claude-code-agent](https://github.com/bureau-foundation/claude-code-agent)).
It wraps Claude Code with the Bureau agent driver (Matrix message pump,
session tracking, metrics), MCP tools, and hook-based write authorization.

### Publish the Template

```bash
bureau template publish \
    --flake github:bureau-foundation/claude-code-agent \
    bureau/template:bureau-agent-claude
```

This evaluates the flake's `bureauTemplate` output and publishes it as a
Matrix state event. Command and environment paths resolve to full
`/nix/store/...` paths. The daemon prefetches missing store paths from the
binary cache before creating the sandbox.

### Provide Authentication

Deploy with `--extra-credential ANTHROPIC_API_KEY=sk-ant-...`. The proxy
injects the key as an `x-api-key` header on outbound Anthropic API
requests. The plaintext key never appears in Matrix state events and is
never visible to the agent process.

```bash
bureau agent create bureau/template:bureau-agent-claude \
    --machine bureau/fleet/prod/machine/sharkbox \
    --name agent/my-agent \
    --extra-credential ANTHROPIC_API_KEY=sk-ant-...
```

### Deploy Supporting Services

The agent needs the artifact and agent services running on the machine:

```bash
bureau service create bureau/template:artifact-service \
    --machine bureau/fleet/prod/machine/sharkbox \
    --name service/artifact

bureau service create bureau/template:agent-service \
    --machine bureau/fleet/prod/machine/sharkbox \
    --name service/agent
```

### Create Agent or Workspace

**Standalone agent** (no workspace/worktree):

```bash
bureau agent create bureau/template:bureau-agent-claude \
    --machine bureau/fleet/prod/machine/sharkbox \
    --name agent/code-review
```

**Workspace with agent** (includes git worktree setup):

```bash
bureau workspace create project/feature \
    --machine bureau/fleet/prod/machine/sharkbox \
    --template bureau/template:bureau-agent-claude \
    --param repository=https://github.com/org/repo.git
```

### Observe

```bash
bureau observe agent/code-review
bureau dashboard
```

## Adding More Machines

Repeat steps 3-5 for each additional machine. The fleet controller
(when enabled) coordinates service placement across machines
automatically.

```bash
# Enable the fleet controller on a machine.
bureau fleet enable bureau/fleet/prod --host local
```

See fleet.md for multi-machine service placement, HA, and scaling.

---

## Related Documents

- [environment](https://github.com/bureau-foundation/environment) —
  infrastructure bootstrap, profiles, and operator fleet configuration
- machine-lifecycle.md — privilege model, socket permissions, directory
  layout, agent autonomy
- nix.md — build pipeline, binary cache, version management protocol
- fleet.md — fleet controller, placement, HA, machine provisioning
- credentials.md — age encryption, TPM/keyring, credential bundles
