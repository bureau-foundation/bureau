// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/ref"
)

// composeParams holds the parameters for the environment compose command.
// Profile is positional in CLI mode (args[0]) and a named property in
// JSON/MCP mode.
type composeParams struct {
	cli.FleetScope
	cli.JSONOutput
	Profile  string        `json:"profile"  desc:"Nix profile name to build and distribute" required:"true"`
	FlakeRef string        `json:"flake_ref" flag:"flake"    desc:"flake reference for the environment repo" default:"github:bureau-foundation/environment"`
	System   string        `json:"system"   flag:"system"   desc:"Nix system triple (e.g., x86_64-linux) — defaults from fleet cache config"`
	Template string        `json:"template" flag:"template" desc:"template reference for the compose sandbox — defaults from fleet cache config"`
	Wait     bool          `json:"wait"     flag:"wait"     desc:"wait for the pipeline to complete after acceptance"`
	Timeout  time.Duration `json:"timeout"  flag:"timeout"  desc:"maximum time to wait when --wait is set (0 means no limit)" default:"0"`

	// clock is injected by tests for deterministic timeout control.
	// Nil in production; defaults to clock.Real().
	clock clock.Clock
}

// composeResult is the JSON output for environment compose and update.
type composeResult struct {
	Profile     string       `json:"profile"                desc:"Nix profile name"`
	Machine     string       `json:"machine"                desc:"builder machine localpart"`
	PipelineRef string       `json:"pipeline_ref"           desc:"pipeline reference"`
	TicketID    ref.TicketID `json:"ticket_id"              desc:"pipeline ticket ID"`
	TicketRoom  ref.RoomID   `json:"ticket_room"            desc:"room where the ticket was created"`
	Conclusion  string       `json:"conclusion,omitempty"   desc:"pipeline conclusion (when --wait is used)"`
}

func composeCommand() *cli.Command {
	var params composeParams

	return &cli.Command{
		Name:    "compose",
		Summary: "Build and distribute an environment profile via pipeline",
		Description: `Trigger a sandboxed pipeline that builds a Nix environment profile,
pushes the closure to the fleet binary cache, and publishes provenance
metadata as an m.bureau.environment_build state event.

Unlike "bureau environment build" (which builds locally), compose runs
the build on a remote machine through the pipeline executor. The build
sandbox inherits the nix-builder template, which provides Nix daemon
access and cache push credentials (ATTIC_TOKEN).

The --machine flag specifies which machine runs the build. That machine
must have a running daemon with pipeline execution enabled.

Default values for --system and --template are read from the fleet cache
configuration (m.bureau.fleet_cache state event). Configure these with
"bureau fleet cache" to avoid specifying them on every compose invocation.

The pipeline reference is derived from the fleet's namespace:
  <namespace>/pipeline:environment-compose

With --wait, the command watches the pipeline ticket until the build
completes, printing step progress. Without --wait (the default), returns
immediately after the daemon accepts the pipeline.`,
		Usage: "bureau environment compose <profile> --machine <machine> [flags]",
		Examples: []cli.Example{
			{
				Description: "Compose the workstation profile on a builder machine",
				Command:     "bureau environment compose workstation --machine bureau/fleet/prod/machine/builder",
			},
			{
				Description: "Compose with a custom flake and wait for completion",
				Command:     "bureau environment compose workstation --machine bureau/fleet/prod/machine/builder --flake github:myorg/environment --wait",
			},
			{
				Description: "Compose for a specific architecture",
				Command:     "bureau environment compose workstation --machine bureau/fleet/prod/machine/builder --system aarch64-linux",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &composeResult{} },
		RequiredGrants: []string{"command/pipeline/execute"},
		Annotations:    cli.Create(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			// In CLI mode, profile comes as a positional argument.
			if len(args) == 1 {
				params.Profile = args[0]
			} else if len(args) > 1 {
				return cli.Validation("usage: bureau environment compose <profile> --machine <machine>")
			}
			if params.Profile == "" {
				return cli.Validation("profile is required\n\nusage: bureau environment compose <profile> --machine <machine>")
			}

			// Resolve fleet scope. Machine is required for compose
			// because the pipeline runs on a specific machine.
			scope, err := params.FleetScope.Resolve()
			if err != nil {
				return err
			}
			if scope.Machine.IsZero() {
				return cli.Validation("--machine is required for compose (specifies which machine runs the build)")
			}

			session, err := cli.ConnectOperator()
			if err != nil {
				return err
			}
			defer session.Close()

			config, err := resolveFleetConfig(ctx, session, scope, params.System, params.Template)
			if err != nil {
				return err
			}

			output, err := submitComposePipeline(ctx, submitComposeParams{
				Session:     session,
				Scope:       scope,
				FleetConfig: config,
				Profile:     params.Profile,
				FlakeRef:    params.FlakeRef,
				Logger:      logger,
			})
			if err != nil {
				return err
			}

			if done, emitErr := params.EmitJSON(output); done {
				return emitErr
			}

			if !params.Wait {
				writeAcceptedHint(os.Stderr, output)
				return nil
			}

			conclusion, err := watchComposePipeline(ctx, watchComposeParams{
				Session: session,
				Result:  output,
				Timeout: params.Timeout,
				Clock:   params.clock,
				Logger:  logger,
			})
			if err != nil {
				return err
			}

			if conclusion != "success" {
				return &cli.ExitError{Code: 1}
			}
			return nil
		},
	}
}
