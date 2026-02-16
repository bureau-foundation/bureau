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

// Resolve fetches a template from Matrix and recursively resolves its
// multi-parent inheritance tree to produce a fully-merged TemplateContent.
// The returned content has all inherited fields merged with the Inherits
// field cleared.
//
// Parents are resolved independently and merged left-to-right: later
// parents override earlier parents for conflicting scalars and map keys.
// The child template is applied last (overrides everything).
//
// Cycle detection prevents infinite loops: if the same template reference
// appears on the current resolution stack, an error is returned. Diamond
// inheritance (two parents sharing a common ancestor) is valid and handled
// efficiently via a resolution cache.
func Resolve(ctx context.Context, session *messaging.Session, templateRef string, serverName string) (*schema.TemplateContent, error) {
	ref, err := schema.ParseTemplateRef(templateRef)
	if err != nil {
		return nil, fmt.Errorf("parsing template reference %q: %w", templateRef, err)
	}

	cache := make(map[string]*schema.TemplateContent)
	stack := make(map[string]bool)
	return resolve(ctx, session, ref, serverName, cache, stack)
}

// resolve is the recursive implementation of Resolve. It fetches a single
// template, resolves each of its parents recursively, merges the resolved
// parents left-to-right, then merges the child on top.
//
// cache stores already-resolved templates so diamond inheritance doesn't
// re-fetch or re-resolve the same template. stack tracks the current
// resolution path for cycle detection: a ref appearing in stack means
// we are already resolving it (a cycle).
func resolve(ctx context.Context, session *messaging.Session, ref schema.TemplateRef, serverName string, cache map[string]*schema.TemplateContent, stack map[string]bool) (*schema.TemplateContent, error) {
	refString := ref.String()

	// Check resolution cache first (handles diamond inheritance).
	if cached, ok := cache[refString]; ok {
		return cached, nil
	}

	// Check for cycles on the current resolution stack.
	if stack[refString] {
		return nil, fmt.Errorf("template inheritance cycle: %q appears twice in resolution stack", refString)
	}
	stack[refString] = true
	defer delete(stack, refString)

	template, err := Fetch(ctx, session, ref, serverName)
	if err != nil {
		return nil, err
	}

	// If no parents, this template is self-contained.
	if len(template.Inherits) == 0 {
		result := *template
		result.Inherits = nil
		cache[refString] = &result
		return &result, nil
	}

	// Resolve each parent independently.
	resolvedParents := make([]*schema.TemplateContent, 0, len(template.Inherits))
	for index, parentRefString := range template.Inherits {
		parentRef, err := schema.ParseTemplateRef(parentRefString)
		if err != nil {
			return nil, fmt.Errorf("parsing inherits[%d] reference %q in template %q: %w", index, parentRefString, refString, err)
		}

		resolvedParent, err := resolve(ctx, session, parentRef, serverName, cache, stack)
		if err != nil {
			return nil, fmt.Errorf("resolving parent %q of template %q: %w", parentRefString, refString, err)
		}
		resolvedParents = append(resolvedParents, resolvedParent)
	}

	// Merge resolved parents left-to-right: later parents override earlier.
	merged := *resolvedParents[0]
	for i := 1; i < len(resolvedParents); i++ {
		merged = Merge(&merged, resolvedParents[i])
	}

	// Merge child template on top (child overrides everything).
	result := Merge(&merged, template)
	cache[refString] = &result
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
	result.Inherits = nil

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
