// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"testing"

	"github.com/bureau-foundation/bureau/lib/codec"
)

// TestModelProviderContent_JSONRoundTrip verifies that a provider
// config survives JSON serialization — the format used in Matrix
// state events.
func TestModelProviderContent_JSONRoundTrip(t *testing.T) {
	original := ModelProviderContent{
		Endpoint:     "https://openrouter.ai/api",
		AuthMethod:   AuthMethodBearer,
		Capabilities: []string{"chat", "embeddings", "streaming"},
		BatchSupport: false,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ModelProviderContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Endpoint != original.Endpoint {
		t.Errorf("Endpoint = %q, want %q", decoded.Endpoint, original.Endpoint)
	}
	if decoded.AuthMethod != original.AuthMethod {
		t.Errorf("AuthMethod = %q, want %q", decoded.AuthMethod, original.AuthMethod)
	}
	if len(decoded.Capabilities) != len(original.Capabilities) {
		t.Errorf("Capabilities length = %d, want %d", len(decoded.Capabilities), len(original.Capabilities))
	}
}

// TestModelProviderContent_LocalProvider verifies a local inference
// provider (llama.cpp) with Unix socket endpoint and batch support.
func TestModelProviderContent_LocalProvider(t *testing.T) {
	provider := ModelProviderContent{
		Endpoint:     "unix:///run/bureau/service/llama.sock",
		AuthMethod:   AuthMethodNone,
		Capabilities: []string{"chat", "embeddings", "batch"},
		BatchSupport: true,
		MaxBatchSize: 32,
	}

	if err := provider.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	data, err := json.Marshal(provider)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ModelProviderContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.MaxBatchSize != 32 {
		t.Errorf("MaxBatchSize = %d, want 32", decoded.MaxBatchSize)
	}
	if !decoded.BatchSupport {
		t.Error("BatchSupport = false, want true")
	}
}

func TestModelProviderContent_Validate(t *testing.T) {
	tests := []struct {
		name    string
		content ModelProviderContent
		wantErr bool
	}{
		{
			name: "valid HTTP provider",
			content: ModelProviderContent{
				Endpoint:     "https://openrouter.ai/api",
				AuthMethod:   AuthMethodBearer,
				Capabilities: []string{"chat"},
			},
		},
		{
			name: "valid local provider",
			content: ModelProviderContent{
				Endpoint:     "unix:///run/bureau/service/llama.sock",
				AuthMethod:   AuthMethodNone,
				Capabilities: []string{"chat", "embeddings"},
				BatchSupport: true,
				MaxBatchSize: 32,
			},
		},
		{
			name: "empty endpoint",
			content: ModelProviderContent{
				AuthMethod:   AuthMethodBearer,
				Capabilities: []string{"chat"},
			},
			wantErr: true,
		},
		{
			name: "unknown auth method",
			content: ModelProviderContent{
				Endpoint:     "https://example.com",
				AuthMethod:   "magic",
				Capabilities: []string{"chat"},
			},
			wantErr: true,
		},
		{
			name: "empty capabilities",
			content: ModelProviderContent{
				Endpoint:   "https://example.com",
				AuthMethod: AuthMethodBearer,
			},
			wantErr: true,
		},
		{
			name: "max_batch_size without batch_support",
			content: ModelProviderContent{
				Endpoint:     "https://example.com",
				AuthMethod:   AuthMethodBearer,
				Capabilities: []string{"chat"},
				MaxBatchSize: 10,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.content.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestModelAliasContent_JSONRoundTrip(t *testing.T) {
	original := ModelAliasContent{
		Provider:      "openrouter",
		ProviderModel: "openai/gpt-4.1",
		Pricing: Pricing{
			InputPerMtokMicrodollars:  2000,
			OutputPerMtokMicrodollars: 8000,
		},
		Capabilities: []string{"code", "reasoning", "streaming"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ModelAliasContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Provider != original.Provider {
		t.Errorf("Provider = %q, want %q", decoded.Provider, original.Provider)
	}
	if decoded.ProviderModel != original.ProviderModel {
		t.Errorf("ProviderModel = %q, want %q", decoded.ProviderModel, original.ProviderModel)
	}
	if decoded.Pricing.InputPerMtokMicrodollars != 2000 {
		t.Errorf("InputPricing = %d, want 2000", decoded.Pricing.InputPerMtokMicrodollars)
	}
	if decoded.Pricing.OutputPerMtokMicrodollars != 8000 {
		t.Errorf("OutputPricing = %d, want 8000", decoded.Pricing.OutputPerMtokMicrodollars)
	}
}

// TestModelAliasContent_FreePricing verifies that zero pricing
// (local inference) round-trips correctly.
func TestModelAliasContent_FreePricing(t *testing.T) {
	original := ModelAliasContent{
		Provider:      "llama-local",
		ProviderModel: "nomic-embed-text-v1.5",
		Pricing:       Pricing{InputPerMtokMicrodollars: 0},
		Capabilities:  []string{"embeddings", "batch"},
	}

	if err := original.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ModelAliasContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Pricing.InputPerMtokMicrodollars != 0 {
		t.Errorf("InputPricing = %d, want 0", decoded.Pricing.InputPerMtokMicrodollars)
	}
	if decoded.Pricing.OutputPerMtokMicrodollars != 0 {
		t.Errorf("OutputPricing = %d, want 0", decoded.Pricing.OutputPerMtokMicrodollars)
	}
}

func TestModelAliasContent_Validate(t *testing.T) {
	tests := []struct {
		name    string
		content ModelAliasContent
		wantErr bool
	}{
		{
			name: "valid",
			content: ModelAliasContent{
				Provider:      "openrouter",
				ProviderModel: "openai/gpt-4.1",
				Pricing:       Pricing{InputPerMtokMicrodollars: 2000},
			},
		},
		{
			name: "empty provider",
			content: ModelAliasContent{
				ProviderModel: "openai/gpt-4.1",
			},
			wantErr: true,
		},
		{
			name: "empty provider_model",
			content: ModelAliasContent{
				Provider: "openrouter",
			},
			wantErr: true,
		},
		{
			name: "negative input pricing",
			content: ModelAliasContent{
				Provider:      "openrouter",
				ProviderModel: "openai/gpt-4.1",
				Pricing:       Pricing{InputPerMtokMicrodollars: -1},
			},
			wantErr: true,
		},
		{
			name: "valid with fallbacks",
			content: ModelAliasContent{
				Provider:      "openrouter",
				ProviderModel: "openai/gpt-4.1",
				Pricing:       Pricing{InputPerMtokMicrodollars: 2000},
				Fallbacks: []ModelAliasFallback{
					{Provider: "anthropic", ProviderModel: "claude-sonnet-4-6"},
					{Provider: "local", ProviderModel: "qwen3-32b"},
				},
			},
		},
		{
			name: "fallback with empty provider",
			content: ModelAliasContent{
				Provider:      "openrouter",
				ProviderModel: "openai/gpt-4.1",
				Fallbacks: []ModelAliasFallback{
					{ProviderModel: "claude-sonnet-4-6"},
				},
			},
			wantErr: true,
		},
		{
			name: "fallback with empty provider_model",
			content: ModelAliasContent{
				Provider:      "openrouter",
				ProviderModel: "openai/gpt-4.1",
				Fallbacks: []ModelAliasFallback{
					{Provider: "anthropic"},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.content.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestModelAccountContent_JSONRoundTrip(t *testing.T) {
	original := ModelAccountContent{
		Provider:      "openrouter",
		CredentialRef: "openrouter-alice",
		Projects:      []string{"iree-amdgpu", "iree-compiler"},
		Priority:      0,
		Quota: &Quota{
			DailyMicrodollars:   50_000_000,
			MonthlyMicrodollars: 500_000_000,
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ModelAccountContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Provider != original.Provider {
		t.Errorf("Provider = %q, want %q", decoded.Provider, original.Provider)
	}
	if decoded.CredentialRef != original.CredentialRef {
		t.Errorf("CredentialRef = %q, want %q", decoded.CredentialRef, original.CredentialRef)
	}
	if len(decoded.Projects) != 2 {
		t.Fatalf("Projects length = %d, want 2", len(decoded.Projects))
	}
	if decoded.Projects[0] != "iree-amdgpu" {
		t.Errorf("Projects[0] = %q, want %q", decoded.Projects[0], "iree-amdgpu")
	}
	if decoded.Quota == nil {
		t.Fatal("Quota is nil, want non-nil")
	}
	if decoded.Quota.DailyMicrodollars != 50_000_000 {
		t.Errorf("DailyMicrodollars = %d, want 50000000", decoded.Quota.DailyMicrodollars)
	}
}

// TestModelAccountContent_WildcardFallback verifies a wildcard
// fallback account with negative priority.
func TestModelAccountContent_WildcardFallback(t *testing.T) {
	account := ModelAccountContent{
		Provider:      "openrouter",
		CredentialRef: "openrouter-shared",
		Projects:      []string{"*"},
		Priority:      -1,
		Quota: &Quota{
			DailyMicrodollars: 10_000_000,
		},
	}

	if err := account.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	data, err := json.Marshal(account)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ModelAccountContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Priority != -1 {
		t.Errorf("Priority = %d, want -1", decoded.Priority)
	}
	if decoded.Quota.MonthlyMicrodollars != 0 {
		t.Errorf("MonthlyMicrodollars = %d, want 0", decoded.Quota.MonthlyMicrodollars)
	}
}

func TestModelAccountContent_Validate(t *testing.T) {
	tests := []struct {
		name    string
		content ModelAccountContent
		wantErr bool
	}{
		{
			name: "valid with credential",
			content: ModelAccountContent{
				Provider:      "openrouter",
				CredentialRef: "openrouter-alice",
				Projects:      []string{"iree-amdgpu"},
			},
		},
		{
			name: "valid without credential (local provider)",
			content: ModelAccountContent{
				Provider: "llama-local",
				Projects: []string{"iree-amdgpu"},
			},
		},
		{
			name: "valid wildcard",
			content: ModelAccountContent{
				Provider:      "openrouter",
				CredentialRef: "openrouter-shared",
				Projects:      []string{"*"},
				Priority:      -1,
			},
		},
		{
			name: "empty provider",
			content: ModelAccountContent{
				CredentialRef: "key",
				Projects:      []string{"proj"},
			},
			wantErr: true,
		},
		{
			name: "empty projects",
			content: ModelAccountContent{
				Provider: "openrouter",
			},
			wantErr: true,
		},
		{
			name: "empty project entry",
			content: ModelAccountContent{
				Provider: "openrouter",
				Projects: []string{"iree-amdgpu", ""},
			},
			wantErr: true,
		},
		{
			name: "project with whitespace",
			content: ModelAccountContent{
				Provider: "openrouter",
				Projects: []string{"iree amdgpu"},
			},
			wantErr: true,
		},
		{
			name: "negative daily quota",
			content: ModelAccountContent{
				Provider: "openrouter",
				Projects: []string{"proj"},
				Quota:    &Quota{DailyMicrodollars: -1},
			},
			wantErr: true,
		},
		{
			name: "negative monthly quota",
			content: ModelAccountContent{
				Provider: "openrouter",
				Projects: []string{"proj"},
				Quota:    &Quota{MonthlyMicrodollars: -1},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.content.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAuthMethod_IsKnown(t *testing.T) {
	if !AuthMethodBearer.IsKnown() {
		t.Error("bearer should be known")
	}
	if !AuthMethodNone.IsKnown() {
		t.Error("none should be known")
	}
	if AuthMethod("magic").IsKnown() {
		t.Error("magic should not be known")
	}
}

// TestWireRequest_CBORRoundTrip verifies that a completion request
// survives CBOR serialization with keyasint encoding.
func TestWireRequest_CBORRoundTrip(t *testing.T) {
	original := Request{
		Action: ActionComplete,
		Model:  "codex",
		Messages: []Message{
			{Role: "system", Content: "You are a code reviewer."},
			{
				Role:    "user",
				Content: "Review this function.",
				Attachments: []Attachment{
					{ContentType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}},
				},
			},
		},
		Stream:        true,
		LatencyPolicy: LatencyImmediate,
	}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded Request
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Action != ActionComplete {
		t.Errorf("Action = %q, want %q", decoded.Action, ActionComplete)
	}
	if decoded.Model != "codex" {
		t.Errorf("Model = %q, want %q", decoded.Model, "codex")
	}
	if len(decoded.Messages) != 2 {
		t.Fatalf("Messages length = %d, want 2", len(decoded.Messages))
	}
	if decoded.Messages[1].Attachments[0].ContentType != "image/png" {
		t.Errorf("Attachment ContentType = %q, want %q",
			decoded.Messages[1].Attachments[0].ContentType, "image/png")
	}
	if len(decoded.Messages[1].Attachments[0].Data) != 4 {
		t.Errorf("Attachment Data length = %d, want 4",
			len(decoded.Messages[1].Attachments[0].Data))
	}
	if !decoded.Stream {
		t.Error("Stream = false, want true")
	}
}

// TestWireEmbedRequest_CBORRoundTrip verifies an embedding request
// with raw byte inputs.
func TestWireEmbedRequest_CBORRoundTrip(t *testing.T) {
	original := Request{
		Action:        ActionEmbed,
		Model:         "local-embed",
		Input:         [][]byte{[]byte("hello world"), []byte("test input")},
		LatencyPolicy: LatencyBatch,
	}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded Request
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Action != ActionEmbed {
		t.Errorf("Action = %q, want %q", decoded.Action, ActionEmbed)
	}
	if len(decoded.Input) != 2 {
		t.Fatalf("Input length = %d, want 2", len(decoded.Input))
	}
	if string(decoded.Input[0]) != "hello world" {
		t.Errorf("Input[0] = %q, want %q", decoded.Input[0], "hello world")
	}
	if decoded.LatencyPolicy != LatencyBatch {
		t.Errorf("LatencyPolicy = %q, want %q", decoded.LatencyPolicy, LatencyBatch)
	}
}

// TestWireResponse_CBORRoundTrip verifies that response messages
// round-trip through CBOR.
func TestWireResponse_CBORRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		response Response
	}{
		{
			name: "delta",
			response: Response{
				Type:    ResponseDelta,
				Content: "The function has",
			},
		},
		{
			name: "done",
			response: Response{
				Type:             ResponseDone,
				Content:          "The function has a potential race condition.",
				Model:            "openai/gpt-4.1",
				Usage:            &Usage{InputTokens: 4200, OutputTokens: 890},
				CostMicrodollars: 15200,
			},
		},
		{
			name: "error",
			response: Response{
				Type:  ResponseError,
				Error: "quota exceeded for project iree-amdgpu",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := codec.Marshal(tt.response)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			var decoded Response
			if err := codec.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}

			if decoded.Type != tt.response.Type {
				t.Errorf("Type = %q, want %q", decoded.Type, tt.response.Type)
			}
			if decoded.Content != tt.response.Content {
				t.Errorf("Content = %q, want %q", decoded.Content, tt.response.Content)
			}
			if decoded.Error != tt.response.Error {
				t.Errorf("Error = %q, want %q", decoded.Error, tt.response.Error)
			}
			if tt.response.Usage != nil {
				if decoded.Usage == nil {
					t.Fatal("Usage is nil, want non-nil")
				}
				if decoded.Usage.InputTokens != tt.response.Usage.InputTokens {
					t.Errorf("InputTokens = %d, want %d", decoded.Usage.InputTokens, tt.response.Usage.InputTokens)
				}
				if decoded.Usage.OutputTokens != tt.response.Usage.OutputTokens {
					t.Errorf("OutputTokens = %d, want %d", decoded.Usage.OutputTokens, tt.response.Usage.OutputTokens)
				}
			}
		})
	}
}

// TestWireEmbedResponse_CBORRoundTrip verifies embedding responses
// with raw float vector bytes.
func TestWireEmbedResponse_CBORRoundTrip(t *testing.T) {
	// Simulate two 4-dimensional float32 vectors (16 bytes each).
	vector1 := []byte{0x00, 0x00, 0x80, 0x3f, 0x00, 0x00, 0x00, 0x40,
		0x00, 0x00, 0x40, 0x40, 0x00, 0x00, 0x80, 0x40}
	vector2 := []byte{0x00, 0x00, 0xa0, 0x40, 0x00, 0x00, 0xc0, 0x40,
		0x00, 0x00, 0xe0, 0x40, 0x00, 0x00, 0x00, 0x41}

	original := EmbedResponse{
		Embeddings:       [][]byte{vector1, vector2},
		Model:            "nomic-embed-text-v1.5",
		Usage:            &Usage{InputTokens: 512},
		CostMicrodollars: 0,
	}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded EmbedResponse
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Embeddings) != 2 {
		t.Fatalf("Embeddings length = %d, want 2", len(decoded.Embeddings))
	}
	if len(decoded.Embeddings[0]) != 16 {
		t.Errorf("Embeddings[0] length = %d, want 16", len(decoded.Embeddings[0]))
	}
	if decoded.Model != "nomic-embed-text-v1.5" {
		t.Errorf("Model = %q, want %q", decoded.Model, "nomic-embed-text-v1.5")
	}
}

// TestWireRequest_ContinuationID verifies that continuation threading
// fields survive CBOR round-trip.
func TestWireRequest_ContinuationID(t *testing.T) {
	original := Request{
		Action:         ActionComplete,
		Model:          "codex",
		ContinuationID: "abc-123",
		Messages: []Message{
			{Role: "user", Content: "Now check for memory leaks too."},
		},
	}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded Request
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.ContinuationID != "abc-123" {
		t.Errorf("ContinuationID = %q, want %q", decoded.ContinuationID, "abc-123")
	}
}

// TestWireUsageEvent_CBORRoundTrip verifies the telemetry usage event.
func TestWireUsageEvent_CBORRoundTrip(t *testing.T) {
	original := UsageEvent{
		Project:          "iree-amdgpu",
		Agent:            "@iree/fleet/prod/agent/code-reviewer:bureau.local",
		ModelAlias:       "codex",
		Provider:         "openrouter",
		ProviderModel:    "openai/gpt-4.1",
		Account:          "openrouter-alice",
		InputTokens:      12000,
		OutputTokens:     3500,
		CostMicrodollars: 52000,
		LatencyMS:        2340,
		BatchSize:        1,
	}

	data, err := codec.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded UsageEvent
	if err := codec.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Project != "iree-amdgpu" {
		t.Errorf("Project = %q, want %q", decoded.Project, "iree-amdgpu")
	}
	if decoded.CostMicrodollars != 52000 {
		t.Errorf("CostMicrodollars = %d, want 52000", decoded.CostMicrodollars)
	}
	if decoded.LatencyMS != 2340 {
		t.Errorf("LatencyMS = %d, want 2340", decoded.LatencyMS)
	}
}

func TestLatencyPolicy_IsKnown(t *testing.T) {
	for _, policy := range []LatencyPolicy{LatencyImmediate, LatencyBatch, LatencyBackground} {
		if !policy.IsKnown() {
			t.Errorf("%q should be known", policy)
		}
	}
	if LatencyPolicy("turbo").IsKnown() {
		t.Error("turbo should not be known")
	}
}

func TestResponseType_IsKnown(t *testing.T) {
	for _, responseType := range []ResponseType{ResponseDelta, ResponseDone, ResponseError} {
		if !responseType.IsKnown() {
			t.Errorf("%q should be known", responseType)
		}
	}
	if ResponseType("partial").IsKnown() {
		t.Error("partial should not be known")
	}
}

// TestWireRequest_CompactSize verifies that CBOR keyasint encoding
// produces reasonably compact wire representations. A typical
// completion request should be well under 1KB (without large
// attachments).
func TestWireRequest_CompactSize(t *testing.T) {
	request := Request{
		Action: ActionComplete,
		Model:  "codex",
		Messages: []Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "Write a hello world function."},
		},
		Stream:        true,
		LatencyPolicy: LatencyImmediate,
	}

	data, err := codec.Marshal(request)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	t.Logf("completion request wire size: %d bytes", len(data))

	// With keyasint, field keys are 1-byte integers instead of
	// multi-byte strings. A simple request should be well under 256
	// bytes.
	if len(data) > 256 {
		t.Errorf("request unexpectedly large: %d bytes", len(data))
	}
}
