// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/model"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestModelService exercises the model service end-to-end: deployment
// via principal.Create, configuration delivery via Matrix state events,
// streaming completion through the CBOR socket, HTTP compatibility
// proxy, and model list queries.
//
// A mock OpenAI-compatible server acts as the upstream provider. The
// model service's registry is configured via Matrix state events
// published to a room the service is invited to.
func TestModelService(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	fleet := createTestFleet(t, admin)

	// Boot a machine.
	machine := newTestMachine(t, fleet, "model-svc")
	startMachine(t, admin, machine, machineOptions{
		LauncherBinary: resolvedBinary(t, "LAUNCHER_BINARY"),
		DaemonBinary:   resolvedBinary(t, "DAEMON_BINARY"),
		ProxyBinary:    resolvedBinary(t, "PROXY_BINARY"),
		Fleet:          fleet,
	})

	// Start a mock OpenAI-compatible server. The model service's
	// OpenAIProvider forwards requests here.
	mock := newMockOpenAICompletionServer(t)

	// Deploy the model service with the HTTP compatibility proxy enabled.
	modelSvc := deployModelService(t, admin, fleet, machine, mock.URL)

	// Create a config room with provider, alias, and account state
	// events. Invite the model service so it picks them up via /sync.
	configureModelRoom(t, admin, fleet, machine, modelSvc, mock.URL)

	// Mint a service token for querying the model service. All
	// subtests reuse this client for read operations.
	modelServiceLocalpart := "service/model/test"
	listEntity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/model-list-tester")
	if err != nil {
		t.Fatalf("build list entity: %v", err)
	}
	listToken := mintModelServiceToken(t, machine, listEntity, "test-project")
	queryClient := service.NewServiceClientFromToken(modelSvc.SocketPath, listToken)

	// Wait for the model service to index the config by polling the
	// model/list action until our test alias appears.
	waitForModelAlias(t, queryClient, "test-model")

	t.Run("List", func(t *testing.T) {
		var result model.ModelListResponse
		if err := queryClient.Call(ctx, model.ActionList, nil, &result); err != nil {
			t.Fatalf("model/list: %v", err)
		}

		if result.Providers != 1 {
			t.Errorf("providers = %d, want 1", result.Providers)
		}
		if result.Accounts != 1 {
			t.Errorf("accounts = %d, want 1", result.Accounts)
		}
		if len(result.Aliases) != 1 {
			t.Fatalf("aliases = %d, want 1", len(result.Aliases))
		}

		alias := result.Aliases[0]
		if alias.Alias != "test-model" {
			t.Errorf("alias = %q, want %q", alias.Alias, "test-model")
		}
		if alias.Provider != "mock-provider" {
			t.Errorf("provider = %q, want %q", alias.Provider, "mock-provider")
		}
		if alias.ProviderModel != "mock-gpt" {
			t.Errorf("provider_model = %q, want %q", alias.ProviderModel, "mock-gpt")
		}
	})

	t.Run("Complete", func(t *testing.T) {
		// Mint a service token for the model service. The model
		// service requires Project for account selection.
		entity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/model-tester")
		if err != nil {
			t.Fatalf("build test entity: %v", err)
		}
		token := mintModelServiceToken(t, machine, entity, "test-project")
		client := service.NewServiceClientFromToken(modelSvc.SocketPath, token)

		// Open a streaming completion.
		stream, err := client.OpenStream(ctx, model.ActionComplete, model.Request{
			Model: "test-model",
			Messages: []model.Message{
				{Role: "user", Content: "Say hello"},
			},
			Stream: true,
		})
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		defer stream.Close()

		// Read all response chunks.
		var deltas []model.Response
		var done *model.Response
		for {
			var response model.Response
			if err := stream.Recv(&response); err != nil {
				t.Fatalf("recv: %v", err)
			}

			switch response.Type {
			case model.ResponseDelta:
				deltas = append(deltas, response)
			case model.ResponseDone:
				done = &response
			case model.ResponseError:
				t.Fatalf("stream error: %s", response.Error)
			default:
				t.Fatalf("unexpected response type: %q", response.Type)
			}

			if done != nil {
				break
			}
		}

		// Verify streamed content. Content may come from both deltas
		// and the done message (the final SSE chunk with finish_reason
		// can carry content alongside the stop signal).
		if len(deltas) == 0 {
			t.Fatal("expected at least one delta chunk")
		}
		var fullContent strings.Builder
		for _, delta := range deltas {
			fullContent.WriteString(delta.Content)
		}
		if done != nil {
			fullContent.WriteString(done.Content)
		}
		if got := fullContent.String(); got != "Hello from mock" {
			t.Errorf("streamed content = %q, want %q", got, "Hello from mock")
		}

		// Verify the done message has usage and cost.
		if done.Usage == nil {
			t.Fatal("done message missing usage")
		}
		if done.Usage.InputTokens != 10 {
			t.Errorf("input_tokens = %d, want 10", done.Usage.InputTokens)
		}
		if done.Usage.OutputTokens != 5 {
			t.Errorf("output_tokens = %d, want 5", done.Usage.OutputTokens)
		}
		if done.CostMicrodollars <= 0 {
			t.Errorf("cost_microdollars = %d, want > 0", done.CostMicrodollars)
		}

		// Wait for the mock to confirm it received the request.
		select {
		case <-mock.RequestReceived:
		case <-ctx.Done():
			t.Fatal("timed out waiting for mock to receive request")
		}
	})

	t.Run("HTTP", func(t *testing.T) {
		// Resolve the HTTP socket path. The launcher bind-mounts
		// /run/bureau/listen/ to configDir/listen/ on the host.
		// The CBOR service socket symlink points to .../listen/service.sock;
		// the HTTP socket is at .../listen/http.sock in the same directory.
		serviceSocketPath := machine.PrincipalServiceSocketPath(t, modelServiceLocalpart)
		symlinkTarget, err := os.Readlink(serviceSocketPath)
		if err != nil {
			t.Fatalf("readlink %s: %v", serviceSocketPath, err)
		}
		httpSocketPath := filepath.Join(filepath.Dir(symlinkTarget), "http.sock")
		waitForFile(t, httpSocketPath)

		// Mint a token for the HTTP proxy (same grants as CBOR test).
		entity, err := ref.NewEntityFromAccountLocalpart(fleet.Ref, "agent/model-http-tester")
		if err != nil {
			t.Fatalf("build test entity: %v", err)
		}
		tokenBytes := mintModelServiceToken(t, machine, entity, "test-project")
		tokenBase64 := base64.StdEncoding.EncodeToString(tokenBytes)

		// Construct an HTTP client that dials the host-side HTTP socket.
		httpClient := &http.Client{
			Transport: &http.Transport{
				DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(dialCtx, "unix", httpSocketPath)
				},
			},
		}

		// Send an OpenAI-format completion request through the HTTP proxy.
		// The path prefix must match the provider name ("mock-provider").
		requestBody, _ := json.Marshal(map[string]any{
			"model":  "test-model",
			"stream": true,
			"messages": []map[string]string{
				{"role": "user", "content": "Say hello via HTTP"},
			},
		})

		request, err := http.NewRequestWithContext(ctx, "POST",
			"http://model-service/mock-provider/v1/chat/completions",
			bytes.NewReader(requestBody))
		if err != nil {
			t.Fatalf("create request: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+tokenBase64)

		response, err := httpClient.Do(request)
		if err != nil {
			t.Fatalf("HTTP request: %v", err)
		}
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			body, _ := bufio.NewReader(response.Body).ReadString(0)
			t.Fatalf("HTTP status = %d, want 200; body: %s", response.StatusCode, body)
		}

		// Read the SSE stream. Expect OpenAI-format data lines.
		scanner := bufio.NewScanner(response.Body)
		var sseContent strings.Builder
		var gotDone bool
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				gotDone = true
				break
			}

			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			if len(chunk.Choices) > 0 {
				sseContent.WriteString(chunk.Choices[0].Delta.Content)
			}
		}

		if !gotDone {
			t.Error("SSE stream did not end with [DONE]")
		}
		if got := sseContent.String(); got != "Hello from mock" {
			t.Errorf("HTTP streamed content = %q, want %q", got, "Hello from mock")
		}
	})
}

// --- Model service deployment helpers ---

// modelServiceDeployment holds the result of deploying a model service
// on a test machine.
type modelServiceDeployment struct {
	Entity     ref.Entity
	Account    principalAccount
	SocketPath string
}

// deployModelService deploys the model service binary using the
// production principal.Create path. The mockURL is the upstream
// provider URL — the model service forwards requests to it.
func deployModelService(
	t *testing.T,
	admin *messaging.DirectSession,
	fleet *testFleet,
	machine *testMachine,
	mockURL string,
) modelServiceDeployment {
	t.Helper()

	svc := deployService(t, admin, fleet, machine, serviceDeployOptions{
		Binary:    resolvedBinary(t, "MODEL_SERVICE_BINARY"),
		Name:      "model-test",
		Localpart: "service/model/test",
		MatrixPolicy: &schema.MatrixPolicy{
			AllowJoin: true,
		},
		ExtraEnvironmentVariables: map[string]string{
			"BUREAU_MODEL_HTTP_SOCKET": "/run/bureau/listen/http.sock",
		},
	})

	return modelServiceDeployment{
		Entity:     svc.Entity,
		Account:    svc.Account,
		SocketPath: svc.SocketPath,
	}
}

// configureModelRoom creates a room with model provider, alias, and
// account state events, then invites the model service so it picks
// up the configuration via /sync.
func configureModelRoom(
	t *testing.T,
	admin *messaging.DirectSession,
	fleet *testFleet,
	machine *testMachine,
	modelSvc modelServiceDeployment,
	mockURL string,
) {
	t.Helper()

	ctx := t.Context()

	// Create the config room.
	room, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name: "model-config",
	})
	if err != nil {
		t.Fatalf("create model config room: %v", err)
	}
	roomID := room.RoomID

	// Publish model provider state event. The endpoint is the mock
	// server's base URL. The OpenAI provider appends "/v1/chat/completions"
	// to this when making requests.
	if _, err := admin.SendStateEvent(ctx, roomID, model.EventTypeModelProvider, "mock-provider", model.ModelProviderContent{
		Endpoint:     mockURL,
		AuthMethod:   model.AuthMethodNone,
		Capabilities: []string{"chat", "streaming"},
	}); err != nil {
		t.Fatalf("publish model provider: %v", err)
	}

	// Publish model alias state event.
	if _, err := admin.SendStateEvent(ctx, roomID, model.EventTypeModelAlias, "test-model", model.ModelAliasContent{
		Provider:      "mock-provider",
		ProviderModel: "mock-gpt",
		Pricing: model.Pricing{
			InputPerMtokMicrodollars:  2_500_000,  // $2.50/Mtok
			OutputPerMtokMicrodollars: 10_000_000, // $10/Mtok
		},
		Capabilities: []string{"chat", "streaming"},
	}); err != nil {
		t.Fatalf("publish model alias: %v", err)
	}

	// Publish model account state event. Projects ["*"] is a wildcard
	// that matches any project in the service token.
	if _, err := admin.SendStateEvent(ctx, roomID, model.EventTypeModelAccount, "mock-account", model.ModelAccountContent{
		Provider: "mock-provider",
		Projects: []string{"*"},
	}); err != nil {
		t.Fatalf("publish model account: %v", err)
	}

	// Invite the model service to the config room. The service's /sync
	// loop accepts invites and reads state events from joined rooms.
	if err := admin.InviteUser(ctx, roomID, modelSvc.Account.UserID); err != nil {
		t.Fatalf("invite model service to config room: %v", err)
	}
}

// waitForModelAlias polls the model/list action until the expected alias
// appears in the registry. The model service's /sync loop is asynchronous:
// it must join the config room, perform a sync cycle, and process the
// state events before the alias is available.
//
// Each model/list call completes in microseconds on a local socket.
// The loop terminates once the model service's sync handler indexes
// the config room's state events (typically 1-2 sync cycles).
func waitForModelAlias(t *testing.T, client *service.ServiceClient, expectedAlias string) {
	t.Helper()

	ctx := t.Context()
	var lastError error
	for {
		var result model.ModelListResponse
		err := client.Call(ctx, model.ActionList, nil, &result)
		if err != nil {
			lastError = err
		} else {
			for _, entry := range result.Aliases {
				if entry.Alias == expectedAlias {
					return
				}
			}
		}

		select {
		case <-ctx.Done():
			if lastError != nil {
				t.Fatalf("timed out waiting for model alias %q (last error: %v)", expectedAlias, lastError)
			}
			t.Fatalf("timed out waiting for model alias %q to appear in registry", expectedAlias)
		default:
			// Yield to other goroutines between poll iterations.
			runtime.Gosched()
		}
	}
}

// mintModelServiceToken mints a service token for the model service
// with the Project field set. The model service requires Project for
// per-project account selection and quota tracking.
func mintModelServiceToken(t *testing.T, machine *testMachine, principal ref.Entity, project string) []byte {
	t.Helper()

	_, privateKey, err := servicetoken.LoadKeypair(machine.StateDir)
	if err != nil {
		t.Fatalf("load machine signing keypair from %s: %v", machine.StateDir, err)
	}

	tokenID := make([]byte, 16)
	if _, err := rand.Read(tokenID); err != nil {
		t.Fatalf("generate token ID: %v", err)
	}

	token := &servicetoken.Token{
		Subject:   principal.UserID(),
		Machine:   machine.Ref,
		Audience:  "model",
		Grants:    []servicetoken.Grant{{Actions: []string{model.ActionAll}}},
		Project:   project,
		ID:        hex.EncodeToString(tokenID),
		IssuedAt:  1735689600, // 2025-01-01T00:00:00Z
		ExpiresAt: 4070908800, // 2099-01-01T00:00:00Z
	}

	tokenBytes, err := servicetoken.Mint(privateKey, token)
	if err != nil {
		t.Fatalf("mint model token for %s: %v", principal, err)
	}
	return tokenBytes
}

// --- Mock OpenAI server ---

// mockOpenAICompletionServer serves OpenAI-compatible streaming
// chat/completions responses. The mock returns a fixed response with
// known content and usage metrics so the integration test can verify
// the full pipeline: model service alias resolution → provider
// forwarding → CBOR streaming → cost calculation.
type mockOpenAICompletionServer struct {
	*httptest.Server

	// RequestReceived is closed after the mock processes its first
	// completion request.
	RequestReceived <-chan struct{}
}

// newMockOpenAICompletionServer creates a mock server that responds
// to POST /v1/chat/completions with a streaming SSE response containing
// "Hello from mock" split across two deltas, with usage metrics in the
// final chunk.
func newMockOpenAICompletionServer(t *testing.T) *mockOpenAICompletionServer {
	t.Helper()

	requestReceived := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Accept both /chat/completions (CBOR path: endpoint + "/chat/completions")
		// and /v1/chat/completions (HTTP proxy path: endpoint + remaining URL path).
		// The endpoint in the state event is the base URL without /v1, so the
		// OpenAI provider appends /chat/completions directly while the HTTP proxy
		// forwards the client's /v1/chat/completions path.
		if request.URL.Path != "/chat/completions" && request.URL.Path != "/v1/chat/completions" {
			http.Error(writer, fmt.Sprintf("unexpected path: %s", request.URL.Path), http.StatusNotFound)
			return
		}
		if request.Method != http.MethodPost {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request to validate it's well-formed.
		var body struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}

		// Signal that a request was received.
		select {
		case requestReceived <- struct{}{}:
		default:
		}

		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		flusher, ok := writer.(http.Flusher)
		if !ok {
			http.Error(writer, "streaming not supported", http.StatusInternalServerError)
			return
		}

		// Send streaming chunks in OpenAI format. The model service's
		// OpenAIProvider parses these using lib/llm.SSEScanner.
		chunks := []string{
			`{"model":"mock-gpt","choices":[{"delta":{"content":"Hello from "},"finish_reason":null}]}`,
			`{"model":"mock-gpt","choices":[{"delta":{"content":"mock"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		}

		for _, chunk := range chunks {
			fmt.Fprintf(writer, "data: %s\n\n", chunk)
			flusher.Flush()
		}

		fmt.Fprintf(writer, "data: [DONE]\n\n")
		flusher.Flush()
	}))

	t.Cleanup(server.Close)
	return &mockOpenAICompletionServer{
		Server:          server,
		RequestReceived: requestReceived,
	}
}
