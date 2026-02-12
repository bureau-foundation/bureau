// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// Resolve fetches a template from Matrix and walks its inheritance chain to
// produce a fully-merged TemplateContent. The returned content has all
// inherited fields merged (base → parent → child) with the Inherits field
// cleared.
//
// Cycle detection prevents infinite loops: if the same template reference
// appears twice in the chain, an error is returned.
func Resolve(ctx context.Context, session *messaging.Session, templateRef string, serverName string) (*schema.TemplateContent, error) {
	ref, err := schema.ParseTemplateRef(templateRef)
	if err != nil {
		return nil, fmt.Errorf("parsing template reference %q: %w", templateRef, err)
	}

	// Walk the inheritance chain, collecting templates from leaf to base.
	// chain[0] is the leaf (the directly-referenced template); chain[last]
	// is the root (the template with no Inherits).
	var chain []schema.TemplateContent
	visited := make(map[string]bool)
	currentRef := ref

	for {
		refString := currentRef.String()
		if visited[refString] {
			return nil, fmt.Errorf("template inheritance cycle: %q appears twice in chain", refString)
		}
		visited[refString] = true

		template, err := Fetch(ctx, session, currentRef, serverName)
		if err != nil {
			return nil, err
		}
		chain = append(chain, *template)

		if template.Inherits == "" {
			break
		}

		parentRef, err := schema.ParseTemplateRef(template.Inherits)
		if err != nil {
			return nil, fmt.Errorf("parsing inherits reference %q in template %q: %w", template.Inherits, refString, err)
		}
		currentRef = parentRef
	}

	// Merge from base to leaf. chain[last] is the base, chain[0] is the leaf.
	// Start with the base and merge each child on top.
	result := chain[len(chain)-1]
	for i := len(chain) - 2; i >= 0; i-- {
		result = Merge(&result, &chain[i])
	}

	return &result, nil
}

// Fetch resolves a single template reference to its content. It resolves
// the room alias to a room ID, then fetches the m.bureau.template state
// event for the template name. No inheritance resolution is performed.
func Fetch(ctx context.Context, session *messaging.Session, ref schema.TemplateRef, serverName string) (*schema.TemplateContent, error) {
	roomAlias := ref.RoomAlias(serverName)
	roomID, err := session.ResolveAlias(ctx, roomAlias)
	if err != nil {
		return nil, fmt.Errorf("resolving room alias %q for template %q: %w", roomAlias, ref.String(), err)
	}

	content, err := session.GetStateEvent(ctx, roomID, schema.EventTypeTemplate, ref.Template)
	if err != nil {
		return nil, fmt.Errorf("fetching template %q from room %q (%s): %w", ref.Template, roomAlias, roomID, err)
	}

	var template schema.TemplateContent
	if err := json.Unmarshal(content, &template); err != nil {
		return nil, fmt.Errorf("parsing template %q from room %q: %w", ref.Template, roomAlias, err)
	}

	return &template, nil
}

// Merge merges a child template over a parent template.
//
// Merge rules:
//   - Scalars (Description, Command, Environment): child replaces parent if non-zero
//   - Maps (EnvironmentVariables, Roles, DefaultPayload): merged, child values win on conflict
//   - Slices (Filesystem, CreateDirs, RequiredCredentials, RequiredServices):
//     child appended after parent, deduplicated where applicable
//     (Filesystem by Dest, strings by value)
//   - Pointers (Namespaces, Resources, Security, HealthCheck): child replaces parent if non-nil
//   - Inherits is always cleared (consumed during resolution)
func Merge(parent, child *schema.TemplateContent) schema.TemplateContent {
	result := *parent
	// Inherits is consumed during resolution; never carry it forward.
	result.Inherits = ""

	// Scalars: child wins if non-zero.
	if child.Description != "" {
		result.Description = child.Description
	}
	if len(child.Command) > 0 {
		result.Command = child.Command
	}
	if child.Environment != "" {
		result.Environment = child.Environment
	}

	// Maps: merge with child winning on conflict.
	result.EnvironmentVariables = mergeStringMaps(parent.EnvironmentVariables, child.EnvironmentVariables)
	result.Roles = mergeStringSliceMaps(parent.Roles, child.Roles)
	result.DefaultPayload = MergeAnyMaps(parent.DefaultPayload, child.DefaultPayload)

	// Slices: child appended after parent, deduplicated.
	result.Filesystem = mergeMounts(parent.Filesystem, child.Filesystem)
	result.CreateDirs = mergeStringSlices(parent.CreateDirs, child.CreateDirs)
	result.RequiredCredentials = mergeStringSlices(parent.RequiredCredentials, child.RequiredCredentials)
	result.RequiredServices = mergeStringSlices(parent.RequiredServices, child.RequiredServices)

	// Pointers: child wins if non-nil.
	if child.Namespaces != nil {
		result.Namespaces = child.Namespaces
	}
	if child.Resources != nil {
		result.Resources = child.Resources
	}
	if child.Security != nil {
		result.Security = child.Security
	}
	if child.HealthCheck != nil {
		result.HealthCheck = child.HealthCheck
	}

	return result
}

// mergeStringMaps merges two string→string maps. Child values win on conflict.
// Returns nil if both inputs are empty.
func mergeStringMaps(parent, child map[string]string) map[string]string {
	if len(parent) == 0 && len(child) == 0 {
		return nil
	}
	result := make(map[string]string, len(parent)+len(child))
	for key, value := range parent {
		result[key] = value
	}
	for key, value := range child {
		result[key] = value
	}
	return result
}

// mergeStringSliceMaps merges two map[string][]string maps (used for Roles).
// Child values win on conflict. Returns nil if both inputs are empty.
func mergeStringSliceMaps(parent, child map[string][]string) map[string][]string {
	if len(parent) == 0 && len(child) == 0 {
		return nil
	}
	result := make(map[string][]string, len(parent)+len(child))
	for key, value := range parent {
		result[key] = value
	}
	for key, value := range child {
		result[key] = value
	}
	return result
}

// MergeAnyMaps merges two map[string]any maps (used for DefaultPayload).
// Child values win on conflict. Returns nil if both inputs are empty.
func MergeAnyMaps(parent, child map[string]any) map[string]any {
	if len(parent) == 0 && len(child) == 0 {
		return nil
	}
	result := make(map[string]any, len(parent)+len(child))
	for key, value := range parent {
		result[key] = value
	}
	for key, value := range child {
		result[key] = value
	}
	return result
}

// mergeMounts appends child mounts after parent mounts, deduplicating by
// Dest (child wins when the same destination appears in both).
func mergeMounts(parent, child []schema.TemplateMount) []schema.TemplateMount {
	if len(parent) == 0 {
		return child
	}
	if len(child) == 0 {
		return parent
	}
	// Build set of child destinations for deduplication.
	childDests := make(map[string]bool, len(child))
	for _, mount := range child {
		childDests[mount.Dest] = true
	}
	// Keep parent mounts whose Dest isn't overridden by a child mount.
	var result []schema.TemplateMount
	for _, mount := range parent {
		if !childDests[mount.Dest] {
			result = append(result, mount)
		}
	}
	// Append all child mounts.
	result = append(result, child...)
	return result
}

// mergeStringSlices appends child strings after parent strings, removing
// duplicates. Used for CreateDirs, RequiredCredentials, and RequiredServices.
func mergeStringSlices(parent, child []string) []string {
	if len(parent) == 0 {
		return child
	}
	if len(child) == 0 {
		return parent
	}
	seen := make(map[string]bool, len(parent))
	result := make([]string, len(parent))
	copy(result, parent)
	for _, s := range parent {
		seen[s] = true
	}
	for _, s := range child {
		if !seen[s] {
			result = append(result, s)
			seen[s] = true
		}
	}
	return result
}
