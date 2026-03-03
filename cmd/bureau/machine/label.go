// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// labelParams holds the CLI parameters shared by cordon, uncordon, and label.
type labelParams struct {
	cli.SessionConfig
	cli.JSONOutput
}

// labelResult is the JSON output of cordon, uncordon, and label commands.
type labelResult struct {
	Machine ref.Machine       `json:"machine" desc:"machine whose labels were modified"`
	Labels  map[string]string `json:"labels"  desc:"labels after modification"`
}

func cordonCommand() *cli.Command {
	var params labelParams

	return &cli.Command{
		Name:    "cordon",
		Summary: "Mark a machine as ineligible for new placements",
		Description: `Add the "cordoned" label to a machine, making it ineligible for
new fleet placements. Existing workloads continue running.

The positional argument is a machine reference — either a bare localpart
(e.g., "bureau/fleet/prod/machine/worker-01") or a full Matrix user ID
(e.g., "@bureau/fleet/prod/machine/worker-01:server"). The @ sigil
distinguishes the two forms. When a bare localpart is given, the server
name is derived from the connected admin session.

The fleet controller treats the "cordoned" label as an INELIGIBLE
condition in placement scoring. Use "uncordon" to re-enable placements.`,
		Usage: "bureau machine cordon <machine-ref> [flags]",
		Examples: []cli.Example{
			{
				Description: "Cordon a machine for maintenance",
				Command:     "bureau machine cordon bureau/fleet/prod/machine/worker-01 --credential-file ./bureau-creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &labelResult{} },
		RequiredGrants: []string{"command/machine/cordon"},
		Annotations:    cli.Idempotent(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("machine reference is required").
					WithHint("Usage: bureau machine cordon <machine-ref> [flags]")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (machine reference), got %d", len(args))
			}

			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			session, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}
			defer session.Close()

			machine, err := resolveSessionMachine(session, args[0])
			if err != nil {
				return err
			}

			result, err := modifyMachineLabels(ctx, session, machine, func(labels map[string]string) map[string]string {
				if labels == nil {
					labels = make(map[string]string)
				}
				labels["cordoned"] = "true"
				return labels
			})
			if err != nil {
				return err
			}

			if done, emitErr := params.EmitJSON(result); done {
				return emitErr
			}

			logger.Info("machine cordoned — new placements will score it as INELIGIBLE",
				"machine", machine.Localpart(),
			)
			return nil
		},
	}
}

func uncordonCommand() *cli.Command {
	var params labelParams

	return &cli.Command{
		Name:    "uncordon",
		Summary: "Re-enable a machine for fleet placements",
		Description: `Remove the "cordoned" label from a machine, making it eligible
for new fleet placements again.

The positional argument is a machine reference — either a bare localpart
(e.g., "bureau/fleet/prod/machine/worker-01") or a full Matrix user ID
(e.g., "@bureau/fleet/prod/machine/worker-01:server"). The @ sigil
distinguishes the two forms. When a bare localpart is given, the server
name is derived from the connected admin session.`,
		Usage: "bureau machine uncordon <machine-ref> [flags]",
		Examples: []cli.Example{
			{
				Description: "Re-enable placements after maintenance",
				Command:     "bureau machine uncordon bureau/fleet/prod/machine/worker-01 --credential-file ./bureau-creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &labelResult{} },
		RequiredGrants: []string{"command/machine/uncordon"},
		Annotations:    cli.Idempotent(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("machine reference is required").
					WithHint("Usage: bureau machine uncordon <machine-ref> [flags]")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (machine reference), got %d", len(args))
			}

			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			session, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}
			defer session.Close()

			machine, err := resolveSessionMachine(session, args[0])
			if err != nil {
				return err
			}

			result, err := modifyMachineLabels(ctx, session, machine, func(labels map[string]string) map[string]string {
				delete(labels, "cordoned")
				return labels
			})
			if err != nil {
				return err
			}

			if done, emitErr := params.EmitJSON(result); done {
				return emitErr
			}

			logger.Info("machine uncordoned — re-eligible for placements",
				"machine", machine.Localpart(),
			)
			return nil
		},
	}
}

func labelCommand() *cli.Command {
	var params labelParams

	return &cli.Command{
		Name:    "label",
		Summary: "Add, update, or remove machine labels",
		Description: `Set or remove labels on a machine. Labels are key-value pairs used
by the fleet controller for placement constraint matching.

The first argument is a machine reference — either a bare localpart
(e.g., "bureau/fleet/prod/machine/worker-01") or a full Matrix user ID.
Remaining arguments are key=value pairs. Split on the first "=" only —
values may contain "=" characters.

To remove a label, set an empty value: "bureau machine label <machine> key=".

Labels persist as part of the MachineInfo state event in the fleet's
machine room. The fleet controller reads them for placement constraint
matching (PlacementConstraints.Requires field).`,
		Usage: "bureau machine label <machine-ref> <key>=<value> [<key>=<value>...] [flags]",
		Examples: []cli.Example{
			{
				Description: "Add GPU and tier labels",
				Command:     "bureau machine label bureau/fleet/prod/machine/worker-01 gpu=h100 tier=production --credential-file ./bureau-creds",
			},
			{
				Description: "Remove a label",
				Command:     "bureau machine label bureau/fleet/prod/machine/worker-01 tier= --credential-file ./bureau-creds",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &labelResult{} },
		RequiredGrants: []string{"command/machine/label"},
		Annotations:    cli.Idempotent(),
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) < 2 {
				return cli.Validation("machine reference and at least one key=value pair are required").
					WithHint("Usage: bureau machine label <machine-ref> <key>=<value> [<key>=<value>...] [flags]")
			}

			updates, removals, err := parseLabelArgs(args[1:])
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			session, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}
			defer session.Close()

			machine, err := resolveSessionMachine(session, args[0])
			if err != nil {
				return err
			}

			result, err := modifyMachineLabels(ctx, session, machine, func(labels map[string]string) map[string]string {
				if labels == nil {
					labels = make(map[string]string)
				}
				for key, value := range updates {
					labels[key] = value
				}
				for _, key := range removals {
					delete(labels, key)
				}
				return labels
			})
			if err != nil {
				return err
			}

			if done, emitErr := params.EmitJSON(result); done {
				return emitErr
			}

			if len(result.Labels) == 0 {
				logger.Info("all labels removed", "machine", machine.Localpart())
				return nil
			}

			keys := make([]string, 0, len(result.Labels))
			for key := range result.Labels {
				keys = append(keys, key)
			}
			slices.Sort(keys)

			writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(writer, "KEY\tVALUE")
			for _, key := range keys {
				fmt.Fprintf(writer, "%s\t%s\n", key, result.Labels[key])
			}
			writer.Flush()
			return nil
		},
	}
}

// parseLabelArgs parses key=value arguments into updates (non-empty values)
// and removals (empty values). Each argument must contain at least one "="
// character. Split on the first "=" only — values may contain "=".
func parseLabelArgs(args []string) (updates map[string]string, removals []string, err error) {
	updates = make(map[string]string)
	for _, arg := range args {
		index := strings.IndexByte(arg, '=')
		if index < 0 {
			return nil, nil, cli.Validation("invalid label %q: expected key=value format", arg).
				WithHint("Each label argument must contain '='. To remove a label, use 'key=' with an empty value.")
		}
		key := arg[:index]
		value := arg[index+1:]
		if key == "" {
			return nil, nil, cli.Validation("invalid label %q: key cannot be empty", arg)
		}
		if value == "" {
			removals = append(removals, key)
		} else {
			updates[key] = value
		}
	}
	return updates, removals, nil
}

// resolveSessionMachine parses a machine argument and derives the default
// server name from the connected session's identity. This is the common
// pattern used by machine subcommands that accept a positional machine ref.
func resolveSessionMachine(session messaging.Session, arg string) (ref.Machine, error) {
	defaultServer, err := ref.ServerFromUserID(session.UserID().String())
	if err != nil {
		return ref.Machine{}, cli.Internal("cannot determine server name from session: %w", err)
	}
	machine, err := resolveMachineArg(arg, defaultServer)
	if err != nil {
		return ref.Machine{}, cli.Validation("invalid machine reference: %v", err)
	}
	return machine, nil
}

// modifyMachineLabels reads the current MachineInfo for a machine from the
// fleet's machine room, applies the transform function to its Labels map,
// and writes the updated MachineInfo back. Returns the post-modification
// result. The transform receives the current labels (possibly nil if none
// were set) and must return the desired labels.
func modifyMachineLabels(ctx context.Context, session messaging.Session, machine ref.Machine, transform func(map[string]string) map[string]string) (*labelResult, error) {
	fleet := machine.Fleet()
	machineRoomAlias := fleet.MachineRoomAlias()
	machineRoomID, err := session.ResolveAlias(ctx, machineRoomAlias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return nil, cli.NotFound("fleet machine room %s not found", machineRoomAlias).
				WithHint("Has 'bureau matrix setup' been run for this fleet?")
		}
		return nil, cli.Transient("resolving fleet machine room %s: %w", machineRoomAlias, err)
	}

	stateKey := machine.Localpart()
	info, err := messaging.GetState[schema.MachineInfo](ctx, session, machineRoomID, schema.EventTypeMachineInfo, stateKey)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return nil, cli.NotFound("no MachineInfo for %s in %s", stateKey, machineRoomAlias).
				WithHint("The machine may not have published its hardware info yet.\n" +
					"Is the daemon running on this machine?")
		}
		return nil, cli.Transient("reading MachineInfo for %s: %w", stateKey, err)
	}

	info.Labels = transform(info.Labels)

	// Normalize empty labels to nil so JSON omitempty drops the field.
	if len(info.Labels) == 0 {
		info.Labels = nil
	}

	if _, err := session.SendStateEvent(ctx, machineRoomID, schema.EventTypeMachineInfo, stateKey, info); err != nil {
		return nil, cli.Transient("writing MachineInfo for %s: %w", stateKey, err)
	}

	// Return a non-nil map for consistent JSON output.
	resultLabels := info.Labels
	if resultLabels == nil {
		resultLabels = make(map[string]string)
	}

	return &labelResult{
		Machine: machine,
		Labels:  resultLabels,
	}, nil
}
