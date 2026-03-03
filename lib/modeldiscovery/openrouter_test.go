// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modeldiscovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// testModelsResponse is a minimal OpenRouter /api/v1/models response
// with three models covering different pricing and capability profiles.
const testModelsResponse = `{
  "data": [
    {
      "id": "anthropic/claude-sonnet-4.6",
      "name": "Anthropic: Claude Sonnet 4.6",
      "created": 1740000000,
      "description": "Fast, capable model for everyday tasks.",
      "context_length": 200000,
      "architecture": {
        "modality": "text+image->text",
        "input_modalities": ["text", "image"],
        "output_modalities": ["text"],
        "tokenizer": "Claude"
      },
      "pricing": {
        "prompt": "0.000003",
        "completion": "0.000015"
      },
      "top_provider": {
        "context_length": 200000,
        "max_completion_tokens": 128000,
        "is_moderated": true
      },
      "supported_parameters": ["temperature", "tools", "tool_choice", "reasoning"],
      "expiration_date": "",
      "default_parameters": {}
    },
    {
      "id": "qwenlm/qwen3-32b",
      "name": "Qwen3 32B",
      "created": 1738000000,
      "description": "Efficient open-weight model.",
      "context_length": 131072,
      "architecture": {
        "modality": "text->text",
        "input_modalities": ["text"],
        "output_modalities": ["text"],
        "tokenizer": "Qwen3"
      },
      "pricing": {
        "prompt": "0.0000002",
        "completion": "0.0000004"
      },
      "top_provider": {
        "context_length": 131072,
        "max_completion_tokens": 32768,
        "is_moderated": false
      },
      "supported_parameters": ["temperature", "top_p"],
      "expiration_date": "",
      "default_parameters": {}
    },
    {
      "id": "openai/gpt-4.1",
      "name": "OpenAI: GPT-4.1",
      "created": 1735000000,
      "description": "Latest GPT model.",
      "context_length": 1047576,
      "architecture": {
        "modality": "text+image+file->text",
        "input_modalities": ["text", "image", "file"],
        "output_modalities": ["text"],
        "tokenizer": "GPT"
      },
      "pricing": {
        "prompt": "0.000002",
        "completion": "0.000008"
      },
      "top_provider": {
        "context_length": 1047576,
        "max_completion_tokens": 32768,
        "is_moderated": true
      },
      "supported_parameters": ["temperature", "tools", "structured_outputs"],
      "expiration_date": "",
      "default_parameters": {}
    }
  ]
}`

func TestOpenRouterFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != "GET" {
			t.Errorf("method = %s, want GET", request.Method)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(testModelsResponse))
	}))
	defer server.Close()

	provider := &OpenRouterProvider{Endpoint: server.URL}
	entries, err := provider.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}

	// Verify Claude Sonnet conversion.
	sonnet := entries[0]
	if sonnet.ID != "anthropic/claude-sonnet-4.6" {
		t.Errorf("entries[0].ID = %q", sonnet.ID)
	}
	if sonnet.Name != "Anthropic: Claude Sonnet 4.6" {
		t.Errorf("entries[0].Name = %q", sonnet.Name)
	}
	if sonnet.ContextLength != 200000 {
		t.Errorf("entries[0].ContextLength = %d, want 200000", sonnet.ContextLength)
	}
	if sonnet.InputPriceMicrodollarsPerMtok != 3_000_000 {
		t.Errorf("entries[0].InputPrice = %d, want 3000000", sonnet.InputPriceMicrodollarsPerMtok)
	}
	if sonnet.OutputPriceMicrodollarsPerMtok != 15_000_000 {
		t.Errorf("entries[0].OutputPrice = %d, want 15000000", sonnet.OutputPriceMicrodollarsPerMtok)
	}
	if len(sonnet.InputModalities) != 2 || sonnet.InputModalities[0] != "text" {
		t.Errorf("entries[0].InputModalities = %v", sonnet.InputModalities)
	}
	if len(sonnet.SupportedParameters) != 4 {
		t.Errorf("entries[0].SupportedParameters = %v", sonnet.SupportedParameters)
	}

	// Verify Qwen conversion — very cheap model.
	qwen := entries[1]
	if qwen.InputPriceMicrodollarsPerMtok != 200_000 {
		t.Errorf("entries[1].InputPrice = %d, want 200000", qwen.InputPriceMicrodollarsPerMtok)
	}
	if qwen.OutputPriceMicrodollarsPerMtok != 400_000 {
		t.Errorf("entries[1].OutputPrice = %d, want 400000", qwen.OutputPriceMicrodollarsPerMtok)
	}
}

func TestOpenRouterFetchHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		writer.Write([]byte("maintenance"))
	}))
	defer server.Close()

	provider := &OpenRouterProvider{Endpoint: server.URL}
	_, err := provider.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch() should return error on HTTP 503")
	}
}

func TestOpenRouterProviderName(t *testing.T) {
	provider := &OpenRouterProvider{}
	if provider.Name() != "openrouter" {
		t.Errorf("Name() = %q, want %q", provider.Name(), "openrouter")
	}
}
