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

## 1. Start the Homeserver

Bureau uses Continuwuity (a Matrix homeserver) as its persistence layer.
The `deploy/matrix/` directory contains a Docker Compose stack with
Continuwuity, coturn (for WebRTC NAT traversal), and attic (Nix binary
cache).

```bash
cd deploy/matrix

# Generate secrets (registration token, TURN shared secret, attic JWT).
REGISTRATION_TOKEN=$(openssl rand -hex 32)
TURN_SECRET=$(openssl rand -hex 32)
ATTIC_TOKEN_SECRET=$(openssl rand 64 | base64 -w0)

cat > .env <<EOF
BUREAU_MATRIX_SERVER_NAME=bureau.local
BUREAU_MATRIX_REGISTRATION_TOKEN=${REGISTRATION_TOKEN}
BUREAU_TURN_SECRET=${TURN_SECRET}
BUREAU_TURN_HOST=localhost
BUREAU_ATTIC_TOKEN_SECRET=${ATTIC_TOKEN_SECRET}
EOF

# Save the registration token to a file (CLI reads secrets from files,
# never from arguments or environment).
echo "${REGISTRATION_TOKEN}" > .env-token

# Start everything.
docker compose up -d
```

Wait for the homeserver to become healthy:

```bash
curl -sf http://localhost:6167/_matrix/client/versions
```

For the full infrastructure playbook including coturn and attic
configuration, see `deploy/matrix/PLAYBOOK.md`.

## 2. Bootstrap the Homeserver

The `bureau matrix setup` command creates the admin account, the Bureau
namespace space, and the global rooms (system, template, pipeline,
agents, machines, services).

```bash
bureau matrix setup \
    --homeserver http://localhost:6167 \
    --server-name bureau.local \
    --registration-token-file deploy/matrix/.env-token \
    --credential-file deploy/matrix/bureau-creds
```

This writes a credential file (`bureau-creds`) containing the admin
token and room IDs. All subsequent commands use `--credential-file` to
authenticate.

Verify with:

```bash
bureau matrix doctor \
    --credential-file deploy/matrix/bureau-creds \
    --server-name bureau.local
```

## 3. Create a Fleet

A fleet is a group of machines managed together. Fleet rooms hold
machine registrations, service bindings, and configuration.

```bash
bureau fleet create bureau/fleet/prod \
    --credential-file deploy/matrix/bureau-creds
```

## 4. Provision a Machine

Provisioning creates the machine's Matrix account, per-machine config
room, and a bootstrap config file that contains a one-time password.

Run this on the operator workstation (the machine with the credential
file):

```bash
bureau machine provision bureau/fleet/prod/machine/worker-01 \
    --credential-file deploy/matrix/bureau-creds \
    --output bootstrap.json
```

Transfer `bootstrap.json` to the target machine. The one-time password
in the file is rotated on first boot, so it becomes useless after the
machine starts.

## 5. Deploy to the Target Machine

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

## 6. Verify

On the target machine:

```bash
bureau machine doctor
```

This checks system prerequisites, directories, binaries, systemd units,
sockets, and Matrix connectivity. All checks should pass.

From the operator workstation:

```bash
bureau machine list --credential-file deploy/matrix/bureau-creds
```

The new machine should appear in the list.

## 7. Add an Operator

Create a Matrix account for each human operator:

```bash
bureau matrix user create alice \
    --credential-file deploy/matrix/bureau-creds \
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
bureau machine upgrade --local \
    --host-env "$HOST_ENV" \
    --credential-file deploy/matrix/bureau-creds

# Remote machine.
bureau machine upgrade bureau/fleet/prod/machine/worker-01 \
    --host-env "$HOST_ENV" \
    --credential-file deploy/matrix/bureau-creds
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
bureau machine decommission bureau/fleet/prod/machine/worker-01 \
    --credential-file deploy/matrix/bureau-creds
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

## Adding More Machines

Repeat steps 4-6 for each additional machine. The fleet controller
(when enabled) coordinates service placement across machines
automatically.

```bash
# Enable the fleet controller on a machine.
bureau fleet enable bureau/fleet/prod \
    --host local \
    --credential-file deploy/matrix/bureau-creds
```

See fleet.md for multi-machine service placement, HA, and scaling.

---

## Related Documents

- `deploy/matrix/PLAYBOOK.md` — detailed infrastructure playbook
  including TURN, binary cache, and validation steps
- machine-lifecycle.md — privilege model, socket permissions, directory
  layout, agent autonomy
- nix.md — build pipeline, binary cache, version management protocol
- fleet.md — fleet controller, placement, HA, machine provisioning
- credentials.md — age encryption, TPM/keyring, credential bundles
