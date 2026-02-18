# Bureau Matrix Deployment Playbook

Runnable guide from zero to a fully operational Bureau Matrix deployment.
Exercises every `bureau matrix` CLI subcommand and validates
infrastructure health including TURN/WebRTC.

## Prerequisites

- Nix dev environment (run `script/setup-nix` from repo root, then `nix develop`)
- Docker and Docker Compose
- `openssl` (for generating secrets)

The Nix dev shell provides Go, curl, jq, and all other tools.

Build the CLI and put it on your PATH:

```bash
bazel build //cmd/bureau
export PATH="$PWD/bazel-bin/cmd/bureau/bureau_:$PATH"
```

## 1. Generate Secrets

Three secrets are needed: a registration token (Matrix account creation),
a TURN shared secret (WebRTC NAT traversal), and an attic JWT secret
(Nix binary cache API authentication).

```bash
cd deploy/matrix

# Generate fresh secrets.
REGISTRATION_TOKEN=$(openssl rand -hex 32)
TURN_SECRET=$(openssl rand -hex 32)
ATTIC_TOKEN_SECRET=$(openssl rand 64 | base64 -w0)

# Write .env file (not committed to git).
cat > .env <<EOF
BUREAU_MATRIX_SERVER_NAME=bureau.local
BUREAU_MATRIX_REGISTRATION_TOKEN=${REGISTRATION_TOKEN}
BUREAU_TURN_SECRET=${TURN_SECRET}
BUREAU_TURN_HOST=localhost
BUREAU_ATTIC_TOKEN_SECRET=${ATTIC_TOKEN_SECRET}
EOF

# Write the registration token to a standalone file for bureau matrix setup,
# which reads secrets from files (not CLI arguments) to avoid exposure in
# process listings, shell history, and LLM conversation context.
echo "${REGISTRATION_TOKEN}" > .env-token
```

## 2. Start Infrastructure

Bring up all services: Continuwuity (Matrix homeserver), coturn (TURN relay
for WebRTC), and attic (Nix binary cache). coturn uses host networking
because Docker port mapping for the UDP relay range (49152-65535) is
unreliable and expensive.

```bash
# Wipe any previous state for a clean start.
docker compose down -v

# Start all services.
docker compose up -d
```

Wait for the homeserver and attic to become reachable:

```bash
until curl -sf http://localhost:6167/_matrix/client/versions > /dev/null; do
  echo "Waiting for Continuwuity..."
  sleep 2
done
echo "Homeserver is ready."

until curl -sf http://localhost:5580 > /dev/null 2>&1; do
  echo "Waiting for attic..."
  sleep 2
done
echo "Attic is ready."
```

Verify coturn is listening:

```bash
# TURN listens on port 3478 (TCP and UDP).
ss -tlnp | grep 3478 || echo "WARN: coturn not listening on 3478"
```

## 3. Bootstrap the Homeserver

`bureau matrix setup` creates the admin account, the Bureau space
(`#bureau:bureau.local`), and four standard rooms (agents, system,
machines, services). It writes a credential file for subsequent commands.

```bash
bureau matrix setup \
  --homeserver http://localhost:6167 \
  --registration-token-file .env-token \
  --credential-file ./bureau-creds \
  --server-name bureau.local
```

The credential file is the primary authentication mechanism for all
subsequent `bureau matrix` commands. Verify it was written:

```bash
cat ./bureau-creds
```

Expected keys: `MATRIX_HOMESERVER_URL`, `MATRIX_ADMIN_USER`,
`MATRIX_ADMIN_TOKEN`, `MATRIX_REGISTRATION_TOKEN`, `MATRIX_SPACE_ROOM`,
`MATRIX_AGENTS_ROOM`, `MATRIX_SYSTEM_ROOM`, `MATRIX_MACHINE_ROOM`,
`MATRIX_SERVICE_ROOM`.

## 4. Verify Bootstrap with Doctor

Doctor validates the entire infrastructure state: reachability,
authentication, space, rooms, hierarchy, power levels, and credential
cross-reference.

```bash
bureau matrix doctor --credential-file ./bureau-creds
```

All checks should pass. If any fail, re-run setup (it's idempotent).

Machine-readable output for scripting:

```bash
bureau matrix doctor --credential-file ./bureau-creds --json | jq .
```

## 5. Verify TURN Credentials

The homeserver generates time-limited HMAC-SHA1 credentials for TURN
access. This is how daemons obtain WebRTC relay credentials — the shared
secret never leaves the server side.

```bash
TOKEN=$(grep MATRIX_ADMIN_TOKEN ./bureau-creds | cut -d= -f2)

curl -s \
  -H "Authorization: Bearer ${TOKEN}" \
  http://localhost:6167/_matrix/client/v3/voip/turnServer | jq .
```

Expected response:

```json
{
  "username": "<unix_timestamp>",
  "password": "<hmac_sha1_credential>",
  "uris": [
    "turn:localhost:3478?transport=udp",
    "turn:localhost:3478?transport=tcp"
  ],
  "ttl": 86400
}
```

Verify the credential actually works against coturn:

```bash
# turnutils_uclient ships with coturn and validates TURN auth end-to-end.
# Skip if not installed — the homeserver response above is sufficient for
# most cases.
TURN_RESPONSE=$(curl -s -H "Authorization: Bearer ${TOKEN}" \
  http://localhost:6167/_matrix/client/v3/voip/turnServer)
TURN_USER=$(echo "${TURN_RESPONSE}" | jq -r .username)
TURN_PASS=$(echo "${TURN_RESPONSE}" | jq -r .password)

if command -v turnutils_uclient &> /dev/null; then
  turnutils_uclient -u "${TURN_USER}" -w "${TURN_PASS}" localhost
  echo "TURN relay verified."
else
  echo "turnutils_uclient not installed, skipping relay test."
  echo "TURN credentials were issued successfully (homeserver + coturn share the secret)."
fi
```

## 6. Bootstrap the Binary Cache

attic needs a cache created and an API token generated before it can accept
pushes. The `atticadm` tool runs inside the container (it needs access to
the JWT signing secret via the server config).

Generate an admin token with full permissions:

```bash
ATTIC_TOKEN=$(docker exec bureau-attic atticadm make-token \
  --sub "admin" \
  --validity "10y" \
  --pull "*" --push "*" \
  --create-cache "*" --configure-cache "*" \
  --configure-cache-retention "*" --destroy-cache "*" --delete "*" \
  -f /etc/attic/server.toml)

echo "${ATTIC_TOKEN}"
```

Configure the local attic CLI (available in the Nix dev shell) and create
the cache:

```bash
attic login bureau http://localhost:5580 "${ATTIC_TOKEN}"
attic cache create bureau
```

Make the cache public so any machine can pull without credentials:

```bash
attic cache configure --public bureau
```

Verify the cache exists and serves Nix binary cache metadata without auth:

```bash
curl -s http://localhost:5580/bureau/nix-cache-info
```

Expected output includes `StoreDir: /nix/store` and `WantMassQuery: 1`.

Note the cache's signing public key (needed for substituter config):

```bash
attic cache info bureau
```

The "Public Key" line (e.g. `bureau:D58z2AZ1...`) goes into each
machine's `nix.conf` as a trusted public key.

Save the token alongside the other credentials (not committed to git):

```bash
echo "${ATTIC_TOKEN}" > .env-attic-token
```

Configure this machine as a substituter (pulls pre-built binaries from
the cache instead of rebuilding):

```bash
PUBLIC_KEY=$(attic cache info bureau 2>&1 \
  | sed -n 's/.*Public Key: //p')

sudo script/configure-substituter \
  http://localhost:5580/bureau \
  "${PUBLIC_KEY}"
```

Build and push all Bureau binaries:

```bash
for pkg in bureau bureau-daemon bureau-launcher bureau-proxy \
           bureau-bridge bureau-sandbox bureau-credentials \
           bureau-proxy-call bureau-observe-relay; do
  nix build .#$pkg && attic push bureau ./result
done
```

## 7. Create Your Operator Account

The admin account created by setup (`bureau-admin`) is a service account
with a derived password — it's not meant for interactive login. Create a
personal operator account with `--operator`, which registers the account
and invites you to all Bureau infrastructure rooms in one step.

```bash
bureau matrix user create ben \
  --credential-file ./bureau-creds \
  --password-file - \
  --operator
```

You'll be prompted to enter and confirm a password (echo disabled). The
command creates the account and invites you to the Bureau space plus all
four standard rooms (agents, system, machines, services).

This command is idempotent: re-running it after the account exists skips
registration and ensures you're invited to any rooms you might be missing.

To log in with Element, point it at the homeserver URL
(`http://localhost:6167`) and sign in with your username and password.

## 8. Create a Project Space

Spaces organize related rooms. Create an "IREE" project space as a
realistic multi-space scenario (Bureau itself is the first space; projects
are additional spaces).

```bash
bureau matrix space create iree \
  --credential-file ./bureau-creds \
  --name "IREE" \
  --topic "IREE compiler infrastructure"
```

List all spaces to confirm:

```bash
bureau matrix space list --credential-file ./bureau-creds
```

## 9. Create Project Rooms

Create rooms within the project space. Rooms under a space use hierarchical
aliases: `iree/amdgpu/general` lives under the `iree` space.

```bash
# General discussion room for the AMDGPU target.
bureau matrix room create iree/amdgpu/general \
  --credential-file ./bureau-creds \
  --name "IREE AMDGPU General" \
  --topic "AMDGPU target discussion and coordination" \
  --space '#iree:bureau.local'

# A room where agents publish build results.
bureau matrix room create iree/builds \
  --credential-file ./bureau-creds \
  --name "IREE Builds" \
  --topic "Build status and artifacts" \
  --space '#iree:bureau.local'
```

List rooms in the space:

```bash
bureau matrix room list --credential-file ./bureau-creds --space '#iree:bureau.local'
```

## 10. Register Agent Accounts

Register accounts for agents that will operate within Bureau. Each agent
gets its own Matrix identity, managed by the launcher and proxied through
the per-sandbox proxy.

```bash
# Create agent accounts. No --password-file means the password is derived
# deterministically from the registration token (agents don't need
# interactive login — the proxy handles auth for them).
bureau matrix user create iree-builder --credential-file ./bureau-creds
bureau matrix user create iree-reviewer --credential-file ./bureau-creds
```

## 11. Invite Agents to Rooms

Invite the agent accounts to the project rooms so they can participate.

```bash
# Invite iree-builder to the builds room.
bureau matrix user invite @iree-builder:bureau.local \
  --credential-file ./bureau-creds \
  --room '#iree/builds:bureau.local'

# Invite both agents to the general room.
bureau matrix user invite @iree-builder:bureau.local \
  --credential-file ./bureau-creds \
  --room '#iree/amdgpu/general:bureau.local'

bureau matrix user invite @iree-reviewer:bureau.local \
  --credential-file ./bureau-creds \
  --room '#iree/amdgpu/general:bureau.local'
```

List room members to verify:

```bash
bureau matrix room members \
  --credential-file ./bureau-creds \
  '#iree/amdgpu/general:bureau.local'
```

## 12. Send Test Messages

Verify messaging works end-to-end.

```bash
# Send a message to the system room.
bureau matrix send \
  --credential-file ./bureau-creds \
  '#bureau/system:bureau.local' \
  "Bureau deployment complete."

# Send a message to a project room.
bureau matrix send \
  --credential-file ./bureau-creds \
  '#iree/amdgpu/general:bureau.local' \
  "IREE AMDGPU workspace initialized."
```

## 13. Read and Write State Events

State events are persistent room configuration. Bureau uses custom state
events for machine keys, service registration, and layout.

```bash
# Read power levels for the machine room.
bureau matrix state get \
  --credential-file ./bureau-creds \
  '#bureau/machine:bureau.local' \
  m.room.power_levels | jq .

# Write a test Bureau state event to the service room.
bureau matrix state set \
  --credential-file ./bureau-creds \
  --key 'test/echo' \
  '#bureau/service:bureau.local' \
  m.bureau.service \
  '{"name": "echo", "version": "0.1.0", "endpoint": "unix:///run/bureau/test/echo.sock"}'
```

Read it back:

```bash
bureau matrix state get \
  --credential-file ./bureau-creds \
  --key 'test/echo' \
  '#bureau/service:bureau.local' \
  m.bureau.service | jq .
```

## 14. Final Doctor Check

Run doctor one more time. All original checks should still pass (nothing
we did should have broken the base infrastructure).

```bash
bureau matrix doctor --credential-file ./bureau-creds
```

## 15. Re-run Setup (Idempotency)

Verify that setup is safe to re-run. It should detect existing resources
and skip creation.

```bash
bureau matrix setup \
  --homeserver http://localhost:6167 \
  --registration-token-file .env-token \
  --credential-file ./bureau-creds \
  --server-name bureau.local
```

## 16. Teardown

```bash
docker compose -f deploy/matrix/docker-compose.yaml down -v
rm -f ./bureau-creds .env-token .env-attic-token
```

## Friction Log

Record any issues encountered during the dogfood exercise. Each entry
becomes a candidate for a new bead.

| Step | Friction | Severity | Bead |
|------|----------|----------|------|
| | | | |
