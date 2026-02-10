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
Runs the Nix dev shell, Bazel, and pre-commit hooks.

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
key is published as a Matrix state event in `#bureau/machines`. The
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

A new machine that will run Bureau principals. Needs three things from
the operator:

1. **Nix** — install determinate nix, configure substituter
2. **Bureau binaries** — `nix build .#bureau-launcher .#bureau-daemon`
   (pulled from cache, not built locally)
3. **Homeserver access** — registration token file + homeserver URL

```bash
# 1. Install Nix (if not present)
curl --proto '=https' --tlsv1.2 -sSf -L \
  https://install.determinate.systems/nix | sh -s -- install

# 2. Configure binary cache (read-only, no credentials needed)
sudo script/configure-substituter \
  http://attic.internal:5580/bureau \
  "bureau:<PUBLIC_KEY>"

# 3. Pull Bureau binaries from cache
nix build github:bureau-foundation/bureau#bureau-launcher
nix build github:bureau-foundation/bureau#bureau-daemon

# 4. First boot — launcher registers with homeserver
bureau-launcher \
  --homeserver http://matrix.internal:6167 \
  --registration-token-file /path/to/token \
  --machine-name machine/worker-01

# 5. Daemon connects to homeserver, syncs config, spawns principals
bureau-daemon --session-file /run/bureau/session.json
```

After step 4, the registration token file can be deleted. The machine
authenticates via its Matrix session from then on.

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
| Worker | 1: registration token (first boot only, then deleted) | operator |
| Dev | 2: registration token + attic push token | operator |
| Hub | 4: registration token + TURN secret + attic JWT secret + admin creds | operator (initial setup) |
| Operator | 1: personal Matrix password | self |

Machine age keypairs are self-generated and don't count as provisioned
secrets. TURN credentials are ephemeral and obtained automatically.
Substituter config (URL + public key) is not secret.

---

## What This Means for the Bootstrap Script

The future `script/bootstrap-machine` should:

- Accept a role (`--role worker|dev|hub`)
- Accept a homeserver URL and registration token file
- Install Nix if needed
- Configure the substituter (URL + public key are not secret, can be hardcoded or fetched from a well-known endpoint)
- Pull the appropriate binaries from the cache
- Run first-boot registration
- Optionally accept an attic push token for dev role
- Clean up the registration token after first boot

The registration token is the only secret that needs to transit to a new
machine. Everything else is either self-generated, derived, or obtained
via Matrix after registration.
