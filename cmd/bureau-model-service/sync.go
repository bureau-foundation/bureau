// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/model"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/messaging"
)

// syncFilter restricts the /sync response to event types the model
// service cares about. Built from typed constants so that event type
// renames are caught at compile time.
//
// The timeline section includes the same types as the state section
// because state events can appear as timeline events during incremental
// sync. The limit is generous since the service needs to see all
// configuration mutations.
var syncFilter = buildSyncFilter()

// buildSyncFilter constructs the Matrix /sync filter JSON from typed
// schema constants.
func buildSyncFilter() string {
	stateEventTypes := []ref.EventType{
		model.EventTypeModelProvider,
		model.EventTypeModelAlias,
		model.EventTypeModelAccount,
	}

	// Timeline includes the same state event types (state events can
	// appear as timeline events with a non-nil state_key during
	// incremental sync).
	timelineEventTypes := make([]ref.EventType, len(stateEventTypes))
	copy(timelineEventTypes, stateEventTypes)

	// Convert to string slices for JSON marshaling.
	stateTypeStrings := make([]string, len(stateEventTypes))
	for i, eventType := range stateEventTypes {
		stateTypeStrings[i] = eventType.String()
	}
	timelineTypeStrings := make([]string, len(timelineEventTypes))
	for i, eventType := range timelineEventTypes {
		timelineTypeStrings[i] = eventType.String()
	}

	filter := map[string]any{
		"room": map[string]any{
			"state": map[string]any{
				"types": stateTypeStrings,
			},
			"timeline": map[string]any{
				"types": timelineTypeStrings,
				"limit": 1000,
			},
			"ephemeral": map[string]any{
				"types": []string{},
			},
			"account_data": map[string]any{
				"types": []string{},
			},
		},
		"presence": map[string]any{
			"types": []string{},
		},
		"account_data": map[string]any{
			"types": []string{},
		},
	}

	filterJSON, err := json.Marshal(filter)
	if err != nil {
		panic("model service: failed to marshal sync filter: " + err.Error())
	}
	return string(filterJSON)
}

// initialSync performs the first /sync and populates the model registry
// with provider, alias, and account configuration. Returns the since
// token for incremental sync.
func (ms *ModelService) initialSync(ctx context.Context) (string, error) {
	sinceToken, response, err := service.InitialSync(ctx, ms.session, syncFilter)
	if err != nil {
		return "", err
	}

	ms.logger.Info("initial sync complete",
		"next_batch", sinceToken,
		"joined_rooms", len(response.Rooms.Join),
		"pending_invites", len(response.Rooms.Invite),
	)

	// Accept pending invites. The service may have been invited to
	// rooms while it was offline.
	service.AcceptInvites(ctx, ms.session, response.Rooms.Invite, ms.logger)

	// Process state from all joined rooms to populate the registry.
	for _, room := range response.Rooms.Join {
		ms.processStateEvents(room.State.Events)
		ms.processStateEvents(room.Timeline.Events)
	}

	ms.logger.Info("model registry populated",
		"providers", ms.registry.ProviderCount(),
		"aliases", ms.registry.AliasCount(),
		"accounts", ms.registry.AccountCount(),
	)

	return sinceToken, nil
}

// handleSync processes an incremental /sync response. Called by the
// sync loop for each batch of events.
func (ms *ModelService) handleSync(ctx context.Context, response *messaging.SyncResponse) {
	// Accept any new invites.
	if len(response.Rooms.Invite) > 0 {
		service.AcceptInvites(ctx, ms.session, response.Rooms.Invite, ms.logger)
	}

	// Process state events from joined rooms.
	for _, room := range response.Rooms.Join {
		ms.processStateEvents(room.State.Events)
		ms.processStateEvents(room.Timeline.Events)
	}
}

// processStateEvents handles a batch of state events, updating the
// model registry for provider, alias, and account events.
func (ms *ModelService) processStateEvents(events []messaging.Event) {
	for _, event := range events {
		if event.StateKey == nil {
			continue
		}
		stateKey := *event.StateKey

		switch ref.EventType(event.Type) {
		case model.EventTypeModelProvider:
			ms.indexProvider(stateKey, event.Content)
		case model.EventTypeModelAlias:
			ms.indexAlias(stateKey, event.Content)
		case model.EventTypeModelAccount:
			ms.indexAccount(stateKey, event.Content)
		}
	}
}

// indexProvider updates the registry with a provider state event.
// Empty content (len == 0) is treated as deletion.
func (ms *ModelService) indexProvider(stateKey string, content map[string]any) {
	if len(content) == 0 {
		ms.registry.RemoveProvider(stateKey)
		// Invalidate the cached provider instance so it's recreated
		// with the new configuration if the provider is re-added.
		ms.removeProviderInstance(stateKey)
		ms.logger.Info("provider removed", "provider", stateKey)
		return
	}

	var typed model.ModelProviderContent
	if err := remarshal(content, &typed); err != nil {
		ms.logger.Error("failed to decode provider event",
			"provider", stateKey,
			"error", err,
		)
		return
	}

	if err := typed.Validate(); err != nil {
		ms.logger.Error("invalid provider event",
			"provider", stateKey,
			"error", err,
		)
		return
	}

	ms.registry.SetProvider(stateKey, typed)
	// Invalidate the cached provider instance so it's recreated
	// with the updated endpoint/config on next use.
	ms.removeProviderInstance(stateKey)
	ms.logger.Info("provider indexed",
		"provider", stateKey,
		"endpoint", typed.Endpoint,
		"auth_method", typed.AuthMethod,
	)
}

// indexAlias updates the registry with an alias state event.
func (ms *ModelService) indexAlias(stateKey string, content map[string]any) {
	if len(content) == 0 {
		ms.registry.RemoveAlias(stateKey)
		ms.logger.Info("alias removed", "alias", stateKey)
		return
	}

	var typed model.ModelAliasContent
	if err := remarshal(content, &typed); err != nil {
		ms.logger.Error("failed to decode alias event",
			"alias", stateKey,
			"error", err,
		)
		return
	}

	if err := typed.Validate(); err != nil {
		ms.logger.Error("invalid alias event",
			"alias", stateKey,
			"error", err,
		)
		return
	}

	ms.registry.SetAlias(stateKey, typed)
	ms.logger.Info("alias indexed",
		"alias", stateKey,
		"provider", typed.Provider,
		"model", typed.ProviderModel,
	)
}

// indexAccount updates the registry with an account state event.
func (ms *ModelService) indexAccount(stateKey string, content map[string]any) {
	if len(content) == 0 {
		ms.registry.RemoveAccount(stateKey)
		ms.logger.Info("account removed", "account", stateKey)
		return
	}

	var typed model.ModelAccountContent
	if err := remarshal(content, &typed); err != nil {
		ms.logger.Error("failed to decode account event",
			"account", stateKey,
			"error", err,
		)
		return
	}

	if err := typed.Validate(); err != nil {
		ms.logger.Error("invalid account event",
			"account", stateKey,
			"error", err,
		)
		return
	}

	// Validate that the credential exists if the provider requires
	// authentication. Missing credentials are logged as warnings, not
	// errors — the account is still indexed so that /sync configuration
	// is visible, but requests will fail at runtime with a clear error.
	if typed.CredentialRef != "" && ms.credentials.Get(typed.CredentialRef) == "" {
		ms.logger.Warn("account references missing credential",
			"account", stateKey,
			"credential_ref", typed.CredentialRef,
		)
	}

	ms.registry.SetAccount(stateKey, typed)
	ms.logger.Info("account indexed",
		"account", stateKey,
		"provider", typed.Provider,
		"projects", typed.Projects,
	)
}

// removeProviderInstance removes a cached provider HTTP client. Called
// when a provider's configuration changes so the next request creates
// a fresh client with updated settings.
func (ms *ModelService) removeProviderInstance(providerName string) {
	ms.providersMu.Lock()
	defer ms.providersMu.Unlock()

	if provider, ok := ms.providers[providerName]; ok {
		provider.Close()
		delete(ms.providers, providerName)
	}
}

// remarshal converts a map[string]any (from Matrix event JSON
// deserialization) into a typed struct. The map is re-serialized to
// JSON and deserialized into the target type.
func remarshal(content map[string]any, target any) error {
	contentJSON, err := json.Marshal(content)
	if err != nil {
		return err
	}
	return json.Unmarshal(contentJSON, target)
}
