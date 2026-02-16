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
				return fmt.Errorf("usage: bureau template impact [flags] <template-ref> [file]")
			}

			targetRefString := args[0]
			targetRef, err := schema.ParseTemplateRef(targetRefString)
			if err != nil {
				return fmt.Errorf("parsing template reference: %w", err)
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
		return nil, fmt.Errorf("listing joined rooms: %w", err)
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

// templateDependsOn checks whether templateRef's inheritance chain contains
// targetRef. Returns true and the depth (0 = direct match) if found.
// Uses chainCache to avoid redundant walks.
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

	ref, err := schema.ParseTemplateRef(templateRef)
	if err != nil {
		return false, 0, fmt.Errorf("parsing %q: %w", templateRef, err)
	}

	depth := 0
	current := ref
	visited := make(map[string]bool)

	for {
		currentString := current.String()

		if currentString == targetRef {
			chainCache[templateRef] = struct {
				depends bool
				depth   int
			}{true, depth}
			return true, depth, nil
		}

		if visited[currentString] {
			break // Cycle — doesn't reach target.
		}
		visited[currentString] = true

		template, err := a.fetchCached(current)
		if err != nil {
			return false, 0, err
		}

		if template.Inherits == "" {
			break // Root of chain — target not found.
		}

		parent, err := schema.ParseTemplateRef(template.Inherits)
		if err != nil {
			return false, 0, fmt.Errorf("parsing inherits %q in %q: %w", template.Inherits, currentString, err)
		}
		current = parent
		depth++
	}

	chainCache[templateRef] = struct {
		depends bool
		depth   int
	}{false, 0}
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

// resolveWithOverride resolves a template's inheritance chain, substituting
// overrideContent for the template at overrideRef. If overrideContent is nil,
// all templates come from Matrix (normal resolution).
func (a *impactAnalyzer) resolveWithOverride(templateRef, overrideRef string, overrideContent *schema.TemplateContent) (*schema.TemplateContent, error) {
	ref, err := schema.ParseTemplateRef(templateRef)
	if err != nil {
		return nil, fmt.Errorf("parsing %q: %w", templateRef, err)
	}

	var chain []schema.TemplateContent
	visited := make(map[string]bool)
	current := ref

	for {
		currentString := current.String()
		if visited[currentString] {
			return nil, fmt.Errorf("inheritance cycle at %q", currentString)
		}
		visited[currentString] = true

		var template *schema.TemplateContent
		if overrideContent != nil && currentString == overrideRef {
			template = overrideContent
		} else {
			template, err = a.fetchCached(current)
			if err != nil {
				return nil, err
			}
		}

		chain = append(chain, *template)

		if template.Inherits == "" {
			break
		}

		parent, err := schema.ParseTemplateRef(template.Inherits)
		if err != nil {
			return nil, fmt.Errorf("parsing inherits %q: %w", template.Inherits, err)
		}
		current = parent
	}

	// Merge from base (last) to leaf (first).
	result := chain[len(chain)-1]
	for i := len(chain) - 2; i >= 0; i-- {
		result = libtmpl.Merge(&result, &chain[i])
	}

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
