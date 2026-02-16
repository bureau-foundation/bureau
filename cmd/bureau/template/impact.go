// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	libtmpl "github.com/bureau-foundation/bureau/lib/template"
	"github.com/bureau-foundation/bureau/messaging"
)

// impactParams holds the parameters for the template impact command. The
// template ref and optional file path are positional in CLI mode.
type impactParams struct {
	cli.JSONOutput
	ServerName string `json:"server_name"  flag:"server-name"  desc:"Matrix server name for resolving room aliases" default:"bureau.local"`
}

// impactCommand returns the "impact" subcommand for analyzing the effect of a
// template change across all machines.
func impactCommand() *cli.Command {
	var params impactParams

	return &cli.Command{
		Name:    "impact",
		Summary: "Show which principals would be affected by a template change",
		Description: `Scan all per-machine config rooms and find every principal that references
the given template — directly or via inheritance. This answers the question
"if I change this template, what happens?"

Without a file argument, shows the affected principals and how they
reference the template (directly or through an inheritance chain).

With a file argument, also classifies each change:
  - metadata: only the description changed (no runtime effect)
  - payload-only: only default_payload changed (hot-reloadable)
  - structural: command, filesystem, namespaces, etc. changed (restart required)`,
		Usage: "bureau template impact [flags] <template-ref> [file]",
		Examples: []cli.Example{
			{
				Description: "Show all principals that use a template (directly or via inheritance)",
				Command:     "bureau template impact bureau/template:base",
			},
			{
				Description: "Classify what would change if you pushed a modified template",
				Command:     "bureau template impact bureau/template:llm-agent llm-agent.json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &[]impactResult{} },
		RequiredGrants: []string{"command/template/impact"},
		Annotations:    cli.ReadOnly(),
		Run: func(args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return cli.Validation("usage: bureau template impact [flags] <template-ref> [file]")
			}

			targetRefString := args[0]
			targetRef, err := schema.ParseTemplateRef(targetRefString)
			if err != nil {
				return cli.Validation("parsing template reference: %w", err)
			}

			// If a local file is provided, read it for change classification.
			var proposedContent *schema.TemplateContent
			if len(args) == 2 {
				proposedContent, err = readTemplateFile(args[1])
				if err != nil {
					return err
				}
			}

			ctx, cancel, session, err := cli.ConnectOperator()
			if err != nil {
				return err
			}
			defer cancel()

			analyzer := &impactAnalyzer{
				session:    session,
				serverName: params.ServerName,
				ctx:        ctx,
				cache:      make(map[string]*schema.TemplateContent),
			}

			// Discover all config rooms and collect MachineConfigs.
			machineConfigs, err := analyzer.discoverMachineConfigs()
			if err != nil {
				return err
			}

			if len(machineConfigs) == 0 {
				fmt.Fprintln(os.Stderr, "no config rooms found (is the operator joined to bureau/config/* rooms?)")
				return nil
			}

			// Find all affected principals.
			targetRefCanonical := targetRef.String()
			affected, err := analyzer.findAffected(targetRefCanonical, machineConfigs)
			if err != nil {
				return err
			}

			// If a file was provided, classify changes for each affected principal.
			if proposedContent != nil {
				if err := analyzer.classifyChanges(affected, targetRefCanonical, proposedContent); err != nil {
					return err
				}
			}

			return printImpactResults(affected, targetRefCanonical, proposedContent != nil, &params.JSONOutput)
		},
	}
}

// impactResult describes one affected principal.
type impactResult struct {
	Machine       string   `json:"machine"                  desc:"affected machine name"`
	Principal     string   `json:"principal"                desc:"affected principal"`
	Template      string   `json:"template"                 desc:"template reference"`
	Depth         int      `json:"depth"                    desc:"inheritance depth"`
	Change        string   `json:"change,omitempty"         desc:"type of change"`
	ChangedFields []string `json:"changed_fields,omitempty" desc:"fields that differ"`
	Overrides     []string `json:"overrides,omitempty"      desc:"override sources"`
}

// impactAnalyzer holds state for the impact analysis.
type impactAnalyzer struct {
	session    *messaging.Session
	serverName string
	ctx        context.Context
	cache      map[string]*schema.TemplateContent
}

// machineConfig pairs a machine name with its parsed config.
type machineConfig struct {
	machine string
	config  schema.MachineConfig
}

// discoverMachineConfigs scans all rooms the operator has joined, identifies
// config rooms by their canonical alias (bureau/config/*), and reads each
// machine's MachineConfig state event.
func (a *impactAnalyzer) discoverMachineConfigs() ([]machineConfig, error) {
	roomIDs, err := a.session.JoinedRooms(a.ctx)
	if err != nil {
		return nil, cli.Internal("listing joined rooms: %w", err)
	}

	var results []machineConfig

	for _, roomID := range roomIDs {
		events, err := a.session.GetRoomState(a.ctx, roomID)
		if err != nil {
			continue // Skip rooms we can't read.
		}

		// Find the canonical alias to check if this is a config room.
		var canonicalAlias string
		for _, event := range events {
			if event.Type == "m.room.canonical_alias" {
				if alias, ok := event.Content["alias"].(string); ok {
					canonicalAlias = alias
				}
				break
			}
		}

		if canonicalAlias == "" {
			continue
		}

		localpart := principal.RoomAliasLocalpart(canonicalAlias)
		if !strings.HasPrefix(localpart, "bureau/config/") {
			continue
		}

		machineName := strings.TrimPrefix(localpart, "bureau/config/")

		// Find the MachineConfig state event.
		for _, event := range events {
			if event.Type != schema.EventTypeMachineConfig {
				continue
			}

			// Re-marshal the Content map to JSON and unmarshal into MachineConfig.
			contentJSON, err := json.Marshal(event.Content)
			if err != nil {
				continue
			}
			var config schema.MachineConfig
			if err := json.Unmarshal(contentJSON, &config); err != nil {
				continue
			}

			results = append(results, machineConfig{
				machine: machineName,
				config:  config,
			})
			break
		}
	}

	return results, nil
}

// findAffected finds all principals across all machines whose template
// references the target (directly or via inheritance).
func (a *impactAnalyzer) findAffected(targetRef string, configs []machineConfig) ([]*impactResult, error) {
	var results []*impactResult

	// Track which template refs we've already checked to avoid redundant
	// inheritance chain walks.
	chainCache := make(map[string]struct {
		depends bool
		depth   int
	})

	for _, mc := range configs {
		for _, assignment := range mc.config.Principals {
			if assignment.Template == "" {
				continue
			}

			depends, depth, err := a.templateDependsOn(assignment.Template, targetRef, chainCache)
			if err != nil {
				// Log but don't fail — one broken template shouldn't
				// prevent analyzing the rest.
				fmt.Fprintf(os.Stderr, "warning: cannot walk inheritance for %q on %s: %v\n",
					assignment.Template, mc.machine, err)
				continue
			}

			if !depends {
				continue
			}

			result := &impactResult{
				Machine:   mc.machine,
				Principal: assignment.Localpart,
				Template:  assignment.Template,
				Depth:     depth,
			}

			// Annotate instance overrides that may mask template changes.
			if len(assignment.CommandOverride) > 0 {
				result.Overrides = append(result.Overrides, "command_override")
			}
			if assignment.EnvironmentOverride != "" {
				result.Overrides = append(result.Overrides, "environment_override")
			}
			if len(assignment.ExtraEnvironmentVariables) > 0 {
				result.Overrides = append(result.Overrides, "extra_environment_variables")
			}
			if len(assignment.Payload) > 0 {
				result.Overrides = append(result.Overrides, "payload")
			}

			results = append(results, result)
		}
	}

	return results, nil
}

// templateDependsOn checks whether templateRef's inheritance tree contains
// targetRef. Returns true and the minimum depth (0 = direct match) if found.
// Uses chainCache to avoid redundant walks across the multi-parent tree.
func (a *impactAnalyzer) templateDependsOn(
	templateRef, targetRef string,
	chainCache map[string]struct {
		depends bool
		depth   int
	},
) (bool, int, error) {
	if cached, ok := chainCache[templateRef]; ok {
		return cached.depends, cached.depth, nil
	}

	depends, depth, err := a.templateDependsOnRecurse(templateRef, targetRef, chainCache, make(map[string]bool))
	if err != nil {
		return false, 0, err
	}

	chainCache[templateRef] = struct {
		depends bool
		depth   int
	}{depends, depth}
	return depends, depth, nil
}

// templateDependsOnRecurse is the recursive implementation of
// templateDependsOn. The visited map tracks the current stack for
// cycle detection.
func (a *impactAnalyzer) templateDependsOnRecurse(
	templateRef, targetRef string,
	chainCache map[string]struct {
		depends bool
		depth   int
	},
	visited map[string]bool,
) (bool, int, error) {
	if templateRef == targetRef {
		return true, 0, nil
	}

	if cached, ok := chainCache[templateRef]; ok {
		if cached.depends {
			return true, cached.depth + 1, nil
		}
		return false, 0, nil
	}

	if visited[templateRef] {
		return false, 0, nil // Cycle — doesn't reach target.
	}
	visited[templateRef] = true
	defer delete(visited, templateRef)

	ref, err := schema.ParseTemplateRef(templateRef)
	if err != nil {
		return false, 0, cli.Validation("parsing %q: %w", templateRef, err)
	}

	template, err := a.fetchCached(ref)
	if err != nil {
		return false, 0, err
	}

	// Recurse into each parent. If any parent's tree contains the
	// target, this template depends on it. Track the minimum depth.
	for index, parentRefString := range template.Inherits {
		if _, err := schema.ParseTemplateRef(parentRefString); err != nil {
			return false, 0, cli.Validation("parsing inherits[%d] %q in %q: %w", index, parentRefString, templateRef, err)
		}

		depends, depth, err := a.templateDependsOnRecurse(parentRefString, targetRef, chainCache, visited)
		if err != nil {
			return false, 0, err
		}
		if depends {
			return true, depth + 1, nil
		}
	}

	return false, 0, nil
}

// fetchCached fetches a template from Matrix, using the cache to avoid
// redundant requests.
func (a *impactAnalyzer) fetchCached(ref schema.TemplateRef) (*schema.TemplateContent, error) {
	key := ref.String()
	if cached, ok := a.cache[key]; ok {
		return cached, nil
	}

	template, err := libtmpl.Fetch(a.ctx, a.session, ref, a.serverName)
	if err != nil {
		return nil, err
	}

	a.cache[key] = template
	return template, nil
}

// classifyChanges resolves the current and proposed template for each affected
// principal and classifies the difference.
func (a *impactAnalyzer) classifyChanges(results []*impactResult, targetRef string, proposed *schema.TemplateContent) error {
	for _, result := range results {
		current, err := a.resolveWithOverride(result.Template, targetRef, nil)
		if err != nil {
			result.Change = fmt.Sprintf("error: %v", err)
			continue
		}
		modified, err := a.resolveWithOverride(result.Template, targetRef, proposed)
		if err != nil {
			result.Change = fmt.Sprintf("error: %v", err)
			continue
		}

		result.Change, result.ChangedFields = classifyTemplateChange(current, modified)
	}

	return nil
}

// resolveWithOverride resolves a template's multi-parent inheritance tree,
// substituting overrideContent for the template at overrideRef. If
// overrideContent is nil, all templates come from Matrix (normal resolution).
func (a *impactAnalyzer) resolveWithOverride(templateRef, overrideRef string, overrideContent *schema.TemplateContent) (*schema.TemplateContent, error) {
	cache := make(map[string]*schema.TemplateContent)
	stack := make(map[string]bool)
	return a.resolveWithOverrideRecurse(templateRef, overrideRef, overrideContent, cache, stack)
}

func (a *impactAnalyzer) resolveWithOverrideRecurse(templateRef, overrideRef string, overrideContent *schema.TemplateContent, cache map[string]*schema.TemplateContent, stack map[string]bool) (*schema.TemplateContent, error) {
	if cached, ok := cache[templateRef]; ok {
		return cached, nil
	}

	if stack[templateRef] {
		return nil, cli.Validation("inheritance cycle at %q", templateRef)
	}
	stack[templateRef] = true
	defer delete(stack, templateRef)

	ref, err := schema.ParseTemplateRef(templateRef)
	if err != nil {
		return nil, cli.Validation("parsing %q: %w", templateRef, err)
	}

	var template *schema.TemplateContent
	if overrideContent != nil && templateRef == overrideRef {
		template = overrideContent
	} else {
		template, err = a.fetchCached(ref)
		if err != nil {
			return nil, err
		}
	}

	// If no parents, this template is self-contained.
	if len(template.Inherits) == 0 {
		result := *template
		result.Inherits = nil
		cache[templateRef] = &result
		return &result, nil
	}

	// Resolve each parent independently.
	resolvedParents := make([]*schema.TemplateContent, 0, len(template.Inherits))
	for index, parentRefString := range template.Inherits {
		if _, err := schema.ParseTemplateRef(parentRefString); err != nil {
			return nil, cli.Validation("parsing inherits[%d] %q: %w", index, parentRefString, err)
		}

		resolvedParent, err := a.resolveWithOverrideRecurse(parentRefString, overrideRef, overrideContent, cache, stack)
		if err != nil {
			return nil, err
		}
		resolvedParents = append(resolvedParents, resolvedParent)
	}

	// Merge resolved parents left-to-right, then child on top.
	merged := *resolvedParents[0]
	for i := 1; i < len(resolvedParents); i++ {
		merged = libtmpl.Merge(&merged, resolvedParents[i])
	}

	result := libtmpl.Merge(&merged, template)
	cache[templateRef] = &result
	return &result, nil
}

// classifyTemplateChange compares two resolved TemplateContents and returns
// the change classification and list of changed field names.
func classifyTemplateChange(current, proposed *schema.TemplateContent) (string, []string) {
	var changedFields []string

	if current.Description != proposed.Description {
		changedFields = append(changedFields, "description")
	}
	if !reflect.DeepEqual(current.Command, proposed.Command) {
		changedFields = append(changedFields, "command")
	}
	if current.Environment != proposed.Environment {
		changedFields = append(changedFields, "environment")
	}
	if !reflect.DeepEqual(current.EnvironmentVariables, proposed.EnvironmentVariables) {
		changedFields = append(changedFields, "environment_variables")
	}
	if !reflect.DeepEqual(current.Filesystem, proposed.Filesystem) {
		changedFields = append(changedFields, "filesystem")
	}
	if !reflect.DeepEqual(current.Namespaces, proposed.Namespaces) {
		changedFields = append(changedFields, "namespaces")
	}
	if !reflect.DeepEqual(current.Resources, proposed.Resources) {
		changedFields = append(changedFields, "resources")
	}
	if !reflect.DeepEqual(current.Security, proposed.Security) {
		changedFields = append(changedFields, "security")
	}
	if !reflect.DeepEqual(current.CreateDirs, proposed.CreateDirs) {
		changedFields = append(changedFields, "create_dirs")
	}
	if !reflect.DeepEqual(current.Roles, proposed.Roles) {
		changedFields = append(changedFields, "roles")
	}
	if !reflect.DeepEqual(current.RequiredCredentials, proposed.RequiredCredentials) {
		changedFields = append(changedFields, "required_credentials")
	}
	if !reflect.DeepEqual(current.DefaultPayload, proposed.DefaultPayload) {
		changedFields = append(changedFields, "default_payload")
	}

	if len(changedFields) == 0 {
		return "no change", nil
	}

	// Classify: structural beats payload-only beats metadata.
	hasPayload := false
	hasStructural := false
	for _, field := range changedFields {
		switch field {
		case "description":
			// Metadata — no runtime effect.
		case "default_payload":
			hasPayload = true
		default:
			hasStructural = true
		}
	}

	if hasStructural {
		return "structural", changedFields
	}
	if hasPayload {
		return "payload-only", changedFields
	}
	return "metadata", changedFields
}

// printImpactResults formats and prints the impact analysis.
func printImpactResults(results []*impactResult, targetRef string, hasFile bool, jsonOutput *cli.JSONOutput) error {
	if done, err := jsonOutput.EmitJSON(results); done {
		return err
	}

	if len(results) == 0 {
		fmt.Fprintf(os.Stderr, "no principals reference %s\n", targetRef)
		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)

	if hasFile {
		fmt.Fprintf(writer, "MACHINE\tPRINCIPAL\tDEPTH\tCHANGE\tNOTES\n")
		for _, result := range results {
			change := result.Change
			if len(result.ChangedFields) > 0 && result.Change != "no change" {
				change = fmt.Sprintf("%s (%s)", result.Change, strings.Join(result.ChangedFields, ", "))
			}
			notes := strings.Join(result.Overrides, ", ")
			if notes != "" {
				notes = "has " + notes
			}
			fmt.Fprintf(writer, "%s\t%s\t%d\t%s\t%s\n",
				result.Machine, result.Principal, result.Depth, change, notes)
		}
	} else {
		fmt.Fprintf(writer, "MACHINE\tPRINCIPAL\tTEMPLATE\tDEPTH\tNOTES\n")
		for _, result := range results {
			notes := strings.Join(result.Overrides, ", ")
			if notes != "" {
				notes = "has " + notes
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\n",
				result.Machine, result.Principal, result.Template, result.Depth, notes)
		}
	}

	if err := writer.Flush(); err != nil {
		return err
	}

	// Summary.
	machineSet := make(map[string]bool)
	for _, result := range results {
		machineSet[result.Machine] = true
	}
	fmt.Fprintf(os.Stdout, "\n%d principal(s) on %d machine(s) affected\n", len(results), len(machineSet))

	if hasFile {
		counts := make(map[string]int)
		for _, result := range results {
			counts[result.Change]++
		}
		if count := counts["structural"]; count > 0 {
			fmt.Fprintf(os.Stdout, "  %d structural (restart required)\n", count)
		}
		if count := counts["payload-only"]; count > 0 {
			fmt.Fprintf(os.Stdout, "  %d payload-only (hot-reload)\n", count)
		}
		if count := counts["metadata"]; count > 0 {
			fmt.Fprintf(os.Stdout, "  %d metadata (no runtime effect)\n", count)
		}
		if count := counts["no change"]; count > 0 {
			fmt.Fprintf(os.Stdout, "  %d no change\n", count)
		}
	}

	return nil
}
