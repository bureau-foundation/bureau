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
    "cache.infra.bureau.foundation-1:3hpghLePqloLp0qMpkgPy/i0gKiL/Sxl2dY8EHZgOeY="
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

**Current status**: Cache writes are commented out pending Determinate Nix
shipping Nix >= 2.33.2 (see [#15](https://github.com/bureau-foundation/bureau/issues/15)).
Nix 2.33.0–2.33.1 has a bug where curl's `--aws-sigv4` signs the
`accept-encoding` header; Cloudflare's edge modifies it before R2 sees the
request, causing 403 on every S3 operation
([NixOS/nix#15019](https://github.com/NixOS/nix/issues/15019), fixed in
2.33.2). The dev shell closure was manually uploaded and reads work.

## Signing

Store paths are signed with an Ed25519 key before upload. Nix verifies the
signature against the public key when downloading.

- **Key name**: `cache.infra.bureau.foundation-1`
- **Public key**: `cache.infra.bureau.foundation-1:3hpghLePqloLp0qMpkgPy/i0gKiL/Sxl2dY8EHZgOeY=`
- **Secret key**: stored in GitHub secret `NIX_CACHE_SIGNING_KEY`

The `-1` suffix is a rotation counter. When rotating: generate a new key with
suffix `-2`, add the new public key to `flake.nix` and CI config alongside
the old one (so both are trusted during transition), update the GitHub secret,
then remove the old public key after all cached paths signed with `-1` have
expired or been re-signed.

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

| Secret | Purpose |
|--------|---------|
| `NIX_CACHE_SIGNING_KEY` | Ed25519 secret key for signing store paths |
| `R2_ACCESS_KEY_ID` | S3-compatible access key for R2 writes |
| `R2_SECRET_ACCESS_KEY` | S3-compatible secret key for R2 writes |
| `R2_ENDPOINT` | Cloudflare S3 API endpoint, hostname only (no `https://` scheme) |

## Relevant files

- `flake.nix` — `nixConfig` block declares cache as a substituter
- `.github/workflows/ci.yaml` — cache configuration, push step
