// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/bureau-foundation/bureau/lib/authorization"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// ResourceProvider is the interface for a class of MCP resources. Each
// provider handles a URI prefix (e.g., "bureau://tickets/") and knows
// how to list available instances, read their content, and optionally
// watch for changes.
//
// Providers are registered on the Server at startup and dispatched by
// the resource request handlers based on URI matching.
type ResourceProvider interface {
	// Handles returns true if this provider owns the given URI. The
	// server routes resources/read and resources/subscribe to the
	// first provider that returns true.
	Handles(uri string) bool

	// List returns concrete resource descriptions and URI templates
	// available from this provider. The grants parameter is used to
	// filter resources the principal is authorized to access.
	List(ctx context.Context, grants []schema.Grant) ([]resourceDescription, []resourceTemplate)

	// Read returns the current content of a resource. The URI has
	// already been validated by Handles. Returns an error if the
	// resource cannot be read (service unavailable, unknown room, etc.).
	Read(ctx context.Context, uri string) ([]resourceContent, error)

	// Subscribe starts watching a resource for changes. The notify
	// function is called each time the resource's content changes.
	// Returns a cancel function that stops the watch and releases
	// resources. If the provider does not support subscriptions for
	// this URI, it returns a non-nil error.
	Subscribe(ctx context.Context, uri string, notify func(uri string)) (cancel func(), err error)

	// GrantAction returns the grant action pattern required to
	// access resources from this provider (e.g., "resource/ticket/**").
	GrantAction() string
}

// lockedEncoder wraps a json.Encoder with a mutex for concurrent
// writes. The MCP server's main request loop and background
// notification goroutines both write to the same output stream;
// the mutex serializes their writes to prevent interleaved JSON.
type lockedEncoder struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

// Encode serializes v as JSON followed by a newline, holding the
// mutex for the duration of the write.
func (e *lockedEncoder) Encode(v any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.encoder.Encode(v)
}

// parsedURI is the result of parsing a bureau:// resource URI.
type parsedURI struct {
	// Service is the resource category: "identity", "tickets", "fleet".
	Service string

	// RoomAlias is the room alias localpart for room-scoped resources.
	// Empty for resources that are not room-scoped (e.g., identity).
	RoomAlias string

	// SubResource is the optional sub-resource filter: "ready",
	// "blocked", "ranked". Empty for the default (all) view.
	SubResource string
}

// knownSubResources is the set of recognized sub-resource suffixes
// that are stripped from the URI path when parsing room-scoped
// resources. Room alias localparts always contain namespace structure
// (at least one "/") so they never collide with these single-segment
// suffixes.
var knownSubResources = map[string]bool{
	"ready":   true,
	"blocked": true,
	"ranked":  true,
}

// parseResourceURI parses a bureau:// URI into its components.
// Returns an error if the URI scheme is not "bureau" or the URI
// is otherwise malformed.
//
// Examples:
//
//	"bureau://identity"                     → {Service: "identity"}
//	"bureau://tickets/ns/room"              → {Service: "tickets", RoomAlias: "ns/room"}
//	"bureau://tickets/ns/room/ready"        → {Service: "tickets", RoomAlias: "ns/room", SubResource: "ready"}
//	"bureau://fleet/machines"               → {Service: "fleet", SubResource: "machines"}
func parseResourceURI(uri string) (parsedURI, error) {
	const prefix = "bureau://"
	if !strings.HasPrefix(uri, prefix) {
		return parsedURI{}, fmt.Errorf("unsupported URI scheme: %s", uri)
	}

	path := strings.TrimPrefix(uri, prefix)
	if path == "" {
		return parsedURI{}, fmt.Errorf("empty resource path: %s", uri)
	}

	// Split into service name and the remainder.
	slashIndex := strings.IndexByte(path, '/')
	if slashIndex == -1 {
		// No slash: the entire path is the service name (e.g., "identity").
		return parsedURI{Service: path}, nil
	}

	service := path[:slashIndex]
	remainder := path[slashIndex+1:]
	if remainder == "" {
		return parsedURI{Service: service}, nil
	}

	// For non-room-scoped services (fleet), the remainder is the
	// sub-resource directly.
	switch service {
	case "tickets":
		return parseTicketURI(service, remainder)
	default:
		// Treat the remainder as a sub-resource (e.g., fleet/machines).
		return parsedURI{Service: service, SubResource: remainder}, nil
	}
}

// parseTicketURI parses the remainder of a bureau://tickets/... URI.
// The remainder contains a room alias localpart and optionally a
// sub-resource suffix. The parser strips known suffixes from the end
// of the path.
func parseTicketURI(service, remainder string) (parsedURI, error) {
	// Check if the last path segment is a known sub-resource.
	lastSlash := strings.LastIndexByte(remainder, '/')
	if lastSlash != -1 {
		candidate := remainder[lastSlash+1:]
		if knownSubResources[candidate] {
			roomAlias := remainder[:lastSlash]
			if roomAlias == "" {
				return parsedURI{}, fmt.Errorf("empty room alias in URI: bureau://%s/%s", service, remainder)
			}
			return parsedURI{
				Service:     service,
				RoomAlias:   roomAlias,
				SubResource: candidate,
			}, nil
		}
	}

	// No known sub-resource suffix — the entire remainder is the room alias.
	return parsedURI{
		Service:   service,
		RoomAlias: remainder,
	}, nil
}

// resourceAuthorized checks whether the principal's grants authorize
// access to resources from the given provider. Uses the same grant
// evaluation as tool authorization.
func resourceAuthorized(grants []schema.Grant, provider ResourceProvider) bool {
	return authorization.GrantsAllow(grants, provider.GrantAction(), ref.UserID{})
}

// sendResourceNotification writes a notifications/resources/updated
// notification to the locked encoder. Called by subscription
// goroutines when a watched resource changes.
func sendResourceNotification(encoder *lockedEncoder, uri string) error {
	return encoder.Encode(notification{
		JSONRPC: "2.0",
		Method:  "notifications/resources/updated",
		Params:  resourceUpdatedParams{URI: uri},
	})
}

// sendResourcesListChanged writes a notifications/resources/list_changed
// notification. Sent when the set of available resources changes (e.g.,
// a new service becomes reachable in the sandbox).
func sendResourcesListChanged(encoder *lockedEncoder) error {
	return encoder.Encode(notification{
		JSONRPC: "2.0",
		Method:  "notifications/resources/list_changed",
	})
}

// sendToolsListChanged writes a notifications/tools/list_changed
// notification. Sent when the set of available tools changes (e.g.,
// grants are updated).
func sendToolsListChanged(encoder *lockedEncoder) error {
	return encoder.Encode(notification{
		JSONRPC: "2.0",
		Method:  "notifications/tools/list_changed",
	})
}
