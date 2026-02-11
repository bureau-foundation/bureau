// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/bureau-foundation/bureau/lib/nix"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// prefetchEnvironment ensures a Nix store path and its full transitive
// closure exist in the local Nix store. Uses an os.Stat fast path to
// avoid forking nix-store on every reconcile cycle when the path is
// already present. Nix store paths are content-addressed and immutable,
// so existence of the top-level path guarantees closure integrity (Nix
// GC operates on closure boundaries).
//
// When the path is missing, delegates to d.prefetchFunc (defaulting to
// prefetchNixStore) which invokes nix-store --realise to fetch from
// configured substituters.
func (d *Daemon) prefetchEnvironment(ctx context.Context, storePath string) error {
	// Fast path: the store path already exists locally. Nix store paths
	// are written atomically (rename into /nix/store/) so there is no
	// race with concurrent fetches — we see either the full path or
	// nothing.
	if _, err := os.Stat(storePath); err == nil {
		return nil
	}

	d.logger.Info("prefetching nix environment", "store_path", storePath)

	prefetch := d.prefetchFunc
	if prefetch == nil {
		prefetch = prefetchNixStore
	}
	if err := prefetch(ctx, storePath); err != nil {
		return err
	}

	d.logger.Info("nix environment prefetched", "store_path", storePath)
	return nil
}

// prefetchBureauVersion ensures all non-empty store paths in a BureauVersion
// exist in the local Nix store. Each path is prefetched independently so that
// a failure on one component (e.g., launcher) does not prevent the others
// (e.g., daemon, proxy) from being prefetched. The caller can then compare
// whatever store paths are available.
//
// Returns the first error encountered, if any. On error, some store paths
// may have been fetched while others were not — the caller should check
// individual path existence before hashing.
func (d *Daemon) prefetchBureauVersion(ctx context.Context, version *schema.BureauVersion) error {
	paths := []struct {
		label string
		path  string
	}{
		{"daemon", version.DaemonStorePath},
		{"launcher", version.LauncherStorePath},
		{"proxy", version.ProxyStorePath},
	}

	for _, entry := range paths {
		if entry.path == "" {
			continue
		}
		if err := d.prefetchEnvironment(ctx, entry.path); err != nil {
			return fmt.Errorf("prefetching %s store path %s: %w", entry.label, entry.path, err)
		}
	}
	return nil
}

// prefetchNixStore invokes nix-store --realise to fetch a store path
// and its full transitive closure from configured substituters (binary
// caches in nix.conf). This is a no-op when the path is already valid
// in the local store.
func prefetchNixStore(ctx context.Context, storePath string) error {
	_, err := nix.RunStore(ctx, "--realise", storePath)
	return err
}
