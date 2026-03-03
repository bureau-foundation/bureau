// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/modelprovider"
	"github.com/bureau-foundation/bureau/lib/modelregistry"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/model"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// --- extractProviderPrefix ---

func TestExtractProviderPrefix(t *testing.T) {
	tests := []struct {
		path          string
		wantProvider  string
		wantRemaining string
		wantError     bool
	}{
		{"/openai/v1/chat/completions", "openai", "/v1/chat/completions", false},
		{"/anthropic/v1/messages", "anthropic", "/v1/messages", false},
		{"/openrouter/v1/chat/completions", "openrouter", "/v1/chat/completions", false},
		{"/local-llm/v1/completions", "local-llm", "/v1/completions", false},
		{"/openai", "openai", "/", false},
		{"", "", "", true},
		{"/", "", "", true},
	}

	for _, test := range tests {
		provider, remaining, err := extractProviderPrefix(test.path)
		if test.wantError {
			if err == nil {
				t.Errorf("extractProviderPrefix(%q): expected error, got provider=%q remaining=%q",
					test.path, provider, remaining)
			}
			continue
		}
		if err != nil {
			t.Errorf("extractProviderPrefix(%q): unexpected error: %v", test.path, err)
			continue
		}
		if provider != test.wantProvider {
			t.Errorf("extractProviderPrefix(%q): provider = %q, want %q", test.path, provider, test.wantProvider)
		}
		if remaining != test.wantRemaining {
			t.Errorf("extractProviderPrefix(%q): remaining = %q, want %q", test.path, remaining, test.wantRemaining)
		}
	}
}

// --- buildUpstreamURL ---

func TestBuildUpstreamURL(t *testing.T) {
	tests := []struct {
		endpoint      string
		remainingPath string
		rawQuery      string
		want          string
	}{
		{"https://api.openai.com", "/v1/chat/completions", "", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/", "/v1/chat/completions", "", "https://api.openai.com/v1/chat/completions"},
		{"https://openrouter.ai/api", "/v1/chat/completions", "", "https://openrouter.ai/api/v1/chat/completions"},
		{"https://api.anthropic.com", "/v1/messages", "limit=10", "https://api.anthropic.com/v1/messages?limit=10"},
		{"http://localhost:8080", "/v1/embeddings", "", "http://localhost:8080/v1/embeddings"},
	}

	for _, test := range tests {
		got, err := buildUpstreamURL(test.endpoint, test.remainingPath, test.rawQuery)
		if err != nil {
			t.Errorf("buildUpstreamURL(%q, %q, %q): unexpected error: %v",
				test.endpoint, test.remainingPath, test.rawQuery, err)
			continue
		}
		if got != test.want {
			t.Errorf("buildUpstreamURL(%q, %q, %q) = %q, want %q",
				test.endpoint, test.remainingPath, test.rawQuery, got, test.want)
		}
	}
}

// --- resolveAuthHeader ---

func TestResolveAuthHeader(t *testing.T) {
	tests := []struct {
		name           string
		httpAuthHeader string
		want           string
	}{
		{"default", "", "Authorization"},
		{"custom", "x-api-key", "x-api-key"},
		{"explicit authorization", "Authorization", "Authorization"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := model.ModelProviderContent{HTTPAuthHeader: test.httpAuthHeader}
			got := resolveAuthHeader(config)
			if got != test.want {
				t.Errorf("resolveAuthHeader() = %q, want %q", got, test.want)
			}
		})
	}
}

// --- injectCredential ---

func TestInjectCredential(t *testing.T) {
	tests := []struct {
		name           string
		httpAuthHeader string
		credential     string
		wantHeader     string
		wantValue      string
	}{
		{
			"default bearer",
			"",
			"sk-test-key",
			"Authorization",
			"Bearer sk-test-key",
		},
		{
			"explicit authorization",
			"Authorization",
			"sk-test-key",
			"Authorization",
			"Bearer sk-test-key",
		},
		{
			"anthropic x-api-key",
			"x-api-key",
			"sk-ant-test",
			"X-Api-Key", // Go canonicalizes header names
			"sk-ant-test",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, _ := http.NewRequest("POST", "http://example.com", nil)
			config := model.ModelProviderContent{HTTPAuthHeader: test.httpAuthHeader}
			injectCredential(request, config, test.credential)

			got := request.Header.Get(test.wantHeader)
			if got != test.wantValue {
				t.Errorf("header %q = %q, want %q", test.wantHeader, got, test.wantValue)
			}
		})
	}
}

// --- extractModelField ---

func TestExtractModelField(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"openai request", `{"model":"gpt-4","messages":[]}`, "gpt-4"},
		{"alias", `{"model":"codex","messages":[]}`, "codex"},
		{"no model field", `{"messages":[]}`, ""},
		{"empty body", "", ""},
		{"invalid json", `{bad json}`, ""},
		{"non-string model", `{"model":42}`, ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := extractModelField([]byte(test.body))
			if got != test.want {
				t.Errorf("extractModelField() = %q, want %q", got, test.want)
			}
		})
	}
}

// --- extractUsageFromJSON ---

func TestExtractUsageFromJSON(t *testing.T) {
	tests := []struct {
		name             string
		body             string
		wantNil          bool
		wantInputTokens  int64
		wantOutputTokens int64
		wantModel        string
	}{
		{
			"openai response",
			`{"model":"gpt-4","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`,
			false, 10, 20, "gpt-4",
		},
		{
			"anthropic response",
			`{"model":"claude-3","usage":{"input_tokens":15,"output_tokens":25}}`,
			false, 15, 25, "claude-3",
		},
		{
			"no usage",
			`{"model":"gpt-4","choices":[]}`,
			true, 0, 0, "",
		},
		{
			"null usage",
			`{"model":"gpt-4","usage":null}`,
			true, 0, 0, "",
		},
		{
			"zero usage",
			`{"model":"gpt-4","usage":{"prompt_tokens":0,"completion_tokens":0}}`,
			true, 0, 0, "",
		},
		{
			"invalid json",
			`not json`,
			true, 0, 0, "",
		},
		{
			"empty body",
			"",
			true, 0, 0, "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := extractUsageFromJSON([]byte(test.body))
			if test.wantNil {
				if usage != nil {
					t.Errorf("expected nil usage, got %+v", usage)
				}
				return
			}
			if usage == nil {
				t.Fatal("expected non-nil usage, got nil")
			}
			if usage.InputTokens != test.wantInputTokens {
				t.Errorf("InputTokens = %d, want %d", usage.InputTokens, test.wantInputTokens)
			}
			if usage.OutputTokens != test.wantOutputTokens {
				t.Errorf("OutputTokens = %d, want %d", usage.OutputTokens, test.wantOutputTokens)
			}
			if usage.Model != test.wantModel {
				t.Errorf("Model = %q, want %q", usage.Model, test.wantModel)
			}
		})
	}
}

// --- usageScanner ---

func TestUsageScanner_OpenAIStreaming(t *testing.T) {
	// Simulate an OpenAI streaming response with usage in the final chunk.
	events := strings.Join([]string{
		`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"Hello"},"finish_reason":null}],"model":"gpt-4"}`,
		"",
		`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":" world"},"finish_reason":null}],"model":"gpt-4"}`,
		"",
		`data: {"id":"chatcmpl-1","choices":[{"delta":{},"finish_reason":"stop"}],"model":"gpt-4","usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	scanner := &usageScanner{}
	scanner.Write([]byte(events))

	usage := scanner.usage()
	if usage == nil {
		t.Fatal("expected non-nil usage")
	}
	if usage.InputTokens != 5 {
		t.Errorf("InputTokens = %d, want 5", usage.InputTokens)
	}
	if usage.OutputTokens != 2 {
		t.Errorf("OutputTokens = %d, want 2", usage.OutputTokens)
	}
	if usage.Model != "gpt-4" {
		t.Errorf("Model = %q, want %q", usage.Model, "gpt-4")
	}
}

func TestUsageScanner_AnthropicStreaming(t *testing.T) {
	// Simulate an Anthropic streaming response where input and output
	// tokens appear in separate events.
	events := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg-1","model":"claude-3","usage":{"input_tokens":10,"output_tokens":0}}}`,
		"",
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`,
		"",
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}`,
		"",
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	scanner := &usageScanner{}
	scanner.Write([]byte(events))

	usage := scanner.usage()
	if usage == nil {
		t.Fatal("expected non-nil usage")
	}
	if usage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", usage.InputTokens)
	}
	if usage.OutputTokens != 15 {
		t.Errorf("OutputTokens = %d, want 15", usage.OutputTokens)
	}
}

func TestUsageScanner_ChunkedDelivery(t *testing.T) {
	// Deliver SSE events in chunks that split across line boundaries.
	event := `data: {"model":"gpt-4","usage":{"prompt_tokens":8,"completion_tokens":12}}` + "\n\n"

	scanner := &usageScanner{}

	// Split the event across three writes at arbitrary boundaries.
	scanner.Write([]byte(event[:15]))
	scanner.Write([]byte(event[15:50]))
	scanner.Write([]byte(event[50:]))

	usage := scanner.usage()
	if usage == nil {
		t.Fatal("expected non-nil usage")
	}
	if usage.InputTokens != 8 {
		t.Errorf("InputTokens = %d, want 8", usage.InputTokens)
	}
	if usage.OutputTokens != 12 {
		t.Errorf("OutputTokens = %d, want 12", usage.OutputTokens)
	}
}

func TestUsageScanner_NoUsage(t *testing.T) {
	events := strings.Join([]string{
		`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"Hello"}}]}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	scanner := &usageScanner{}
	scanner.Write([]byte(events))

	if usage := scanner.usage(); usage != nil {
		t.Errorf("expected nil usage, got %+v", usage)
	}
}

// --- Integration-style HTTP proxy tests ---

// testFixture sets up a model service, auth config, and helper
// functions for HTTP proxy testing. No real Matrix or homeserver
// needed — we populate the registry directly.
type testFixture struct {
	modelService  *ModelService
	authConfig    *service.AuthConfig
	proxy         *httpProxy
	publicKey     ed25519.PublicKey
	privateKey    ed25519.PrivateKey
	fakeClock     *clock.FakeClock
	credentialDir string
}

func newTestFixture(t *testing.T) *testFixture {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating test key pair: %v", err)
	}

	fakeClock := clock.Fake(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))

	authConfig := &service.AuthConfig{
		PublicKey: publicKey,
		Audience:  "model",
		Blacklist: servicetoken.NewBlacklist(),
		Clock:     fakeClock,
	}

	serverName, _ := ref.ParseServerName("bureau.local")
	fleet, _ := ref.ParseFleet("test/fleet/prod", serverName)
	serviceRef, _ := ref.NewService(fleet, "model")
	machineRef, _ := ref.NewMachine(fleet, "test-machine")

	logger := service.NewLogger()

	// Create a temp directory for test credentials. Tests write
	// credential files here and call loadTestCredentials.
	credentialDir := t.TempDir()

	// Start with an empty credential store.
	t.Setenv("BUREAU_MODEL_CREDENTIALS_DIR", credentialDir)
	credentials, err := loadCredentials()
	if err != nil {
		t.Fatalf("loading empty credential store: %v", err)
	}
	t.Cleanup(func() { credentials.Close() })

	ms := &ModelService{
		clock:        fakeClock,
		service:      serviceRef,
		machine:      machineRef,
		logger:       logger,
		registry:     modelregistry.New(logger),
		quotaTracker: modelregistry.NewQuotaTracker(fakeClock),
		credentials:  credentials,
		providers:    make(map[string]modelprovider.Provider),
	}

	proxy := newHTTPProxy(ms, authConfig)

	return &testFixture{
		modelService:  ms,
		authConfig:    authConfig,
		proxy:         proxy,
		publicKey:     publicKey,
		privateKey:    privateKey,
		fakeClock:     fakeClock,
		credentialDir: credentialDir,
	}
}

// setCredential writes a credential file and reloads the credential
// store. Must be called before the first request that needs this
// credential.
func (fixture *testFixture) setCredential(t *testing.T, name, value string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(fixture.credentialDir, name), []byte(value), 0600); err != nil {
		t.Fatalf("writing test credential %q: %v", name, err)
	}

	// Reload the credential store to pick up the new file.
	t.Setenv("BUREAU_MODEL_CREDENTIALS_DIR", fixture.credentialDir)
	newCredentials, err := loadCredentials()
	if err != nil {
		t.Fatalf("reloading credentials: %v", err)
	}
	// Close the old store and replace.
	fixture.modelService.credentials.Close()
	fixture.modelService.credentials = newCredentials
	t.Cleanup(func() { newCredentials.Close() })
}

func (fixture *testFixture) mintToken(t *testing.T, project string) string {
	t.Helper()
	return fixture.mintTokenWithGrants(t, project,
		[]servicetoken.Grant{{Actions: []string{"model/*"}}})
}

func (fixture *testFixture) mintTokenWithGrants(t *testing.T, project string, grants []servicetoken.Grant) string {
	t.Helper()

	serverName, _ := ref.ParseServerName("bureau.local")
	subject, _ := ref.ParseUserID(fmt.Sprintf("@test/fleet/prod/agent/test-agent:%s", serverName))
	fleet, _ := ref.ParseFleet("test/fleet/prod", serverName)
	machine, _ := ref.NewMachine(fleet, "test-machine")

	token := &servicetoken.Token{
		Subject:   subject,
		Machine:   machine,
		Audience:  "model",
		Grants:    grants,
		ID:        "test-token-1",
		IssuedAt:  fixture.fakeClock.Now().Unix(),
		ExpiresAt: fixture.fakeClock.Now().Add(1 * time.Hour).Unix(),
		Project:   project,
	}

	tokenBytes, err := servicetoken.Mint(fixture.privateKey, token)
	if err != nil {
		t.Fatalf("minting test token: %v", err)
	}

	return base64.StdEncoding.EncodeToString(tokenBytes)
}

func TestHTTPProxy_UnknownProvider(t *testing.T) {
	fixture := newTestFixture(t)

	request := httptest.NewRequest("POST", "/unknown-provider/v1/chat/completions", nil)
	request.Header.Set("Authorization", "Bearer "+fixture.mintToken(t, "test-project"))
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if !strings.Contains(recorder.Body.String(), "unknown provider") {
		t.Errorf("body = %q, want 'unknown provider' message", recorder.Body.String())
	}
}

func TestHTTPProxy_MissingAuth(t *testing.T) {
	fixture := newTestFixture(t)

	// Register a provider so we get past the provider lookup.
	fixture.modelService.registry.SetProvider("openai", model.ModelProviderContent{
		Endpoint:     "https://api.openai.com",
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"completion"},
	})

	request := httptest.NewRequest("POST", "/openai/v1/chat/completions", nil)
	// No Authorization header.
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestHTTPProxy_InvalidToken(t *testing.T) {
	fixture := newTestFixture(t)

	fixture.modelService.registry.SetProvider("openai", model.ModelProviderContent{
		Endpoint:     "https://api.openai.com",
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"completion"},
	})

	request := httptest.NewRequest("POST", "/openai/v1/chat/completions", nil)
	request.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString([]byte("garbage")))
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestHTTPProxy_MissingProject(t *testing.T) {
	fixture := newTestFixture(t)

	fixture.modelService.registry.SetProvider("openai", model.ModelProviderContent{
		Endpoint:     "https://api.openai.com",
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"completion"},
	})

	// Mint a token with no project.
	tokenString := fixture.mintToken(t, "")

	request := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	request.Header.Set("Authorization", "Bearer "+tokenString)
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestHTTPProxy_ForwardsToUpstream(t *testing.T) {
	fixture := newTestFixture(t)

	// Start a fake upstream server.
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Verify the credential was injected.
		authHeader := request.Header.Get("Authorization")
		if authHeader != "Bearer real-api-key" {
			t.Errorf("upstream Authorization = %q, want %q", authHeader, "Bearer real-api-key")
		}

		// Verify the path was forwarded correctly.
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want %q", request.URL.Path, "/v1/chat/completions")
		}

		// Verify the traceparent header was injected.
		if request.Header.Get("Traceparent") == "" {
			t.Error("upstream missing Traceparent header")
		}

		// Verify the request body was forwarded.
		body, _ := io.ReadAll(request.Body)
		var requestBody struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &requestBody)
		if requestBody.Model != "codex" {
			t.Errorf("upstream model = %q, want %q", requestBody.Model, "codex")
		}

		// Send a response with usage.
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"model":   "gpt-4-turbo",
			"choices": []map[string]any{{"message": map[string]string{"content": "Hello!"}}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	defer upstream.Close()

	// Register provider, alias, and account.
	fixture.modelService.registry.SetProvider("openai", model.ModelProviderContent{
		Endpoint:     upstream.URL,
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"completion"},
	})
	fixture.modelService.registry.SetAlias("codex", model.ModelAliasContent{
		Provider:      "openai",
		ProviderModel: "gpt-4-turbo",
		Pricing: model.Pricing{
			InputPerMtokMicrodollars:  2500000,
			OutputPerMtokMicrodollars: 10000000,
		},
	})
	fixture.modelService.registry.SetAccount("openai-shared", model.ModelAccountContent{
		Provider:      "openai",
		CredentialRef: "openai-key",
		Projects:      []string{"*"},
	})

	// Set up the credential.
	fixture.setCredential(t, "openai-key", "real-api-key")

	// Make the request.
	requestBody := `{"model":"codex","messages":[{"role":"user","content":"Hi"}]}`
	request := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+fixture.mintToken(t, "my-project"))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	// Verify the response was forwarded.
	var response struct {
		Model string `json:"model"`
	}
	json.Unmarshal(recorder.Body.Bytes(), &response)
	if response.Model != "gpt-4-turbo" {
		t.Errorf("response model = %q, want %q", response.Model, "gpt-4-turbo")
	}
}

func TestHTTPProxy_SSEStreaming(t *testing.T) {
	fixture := newTestFixture(t)

	// Start a fake upstream that returns an SSE stream.
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.WriteHeader(http.StatusOK)

		flusher := writer.(http.Flusher)

		events := []string{
			`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"Hello"},"finish_reason":null}],"model":"gpt-4"}`,
			`data: {"id":"chatcmpl-1","choices":[{"delta":{},"finish_reason":"stop"}],"model":"gpt-4","usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
			`data: [DONE]`,
		}

		for _, event := range events {
			fmt.Fprintf(writer, "%s\n\n", event)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	fixture.modelService.registry.SetProvider("openai", model.ModelProviderContent{
		Endpoint:     upstream.URL,
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"completion", "streaming"},
	})
	fixture.modelService.registry.SetAlias("fast", model.ModelAliasContent{
		Provider:      "openai",
		ProviderModel: "gpt-4",
	})
	fixture.modelService.registry.SetAccount("openai-shared", model.ModelAccountContent{
		Provider:      "openai",
		CredentialRef: "openai-key",
		Projects:      []string{"*"},
	})
	fixture.setCredential(t, "openai-key", "real-key")

	requestBody := `{"model":"fast","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	request := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+fixture.mintToken(t, "my-project"))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	// Verify SSE content was forwarded.
	body := recorder.Body.String()
	if !strings.Contains(body, "Hello") {
		t.Errorf("response body missing streamed content; got: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("response body missing [DONE] sentinel; got: %s", body)
	}
}

func TestHTTPProxy_AnthropicAuthHeader(t *testing.T) {
	fixture := newTestFixture(t)

	// Start a fake upstream that checks for the x-api-key header.
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		apiKey := request.Header.Get("x-api-key")
		if apiKey != "real-anthropic-key" {
			t.Errorf("upstream x-api-key = %q, want %q", apiKey, "real-anthropic-key")
		}

		// Should not have an Authorization header (stripped from incoming).
		if auth := request.Header.Get("Authorization"); auth != "" {
			t.Errorf("upstream Authorization should be empty, got %q", auth)
		}

		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{
			"model": "claude-3",
			"usage": map[string]int{"input_tokens": 8, "output_tokens": 12},
		})
	}))
	defer upstream.Close()

	fixture.modelService.registry.SetProvider("anthropic", model.ModelProviderContent{
		Endpoint:       upstream.URL,
		AuthMethod:     model.AuthMethodBearer,
		Capabilities:   []string{"completion"},
		HTTPAuthHeader: "x-api-key",
	})
	fixture.modelService.registry.SetAlias("claude", model.ModelAliasContent{
		Provider:      "anthropic",
		ProviderModel: "claude-3-sonnet",
	})
	fixture.modelService.registry.SetAccount("anthropic-shared", model.ModelAccountContent{
		Provider:      "anthropic",
		CredentialRef: "anthropic-key",
		Projects:      []string{"*"},
	})
	fixture.setCredential(t, "anthropic-key", "real-anthropic-key")

	requestBody := `{"model":"claude","messages":[{"role":"user","content":"Hi"}]}`
	request := httptest.NewRequest("POST", "/anthropic/v1/messages",
		strings.NewReader(requestBody))
	// Anthropic SDK sends the token in x-api-key, not Authorization.
	request.Header.Set("x-api-key", fixture.mintToken(t, "my-project"))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
}

func TestHTTPProxy_QuotaExceeded(t *testing.T) {
	fixture := newTestFixture(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("request should not reach upstream when quota is exceeded")
	}))
	defer upstream.Close()

	fixture.modelService.registry.SetProvider("openai", model.ModelProviderContent{
		Endpoint:     upstream.URL,
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"completion"},
	})
	fixture.modelService.registry.SetAlias("codex", model.ModelAliasContent{
		Provider:      "openai",
		ProviderModel: "gpt-4",
		Pricing: model.Pricing{
			InputPerMtokMicrodollars:  2500000,
			OutputPerMtokMicrodollars: 10000000,
		},
	})
	fixture.modelService.registry.SetAccount("openai-limited", model.ModelAccountContent{
		Provider:      "openai",
		CredentialRef: "openai-key",
		Projects:      []string{"*"},
		Quota:         &model.Quota{DailyMicrodollars: 100},
	})
	fixture.setCredential(t, "openai-key", "real-key")

	// Exhaust the quota.
	fixture.modelService.quotaTracker.Record("openai-limited", 200)

	requestBody := `{"model":"codex","messages":[{"role":"user","content":"Hi"}]}`
	request := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+fixture.mintToken(t, "my-project"))
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d; body: %s", recorder.Code, http.StatusTooManyRequests, recorder.Body.String())
	}
}

func TestHTTPProxy_EmptyPath(t *testing.T) {
	fixture := newTestFixture(t)

	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestHTTPProxy_NoModelField_DirectForward(t *testing.T) {
	fixture := newTestFixture(t)

	// When there's no model field in the body (e.g., GET /v1/models),
	// the proxy should forward to the provider named by the path prefix
	// without alias resolution.
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			t.Errorf("upstream path = %q, want %q", request.URL.Path, "/v1/models")
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`{"data":[{"id":"gpt-4"}]}`))
	}))
	defer upstream.Close()

	fixture.modelService.registry.SetProvider("openai", model.ModelProviderContent{
		Endpoint:     upstream.URL,
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"completion"},
	})
	fixture.modelService.registry.SetAccount("openai-shared", model.ModelAccountContent{
		Provider:      "openai",
		CredentialRef: "openai-key",
		Projects:      []string{"*"},
	})
	fixture.setCredential(t, "openai-key", "real-key")

	request := httptest.NewRequest("GET", "/openai/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+fixture.mintToken(t, "my-project"))
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
}

func TestHTTPProxy_QueryStringForwarded(t *testing.T) {
	fixture := newTestFixture(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.RawQuery != "page=2&limit=10" {
			t.Errorf("upstream query = %q, want %q", request.URL.RawQuery, "page=2&limit=10")
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fixture.modelService.registry.SetProvider("openai", model.ModelProviderContent{
		Endpoint:     upstream.URL,
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"completion"},
	})
	fixture.modelService.registry.SetAccount("openai-shared", model.ModelAccountContent{
		Provider:      "openai",
		CredentialRef: "openai-key",
		Projects:      []string{"*"},
	})
	fixture.setCredential(t, "openai-key", "real-key")

	request := httptest.NewRequest("GET", "/openai/v1/models?page=2&limit=10", nil)
	request.Header.Set("Authorization", "Bearer "+fixture.mintToken(t, "my-project"))
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
}

// --- Grant enforcement tests for HTTP proxy ---

func TestHTTPProxy_GrantDenied_WrongAction(t *testing.T) {
	fixture := newTestFixture(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("request should not reach upstream when grant is denied")
	}))
	defer upstream.Close()

	fixture.modelService.registry.SetProvider("openai", model.ModelProviderContent{
		Endpoint:     upstream.URL,
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"completion"},
	})
	fixture.modelService.registry.SetAlias("codex", model.ModelAliasContent{
		Provider:      "openai",
		ProviderModel: "gpt-4",
	})
	fixture.modelService.registry.SetAccount("openai-shared", model.ModelAccountContent{
		Provider:      "openai",
		CredentialRef: "openai-key",
		Projects:      []string{"*"},
	})
	fixture.setCredential(t, "openai-key", "real-key")

	// Token only grants model/embed, not model/complete.
	tokenString := fixture.mintTokenWithGrants(t, "my-project",
		[]servicetoken.Grant{{Actions: []string{"model/embed"}}})

	requestBody := `{"model":"codex","messages":[{"role":"user","content":"Hi"}]}`
	request := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+tokenString)
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d; body: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

func TestHTTPProxy_GrantDenied_WrongModel(t *testing.T) {
	fixture := newTestFixture(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("request should not reach upstream when model is denied")
	}))
	defer upstream.Close()

	fixture.modelService.registry.SetProvider("openai", model.ModelProviderContent{
		Endpoint:     upstream.URL,
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"completion"},
	})
	fixture.modelService.registry.SetAlias("codex", model.ModelAliasContent{
		Provider:      "openai",
		ProviderModel: "gpt-4",
	})
	fixture.modelService.registry.SetAlias("sonnet", model.ModelAliasContent{
		Provider:      "openai",
		ProviderModel: "claude-sonnet",
	})
	fixture.modelService.registry.SetAccount("openai-shared", model.ModelAccountContent{
		Provider:      "openai",
		CredentialRef: "openai-key",
		Projects:      []string{"*"},
	})
	fixture.setCredential(t, "openai-key", "real-key")

	// Token grants model/complete but only for "codex" target.
	tokenString := fixture.mintTokenWithGrants(t, "my-project",
		[]servicetoken.Grant{{
			Actions: []string{"model/complete"},
			Targets: []string{"codex"},
		}})

	// Request "sonnet" which is not in the target list.
	requestBody := `{"model":"sonnet","messages":[{"role":"user","content":"Hi"}]}`
	request := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+tokenString)
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d; body: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "not permitted") {
		t.Errorf("body should mention 'not permitted', got: %s", recorder.Body.String())
	}
}

func TestHTTPProxy_GrantAllowed_TargetedGrant(t *testing.T) {
	fixture := newTestFixture(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`{"model":"gpt-4","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	fixture.modelService.registry.SetProvider("openai", model.ModelProviderContent{
		Endpoint:     upstream.URL,
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"completion"},
	})
	fixture.modelService.registry.SetAlias("codex", model.ModelAliasContent{
		Provider:      "openai",
		ProviderModel: "gpt-4",
	})
	fixture.modelService.registry.SetAccount("openai-shared", model.ModelAccountContent{
		Provider:      "openai",
		CredentialRef: "openai-key",
		Projects:      []string{"*"},
	})
	fixture.setCredential(t, "openai-key", "real-key")

	// Token grants model/complete only for "codex".
	tokenString := fixture.mintTokenWithGrants(t, "my-project",
		[]servicetoken.Grant{{
			Actions: []string{"model/complete"},
			Targets: []string{"codex"},
		}})

	requestBody := `{"model":"codex","messages":[{"role":"user","content":"Hi"}]}`
	request := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+tokenString)
	recorder := httptest.NewRecorder()

	fixture.proxy.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
}
