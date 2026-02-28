// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/cli/doctor"
	"github.com/bureau-foundation/bureau/lib/content"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// doctorParams holds the parameters for the matrix doctor command.
// SessionConfig is embedded as a FlagBinder — its flags are registered
// via AddFlags and excluded from JSON Schema generation.
type doctorParams struct {
	cli.SessionConfig
	ServerName string `json:"server_name"  flag:"server-name"  desc:"Matrix server name for constructing aliases (auto-detected from machine.conf)"`
	cli.JSONOutput
	Fix    bool `json:"fix"    flag:"fix"     desc:"automatically repair fixable issues"`
	DryRun bool `json:"dry_run" flag:"dry-run" desc:"preview repairs without executing (requires --fix)"`
}

// matrixPermissionDeniedHint is the guidance printed when Matrix API
// calls fail with M_FORBIDDEN, indicating the session lacks admin
// power levels.
const matrixPermissionDeniedHint = `Some fixes failed due to insufficient permissions. Power level
and state event changes require admin credentials. Re-run with
the credential file from "bureau matrix setup":

  bureau matrix doctor --fix --credential-file ./bureau-creds`

// DoctorCommand returns the "doctor" subcommand for checking Bureau Matrix
// infrastructure health and optionally repairing fixable issues.
func DoctorCommand() *cli.Command {
	var params doctorParams

	return &cli.Command{
		Name:    "doctor",
		Summary: "Check Bureau Matrix infrastructure health",
		Description: `Validate the Bureau Matrix homeserver state: reachability, authentication,
spaces, rooms, hierarchy, power levels, join rules, and membership.

Runs a series of checks against the expected state created by "bureau matrix setup"
and reports pass/fail/warn for each. Exits with code 1 if any check fails.

Use --fix to automatically repair fixable issues. Use --fix --dry-run to preview
what would be repaired without making changes.

Use --json for machine-readable output suitable for monitoring or CI.`,
		Usage: "bureau matrix doctor [flags]",
		Examples: []cli.Example{
			{
				Description: "Check with credential file",
				Command:     "bureau matrix doctor --credential-file ./creds",
			},
			{
				Description: "Check and repair fixable issues",
				Command:     "bureau matrix doctor --credential-file ./creds --fix",
			},
			{
				Description: "Preview repairs without executing",
				Command:     "bureau matrix doctor --credential-file ./creds --fix --dry-run",
			},
			{
				Description: "Machine-readable output",
				Command:     "bureau matrix doctor --credential-file ./creds --json",
			},
		},
		Annotations:    cli.ReadOnly(),
		Output:         func() any { return &doctor.JSONOutput{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/doctor"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}
			if params.DryRun && !params.Fix {
				return cli.Validation("--dry-run requires --fix")
			}

			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()

			homeserverURL, err := params.SessionConfig.ResolveHomeserverURL()
			if err != nil {
				return err
			}

			var storedCredentials map[string]string
			if params.SessionConfig.CredentialFile != "" {
				storedCredentials, err = cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
				if err != nil {
					return cli.Internal("read credential file: %w", err)
				}
			}

			client, err := messaging.NewClient(messaging.ClientConfig{
				HomeserverURL: homeserverURL,
			})
			if err != nil {
				return cli.Internal("create client: %w", err)
			}

			sess, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return err
			}

			// Run checks, optionally fixing. In fix mode, loop until no
			// new fixes are applied (or a cap of 5 iterations). This handles
			// cascading dependencies: iteration 1 might create missing rooms,
			// enabling iteration 2 to fix hierarchy, power levels, templates,
			// and credentials that depend on those rooms existing. Typical
			// convergence is 2 iterations.
			const maxFixIterations = 5
			repairedNames := make(map[string]bool)
			var aggregateOutcome doctor.Outcome
			var results []doctor.Result

			params.ServerName = cli.ResolveServerName(params.ServerName)

			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return cli.Validation("invalid --server-name %q: %w", params.ServerName, err)
			}

			isMatrixPermissionDenied := func(err error) bool {
				return messaging.IsMatrixError(err, messaging.ErrCodeForbidden)
			}

			for iteration := range maxFixIterations {
				_ = iteration
				results = runDoctor(ctx, client, sess, serverName, storedCredentials, params.SessionConfig.CredentialFile, logger)

				if !params.Fix {
					break
				}

				// Track every failing check name before fixing.
				// Some fixes repair multiple failures at once (e.g.,
				// a single credential file update fixes all stale
				// room IDs). Tracking failing names before fixes run
				// lets us mark all of them as repaired in the final
				// output, not just the one that carried the fix closure.
				for _, r := range results {
					if r.Status == doctor.StatusFail {
						repairedNames[r.Name] = true
					}
				}

				outcome := doctor.ExecuteFixes(ctx, results, params.DryRun, isMatrixPermissionDenied)
				if outcome.PermissionDenied {
					aggregateOutcome.PermissionDenied = true
				}
				if outcome.FixedCount == 0 || params.DryRun {
					break
				}

				// Re-read credential file for the next iteration since
				// a fix may have updated it.
				if params.SessionConfig.CredentialFile != "" {
					storedCredentials, err = cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
					if err != nil {
						return cli.Internal("re-read credential file: %w", err)
					}
				}
			}

			// Mark checks that pass now but were failing in an
			// earlier iteration — these were repaired by a fix even
			// if they didn't directly carry the fix closure.
			doctor.MarkRepaired(results, repairedNames)

			if aggregateOutcome.PermissionDenied {
				aggregateOutcome.PermissionDeniedHint = matrixPermissionDeniedHint
			}

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
		},
	}
}

// standardRoom defines one of the rooms that "bureau matrix setup" creates.
type standardRoom struct {
	alias                    string          // local alias (e.g., "bureau/system")
	displayName              string          // room name for CreateRoom (e.g., "Bureau Machine")
	topic                    string          // room topic for CreateRoom
	name                     string          // human name for check output
	credentialKey            string          // key in credential file (e.g., "MATRIX_SYSTEM_ROOM")
	memberSettableEventTypes []ref.EventType // event types that members should be able to set

	// powerLevelsFunc overrides the default power level generation. When
	// nil, powerLevels() returns adminOnlyPowerLevels with the room's
	// memberSettableEventTypes. Set this for rooms that need a different
	// power level structure (e.g., pipeline rooms use events_default: 100).
	powerLevelsFunc func(adminUserID ref.UserID) map[string]any
}

// powerLevels returns the correct power level structure for this room.
func (r standardRoom) powerLevels(adminUserID ref.UserID) map[string]any {
	if r.powerLevelsFunc != nil {
		return r.powerLevelsFunc(adminUserID)
	}
	return adminOnlyPowerLevels(adminUserID, r.memberSettableEventTypes)
}

var standardRooms = []standardRoom{
	{
		alias:           "bureau/system",
		displayName:     "Bureau System",
		topic:           "Operational messages",
		name:            "system room",
		credentialKey:   "MATRIX_SYSTEM_ROOM",
		powerLevelsFunc: schema.SystemRoomPowerLevels,
		memberSettableEventTypes: []ref.EventType{
			schema.EventTypeTokenSigningKey,
		},
	},
	{
		alias:         "bureau/template",
		displayName:   "Bureau Template",
		topic:         "Sandbox templates",
		name:          "template room",
		credentialKey: "MATRIX_TEMPLATE_ROOM",
	},
	{
		alias:           "bureau/pipeline",
		displayName:     "Bureau Pipeline",
		topic:           "Pipeline definitions",
		name:            "pipeline room",
		credentialKey:   "MATRIX_PIPELINE_ROOM",
		powerLevelsFunc: schema.PipelineRoomPowerLevels,
	},
	{
		alias:           "bureau/artifact",
		displayName:     "Bureau Artifact",
		topic:           "Artifact coordination",
		name:            "artifact room",
		credentialKey:   "MATRIX_ARTIFACT_ROOM",
		powerLevelsFunc: schema.ArtifactRoomPowerLevels,
	},
}

// runDoctor executes all health checks and returns the results. Fixable
// failures carry fix closures that ExecuteFixes can invoke.
//
// When credentialFilePath is non-empty, credential mismatches are reported
// as fixable failures (the fix updates the credential file). When empty,
// credential mismatches remain warnings since there is no file to update.
func runDoctor(ctx context.Context, client *messaging.Client, session messaging.Session, serverName ref.ServerName, storedCredentials map[string]string, credentialFilePath string, logger *slog.Logger) []doctor.Result {
	var results []doctor.Result

	// Section 1: Connectivity and authentication.
	versionsResult := checkServerVersions(ctx, client)
	results = append(results, versionsResult)

	if versionsResult.Status == doctor.StatusFail {
		results = append(results, doctor.Skip("authentication", "skipped: homeserver unreachable"))
		results = append(results, doctor.Skip("bureau space", "skipped: homeserver unreachable"))
		for _, room := range standardRooms {
			results = append(results, doctor.Skip(room.name, "skipped: homeserver unreachable"))
		}
		return results
	}

	authResult := checkAuth(ctx, session)
	results = append(results, authResult)

	if authResult.Status == doctor.StatusFail {
		results = append(results, doctor.Skip("bureau space", "skipped: authentication failed"))
		for _, room := range standardRooms {
			results = append(results, doctor.Skip(room.name, "skipped: authentication failed"))
		}
		return results
	}

	// Section 2: Space and rooms exist.
	spaceAlias := ref.MustParseRoomAlias(schema.FullRoomAlias("bureau", serverName))
	spaceResult, spaceRoomID := checkRoomExists(ctx, session, "bureau space", spaceAlias)
	if spaceResult.Status == doctor.StatusFail {
		spaceResult.FixHint = "create Bureau space"
		spaceResult = doctor.FailWithFix(spaceResult.Name, spaceResult.Message, "create Bureau space",
			func(ctx context.Context) error {
				id, err := ensureSpace(ctx, session, serverName, logger)
				if err != nil {
					return err
				}
				spaceRoomID = id
				return nil
			})
	}
	results = append(results, spaceResult)

	roomIDs := make(map[string]ref.RoomID) // local alias → room ID
	for _, room := range standardRooms {
		room := room // capture for closure
		fullAlias := ref.MustParseRoomAlias(schema.FullRoomAlias(room.alias, serverName))
		result, roomID := checkRoomExists(ctx, session, room.name, fullAlias)
		if result.Status == doctor.StatusFail {
			result = doctor.FailWithFix(result.Name, result.Message, fmt.Sprintf("create %s", room.name),
				func(ctx context.Context) error {
					id, err := ensureRoom(ctx, session, room.alias, room.displayName, room.topic,
						spaceRoomID, serverName, room.powerLevels(session.UserID()), logger)
					if err != nil {
						return err
					}
					roomIDs[room.alias] = id
					return nil
				})
		}
		results = append(results, result)
		if !roomID.IsZero() {
			roomIDs[room.alias] = roomID
		}
	}

	// Section 3: Credential file cross-reference. When a credential file
	// path is available, mismatches are fixable (the fix updates the file
	// in place). Without a path, mismatches are informational warnings.
	if storedCredentials != nil {
		// Build the complete set of correct room IDs for a batch update.
		// The fix closure captures this, so all credential issues are
		// repaired in a single file write (same pattern as power levels).
		correctRoomIDs := make(map[string]string)
		if !spaceRoomID.IsZero() {
			correctRoomIDs["MATRIX_SPACE_ROOM"] = spaceRoomID.String()
		}
		for _, room := range standardRooms {
			if roomID, ok := roomIDs[room.alias]; ok {
				correctRoomIDs[room.credentialKey] = roomID.String()
			}
		}

		var credentialFix doctor.FixAction
		if credentialFilePath != "" {
			credentialFix = func(ctx context.Context) error {
				return cli.UpdateCredentialFile(credentialFilePath, correctRoomIDs)
			}
		}
		fixAttached := false

		if !spaceRoomID.IsZero() {
			result := checkCredentialMatch("bureau space", "MATRIX_SPACE_ROOM", spaceRoomID.String(), storedCredentials)
			if credentialFix != nil && (result.Status == doctor.StatusWarn || result.Status == doctor.StatusFail) {
				result.Status = doctor.StatusFail
				if !fixAttached {
					result = doctor.FailWithFix(result.Name, result.Message, "update credential file", credentialFix)
					fixAttached = true
				}
			}
			results = append(results, result)
		}
		for _, room := range standardRooms {
			if roomID, ok := roomIDs[room.alias]; ok {
				result := checkCredentialMatch(room.name, room.credentialKey, roomID.String(), storedCredentials)
				if credentialFix != nil && (result.Status == doctor.StatusWarn || result.Status == doctor.StatusFail) {
					result.Status = doctor.StatusFail
					if !fixAttached {
						result = doctor.FailWithFix(result.Name, result.Message, "update credential file", credentialFix)
						fixAttached = true
					}
				}
				results = append(results, result)
			}
		}
	}

	// Section 4: Space hierarchy — rooms are children of the space.
	if !spaceRoomID.IsZero() {
		spaceChildIDs, err := getSpaceChildIDs(ctx, session, spaceRoomID)
		if err != nil {
			results = append(results, doctor.Fail("space hierarchy", fmt.Sprintf("cannot read space state: %v", err)))
		} else {
			for _, room := range standardRooms {
				roomID, ok := roomIDs[room.alias]
				if !ok {
					continue
				}
				result := checkSpaceChild(room.name, roomID.String(), spaceChildIDs)
				if result.Status == doctor.StatusFail {
					capturedRoomID := roomID
					capturedName := room.name
					result = doctor.FailWithFix(result.Name, result.Message,
						fmt.Sprintf("add %s as child of Bureau space", capturedName),
						func(ctx context.Context) error {
							_, err := session.SendStateEvent(ctx, spaceRoomID, "m.space.child", capturedRoomID.String(),
								map[string]any{"via": []string{serverName.String()}})
							return err
						})
				}
				results = append(results, result)
			}
		}
	}

	// Section 5: Power levels.
	adminUserID := session.UserID()
	if !spaceRoomID.IsZero() {
		results = append(results, checkPowerLevels(ctx, session, "bureau space", spaceRoomID, adminUserID,
			nil, adminOnlyPowerLevels(adminUserID, nil))...)
	}
	for _, room := range standardRooms {
		roomID, ok := roomIDs[room.alias]
		if !ok {
			continue
		}
		results = append(results, checkPowerLevels(ctx, session, room.name, roomID, adminUserID,
			room.memberSettableEventTypes, room.powerLevels(adminUserID))...)
	}

	// Section 6: Join rules.
	if !spaceRoomID.IsZero() {
		results = append(results, checkJoinRules(ctx, session, "bureau space", spaceRoomID))
	}
	for _, room := range standardRooms {
		roomID, ok := roomIDs[room.alias]
		if !ok {
			continue
		}
		results = append(results, checkJoinRules(ctx, session, room.name, roomID))
	}

	// Section 7: Operator membership — non-admin, non-machine members of
	// the Bureau space should be in every standard room. The space is
	// invite-only, so anyone who's joined it was deliberately onboarded.
	if !spaceRoomID.IsZero() {
		results = append(results, checkOperatorMembership(ctx, session, spaceRoomID, serverName, roomIDs)...)
	}

	// Section 9: Base templates published.
	if templateRoomID, ok := roomIDs["bureau/template"]; ok {
		results = append(results, checkBaseTemplates(ctx, session, templateRoomID)...)
	}

	// Section 10: Base pipelines published.
	if pipelineRoomID, ok := roomIDs["bureau/pipeline"]; ok {
		results = append(results, checkBasePipelines(ctx, session, pipelineRoomID)...)
	}

	return results
}

// checkServerVersions verifies the homeserver is reachable by calling the
// unauthenticated /_matrix/client/versions endpoint.
func checkServerVersions(ctx context.Context, client *messaging.Client) doctor.Result {
	versions, err := client.ServerVersions(ctx)
	if err != nil {
		return doctor.Fail("homeserver", fmt.Sprintf("unreachable: %v", err))
	}
	return doctor.Pass("homeserver", fmt.Sprintf("reachable, versions: %s", strings.Join(versions.Versions, ", ")))
}

// checkAuth verifies that the session's access token is valid.
func checkAuth(ctx context.Context, session messaging.Session) doctor.Result {
	userID, err := session.WhoAmI(ctx)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeUnknownToken) {
			return doctor.Fail("authentication", "access token is invalid or expired")
		}
		return doctor.Fail("authentication", fmt.Sprintf("WhoAmI failed: %v", err))
	}

	if userID != session.UserID() {
		return doctor.Warn("authentication", fmt.Sprintf("token valid but user ID mismatch: WhoAmI returned %q, session has %q", userID, session.UserID()))
	}

	return doctor.Pass("authentication", fmt.Sprintf("authenticated as %s", userID))
}

// checkRoomExists resolves a room alias and returns the result plus the room ID
// (zero value if resolution failed).
func checkRoomExists(ctx context.Context, session messaging.Session, name string, alias ref.RoomAlias) (doctor.Result, ref.RoomID) {
	roomID, err := session.ResolveAlias(ctx, alias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return doctor.Fail(name, fmt.Sprintf("alias %s not found", alias)), ref.RoomID{}
		}
		return doctor.Fail(name, fmt.Sprintf("resolve %s: %v", alias, err)), ref.RoomID{}
	}
	return doctor.Pass(name, fmt.Sprintf("%s → %s", alias, roomID)), roomID
}

// checkCredentialMatch verifies that a resolved room ID matches the value
// stored in the credential file.
func checkCredentialMatch(name, credentialKey, resolvedRoomID string, credentials map[string]string) doctor.Result {
	storedRoomID, ok := credentials[credentialKey]
	if !ok {
		return doctor.Warn(name+" credential", fmt.Sprintf("%s not found in credential file", credentialKey))
	}
	if storedRoomID != resolvedRoomID {
		return doctor.Fail(name+" credential", fmt.Sprintf("%s=%s but alias resolves to %s (credential file stale?)", credentialKey, storedRoomID, resolvedRoomID))
	}
	return doctor.Pass(name+" credential", fmt.Sprintf("%s matches", credentialKey))
}

// getSpaceChildIDs reads m.space.child state events from a space and returns
// the set of child room IDs.
func getSpaceChildIDs(ctx context.Context, session messaging.Session, spaceRoomID ref.RoomID) (map[string]bool, error) {
	events, err := session.GetRoomState(ctx, spaceRoomID)
	if err != nil {
		return nil, err
	}

	children := make(map[string]bool)
	for _, event := range events {
		if event.Type == "m.space.child" && event.StateKey != nil && *event.StateKey != "" {
			children[*event.StateKey] = true
		}
	}
	return children, nil
}

// checkSpaceChild verifies that a room is a child of the Bureau space.
func checkSpaceChild(name, roomID string, spaceChildren map[string]bool) doctor.Result {
	if spaceChildren[roomID] {
		return doctor.Pass(name+" in space", "listed as m.space.child")
	}
	return doctor.Fail(name+" in space", fmt.Sprintf("%s is not a child of the Bureau space", roomID))
}

// checkPowerLevels reads the m.room.power_levels state event from a room and
// verifies: admin at 100, state_default at 100, and any expected member-settable
// event types at power level 0. The first failing sub-check gets a fix closure
// that resets the entire power levels state to expectedPowerLevels; subsequent
// sub-checks share that single fix.
func checkPowerLevels(ctx context.Context, session messaging.Session, name string, roomID ref.RoomID, adminUserID ref.UserID, memberSettableEventTypes []ref.EventType, expectedPowerLevels map[string]any) []doctor.Result {
	var results []doctor.Result

	stateContent, err := session.GetStateEvent(ctx, roomID, "m.room.power_levels", "")
	if err != nil {
		results = append(results, doctor.Fail(name+" power levels", fmt.Sprintf("cannot read: %v", err)))
		return results
	}

	var powerLevels map[string]any
	if err := json.Unmarshal(stateContent, &powerLevels); err != nil {
		results = append(results, doctor.Fail(name+" power levels", fmt.Sprintf("invalid JSON: %v", err)))
		return results
	}

	// Build fix closure shared across all PL sub-checks for this room.
	// Only attached to the first failure — a single PL write fixes all issues.
	fixPowerLevels := func(ctx context.Context) error {
		_, err := session.SendStateEvent(ctx, roomID, "m.room.power_levels", "",
			expectedPowerLevels)
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

	// Check admin user power level.
	adminLevel := getUserPowerLevel(powerLevels, adminUserID.String())
	if adminLevel == 100 {
		results = append(results, doctor.Pass(name+" admin power", fmt.Sprintf("%s has power level 100", adminUserID)))
	} else {
		result := doctor.Fail(name+" admin power", fmt.Sprintf("%s has power level %.0f, expected 100", adminUserID, adminLevel))
		attachFix(&result)
		results = append(results, result)
	}

	// Check state_default (should be 100 — only admin can set arbitrary state).
	stateDefault := getNumericField(powerLevels, "state_default")
	if stateDefault == 100 {
		results = append(results, doctor.Pass(name+" state_default", "state_default is 100"))
	} else {
		result := doctor.Fail(name+" state_default", fmt.Sprintf("state_default is %.0f, expected 100", stateDefault))
		attachFix(&result)
		results = append(results, result)
	}

	// Check that each member-settable event type is at power level 0.
	for _, eventType := range memberSettableEventTypes {
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
// the events map fall back to state_default, not events_default. This
// distinction matters: rooms with state_default=100 and events_default=0
// block state events from regular members unless the event type has an
// explicit entry in the events map.
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

// checkJoinRules verifies that a room's join rules are set to "invite"
// (the required configuration for Bureau rooms).
func checkJoinRules(ctx context.Context, session messaging.Session, name string, roomID ref.RoomID) doctor.Result {
	joinRulesContent, err := session.GetStateEvent(ctx, roomID, "m.room.join_rules", "")
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			capturedRoomID := roomID
			return doctor.FailWithFix(
				name+" join rules",
				"join_rules state not found",
				fmt.Sprintf("set %s join_rule to invite", name),
				func(ctx context.Context) error {
					_, err := session.SendStateEvent(ctx, capturedRoomID, "m.room.join_rules", "",
						map[string]any{"join_rule": "invite"})
					return err
				},
			)
		}
		return doctor.Fail(name+" join rules", fmt.Sprintf("cannot read join rules: %v", err))
	}

	var joinRules map[string]any
	if err := json.Unmarshal(joinRulesContent, &joinRules); err != nil {
		return doctor.Fail(name+" join rules", fmt.Sprintf("invalid join rules JSON: %v", err))
	}

	joinRule, _ := joinRules["join_rule"].(string)
	if joinRule == "invite" {
		return doctor.Pass(name+" join rules", "join_rule is invite")
	}

	capturedRoomID := roomID
	return doctor.FailWithFix(
		name+" join rules",
		fmt.Sprintf("join_rule is %q, expected invite", joinRule),
		fmt.Sprintf("set %s join_rule to invite", name),
		func(ctx context.Context) error {
			_, err := session.SendStateEvent(ctx, capturedRoomID, "m.room.join_rules", "",
				map[string]any{"join_rule": "invite"})
			return err
		},
	)
}

// checkOperatorMembership verifies that every operator (a non-admin,
// non-machine member of the Bureau space) is a member of every standard
// room. When doctor --fix creates new rooms, existing operators won't be
// in them — this check catches that gap and offers invite fixes.
//
// Operators are identified by space membership: the Bureau space is
// invite-only, so any joined user was deliberately onboarded. Machine
// accounts (localparts starting with "machine/") are excluded because
// they are only invited to global rooms during provisioning.
func checkOperatorMembership(ctx context.Context, session messaging.Session, spaceRoomID ref.RoomID, serverName ref.ServerName, roomIDs map[string]ref.RoomID) []doctor.Result {
	// Get all joined members of the Bureau space.
	spaceMembers, err := session.GetRoomMembers(ctx, spaceRoomID)
	if err != nil {
		return []doctor.Result{doctor.Fail("operator membership", fmt.Sprintf("cannot read space members: %v", err))}
	}

	adminUserID := session.UserID()
	var operators []ref.UserID
	for _, member := range spaceMembers {
		if member.Membership != "join" {
			continue
		}
		if member.UserID == adminUserID {
			continue
		}
		// Machine accounts have localparts like "machine/worker-01".
		localpart := strings.TrimSuffix(strings.TrimPrefix(member.UserID.String(), "@"), ":"+serverName.String())
		if strings.HasPrefix(localpart, "machine/") {
			continue
		}
		operators = append(operators, member.UserID)
	}

	if len(operators) == 0 {
		return nil
	}

	var results []doctor.Result

	for _, room := range standardRooms {
		roomID, ok := roomIDs[room.alias]
		if !ok {
			continue
		}

		members, err := session.GetRoomMembers(ctx, roomID)
		if err != nil {
			results = append(results, doctor.Fail(room.name+" operator membership", fmt.Sprintf("cannot read members: %v", err)))
			continue
		}

		memberSet := make(map[ref.UserID]bool)
		for _, member := range members {
			if member.Membership == "join" || member.Membership == "invite" {
				memberSet[member.UserID] = true
			}
		}

		for _, operatorUserID := range operators {
			checkName := fmt.Sprintf("%s in %s", operatorUserID, room.name)
			if memberSet[operatorUserID] {
				results = append(results, doctor.Pass(checkName, "member"))
			} else {
				capturedUserID := operatorUserID
				capturedRoomID := roomID
				results = append(results, doctor.FailWithFix(
					checkName,
					fmt.Sprintf("operator %s not in %s", capturedUserID, room.name),
					fmt.Sprintf("invite %s to %s", capturedUserID, room.name),
					func(ctx context.Context) error {
						return session.InviteUser(ctx, capturedRoomID, capturedUserID)
					},
				))
			}
		}
	}

	return results
}

// stateEventItem describes a single state event that should be published
// in a Matrix room. Used by checkPublishedStateEvents to verify presence
// and offer a fix action if missing.
type stateEventItem struct {
	label     string        // human-readable label (e.g., "template", "pipeline")
	name      string        // state key
	eventType ref.EventType // Matrix event type
	content   any           // value to publish on fix
}

// checkPublishedStateEvents verifies that each item in the list is present
// as a state event in the given room. Missing items produce a fixable failure
// that re-publishes the expected content.
func checkPublishedStateEvents(ctx context.Context, session messaging.Session, roomID ref.RoomID, items []stateEventItem) []doctor.Result {
	var results []doctor.Result

	for _, item := range items {
		checkName := fmt.Sprintf("%s %q", item.label, item.name)
		_, err := session.GetStateEvent(ctx, roomID, item.eventType, item.name)
		if err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				capturedItem := item
				capturedRoomID := roomID
				results = append(results, doctor.FailWithFix(
					checkName,
					fmt.Sprintf("%s %q not published", item.label, item.name),
					fmt.Sprintf("publish %s %q", item.label, item.name),
					func(ctx context.Context) error {
						_, err := session.SendStateEvent(ctx, capturedRoomID, capturedItem.eventType,
							capturedItem.name, capturedItem.content)
						return err
					},
				))
				continue
			}
			results = append(results, doctor.Fail(checkName, fmt.Sprintf("cannot read: %v", err)))
			continue
		}
		results = append(results, doctor.Pass(checkName, "published"))
	}

	return results
}

// checkBaseTemplates verifies that the standard Bureau templates ("base" and
// "base-networked") are published as m.bureau.template state events in the
// template room. Missing templates are fixable by re-publishing them.
func checkBaseTemplates(ctx context.Context, session messaging.Session, templateRoomID ref.RoomID) []doctor.Result {
	var items []stateEventItem
	for _, template := range baseTemplates() {
		items = append(items, stateEventItem{
			label:     "template",
			name:      template.name,
			eventType: schema.EventTypeTemplate,
			content:   template.content,
		})
	}
	return checkPublishedStateEvents(ctx, session, templateRoomID, items)
}

// checkBasePipelines verifies that the embedded pipeline definitions are
// published as m.bureau.pipeline state events in the pipeline room. Missing
// pipelines are fixable by re-publishing them.
func checkBasePipelines(ctx context.Context, session messaging.Session, pipelineRoomID ref.RoomID) []doctor.Result {
	pipelines, err := content.Pipelines()
	if err != nil {
		return []doctor.Result{doctor.Fail("pipeline content", fmt.Sprintf("cannot load embedded pipelines: %v", err))}
	}

	var items []stateEventItem
	for _, pipeline := range pipelines {
		items = append(items, stateEventItem{
			label:     "pipeline",
			name:      pipeline.Name,
			eventType: schema.EventTypePipeline,
			content:   pipeline.Content,
		})
	}
	return checkPublishedStateEvents(ctx, session, pipelineRoomID, items)
}
