// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// cacheParams holds the parameters for the fleet cache command.
type cacheParams struct {
	cli.SessionConfig
	cli.JSONOutput
	URL             string   `json:"url"              flag:"url"              desc:"substituter URL for pulling closures (e.g., https://cache.infra.bureau.foundation)"`
	Name            string   `json:"name"             flag:"name"             desc:"Attic cache name for push operations (e.g., main)"`
	PublicKey       []string `json:"public_key"       flag:"public-key"       desc:"Nix signing public key in name:base64 format (repeatable; replaces all keys)"`
	ComposeTemplate string   `json:"compose_template" flag:"compose-template" desc:"template reference for environment composition (e.g., bureau/template:nix-builder)"`
	DefaultSystem   string   `json:"default_system"   flag:"default-system"   desc:"default Nix system for environment composition (e.g., x86_64-linux)"`
}

// cacheResult is the JSON output of the cache command.
type cacheResult struct {
	Fleet ref.Fleet                `json:"fleet"  desc:"fleet reference"`
	Cache schema.FleetCacheContent `json:"cache"  desc:"fleet cache configuration"`
}

func cacheCommand() *cli.Command {
	var params cacheParams

	return &cli.Command{
		Name:    "cache",
		Summary: "View or update fleet Nix binary cache configuration",
		Description: `Read or write the Nix binary cache configuration for a fleet.

The argument is a fleet localpart in the form "namespace/fleet/name"
(e.g., "bureau/fleet/prod"). The server name is derived from the
connected session's identity.

Without config flags, displays the current cache configuration. With
flags (--url, --name, --public-key, --compose-template, --default-system),
performs a read-modify-write to update the configuration.

The fleet cache event (m.bureau.fleet_cache) records the substituter URL,
Attic cache name, and signing public keys that machines and compose
pipelines need. Push credentials (ATTIC_PUSH_TOKEN) flow through the
existing m.bureau.credentials system — this event carries only public
metadata, never secrets.

This event is admin-only (PL 100) because it controls binary provenance
for every machine in the fleet. The "bureau machine doctor" command
verifies that each machine's nix.conf is configured to trust the fleet
cache.

Multiple --public-key flags replace all existing keys. Use this during
key rotation: publish both old and new keys, propagate via "bureau
machine doctor --fix" on each machine, then remove the old key.`,
		Usage: "bureau fleet cache <namespace/fleet/name> [flags]",
		Examples: []cli.Example{
			{
				Description: "View current cache configuration",
				Command:     "bureau fleet cache bureau/fleet/prod --credential-file ./creds",
			},
			{
				Description: "Configure the fleet cache",
				Command:     "bureau fleet cache bureau/fleet/prod --url http://attic.internal:5580/main --name main --public-key 'bureau-prod:base64key...' --credential-file ./creds",
			},
			{
				Description: "Rotate signing keys (publish both during transition)",
				Command:     "bureau fleet cache bureau/fleet/prod --public-key 'bureau-prod-v1:oldkey' --public-key 'bureau-prod-v2:newkey' --credential-file ./creds",
			},
			{
				Description: "Configure environment composition defaults",
				Command:     "bureau fleet cache bureau/fleet/prod --compose-template bureau/template:nix-builder --default-system x86_64-linux --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &cacheResult{} },
		Annotations:    cli.Idempotent(),
		RequiredGrants: []string{"command/fleet/cache"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("fleet localpart is required (e.g., bureau/fleet/prod)")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (fleet localpart), got %d", len(args))
			}
			return runCache(ctx, logger, args[0], &params)
		},
	}
}

// hasCacheUpdates returns true if any cache flag was explicitly set.
func hasCacheUpdates(params *cacheParams) bool {
	return params.URL != "" || params.Name != "" || len(params.PublicKey) > 0 ||
		params.ComposeTemplate != "" || params.DefaultSystem != ""
}

// validateCacheURL checks that the URL is parseable and has an http or
// https scheme. Nix substituters are HTTP endpoints — other schemes
// (file://, s3://) have different trust properties and are not supported
// as fleet cache URLs.
func validateCacheURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("cannot parse URL: %w", err)
	}
	switch parsed.Scheme {
	case "http", "https":
		// Valid.
	case "":
		return fmt.Errorf("URL %q has no scheme (expected http:// or https://)", rawURL)
	default:
		return fmt.Errorf("URL %q has unsupported scheme %q (expected http or https)", rawURL, parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("URL %q has no host", rawURL)
	}
	return nil
}

// validatePublicKey checks that a Nix signing public key has the expected
// "name:base64" format. The name identifies the key (e.g.,
// "cache.infra.bureau.foundation-1") and the base64 portion is the
// ed25519 public key material.
func validatePublicKey(key string) error {
	colonIndex := strings.IndexByte(key, ':')
	if colonIndex < 0 {
		return fmt.Errorf("key %q missing colon separator (expected name:base64-key format)", key)
	}
	name := key[:colonIndex]
	if name == "" {
		return fmt.Errorf("key %q has empty name before colon", key)
	}
	base64Part := key[colonIndex+1:]
	if base64Part == "" {
		return fmt.Errorf("key %q has empty key material after colon", key)
	}
	return nil
}

func runCache(ctx context.Context, logger *slog.Logger, fleetLocalpart string, params *cacheParams) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	fleet, fleetRoomID, err := resolveFleetRoom(ctx, session, fleetLocalpart)
	if err != nil {
		return err
	}

	// Read existing cache config. An empty state key means singleton
	// per fleet.
	cacheConfig, err := messaging.GetState[schema.FleetCacheContent](ctx, session, fleetRoomID, schema.EventTypeFleetCache, "")
	if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return cli.Transient("reading fleet cache config from room %s: %w", fleetRoomID, err)
	}

	if !hasCacheUpdates(params) {
		// Read-only mode.
		result := cacheResult{
			Fleet: fleet,
			Cache: cacheConfig,
		}

		if done, emitError := params.EmitJSON(result); done {
			return emitError
		}

		if cacheConfig.URL == "" {
			fmt.Fprintf(os.Stdout, "No cache configured for fleet %s.\n", fleet.Localpart())
			fmt.Fprintf(os.Stdout, "  Run: bureau fleet cache %s --url <url> --public-key <key>\n", fleetLocalpart)
			return nil
		}

		writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(writer, "Fleet Cache: %s\n\n", fleet.Localpart())
		fmt.Fprintf(writer, "  URL:\t%s\n", cacheConfig.URL)
		fmt.Fprintf(writer, "  Name:\t%s\n", defaultString(cacheConfig.Name, "(none)"))
		fmt.Fprintf(writer, "  Public Keys:\n")
		if len(cacheConfig.PublicKeys) == 0 {
			fmt.Fprintf(writer, "    (none)\n")
		}
		for _, key := range cacheConfig.PublicKeys {
			fmt.Fprintf(writer, "    %s\n", key)
		}
		fmt.Fprintf(writer, "  Compose Template:\t%s\n", defaultString(cacheConfig.ComposeTemplate, "(none)"))
		fmt.Fprintf(writer, "  Default System:\t%s\n", defaultString(cacheConfig.DefaultSystem, "(none)"))
		writer.Flush()
		return nil
	}

	// Write mode: validate inputs, merge, and publish.
	if params.URL != "" {
		if err := validateCacheURL(params.URL); err != nil {
			return cli.Validation("invalid --url: %v", err)
		}
		cacheConfig.URL = params.URL
	}
	if params.Name != "" {
		cacheConfig.Name = params.Name
	}
	if len(params.PublicKey) > 0 {
		for _, key := range params.PublicKey {
			if err := validatePublicKey(key); err != nil {
				return cli.Validation("invalid --public-key: %v", err)
			}
		}
		cacheConfig.PublicKeys = params.PublicKey
	}
	if params.ComposeTemplate != "" {
		cacheConfig.ComposeTemplate = params.ComposeTemplate
	}
	if params.DefaultSystem != "" {
		cacheConfig.DefaultSystem = params.DefaultSystem
	}

	// Final validation: a cache config must have both URL and at least
	// one public key to be useful. Warn if incomplete.
	if cacheConfig.URL == "" {
		return cli.Validation("--url is required (or set it in a previous invocation)").
			WithHint("A fleet cache needs at least a URL and one public key.")
	}
	if len(cacheConfig.PublicKeys) == 0 {
		return cli.Validation("at least one --public-key is required").
			WithHint("Without a signing key, machines cannot verify closures from this cache.")
	}

	_, err = session.SendStateEvent(ctx, fleetRoomID, schema.EventTypeFleetCache, "", cacheConfig)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			return cli.Forbidden("cannot publish fleet cache config to %s: %w", fleetRoomID, err).
				WithHint("Fleet cache configuration requires admin power level (PL 100) in the fleet room.")
		}
		return cli.Transient("publishing fleet cache config to %s: %w", fleetRoomID, err)
	}

	result := cacheResult{
		Fleet: fleet,
		Cache: cacheConfig,
	}

	if done, emitError := params.EmitJSON(result); done {
		return emitError
	}

	logger.Info("updated fleet cache config",
		"fleet", fleet.Localpart(),
		"url", cacheConfig.URL,
		"name", cacheConfig.Name,
		"public_keys", len(cacheConfig.PublicKeys),
		"compose_template", cacheConfig.ComposeTemplate,
		"default_system", cacheConfig.DefaultSystem,
	)
	return nil
}
