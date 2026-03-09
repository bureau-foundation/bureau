// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"context"
	"fmt"
	"io"
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

// fleetConfig holds resolved fleet cache configuration for environment
// pipeline dispatch. Produced by resolveFleetConfig.
type fleetConfig struct {
	// FleetRoomID is the resolved room ID for the fleet room.
	FleetRoomID ref.RoomID

	// CacheConfig holds the fleet binary cache settings.
	CacheConfig schema.FleetCacheContent

	// System is the resolved Nix system triple.
	System string

	// Template is the resolved template reference for the build sandbox.
	Template string
}

// resolveFleetConfig resolves the fleet room, reads the cache
// configuration, and applies defaults for system and template from the
// cache config when the corresponding flag is empty.
func resolveFleetConfig(ctx context.Context, session messaging.Session, scope *cli.ResolvedFleetScope, system, template string) (*fleetConfig, error) {
	fleetRoomAlias := scope.Fleet.RoomAlias()
	fleetRoomID, err := session.ResolveAlias(ctx, fleetRoomAlias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return nil, cli.NotFound("fleet room %s not found", fleetRoomAlias).
				WithHint("Has the fleet been created? Run 'bureau fleet create' first.")
		}
		return nil, cli.Transient("resolving fleet room %s: %w", fleetRoomAlias, err)
	}

	cacheConfig, err := messaging.GetState[schema.FleetCacheContent](
		ctx, session, fleetRoomID, schema.EventTypeFleetCache, "",
	)
	if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return nil, cli.Transient("reading fleet cache config: %w", err)
	}

	if cacheConfig.Name == "" {
		return nil, cli.Validation("fleet cache has no Attic cache name configured").
			WithHint("Run: bureau fleet cache " + scope.Fleet.Localpart() + " --name <cache-name> --url <url> --public-key <key>")
	}

	// Apply defaults from fleet cache config.
	resolvedSystem := system
	if resolvedSystem == "" {
		resolvedSystem = cacheConfig.DefaultSystem
	}
	if resolvedSystem == "" {
		return nil, cli.Validation("--system is required (no default configured in fleet cache)").
			WithHint("Set a default: bureau fleet cache " + scope.Fleet.Localpart() + " --default-system x86_64-linux")
	}

	resolvedTemplate := template
	if resolvedTemplate == "" {
		resolvedTemplate = cacheConfig.ComposeTemplate
	}
	if resolvedTemplate == "" {
		return nil, cli.Validation("--template is required (no default configured in fleet cache)").
			WithHint("Set a default: bureau fleet cache " + scope.Fleet.Localpart() + " --compose-template bureau/template:nix-builder")
	}

	return &fleetConfig{
		FleetRoomID: fleetRoomID,
		CacheConfig: cacheConfig,
		System:      resolvedSystem,
		Template:    resolvedTemplate,
	}, nil
}

// submitComposeParams holds parameters for submitting an environment
// compose pipeline.
type submitComposeParams struct {
	Session     messaging.Session
	Scope       *cli.ResolvedFleetScope
	FleetConfig *fleetConfig
	Profile     string
	FlakeRef    string
	Logger      *slog.Logger
}

// submitComposePipeline validates and publishes flake definitions
// (templates and pipelines), then submits the environment compose
// pipeline to the target machine. Returns the pipeline result
// immediately after the daemon accepts it — does not wait for
// completion.
func submitComposePipeline(ctx context.Context, params submitComposeParams) (*composeResult, error) {
	namespace := params.Scope.Fleet.Namespace()

	// Discover and validate any templates and pipelines declared
	// by the flake. Both are validated before either is published,
	// so a validation failure in pipelines cannot leave templates
	// in a partially-published state.
	templates, err := ValidateFlakeTemplates(ctx, params.FlakeRef, params.FleetConfig.System, namespace, params.Logger)
	if err != nil {
		return nil, err
	}

	pipelines, err := ValidateFlakePipelines(ctx, params.FlakeRef, namespace, params.Logger)
	if err != nil {
		return nil, err
	}

	// All definitions validated — publish.
	serverName := namespace.Server()

	templateCount, err := publishTemplates(ctx, params.Session, serverName, templates, params.Logger)
	if err != nil {
		return nil, err
	}

	pipelineCount, err := publishPipelines(ctx, params.Session, serverName, pipelines, params.Logger)
	if err != nil {
		return nil, err
	}

	if templateCount > 0 || pipelineCount > 0 {
		params.Logger.Info("published flake definitions",
			"templates", templateCount,
			"pipelines", pipelineCount,
		)
	}

	// Derive the pipeline reference from the fleet's namespace.
	pipelineRef := namespace.PipelineRoomAliasLocalpart() + ":environment-compose"
	if _, parseErr := schema.ParsePipelineRef(pipelineRef); parseErr != nil {
		return nil, cli.Internal("derived pipeline reference %q is invalid: %w", pipelineRef, parseErr)
	}

	// Resolve the machine's config room.
	configRoomAlias := params.Scope.Machine.RoomAlias()
	configRoomID, err := params.Session.ResolveAlias(ctx, configRoomAlias)
	if err != nil {
		return nil, cli.NotFound("resolving machine config room %s: %w (is the machine registered?)", configRoomAlias, err)
	}

	// Build pipeline parameters.
	parameters := map[string]any{
		"pipeline":      pipelineRef,
		"room":          params.FleetConfig.FleetRoomID.String(),
		"template":      params.FleetConfig.Template,
		"FLAKE_REF":     params.FlakeRef,
		"PROFILE":       params.Profile,
		"SYSTEM":        params.FleetConfig.System,
		"CACHE_NAME":    params.FleetConfig.CacheConfig.Name,
		"FLEET_ROOM_ID": params.FleetConfig.FleetRoomID.String(),
	}

	params.Logger.Info("submitting environment compose pipeline",
		"profile", params.Profile,
		"machine", params.Scope.Machine.Localpart(),
		"pipeline", pipelineRef,
		"flake", params.FlakeRef,
		"system", params.FleetConfig.System,
	)

	result, err := command.Execute(ctx, command.SendParams{
		Session:    params.Session,
		RoomID:     configRoomID,
		Command:    "pipeline.execute",
		Parameters: parameters,
	})
	if err != nil {
		return nil, err
	}
	if err := result.Err(); err != nil {
		return nil, err
	}

	output := &composeResult{
		Profile:     params.Profile,
		Machine:     params.Scope.Machine.Localpart(),
		PipelineRef: pipelineRef,
		TicketID:    result.TicketID,
		TicketRoom:  result.TicketRoom,
	}

	params.Logger.Info("pipeline accepted",
		"profile", params.Profile,
		"machine", params.Scope.Machine.Localpart(),
		"ticket_id", result.TicketID,
	)

	return output, nil
}

// watchComposeParams holds parameters for watching a compose pipeline
// ticket to completion.
type watchComposeParams struct {
	Session messaging.Session
	Result  *composeResult
	Timeout time.Duration
	Clock   clock.Clock
	Logger  *slog.Logger
}

// watchComposePipeline watches a compose pipeline ticket to completion,
// printing step progress to stderr. Returns the pipeline conclusion
// ("success" or "failure"). Returns an error if the watch itself fails
// (context canceled, connection lost). Does nothing if the result has
// no ticket ID.
func watchComposePipeline(ctx context.Context, params watchComposeParams) (string, error) {
	if params.Result.TicketID.IsZero() {
		return "", nil
	}

	watchCtx := ctx
	if params.Timeout > 0 {
		clk := params.Clock
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
		Session:    params.Session,
		RoomID:     params.Result.TicketRoom,
		TicketID:   params.Result.TicketID,
		OnProgress: command.StepProgressWriter(os.Stderr),
	})
	if watchErr != nil {
		return "", watchErr
	}

	conclusion := ""
	if final.Pipeline != nil {
		conclusion = string(final.Pipeline.Conclusion)
	}

	params.Logger.Info("compose pipeline completed",
		"profile", params.Result.Profile,
		"conclusion", conclusion,
	)

	return conclusion, nil
}

// writeAcceptedHint prints the pipeline ticket info and a follow-up
// command hint, matching the format of command.Result.WriteAcceptedHint.
func writeAcceptedHint(writer io.Writer, result *composeResult) {
	if result.TicketID.IsZero() {
		return
	}
	fmt.Fprintf(writer, "  ticket:  %s\n", result.TicketID)
	if !result.TicketRoom.IsZero() {
		fmt.Fprintf(writer, "  room:    %s\n", result.TicketRoom)
	}
	fmt.Fprintf(writer, "\nWatch progress: bureau pipeline wait %s --room %s\n",
		result.TicketID, result.TicketRoom)
}
