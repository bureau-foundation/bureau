// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/mcp"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestMCPResourceTicketSubscribe exercises the full MCP resource
// subscribe → notify → read flow with a real ticket service. An
// in-process MCP server connects to the ticket service's socket,
// subscribes to ticket resources, receives notifications when tickets
// change, and reads updated resource state.
//
// This validates the end-to-end path that a sandboxed agent uses to
// stay informed about ticket state without polling.
func TestMCPResourceTicketSubscribe(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, fleet, "mcp-res")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Deploy ticket service.
	ticketSvc := deployTicketService(t, admin, fleet, machine, "mcp-res")

	// Create a project room with an alias. The MCP resource URI requires
	// an alias localpart to identify the room. The ticket service resolves
	// bare localparts against its server name to find matching rooms.
	roomAlias := "mcp-res-project"
	room, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:   "MCP Resource Test Project",
		Alias:  roomAlias,
		Invite: []string{machine.UserID.String()},
	})
	if err != nil {
		t.Fatalf("create project room: %v", err)
	}
	projectRoomID := room.RoomID

	// Configure tickets in the room (production path).
	enableTicketsInRoom(t, admin, projectRoomID, ticketSvc, "tkt", nil)

	// Mint a service token for the admin so the MCP TicketProvider can
	// authenticate to the ticket service. Write it to a temp file since
	// TicketProvider reads tokens from disk.
	adminUserID := admin.UserID()
	tokenBytes := mintTestServiceTokenForUser(t, machine, adminUserID, "ticket",
		[]servicetoken.Grant{{Actions: []string{ticket.ActionAll}}})

	tokenDir := t.TempDir()
	tokenPath := filepath.Join(tokenDir, "ticket.token")
	if err := os.WriteFile(tokenPath, tokenBytes, 0600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	// Create the TicketProvider pointing at the real ticket service socket.
	provider := mcp.NewTicketProvider(&mcp.TicketProviderConfig{
		SocketPath: ticketSvc.SocketPath,
		TokenPath:  tokenPath,
	})

	// Create an MCP server with the provider. The command tree is minimal
	// since this test only exercises resources, not tools. The grants
	// authorize resource access.
	root := &cli.Command{Name: "bureau"}
	grants := []schema.Grant{{Actions: []string{"resource/ticket/**"}}}
	server := mcp.NewServer(root, grants, mcp.WithResourceProvider(provider))

	// Start the MCP server in a background goroutine using io.Pipe for
	// interleaved send/receive.
	pipe := newMCPPipe(t, server)

	// --- Protocol: initialize ---

	pipe.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "1.0"},
		},
	})
	initResponse := pipe.readMessage()
	if initResponse["error"] != nil {
		t.Fatalf("initialize failed: %v", initResponse["error"])
	}

	// Verify the server advertises resource capabilities.
	result, ok := initResponse["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize result is not a map: %T", initResponse["result"])
	}
	capabilities, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities is not a map: %T", result["capabilities"])
	}
	resources, ok := capabilities["resources"].(map[string]any)
	if !ok {
		t.Fatalf("resources capability not advertised: %v", capabilities)
	}
	if subscribe, ok := resources["subscribe"].(bool); !ok || !subscribe {
		t.Errorf("resources.subscribe = %v, want true", resources["subscribe"])
	}

	// Send initialized notification (no response expected).
	pipe.send(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})

	// --- Protocol: resources/subscribe ---

	resourceURI := "bureau://tickets/" + roomAlias
	pipe.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "resources/subscribe",
		"params":  map[string]any{"uri": resourceURI},
	})
	subscribeResponse := pipe.readMessage()
	if subscribeResponse["error"] != nil {
		t.Fatalf("resources/subscribe failed: %v", subscribeResponse["error"])
	}

	// Wait for the caught_up notification. The TicketProvider sends one
	// notification/resources/updated after the initial snapshot completes.
	caughtUpNotification := pipe.readUntilNotification(t, "notifications/resources/updated")
	params, ok := caughtUpNotification["params"].(map[string]any)
	if !ok {
		t.Fatalf("caught_up notification params is not a map: %T", caughtUpNotification["params"])
	}
	if notifyURI, ok := params["uri"].(string); !ok || notifyURI != resourceURI {
		t.Errorf("caught_up notification URI = %q, want %q", params["uri"], resourceURI)
	}

	// --- Create a ticket externally ---

	// Use a ServiceClient with the same token to create a ticket via the
	// ticket service socket. This triggers a put frame on the subscribe
	// stream, which the TicketProvider translates to an MCP notification.
	ticketClient := service.NewServiceClientFromToken(ticketSvc.SocketPath, tokenBytes)
	var createResult struct {
		ID   string `json:"id"`
		Room string `json:"room"`
	}
	if err := ticketClient.Call(ctx, "create", map[string]any{
		"room":     projectRoomID.String(),
		"title":    "MCP resource test ticket",
		"type":     "task",
		"priority": 2,
	}, &createResult); err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	if createResult.ID == "" {
		t.Fatal("ticket creation returned empty ID")
	}
	t.Logf("created ticket %s in room %s", createResult.ID, createResult.Room)

	// Wait for the put notification.
	putNotification := pipe.readUntilNotification(t, "notifications/resources/updated")
	params, ok = putNotification["params"].(map[string]any)
	if !ok {
		t.Fatalf("put notification params is not a map: %T", putNotification["params"])
	}
	if notifyURI, ok := params["uri"].(string); !ok || notifyURI != resourceURI {
		t.Errorf("put notification URI = %q, want %q", params["uri"], resourceURI)
	}

	// --- Protocol: resources/read ---

	pipe.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "resources/read",
		"params":  map[string]any{"uri": resourceURI},
	})
	readResponse := pipe.readMessage()
	if readResponse["error"] != nil {
		t.Fatalf("resources/read failed: %v", readResponse["error"])
	}

	// Parse the resource content. The ticket provider returns a JSON
	// array of tickets in the "text" field of the first content block.
	readResult, ok := readResponse["result"].(map[string]any)
	if !ok {
		t.Fatalf("read result is not a map: %T", readResponse["result"])
	}
	contents, ok := readResult["contents"].([]any)
	if !ok || len(contents) == 0 {
		t.Fatalf("read result has no contents: %v", readResult)
	}
	firstContent, ok := contents[0].(map[string]any)
	if !ok {
		t.Fatalf("first content is not a map: %T", contents[0])
	}
	textJSON, ok := firstContent["text"].(string)
	if !ok {
		t.Fatalf("content text is not a string: %T", firstContent["text"])
	}

	// The ticket service returns ticket data as JSON. Parse it and
	// verify the ticket we created is present.
	var tickets []json.RawMessage
	if err := json.Unmarshal([]byte(textJSON), &tickets); err != nil {
		t.Fatalf("parse ticket list JSON: %v\nraw: %s", err, textJSON)
	}
	if len(tickets) == 0 {
		t.Fatal("ticket list is empty after creating a ticket")
	}

	// Find our ticket by ID.
	found := false
	for _, raw := range tickets {
		var entry struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Status   string `json:"status"`
			Type     string `json:"type"`
			Priority int    `json:"priority"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		if entry.ID == createResult.ID {
			found = true
			if entry.Title != "MCP resource test ticket" {
				t.Errorf("ticket title = %q, want %q", entry.Title, "MCP resource test ticket")
			}
			if entry.Status != "open" {
				t.Errorf("ticket status = %q, want %q", entry.Status, "open")
			}
			if entry.Type != "task" {
				t.Errorf("ticket type = %q, want %q", entry.Type, "task")
			}
			if entry.Priority != 2 {
				t.Errorf("ticket priority = %d, want 2", entry.Priority)
			}
			break
		}
	}
	if !found {
		t.Errorf("ticket %s not found in resource read response", createResult.ID)
	}

	// Close the MCP server by closing the input pipe.
	pipe.close()
}

// mcpPipe drives an MCP server via io.Pipe for concurrent interleaved
// send/receive. The server runs in a background goroutine; the test
// sends requests and reads responses/notifications through the pipe.
type mcpPipe struct {
	inputWriter *io.PipeWriter
	scanner     *bufio.Scanner
	serverDone  chan error
	closed      bool
	t           *testing.T
}

// newMCPPipe starts an MCP server in a background goroutine and returns
// a pipe for driving it. The server reads from an input pipe and writes
// to an output pipe. The output pipe is read by the scanner.
func newMCPPipe(t *testing.T, server *mcp.Server) *mcpPipe {
	t.Helper()

	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Run(inputReader, outputWriter)
		// Close the output writer so the scanner sees EOF.
		outputWriter.Close()
	}()

	scanner := bufio.NewScanner(outputReader)
	// MCP messages can be large.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	p := &mcpPipe{
		inputWriter: inputWriter,
		scanner:     scanner,
		serverDone:  serverDone,
		t:           t,
	}

	t.Cleanup(func() {
		p.close()
	})

	return p
}

// send writes a JSON-RPC message to the MCP server's input.
func (p *mcpPipe) send(message map[string]any) {
	p.t.Helper()

	data, err := json.Marshal(message)
	if err != nil {
		p.t.Fatalf("marshal MCP message: %v", err)
	}
	data = append(data, '\n')
	if _, err := p.inputWriter.Write(data); err != nil {
		p.t.Fatalf("write MCP message: %v", err)
	}
}

// readMessage reads one NDJSON line from the MCP server's output and
// returns it as a generic map. Blocks until a line is available or
// the server closes the output.
func (p *mcpPipe) readMessage() map[string]any {
	p.t.Helper()

	if !p.scanner.Scan() {
		err := p.scanner.Err()
		if err != nil {
			p.t.Fatalf("MCP output scan error: %v", err)
		}
		p.t.Fatal("MCP output closed unexpectedly (EOF)")
	}

	var message map[string]any
	if err := json.Unmarshal(p.scanner.Bytes(), &message); err != nil {
		p.t.Fatalf("unmarshal MCP output: %v\nraw: %s", err, p.scanner.Bytes())
	}
	return message
}

// readUntilNotification reads messages until a notification with the
// given method is received. Responses (messages with an "id" field)
// are skipped — they are from prior requests that haven't been read
// yet. Returns the notification message.
func (p *mcpPipe) readUntilNotification(t *testing.T, method string) map[string]any {
	t.Helper()

	for {
		message := p.readMessage()

		// Skip responses (have an "id" field).
		if _, hasID := message["id"]; hasID {
			t.Logf("skipping response while waiting for %s: id=%v", method, message["id"])
			continue
		}

		// Check if this is the notification we're waiting for.
		if messageMethod, ok := message["method"].(string); ok && messageMethod == method {
			return message
		}

		t.Logf("skipping notification %v while waiting for %s", message["method"], method)
	}
}

// close shuts down the MCP server by closing the input pipe and waits
// for the server goroutine to exit. Safe to call multiple times.
func (p *mcpPipe) close() {
	if p.closed {
		return
	}
	p.closed = true
	p.inputWriter.Close()
	<-p.serverDone
}
