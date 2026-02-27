// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// --- URI parsing tests ---

func TestParseResourceURI(t *testing.T) {
	tests := []struct {
		name        string
		uri         string
		want        parsedURI
		wantErr     bool
		errContains string
	}{
		{
			name: "identity service",
			uri:  "bureau://identity",
			want: parsedURI{Service: "identity"},
		},
		{
			name: "tickets with room alias",
			uri:  "bureau://tickets/ns/room",
			want: parsedURI{Service: "tickets", RoomAlias: "ns/room"},
		},
		{
			name: "tickets with ready sub-resource",
			uri:  "bureau://tickets/ns/room/ready",
			want: parsedURI{Service: "tickets", RoomAlias: "ns/room", SubResource: "ready"},
		},
		{
			name: "tickets with blocked sub-resource",
			uri:  "bureau://tickets/ns/room/blocked",
			want: parsedURI{Service: "tickets", RoomAlias: "ns/room", SubResource: "blocked"},
		},
		{
			name: "tickets with ranked sub-resource",
			uri:  "bureau://tickets/ns/room/ranked",
			want: parsedURI{Service: "tickets", RoomAlias: "ns/room", SubResource: "ranked"},
		},
		{
			name: "tickets with deep room alias",
			uri:  "bureau://tickets/ns/fleet/prod/general",
			want: parsedURI{Service: "tickets", RoomAlias: "ns/fleet/prod/general"},
		},
		{
			name: "tickets with deep room alias and ready",
			uri:  "bureau://tickets/ns/fleet/prod/general/ready",
			want: parsedURI{Service: "tickets", RoomAlias: "ns/fleet/prod/general", SubResource: "ready"},
		},
		{
			name: "fleet with machines sub-resource",
			uri:  "bureau://fleet/machines",
			want: parsedURI{Service: "fleet", SubResource: "machines"},
		},
		{
			name: "fleet with services sub-resource",
			uri:  "bureau://fleet/services",
			want: parsedURI{Service: "fleet", SubResource: "services"},
		},
		{
			name: "service with trailing slash",
			uri:  "bureau://identity/",
			want: parsedURI{Service: "identity"},
		},
		{
			name:        "wrong scheme",
			uri:         "https://identity",
			wantErr:     true,
			errContains: "unsupported URI scheme",
		},
		{
			name:        "empty path",
			uri:         "bureau://",
			wantErr:     true,
			errContains: "empty resource path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResourceURI(tt.uri)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tt.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Service != tt.want.Service {
				t.Errorf("Service = %q, want %q", got.Service, tt.want.Service)
			}
			if got.RoomAlias != tt.want.RoomAlias {
				t.Errorf("RoomAlias = %q, want %q", got.RoomAlias, tt.want.RoomAlias)
			}
			if got.SubResource != tt.want.SubResource {
				t.Errorf("SubResource = %q, want %q", got.SubResource, tt.want.SubResource)
			}
		})
	}
}

func TestParseTicketURI_EmptyRoomAlias(t *testing.T) {
	// A URI like "bureau://tickets//ready" has an empty room alias
	// before the sub-resource. This should be rejected.
	_, err := parseResourceURI("bureau://tickets//ready")
	if err == nil {
		t.Fatal("expected error for empty room alias")
	}
	if !strings.Contains(err.Error(), "empty room alias") {
		t.Errorf("error = %q, want it to contain 'empty room alias'", err.Error())
	}
}

// --- Mock resource provider ---

// mockProvider is a test resource provider that returns canned responses.
type mockProvider struct {
	uri         string
	resources   []resourceDescription
	templates   []resourceTemplate
	content     []resourceContent
	readErr     error
	subscribeFn func(ctx context.Context, uri string, notify func(string)) (func(), error)
	grantAction string
}

func (p *mockProvider) Handles(uri string) bool {
	return strings.HasPrefix(uri, p.uri)
}

func (p *mockProvider) List(_ context.Context, _ []schema.Grant) ([]resourceDescription, []resourceTemplate) {
	return p.resources, p.templates
}

func (p *mockProvider) Read(_ context.Context, uri string) ([]resourceContent, error) {
	if p.readErr != nil {
		return nil, p.readErr
	}
	return p.content, nil
}

func (p *mockProvider) Subscribe(ctx context.Context, uri string, notify func(string)) (func(), error) {
	if p.subscribeFn != nil {
		return p.subscribeFn(ctx, uri, notify)
	}
	return nil, fmt.Errorf("subscriptions not supported")
}

func (p *mockProvider) GrantAction() string {
	return p.grantAction
}

// --- Session helpers for resource tests ---

// resourceGrants returns grants that authorize both commands and
// resources. Used by tests that need full access.
func resourceGrants() []schema.Grant {
	return []schema.Grant{
		{Actions: []string{"command/**", "resource/**"}},
	}
}

// resourceSession runs an initialized MCP session with a mock
// resource provider registered and returns the responses.
func resourceSession(t *testing.T, provider ResourceProvider, grants []schema.Grant, messages ...map[string]any) []testResponse {
	t.Helper()
	root := testCommandTree()
	all := append(initMessages(), messages...)
	options := []ServerOption{WithResourceProvider(provider)}
	return mcpSessionWithOptions(t, root, grants, options, all...)
}

// --- Resource dispatch tests ---

func TestServer_ResourcesList_EmptyProviders(t *testing.T) {
	// With no providers, resources/list returns an empty list.
	responses := session(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/list",
	})
	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("unexpected error: code=%d message=%q", resp.Error.Code, resp.Error.Message)
	}

	var result resourcesListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(result.Resources) != 0 {
		t.Errorf("expected 0 resources, got %d", len(result.Resources))
	}
}

func TestServer_ResourcesList_WithProvider(t *testing.T) {
	provider := &mockProvider{
		uri: "bureau://test/",
		resources: []resourceDescription{
			{URI: "bureau://test/one", Name: "Test resource", MIMEType: "application/json"},
		},
		templates: []resourceTemplate{
			{URITemplate: "bureau://test/{id}", Name: "Test template"},
		},
		grantAction: "resource/test/**",
	}

	responses := resourceSession(t, provider, resourceGrants(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/list",
	})
	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("unexpected error: code=%d message=%q", resp.Error.Code, resp.Error.Message)
	}

	var result resourcesListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(result.Resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(result.Resources))
	}
	if result.Resources[0].URI != "bureau://test/one" {
		t.Errorf("URI = %q, want %q", result.Resources[0].URI, "bureau://test/one")
	}
	if len(result.ResourceTemplates) != 1 {
		t.Fatalf("expected 1 template, got %d", len(result.ResourceTemplates))
	}
	if result.ResourceTemplates[0].URITemplate != "bureau://test/{id}" {
		t.Errorf("URITemplate = %q, want %q", result.ResourceTemplates[0].URITemplate, "bureau://test/{id}")
	}
}

func TestServer_ResourcesList_UnauthorizedProvider(t *testing.T) {
	provider := &mockProvider{
		uri: "bureau://test/",
		resources: []resourceDescription{
			{URI: "bureau://test/one", Name: "Test resource"},
		},
		grantAction: "resource/test/**",
	}

	// Grant commands but not resources — provider should be filtered.
	grants := []schema.Grant{{Actions: []string{"command/**"}}}
	responses := resourceSession(t, provider, grants, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/list",
	})
	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("unexpected error: code=%d message=%q", resp.Error.Code, resp.Error.Message)
	}

	var result resourcesListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(result.Resources) != 0 {
		t.Errorf("expected 0 resources (unauthorized), got %d", len(result.Resources))
	}
}

func TestServer_ResourcesRead_Success(t *testing.T) {
	provider := &mockProvider{
		uri: "bureau://test/",
		content: []resourceContent{
			{URI: "bureau://test/data", MIMEType: "application/json", Text: `{"key":"value"}`},
		},
		grantAction: "resource/test/**",
	}

	responses := resourceSession(t, provider, resourceGrants(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/read",
		"params":  map[string]any{"uri": "bureau://test/data"},
	})
	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("unexpected error: code=%d message=%q", resp.Error.Code, resp.Error.Message)
	}

	var result resourcesReadResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(result.Contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(result.Contents))
	}
	if result.Contents[0].Text != `{"key":"value"}` {
		t.Errorf("text = %q, want %q", result.Contents[0].Text, `{"key":"value"}`)
	}
}

func TestServer_ResourcesRead_UnknownResource(t *testing.T) {
	provider := &mockProvider{
		uri:         "bureau://test/",
		grantAction: "resource/test/**",
	}

	responses := resourceSession(t, provider, resourceGrants(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/read",
		"params":  map[string]any{"uri": "bureau://other/data"},
	})
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for unknown resource")
	}
	if !strings.Contains(resp.Error.Message, "unknown resource") {
		t.Errorf("error = %q, want it to contain 'unknown resource'", resp.Error.Message)
	}
}

func TestServer_ResourcesRead_Unauthorized(t *testing.T) {
	provider := &mockProvider{
		uri:         "bureau://test/",
		content:     []resourceContent{{URI: "bureau://test/data", Text: "secret"}},
		grantAction: "resource/test/**",
	}

	// Grant commands but not resources.
	grants := []schema.Grant{{Actions: []string{"command/**"}}}
	responses := resourceSession(t, provider, grants, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/read",
		"params":  map[string]any{"uri": "bureau://test/data"},
	})
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for unauthorized resource read")
	}
	if !strings.Contains(resp.Error.Message, "not authorized") {
		t.Errorf("error = %q, want it to contain 'not authorized'", resp.Error.Message)
	}
}

func TestServer_ResourcesRead_MissingURI(t *testing.T) {
	provider := &mockProvider{
		uri:         "bureau://test/",
		grantAction: "resource/test/**",
	}

	responses := resourceSession(t, provider, resourceGrants(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/read",
		"params":  map[string]any{},
	})
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for missing URI")
	}
	if !strings.Contains(resp.Error.Message, "uri is required") {
		t.Errorf("error = %q, want it to contain 'uri is required'", resp.Error.Message)
	}
}

func TestServer_ResourcesRead_ReadError(t *testing.T) {
	provider := &mockProvider{
		uri:         "bureau://test/",
		readErr:     fmt.Errorf("service unavailable"),
		grantAction: "resource/test/**",
	}

	responses := resourceSession(t, provider, resourceGrants(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/read",
		"params":  map[string]any{"uri": "bureau://test/data"},
	})
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for read failure")
	}
	if !strings.Contains(resp.Error.Message, "service unavailable") {
		t.Errorf("error = %q, want it to contain 'service unavailable'", resp.Error.Message)
	}
}

func TestServer_ResourcesSubscribe_Success(t *testing.T) {
	cancelCalled := false
	provider := &mockProvider{
		uri:         "bureau://test/",
		grantAction: "resource/test/**",
		subscribeFn: func(_ context.Context, _ string, _ func(string)) (func(), error) {
			return func() { cancelCalled = true }, nil
		},
	}

	responses := resourceSession(t, provider, resourceGrants(),
		map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "resources/subscribe",
			"params":  map[string]any{"uri": "bureau://test/data"},
		},
		map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "resources/unsubscribe",
			"params":  map[string]any{"uri": "bureau://test/data"},
		},
	)

	// Subscribe should succeed.
	subscribeResp := responses[1]
	if subscribeResp.Error != nil {
		t.Fatalf("subscribe error: code=%d message=%q", subscribeResp.Error.Code, subscribeResp.Error.Message)
	}

	// Unsubscribe should succeed.
	unsubscribeResp := responses[2]
	if unsubscribeResp.Error != nil {
		t.Fatalf("unsubscribe error: code=%d message=%q", unsubscribeResp.Error.Code, unsubscribeResp.Error.Message)
	}

	// Cancel function should have been called.
	if !cancelCalled {
		t.Error("subscription cancel function was not called on unsubscribe")
	}
}

func TestServer_ResourcesSubscribe_IdempotentResubscribe(t *testing.T) {
	subscribeCount := 0
	provider := &mockProvider{
		uri:         "bureau://test/",
		grantAction: "resource/test/**",
		subscribeFn: func(_ context.Context, _ string, _ func(string)) (func(), error) {
			subscribeCount++
			return func() {}, nil
		},
	}

	responses := resourceSession(t, provider, resourceGrants(),
		map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "resources/subscribe",
			"params":  map[string]any{"uri": "bureau://test/data"},
		},
		map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "resources/subscribe",
			"params":  map[string]any{"uri": "bureau://test/data"},
		},
	)

	// Both should succeed.
	if responses[1].Error != nil {
		t.Fatalf("first subscribe error: %v", responses[1].Error)
	}
	if responses[2].Error != nil {
		t.Fatalf("second subscribe error: %v", responses[2].Error)
	}

	// The provider's Subscribe should only be called once.
	if subscribeCount != 1 {
		t.Errorf("Subscribe called %d times, want 1 (second should be idempotent)", subscribeCount)
	}
}

func TestServer_ResourcesSubscribe_SubscribeError(t *testing.T) {
	provider := &mockProvider{
		uri:         "bureau://test/",
		grantAction: "resource/test/**",
		subscribeFn: func(_ context.Context, _ string, _ func(string)) (func(), error) {
			return nil, fmt.Errorf("subscriptions not supported for this resource")
		},
	}

	responses := resourceSession(t, provider, resourceGrants(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/subscribe",
		"params":  map[string]any{"uri": "bureau://test/static"},
	})
	resp := responses[1]
	if resp.Error == nil {
		t.Fatal("expected error for unsupported subscription")
	}
	if !strings.Contains(resp.Error.Message, "not supported") {
		t.Errorf("error = %q, want it to contain 'not supported'", resp.Error.Message)
	}
}

func TestServer_ResourcesUnsubscribe_IdempotentWithoutSubscription(t *testing.T) {
	provider := &mockProvider{
		uri:         "bureau://test/",
		grantAction: "resource/test/**",
	}

	responses := resourceSession(t, provider, resourceGrants(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/unsubscribe",
		"params":  map[string]any{"uri": "bureau://test/never-subscribed"},
	})
	resp := responses[1]
	if resp.Error != nil {
		t.Fatalf("unsubscribe should be idempotent, got error: %v", resp.Error)
	}
}

func TestServer_ResourcesNotInitialized(t *testing.T) {
	root := testCommandTree()
	// Send resources/read without initializing first.
	responses := mcpSession(t, root, wildcardGrants(), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/read",
		"params":  map[string]any{"uri": "bureau://identity"},
	})

	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0].Error == nil {
		t.Fatal("expected error for pre-init resources/read")
	}
	if !strings.Contains(responses[0].Error.Message, "not initialized") {
		t.Errorf("error = %q, want it to contain 'not initialized'", responses[0].Error.Message)
	}
}

// --- Initialize capabilities tests ---

func TestServer_InitializeCapabilities_WithProviders(t *testing.T) {
	provider := &mockProvider{
		uri:         "bureau://test/",
		grantAction: "resource/test/**",
	}

	root := testCommandTree()
	all := initMessages()
	responses := mcpSessionWithOptions(t, root, resourceGrants(),
		[]ServerOption{WithResourceProvider(provider)}, all...)

	if len(responses) != 1 {
		t.Fatalf("expected 1 response (init), got %d", len(responses))
	}

	var result initializeResult
	if err := json.Unmarshal(responses[0].Result, &result); err != nil {
		t.Fatalf("unmarshal init result: %v", err)
	}

	if result.Capabilities.Resources == nil {
		t.Fatal("capabilities.resources should be present when providers are registered")
	}
	if !result.Capabilities.Resources.Subscribe {
		t.Error("resources.subscribe should be true")
	}
	if !result.Capabilities.Resources.ListChanged {
		t.Error("resources.listChanged should be true")
	}
}

func TestServer_InitializeCapabilities_WithoutProviders(t *testing.T) {
	responses := session(t)

	var result initializeResult
	if err := json.Unmarshal(responses[0].Result, &result); err != nil {
		t.Fatalf("unmarshal init result: %v", err)
	}

	if result.Capabilities.Resources != nil {
		t.Errorf("capabilities.resources should be nil when no providers are registered, got %+v",
			result.Capabilities.Resources)
	}
}

func TestServer_InitializeCapabilities_ToolsListChanged(t *testing.T) {
	responses := session(t)

	var result initializeResult
	if err := json.Unmarshal(responses[0].Result, &result); err != nil {
		t.Fatalf("unmarshal init result: %v", err)
	}

	if result.Capabilities.Tools == nil {
		t.Fatal("capabilities.tools should be present")
	}
	if !result.Capabilities.Tools.ListChanged {
		t.Error("tools.listChanged should be true")
	}
}

// --- Identity provider unit tests ---

func TestIdentityProvider_Handles(t *testing.T) {
	provider := NewIdentityProvider(nil)
	if !provider.Handles("bureau://identity") {
		t.Error("should handle bureau://identity")
	}
	if provider.Handles("bureau://tickets/ns/room") {
		t.Error("should not handle bureau://tickets/ns/room")
	}
	if provider.Handles("bureau://identity/sub") {
		t.Error("should not handle bureau://identity/sub")
	}
}

func TestIdentityProvider_List(t *testing.T) {
	provider := NewIdentityProvider(nil)
	resources, templates := provider.List(context.Background(), nil)
	if len(resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resources))
	}
	if resources[0].URI != "bureau://identity" {
		t.Errorf("URI = %q, want %q", resources[0].URI, "bureau://identity")
	}
	if resources[0].MIMEType != "application/json" {
		t.Errorf("MIMEType = %q, want %q", resources[0].MIMEType, "application/json")
	}
	if templates != nil {
		t.Errorf("expected nil templates, got %v", templates)
	}
}

func TestIdentityProvider_SubscribeReturnsError(t *testing.T) {
	provider := NewIdentityProvider(nil)
	cancel, err := provider.Subscribe(context.Background(), "bureau://identity", func(string) {})
	if err == nil {
		t.Fatal("expected error for identity subscription")
	}
	if cancel != nil {
		t.Error("cancel should be nil on error")
	}
	if !strings.Contains(err.Error(), "static") {
		t.Errorf("error = %q, want it to mention 'static'", err.Error())
	}
}

func TestIdentityProvider_GrantAction(t *testing.T) {
	provider := NewIdentityProvider(nil)
	if provider.GrantAction() != "resource/identity" {
		t.Errorf("GrantAction() = %q, want %q", provider.GrantAction(), "resource/identity")
	}
}

// --- Ticket provider unit tests ---

func TestTicketProvider_Handles(t *testing.T) {
	provider := NewTicketProvider(nil)
	if !provider.Handles("bureau://tickets/ns/room") {
		t.Error("should handle bureau://tickets/ns/room")
	}
	if !provider.Handles("bureau://tickets/ns/room/ready") {
		t.Error("should handle bureau://tickets/ns/room/ready")
	}
	if provider.Handles("bureau://identity") {
		t.Error("should not handle bureau://identity")
	}
	if provider.Handles("bureau://fleet/machines") {
		t.Error("should not handle bureau://fleet/machines")
	}
}

func TestTicketProvider_GrantAction(t *testing.T) {
	provider := NewTicketProvider(nil)
	if provider.GrantAction() != "resource/ticket/**" {
		t.Errorf("GrantAction() = %q, want %q", provider.GrantAction(), "resource/ticket/**")
	}
}
