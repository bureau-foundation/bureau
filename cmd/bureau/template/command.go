// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package template implements the bureau template subcommands for managing
// sandbox templates stored as Matrix state events. Templates define the
// sandbox configuration (command, filesystem, namespaces, resources, security,
// environment) and support multi-level inheritance.
//
// All commands that access Matrix use the operator session from
// ~/.config/bureau/session.json (created by "bureau login"). The --server-name
// flag controls how room alias localparts are resolved to full Matrix aliases.
package template

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// Command returns the "template" command group.
func Command() *cli.Command {
	return &cli.Command{
		Name:    "template",
		Summary: "Manage sandbox templates",
		Description: `Manage Bureau sandbox templates stored as Matrix state events.

Templates define the sandbox configuration for agent processes: command to
run, filesystem mounts, namespace isolation, resource limits, security
options, environment variables, and agent payload. Templates support
multi-level inheritance — a child template inherits from a parent and
overrides specific fields.

Template references use the format:

  <room-alias-localpart>:<template-name>

For example: "bureau/templates:base", "iree/templates:amdgpu-developer".

Template files use JSONC (JSON with comments): the same format stored in
Matrix state events, plus // line comments and /* block comments */ for
documentation. Use "bureau template show --raw" to export a template for
editing.

All commands that access Matrix require an operator session. Run
"bureau login" first to authenticate.`,
		Subcommands: []*cli.Command{
			listCommand(),
			showCommand(),
			pushCommand(),
			validateCommand(),
			diffCommand(),
		},
		Examples: []cli.Example{
			{
				Description: "List templates in the built-in templates room",
				Command:     "bureau template list bureau/templates",
			},
			{
				Description: "Show the fully resolved base-networked template (with inheritance)",
				Command:     "bureau template show bureau/templates:base-networked",
			},
			{
				Description: "Show a template without resolving inheritance",
				Command:     "bureau template show --raw bureau/templates:base-networked",
			},
			{
				Description: "Validate a local template file",
				Command:     "bureau template validate my-agent.json",
			},
			{
				Description: "Push a local template to Matrix",
				Command:     "bureau template push bureau/templates:my-agent my-agent.json",
			},
			{
				Description: "Diff a Matrix template against a local file",
				Command:     "bureau template diff bureau/templates:my-agent my-agent.json",
			},
		},
	}
}

// connectOperator loads the operator session and creates an authenticated
// Matrix session. Returns the session and a context with a 30-second timeout.
func connectOperator() (context.Context, context.CancelFunc, *messaging.Session, error) {
	operatorSession, err := cli.LoadSession()
	if err != nil {
		return nil, nil, nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: operatorSession.Homeserver,
		Logger:        logger,
	})
	if err != nil {
		cancel()
		return nil, nil, nil, fmt.Errorf("create matrix client: %w", err)
	}

	session, err := client.SessionFromToken(operatorSession.UserID, operatorSession.AccessToken)
	if err != nil {
		cancel()
		return nil, nil, nil, fmt.Errorf("create session: %w", err)
	}

	return ctx, cancel, session, nil
}

// printTemplateJSON marshals a TemplateContent as indented JSON to stdout.
func printTemplateJSON(template *schema.TemplateContent) error {
	data, err := json.MarshalIndent(template, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	_, err = fmt.Fprintln(os.Stdout, string(data))
	return err
}
