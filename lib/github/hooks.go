// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
)

// CreateWebhookRequest contains the fields for creating a repository
// webhook.
type CreateWebhookRequest struct {
	// Events is the list of event types to deliver. Use ["*"] for all events.
	Events []string `json:"events"`

	// Config holds the webhook endpoint configuration.
	Config CreateWebhookConfig `json:"config"`

	// Active enables or disables the webhook. Defaults to true.
	Active *bool `json:"active,omitempty"`
}

// CreateWebhookConfig is the webhook endpoint configuration for creation.
type CreateWebhookConfig struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"` // "json" or "form"
	Secret      string `json:"secret,omitempty"`
	InsecureSSL string `json:"insecure_ssl,omitempty"` // "0" (verify) or "1" (skip)
}

// UpdateWebhookRequest contains the fields for updating a repository
// webhook.
type UpdateWebhookRequest struct {
	Events []string             `json:"events,omitempty"`
	Config *CreateWebhookConfig `json:"config,omitempty"`
	Active *bool                `json:"active,omitempty"`
}

// UpdateAppWebhookRequest contains the fields for updating the GitHub
// App's own webhook configuration. The App webhook is created through
// the App settings page; this only updates the config.
type UpdateAppWebhookRequest struct {
	Config CreateWebhookConfig `json:"config"`
	Events []string            `json:"events,omitempty"`
	Active *bool               `json:"active,omitempty"`
}

// ListRepoWebhooks returns a paginated iterator over webhooks configured
// on a repository.
func (client *Client) ListRepoWebhooks(ctx context.Context, owner, repo string) *PageIterator[Webhook] {
	path := fmt.Sprintf("/repos/%s/%s/hooks", owner, repo)
	return list[Webhook](client, path)
}

// CreateRepoWebhook creates a webhook on a repository.
func (client *Client) CreateRepoWebhook(ctx context.Context, owner, repo string, request CreateWebhookRequest) (*Webhook, error) {
	var webhook Webhook
	path := fmt.Sprintf("/repos/%s/%s/hooks", owner, repo)
	if err := client.post(ctx, path, request, &webhook); err != nil {
		return nil, fmt.Errorf("creating webhook on %s/%s: %w", owner, repo, err)
	}
	return &webhook, nil
}

// UpdateRepoWebhook updates an existing repository webhook.
func (client *Client) UpdateRepoWebhook(ctx context.Context, owner, repo string, hookID int64, request UpdateWebhookRequest) (*Webhook, error) {
	var webhook Webhook
	path := fmt.Sprintf("/repos/%s/%s/hooks/%d", owner, repo, hookID)
	if err := client.patch(ctx, path, request, &webhook); err != nil {
		return nil, fmt.Errorf("updating webhook %d on %s/%s: %w", hookID, owner, repo, err)
	}
	return &webhook, nil
}

// DeleteRepoWebhook deletes a repository webhook.
func (client *Client) DeleteRepoWebhook(ctx context.Context, owner, repo string, hookID int64) error {
	path := fmt.Sprintf("/repos/%s/%s/hooks/%d", owner, repo, hookID)
	if err := client.delete(ctx, path); err != nil {
		return fmt.Errorf("deleting webhook %d on %s/%s: %w", hookID, owner, repo, err)
	}
	return nil
}

// UpdateAppWebhook updates the GitHub App's webhook configuration.
// This uses the App's JWT authentication (not an installation token)
// since it's an App-level operation.
func (client *Client) UpdateAppWebhook(ctx context.Context, request UpdateAppWebhookRequest) error {
	if err := client.patch(ctx, "/app/hook/config", request, nil); err != nil {
		return fmt.Errorf("updating app webhook: %w", err)
	}
	return nil
}
