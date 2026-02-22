// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/command"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// pipelineRunParams holds the parameters for the pipeline run command.
// PipelineRef is positional in CLI mode (args[0]) and a named property
// in JSON/MCP mode.
type pipelineRunParams struct {
	cli.JSONOutput
	PipelineRef string   `json:"pipeline_ref" desc:"pipeline reference (e.g. bureau/pipeline:dev-workspace-init)" required:"true"`
	Machine     string   `json:"machine"      flag:"machine"     desc:"fleet-scoped machine localpart (required)" required:"true"`
	Room        string   `json:"room"         flag:"room"        desc:"room ID where the pipeline ticket is created (required)" required:"true"`
	Param       []string `json:"param"        flag:"param"       desc:"key=value parameter passed to the pipeline (repeatable)"`
	Wait        bool     `json:"wait"         flag:"wait"        desc:"wait for the pipeline to complete after acceptance"`
	ServerName  string   `json:"server_name"  flag:"server-name" desc:"Matrix server name for resolving room aliases" default:"bureau.local"`
}

// runResult is the JSON output for pipeline run.
type runResult struct {
	Machine     string       `json:"machine"      desc:"target machine name"`
	PipelineRef string       `json:"pipeline_ref" desc:"pipeline reference"`
	TicketID    ref.TicketID `json:"ticket_id"    desc:"pipeline ticket ID"`
	TicketRoom  ref.RoomID   `json:"ticket_room"  desc:"room where the ticket was created"`
	EventID     ref.EventID  `json:"event_id"     desc:"command event ID"`
	RequestID   string       `json:"request_id"   desc:"unique request identifier"`
}

// runCommand returns the "run" subcommand for executing a pipeline on
// a remote machine. Unlike the old "execute" command, "run" waits for
// the daemon to acknowledge the pipeline (creating the pip- ticket)
// before returning. This gives the operator the ticket ID and room
// needed for follow-up commands (wait, status, cancel).
func runCommand() *cli.Command {
	var params pipelineRunParams

	return &cli.Command{
		Name:    "run",
		Summary: "Run a pipeline on a machine",
		Description: `Send a pipeline.execute command to a machine's daemon and wait for
the accepted acknowledgment. The daemon creates a pip- ticket and
returns the ticket ID and room. The ticket watcher then spawns a
sandboxed executor that runs the pipeline steps.

With --wait, the command watches the pipeline ticket via Matrix /sync
until the pipeline completes, printing step progress. Without --wait
(the default), returns immediately after acceptance.

The --room flag specifies where the pip- ticket is created. The room
must have ticket management enabled with pipeline type allowed.

Parameters are passed through to the pipeline executor as ticket
variables, accessible in pipeline steps via ${NAME} substitution.`,
		Usage: "bureau pipeline run [flags] <pipeline-ref> --machine <machine> --room <room>",
		Examples: []cli.Example{
			{
				Description: "Run the workspace init pipeline",
				Command:     "bureau pipeline run bureau/pipeline:dev-workspace-init --machine bureau/fleet/prod/machine/workstation --room '!project:bureau.local' --param PROJECT=iree --param BRANCH=main",
			},
			{
				Description: "Run a deploy pipeline",
				Command:     "bureau pipeline run iree/pipeline:deploy --machine bureau/fleet/prod/machine/builder --room '!deploys:bureau.local'",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &runResult{} },
		RequiredGrants: []string{"command/pipeline/execute"},
		Annotations:    cli.Create(),
		Run: func(args []string) error {
			// In CLI mode, pipeline ref comes as a positional argument.
			// In JSON/MCP mode, it's populated from the JSON input.
			if len(args) == 1 {
				params.PipelineRef = args[0]
			} else if len(args) > 1 {
				return cli.Validation("usage: bureau pipeline run [flags] <pipeline-ref> --machine <machine> --room <room>")
			}
			if params.PipelineRef == "" {
				return cli.Validation("pipeline reference is required\n\nusage: bureau pipeline run [flags] <pipeline-ref> --machine <machine> --room <room>")
			}
			if params.Machine == "" {
				return cli.Validation("--machine is required")
			}
			if params.Room == "" {
				return cli.Validation("--room is required")
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

			// Validate the room ID.
			ticketRoomID, err := ref.ParseRoomID(params.Room)
			if err != nil {
				return cli.Validation("invalid --room: %w", err)
			}

			// Parse key=value parameters. The pipeline ref and room go
			// into dedicated parameter keys consumed by the daemon. Extra
			// params from --param are merged alongside as ticket variables.
			parameters := make(map[string]any)
			parameters["pipeline"] = pipelineRefString
			parameters["room"] = ticketRoomID.String()
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

			// Send the command and wait for the accepted result. The
			// daemon creates a pip- ticket and returns ticket_id + room.
			result, err := command.Execute(ctx, command.SendParams{
				Session:    session,
				RoomID:     configRoomID,
				Command:    "pipeline.execute",
				Parameters: parameters,
			})
			if err != nil {
				return err
			}
			if err := result.Err(); err != nil {
				return err
			}

			if done, err := params.EmitJSON(runResult{
				Machine:     params.Machine,
				PipelineRef: pipelineRefString,
				TicketID:    result.TicketID,
				TicketRoom:  result.TicketRoom,
				EventID:     result.Event.EventID,
				RequestID:   result.RequestID,
			}); done {
				return err
			}

			fmt.Fprintf(os.Stderr, "Pipeline accepted on %s\n", params.Machine)
			fmt.Fprintf(os.Stderr, "  pipeline:  %s\n", pipelineRefString)

			if !params.Wait || result.TicketID.IsZero() {
				result.WriteAcceptedHint(os.Stderr)
				return nil
			}

			// Watch the ticket until the pipeline finishes. Use a 10-minute
			// context — pipelines can run long (build steps, large git clones).
			watchCtx, watchCancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer watchCancel()

			final, err := command.WatchTicket(watchCtx, command.WatchTicketParams{
				Session:    session,
				RoomID:     result.TicketRoom,
				TicketID:   result.TicketID,
				OnProgress: command.StepProgressWriter(os.Stderr),
			})
			if err != nil {
				return err
			}

			return emitWaitResult(pipelineWaitParams{JSONOutput: params.JSONOutput}, result.TicketID, *final)
		},
	}
}
