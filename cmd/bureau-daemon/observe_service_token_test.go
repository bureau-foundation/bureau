// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/observation"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/observe"
)

// sendMintServiceToken sends a mint_service_token request to the daemon's
// observe socket and returns the parsed response.
func sendMintServiceToken(t *testing.T, socketPath string, serviceRole string) observe.ServiceTokenResponse {
	t.Helper()

	connection, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial observe socket: %v", err)
	}
	defer connection.Close()

	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	request := map[string]any{
		"action":       "mint_service_token",
		"service_role": serviceRole,
		"observer":     testObserverUserID,
		"token":        testObserverToken,
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("send mint_service_token request: %v", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read mint_service_token response: %v", err)
	}

	var response observe.ServiceTokenResponse
	if err := json.Unmarshal(responseLine, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return response
}

// sendMintServiceTokenRaw sends a mint_service_token request and returns
// the raw JSON map (for error-case tests where we want ok=false).
func sendMintServiceTokenRaw(t *testing.T, socketPath string, request map[string]any) map[string]any {
	t.Helper()

	connection, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial observe socket: %v", err)
	}
	defer connection.Close()

	connection.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:realclock // kernel I/O deadline

	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("send request: %v", err)
	}

	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	var response map[string]any
	if err := json.Unmarshal(responseLine, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return response
}

// newTestDaemonWithServiceToken creates a test daemon with token signing
// keys and a mock Matrix server that has a service binding for the
// "ticket" role in the config room.
func newTestDaemonWithServiceToken(t *testing.T) *Daemon {
	t.Helper()

	daemon, matrixState := newTestDaemonWithQuery(t)

	// Generate token signing keypair.
	publicKey, privateKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	daemon.tokenSigningPublicKey = publicKey
	daemon.tokenSigningPrivateKey = privateKey

	// Set up a service binding in the config room: the "ticket" role
	// is handled by a service principal.
	ticketServicePrincipal := "@bureau/fleet/prod/service/ticket:bureau.local"
	matrixState.setStateEvent(
		daemon.configRoomID.String(),
		schema.EventTypeServiceBinding,
		"ticket",
		schema.ServiceBindingContent{
			Principal: mustParseEntity(t, ticketServicePrincipal),
		},
	)

	// The daemon needs fleetRunDir for socket path derivation.
	daemon.fleetRunDir = "/run/bureau/fleet/prod"

	return daemon
}

func mustParseEntity(t *testing.T, userID string) ref.Entity {
	t.Helper()
	entity, err := ref.ParseEntityUserID(userID)
	if err != nil {
		t.Fatalf("ParseEntity(%q): %v", userID, err)
	}
	return entity
}

func TestMintServiceTokenSuccess(t *testing.T) {
	daemon := newTestDaemonWithServiceToken(t)

	// Set up grants for the observer that cover the ticket namespace.
	observerUserID, err := ref.ParseUserID(testObserverUserID)
	if err != nil {
		t.Fatalf("ParseUserID: %v", err)
	}
	daemon.authorizationIndex.SetPrincipal(observerUserID, schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{ticket.ActionAll}},
		},
	})

	response := sendMintServiceToken(t, daemon.observeSocketPath, "ticket")

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}
	if response.Token == "" {
		t.Fatal("expected non-empty token")
	}
	if response.SocketPath == "" {
		t.Fatal("expected non-empty socket path")
	}
	if response.TTLSeconds != 300 {
		t.Errorf("TTLSeconds = %d, want 300", response.TTLSeconds)
	}
	if response.ExpiresAt == 0 {
		t.Error("expected non-zero ExpiresAt")
	}

	// Verify the token can be decoded and verified.
	tokenBytes, err := response.TokenBytes()
	if err != nil {
		t.Fatalf("TokenBytes: %v", err)
	}

	token, err := servicetoken.VerifyForServiceAt(daemon.tokenSigningPublicKey, tokenBytes, "ticket", daemon.clock.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if token.Subject != observerUserID {
		t.Errorf("Subject = %v, want %v", token.Subject, observerUserID)
	}
	if token.Audience != "ticket" {
		t.Errorf("Audience = %q, want %q", token.Audience, "ticket")
	}
	if len(token.Grants) != 1 {
		t.Fatalf("Grants = %d, want 1", len(token.Grants))
	}
	if token.Grants[0].Actions[0] != ticket.ActionAll {
		t.Errorf("Grants[0].Actions[0] = %q, want %q", token.Grants[0].Actions[0], ticket.ActionAll)
	}
}

func TestMintServiceTokenFilterGrants(t *testing.T) {
	daemon := newTestDaemonWithServiceToken(t)

	// Set up grants that span multiple namespaces — only ticket/**
	// should appear in the minted token.
	observerUserID, err := ref.ParseUserID(testObserverUserID)
	if err != nil {
		t.Fatalf("ParseUserID: %v", err)
	}
	daemon.authorizationIndex.SetPrincipal(observerUserID, schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{ticket.ActionCreate, ticket.ActionUpdate}},
			{Actions: []string{observation.ActionAll}},
			{Actions: []string{"artifact/upload"}},
		},
	})

	response := sendMintServiceToken(t, daemon.observeSocketPath, "ticket")

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}

	tokenBytes, err := response.TokenBytes()
	if err != nil {
		t.Fatalf("TokenBytes: %v", err)
	}

	token, err := servicetoken.VerifyForServiceAt(daemon.tokenSigningPublicKey, tokenBytes, "ticket", daemon.clock.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// Only the ticket-namespace grant should be included.
	if len(token.Grants) != 1 {
		t.Fatalf("Grants = %d, want 1 (only ticket-namespaced grants)", len(token.Grants))
	}
}

func TestMintServiceTokenNoGrants(t *testing.T) {
	daemon := newTestDaemonWithServiceToken(t)

	// Observer has no grants at all — token should still be minted
	// (with empty grants). The service will deny actions, but the
	// token itself is valid.
	observerUserID, err := ref.ParseUserID(testObserverUserID)
	if err != nil {
		t.Fatalf("ParseUserID: %v", err)
	}
	daemon.authorizationIndex.SetPrincipal(observerUserID, schema.AuthorizationPolicy{})

	response := sendMintServiceToken(t, daemon.observeSocketPath, "ticket")

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}

	tokenBytes, err := response.TokenBytes()
	if err != nil {
		t.Fatalf("TokenBytes: %v", err)
	}

	token, err := servicetoken.VerifyForServiceAt(daemon.tokenSigningPublicKey, tokenBytes, "ticket", daemon.clock.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if len(token.Grants) != 0 {
		t.Errorf("Grants = %d, want 0", len(token.Grants))
	}
}

func TestMintServiceTokenMissingServiceRole(t *testing.T) {
	daemon := newTestDaemonWithServiceToken(t)

	response := sendMintServiceTokenRaw(t, daemon.observeSocketPath, map[string]any{
		"action":   "mint_service_token",
		"observer": testObserverUserID,
		"token":    testObserverToken,
	})

	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
}

func TestMintServiceTokenUnknownService(t *testing.T) {
	daemon := newTestDaemonWithServiceToken(t)

	// Request a service role that has no binding in the config room.
	response := sendMintServiceTokenRaw(t, daemon.observeSocketPath, map[string]any{
		"action":       "mint_service_token",
		"service_role": "nonexistent",
		"observer":     testObserverUserID,
		"token":        testObserverToken,
	})

	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
}

func TestMintServiceTokenNoSigningKey(t *testing.T) {
	daemon, _ := newTestDaemonWithQuery(t)

	// Deliberately do not set tokenSigningPrivateKey — should fail.
	response := sendMintServiceTokenRaw(t, daemon.observeSocketPath, map[string]any{
		"action":       "mint_service_token",
		"service_role": "ticket",
		"observer":     testObserverUserID,
		"token":        testObserverToken,
	})

	if response["ok"] != false {
		t.Errorf("expected ok=false, got %v", response["ok"])
	}
}

func TestMintServiceTokenSocketPathDerivation(t *testing.T) {
	daemon := newTestDaemonWithServiceToken(t)

	observerUserID, err := ref.ParseUserID(testObserverUserID)
	if err != nil {
		t.Fatalf("ParseUserID: %v", err)
	}
	daemon.authorizationIndex.SetPrincipal(observerUserID, schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{ticket.ActionAll}},
		},
	})

	response := sendMintServiceToken(t, daemon.observeSocketPath, "ticket")

	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}

	// The socket path should be derived from the service principal
	// and the fleet run dir.
	expectedPrincipal := mustParseEntity(t, "@bureau/fleet/prod/service/ticket:bureau.local")
	expectedPath := expectedPrincipal.ServiceSocketPath(daemon.fleetRunDir)
	if response.SocketPath != expectedPath {
		t.Errorf("SocketPath = %q, want %q", response.SocketPath, expectedPath)
	}
}

// TestMintServiceTokenClientFunction exercises the observe.MintServiceToken
// client function against a real daemon observe socket.
func TestMintServiceTokenClientFunction(t *testing.T) {
	daemon := newTestDaemonWithServiceToken(t)

	observerUserID, err := ref.ParseUserID(testObserverUserID)
	if err != nil {
		t.Fatalf("ParseUserID: %v", err)
	}
	daemon.authorizationIndex.SetPrincipal(observerUserID, schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{ticket.ActionAll}},
		},
	})

	response, err := observe.MintServiceToken(
		daemon.observeSocketPath,
		"ticket",
		testObserverUserID,
		testObserverToken,
	)
	if err != nil {
		t.Fatalf("MintServiceToken: %v", err)
	}
	if !response.OK {
		t.Fatalf("expected OK, got error: %s", response.Error)
	}

	tokenBytes, err := response.TokenBytes()
	if err != nil {
		t.Fatalf("TokenBytes: %v", err)
	}

	token, err := servicetoken.VerifyForServiceAt(
		daemon.tokenSigningPublicKey,
		tokenBytes,
		"ticket",
		daemon.clock.Now(),
	)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if token.Subject != observerUserID {
		t.Errorf("Subject = %v, want %v", token.Subject, observerUserID)
	}
}
