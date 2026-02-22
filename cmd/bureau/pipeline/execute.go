// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"
	"os"
	"strings"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// pipelineExecuteParams holds the parameters for the pipeline execute command.
// PipelineRef is positional in CLI mode (args[0]) and a named property in
// JSON/MCP mode.
type pipelineExecuteParams struct {
	cli.JSONOutput
	PipelineRef string   `json:"pipeline_ref" desc:"pipeline reference (e.g. bureau/pipeline:dev-workspace-init)" required:"true"`
	Machine     string   `json:"machine"      flag:"machine"     desc:"fleet-scoped machine localpart (e.g., bureau/fleet/prod/machine/workstation) (required)"`
	Param       []string `json:"param"        flag:"param"       desc:"key=value parameter passed to the pipeline (repeatable)"`
	ServerName  string   `json:"server_name"  flag:"server-name" desc:"Matrix server name for resolving room aliases" default:"bureau.local"`
}

// executeResult is the JSON output for pipeline execute.
type executeResult struct {
	Machine      string        `json:"machine"        desc:"target machine name"`
	PipelineRef  string        `json:"pipeline_ref"   desc:"pipeline reference"`
	ConfigRoom   ref.RoomAlias `json:"config_room"    desc:"configuration room alias"`
	ConfigRoomID ref.RoomID    `json:"config_room_id" desc:"configuration room Matrix ID"`
	EventID      ref.EventID   `json:"event_id"       desc:"execution request event ID"`
	RequestID    string        `json:"request_id"     desc:"unique request identifier"`
}

// executeCommand returns the "execute" subcommand for running a pipeline
// on a remote machine.
func executeCommand() *cli.Command {
	var params pipelineExecuteParams

	return &cli.Command{
		Name:    "execute",
		Summary: "Execute a pipeline on a machine",
		Description: `Send a pipeline.execute command to a machine's daemon. The daemon
spawns an ephemeral sandbox running the pipeline executor, which
fetches the pipeline definition from Matrix and runs each step.

The command is asynchronous: the daemon acknowledges immediately
and posts step-by-step results as threaded replies in the config
room. Use "bureau observe" to watch the executor's terminal in
real time.

Parameters are passed through to the pipeline executor as payload
variables, accessible in pipeline steps via ${NAME} substitution.`,
		Usage: "bureau pipeline execute [flags] <pipeline-ref> --machine <machine>",
		Examples: []cli.Example{
			{
				Description: "Run the workspace init pipeline on a machine",
				Command:     "bureau pipeline execute bureau/pipeline:dev-workspace-init --machine bureau/fleet/prod/machine/workstation --param PROJECT=iree --param BRANCH=main",
			},
			{
				Description: "Run a project-specific deploy pipeline",
				Command:     "bureau pipeline execute iree/pipeline:deploy --machine bureau/fleet/prod/machine/builder",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &executeResult{} },
		RequiredGrants: []string{"command/pipeline/execute"},
		Annotations:    cli.Create(),
		Run: func(args []string) error {
			// In CLI mode, pipeline ref comes as a positional argument.
			// In JSON/MCP mode, it's populated from the JSON input.
			if len(args) == 1 {
				params.PipelineRef = args[0]
			} else if len(args) > 1 {
				return cli.Validation("usage: bureau pipeline execute [flags] <pipeline-ref> --machine <machine>")
			}
			if params.PipelineRef == "" {
				return cli.Validation("pipeline reference is required\n\nusage: bureau pipeline execute [flags] <pipeline-ref> --machine <machine>")
			}
			if params.Machine == "" {
				return cli.Validation("--machine is required")
			}

			pipelineRefString := params.PipelineRef

			// Validate the pipeline ref is parseable.
			if _, err := schema.ParsePipelineRef(pipelineRefString); err != nil {
				return cli.Validation("parsing pipeline reference: %w", err)
			}

			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return fmt.Errorf("invalid --server-name: %w", err)
			}

			// Parse and validate the machine localpart as a typed ref.
			machine, err := ref.ParseMachine(params.Machine, serverName)
			if err != nil {
				return cli.Validation("invalid machine name: %w", err)
			}

			// Parse key=value parameters. The pipeline ref goes into
			// parameters["pipeline"] (the daemon extracts this). Additional
			// params from --param are merged alongside it.
			parameters := make(map[string]any)
			parameters["pipeline"] = pipelineRefString
			for _, param := range params.Param {
				key, value, found := strings.Cut(param, "=")
				if !found {
					return cli.Validation("invalid --param %q: expected key=value", param)
				}
				if key == "" {
					return cli.Validation("invalid --param %q: empty key", param)
				}
				parameters[key] = value
			}

			// Generate a request ID for correlating the command with its
			// threaded result replies.
			requestID, err := cli.GenerateRequestID()
			if err != nil {
				return cli.Internal("generating request ID: %w", err)
			}

			// Connect to Matrix.
			ctx, cancel, session, err := cli.ConnectOperator()
			if err != nil {
				return err
			}
			defer cancel()

			// Resolve the config room for the target machine. The daemon
			// monitors this room for m.bureau.command messages.
			configRoomAlias := machine.RoomAlias()
			configRoomID, err := session.ResolveAlias(ctx, configRoomAlias)
			if err != nil {
				return cli.NotFound("resolving config room %s: %w (is the machine registered?)", configRoomAlias, err)
			}

			// Build the command message.
			command := schema.CommandMessage{
				MsgType:    schema.MsgTypeCommand,
				Body:       fmt.Sprintf("pipeline.execute %s on %s", pipelineRefString, params.Machine),
				Command:    "pipeline.execute",
				RequestID:  requestID,
				Parameters: parameters,
			}

			// Send as an m.room.message event.
			eventID, err := session.SendEvent(ctx, configRoomID, "m.room.message", command)
			if err != nil {
				return cli.Internal("sending pipeline.execute command: %w", err)
			}

			if done, err := params.EmitJSON(executeResult{
				Machine:      params.Machine,
				PipelineRef:  pipelineRefString,
				ConfigRoom:   configRoomAlias,
				ConfigRoomID: configRoomID,
				EventID:      eventID,
				RequestID:    requestID,
			}); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "pipeline.execute sent to %s\n", params.Machine)
			fmt.Fprintf(os.Stdout, "  pipeline:    %s\n", pipelineRefString)
			fmt.Fprintf(os.Stdout, "  config room: %s (%s)\n", configRoomAlias, configRoomID)
			fmt.Fprintf(os.Stdout, "  event:       %s\n", eventID)
			fmt.Fprintf(os.Stdout, "  request_id:  %s\n", requestID)
			return nil
		},
	}
}
