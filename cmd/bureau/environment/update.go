// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/nix"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// updateParams holds the parameters for the environment update command.
type updateParams struct {
	cli.FleetScope
	cli.JSONOutput
	Profile  string        `json:"profile"   desc:"Nix profile name to check for updates" required:"true"`
	FlakeRef string        `json:"flake_ref" flag:"flake"    desc:"flake reference for the environment repo" default:"github:bureau-foundation/environment"`
	System   string        `json:"system"    flag:"system"   desc:"Nix system triple (e.g., x86_64-linux) — defaults from fleet cache config"`
	Template string        `json:"template"  flag:"template" desc:"template reference for the compose sandbox — defaults from fleet cache config"`
	DryRun   bool          `json:"dry_run"   flag:"dry-run"  desc:"check for updates without triggering a compose"`
	Wait     bool          `json:"wait"      flag:"wait"     desc:"wait for the compose pipeline to complete"`
	Timeout  time.Duration `json:"timeout"   flag:"timeout"  desc:"maximum time to wait when --wait is set (0 means no limit)" default:"0"`
	Yes      bool          `json:"yes"       flag:"yes"      desc:"skip confirmation prompt"`

	// clock is injected by tests for deterministic timeout control.
	clock clock.Clock
}

// environmentUpdateResult is the JSON output for environment update.
type environmentUpdateResult struct {
	Profile         string       `json:"profile"                    desc:"Nix profile name"`
	Status          string       `json:"status"                     desc:"updated, current, or error"`
	FlakeRef        string       `json:"flake_ref"                  desc:"source flake reference"`
	BuildRevision   string       `json:"build_revision,omitempty"   desc:"revision of the last build (short hash)"`
	CurrentRevision string       `json:"current_revision,omitempty" desc:"current HEAD revision of the source flake (short hash)"`
	StorePath       string       `json:"store_path,omitempty"       desc:"store path from the last build"`
	TicketID        ref.TicketID `json:"ticket_id,omitempty"        desc:"pipeline ticket ID (when compose is triggered)"`
	TicketRoom      ref.RoomID   `json:"ticket_room,omitempty"      desc:"ticket room (when compose is triggered)"`
	Conclusion      string       `json:"conclusion,omitempty"       desc:"pipeline conclusion (when --wait is used)"`
	Error           string       `json:"error,omitempty"            desc:"error message"`
	DryRun          bool         `json:"dry_run"                    desc:"true if no compose was triggered"`
}

func updateCommand() *cli.Command {
	var params updateParams

	return &cli.Command{
		Name:    "update",
		Summary: "Check for environment updates and optionally recompose",
		Description: `Check whether the source flake for a profile has a newer revision than
the last compose and optionally trigger a recompose.

Reads the m.bureau.environment_build state event for the profile to get
the revision that was built last, then queries the source flake for its
current HEAD revision. If they differ, the profile is stale.

Without --dry-run or --yes, the command prompts before triggering a
compose. With --dry-run, only reports staleness without composing.
With --yes, triggers the compose immediately.

The profile must have been composed at least once — use "bureau
environment compose" for the initial build.`,
		Usage: "bureau environment update <profile> --machine <machine> [flags]",
		Examples: []cli.Example{
			{
				Description: "Check if the workstation profile is up to date",
				Command:     "bureau environment update workstation --machine bureau/fleet/prod/machine/builder --dry-run",
			},
			{
				Description: "Update a stale profile and wait for completion",
				Command:     "bureau environment update workstation --machine bureau/fleet/prod/machine/builder --wait",
			},
			{
				Description: "Non-interactive update",
				Command:     "bureau environment update workstation --machine bureau/fleet/prod/machine/builder --yes",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &environmentUpdateResult{} },
		RequiredGrants: []string{"command/pipeline/execute"},
		Annotations:    cli.Create(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			// In CLI mode, profile comes as a positional argument.
			if len(args) == 1 {
				params.Profile = args[0]
			} else if len(args) > 1 {
				return cli.Validation("usage: bureau environment update <profile> --machine <machine>")
			}
			if params.Profile == "" {
				return cli.Validation("profile is required\n\nusage: bureau environment update <profile> --machine <machine>")
			}

			scope, err := params.FleetScope.Resolve()
			if err != nil {
				return err
			}
			if scope.Machine.IsZero() {
				return cli.Validation("--machine is required for update (specifies which machine runs the build)")
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

			// When --json is set, treat as dry-run unless --yes is
			// explicit, to prevent MCP tools from triggering composes.
			dryRun := params.DryRun
			if params.JSONOutput.OutputJSON && !params.Yes {
				dryRun = true
			}

			result, err := checkEnvironmentUpdate(ctx, checkUpdateParams{
				Session:     session,
				FleetRoomID: config.FleetRoomID,
				Profile:     params.Profile,
				FlakeRef:    params.FlakeRef,
				Logger:      logger,
			})
			if err != nil {
				return err
			}

			if result.Status != "updated" {
				// Current or error — emit and return.
				if done, emitErr := params.EmitJSON(result); done {
					return emitErr
				}
				printUpdateResult(result)
				return nil
			}

			// Profile is stale.
			result.DryRun = dryRun

			if dryRun {
				if done, emitErr := params.EmitJSON(result); done {
					return emitErr
				}
				printUpdateResult(result)
				return nil
			}

			if !params.Yes {
				if !confirmEnvironmentUpdate(result) {
					result.Status = "skipped"
					result.DryRun = true
					if done, emitErr := params.EmitJSON(result); done {
						return emitErr
					}
					printUpdateResult(result)
					return nil
				}
			}

			// Trigger compose.
			composeOutput, err := submitComposePipeline(ctx, submitComposeParams{
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

			result.TicketID = composeOutput.TicketID
			result.TicketRoom = composeOutput.TicketRoom

			if done, emitErr := params.EmitJSON(result); done {
				return emitErr
			}

			if !params.Wait {
				printUpdateResult(result)
				writeAcceptedHint(os.Stderr, composeOutput)
				return nil
			}

			conclusion, err := watchComposePipeline(ctx, watchComposeParams{
				Session: session,
				Result:  composeOutput,
				Timeout: params.Timeout,
				Clock:   params.clock,
				Logger:  logger,
			})
			if err != nil {
				return err
			}

			result.Conclusion = conclusion
			printUpdateResult(result)

			if conclusion != "success" {
				return &cli.ExitError{Code: 1}
			}
			return nil
		},
	}
}

// checkUpdateParams holds parameters for checking environment staleness.
type checkUpdateParams struct {
	Session     messaging.Session
	FleetRoomID ref.RoomID
	Profile     string
	FlakeRef    string
	Logger      *slog.Logger
}

// checkEnvironmentUpdate reads the last build record for a profile and
// compares its revision against the source flake's current HEAD.
func checkEnvironmentUpdate(ctx context.Context, params checkUpdateParams) (*environmentUpdateResult, error) {
	result := &environmentUpdateResult{
		Profile:  params.Profile,
		FlakeRef: params.FlakeRef,
	}

	// Read the existing build record.
	build, err := messaging.GetState[schema.EnvironmentBuildContent](
		ctx, params.Session, params.FleetRoomID, schema.EventTypeEnvironmentBuild, params.Profile,
	)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			result.Status = "error"
			result.Error = "no previous build found for this profile"
			return result, nil
		}
		return nil, cli.Transient("reading environment build for %q: %w", params.Profile, err)
	}

	result.StorePath = build.StorePath
	if build.ResolvedRevision != "" {
		result.BuildRevision = cli.ShortRevision(build.ResolvedRevision)
	}

	// Resolve the current flake revision.
	params.Logger.Info("checking flake for updates",
		"profile", params.Profile,
		"flake_ref", params.FlakeRef,
		"build_revision", result.BuildRevision,
	)

	currentRevision, err := resolveFlakeRevision(ctx, params.FlakeRef)
	if err != nil {
		result.Status = "error"
		result.Error = fmt.Sprintf("resolving flake revision: %v", err)
		return result, nil
	}

	result.CurrentRevision = cli.ShortRevision(currentRevision)

	// Compare revisions.
	if build.ResolvedRevision == "" {
		// Build predates revision tracking. Treat as stale so the
		// user can rebuild and start tracking revisions.
		result.Status = "updated"
		params.Logger.Info("build has no recorded revision — treating as stale",
			"profile", params.Profile,
			"current_revision", result.CurrentRevision,
		)
		return result, nil
	}

	if build.ResolvedRevision == currentRevision {
		result.Status = "current"
		return result, nil
	}

	result.Status = "updated"
	params.Logger.Info("update available",
		"profile", params.Profile,
		"build_revision", result.BuildRevision,
		"current_revision", result.CurrentRevision,
	)

	return result, nil
}

// resolveFlakeRevision queries a flake for its current git revision
// using "nix flake metadata --json".
func resolveFlakeRevision(ctx context.Context, flakeRef string) (string, error) {
	output, err := nix.RunContext(ctx, "flake", "metadata", "--json", flakeRef)
	if err != nil {
		return "", fmt.Errorf("nix flake metadata %s: %w", flakeRef, err)
	}

	var metadata struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal([]byte(output), &metadata); err != nil {
		return "", fmt.Errorf("parsing flake metadata: %w", err)
	}

	if metadata.Revision == "" {
		return "", fmt.Errorf("flake %s has no revision (local path?)", flakeRef)
	}

	return metadata.Revision, nil
}

// confirmEnvironmentUpdate prompts the user to confirm a recompose.
func confirmEnvironmentUpdate(result *environmentUpdateResult) bool {
	fmt.Fprintf(os.Stderr, "\n%s: update available\n", result.Profile)
	fmt.Fprintf(os.Stderr, "  flake: %s\n", result.FlakeRef)
	if result.BuildRevision != "" && result.CurrentRevision != "" {
		fmt.Fprintf(os.Stderr, "  revision: %s → %s\n", result.BuildRevision, result.CurrentRevision)
	} else if result.CurrentRevision != "" {
		fmt.Fprintf(os.Stderr, "  current revision: %s (no previous revision recorded)\n", result.CurrentRevision)
	}
	if result.StorePath != "" {
		fmt.Fprintf(os.Stderr, "  current store path: %s\n", result.StorePath)
	}
	fmt.Fprintf(os.Stderr, "  Trigger recompose? [y/N] ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	return answer == "y" || answer == "yes"
}

// printUpdateResult prints a human-readable summary of the update check.
func printUpdateResult(result *environmentUpdateResult) {
	switch result.Status {
	case "current":
		fmt.Fprintf(os.Stdout, "%s: up to date", result.Profile)
		if result.BuildRevision != "" {
			fmt.Fprintf(os.Stdout, " (rev %s)", result.BuildRevision)
		}
		fmt.Fprintln(os.Stdout)

	case "updated":
		if result.DryRun {
			fmt.Fprintf(os.Stdout, "%s: update available (dry-run)\n", result.Profile)
		} else if result.Conclusion != "" {
			fmt.Fprintf(os.Stdout, "%s: recompose %s\n", result.Profile, result.Conclusion)
		} else if !result.TicketID.IsZero() {
			fmt.Fprintf(os.Stdout, "%s: recompose triggered\n", result.Profile)
		}
		if result.BuildRevision != "" && result.CurrentRevision != "" {
			fmt.Fprintf(os.Stdout, "  revision: %s → %s\n", result.BuildRevision, result.CurrentRevision)
		} else if result.CurrentRevision != "" {
			fmt.Fprintf(os.Stdout, "  current revision: %s\n", result.CurrentRevision)
		}

	case "skipped":
		fmt.Fprintf(os.Stdout, "%s: skipped by user\n", result.Profile)

	case "error":
		fmt.Fprintf(os.Stderr, "%s: error: %s\n", result.Profile, result.Error)
	}
}
