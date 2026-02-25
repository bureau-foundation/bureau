# Bureau Machine Roles and Bootstrap Inventory

What state a machine needs to participate in a Bureau deployment, organized
by role. Drives the future bootstrap script and prevents credential sprawl.

## Design Principle: Minimal Credential Surface

Most machines are consumers, not producers. A machine running a Gmail watcher
doesn't need push access to the binary cache, a Bazel toolchain, or the
Matrix registration token. The bootstrap should give each machine exactly
the credentials its role requires and nothing more.

Every credential listed below answers three questions:

- **Who provisions it?** (human operator, another machine, self-generated)
- **Where does it live at rest?** (file, TPM, Matrix state event, environment variable)
- **What access level?** (read-only, read-write, admin)

---

## Machine Roles

### Hub (the machine running infrastructure services)

Runs Continuwuity, coturn, and attic. There is exactly one hub in a
deployment (for now — federation changes this later). The hub is also
typically a dev machine during early deployment.

### Dev (full development workstation)

Builds, tests, and pushes binaries. Has write access to the binary cache.
Runs the Nix dev shell, Bazel, and lefthook pre-commit hooks.

### Worker (agent execution host)

Runs bureau-daemon and bureau-launcher. Pulls pre-built binaries from the
cache. Spawns sandboxed principals. No build toolchain needed.

### Operator (human interactive access)

A machine where a human logs in to administer Bureau — creating principals,
provisioning credentials, running `bureau matrix doctor`. May overlap with
dev or worker.

---

## Credential and State Inventory

### 1. Nix Binary Cache (attic)

| What | Hub | Dev | Worker | Operator |
|------|-----|-----|--------|----------|
| attic server | runs it | — | — | — |
| attic JWT signing secret | env var in docker-compose | — | — | — |
| attic admin token | local file (.env-attic-token) | — | — | — |
| attic push token | — | local file (~/.config/attic) | — | — |
| substituter URL in nix.conf | yes | yes | yes | optional |
| cache public key in nix.conf | yes | yes | yes | optional |

**Key insight:** Read access to the binary cache requires zero credentials.
The cache is public. Any machine with the substituter URL and signing public
key in its `nix.conf` can pull binaries. Only push access requires a token.

Push tokens are scoped: `atticadm make-token --push "bureau" --pull "bureau"`
gives push+pull for the `bureau` cache only. A CI machine might get push
access; a worker never does.

Provisioning:
```
# Read-only (worker, operator): just configure nix.conf
sudo script/configure-substituter <url> <public-key>

# Read-write (dev, CI): also generate and store a push token
attic login bureau <url> <push-token>
```

### 2. Matrix Homeserver (Continuwuity)

| What | Hub | Dev | Worker | Operator |
|------|-----|-----|--------|----------|
| Continuwuity server | runs it | — | — | — |
| Registration token | env var in docker-compose | — | — | — |
| Admin credentials (bureau-creds) | local file | — | — | — |
| Machine Matrix account + token | — | — | session.json | — |
| Operator Matrix account + token | — | — | — | ~/.config/bureau/session.json |
| TURN shared secret | env var in docker-compose | — | — | — |

**Key insight:** The registration token is the root secret. Everything else
derives from it: admin password (SHA-256 of token), machine accounts
(registered by launcher using token), principal accounts (registered by
launcher on behalf of daemon). A machine that has the registration token
and the homeserver URL can bootstrap itself completely.

Worker bootstrap: the launcher takes `--registration-token-file` and
`--homeserver` on first boot, generates a machine keypair, registers a
Matrix account, and writes `session.json`. Subsequent boots read
`session.json`. The registration token is needed exactly once per machine.

### 3. TURN Server (coturn)

| What | Hub | Dev | Worker | Operator |
|------|-----|-----|--------|----------|
| coturn server | runs it | — | — | — |
| TURN shared secret | env var (shared with Continuwuity) | — | — | — |
| TURN credentials | — | — | via Matrix API | — |

**Key insight:** No machine other than the hub ever sees the TURN secret.
Workers obtain time-limited TURN credentials by calling
`/_matrix/client/v3/voip/turnServer` with their Matrix token. The
homeserver generates HMAC-SHA1 credentials from the shared secret. This
is the standard Matrix/WebRTC credential flow — coturn validates the
HMAC without needing to know individual machine identities.

### 4. Machine Identity (age keypair)

| What | Hub | Dev | Worker | Operator |
|------|-----|-----|--------|----------|
| Machine age keypair | — | — | generated locally | — |
| Machine public key | — | — | published to Matrix | — |

Workers generate their own keypair at first boot (launcher). The public
key is published as a Matrix state event in `#bureau/machine`. The
private key stays on the machine (ideally TPM-sealed, currently filesystem).

Credential bundles for principals are age-encrypted to the machine's
public key and stored as Matrix state events. The daemon syncs them
and passes ciphertext to the launcher, which decrypts and pipes plaintext
to the proxy process.

### 5. Principal Credentials (per-sandbox)

| What | Hub | Dev | Worker | Operator |
|------|-----|-----|--------|----------|
| External API keys | — | — | encrypted in Matrix | provisioned by operator |
| Matrix access tokens | — | — | proxy holds in memory | — |
| OAuth tokens | — | — | proxy holds in memory | provisioned by operator |

These never touch the filesystem in plaintext. The operator provisions
them via `bureau-credentials`, which encrypts them to the target machine's
public key and writes the ciphertext to a Matrix state event. The
launcher decrypts and pipes to the proxy. The proxy holds them in memory
and injects them into HTTP requests.

---

## Bootstrap Sequences

### Worker Bootstrap (the common case)

A new machine that will run Bureau principals. The operator provisions the
machine from the hub, transfers the bootstrap config, and runs the
bootstrap script on the new machine.

**On the hub machine** (where the credential file lives):

```bash
# Create the machine account and write a bootstrap config.
bureau machine provision machine/worker-01 \
  --credential-file ./bureau-creds \
  --output bootstrap.json

# Transfer the config to the new machine.
scp bootstrap.json user@worker-01:/tmp/bootstrap.json
```

**On the new machine:**

```bash
# Full automated bootstrap (installs Nix, pulls binaries, registers, starts services).
script/bootstrap-machine \
  --substituter-url http://attic.internal:5580/bureau \
  --cache-public-key "bureau:<PUBLIC_KEY>" \
  /tmp/bootstrap.json
```

Or step by step:

```bash
# 1. Install Nix (if not present)
script/setup-nix

# 2. Configure binary cache (read-only, no credentials needed)
sudo script/configure-substituter \
  http://attic.internal:5580/bureau \
  "bureau:<PUBLIC_KEY>"

# 3. Pull Bureau binaries from cache
nix build github:bureau-foundation/bureau#bureau-launcher --no-link
nix build github:bureau-foundation/bureau#bureau-daemon --no-link

# 4. First boot — launcher logs in, rotates password, publishes key
bureau-launcher \
  --bootstrap-file /tmp/bootstrap.json \
  --machine-name machine/worker-01 \
  --first-boot-only

# 5. Install and start systemd services
sudo bureau machine doctor --fix
# Or manually:
# sudo cp lib/content/systemd/bureau-{launcher,daemon}.service /etc/systemd/system/
# sudo systemctl daemon-reload
# sudo systemctl enable --now bureau-launcher bureau-daemon
```

After first boot, the one-time password in the bootstrap config has been
rotated. The config file is deleted automatically. The machine authenticates
via its Matrix session from then on.

**Security note:** The registration token never leaves the hub machine.
The bootstrap config contains only a random one-time password scoped to
a single machine account. Even if captured, it's useless after the
launcher rotates it on first boot.

**Legacy path:** The `--registration-token-file` flag on the launcher
still works for development and testing. Production deployments should
use the provision flow described above.

### Dev Bootstrap (adds build + push capabilities)

Everything from worker bootstrap, plus:

```bash
# Build tools
nix develop  # enters dev shell with bazel, go, etc.

# Push access to binary cache (requires admin-generated token)
attic login bureau http://attic.internal:5580 <push-token>

# Verify push works
nix build .#bureau
attic push bureau ./result
```

### Hub Bootstrap (the infrastructure host)

Done once, manually, following the deployment playbook (PLAYBOOK.md).
Brings up Continuwuity + coturn + attic, generates root secrets,
bootstraps the homeserver. The hub may also be a dev and/or worker.

---

## Credential Count by Role

| Role | Secrets the machine holds | Provisioned by |
|------|--------------------------|----------------|
| Worker | 0 persistent (one-time password rotated on first boot) | operator via `bureau machine provision` |
| Dev | 1: attic push token | operator |
| Hub | 4: registration token + TURN secret + attic JWT secret + admin creds | operator (initial setup) |
| Operator | 1: personal Matrix password | self |

Machine age keypairs are self-generated and don't count as provisioned
secrets. TURN credentials are ephemeral and obtained automatically.
Substituter config (URL + public key) is not secret. Worker machines hold
zero long-term provisioned secrets — their Matrix session is obtained via
one-time password rotation and their keypair is self-generated.

---

## Machine Lifecycle Commands

```bash
# Provision a new machine (run on the hub)
bureau machine provision machine/worker-01 --credential-file ./bureau-creds --output bootstrap.json

# List all fleet machines
bureau machine list --credential-file ./bureau-creds

# Remove a machine from the fleet
bureau machine decommission machine/worker-01 --credential-file ./bureau-creds
```

## Systemd Units

Service unit files are embedded in the Bureau binary via `lib/content/systemd/` and
written to `/etc/systemd/system/` by `bureau machine doctor --fix` or the bootstrap
script. See `deploy/systemd/machine.conf.example` for the EnvironmentFile format.
