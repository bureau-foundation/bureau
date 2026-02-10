// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"github.com/bureau-foundation/bureau/lib/schema"
	libtmpl "github.com/bureau-foundation/bureau/lib/template"
	"github.com/bureau-foundation/bureau/messaging"
)

// resolveTemplate fetches a template from Matrix and walks its inheritance
// chain to produce a fully-merged TemplateContent. Delegates to
// lib/template.Resolve.
func resolveTemplate(ctx context.Context, session *messaging.Session, templateRef string, serverName string) (*schema.TemplateContent, error) {
	return libtmpl.Resolve(ctx, session, templateRef, serverName)
}

// resolveInstanceConfig converts a fully-resolved TemplateContent into a
// SandboxSpec, applying PrincipalAssignment instance overrides (command,
// environment, extra env vars, payload).
func resolveInstanceConfig(template *schema.TemplateContent, assignment *schema.PrincipalAssignment) *schema.SandboxSpec {
	spec := &schema.SandboxSpec{
		Command:         template.Command,
		Filesystem:      template.Filesystem,
		Namespaces:      template.Namespaces,
		Resources:       template.Resources,
		Security:        template.Security,
		EnvironmentPath: template.Environment,
		Roles:           template.Roles,
		CreateDirs:      template.CreateDirs,
	}

	// Copy environment variables so overrides don't mutate the template.
	if len(template.EnvironmentVariables) > 0 {
		spec.EnvironmentVariables = make(map[string]string, len(template.EnvironmentVariables))
		for key, value := range template.EnvironmentVariables {
			spec.EnvironmentVariables[key] = value
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

	// Merge payload: template DefaultPayload as base, assignment Payload wins.
	spec.Payload = libtmpl.MergeAnyMaps(template.DefaultPayload, assignment.Payload)

	return spec
}
