# Nix Binary Cache

Bureau runs its own Nix binary cache so that custom derivations (overridden
packages, environment closures, release binaries) don't need to be rebuilt
from source on every CI run or local `nix develop`.

## Cache tiers

| Tier | URL | Scope | TTL |
|------|-----|-------|-----|
| Bureau R2 cache | `https://cache.infra.bureau.foundation` | CI + local dev | Permanent (Cloudflare R2) |
| Upstream Nix cache | `https://cache.nixos.org` | Everything in nixpkgs | Permanent |

The R2 cache holds anything that isn't in cache.nixos.org: version-pinned
compilers, custom derivations, dev shell closures, release binary closures,
and eventually environment closures for sandbox runtimes.

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
