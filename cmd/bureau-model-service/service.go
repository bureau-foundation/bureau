// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"sync"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/modelprovider"
	"github.com/bureau-foundation/bureau/lib/modelregistry"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/model"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/messaging"
)

// ModelService is the core service state. The model registry, quota
// tracker, provider map, and continuation store are the four mutable
// data structures:
//
//   - The registry is self-synchronized (internal RWMutex).
//   - The quota tracker is self-synchronized (internal Mutex).
//   - The provider map is protected by providersMu. Providers are
//     created lazily on first use and reused for all subsequent
//     requests to the same endpoint.
//   - The continuation store is self-synchronized (internal Mutex).
type ModelService struct {
	session   messaging.Session
	clock     clock.Clock
	service   ref.Service
	machine   ref.Machine
	telemetry *service.TelemetryEmitter
	logger    *slog.Logger

	// registry is the in-memory index of providers, aliases, and
	// accounts. Populated from Matrix state events during initial
	// sync and updated incrementally. Thread-safe (internal RWMutex).
	registry *modelregistry.Registry

	// quotaTracker tracks cumulative spend per account per time
	// window. Thread-safe (internal Mutex).
	quotaTracker *modelregistry.QuotaTracker

	// credentials maps credential_ref names (from ModelAccountContent)
	// to API key values. Read-only after construction — the launcher
	// delivers credentials at startup and they don't change until
	// restart.
	credentials *credentialStore

	// continuations stores conversation history for multi-turn
	// completion requests. Keyed by (agent, continuation_id). Entries
	// expire after defaultContinuationTTL. Thread-safe (internal
	// Mutex).
	continuations *continuationStore

	// latencyRouter coordinates batching, gating, and background
	// scheduling for all provider requests. Handlers call SubmitEmbed
	// or GateComplete instead of calling providers directly. Nil
	// until initialization in main.go.
	latencyRouter *LatencyRouter

	// providersMu protects the providers map. Providers are
	// long-lived HTTP clients keyed by provider name. Created lazily
	// on first request to that provider.
	providersMu sync.Mutex
	providers   map[string]modelprovider.Provider
}

// newModelService creates a ModelService from the bootstrap result and
// credential store.
func newModelService(boot *service.BootstrapResult, credentials *credentialStore) *ModelService {
	return &ModelService{
		session:       boot.Session,
		clock:         boot.Clock,
		service:       boot.Service,
		machine:       boot.Machine,
		telemetry:     boot.Telemetry,
		logger:        boot.Logger,
		registry:      modelregistry.New(),
		quotaTracker:  modelregistry.NewQuotaTracker(boot.Clock),
		credentials:   credentials,
		continuations: newContinuationStore(defaultContinuationTTL, boot.Clock),
		providers:     make(map[string]modelprovider.Provider),
	}
}

// registerActions registers all socket handlers on the server.
func (ms *ModelService) registerActions(server *service.SocketServer) {
	server.HandleAuthStream(model.ActionComplete, ms.handleComplete)
	server.HandleAuth(model.ActionEmbed, ms.handleEmbed)
	server.HandleAuth(model.ActionList, ms.handleList)
	server.HandleAuth(model.ActionStatus, ms.handleStatus)
}

// getOrCreateProvider returns a provider for the given name and
// configuration. Creates one on first access. The provider is reused
// for all requests to the same endpoint — only the credential changes
// per-request.
func (ms *ModelService) getOrCreateProvider(providerName string, config model.ModelProviderContent) modelprovider.Provider {
	ms.providersMu.Lock()
	defer ms.providersMu.Unlock()

	if existing, ok := ms.providers[providerName]; ok {
		return existing
	}

	// Determine if this is a Unix socket endpoint. Unix socket URIs
	// use the "unix://" scheme prefix.
	var options modelprovider.OpenAIOptions
	endpoint := config.Endpoint
	if len(endpoint) > 7 && endpoint[:7] == "unix://" {
		options.UnixSocket = endpoint[7:]
		// Local providers use http://localhost as the base URL — the
		// actual routing is via the Unix socket dialer.
		endpoint = "http://localhost/v1"
	}

	provider := modelprovider.NewOpenAI(endpoint, options)
	ms.providers[providerName] = provider

	ms.logger.Info("provider initialized",
		"provider", providerName,
		"endpoint", config.Endpoint,
	)

	return provider
}

// closeProviders closes all provider HTTP clients. Called during
// shutdown to release connection pools.
func (ms *ModelService) closeProviders() {
	ms.providersMu.Lock()
	defer ms.providersMu.Unlock()

	for name, provider := range ms.providers {
		if err := provider.Close(); err != nil {
			ms.logger.Error("failed to close provider",
				"provider", name,
				"error", err,
			)
		}
	}
}
