// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/modelprovider"
	"github.com/bureau-foundation/bureau/lib/modelregistry"
	"github.com/bureau-foundation/bureau/lib/schema/model"
)

func TestIsRetriableProviderError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retriable bool
	}{
		{
			name:      "nil error",
			err:       nil,
			retriable: false,
		},
		{
			name:      "connection refused",
			err:       errors.New("dial tcp 10.0.0.1:443: connection refused"),
			retriable: true,
		},
		{
			name:      "DNS failure",
			err:       errors.New("dial tcp: lookup api.openrouter.ai: no such host"),
			retriable: true,
		},
		{
			name:      "i/o timeout",
			err:       errors.New("read tcp 10.0.0.1:443: i/o timeout"),
			retriable: true,
		},
		{
			name:      "connection reset",
			err:       errors.New("read tcp 10.0.0.1:443: connection reset by peer"),
			retriable: true,
		},
		{
			name:      "unexpected EOF",
			err:       errors.New("unexpected EOF"),
			retriable: true,
		},
		{
			name:      "HTTP 500",
			err:       errors.New("provider returned status 500: internal server error"),
			retriable: true,
		},
		{
			name:      "HTTP 502",
			err:       errors.New("upstream returned 502"),
			retriable: true,
		},
		{
			name:      "HTTP 503",
			err:       errors.New("service unavailable: 503"),
			retriable: true,
		},
		{
			name:      "HTTP 504",
			err:       errors.New("gateway timeout 504"),
			retriable: true,
		},
		{
			name:      "HTTP 429 rate limited",
			err:       errors.New("provider returned status 429: rate limited"),
			retriable: true,
		},
		{
			name:      "HTTP 400 bad request",
			err:       errors.New("provider returned status 400: bad request"),
			retriable: false,
		},
		{
			name:      "HTTP 401 unauthorized",
			err:       errors.New("provider returned status 401: unauthorized"),
			retriable: false,
		},
		{
			name:      "HTTP 403 forbidden",
			err:       errors.New("provider returned status 403: forbidden"),
			retriable: false,
		},
		{
			name:      "HTTP 404 not found",
			err:       errors.New("provider returned status 404: model not found"),
			retriable: false,
		},
		{
			name:      "generic application error",
			err:       errors.New("invalid model format"),
			retriable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetriableProviderError(tt.err)
			if got != tt.retriable {
				t.Errorf("isRetriableProviderError(%v) = %v, want %v", tt.err, got, tt.retriable)
			}
		})
	}
}

func TestResolveModelChain_ExplicitAlias(t *testing.T) {
	logger := slog.Default()
	fakeClock := clock.Real()

	ms := &ModelService{
		clock:        fakeClock,
		logger:       logger,
		registry:     modelregistry.New(logger),
		quotaTracker: modelregistry.NewQuotaTracker(fakeClock),
		providers:    make(map[string]modelprovider.Provider),
	}

	ms.registry.SetProvider("primary", model.ModelProviderContent{
		Endpoint:     "https://primary.ai",
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"chat"},
	})
	ms.registry.SetProvider("fallback", model.ModelProviderContent{
		Endpoint:     "https://fallback.ai",
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"chat"},
	})
	ms.registry.SetAlias("ha-model", model.ModelAliasContent{
		Provider:      "primary",
		ProviderModel: "gpt-5",
		Pricing:       model.Pricing{InputPerMtokMicrodollars: 5000},
		Fallbacks: []model.ModelAliasFallback{
			{Provider: "fallback", ProviderModel: "claude-sonnet-4-6"},
		},
	})

	chain, err := ms.resolveModelChain("ha-model")
	if err != nil {
		t.Fatalf("resolveModelChain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("expected 2 resolutions, got %d", len(chain))
	}
	if chain[0].ProviderName != "primary" {
		t.Errorf("chain[0] provider = %q, want primary", chain[0].ProviderName)
	}
	if chain[1].ProviderName != "fallback" {
		t.Errorf("chain[1] provider = %q, want fallback", chain[1].ProviderName)
	}
}

func TestResolveModelChain_Auto(t *testing.T) {
	logger := slog.Default()
	fakeClock := clock.Real()

	ms := &ModelService{
		clock:        fakeClock,
		logger:       logger,
		registry:     modelregistry.New(logger),
		quotaTracker: modelregistry.NewQuotaTracker(fakeClock),
		providers:    make(map[string]modelprovider.Provider),
	}

	ms.registry.SetProvider("only", model.ModelProviderContent{
		Endpoint:     "https://only.ai",
		AuthMethod:   model.AuthMethodBearer,
		Capabilities: []string{"chat"},
	})
	ms.registry.SetAlias("the-model", model.ModelAliasContent{
		Provider:      "only",
		ProviderModel: "model-x",
		Pricing:       model.Pricing{InputPerMtokMicrodollars: 1000},
		Capabilities:  []string{"chat"},
	})

	chain, err := ms.resolveModelChain("auto")
	if err != nil {
		t.Fatalf("resolveModelChain(auto): %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("expected 1 resolution for auto, got %d", len(chain))
	}
}

// failingEmbedProvider returns an error on Embed calls.
type failingEmbedProvider struct {
	err error
}

func (p *failingEmbedProvider) Complete(_ context.Context, _ *modelprovider.CompleteRequest) (modelprovider.CompletionStream, error) {
	return nil, errors.New("not implemented")
}

func (p *failingEmbedProvider) Embed(_ context.Context, _ *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error) {
	return nil, p.err
}

func (p *failingEmbedProvider) Close() error { return nil }

// successEmbedProvider returns a fixed embedding result.
type successEmbedProvider struct {
	modelName string
}

func (p *successEmbedProvider) Complete(_ context.Context, _ *modelprovider.CompleteRequest) (modelprovider.CompletionStream, error) {
	return nil, errors.New("not implemented")
}

func (p *successEmbedProvider) Embed(_ context.Context, _ *modelprovider.EmbedRequest) (*modelprovider.EmbedResult, error) {
	return &modelprovider.EmbedResult{
		Embeddings: [][]byte{[]byte("mock-embedding")},
		Model:      p.modelName,
		Usage:      model.Usage{InputTokens: 10},
	}, nil
}

func (p *successEmbedProvider) Close() error { return nil }

func newTestModelService(t *testing.T) *ModelService {
	t.Helper()
	logger := slog.Default()
	fakeClock := clock.Real()

	ms := &ModelService{
		clock:        fakeClock,
		logger:       logger,
		registry:     modelregistry.New(logger),
		quotaTracker: modelregistry.NewQuotaTracker(fakeClock),
		providers:    make(map[string]modelprovider.Provider),
		latencyRouter: NewLatencyRouter(context.Background(), fakeClock, logger, LatencyConfig{
			FlushDelay:          50 * time.Millisecond,
			DefaultMaxBatchSize: 8,
		}),
	}

	return ms
}

func setupFallbackProviders(t *testing.T, ms *ModelService) {
	t.Helper()

	ms.registry.SetProvider("primary", model.ModelProviderContent{
		Endpoint:     "https://primary.ai",
		AuthMethod:   model.AuthMethodNone,
		Capabilities: []string{"chat", "embeddings"},
	})
	ms.registry.SetProvider("fallback", model.ModelProviderContent{
		Endpoint:     "https://fallback.ai",
		AuthMethod:   model.AuthMethodNone,
		Capabilities: []string{"chat", "embeddings"},
	})

	ms.registry.SetAlias("ha-embed", model.ModelAliasContent{
		Provider:      "primary",
		ProviderModel: "embed-v1",
		Pricing:       model.Pricing{InputPerMtokMicrodollars: 100},
		Capabilities:  []string{"embeddings"},
		Fallbacks: []model.ModelAliasFallback{
			{Provider: "fallback", ProviderModel: "embed-backup"},
		},
	})

	ms.registry.SetAccount("test-account-primary", model.ModelAccountContent{
		Provider:      "primary",
		CredentialRef: "",
		Projects:      []string{"*"},
		Priority:      0,
	})
	ms.registry.SetAccount("test-account-fallback", model.ModelAccountContent{
		Provider:      "fallback",
		CredentialRef: "",
		Projects:      []string{"*"},
		Priority:      0,
	})
}

func TestEmbedFallback_PrimaryFails_FallbackSucceeds(t *testing.T) {
	ms := newTestModelService(t)
	setupFallbackProviders(t, ms)

	// Primary fails with retriable error, fallback succeeds.
	ms.providers["primary"] = &failingEmbedProvider{
		err: errors.New("dial tcp: connection refused"),
	}
	ms.providers["fallback"] = &successEmbedProvider{
		modelName: "embed-backup",
	}

	chain, err := ms.resolveModelChain("ha-embed")
	if err != nil {
		t.Fatalf("resolveModelChain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("expected 2 resolutions, got %d", len(chain))
	}

	// Simulate what handleEmbed does: loop over chain.
	var lastError error
	var succeeded bool
	for chainIndex, candidate := range chain {
		provider := ms.getOrCreateProvider(candidate.ProviderName, candidate.Provider)
		_, attemptErr := provider.Embed(context.Background(), &modelprovider.EmbedRequest{
			Model: candidate.ProviderModel,
			Input: [][]byte{[]byte("test input")},
		})

		if attemptErr == nil {
			if chainIndex > 0 {
				t.Logf("fallback activated at index %d (provider=%s)", chainIndex, candidate.ProviderName)
			}
			succeeded = true
			break
		}

		if !isRetriableProviderError(attemptErr) {
			t.Fatalf("non-retriable error at index %d: %v", chainIndex, attemptErr)
		}
		lastError = attemptErr
	}

	if !succeeded {
		t.Fatalf("all providers failed, last error: %v", lastError)
	}
}

func TestEmbedFallback_AllFail(t *testing.T) {
	ms := newTestModelService(t)
	setupFallbackProviders(t, ms)

	ms.providers["primary"] = &failingEmbedProvider{
		err: errors.New("connection refused"),
	}
	ms.providers["fallback"] = &failingEmbedProvider{
		err: errors.New("connection refused"),
	}

	chain, err := ms.resolveModelChain("ha-embed")
	if err != nil {
		t.Fatalf("resolveModelChain: %v", err)
	}

	allFailed := true
	for _, candidate := range chain {
		provider := ms.getOrCreateProvider(candidate.ProviderName, candidate.Provider)
		_, attemptErr := provider.Embed(context.Background(), &modelprovider.EmbedRequest{
			Model: candidate.ProviderModel,
			Input: [][]byte{[]byte("test")},
		})
		if attemptErr == nil {
			allFailed = false
			break
		}
	}

	if !allFailed {
		t.Fatal("expected all providers to fail")
	}
}

func TestEmbedFallback_NonRetriableStopsChain(t *testing.T) {
	ms := newTestModelService(t)
	setupFallbackProviders(t, ms)

	// Primary fails with a non-retriable error (400). Should NOT
	// try the fallback.
	ms.providers["primary"] = &failingEmbedProvider{
		err: errors.New("provider returned status 400: invalid input"),
	}
	ms.providers["fallback"] = &successEmbedProvider{
		modelName: "should-not-reach",
	}

	chain, err := ms.resolveModelChain("ha-embed")
	if err != nil {
		t.Fatalf("resolveModelChain: %v", err)
	}

	for _, candidate := range chain {
		provider := ms.getOrCreateProvider(candidate.ProviderName, candidate.Provider)
		_, attemptErr := provider.Embed(context.Background(), &modelprovider.EmbedRequest{
			Model: candidate.ProviderModel,
			Input: [][]byte{[]byte("test")},
		})
		if attemptErr == nil {
			t.Fatal("expected primary to fail, but it succeeded")
		}
		if !isRetriableProviderError(attemptErr) {
			// Non-retriable: stop the chain. This is the correct behavior.
			return
		}
	}
	t.Fatal("expected chain to stop at non-retriable error")
}

func TestEmbedFallback_NoFallbacks_SingleAttempt(t *testing.T) {
	ms := newTestModelService(t)

	ms.registry.SetProvider("only", model.ModelProviderContent{
		Endpoint:     "https://only.ai",
		AuthMethod:   model.AuthMethodNone,
		Capabilities: []string{"embeddings"},
	})
	ms.registry.SetAlias("no-fallback", model.ModelAliasContent{
		Provider:      "only",
		ProviderModel: "embed-v1",
		Pricing:       model.Pricing{InputPerMtokMicrodollars: 100},
		// No Fallbacks field — fail-loud.
	})
	ms.registry.SetAccount("test-account", model.ModelAccountContent{
		Provider: "only",
		Projects: []string{"*"},
	})

	ms.providers["only"] = &failingEmbedProvider{
		err: errors.New("connection refused"),
	}

	chain, err := ms.resolveModelChain("no-fallback")
	if err != nil {
		t.Fatalf("resolveModelChain: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("expected 1 resolution (no fallbacks), got %d", len(chain))
	}

	provider := ms.getOrCreateProvider(chain[0].ProviderName, chain[0].Provider)
	_, attemptErr := provider.Embed(context.Background(), &modelprovider.EmbedRequest{
		Model: chain[0].ProviderModel,
		Input: [][]byte{[]byte("test")},
	})
	if attemptErr == nil {
		t.Fatal("expected error from only provider")
	}
}
