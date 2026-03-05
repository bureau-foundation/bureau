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
	"github.com/bureau-foundation/bureau/lib/command"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
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

// composeResult is the JSON output for environment compose.
type composeResult struct {
	Profile     string       `json:"profile"      desc:"Nix profile name"`
	Machine     string       `json:"machine"      desc:"builder machine localpart"`
	PipelineRef string       `json:"pipeline_ref" desc:"pipeline reference"`
	TicketID    ref.TicketID `json:"ticket_id"    desc:"pipeline ticket ID"`
	TicketRoom  ref.RoomID   `json:"ticket_room"  desc:"room where the ticket was created"`
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

			// Connect to Matrix.
			session, err := cli.ConnectOperator()
			if err != nil {
				return err
			}
			defer session.Close()

			// Resolve the fleet room to read cache config.
			fleetRoomAlias := scope.Fleet.RoomAlias()
			fleetRoomID, err := session.ResolveAlias(ctx, fleetRoomAlias)
			if err != nil {
				if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
					return cli.NotFound("fleet room %s not found", fleetRoomAlias).
						WithHint("Has the fleet been created? Run 'bureau fleet create' first.")
				}
				return cli.Transient("resolving fleet room %s: %w", fleetRoomAlias, err)
			}

			// Read fleet cache config for defaults.
			cacheConfig, err := messaging.GetState[schema.FleetCacheContent](
				ctx, session, fleetRoomID, schema.EventTypeFleetCache, "",
			)
			if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				return cli.Transient("reading fleet cache config: %w", err)
			}

			if cacheConfig.Name == "" {
				return cli.Validation("fleet cache has no Attic cache name configured").
					WithHint("Run: bureau fleet cache " + scope.Fleet.Localpart() + " --name <cache-name> --url <url> --public-key <key>")
			}

			// Apply defaults from fleet cache config.
			system := params.System
			if system == "" {
				system = cacheConfig.DefaultSystem
			}
			if system == "" {
				return cli.Validation("--system is required (no default configured in fleet cache)").
					WithHint("Set a default: bureau fleet cache " + scope.Fleet.Localpart() + " --default-system x86_64-linux")
			}

			template := params.Template
			if template == "" {
				template = cacheConfig.ComposeTemplate
			}
			if template == "" {
				return cli.Validation("--template is required (no default configured in fleet cache)").
					WithHint("Set a default: bureau fleet cache " + scope.Fleet.Localpart() + " --compose-template bureau/template:nix-builder")
			}

			// Derive the pipeline reference from the fleet's namespace.
			namespace := scope.Fleet.Namespace()
			pipelineRef := namespace.PipelineRoomAliasLocalpart() + ":environment-compose"

			// Validate the pipeline ref is parseable.
			if _, parseErr := schema.ParsePipelineRef(pipelineRef); parseErr != nil {
				return cli.Internal("derived pipeline reference %q is invalid: %w", pipelineRef, parseErr)
			}

			// Resolve the machine's config room. The daemon monitors
			// this room for m.bureau.command messages.
			configRoomAlias := scope.Machine.RoomAlias()
			configRoomID, err := session.ResolveAlias(ctx, configRoomAlias)
			if err != nil {
				return cli.NotFound("resolving machine config room %s: %w (is the machine registered?)", configRoomAlias, err)
			}

			// Build pipeline parameters. These become ticket variables
			// that the pipeline executor substitutes into step templates.
			// MACHINE is injected by the daemon, not the CLI.
			parameters := map[string]any{
				"pipeline":      pipelineRef,
				"room":          fleetRoomID.String(),
				"template":      template,
				"FLAKE_REF":     params.FlakeRef,
				"PROFILE":       params.Profile,
				"SYSTEM":        system,
				"CACHE_NAME":    cacheConfig.Name,
				"FLEET_ROOM_ID": fleetRoomID.String(),
			}

			logger.Info("submitting environment compose pipeline",
				"profile", params.Profile,
				"machine", scope.Machine.Localpart(),
				"pipeline", pipelineRef,
				"flake", params.FlakeRef,
				"system", system,
			)

			// Send the command and wait for the accepted result.
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

			output := composeResult{
				Profile:     params.Profile,
				Machine:     scope.Machine.Localpart(),
				PipelineRef: pipelineRef,
				TicketID:    result.TicketID,
				TicketRoom:  result.TicketRoom,
			}

			if done, emitErr := params.EmitJSON(output); done {
				return emitErr
			}

			logger.Info("pipeline accepted",
				"profile", params.Profile,
				"machine", scope.Machine.Localpart(),
				"ticket_id", result.TicketID,
			)

			if !params.Wait || result.TicketID.IsZero() {
				result.WriteAcceptedHint(os.Stderr)
				return nil
			}

			// Environment builds can be slow (Nix evaluation + build +
			// cache push).
			watchCtx := ctx
			if params.Timeout > 0 {
				clk := params.clock
				if clk == nil {
					clk = clock.Real()
				}
				var watchCancel context.CancelFunc
				watchCtx, watchCancel = context.WithCancel(ctx)
				defer watchCancel()
				timer := clk.AfterFunc(params.Timeout, watchCancel)
				defer timer.Stop()
			}

			final, watchErr := command.WatchTicket(watchCtx, command.WatchTicketParams{
				Session:    session,
				RoomID:     result.TicketRoom,
				TicketID:   result.TicketID,
				OnProgress: command.StepProgressWriter(os.Stderr),
			})
			if watchErr != nil {
				return watchErr
			}

			conclusion := ""
			if final.Pipeline != nil {
				conclusion = string(final.Pipeline.Conclusion)
			}

			logger.Info("compose pipeline completed",
				"profile", params.Profile,
				"conclusion", conclusion,
			)

			if conclusion != "success" {
				return &cli.ExitError{Code: 1}
			}
			return nil
		},
	}
}
