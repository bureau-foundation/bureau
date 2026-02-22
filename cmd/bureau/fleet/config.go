// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	fleetschema "github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/messaging"
)

// configParams holds the parameters for the fleet config command.
// Config management requires direct Matrix access to read/write
// FleetConfigContent state events in the fleet room, since the fleet
// controller's socket API does not have a config-write action.
type configParams struct {
	cli.SessionConfig
	cli.JSONOutput
	RebalancePolicy          string `json:"rebalance_policy"          flag:"rebalance-policy"          desc:"rebalance policy: auto or alert"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds" flag:"heartbeat-interval"       desc:"expected heartbeat interval in seconds"`
	PressureThresholdCPU     int    `json:"pressure_threshold_cpu"    flag:"pressure-threshold-cpu"    desc:"CPU pressure threshold percentage"`
	PressureThresholdMemory  int    `json:"pressure_threshold_memory" flag:"pressure-threshold-memory" desc:"memory pressure threshold percentage"`
	PressureThresholdGPU     int    `json:"pressure_threshold_gpu"    flag:"pressure-threshold-gpu"    desc:"GPU pressure threshold percentage"`
	PressureSustainedSeconds int    `json:"pressure_sustained_seconds" flag:"pressure-sustained"       desc:"seconds pressure must be sustained before action"`
	RebalanceCooldownSeconds int    `json:"rebalance_cooldown_seconds" flag:"rebalance-cooldown"       desc:"seconds after placement before service can be moved again"`
}

// configResult is the JSON output of the config command.
type configResult struct {
	Fleet   ref.Fleet                      `json:"fleet"   desc:"fleet reference"`
	Service ref.Service                    `json:"service" desc:"fleet controller service reference"`
	Config  fleetschema.FleetConfigContent `json:"config"  desc:"fleet controller configuration"`
}

func configCommand() *cli.Command {
	var params configParams

	return &cli.Command{
		Name:    "config",
		Summary: "View or update fleet controller configuration",
		Description: `Read or write the FleetConfigContent for a fleet controller.

The argument is a fleet localpart in the form "namespace/fleet/name"
(e.g., "bureau/fleet/prod"). The server name is derived from the
connected session's identity. The fleet controller's service localpart
(used as the state key) is derived from the fleet: for bureau/fleet/prod,
the state key is "bureau/fleet/prod/service/fleet".

Without any config flags, displays the current configuration. With
config flags (--rebalance-policy, --heartbeat-interval, etc.), performs
a read-modify-write to update the configuration.

Requires direct Matrix access via --credential-file, since the fleet
controller's socket API does not expose a config-write action.`,
		Usage: "bureau fleet config <namespace/fleet/name> [flags]",
		Examples: []cli.Example{
			{
				Description: "View current fleet config",
				Command:     "bureau fleet config bureau/fleet/prod --credential-file ./creds",
			},
			{
				Description: "Set rebalance policy to auto",
				Command:     "bureau fleet config bureau/fleet/prod --rebalance-policy auto --credential-file ./creds",
			},
			{
				Description: "Set heartbeat interval and pressure thresholds",
				Command:     "bureau fleet config bureau/fleet/prod --heartbeat-interval 60 --pressure-threshold-cpu 80 --credential-file ./creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &configResult{} },
		Annotations:    cli.Idempotent(),
		RequiredGrants: []string{"command/fleet/config"},
		Run: func(args []string) error {
			if len(args) == 0 {
				return cli.Validation("fleet localpart is required (e.g., bureau/fleet/prod)")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (fleet localpart), got %d", len(args))
			}
			return runConfig(args[0], &params)
		},
	}
}

// hasConfigUpdates returns true if any config flag was explicitly set.
func hasConfigUpdates(params *configParams) bool {
	return params.RebalancePolicy != "" ||
		params.HeartbeatIntervalSeconds != 0 ||
		params.PressureThresholdCPU != 0 ||
		params.PressureThresholdMemory != 0 ||
		params.PressureThresholdGPU != 0 ||
		params.PressureSustainedSeconds != 0 ||
		params.RebalanceCooldownSeconds != 0
}

func runConfig(fleetLocalpart string, params *configParams) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return cli.Internal("connecting admin session: %w", err)
	}
	defer session.Close()

	// Derive server name from the connected session's identity.
	server, err := ref.ServerFromUserID(session.UserID().String())
	if err != nil {
		return cli.Internal("cannot determine server name from session: %w", err)
	}

	// Parse and validate the fleet localpart.
	fleet, err := ref.ParseFleet(fleetLocalpart, server)
	if err != nil {
		return cli.Validation("%v", err)
	}

	// The fleet controller service is always named "fleet" within its fleet.
	// Its localpart is the state key for the FleetConfigContent event.
	service, err := ref.NewService(fleet, "fleet")
	if err != nil {
		return cli.Internal("constructing fleet controller service ref: %w", err)
	}
	stateKey := service.Localpart()

	// Resolve fleet room from the fleet's room alias.
	fleetRoomID, err := session.ResolveAlias(ctx, fleet.RoomAlias())
	if err != nil {
		return cli.NotFound("resolving fleet room %s: %w (run 'bureau fleet create' first)", fleet.RoomAlias(), err)
	}

	// Read existing config.
	var config fleetschema.FleetConfigContent
	existingContent, err := session.GetStateEvent(ctx, fleetRoomID, schema.EventTypeFleetConfig, stateKey)
	if err == nil {
		if unmarshalErr := json.Unmarshal(existingContent, &config); unmarshalErr != nil {
			return cli.Internal("parsing existing fleet config: %w", unmarshalErr)
		}
	} else if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return cli.Internal("reading fleet config: %w", err)
	}

	if !hasConfigUpdates(params) {
		// Read-only mode: display current config.
		result := configResult{
			Fleet:   fleet,
			Service: service,
			Config:  config,
		}

		if done, err := params.EmitJSON(result); done {
			return err
		}

		writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(writer, "Fleet Config: %s (key: %s)\n\n", fleet.Localpart(), stateKey)
		fmt.Fprintf(writer, "  Rebalance Policy:\t%s\n", defaultString(config.RebalancePolicy, "(not set)"))
		fmt.Fprintf(writer, "  Heartbeat Interval:\t%s\n", formatOptionalSeconds(config.HeartbeatIntervalSeconds, 30))
		fmt.Fprintf(writer, "  Pressure CPU:\t%s\n", formatOptionalPercent(config.PressureThresholdCPU, 85))
		fmt.Fprintf(writer, "  Pressure Memory:\t%s\n", formatOptionalPercent(config.PressureThresholdMemory, 90))
		fmt.Fprintf(writer, "  Pressure GPU:\t%s\n", formatOptionalPercent(config.PressureThresholdGPU, 95))
		fmt.Fprintf(writer, "  Pressure Sustained:\t%s\n", formatOptionalSeconds(config.PressureSustainedSeconds, 300))
		fmt.Fprintf(writer, "  Rebalance Cooldown:\t%s\n", formatOptionalSeconds(config.RebalanceCooldownSeconds, 600))
		writer.Flush()
		return nil
	}

	// Write mode: merge updates and publish.
	if params.RebalancePolicy != "" {
		if params.RebalancePolicy != "auto" && params.RebalancePolicy != "alert" {
			return cli.Validation("--rebalance-policy must be 'auto' or 'alert', got %q", params.RebalancePolicy)
		}
		config.RebalancePolicy = params.RebalancePolicy
	}
	if params.HeartbeatIntervalSeconds != 0 {
		config.HeartbeatIntervalSeconds = params.HeartbeatIntervalSeconds
	}
	if params.PressureThresholdCPU != 0 {
		config.PressureThresholdCPU = params.PressureThresholdCPU
	}
	if params.PressureThresholdMemory != 0 {
		config.PressureThresholdMemory = params.PressureThresholdMemory
	}
	if params.PressureThresholdGPU != 0 {
		config.PressureThresholdGPU = params.PressureThresholdGPU
	}
	if params.PressureSustainedSeconds != 0 {
		config.PressureSustainedSeconds = params.PressureSustainedSeconds
	}
	if params.RebalanceCooldownSeconds != 0 {
		config.RebalanceCooldownSeconds = params.RebalanceCooldownSeconds
	}

	_, err = session.SendStateEvent(ctx, fleetRoomID, schema.EventTypeFleetConfig, stateKey, config)
	if err != nil {
		return cli.Internal("publishing fleet config: %w", err)
	}

	result := configResult{
		Fleet:   fleet,
		Service: service,
		Config:  config,
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	fmt.Fprintf(os.Stderr, "Updated fleet config for %s (key: %s)\n", fleet.Localpart(), stateKey)
	return nil
}

// defaultString returns the value if non-empty, otherwise the fallback.
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// formatOptionalSeconds formats a seconds value with its default shown.
func formatOptionalSeconds(value, defaultValue int) string {
	if value == 0 {
		return fmt.Sprintf("%ds (default)", defaultValue)
	}
	return fmt.Sprintf("%ds", value)
}

// formatOptionalPercent formats a percentage value with its default shown.
func formatOptionalPercent(value, defaultValue int) string {
	if value == 0 {
		return fmt.Sprintf("%d%% (default)", defaultValue)
	}
	return fmt.Sprintf("%d%%", value)
}
