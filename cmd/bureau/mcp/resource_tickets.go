// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// ticketGrantAction is the grant action pattern for ticket resources.
const ticketGrantAction = "resource/ticket/**"

// ticketSocketPath is the well-known ticket service socket inside a
// sandbox.
const ticketSocketPath = "/run/bureau/service/ticket.sock"

// ticketTokenPath is the well-known ticket service token inside a
// sandbox.
const ticketTokenPath = "/run/bureau/service/token/ticket.token"

// streamDialTimeout is the timeout for dialing the ticket service
// socket for streaming subscribe connections. Longer than the
// one-shot dial timeout because the connection is long-lived.
const streamDialTimeout = 10 * time.Second

// streamReconnectInterval is the base delay between reconnection
// attempts when a streaming subscribe connection drops.
const streamReconnectInterval = 2 * time.Second

// streamReconnectMaxInterval is the maximum backoff delay.
const streamReconnectMaxInterval = 30 * time.Second

// TicketProvider exposes ticket data from the ticket service as MCP
// resources. It supports three views per room:
//
//   - bureau://tickets/{room_alias}: all tickets
//   - bureau://tickets/{room_alias}/ready: actionable tickets
//   - bureau://tickets/{room_alias}/blocked: blocked tickets
//
// Read operations use one-shot ServiceClient calls. Subscribe
// operations dial the ticket service directly and consume the
// streaming subscribe protocol (phased snapshot + live put/remove
// frames), translating mutations into MCP resource notifications.
type TicketProvider struct {
	socketPath string
	tokenPath  string
	logger     *slog.Logger
}

// TicketProviderConfig configures a TicketProvider. All fields are
// optional; defaults use the well-known sandbox paths.
type TicketProviderConfig struct {
	SocketPath string
	TokenPath  string
	Logger     *slog.Logger
}

// NewTicketProvider creates a ticket resource provider. If config is
// nil, defaults to well-known sandbox paths and a no-op logger.
func NewTicketProvider(config *TicketProviderConfig) *TicketProvider {
	provider := &TicketProvider{
		socketPath: ticketSocketPath,
		tokenPath:  ticketTokenPath,
		logger:     slog.Default(),
	}
	if config != nil {
		if config.SocketPath != "" {
			provider.socketPath = config.SocketPath
		}
		if config.TokenPath != "" {
			provider.tokenPath = config.TokenPath
		}
		if config.Logger != nil {
			provider.logger = config.Logger
		}
	}
	return provider
}

// Handles returns true for URIs starting with "bureau://tickets/".
func (p *TicketProvider) Handles(uri string) bool {
	return strings.HasPrefix(uri, "bureau://tickets/")
}

// List returns concrete resources for each tracked room and URI
// templates for parameterized access. Calls the ticket service's
// list-rooms action to discover available rooms.
func (p *TicketProvider) List(ctx context.Context, _ []schema.Grant) ([]resourceDescription, []resourceTemplate) {
	client, err := service.NewServiceClient(p.socketPath, p.tokenPath)
	if err != nil {
		p.logger.Warn("ticket provider: cannot create service client for list", "error", err)
		return nil, p.templates()
	}

	var rooms []roomInfo
	if err := client.Call(ctx, "list-rooms", nil, &rooms); err != nil {
		p.logger.Warn("ticket provider: list-rooms failed", "error", err)
		return nil, p.templates()
	}

	var resources []resourceDescription
	for _, room := range rooms {
		if room.Alias == "" {
			continue
		}

		// Strip the server name from the alias if present (e.g.,
		// "#ns/room:server" → "ns/room").
		alias := room.Alias
		if strings.HasPrefix(alias, "#") {
			alias = alias[1:]
		}
		if colonIndex := strings.IndexByte(alias, ':'); colonIndex != -1 {
			alias = alias[:colonIndex]
		}

		openCount := room.Stats.ByStatus["open"] + room.Stats.ByStatus["in_progress"]
		closedCount := room.Stats.ByStatus["closed"]
		description := fmt.Sprintf("Tickets in %s (%d open, %d closed)",
			room.Alias, openCount, closedCount)

		resources = append(resources, resourceDescription{
			URI:         "bureau://tickets/" + alias,
			Name:        "Tickets: " + alias,
			Description: description,
			MIMEType:    "application/json",
			Annotations: &resourceAnnotation{
				Audience: []string{"assistant"},
				Priority: 0.6,
			},
		})
	}

	return resources, p.templates()
}

// templates returns URI templates for ticket resources. These allow
// MCP clients to construct URIs for rooms not yet discovered.
func (p *TicketProvider) templates() []resourceTemplate {
	return []resourceTemplate{
		{
			URITemplate: "bureau://tickets/{room_alias}",
			Name:        "Tickets by room",
			Description: "All tickets in a room, identified by its alias localpart (e.g., ns/room).",
			MIMEType:    "application/json",
		},
		{
			URITemplate: "bureau://tickets/{room_alias}/ready",
			Name:        "Ready tickets by room",
			Description: "Actionable tickets: open with all blockers resolved and gates satisfied, plus in-progress tickets.",
			MIMEType:    "application/json",
		},
		{
			URITemplate: "bureau://tickets/{room_alias}/blocked",
			Name:        "Blocked tickets by room",
			Description: "Blocked tickets: open with at least one unresolved blocker or unsatisfied gate.",
			MIMEType:    "application/json",
		},
	}
}

// Read fetches ticket data from the ticket service. The URI determines
// the query: all tickets, ready only, or blocked only.
func (p *TicketProvider) Read(ctx context.Context, uri string) ([]resourceContent, error) {
	parsed, err := parseResourceURI(uri)
	if err != nil {
		return nil, err
	}
	if parsed.Service != "tickets" || parsed.RoomAlias == "" {
		return nil, fmt.Errorf("invalid ticket resource URI: %s", uri)
	}

	client, err := service.NewServiceClient(p.socketPath, p.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("creating ticket service client: %w", err)
	}

	// Determine which action to call based on the sub-resource.
	action := "list"
	switch parsed.SubResource {
	case "ready":
		action = "ready"
	case "blocked":
		action = "blocked"
	case "":
		action = "list"
	default:
		return nil, fmt.Errorf("unknown ticket sub-resource: %s", parsed.SubResource)
	}

	// The ticket service expects a room ID or alias string in the
	// "room" field. Pass the alias localpart with a "#" prefix so the
	// ticket service can resolve it.
	fields := map[string]any{
		"room": "#" + parsed.RoomAlias,
	}

	var result json.RawMessage
	if err := client.Call(ctx, action, fields, &result); err != nil {
		return nil, fmt.Errorf("ticket service %s: %w", action, err)
	}

	return []resourceContent{
		{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(result),
		},
	}, nil
}

// Subscribe streams ticket mutations from the ticket service and
// translates them into MCP resource notifications. The streaming
// connection uses the ticket service's subscribe protocol:
//
//   - Phased snapshot: closed deps → open tickets → open_complete →
//     remaining closed → caught_up
//   - Live events: put/remove frames as tickets change
//   - Heartbeats every 30 seconds
//   - Resync on buffer overflow
//
// The notify function is called on each put or remove frame after the
// caught_up marker, not during the initial snapshot (to avoid flooding
// the agent with notifications for existing state it hasn't read yet).
func (p *TicketProvider) Subscribe(ctx context.Context, uri string, notify func(string)) (func(), error) {
	parsed, err := parseResourceURI(uri)
	if err != nil {
		return nil, err
	}
	if parsed.Service != "tickets" || parsed.RoomAlias == "" {
		return nil, fmt.Errorf("invalid ticket resource URI for subscribe: %s", uri)
	}

	// Create a cancellable context for the subscription goroutine.
	subscribeContext, cancel := context.WithCancel(ctx)

	go p.subscribeLoop(subscribeContext, uri, parsed.RoomAlias, notify)

	return cancel, nil
}

// subscribeLoop manages the streaming connection to the ticket service
// with reconnection and backoff. Runs until the context is cancelled.
func (p *TicketProvider) subscribeLoop(ctx context.Context, uri, roomAlias string, notify func(string)) {
	backoff := streamReconnectInterval

	for {
		err := p.subscribeOnce(ctx, uri, roomAlias, notify)
		if ctx.Err() != nil {
			// Context cancelled — clean shutdown.
			return
		}

		p.logger.Warn("ticket subscribe stream disconnected, reconnecting",
			"uri", uri, "error", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Exponential backoff with cap.
		backoff = backoff * 2
		if backoff > streamReconnectMaxInterval {
			backoff = streamReconnectMaxInterval
		}
	}
}

// subscribeOnce runs a single streaming subscribe connection. Returns
// when the connection drops or the context is cancelled.
func (p *TicketProvider) subscribeOnce(ctx context.Context, uri, roomAlias string, notify func(string)) error {
	// Read the service token for authentication.
	tokenBytes, err := readTokenFile(p.tokenPath)
	if err != nil {
		return fmt.Errorf("reading ticket service token: %w", err)
	}

	// Dial the ticket service socket.
	dialer := net.Dialer{Timeout: streamDialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", p.socketPath)
	if err != nil {
		return fmt.Errorf("dialing ticket service: %w", err)
	}
	defer conn.Close()

	// Send the subscribe request as CBOR. The socket server reads
	// a single CBOR value, extracts the action and token, then
	// dispatches to the stream handler which takes ownership of
	// the connection.
	request := map[string]any{
		"action": "subscribe",
		"token":  tokenBytes,
		"room":   "#" + roomAlias,
	}
	if err := codec.NewEncoder(conn).Encode(request); err != nil {
		return fmt.Errorf("sending subscribe request: %w", err)
	}

	// Read frames from the ticket service.
	decoder := codec.NewDecoder(conn)
	caughtUp := false

	for {
		// Check context before blocking on read.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var frame subscribeFrame
		if err := decoder.Decode(&frame); err != nil {
			return fmt.Errorf("reading subscribe frame: %w", err)
		}

		switch frame.Type {
		case "put", "remove":
			// Only notify after caught_up to avoid flooding the
			// agent with notifications for the initial snapshot.
			if caughtUp {
				notify(uri)
				// Reset backoff on successful event.
				// (Handled implicitly by the reconnect loop creating
				// a new connection with reset backoff.)
			}

		case "open_complete":
			// Interactive rendering phase reached. No notification
			// needed — the agent reads the resource explicitly.

		case "caught_up":
			caughtUp = true
			// Send one notification so the agent knows the full
			// snapshot is available for reading.
			notify(uri)

		case "heartbeat":
			// Connection is alive. No action needed.

		case "resync":
			// Buffer overflow — ticket service will send a fresh
			// snapshot. Reset caught_up so we don't notify during
			// the re-snapshot.
			caughtUp = false

		case "error":
			return fmt.Errorf("ticket service error: %s", frame.Message)
		}
	}
}

// subscribeFrame mirrors the ticket service's subscribe wire protocol.
// Defined here to avoid importing from cmd/bureau-ticket-service/.
type subscribeFrame struct {
	Type     string               `cbor:"type"`
	TicketID string               `cbor:"ticket_id,omitempty"`
	Content  ticket.TicketContent `cbor:"content,omitempty"`
	Stats    *ticketindex.Stats   `cbor:"stats,omitempty"`
	Message  string               `cbor:"message,omitempty"`
}

// roomInfo mirrors the ticket service's room info response for
// list-rooms. Defined here to avoid importing from
// cmd/bureau-ticket-service/.
type roomInfo struct {
	RoomID string            `cbor:"room_id"`
	Alias  string            `cbor:"alias,omitempty"`
	Prefix string            `cbor:"prefix,omitempty"`
	Stats  ticketindex.Stats `cbor:"stats"`
}

// readTokenFile reads raw token bytes from a file path. Returns an
// error if the file cannot be read or is empty.
func readTokenFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading token file %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("token file is empty: %s", path)
	}
	return data, nil
}

// GrantAction returns the grant action pattern for ticket resources.
func (p *TicketProvider) GrantAction() string {
	return ticketGrantAction
}
