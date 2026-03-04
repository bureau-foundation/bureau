// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/cli/doctor"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// doctorParams holds the parameters for the fleet doctor command.
type doctorParams struct {
	cli.SessionConfig
	cli.JSONOutput
	Fix    bool `json:"fix"      flag:"fix"     desc:"automatically repair fixable issues"`
	DryRun bool `json:"dry_run"  flag:"dry-run" desc:"preview repairs without executing (requires --fix)"`
}

// fleetRoom describes a fleet room to check.
type fleetRoom struct {
	name             string
	alias            ref.RoomAlias
	powerLevelsFunc  func(ref.UserID) map[string]any
	memberEventTypes []ref.EventType // PL 0 events
	adminEventTypes  []ref.EventType // PL 100 events (beyond AdminProtectedEvents)
}

func doctorCommand() *cli.Command {
	var params doctorParams

	return &cli.Command{
		Name:    "doctor",
		Summary: "Check and repair fleet room health",
		Description: `Validate a fleet's Matrix room infrastructure: room existence, power levels,
dev team metadata, fleet cache configuration, and space hierarchy.

Fleet rooms accumulate drift as the schema evolves — new event types get
added, power level policies change, dev team metadata is introduced.
This command detects and optionally repairs that drift.

Requires admin credentials (--credential-file) because power level
changes require PL 100 in the target rooms.

Use --fix to automatically repair fixable issues. Use --fix --dry-run
to preview what would be repaired without making changes.`,
		Usage: "bureau fleet doctor <namespace/fleet/name> [flags]",
		Examples: []cli.Example{
			{
				Description: "Check fleet room health",
				Command:     "bureau fleet doctor bureau/fleet/prod --credential-file ./creds",
			},
			{
				Description: "Fix fleet room issues",
				Command:     "bureau fleet doctor bureau/fleet/prod --credential-file ./creds --fix",
			},
			{
				Description: "Preview fixes without applying",
				Command:     "bureau fleet doctor bureau/fleet/prod --credential-file ./creds --fix --dry-run",
			},
		},
		Annotations:    cli.ReadOnly(),
		Output:         func() any { return &doctor.JSONOutput{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/fleet/doctor"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) == 0 {
				return cli.Validation("fleet localpart is required (e.g., bureau/fleet/prod)")
			}
			if len(args) > 1 {
				return cli.Validation("expected exactly one argument (fleet localpart), got %d", len(args))
			}
			if params.DryRun && !params.Fix {
				return cli.Validation("--dry-run requires --fix")
			}

			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()

			return runFleetDoctor(ctx, args[0], &params, logger)
		},
	}
}

func runFleetDoctor(ctx context.Context, fleetLocalpart string, params *doctorParams, logger *slog.Logger) error {
	session, err := params.SessionConfig.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()

	server, err := ref.ServerFromUserID(session.UserID().String())
	if err != nil {
		return cli.Internal("cannot determine server name from session: %w", err)
	}

	fleet, err := ref.ParseFleet(fleetLocalpart, server)
	if err != nil {
		return cli.Validation("%v", err)
	}

	adminUserID := session.UserID()

	results, aggregateOutcome := runFleetDoctorChecks(ctx, session, fleet, adminUserID, server, params, logger)

	if done, err := params.EmitJSON(doctor.BuildJSON(results, params.DryRun, aggregateOutcome)); done {
		if err != nil {
			return err
		}
		for _, result := range results {
			if result.Status == doctor.StatusFail {
				return &cli.ExitError{Code: 1}
			}
		}
		return nil
	}

	return doctor.PrintChecklist(results, params.Fix, params.DryRun, aggregateOutcome)
}

func runFleetDoctorChecks(ctx context.Context, session messaging.Session, fleet ref.Fleet, adminUserID ref.UserID, server ref.ServerName, params *doctorParams, logger *slog.Logger) ([]doctor.Result, doctor.Outcome) {
	const maxFixIterations = 5
	repairedNames := make(map[string]bool)
	aggregateOutcome := doctor.Outcome{}

	isMatrixPermissionDenied := func(err error) bool {
		return messaging.IsMatrixError(err, messaging.ErrCodeForbidden)
	}

	var results []doctor.Result

	for range maxFixIterations {
		results = checkFleet(ctx, session, fleet, adminUserID, server, logger)

		if !params.Fix {
			break
		}

		for _, result := range results {
			if result.Status == doctor.StatusFail {
				repairedNames[result.Name] = true
			}
		}

		outcome := doctor.ExecuteFixes(ctx, results, params.DryRun, isMatrixPermissionDenied)
		if outcome.PermissionDenied {
			aggregateOutcome.PermissionDenied = true
		}
		aggregateOutcome.FixedCount += outcome.FixedCount
		if outcome.FixedCount == 0 || params.DryRun {
			break
		}
	}

	doctor.MarkRepaired(results, repairedNames)

	if aggregateOutcome.PermissionDenied {
		aggregateOutcome.PermissionDeniedHint = "The connected session lacks admin power level (100) in one or more fleet rooms. " +
			"Use --credential-file with admin credentials from 'bureau matrix setup'."
	}

	return results, aggregateOutcome
}

func checkFleet(ctx context.Context, session messaging.Session, fleet ref.Fleet, adminUserID ref.UserID, server ref.ServerName, logger *slog.Logger) []doctor.Result {
	var results []doctor.Result

	rooms := []fleetRoom{
		{
			name:            "fleet config",
			alias:           fleet.RoomAlias(),
			powerLevelsFunc: schema.FleetRoomPowerLevels,
			memberEventTypes: []ref.EventType{
				schema.EventTypeHALease,
				schema.EventTypeServiceStatus,
				schema.EventTypeFleetAlert,
			},
			adminEventTypes: []ref.EventType{
				schema.EventTypeFleetService,
				schema.EventTypeMachineDefinition,
				schema.EventTypeFleetConfig,
				schema.EventTypeFleetCache,
			},
		},
		{
			name:            "machine presence",
			alias:           fleet.MachineRoomAlias(),
			powerLevelsFunc: schema.MachineRoomPowerLevels,
			memberEventTypes: []ref.EventType{
				schema.EventTypeMachineKey,
				schema.EventTypeMachineInfo,
				schema.EventTypeMachineStatus,
				schema.EventTypeWebRTCOffer,
				schema.EventTypeWebRTCAnswer,
			},
		},
		{
			name:            "service directory",
			alias:           fleet.ServiceRoomAlias(),
			powerLevelsFunc: schema.ServiceRoomPowerLevels,
			memberEventTypes: []ref.EventType{
				schema.EventTypeService,
			},
		},
	}

	// Resolve the namespace space for space hierarchy checks.
	spaceAlias := fleet.Namespace().SpaceAlias()
	spaceRoomID, spaceErr := session.ResolveAlias(ctx, spaceAlias)
	if spaceErr != nil {
		results = append(results, doctor.Warn("namespace space", fmt.Sprintf("cannot resolve %s: %v (space hierarchy checks skipped)", spaceAlias, spaceErr)))
	}

	var spaceChildIDs map[string]bool
	if !spaceRoomID.IsZero() {
		var err error
		spaceChildIDs, err = getSpaceChildIDs(ctx, session, spaceRoomID)
		if err != nil {
			results = append(results, doctor.Warn("space children", fmt.Sprintf("cannot read space children: %v", err)))
		}
	}

	// Section 1: Room existence and power levels.
	for _, room := range rooms {
		roomID, err := session.ResolveAlias(ctx, room.alias)
		if err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				results = append(results, doctor.Fail(room.name+" room", fmt.Sprintf("%s not found — run 'bureau fleet create' first", room.alias)))
			} else {
				results = append(results, doctor.Fail(room.name+" room", fmt.Sprintf("cannot resolve %s: %v", room.alias, err)))
			}

			// Skip dependent checks for this room.
			results = append(results, doctor.Skip(room.name+" power levels", "skipped: room not found"))
			results = append(results, doctor.Skip(room.name+" dev team", "skipped: room not found"))
			if spaceChildIDs != nil {
				results = append(results, doctor.Skip(room.name+" in space", "skipped: room not found"))
			}
			continue
		}

		results = append(results, doctor.Pass(room.name+" room", fmt.Sprintf("%s → %s", room.alias, roomID)))

		// Power levels.
		expectedPowerLevels := room.powerLevelsFunc(adminUserID)
		results = append(results, checkPowerLevels(ctx, session, room.name, roomID, adminUserID, expectedPowerLevels, room.memberEventTypes, room.adminEventTypes)...)

		// Dev team metadata.
		results = append(results, checkDevTeamMetadata(ctx, session, room.name, roomID, fleet.Namespace())...)

		// Space hierarchy.
		if spaceChildIDs != nil {
			results = append(results, checkSpaceChild(room.name, roomID, spaceRoomID, spaceChildIDs, server, session)...)
		}
	}

	// Section 2: Fleet cache configuration (warn-only, not fail).
	fleetRoomID, err := session.ResolveAlias(ctx, fleet.RoomAlias())
	if err == nil {
		results = append(results, checkFleetCache(ctx, session, fleetRoomID)...)
	}

	return results
}

// --- Power level checks ---

// checkPowerLevels reads the m.room.power_levels state event from a room and
// verifies admin power, state_default, and the expected power levels for
// member-settable and admin-only event types. A single fix closure resets
// the entire power levels state.
func checkPowerLevels(ctx context.Context, session messaging.Session, name string, roomID ref.RoomID, adminUserID ref.UserID, expectedPowerLevels map[string]any, memberEventTypes, adminEventTypes []ref.EventType) []doctor.Result {
	var results []doctor.Result

	stateContent, err := session.GetStateEvent(ctx, roomID, schema.MatrixEventTypePowerLevels, "")
	if err != nil {
		results = append(results, doctor.Fail(name+" power levels", fmt.Sprintf("cannot read: %v", err)))
		return results
	}

	var powerLevels map[string]any
	if err := json.Unmarshal(stateContent, &powerLevels); err != nil {
		results = append(results, doctor.Fail(name+" power levels", fmt.Sprintf("invalid JSON: %v", err)))
		return results
	}

	// Build fix closure shared across all sub-checks. A single power level
	// write fixes all issues, so only the first failure gets the fix attached.
	fixPowerLevels := func(ctx context.Context) error {
		_, err := session.SendStateEvent(ctx, roomID, schema.MatrixEventTypePowerLevels, "", expectedPowerLevels)
		return err
	}
	fixAttached := false
	attachFix := func(result *doctor.Result) {
		if result.Status == doctor.StatusFail && !fixAttached {
			*result = doctor.FailWithFix(result.Name, result.Message,
				fmt.Sprintf("reset %s power levels", name), fixPowerLevels)
			fixAttached = true
		}
	}

	// Admin user power level.
	adminLevel := getUserPowerLevel(powerLevels, adminUserID.String())
	if adminLevel == 100 {
		results = append(results, doctor.Pass(name+" admin power", fmt.Sprintf("%s has power level 100", adminUserID)))
	} else {
		result := doctor.Fail(name+" admin power", fmt.Sprintf("%s has power level %.0f, expected 100", adminUserID, adminLevel))
		attachFix(&result)
		results = append(results, result)
	}

	// state_default must be 100.
	stateDefault := getNumericField(powerLevels, "state_default")
	if stateDefault == 100 {
		results = append(results, doctor.Pass(name+" state_default", "state_default is 100"))
	} else {
		result := doctor.Fail(name+" state_default", fmt.Sprintf("state_default is %.0f, expected 100", stateDefault))
		attachFix(&result)
		results = append(results, result)
	}

	// Member-settable event types must be at PL 0.
	for _, eventType := range memberEventTypes {
		eventTypeString := eventType.String()
		level := getStateEventPowerLevel(powerLevels, eventTypeString)
		if level == 0 {
			results = append(results, doctor.Pass(name+" "+eventTypeString, fmt.Sprintf("members can set %s (level 0)", eventType)))
		} else {
			result := doctor.Fail(name+" "+eventTypeString, fmt.Sprintf("%s requires power level %.0f, expected 0", eventType, level))
			attachFix(&result)
			results = append(results, result)
		}
	}

	// Admin-only event types must be at PL 100.
	for _, eventType := range adminEventTypes {
		eventTypeString := eventType.String()
		level := getStateEventPowerLevel(powerLevels, eventTypeString)
		if level == 100 {
			results = append(results, doctor.Pass(name+" "+eventTypeString, fmt.Sprintf("%s is admin-only (level 100)", eventType)))
		} else {
			result := doctor.Fail(name+" "+eventTypeString, fmt.Sprintf("%s requires power level %.0f, expected 100 (admin-only)", eventType, level))
			attachFix(&result)
			results = append(results, result)
		}
	}

	return results
}

// getUserPowerLevel extracts a user's power level from the power_levels content.
// Returns users_default if the user is not in the users map.
func getUserPowerLevel(powerLevels map[string]any, userID string) float64 {
	usersDefault := getNumericField(powerLevels, "users_default")

	users, ok := powerLevels["users"].(map[string]any)
	if !ok {
		return usersDefault
	}

	level, ok := users[userID].(float64)
	if !ok {
		return usersDefault
	}
	return level
}

// getStateEventPowerLevel extracts the power level required to send a specific
// state event type. Per the Matrix spec, state events not explicitly listed in
// the events map fall back to state_default, not events_default.
func getStateEventPowerLevel(powerLevels map[string]any, eventType string) float64 {
	stateDefault := getNumericField(powerLevels, "state_default")

	events, ok := powerLevels["events"].(map[string]any)
	if !ok {
		return stateDefault
	}

	level, ok := events[eventType].(float64)
	if !ok {
		return stateDefault
	}
	return level
}

// getNumericField extracts a float64 field from a JSON-decoded map.
// Returns 0 if the field is missing or not a number.
func getNumericField(data map[string]any, key string) float64 {
	value, ok := data[key].(float64)
	if !ok {
		return 0
	}
	return value
}

// --- Dev team metadata checks ---

// checkDevTeamMetadata verifies that a room carries an m.bureau.dev_team state
// event pointing to the conventional dev team room alias for this namespace.
func checkDevTeamMetadata(ctx context.Context, session messaging.Session, name string, roomID ref.RoomID, namespace ref.Namespace) []doctor.Result {
	expectedAlias := schema.DevTeamRoomAlias(namespace)
	expectedContent := schema.DevTeamContent{Room: expectedAlias}

	checkName := name + " dev team"
	content, err := messaging.GetState[schema.DevTeamContent](ctx, session, roomID, schema.EventTypeDevTeam, "")
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			capturedRoomID := roomID
			return []doctor.Result{doctor.FailWithFix(
				checkName,
				"m.bureau.dev_team not set",
				fmt.Sprintf("publish dev team metadata on %s", name),
				func(ctx context.Context) error {
					_, err := session.SendStateEvent(ctx, capturedRoomID, schema.EventTypeDevTeam, "", expectedContent)
					return err
				},
			)}
		}
		return []doctor.Result{doctor.Fail(checkName, fmt.Sprintf("cannot read: %v", err))}
	}

	if content.Room != expectedAlias {
		capturedRoomID := roomID
		return []doctor.Result{doctor.FailWithFix(
			checkName,
			fmt.Sprintf("points to %s, expected %s", content.Room, expectedAlias),
			fmt.Sprintf("fix dev team metadata on %s", name),
			func(ctx context.Context) error {
				_, err := session.SendStateEvent(ctx, capturedRoomID, schema.EventTypeDevTeam, "", expectedContent)
				return err
			},
		)}
	}

	return []doctor.Result{doctor.Pass(checkName, fmt.Sprintf("points to %s", content.Room))}
}

// --- Space hierarchy checks ---

// getSpaceChildIDs reads m.space.child state events from a space and returns
// the set of child room IDs.
func getSpaceChildIDs(ctx context.Context, session messaging.Session, spaceRoomID ref.RoomID) (map[string]bool, error) {
	events, err := session.GetRoomState(ctx, spaceRoomID)
	if err != nil {
		return nil, err
	}

	children := make(map[string]bool)
	for _, event := range events {
		if event.Type == schema.MatrixEventTypeSpaceChild && event.StateKey != nil && *event.StateKey != "" {
			children[*event.StateKey] = true
		}
	}
	return children, nil
}

// checkSpaceChild verifies that a room is a child of the namespace space.
func checkSpaceChild(name string, roomID ref.RoomID, spaceRoomID ref.RoomID, spaceChildren map[string]bool, server ref.ServerName, session messaging.Session) []doctor.Result {
	roomIDString := roomID.String()
	if spaceChildren[roomIDString] {
		return []doctor.Result{doctor.Pass(name+" in space", "listed as m.space.child")}
	}

	capturedRoomID := roomID
	return []doctor.Result{doctor.FailWithFix(
		name+" in space",
		fmt.Sprintf("%s is not a child of the namespace space", roomIDString),
		fmt.Sprintf("add %s as space child", name),
		func(ctx context.Context) error {
			_, err := session.SendStateEvent(ctx, spaceRoomID, schema.MatrixEventTypeSpaceChild, capturedRoomID.String(),
				map[string]any{"via": []string{server.String()}})
			return err
		},
	)}
}

// --- Fleet cache check ---

// checkFleetCache verifies that the fleet has a fleet cache configuration
// event. This is a warning (not failure) because not all fleets use a
// binary cache.
func checkFleetCache(ctx context.Context, session messaging.Session, fleetRoomID ref.RoomID) []doctor.Result {
	cacheConfig, err := messaging.GetState[schema.FleetCacheContent](ctx, session, fleetRoomID, schema.EventTypeFleetCache, "")
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return []doctor.Result{doctor.Warn("fleet cache", "no m.bureau.fleet_cache event published (run 'bureau fleet cache' to configure)")}
		}
		return []doctor.Result{doctor.Fail("fleet cache", fmt.Sprintf("cannot read: %v", err))}
	}

	if cacheConfig.URL == "" {
		return []doctor.Result{doctor.Warn("fleet cache", "fleet cache event exists but URL is empty")}
	}

	keyCount := len(cacheConfig.PublicKeys)
	if keyCount == 0 {
		return []doctor.Result{doctor.Warn("fleet cache", fmt.Sprintf("url=%s but no signing keys configured", cacheConfig.URL))}
	}

	return []doctor.Result{doctor.Pass("fleet cache", fmt.Sprintf("url=%s keys=%d", cacheConfig.URL, keyCount))}
}
