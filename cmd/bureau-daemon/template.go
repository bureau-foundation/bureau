// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	libtmpl "github.com/bureau-foundation/bureau/lib/templatedef"
	"github.com/bureau-foundation/bureau/messaging"
)

// resolveTemplate fetches a template from Matrix and walks its inheritance
// chain to produce a fully-merged TemplateContent. Delegates to
// lib/templatedef.Resolve.
func resolveTemplate(ctx context.Context, session *messaging.DirectSession, templateRef string, serverName ref.ServerName) (*schema.TemplateContent, error) {
	return libtmpl.Resolve(ctx, session, templateRef, serverName)
}

// resolveTemplateWithAuthor fetches a template from Matrix and walks its
// inheritance chain, additionally tracking the sender of the template state
// event that introduced the CredentialRef (if any). This is the template
// resolution path used when credential-bound templates need authorization
// checks against the template author.
func resolveTemplateWithAuthor(ctx context.Context, session *messaging.DirectSession, templateRef string, serverName ref.ServerName) (libtmpl.ResolveResult, error) {
	return libtmpl.ResolveWithAuthor(ctx, session, templateRef, serverName)
}

// resolveExtraInherits resolves additional template references and merges
// them on top of the base template. Each extra template is resolved through
// the same inheritance-walking path as the main template, then merged
// left-to-right using templatedef.Merge (maps merge by key, slices append,
// extras win on conflict). This provides deployment-time mix-in composition:
// an operator can add capabilities (e.g., GitHub API proxy with forge
// attribution interceptors) to any principal without modifying the template.
func resolveExtraInherits(ctx context.Context, session *messaging.DirectSession, base *schema.TemplateContent, extraInherits []string, serverName ref.ServerName) (*schema.TemplateContent, error) {
	result := base
	for _, extraRef := range extraInherits {
		extra, err := resolveTemplate(ctx, session, extraRef, serverName)
		if err != nil {
			return nil, fmt.Errorf("resolving extra inherit %q: %w", extraRef, err)
		}
		merged := libtmpl.Merge(result, extra)
		result = &merged
	}
	return result, nil
}

// resolveExtraInheritsWithAuthor resolves additional template references
// and merges them on top of the base template, tracking CredentialRefAuthor
// through the merge chain. If an extra inherit overrides the base template's
// CredentialRef, the extra inherit's author becomes the credential ref author.
func resolveExtraInheritsWithAuthor(
	ctx context.Context,
	session *messaging.DirectSession,
	base libtmpl.ResolveResult,
	extraInherits []string,
	serverName ref.ServerName,
) (libtmpl.ResolveResult, error) {
	result := base
	for _, extraRef := range extraInherits {
		extra, err := resolveTemplateWithAuthor(ctx, session, extraRef, serverName)
		if err != nil {
			return libtmpl.ResolveResult{}, fmt.Errorf("resolving extra inherit %q: %w", extraRef, err)
		}
		merged := libtmpl.Merge(result.Template, extra.Template)
		// If the extra inherit has a CredentialRef, its author wins
		// (scalar override: the extra is applied after the base).
		author := result.CredentialRefAuthor
		if extra.Template.CredentialRef != "" {
			author = extra.CredentialRefAuthor
		}
		result = libtmpl.ResolveResult{Template: &merged, CredentialRefAuthor: author}
	}
	return result, nil
}

// resolveInstanceConfig converts a fully-resolved TemplateContent into a
// SandboxSpec, applying PrincipalAssignment instance overrides (command,
// environment, extra env vars, payload).
func resolveInstanceConfig(template *schema.TemplateContent, assignment *schema.PrincipalAssignment) *schema.SandboxSpec {
	spec := &schema.SandboxSpec{
		Isolation:        template.Isolation,
		Command:          template.Command,
		Filesystem:       template.Filesystem,
		Namespaces:       template.Namespaces,
		Resources:        template.Resources,
		Security:         template.Security,
		OutputCapture:    template.OutputCapture,
		EnvironmentPath:  template.Environment,
		Roles:            template.Roles,
		CreateDirs:       template.CreateDirs,
		RequiredServices: template.RequiredServices,
		ProxyServices:    template.ProxyServices,
		Secrets:          template.Secrets,
	}

	// Copy environment variables so overrides don't mutate the template.
	if len(template.EnvironmentVariables) > 0 {
		spec.EnvironmentVariables = make(map[string]string, len(template.EnvironmentVariables))
		for key, value := range template.EnvironmentVariables {
			spec.EnvironmentVariables[key] = value
		}
	}

	// Copy prepend variables (deep copy: each slice is independent).
	if len(template.PrependVariables) > 0 {
		spec.PrependVariables = make(map[string][]string, len(template.PrependVariables))
		for key, values := range template.PrependVariables {
			copied := make([]string, len(values))
			copy(copied, values)
			spec.PrependVariables[key] = copied
		}
	}

	// Apply instance overrides.
	if len(assignment.CommandOverride) > 0 {
		spec.Command = assignment.CommandOverride
	}
	if assignment.EnvironmentOverride != "" {
		spec.EnvironmentPath = assignment.EnvironmentOverride
	}

	// Merge extra environment variables (instance wins on conflict).
	if len(assignment.ExtraEnvironmentVariables) > 0 {
		if spec.EnvironmentVariables == nil {
			spec.EnvironmentVariables = make(map[string]string, len(assignment.ExtraEnvironmentVariables))
		}
		for key, value := range assignment.ExtraEnvironmentVariables {
			spec.EnvironmentVariables[key] = value
		}
	}

	// Override required services if the assignment specifies them.
	if len(assignment.RequiredServicesOverride) > 0 {
		spec.RequiredServices = assignment.RequiredServicesOverride
	}

	// Override secrets if the assignment specifies them.
	if assignment.SecretsOverride != nil {
		spec.Secrets = assignment.SecretsOverride
	}

	// Merge payload: template DefaultPayload as base, assignment Payload wins.
	spec.Payload = libtmpl.MergeAnyMaps(template.DefaultPayload, assignment.Payload)

	return spec
}
