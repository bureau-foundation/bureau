// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/proxyclient"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// identityResourceURI is the canonical URI for the identity resource.
const identityResourceURI = "bureau://identity"

// identityGrantAction is the grant action required to access identity
// resources. Checked at both the provider level (MCP authorization) and
// when the resource is read.
const identityGrantAction = "resource/identity"

// IdentityProvider exposes the sandbox principal's identity, grants,
// and visible services as a single MCP resource. The resource is static
// for the lifetime of the sandbox: the principal's identity and grants
// are fixed at creation time, and service registrations change only
// when the daemon reconciles (which restarts the sandbox).
//
// Subscriptions are not supported because the underlying data does not
// change during a sandbox's lifetime.
type IdentityProvider struct {
	proxy *proxyclient.Client
}

// NewIdentityProvider creates an identity resource provider that reads
// from the given proxy client.
func NewIdentityProvider(proxy *proxyclient.Client) *IdentityProvider {
	return &IdentityProvider{proxy: proxy}
}

// Handles returns true for the identity resource URI.
func (p *IdentityProvider) Handles(uri string) bool {
	return uri == identityResourceURI
}

// List returns a single concrete resource description for the
// principal's identity.
func (p *IdentityProvider) List(_ context.Context, _ []schema.Grant) ([]resourceDescription, []resourceTemplate) {
	return []resourceDescription{
		{
			URI:      identityResourceURI,
			Name:     "Principal identity",
			MIMEType: "application/json",
			Description: "The sandbox principal's identity, authorization grants, " +
				"and visible service directory. Static for the sandbox lifetime.",
			Annotations: &resourceAnnotation{
				Audience: []string{"assistant"},
				Priority: 0.8,
			},
		},
	}, nil
}

// identityResource is the JSON structure returned by reading the
// identity resource. Combines identity, grants, and services into
// a single read for efficient context loading.
type identityResource struct {
	Identity *proxyclient.IdentityResponse `json:"identity"`
	Grants   []schema.Grant                `json:"grants"`
	Services []proxyclient.ServiceEntry    `json:"services"`
}

// Read fetches the principal's identity, grants, and service directory
// from the proxy and returns them as a single JSON resource.
func (p *IdentityProvider) Read(ctx context.Context, uri string) ([]resourceContent, error) {
	if uri != identityResourceURI {
		return nil, fmt.Errorf("unknown identity resource: %s", uri)
	}

	identity, err := p.proxy.Identity(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading identity: %w", err)
	}

	grants, err := p.proxy.Grants(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading grants: %w", err)
	}

	services, err := p.proxy.Services(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading services: %w", err)
	}

	data, err := json.Marshal(identityResource{
		Identity: identity,
		Grants:   grants,
		Services: services,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling identity resource: %w", err)
	}

	return []resourceContent{
		{
			URI:      identityResourceURI,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

// Subscribe returns an error: identity is static for the sandbox
// lifetime and does not change.
func (p *IdentityProvider) Subscribe(_ context.Context, _ string, _ func(string)) (func(), error) {
	return nil, fmt.Errorf("identity resources do not support subscriptions (static for sandbox lifetime)")
}

// GrantAction returns the grant action pattern for identity resources.
func (p *IdentityProvider) GrantAction() string {
	return identityGrantAction
}
