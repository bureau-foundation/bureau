// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// contextCommand returns the "context" parent command with list and show
// subcommands.
func contextCommand() *cli.Command {
	return &cli.Command{
		Name:    "context",
		Summary: "Browse agent context entries",
		Description: `Browse the key-value context index for an agent principal.

Context entries are stored as m.bureau.agent_context state events by the
agent service. Each entry maps a key (e.g., "conversation", "summary")
to an artifact ref with metadata (size, content type, modification time).`,
		Subcommands: []*cli.Command{
			contextListCommand(),
			contextShowCommand(),
		},
	}
}

type contextListParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"machine localpart (optional — auto-discovers if omitted)"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
	Prefix     string `json:"prefix"      flag:"prefix"      desc:"filter keys by prefix"`
}

type contextListResult struct {
	Localpart string                         `json:"localpart"`
	Machine   string                         `json:"machine"`
	Entries   map[string]schema.ContextEntry `json:"entries"`
}

func contextListCommand() *cli.Command {
	var params contextListParams

	return &cli.Command{
		Name:    "list",
		Summary: "List context entries for an agent",
		Description: `List all context entries for an agent principal.

Each entry shows its key, artifact ref, content type, size, and
modification time. Use --prefix to filter keys (e.g., --prefix "summary/").`,
		Usage: "bureau agent context list <localpart> [--prefix <prefix>]",
		Examples: []cli.Example{
			{
				Description: "List all context entries",
				Command:     "bureau agent context list agent/code-review --credential-file ./creds",
			},
			{
				Description: "Filter by prefix",
				Command:     "bureau agent context list agent/code-review --credential-file ./creds --prefix summary/",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &contextListResult{} },
		RequiredGrants: []string{"command/agent/context/list"},
		Annotations:    cli.ReadOnly(),
		Run: requireLocalpart("bureau agent context list <localpart> [--prefix <prefix>]", func(localpart string) error {
			return runContextList(localpart, params)
		}),
	}
}

func runContextList(localpart string, params contextListParams) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	location, machineCount, err := principal.Resolve(ctx, session, localpart, params.Machine, params.ServerName)
	if err != nil {
		return cli.NotFound("resolve agent: %w", err)
	}

	if params.Machine == "" && machineCount > 0 {
		fmt.Fprintf(os.Stderr, "resolved %s → %s (scanned %d machines)\n", localpart, location.MachineName, machineCount)
	}

	contextRaw, err := session.GetStateEvent(ctx, location.ConfigRoomID, schema.EventTypeAgentContext, localpart)
	if err != nil {
		return cli.NotFound("no context data for %s: %w", localpart, err)
	}

	var content schema.AgentContextContent
	if err := json.Unmarshal(contextRaw, &content); err != nil {
		return cli.Internal("parse context data: %w", err)
	}

	// Apply prefix filter.
	entries := content.Entries
	if params.Prefix != "" {
		filtered := make(map[string]schema.ContextEntry)
		for key, entry := range entries {
			if strings.HasPrefix(key, params.Prefix) {
				filtered[key] = entry
			}
		}
		entries = filtered
	}

	if done, err := params.EmitJSON(contextListResult{
		Localpart: localpart,
		Machine:   location.MachineName,
		Entries:   entries,
	}); done {
		return err
	}

	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "no context entries found")
		return nil
	}

	// Sort keys for deterministic output.
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "KEY\tTYPE\tSIZE\tMODIFIED\tARTIFACT")
	for _, key := range keys {
		entry := entries[key]
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			key, entry.ContentType,
			formatSize(entry.Size), entry.ModifiedAt,
			truncateRef(entry.ArtifactRef))
	}
	writer.Flush()

	return nil
}

type contextShowParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Machine    string `json:"machine"     flag:"machine"     desc:"machine localpart (optional — auto-discovers if omitted)"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
}

func contextShowCommand() *cli.Command {
	var params contextShowParams

	return &cli.Command{
		Name:    "show",
		Summary: "Show a single context entry",
		Description: `Display detailed metadata for a single context entry.

Shows the full artifact ref, content type, size, modification time,
and additional metadata (session ID, message count, token count) if
available.`,
		Usage: "bureau agent context show <localpart> <key> [--machine <machine>]",
		Examples: []cli.Example{
			{
				Description: "Show the conversation context entry",
				Command:     "bureau agent context show agent/code-review conversation --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &schema.ContextEntry{} },
		RequiredGrants: []string{"command/agent/context/show"},
		Annotations:    cli.ReadOnly(),
		Run: func(args []string) error {
			if len(args) < 2 {
				return cli.Validation("agent localpart and context key are required\n\nUsage: bureau agent context show <localpart> <key> [--machine <machine>]")
			}
			localpart := args[0]
			key := args[1]
			if len(args) > 2 {
				return cli.Validation("unexpected argument: %s", args[2])
			}
			if err := principal.ValidateLocalpart(localpart); err != nil {
				return cli.Validation("invalid agent localpart: %v", err)
			}
			return runContextShow(localpart, key, params)
		},
	}
}

func runContextShow(localpart, key string, params contextShowParams) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	location, machineCount, err := principal.Resolve(ctx, session, localpart, params.Machine, params.ServerName)
	if err != nil {
		return cli.NotFound("resolve agent: %w", err)
	}

	if params.Machine == "" && machineCount > 0 {
		fmt.Fprintf(os.Stderr, "resolved %s → %s (scanned %d machines)\n", localpart, location.MachineName, machineCount)
	}

	contextRaw, err := session.GetStateEvent(ctx, location.ConfigRoomID, schema.EventTypeAgentContext, localpart)
	if err != nil {
		return cli.NotFound("no context data for %s: %w", localpart, err)
	}

	var content schema.AgentContextContent
	if err := json.Unmarshal(contextRaw, &content); err != nil {
		return cli.Internal("parse context data: %w", err)
	}

	entry, exists := content.Entries[key]
	if !exists {
		return cli.NotFound("context key %q not found for %s", key, localpart)
	}

	if done, err := params.EmitJSON(entry); done {
		return err
	}

	fmt.Printf("Agent:        %s\n", principal.MatrixUserID(localpart, params.ServerName))
	fmt.Printf("Key:          %s\n", key)
	fmt.Printf("Artifact:     %s\n", entry.ArtifactRef)
	fmt.Printf("Content type: %s\n", entry.ContentType)
	fmt.Printf("Size:         %s\n", formatSize(entry.Size))
	fmt.Printf("Modified:     %s\n", entry.ModifiedAt)
	if entry.SessionID != "" {
		fmt.Printf("Session:      %s\n", entry.SessionID)
	}
	if entry.MessageCount > 0 {
		fmt.Printf("Messages:     %d\n", entry.MessageCount)
	}
	if entry.TokenCount > 0 {
		fmt.Printf("Tokens:       %s\n", formatTokenCount(entry.TokenCount))
	}

	return nil
}

// formatSize formats a byte count for display.
func formatSize(bytes int64) string {
	if bytes == 0 {
		return "0"
	}
	if bytes >= 1_000_000 {
		return fmt.Sprintf("%.1fMB", float64(bytes)/1_000_000)
	}
	if bytes >= 1_000 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1_000)
	}
	return fmt.Sprintf("%dB", bytes)
}

// truncateRef shortens an artifact ref for tabular display. Shows the
// first 12 characters if the ref is longer than 16 characters.
func truncateRef(ref string) string {
	if len(ref) > 16 {
		return ref[:12] + "..."
	}
	return ref
}
