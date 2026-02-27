# Credentials

[fundamentals.md](fundamentals.md) establishes that Bureau runs untrusted
code in sandboxed environments; [architecture.md](architecture.md)
describes the privilege separation between launcher and daemon;
[authorization.md](authorization.md) defines how principals receive
grants and how grants govern credential provisioning scope. This
document describes how Bureau stores, distributes, isolates, and
rotates secrets — the most security-sensitive part of the system.

---

## Why Credential Management

Every principal in Bureau — agent, service, connector, operator — needs
secrets to function. An LLM agent needs API keys. A code review agent
needs a Forgejo token. A speech-to-text service needs model access
credentials. The observation relay needs a Matrix token. These secrets
must reach the right principal on the right machine without ever
appearing in logs, command lines, environment listings, core dumps,
swap files, or conversation context.

The difficulty is not encryption — age handles that. The difficulty is
lifecycle: how do secrets get from a human operator's head into a
sandboxed process's memory, how do they rotate without downtime, how
do automated services provision credentials for other principals
without holding secrets they shouldn't see, and how does the system
recover when a machine is compromised. Bureau's credential system
addresses all of these through a write-only encrypted store distributed
via Matrix state events, with hardware-backed keys protecting the
decryption path and strict privilege separation ensuring that no
single process compromise exposes more than one principal's secrets.

---

## Design Principles

**No secrets on the command line.** CLI arguments appear in
`/proc/*/cmdline`, `ps` output, shell history, tmux scrollback, and
LLM conversation context. They are effectively public. Bureau accepts
secrets only through sealed channels: terminal password prompts (no
echo), stdin pipes, or tmpfs-backed editor sessions. Any code path
that places a secret in a CLI argument is a bug.

**Write-only store.** Credentials enter as plaintext through sealed
input, are encrypted immediately, and the tooling has no "decrypt and
display" command. The decryption path runs only inside the launcher,
and it delivers plaintext exclusively via stdin pipe to a proxy
process. There is no intermediate file, no log entry, no stdout
output.

**Hardware-backed keys where possible.** The operator's private key
lives on a YubiKey and is never extracted. The machine's private key
is sealed to the TPM and never appears on the filesystem in plaintext.
Secrets exist in cleartext only in process memory, briefly, in
memory regions locked against swap and excluded from core dumps, and
are explicitly zeroed after use.

**Encrypt at rest, everywhere.** Credential bundles stored in Matrix
are age-encrypted ciphertext. The storage layer — Matrix database,
backups, replication — never sees plaintext. A complete database dump
reveals credential names but not credential values.

**One distribution mechanism.** Matrix state events are the only way
credentials move between machines. There is no scp, no shared
filesystem, no manual file placement. Matrix provides audit trail,
access control, sync, and conflict resolution. When a credential
changes, every machine that subscribes to the relevant config room
sees the update through its normal sync loop.

**Principals, not agents.** The credential system manages secrets for
Matrix identities. It does not know or care whether the identity is
an LLM agent, a service, a connector, or a phone relay. Every
sandboxed workload is a principal. "Agent" is a label for principals
that happen to be autonomous — the credential infrastructure treats
them identically.

---

## Credential Taxonomy

Bureau manages several categories of secret material, each with
different provisioning sources, lifetimes, and rotation patterns.

**Matrix access tokens** are issued by the homeserver when a principal
is registered. The launcher holds the registration capability and
provisions tokens as part of sandbox creation. These tokens are
long-lived (until revoked) and are delivered to the proxy alongside
all other credentials. Rotation requires re-registration or admin API
token refresh.

**External API keys** (for LLM providers, cloud services, etc.) are
provisioned by a human operator through sealed input. They have
varying lifetimes depending on the provider and are rotated either
manually by the operator or automatically by a sysadmin principal
with access to the provider's key management API.

**OAuth refresh tokens** come from OAuth authorization flows and are
provisioned into a principal's credential bundle. The proxy handles
the access token lifecycle transparently — exchanging refresh tokens
for short-lived access tokens without external coordination. The
durable secret is the refresh token; access tokens are ephemeral and
never stored.

**Machine age keypairs** are generated locally at first boot. The
private key is sealed to the machine's TPM (or the best available
alternative) and never exists as a plaintext file on a persistent
filesystem. These keypairs are permanent for the machine's lifetime
and are the decryption keys for all credential bundles assigned to
that machine.

**Operator escrow keys** live on YubiKey hardware. Every credential
bundle is also encrypted to the operator's escrow key, enabling
break-glass recovery when a machine's TPM-sealed key is unavailable.
Recovery requires physical YubiKey presence and PIN entry.

**Registration tokens** are single-use secrets provided by the
operator at machine bootstrap. They authorize the launcher to register
a Matrix account for the machine. After use, they are zeroed and have
no further value.

**Connector admin credentials** grant a connector service full
management access to one external system (a Forgejo instance, a
GitHub organization, an S3 bucket). The connector uses this admin
access to provision per-principal credentials within the external
system. See "Connector-Managed Credentials" below.

**Service identity tokens** are short-lived bearer tokens for
inter-service authentication, described in
[authorization.md](authorization.md). They are not part of the
credential bundle system — they are minted on demand by the daemon
and verified against the daemon's public signing key.

---

## Key Hierarchy

The credential system rests on a hierarchy of cryptographic keys with
decreasing trust and increasing automation.

At the top is the **operator's YubiKey**. The operator's private key
never leaves the hardware token. It serves two purposes: signing
credential provisioning events (so the launcher can verify that a
human authorized the credentials) and decrypting escrow copies of
any credential bundle (break-glass recovery). Physical presence and
PIN entry are required for every operation.

Below the operator sit **machine age keypairs**, one per machine. Each
machine generates its own x25519 keypair at first boot. The private
key is sealed to the machine's TPM — copying the sealed blob to
another machine is useless because the TPM binding is
hardware-specific. At runtime, the private key is loaded into the
kernel keyring (not user-space memory, not swappable, not dumpable)
and the in-memory copy is zeroed. The machine's public key is
published to Matrix so that operators and connectors can encrypt
credentials to it.

At the bottom are **per-principal credential bundles**, encrypted to
one or more machine keys plus the operator's escrow key. A credential
bundle for a principal running on three machines is encrypted to all
three machine keys — any of the three launchers can decrypt it, but
no other machine can. Adding or removing a machine from the
recipient list requires re-encryption of the bundle.

This hierarchy means:

- Compromising a machine's TPM-sealed key exposes only that machine's
  credential bundles. Other machines' bundles remain opaque.
- Compromising the operator's YubiKey (physical theft plus PIN)
  exposes all credential bundles via escrow copies. This is the
  "game over" scenario, mitigated by physical security.
- Losing a machine (hardware failure, VM termination) orphans its
  credentials. The operator re-provisions affected principals on
  replacement hardware. The lost machine's Matrix account is
  deactivated to prevent stale state.
- A compromised machine is revoked via `bureau machine revoke`, which
  deactivates the account, tombstones the machine key and all
  credential state events, kicks the machine from all rooms, and
  publishes a fleet-wide revocation event. See the Attack Surface
  Analysis section for the full defense sequence.

---

## Machine Key Lifecycle

### Generation

On first boot, the launcher generates an age x25519 keypair in memory.
The private key immediately moves through three stages:

1. **Seal to TPM.** The private key is encrypted by the TPM using
   platform-specific PCR bindings. The sealed blob is written to disk
   — it is opaque without this specific TPM chip.
2. **Load into kernel keyring.** The plaintext private key is placed
   in the kernel's process keyring, where it is inaccessible to
   user-space memory inspection, not subject to swap, and excluded
   from core dumps.
3. **Zero the in-memory copy.** The original plaintext buffer is
   explicitly overwritten. From this point forward, all decryption
   operations use the keyring, not process memory.

After sealing, the launcher publishes the machine's public key to
Matrix as a state event in the machines room. This makes the public
key available to anyone who needs to encrypt credentials to this
machine.

### Subsequent Boots

On every boot after the first, the launcher reads the TPM-sealed blob
from disk, unseals it through the TPM, loads the plaintext into the
kernel keyring, and zeros the in-memory copy. The process converges
to the same runtime state regardless of whether this is the first
boot or the thousandth.

### Graceful Degradation

Not all machines have TPM 2.0 — Raspberry Pis, some cloud VMs, older
hardware. The launcher detects what is available and uses the best
option:

**TPM 2.0 with systemd-creds:** The private key is sealed to TPM PCR
state. The sealed blob is bound to this specific hardware; copying it
to another machine produces an unusable artifact. This is the
strongest protection.

**systemd-creds without TPM:** The private key is encrypted with a
host-specific key derived from `/var/lib/systemd/credential.secret`.
This survives reboots but not disk cloning to different hardware.
Weaker than TPM binding but still ties the key to a specific host
identity.

**Neither (bare metal without systemd, containers):** The private key
is age-encrypted with a passphrase. The operator enters the
passphrase at launcher start via terminal prompt or stdin. This
requires human interaction on every boot but provides encryption at
rest.

All three paths converge at runtime: private key in the kernel keyring,
user-space copies zeroed. The keyring provides uniform protection
regardless of the at-rest mechanism — not reachable via
`/proc/pid/mem`, not paged to swap, permission-controlled by UID,
optionally auto-expiring to force periodic re-read from the sealed
source.

---

## Credential Provisioning

### Operator Workflow

The operator provisions credentials using sealed input channels that
keep plaintext out of command lines, environment variables, and
persistent files.

**Single credential entry:** The operator runs the provisioning tool,
which prompts for each credential value using raw terminal mode — no
echo, no history. The secret exists only in process memory. The tool
encrypts the value to the target machine's public key (fetched from
Matrix) plus the operator's escrow key, signs the bundle with the
operator's YubiKey (requiring physical touch and PIN), and publishes
the encrypted bundle as a Matrix state event. The plaintext buffer is
zeroed before the tool exits.

**Bulk credential entry:** For principals with many credentials, the
tool mounts a tmpfs (RAM-backed, never written to disk), creates a
template file, and opens the operator's editor. The operator fills in
values, saves, and quits. The tool reads the tmpfs file, encrypts,
zeros, and unmounts. The kernel reclaims the RAM — no trace remains
on any persistent storage.

**Rotation:** The operator runs the provisioning tool in rotation mode,
specifying which credential to replace. The tool decrypts the
existing bundle (requiring the operator's YubiKey for the escrow copy,
or running on the target machine where the launcher can decrypt),
replaces the specified credential, re-encrypts, and publishes the
updated bundle. The target machine's daemon detects the state event
change through its sync loop and cycles the affected proxy.

### What Gets Published to Matrix

A credential state event in the machine's config room contains:

- **Version number** for schema evolution.
- **Principal identity** — which Matrix user ID owns these credentials.
- **Recipient list** — which machine identities and escrow keys can
  decrypt the bundle. This is metadata, not secret — it tells the
  system which machines can use these credentials.
- **Credential names** — the keys (not values) in the bundle, listed
  in plaintext. This allows auditing ("this principal has OpenAI and
  Anthropic keys") without decryption.
- **Ciphertext** — the age-encrypted blob containing a flat map of
  credential names to credential values. Encrypted to every recipient
  in the recipient list using standard age multi-recipient encryption
  (each recipient has a separately-encrypted copy of the symmetric
  file key in the age header).
- **Provisioner identity** — which Matrix user published this bundle.
- **Timestamp** — when the bundle was provisioned.
- **Signature** — an Ed25519 signature from the provisioner's signing
  key (the operator's YubiKey for human-provisioned credentials,
  or a service signing key for connector-provisioned credentials).

The signature prevents a compromised Matrix account from publishing
poisoned credentials. Without the operator's physical YubiKey, an
attacker who gains Matrix access cannot produce a valid signature,
and the launcher rejects unsigned or mis-signed bundles.

---

## Credential Delivery

### Launcher to Proxy

When the launcher creates a sandbox for a principal, it delivers
credentials to the proxy through a stdin pipe. No files, no
environment variables, no command-line arguments.

The flow:

1. The launcher reads the credential state event (the daemon forwarded
   the ciphertext via the IPC socket).
2. The launcher verifies the provisioner's signature on the event.
3. The launcher decrypts the ciphertext using the machine's private
   key from the kernel keyring.
4. The launcher generates or retrieves the principal's Matrix access
   token.
5. The launcher assembles a credential payload containing the Matrix
   token, the Matrix user ID, the homeserver URL, the decrypted
   credential map, and the principal's authorization grants.
6. The launcher forks the proxy process with its stdin connected to
   a pipe.
7. The launcher writes the CBOR-encoded payload to the pipe, then
   closes it.
8. The launcher zeros the plaintext payload in its own memory.

The proxy reads the payload from stdin at startup, parses it,
moves each credential value into memory that is locked against swap
and excluded from core dumps, and zeros the raw input buffer. From
this point, the proxy holds credentials in protected memory and
serves them through its socket API.

### What the Sandbox Sees

Nothing. The sandbox has no access to credentials. It sees Unix
sockets — a proxy socket for Matrix operations and service sockets
for external APIs. When the sandbox makes a request, the proxy
intercepts it, injects the appropriate credential (an API key header,
a bearer token, an OAuth access token), forwards the request, and
returns the response. The sandbox process cannot read the proxy's
memory: different PID namespace, dumpability disabled, ptrace blocked
by `no_new_privs`.

If the proxy crashes, the launcher restarts it and re-delivers
credentials via the same stdin pipe mechanism. The sandbox sees a
brief interruption in socket availability, then service resumes.
Credentials are never cached on disk.

---

## Privilege Separation

The machine-level Bureau runtime is split into two processes
specifically to contain credential exposure.

### The Launcher

The launcher is the privileged, minimal process. It holds the machine's
private key (in the kernel keyring), can decrypt credential bundles,
creates sandboxes (namespace setup), and spawns proxy processes with
credential delivery. It has no network access — no Matrix connection,
no transport connections, no listening network sockets. Its only IPC
channel is a Unix socket. Its codebase is deliberately small enough
to audit manually, its syscall surface is restricted by seccomp
allowlist, and it runs as a dedicated user with ambient capabilities
rather than as root.

The launcher validates every request from the daemon: Is the principal
assigned to this machine? Is the localpart safe? Is there a valid,
signed credential bundle? Is the request properly formed? The daemon
cannot trick the launcher into decrypting arbitrary bundles or
creating unassigned sandboxes.

### The Daemon

The daemon is the unprivileged, network-facing process. It connects to
Matrix, manages transport tunnels, handles service discovery, and makes
lifecycle decisions (when to start, stop, or idle principals). It
cannot access the kernel keyring, cannot decrypt credentials, and
cannot create namespaces. When it needs a sandbox created, it asks the
launcher and the launcher decides whether to comply.

### Compromise Impact

The separation creates distinct blast radii:

**A compromised sandbox** gives the attacker that sandbox's
stdin/stdout and the ability to reach the proxy socket. They get that
one principal's request capabilities (whatever the proxy is configured
to serve). They do not get the proxy's credential memory (different
PID namespace), other sandboxes' data, network access, or the ability
to escalate to the launcher.

**A compromised proxy** gives the attacker that principal's Matrix
token and API keys — everything the proxy holds in memory. They can
impersonate that principal. They do not get other proxies' credentials,
launcher access, or namespace creation capability.

**A compromised daemon** gives the attacker the Matrix sync stream
(room contents visible to the machine account) and the ability to
send lifecycle requests to the launcher. They cannot decrypt
credentials (no keyring access) and cannot create arbitrary sandboxes
(the launcher validates requests against Matrix configuration).

**A compromised launcher** is the worst case short of full host
compromise. The attacker has the machine's private key and can decrypt
any credential bundle assigned to this machine. They can create
sandboxes. Mitigation: the launcher's minimal codebase and tight
seccomp profile minimize the attack surface, and credentials assigned
to other machines remain unaffected.

**A compromised connector** gives the attacker admin access to one
external system. They can create and delete users, forge tokens, and
provision credentials to other principals through the provisioning
endpoint. The blast radius is scoped to that one external system —
other connectors' credentials, the launcher's keyring, and other
principals' non-connector credential bundles remain inaccessible. The
provisioning endpoint enforces key-name scoping: a Forgejo connector
can provision only Forgejo-related keys, not OpenAI keys.

**Full host compromise (root)** exposes everything on that machine.
Root can read the kernel keyring, attach to any process, and extract
memory contents. No software-only defense prevents this. The
mitigation is that credentials assigned to other machines remain
encrypted with those machines' keys, and the operator's YubiKey
remains physically separate.

---

## Credential Rotation

### Automated Rotation by Sysadmin Principals

A sysadmin principal runs in its own sandbox with its own proxy. Its
proxy holds credentials for provider admin APIs (key management
endpoints, token APIs). Rotation follows a predictable sequence:

1. The sysadmin detects upcoming expiry — either by polling
   `expires_at` metadata on credential state events or by receiving
   a notification from a monitoring principal.
2. The sysadmin calls the provider's API to generate a new key.
3. The sysadmin calls the provisioning endpoint on its own proxy,
   which encrypts the new key to the target machine's public key
   (fetched from Matrix) and publishes an updated credential state
   event.
4. The target machine's daemon detects the update through its sync
   loop.
5. The daemon forwards the new ciphertext to the launcher.
6. The launcher decrypts, restarts the affected proxy with the new
   credentials via stdin pipe.
7. The sysadmin verifies the new key works (probes the target
   principal's service endpoint).
8. The sysadmin revokes the old key at the provider.

The sysadmin never holds the target machine's private key. It passes
the new credential value to the provisioning endpoint, which handles
encryption using only public keys. The sysadmin sees the plaintext
value briefly (it generated it), but cannot decrypt the resulting
bundle or any other machine's bundles.

### Rollback on Failure

When the daemon detects a credential rotation (ciphertext change in the
`m.bureau.credentials` state event), it destroys the existing sandbox
and recreates it with the new credentials. If the new credentials are
bad, the health monitor triggers a rollback:

- **Before destroying the sandbox**, the daemon saves the previous
  working credential ciphertext, sandbox spec, and template. This
  parallels the structural restart path, which saves the previous spec
  for configuration rollbacks.

- **On health check failure**, the daemon uses the saved credentials
  rather than reading fresh from Matrix. Reading from Matrix would
  return the new (broken) credentials, creating an infinite
  destroy/recreate loop. The saved credentials are one-shot: consumed
  on rollback and not persisted across daemon restarts.

- **Notifications**: the daemon posts a `m.bureau.credentials_rotated`
  message with status `rolled_back` when it reverts to previous
  credentials, or `rollback_failed` if the rollback cannot complete.
  Operators see these alongside the health check notifications.

- **Daemon restart during rollback window**: the previous credentials
  are in-memory only. If the daemon restarts between the credential
  rotation and the health check failure, the saved credentials are
  lost. The daemon falls back to reading from Matrix (the new
  credentials), which may fail health checks again. The operator must
  re-provision working credentials manually. The rollback window is
  narrow (the health check grace period, typically 30-60 seconds).

The recommended rotation protocol — generate new key, provision,
verify, then revoke old — avoids most rollback scenarios. Rollback
handles the case where verification fails before revocation: the old
key is still valid at the provider, and the daemon restores it
automatically.

### Matrix Token Rotation

The launcher can rotate Matrix tokens directly. It holds the
registration capability (from the bootstrap registration token) and
can use the homeserver's admin API if the machine account has admin
privileges. Token rotation is a local operation that does not require
the credential provisioning flow.

### OAuth Auto-Refresh

For OAuth-based services, the proxy handles token refresh
transparently. The durable credential in the bundle is the refresh
token. When an access token expires, the proxy exchanges the refresh
token for a new access token without any external coordination. The
sandbox sees no interruption — requests block briefly during refresh,
then proceed with the new token.

### Expiry Tracking

Credential state events include an optional expiry timestamp. A
monitoring principal (or the sysadmin itself) polls for upcoming
expirations. Alerts flow through Matrix messaging — the same
structured communication channel all Bureau principals use.

---

## Connector-Managed Credentials

Some Bureau services hold admin-level credentials for external systems
and provision per-principal credentials within those systems. This
extends the sysadmin rotation pattern into a full credential lifecycle:
where sysadmin principals rotate individual keys, connector services
own creation, delivery, rotation, and revocation for an external
system's identity model.

### The Pattern

A connector service receives its own admin credential through the
standard operator provisioning flow. This admin credential grants the
connector full management access to one external system. The
connector then uses that admin access to create per-principal
identities and tokens within the external system.

For example, a Forgejo connector receives a Forgejo admin API token
from the operator. When a new principal needs Forgejo access (declared
by the principal's template), the connector:

1. Creates a user account in Forgejo for the principal (mapping
   Matrix localpart to Forgejo username).
2. Generates a scoped API token for that user.
3. Calls the provisioning endpoint on its own proxy to deliver the
   token into the target principal's credential bundle.
4. The provisioning endpoint encrypts to the target machine's public
   key and the operator's escrow key, then publishes an updated
   credential state event.
5. The target machine's daemon picks up the change through its sync
   loop, and the launcher cycles the principal's proxy with the new
   credential included.

The connector never sees the target principal's other credentials.
The provisioning endpoint merges the new key into the existing
bundle: it fetches the current ciphertext, decrypts (the connector's
proxy runs on the same machine as a launcher that can decrypt),
inserts the new key, re-encrypts to the same recipients, and
publishes the updated event.

### Lifecycle

**Creation** is triggered by principal lifecycle events. When the
daemon activates a principal whose template declares a connector
dependency, the connector observes the new principal and provisions
credentials for it. The principal's proxy does not start serving
requests on the external socket until the credential arrives.

**Rotation** follows the same flow as sysadmin rotation. The
connector generates a new token via the external admin API,
provisions it into the principal's bundle, verifies it works, then
revokes the old token.

**Revocation** is triggered by principal deactivation. The connector
watches for principals being deactivated and cleans up the
corresponding external identity and tokens. This ensures that
deactivated principals leave no dangling access in external systems.

**Emergency revocation** is triggered by `bureau machine revoke`,
which publishes an `m.bureau.credential_revocation` event to
`#bureau/machine`. Connectors watch for this event and revoke all
external tokens associated with the affected principals. Unlike
normal revocation (which handles one principal at a time), emergency
revocation invalidates all principals on a compromised machine
simultaneously.

### Signing Without the Operator's YubiKey

Credential events published by connector services cannot carry the
operator's YubiKey signature — the YubiKey is a physical device held
by the operator, not accessible to automated services. For
connector-provisioned credentials, the launcher trusts the event
based on layered verification:

- **Matrix identity:** the event was published by a known service
  principal with room-level permissions to publish credential state
  events. The homeserver enforces power levels.
- **Authorization grants:** the service has a credential provisioning
  grant scoped to specific key names (as described in
  [authorization.md](authorization.md)). A Forgejo connector has
  `credential/provision/key/FORGEJO_TOKEN` but not
  `credential/provision/key/OPENAI_API_KEY`. The provisioning
  endpoint checks this grant before accepting the request.
- **Key-name scoping:** connector-provisioned events can only add or
  update keys that the connector is authorized to manage. This is
  the intersection of the authorization grant (which keys the
  connector may provision) and the Matrix power level (which state
  events the connector may write).

This is weaker than operator YubiKey signatures but sufficient for
automated credential management. The alternative — requiring physical
YubiKey touch for every automated token rotation — is operationally
unusable. The mitigation is defense in depth: the connector runs in
a sandbox, its admin token is scoped to one system, and the
provisioning endpoint enforces key-name scoping.

A further hardening option: connectors could have their own signing
keypair, generated at provisioning time and stored in the connector's
credential bundle. The launcher would verify service signatures
against the connector's published public key. This is weaker than the
operator's YubiKey (software key vs. hardware key) but provides
cryptographic attribution — verifiable proof of which service
published a credential event.

### Generalization Across Connectors

Every connector service follows this pattern with variations in what
the admin credential grants and whether per-principal credentials
are needed:

- **Forgejo / GitHub:** admin API token provisions per-user API
  tokens. Matrix localpart maps to external username.
- **S3 / object storage:** IAM admin credentials provision scoped
  IAM credentials, or the connector acts as sole consumer with its
  own credentials mediating all access through its socket API.
- **Slack / messaging:** a single bot token serves all principals.
  No per-principal credential needed — the connector posts on behalf
  of each principal using its identity mapping.
- **Container registry:** admin credentials provision per-principal
  pull tokens for image access.

Not all connectors need per-principal credentials. The per-principal
flow applies only when the external system has its own identity model
and principals interact with it directly (git push to Forgejo, API
calls to GitHub). When the connector mediates all access through its
own socket (like an artifact service backed by S3), its own
credentials suffice.

---

## Bootstrap

### New Machine

The operator provides a single-use registration token to the launcher
through stdin (never as a CLI argument). The launcher:

1. Reads and zeros the registration token.
2. Generates an age x25519 keypair.
3. Seals the private key to the TPM (or best available alternative).
4. Loads the private key into the kernel keyring.
5. Registers a Matrix account for the machine using the registration
   token.
6. Publishes the machine's public key to the machines room in Matrix.
7. Starts listening for daemon requests on its Unix socket.
8. Launches the daemon as an unprivileged child process.

The daemon connects to Matrix, begins syncing, reads the machine's
configuration (which principals to run), and for each assigned
principal requests the launcher to create a sandbox. The launcher
decrypts credentials for each principal and spawns proxies with
credential delivery. The machine enters steady state.

### Adding Credentials

After a machine is bootstrapped, the operator provisions credentials
from any machine with Matrix access:

1. The provisioning tool fetches the target machine's public key from
   Matrix.
2. The operator enters credential values through sealed input.
3. The tool encrypts to the target machine's key plus the operator's
   escrow key, signs with the operator's YubiKey, and publishes.
4. The target machine's daemon sees the state event through its sync
   loop.
5. The daemon forwards the ciphertext to the launcher.
6. The launcher decrypts, verifies the signature, and either delivers
   to the running proxy (if the principal is active) or stores for
   the next launch.

### Ephemeral Cloud Machines

Cloud VMs bootstrap through cloud-init or user-data scripts. The
registration token is injected via the cloud provider's secret
mechanism (AWS Secrets Manager, GCP Secret Manager, or equivalent)
and passed to the launcher through a file path. After registration,
the launcher publishes its public key and the operator (or an
automation principal) provisions credentials. When the VM terminates,
the TPM-sealed key is destroyed with the hardware. The machine's
Matrix account is deactivated to prevent stale state.

### Machines Without TPM

Machines without TPM 2.0 fall back through the degradation hierarchy
described in "Machine Key Lifecycle." A Raspberry Pi without TPM and
without systemd uses passphrase-encrypted key storage, requiring
passphrase entry on each boot. The runtime security properties are
identical once the key reaches the kernel keyring — the difference
is only in at-rest protection and boot-time friction.

---

## Deployment Scenarios

### Single Developer Machine

One machine, one daemon, a handful of principals. The operator is also
the machine user. TPM seals the machine key. Credentials are
provisioned locally — the provisioning tool runs on the same machine
and encrypts to the local machine's public key. The escrow copy goes
to the operator's YubiKey for recovery if the TPM fails.

### Small Fleet

A few machines (home lab, small team), each with their own daemon.
The operator provisions credentials from their workstation, which
requires the YubiKey for signing. Matrix runs on one of the machines
or a dedicated small server. Registration tokens are distributed
manually via SSH. Each machine decrypts only its own bundles — a
compromised home automation Pi does not expose the GPU server's API
keys.

### Cloud Deployment

VMs spin up with cloud-init, register with Matrix, and receive
configuration and credentials. When a VM terminates, its TPM-sealed
key is destroyed with the hardware. Credentials assigned to the
terminated machine's identity are orphaned — the operator or a
sysadmin principal cleans up by deactivating the Matrix account and
removing the credential state events.

Auto-scaling integrates with fleet management (see
[fleet.md](fleet.md)): a provisioning principal watches load metrics,
requests new VMs from the cloud provider, and once a VM registers,
encrypts credentials to the new machine and assigns principals to
run.

### Mixed Environments

Cloud VMs for heavy compute, edge devices for local tasks, developer
workstations for interactive work — all participate in the same
Matrix space. Cross-network service routing (see
[architecture.md](architecture.md)) handles connectivity. Credentials
are per-machine, so the security boundary follows the physical
boundary: each machine's compromise scope is limited to its own
credential bundles.

---

## Attack Surface Analysis

### What the System Protects Against

**Compromised storage (Matrix database backup leaked).** All
credential values are age-encrypted. The attacker sees credential
names and encrypted blobs. Without a machine's private key or the
operator's YubiKey, the ciphertext is opaque.

**Sandbox escape (principal breaks out of bwrap isolation).** The
attacker reaches the proxy process for that sandbox. They get that
one principal's request capabilities. They cannot reach other proxies
(separate processes, separate PIDs), the launcher (Unix socket with
request validation), or the daemon (unprivileged, holds no
credentials).

**Compromised daemon (Matrix or transport vulnerability).** The
attacker controls the network-facing process. They can observe the
Matrix sync stream and request sandbox operations from the launcher.
They cannot decrypt credentials (no keyring access) and cannot
create arbitrary sandboxes (the launcher validates every request
against Matrix configuration).

**Compromised launcher.** The attacker has the machine's private key
and can decrypt all credential bundles assigned to this machine. They
can create sandboxes. The launcher's minimal codebase and tight
seccomp profile reduce the likelihood of this scenario. Credentials
assigned to other machines are not affected.

**Operator workstation compromised (without YubiKey).** The attacker
has the operator's Matrix session but not the physical YubiKey. They
cannot provision credentials (signing requires the YubiKey) or
decrypt escrow copies (decryption requires the YubiKey). They can
read Matrix room contents visible to the operator, which includes
credential names but not values.

**Compromised connector service.** The attacker gets the connector's
admin credential for one external system. They can manipulate that
system (create/delete users, forge tokens) and provision credentials
to other principals via the scoped provisioning endpoint. They cannot
access other connectors' credentials, the launcher's keyring, or
other principals' non-connector credential bundles. The provisioning
endpoint's key-name scoping prevents cross-contamination.

**Compromised machine (detected by operator).** The operator runs
`bureau machine revoke <machine>` which executes three defense layers:

- **Layer 1 — Machine isolation (seconds):** The machine's Matrix
  account is deactivated via admin API. The daemon detects the auth
  failure (`M_UNKNOWN_TOKEN` from `/sync`), destroys all running
  sandboxes via launcher IPC, and exits. No cooperation from the
  compromised machine is needed — the homeserver enforces the
  deactivation.
- **Layer 2 — State cleanup (seconds):** Credential state events are
  tombstoned (set to `{}`), machine key and status are cleared, and
  the machine is kicked from all rooms. An
  `m.bureau.credential_revocation` event is published to
  `#bureau/machine` for fleet-wide notification.
- **Layer 3 — Token expiry (≤5 minutes):** Outstanding service tokens
  expire via natural TTL. The daemon's emergency shutdown also pushes
  revocation notices to reachable services for faster invalidation.

The attacker retains whatever credentials were in sandbox memory at
the time of compromise, but all Matrix-side state is invalidated and
the machine can no longer authenticate to any Bureau service.

### What the System Does Not Protect Against

**Full host compromise (root on the machine).** Root can read the
kernel keyring, attach to any process, and extract memory contents.
No software-only solution prevents this. The mitigation is hardware:
TPM-sealed keys mean the private key material lives in the TPM chip,
not extractable even by root. But decrypted credential payloads in
proxy memory can be read by root via `/proc/pid/mem`.

**Physical access to the machine.** Cold boot attacks, bus sniffing,
TPM interposer attacks — these are out of scope for a software
system. Physical security is the mitigation.

**A malicious operator.** The operator can provision arbitrary
credentials. The YubiKey prevents impersonation (nobody else can sign
as the operator) but does not prevent the operator from acting
maliciously. Multi-operator approval (requiring multiple YubiKeys to
co-sign) could mitigate this but adds operational friction.

---

## Relationship to Other Design Documents

- [fundamentals.md](fundamentals.md) defines the sandbox and proxy
  primitives that credentials flow through.
- [architecture.md](architecture.md) describes the launcher/daemon
  privilege separation that protects the decryption path.
- [authorization.md](authorization.md) defines the grant model that
  governs credential provisioning scope — which connectors can
  provision which keys, which principals can access which services.
- [fleet.md](fleet.md) describes machine provisioning and
  deprovisioning, which interacts with credential lifecycle (new
  machines need credentials, terminated machines need cleanup).
- [information-architecture.md](information-architecture.md) defines
  the Matrix room structure where credential state events are stored.
- [stewardship.md](stewardship.md) — credential rotation stewardship
  ensures affected project leads approve before shared credentials
  are rotated. Stewardship declarations for `credential/**` resources
  auto-configure review gates on rotation tickets. The per-principal
  signing hardening described in stewardship.md extends the proxy
  credential architecture to provide cryptographic attribution of
  review dispositions.
