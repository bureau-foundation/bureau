# Nix Binary Cache

Bureau runs its own Nix binary cache so that custom derivations (overridden
packages, environment closures, release binaries) don't need to be rebuilt
from source on every CI run or local `nix develop`.

## Cache tiers

| Tier | URL | Scope | TTL |
|------|-----|-------|-----|
| GitHub Actions cache | GitHub-managed | CI only (build deps + outputs) | 7 days inactive |
| Bureau R2 cache | `https://cache.infra.bureau.foundation` | CI + local dev | Permanent (Cloudflare R2) |
| Upstream Nix cache | `https://cache.nixos.org` | Everything in nixpkgs | Permanent |

The GitHub Actions cache (`magic-nix-cache-action`) automatically caches every
store path built during a CI run, including build-time dependencies like the Go
compiler and gomod2nix per-module derivations that aren't in any runtime closure.
This is the primary cache for CI build speed.

The R2 cache holds release artifacts for distribution: signed binary closures,
environment packages, dev shell closures. These serve local `nix develop` and
fleet machine deployments. R2 is permanent storage; the GitHub Actions cache
evicts entries after 7 days of inactivity.

## How it works

### Reads (CI and local dev)

Nix checks substituters in order. The flake declares the R2 cache via
`nixConfig`:

```nix
nixConfig = {
  extra-substituters = [ "https://cache.infra.bureau.foundation" ];
  extra-trusted-public-keys = [
    "cache.infra.bureau.foundation-1:3hpghLePqloLp0qMpkgPy/i0gKiL/Sxl2dY8EHZgOeY= cache.infra.bureau.foundation-2:e1rDOXBK+uLDTT+YU2UzIzkNHpLEaG2jCHZumlH1UmY="
  ];
};
```

For local dev, `~/.config/nix/nix.conf` needs `accept-flake-config = true`
to trust the flake's substituter declaration without prompting on every
invocation.

In CI, the substituter and public key are written to `/etc/nix/nix.custom.conf`
(which Determinate Nix's `nix.conf` includes via `!include`), followed by a
`systemctl restart nix-daemon` so the daemon picks up the new substituter.

### Writes (CI only, main branch)

After a successful build on main, the CI workflow:

- Writes the signing secret key to a temporary file (cleaned up via `trap`)
- Sets `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` env vars for R2
- Builds the dev shell derivation (`nix build --no-link` — necessary because
  `nix develop` fetches dependencies but doesn't realize the mkShell output)
- Signs the dev shell closure and the release binary closure
- Pushes both to R2 via `nix copy --to 's3://bureau-cache?endpoint=...&region=auto'`

Both the lint and build jobs push to the cache — any job that builds Nix
derivations should push, so parallel jobs can share store paths.

The push step uses `continue-on-error: true` so cache failures never block
CI. Only pushes on main trigger the upload (fork PRs don't receive secrets;
same-repo PRs do, but the `event_name == 'push'` guard prevents execution).

Cache writes are enabled on main branch merges. Nix 2.33.0–2.33.1 had a bug
where curl's `--aws-sigv4` signed the `accept-encoding` header; Cloudflare's
edge modified it before R2 saw the request, causing 403 on every S3 operation
([NixOS/nix#15019](https://github.com/NixOS/nix/issues/15019)). Fixed in
upstream Nix 2.33.2, shipped in Determinate Nix 3.16.0+.

## Signing

Store paths are signed with an Ed25519 key before upload. Nix verifies the
signature against the public key when downloading.

- **Active key name**: `cache.infra.bureau.foundation-2`
- **Active public key**: `cache.infra.bureau.foundation-2:e1rDOXBK+uLDTT+YU2UzIzkNHpLEaG2jCHZumlH1UmY=`
- **Previous key name**: `cache.infra.bureau.foundation-1` (trusted during transition)
- **Previous public key**: `cache.infra.bureau.foundation-1:3hpghLePqloLp0qMpkgPy/i0gKiL/Sxl2dY8EHZgOeY=`
- **Secret key**: stored in GitHub org-level secret `NIX_CACHE_SIGNING_KEY`

### Key naming

A Nix signing key file is a single line: `<name>:<base64-ed25519-key>`. The
name is not cryptographic material — it's an arbitrary string chosen when the
key is generated. But it is operationally critical because **Nix matches
signatures to trusted keys by name, not by cryptographic identity**.

When `nix store sign --key-file` signs a store path, the resulting narinfo
contains `Sig: <name>:<base64-signature>`. When a client wants to substitute
a path, it scans the narinfo's `Sig` lines for one whose `<name>` prefix
matches an entry in its `trusted-public-keys`. If no name matches, the path
is treated as unsigned and the substituter refuses it — even if the
cryptographic key material is identical under a different name.

This means `bureau:abc...` and `cache.infra.bureau.foundation-2:abc...`
are the same Ed25519 key producing valid signatures, but Nix treats them as
completely different identities. A secret key named `bureau` will produce
signatures that no client trusts unless `bureau:<public-key>` is in their
`trusted-public-keys` — regardless of whether the same key material appears
under a different name.

**The name in `NIX_CACHE_SIGNING_KEY` must match the name in `flake.nix`'s
`extra-trusted-public-keys`.** If the secret is `bureau:<secret-base64>` but
`flake.nix` trusts `cache.infra.bureau.foundation-2:<public-base64>`, every
path signed by CI will be present in R2 but invisible to the substituter.
Paths that carry pass-through signatures from upstream caches
(`cache.nixos.org-1`, `cache.flakehub.com`) will still substitute because
those key names are trusted by default.

To verify the key name matches, extract a narinfo from R2 and check the
`Sig:` line's prefix:

```bash
# Pick any store path hash known to be in R2
curl -s "https://cache.infra.bureau.foundation/<hash>.narinfo" | grep '^Sig:'
# Should show: Sig: cache.infra.bureau.foundation-2:<base64>
# If it shows: Sig: bureau:<base64>  — the key name is wrong
```

To fix a misnamed key, edit the GitHub secret: change the `<name>:` prefix
from whatever it currently is to `cache.infra.bureau.foundation-2:` (keeping
the base64 key material unchanged). The next CI run will sign new paths with
the correct name. `nix copy` skips paths that already exist in the
destination without comparing narinfo signatures, so stale narinfos persist
until the store path itself changes (e.g., a Go version bump produces a new
hash). The CI signature verification step catches this: it checks every
pushed narinfo for a trusted `Sig:` prefix and fails if any are untrusted.

### Key rotation

The numeric suffix is a rotation counter. When rotating: generate a new key
with an incremented suffix, add the new public key to `flake.nix` and CI
config alongside the old one (so both are trusted during transition), update
the GitHub secret, then remove the old public key after all cached paths
signed with the previous key have expired or been re-signed.

## Storage

The cache is a Cloudflare R2 bucket with a custom domain. R2 is
S3-compatible, so Nix's built-in S3 store backend works directly.

- **Bucket**: `bureau-cache`
- **Public URL**: `https://cache.infra.bureau.foundation`
- **S3 endpoint**: set in GitHub secret `R2_ENDPOINT`

Nix writes standard binary cache layout: `nix-cache-info` at the root,
`<hash>.narinfo` metadata files, and `nar/<hash>.nar.zst` compressed
archives.

## GitHub secrets

All cache secrets are org-level (`bureau-foundation` organization) so that
out-of-tree service repos (e.g., `cloudflare-tunnel`) can push to the same
cache.

| Secret | Purpose |
|--------|---------|
| `NIX_CACHE_SIGNING_KEY` | Ed25519 secret key for signing store paths |
| `R2_ACCESS_KEY_ID` | S3-compatible access key for R2 writes |
| `R2_SECRET_ACCESS_KEY` | S3-compatible secret key for R2 writes |
| `R2_ENDPOINT` | Cloudflare S3 API endpoint, hostname only (no `https://` scheme) |

## Fleet binary cache

Fleets declare their Nix binary cache configuration as a Matrix state event
(`m.bureau.fleet_cache`) in the fleet room. This is separate from the R2
public cache above — the R2 cache serves open-source Bureau artifacts for
CI and dev, while fleet caches serve fleet-specific composed environments
(sandbox closures, service derivations, host environment packages).

### Recording fleet cache config

```bash
bureau fleet cache bureau/fleet/prod \
  --url https://attic.internal:5580/main \
  --name main \
  --public-key 'bureau-prod:base64key...' \
  --credential-file ./creds
```

This publishes the substituter URL, Attic cache name, and signing public
keys to the fleet room as `m.bureau.fleet_cache`. The event is admin-only
(PL 100) because a malicious rewrite is a supply chain compromise — every
subsequent `nix-store --realise` on every fleet machine would pull
attacker-controlled binaries that pass signature verification.

Without flags, the command displays the current configuration:

```bash
bureau fleet cache bureau/fleet/prod --credential-file ./creds
```

### Machine configuration via doctor

`bureau machine doctor` reads the fleet cache config from the daemon status
file (the daemon syncs it from Matrix) and verifies the machine's nix.conf:

- **fleet cache configured**: fleet cache event exists in daemon status
- **nix substituter**: fleet cache URL in `substituters` or `extra-substituters`
- **nix trusted keys**: fleet signing keys in `trusted-public-keys` or `extra-trusted-public-keys`
- **cache reachable**: HTTP GET to `<url>/nix-cache-info` returns valid response

`bureau machine doctor --fix` writes missing configuration to
`/etc/nix/nix.custom.conf` (the file that Determinate Nix includes via
`!include`) and restarts `nix-daemon`.

### Key rotation

Fleet signing key rotation uses the multi-key support in the fleet cache event:

1. Generate a new signing keypair
2. Publish both old and new public keys to the fleet cache event:
   ```bash
   bureau fleet cache bureau/fleet/prod \
     --public-key 'bureau-prod-v1:oldkey' \
     --public-key 'bureau-prod-v2:newkey'
   ```
3. Run `bureau machine doctor --fix` on each fleet machine to propagate
   the new trusted keys to nix.conf
4. Switch Attic to sign with the new key
5. Once all store paths signed with the old key have been consumed or
   re-signed, remove the old key from the fleet cache event

### Push credentials

Push credentials (`ATTIC_PUSH_TOKEN` or equivalent) flow through the
existing `m.bureau.credentials` system. The fleet cache event carries only
public metadata — never secrets.

## Relevant files

- `flake.nix` — `nixConfig` block declares cache as a substituter
- `.github/workflows/ci.yaml` — cache configuration, push step
- `lib/schema/events_fleet.go` — `EventTypeFleetCache`, `FleetCacheContent`
- `cmd/bureau/fleet/cache.go` — `bureau fleet cache` command
- `cmd/bureau/machine/doctor.go` — nix cache configuration checks
