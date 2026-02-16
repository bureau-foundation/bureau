// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/content"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// doctorParams holds the parameters for the matrix doctor command.
// SessionConfig is embedded as a FlagBinder — its flags are registered
// via AddFlags and excluded from JSON Schema generation.
type doctorParams struct {
	cli.SessionConfig
	ServerName string `json:"server_name"  flag:"server-name"  desc:"Matrix server name for constructing aliases" default:"bureau.local"`
	cli.JSONOutput
	Fix    bool `json:"fix"    flag:"fix"     desc:"automatically repair fixable issues"`
	DryRun bool `json:"dry_run" flag:"dry-run" desc:"preview repairs without executing (requires --fix)"`
}

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
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/doctor"},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}
			if params.DryRun && !params.Fix {
				return fmt.Errorf("--dry-run requires --fix")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			homeserverURL, err := params.SessionConfig.ResolveHomeserverURL()
			if err != nil {
				return err
			}

			var storedCredentials map[string]string
			if params.SessionConfig.CredentialFile != "" {
				storedCredentials, err = cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
				if err != nil {
					return fmt.Errorf("read credential file: %w", err)
				}
			}

			client, err := messaging.NewClient(messaging.ClientConfig{
				HomeserverURL: homeserverURL,
			})
			if err != nil {
				return fmt.Errorf("create client: %w", err)
			}

			sess, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}

			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
				Level: slog.LevelInfo,
			}))

			// Run checks, optionally fixing. In fix mode, loop until no
			// new fixes are applied (or a cap of 5 iterations). This handles
			// cascading dependencies: iteration 1 might create missing rooms,
			// enabling iteration 2 to fix hierarchy, power levels, templates,
			// and credentials that depend on those rooms existing. Typical
			// convergence is 2 iterations.
			const maxFixIterations = 5
			repairedNames := make(map[string]bool)
			var results []checkResult

			for iteration := range maxFixIterations {
				_ = iteration
				results = runDoctor(ctx, client, sess, params.ServerName, storedCredentials, params.SessionConfig.CredentialFile, logger)

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
					if r.Status == statusFail {
						repairedNames[r.Name] = true
					}
				}

				fixCount := executeFixes(ctx, sess, results, params.DryRun)
				if fixCount == 0 || params.DryRun {
					break
				}

				// Re-read credential file for the next iteration since
				// a fix may have updated it.
				if params.SessionConfig.CredentialFile != "" {
					storedCredentials, err = cli.ReadCredentialFile(params.SessionConfig.CredentialFile)
					if err != nil {
						return fmt.Errorf("re-read credential file: %w", err)
					}
				}
			}

			// Mark checks that pass now but were failing in an
			// earlier iteration — these were repaired by a fix even
			// if they didn't directly carry the fix closure.
			for i := range results {
				if results[i].Status == statusPass && repairedNames[results[i].Name] {
					results[i].Status = statusFixed
				}
			}

			if done, err := params.EmitJSON(doctorJSON(results, params.DryRun)); done {
				if err != nil {
					return err
				}
				for _, result := range results {
					if result.Status == statusFail {
						return &cli.ExitError{Code: 1}
					}
				}
				return nil
			}
			return printChecklist(results, params.Fix, params.DryRun)
		},
	}
}

// fixAction is a function that repairs a failed check.
type fixAction func(ctx context.Context, session *messaging.Session) error

// checkStatus is the outcome of a single health check.
type checkStatus string

const (
	statusPass  checkStatus = "pass"
	statusFail  checkStatus = "fail"
	statusWarn  checkStatus = "warn"
	statusSkip  checkStatus = "skip"
	statusFixed checkStatus = "fixed"
)

// checkResult holds the outcome of a single health check. Fixable failures
// carry a FixHint (human description) and an unexported fix function.
type checkResult struct {
	Name    string      `json:"name"`
	Status  checkStatus `json:"status"`
	Message string      `json:"message"`
	FixHint string      `json:"fix_hint,omitempty"`
	fix     fixAction
}

func pass(name, message string) checkResult {
	return checkResult{Name: name, Status: statusPass, Message: message}
}

func fail(name, message string) checkResult {
	return checkResult{Name: name, Status: statusFail, Message: message}
}

func failWithFix(name, message, fixHint string, fix fixAction) checkResult {
	return checkResult{Name: name, Status: statusFail, Message: message, FixHint: fixHint, fix: fix}
}

func warn(name, message string) checkResult {
	return checkResult{Name: name, Status: statusWarn, Message: message}
}

func skip(name, message string) checkResult {
	return checkResult{Name: name, Status: statusSkip, Message: message}
}

// standardRoom defines one of the rooms that "bureau matrix setup" creates.
type standardRoom struct {
	alias                    string   // local alias (e.g., "bureau/machine")
	displayName              string   // room name for CreateRoom (e.g., "Bureau Machine")
	topic                    string   // room topic for CreateRoom
	name                     string   // human name for check output
	credentialKey            string   // key in credential file (e.g., "MATRIX_MACHINE_ROOM")
	memberSettableEventTypes []string // event types that members should be able to set

	// powerLevelsFunc overrides the default power level generation. When
	// nil, powerLevels() returns adminOnlyPowerLevels with the room's
	// memberSettableEventTypes. Set this for rooms that need a different
	// power level structure (e.g., pipeline rooms use events_default: 100).
	powerLevelsFunc func(adminUserID string) map[string]any
}

// powerLevels returns the correct power level structure for this room.
func (r standardRoom) powerLevels(adminUserID string) map[string]any {
	if r.powerLevelsFunc != nil {
		return r.powerLevelsFunc(adminUserID)
	}
	return adminOnlyPowerLevels(adminUserID, r.memberSettableEventTypes)
}

var standardRooms = []standardRoom{
	{
		alias:         "bureau/system",
		displayName:   "Bureau System",
		topic:         "Operational messages",
		name:          "system room",
		credentialKey: "MATRIX_SYSTEM_ROOM",
		memberSettableEventTypes: []string{
			schema.EventTypeTokenSigningKey,
		},
	},
	{
		alias:         "bureau/machine",
		displayName:   "Bureau Machine",
		topic:         "Machine keys and status",
		name:          "machine room",
		credentialKey: "MATRIX_MACHINE_ROOM",
		memberSettableEventTypes: []string{
			schema.EventTypeMachineKey,
			schema.EventTypeMachineInfo,
			schema.EventTypeMachineStatus,
			schema.EventTypeWebRTCOffer,
			schema.EventTypeWebRTCAnswer,
		},
	},
	{
		alias:                    "bureau/service",
		displayName:              "Bureau Service",
		topic:                    "Service directory",
		name:                     "service room",
		credentialKey:            "MATRIX_SERVICE_ROOM",
		memberSettableEventTypes: []string{schema.EventTypeService},
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
// failures carry fix closures that executeFixes can invoke.
//
// When credentialFilePath is non-empty, credential mismatches are reported
// as fixable failures (the fix updates the credential file). When empty,
// credential mismatches remain warnings since there is no file to update.
func runDoctor(ctx context.Context, client *messaging.Client, session *messaging.Session, serverName string, storedCredentials map[string]string, credentialFilePath string, logger *slog.Logger) []checkResult {
	var results []checkResult

	// Section 1: Connectivity and authentication.
	versionsResult := checkServerVersions(ctx, client)
	results = append(results, versionsResult)

	if versionsResult.Status == statusFail {
		results = append(results, skip("authentication", "skipped: homeserver unreachable"))
		results = append(results, skip("bureau space", "skipped: homeserver unreachable"))
		for _, room := range standardRooms {
			results = append(results, skip(room.name, "skipped: homeserver unreachable"))
		}
		return results
	}

	authResult := checkAuth(ctx, session)
	results = append(results, authResult)

	if authResult.Status == statusFail {
		results = append(results, skip("bureau space", "skipped: authentication failed"))
		for _, room := range standardRooms {
			results = append(results, skip(room.name, "skipped: authentication failed"))
		}
		return results
	}

	// Section 2: Space and rooms exist.
	spaceAlias := fmt.Sprintf("#bureau:%s", serverName)
	spaceResult, spaceRoomID := checkRoomExists(ctx, session, "bureau space", spaceAlias)
	if spaceResult.Status == statusFail {
		spaceResult.FixHint = "create Bureau space"
		spaceResult.fix = func(ctx context.Context, session *messaging.Session) error {
			id, err := ensureSpace(ctx, session, serverName, logger)
			if err != nil {
				return err
			}
			spaceRoomID = id
			return nil
		}
	}
	results = append(results, spaceResult)

	roomIDs := make(map[string]string) // local alias → room ID
	for _, room := range standardRooms {
		room := room // capture for closure
		fullAlias := fmt.Sprintf("#%s:%s", room.alias, serverName)
		result, roomID := checkRoomExists(ctx, session, room.name, fullAlias)
		if result.Status == statusFail {
			result.FixHint = fmt.Sprintf("create %s", room.name)
			result.fix = func(ctx context.Context, session *messaging.Session) error {
				id, err := ensureRoom(ctx, session, room.alias, room.displayName, room.topic,
					spaceRoomID, serverName, room.powerLevels(session.UserID()), logger)
				if err != nil {
					return err
				}
				roomIDs[room.alias] = id
				return nil
			}
		}
		results = append(results, result)
		if roomID != "" {
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
		if spaceRoomID != "" {
			correctRoomIDs["MATRIX_SPACE_ROOM"] = spaceRoomID
		}
		for _, room := range standardRooms {
			if roomID, ok := roomIDs[room.alias]; ok {
				correctRoomIDs[room.credentialKey] = roomID
			}
		}

		var credentialFix fixAction
		if credentialFilePath != "" {
			credentialFix = func(ctx context.Context, session *messaging.Session) error {
				return cli.UpdateCredentialFile(credentialFilePath, correctRoomIDs)
			}
		}
		fixAttached := false

		if spaceRoomID != "" {
			result := checkCredentialMatch("bureau space", "MATRIX_SPACE_ROOM", spaceRoomID, storedCredentials)
			if credentialFix != nil && (result.Status == statusWarn || result.Status == statusFail) {
				result.Status = statusFail
				if !fixAttached {
					result.FixHint = "update credential file"
					result.fix = credentialFix
					fixAttached = true
				}
			}
			results = append(results, result)
		}
		for _, room := range standardRooms {
			if roomID, ok := roomIDs[room.alias]; ok {
				result := checkCredentialMatch(room.name, room.credentialKey, roomID, storedCredentials)
				if credentialFix != nil && (result.Status == statusWarn || result.Status == statusFail) {
					result.Status = statusFail
					if !fixAttached {
						result.FixHint = "update credential file"
						result.fix = credentialFix
						fixAttached = true
					}
				}
				results = append(results, result)
			}
		}
	}

	// Section 4: Space hierarchy — rooms are children of the space.
	if spaceRoomID != "" {
		spaceChildIDs, err := getSpaceChildIDs(ctx, session, spaceRoomID)
		if err != nil {
			results = append(results, fail("space hierarchy", fmt.Sprintf("cannot read space state: %v", err)))
		} else {
			for _, room := range standardRooms {
				roomID, ok := roomIDs[room.alias]
				if !ok {
					continue
				}
				result := checkSpaceChild(room.name, roomID, spaceChildIDs)
				if result.Status == statusFail {
					capturedRoomID := roomID
					capturedName := room.name
					result.FixHint = fmt.Sprintf("add %s as child of Bureau space", capturedName)
					result.fix = func(ctx context.Context, session *messaging.Session) error {
						_, err := session.SendStateEvent(ctx, spaceRoomID, "m.space.child", capturedRoomID,
							map[string]any{"via": []string{serverName}})
						return err
					}
				}
				results = append(results, result)
			}
		}
	}

	// Section 5: Power levels.
	adminUserID := session.UserID()
	if spaceRoomID != "" {
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
	if spaceRoomID != "" {
		results = append(results, checkJoinRules(ctx, session, "bureau space", spaceRoomID))
	}
	for _, room := range standardRooms {
		roomID, ok := roomIDs[room.alias]
		if !ok {
			continue
		}
		results = append(results, checkJoinRules(ctx, session, room.name, roomID))
	}

	// Section 7: Machine account membership.
	results = append(results, checkMachineMembership(ctx, session, serverName, roomIDs)...)

	// Section 8: Base templates published.
	if templateRoomID, ok := roomIDs["bureau/template"]; ok {
		results = append(results, checkBaseTemplates(ctx, session, templateRoomID)...)
	}

	// Section 9: Base pipelines published.
	if pipelineRoomID, ok := roomIDs["bureau/pipeline"]; ok {
		results = append(results, checkBasePipelines(ctx, session, pipelineRoomID)...)
	}

	return results
}

// checkServerVersions verifies the homeserver is reachable by calling the
// unauthenticated /_matrix/client/versions endpoint.
func checkServerVersions(ctx context.Context, client *messaging.Client) checkResult {
	versions, err := client.ServerVersions(ctx)
	if err != nil {
		return fail("homeserver", fmt.Sprintf("unreachable: %v", err))
	}
	return pass("homeserver", fmt.Sprintf("reachable, versions: %s", strings.Join(versions.Versions, ", ")))
}

// checkAuth verifies that the session's access token is valid.
func checkAuth(ctx context.Context, session *messaging.Session) checkResult {
	userID, err := session.WhoAmI(ctx)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeUnknownToken) {
			return fail("authentication", "access token is invalid or expired")
		}
		return fail("authentication", fmt.Sprintf("WhoAmI failed: %v", err))
	}

	if userID != session.UserID() {
		return warn("authentication", fmt.Sprintf("token valid but user ID mismatch: WhoAmI returned %q, session has %q", userID, session.UserID()))
	}

	return pass("authentication", fmt.Sprintf("authenticated as %s", userID))
}

// checkRoomExists resolves a room alias and returns the result plus the room ID
// (empty string if resolution failed).
func checkRoomExists(ctx context.Context, session *messaging.Session, name, alias string) (checkResult, string) {
	roomID, err := session.ResolveAlias(ctx, alias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return fail(name, fmt.Sprintf("alias %s not found", alias)), ""
		}
		return fail(name, fmt.Sprintf("resolve %s: %v", alias, err)), ""
	}
	return pass(name, fmt.Sprintf("%s → %s", alias, roomID)), roomID
}

// checkCredentialMatch verifies that a resolved room ID matches the value
// stored in the credential file.
func checkCredentialMatch(name, credentialKey, resolvedRoomID string, credentials map[string]string) checkResult {
	storedRoomID, ok := credentials[credentialKey]
	if !ok {
		return warn(name+" credential", fmt.Sprintf("%s not found in credential file", credentialKey))
	}
	if storedRoomID != resolvedRoomID {
		return fail(name+" credential", fmt.Sprintf("%s=%s but alias resolves to %s (credential file stale?)", credentialKey, storedRoomID, resolvedRoomID))
	}
	return pass(name+" credential", fmt.Sprintf("%s matches", credentialKey))
}

// getSpaceChildIDs reads m.space.child state events from a space and returns
// the set of child room IDs.
func getSpaceChildIDs(ctx context.Context, session *messaging.Session, spaceRoomID string) (map[string]bool, error) {
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
func checkSpaceChild(name, roomID string, spaceChildren map[string]bool) checkResult {
	if spaceChildren[roomID] {
		return pass(name+" in space", "listed as m.space.child")
	}
	return fail(name+" in space", fmt.Sprintf("%s is not a child of the Bureau space", roomID))
}

// checkPowerLevels reads the m.room.power_levels state event from a room and
// verifies: admin at 100, state_default at 100, and any expected member-settable
// event types at power level 0. The first failing sub-check gets a fix closure
// that resets the entire power levels state to expectedPowerLevels; subsequent
// sub-checks share that single fix.
func checkPowerLevels(ctx context.Context, session *messaging.Session, name, roomID, adminUserID string, memberSettableEventTypes []string, expectedPowerLevels map[string]any) []checkResult {
	var results []checkResult

	content, err := session.GetStateEvent(ctx, roomID, "m.room.power_levels", "")
	if err != nil {
		results = append(results, fail(name+" power levels", fmt.Sprintf("cannot read: %v", err)))
		return results
	}

	var powerLevels map[string]any
	if err := json.Unmarshal(content, &powerLevels); err != nil {
		results = append(results, fail(name+" power levels", fmt.Sprintf("invalid JSON: %v", err)))
		return results
	}

	// Build fix closure shared across all PL sub-checks for this room.
	// Only attached to the first failure — a single PL write fixes all issues.
	fixPowerLevels := func(ctx context.Context, session *messaging.Session) error {
		_, err := session.SendStateEvent(ctx, roomID, "m.room.power_levels", "",
			expectedPowerLevels)
		return err
	}
	fixAttached := false
	attachFix := func(result *checkResult) {
		if result.Status == statusFail && !fixAttached {
			result.FixHint = fmt.Sprintf("reset %s power levels", name)
			result.fix = fixPowerLevels
			fixAttached = true
		}
	}

	// Check admin user power level.
	adminLevel := getUserPowerLevel(powerLevels, adminUserID)
	if adminLevel == 100 {
		results = append(results, pass(name+" admin power", fmt.Sprintf("%s has power level 100", adminUserID)))
	} else {
		result := fail(name+" admin power", fmt.Sprintf("%s has power level %.0f, expected 100", adminUserID, adminLevel))
		attachFix(&result)
		results = append(results, result)
	}

	// Check state_default (should be 100 — only admin can set arbitrary state).
	stateDefault := getNumericField(powerLevels, "state_default")
	if stateDefault == 100 {
		results = append(results, pass(name+" state_default", "state_default is 100"))
	} else {
		result := fail(name+" state_default", fmt.Sprintf("state_default is %.0f, expected 100", stateDefault))
		attachFix(&result)
		results = append(results, result)
	}

	// Check that each member-settable event type is at power level 0.
	for _, eventType := range memberSettableEventTypes {
		level := getEventPowerLevel(powerLevels, eventType)
		if level == 0 {
			results = append(results, pass(name+" "+eventType, fmt.Sprintf("members can set %s (level 0)", eventType)))
		} else {
			result := fail(name+" "+eventType, fmt.Sprintf("%s requires power level %.0f, expected 0", eventType, level))
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

// getEventPowerLevel extracts the power level required to send a specific event type.
// Returns events_default if the event type is not in the events map.
func getEventPowerLevel(powerLevels map[string]any, eventType string) float64 {
	eventsDefault := getNumericField(powerLevels, "events_default")

	events, ok := powerLevels["events"].(map[string]any)
	if !ok {
		return eventsDefault
	}

	level, ok := events[eventType].(float64)
	if !ok {
		return eventsDefault
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
func checkJoinRules(ctx context.Context, session *messaging.Session, name, roomID string) checkResult {
	content, err := session.GetStateEvent(ctx, roomID, "m.room.join_rules", "")
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			capturedRoomID := roomID
			return failWithFix(
				name+" join rules",
				"join_rules state not found",
				fmt.Sprintf("set %s join_rule to invite", name),
				func(ctx context.Context, session *messaging.Session) error {
					_, err := session.SendStateEvent(ctx, capturedRoomID, "m.room.join_rules", "",
						map[string]any{"join_rule": "invite"})
					return err
				},
			)
		}
		return fail(name+" join rules", fmt.Sprintf("cannot read join rules: %v", err))
	}

	var joinRules map[string]any
	if err := json.Unmarshal(content, &joinRules); err != nil {
		return fail(name+" join rules", fmt.Sprintf("invalid join rules JSON: %v", err))
	}

	joinRule, _ := joinRules["join_rule"].(string)
	if joinRule == "invite" {
		return pass(name+" join rules", "join_rule is invite")
	}

	capturedRoomID := roomID
	return failWithFix(
		name+" join rules",
		fmt.Sprintf("join_rule is %q, expected invite", joinRule),
		fmt.Sprintf("set %s join_rule to invite", name),
		func(ctx context.Context, session *messaging.Session) error {
			_, err := session.SendStateEvent(ctx, capturedRoomID, "m.room.join_rules", "",
				map[string]any{"join_rule": "invite"})
			return err
		},
	)
}

// checkMachineMembership verifies that active machine accounts (those with
// non-empty m.bureau.machine_key state in the machine room) are members of
// the service room. Machines don't need to be in the system room — they
// receive instructions through per-machine config rooms, not system room
// membership. Missing memberships are fixable via invite.
//
// The state key for machine_key events is the machine's localpart (e.g.,
// "machine/worker-01"), not the full Matrix user ID. This function
// constructs the full user ID using the server name for membership checks
// and invite fixes.
//
// Decommissioned machines have their machine_key content cleared to {}.
// These are skipped — only machines with a non-empty "key" field are
// considered active.
func checkMachineMembership(ctx context.Context, session *messaging.Session, serverName string, roomIDs map[string]string) []checkResult {
	machineRoomID, ok := roomIDs["bureau/machine"]
	if !ok {
		return nil
	}

	// Find active machine users from m.bureau.machine_key state events.
	// The state key is the machine's localpart. Skip events with empty
	// content (decommissioned machines).
	events, err := session.GetRoomState(ctx, machineRoomID)
	if err != nil {
		return []checkResult{fail("machine membership", fmt.Sprintf("cannot read machine room state: %v", err))}
	}

	type machineEntry struct {
		localpart string
		userID    string
	}
	var machines []machineEntry
	for _, event := range events {
		if event.Type != schema.EventTypeMachineKey || event.StateKey == nil || *event.StateKey == "" {
			continue
		}
		// Decommissioned machines have their key cleared to empty content.
		// Only consider machines with a non-empty "key" field as active.
		if keyValue, _ := event.Content["key"].(string); keyValue == "" {
			continue
		}
		localpart := *event.StateKey
		userID := fmt.Sprintf("@%s:%s", localpart, serverName)
		machines = append(machines, machineEntry{localpart: localpart, userID: userID})
	}

	if len(machines) == 0 {
		return nil
	}

	var results []checkResult

	serviceRoomID, ok := roomIDs["bureau/service"]
	if !ok {
		return nil
	}

	members, err := session.GetRoomMembers(ctx, serviceRoomID)
	if err != nil {
		return []checkResult{fail("service room membership", fmt.Sprintf("cannot read members: %v", err))}
	}

	memberSet := make(map[string]bool)
	for _, member := range members {
		if member.Membership == "join" {
			memberSet[member.UserID] = true
		}
	}

	for _, machine := range machines {
		checkName := fmt.Sprintf("%s in service room", machine.userID)
		if memberSet[machine.userID] {
			results = append(results, pass(checkName, "member"))
		} else {
			capturedUserID := machine.userID
			results = append(results, failWithFix(
				checkName,
				"not a member of service room",
				fmt.Sprintf("invite %s to service room", capturedUserID),
				func(ctx context.Context, session *messaging.Session) error {
					return session.InviteUser(ctx, serviceRoomID, capturedUserID)
				},
			))
		}
	}

	return results
}

// stateEventItem describes a single state event that should be published
// in a Matrix room. Used by checkPublishedStateEvents to verify presence
// and offer a fix action if missing.
type stateEventItem struct {
	label     string // human-readable label (e.g., "template", "pipeline")
	name      string // state key
	eventType string // Matrix event type
	content   any    // value to publish on fix
}

// checkPublishedStateEvents verifies that each item in the list is present
// as a state event in the given room. Missing items produce a fixable failure
// that re-publishes the expected content.
func checkPublishedStateEvents(ctx context.Context, session *messaging.Session, roomID string, items []stateEventItem) []checkResult {
	var results []checkResult

	for _, item := range items {
		checkName := fmt.Sprintf("%s %q", item.label, item.name)
		_, err := session.GetStateEvent(ctx, roomID, item.eventType, item.name)
		if err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				capturedItem := item
				capturedRoomID := roomID
				results = append(results, failWithFix(
					checkName,
					fmt.Sprintf("%s %q not published", item.label, item.name),
					fmt.Sprintf("publish %s %q", item.label, item.name),
					func(ctx context.Context, session *messaging.Session) error {
						_, err := session.SendStateEvent(ctx, capturedRoomID, capturedItem.eventType,
							capturedItem.name, capturedItem.content)
						return err
					},
				))
				continue
			}
			results = append(results, fail(checkName, fmt.Sprintf("cannot read: %v", err)))
			continue
		}
		results = append(results, pass(checkName, "published"))
	}

	return results
}

// checkBaseTemplates verifies that the standard Bureau templates ("base" and
// "base-networked") are published as m.bureau.template state events in the
// template room. Missing templates are fixable by re-publishing them.
func checkBaseTemplates(ctx context.Context, session *messaging.Session, templateRoomID string) []checkResult {
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
func checkBasePipelines(ctx context.Context, session *messaging.Session, pipelineRoomID string) []checkResult {
	pipelines, err := content.Pipelines()
	if err != nil {
		return []checkResult{fail("pipeline content", fmt.Sprintf("cannot load embedded pipelines: %v", err))}
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

// executeFixes runs the fix action for each fixable failure, updating
// results in place. Returns the number of successfully applied fixes.
// In dry-run mode, no fixes are executed and 0 is returned.
func executeFixes(ctx context.Context, session *messaging.Session, results []checkResult, dryRun bool) int {
	if dryRun {
		return 0
	}
	fixedCount := 0
	for i := range results {
		if results[i].Status != statusFail || results[i].fix == nil {
			continue
		}
		if err := results[i].fix(ctx, session); err != nil {
			results[i].Message = fmt.Sprintf("%s (fix failed: %v)", results[i].Message, err)
		} else {
			results[i].Status = statusFixed
			fixedCount++
		}
	}
	return fixedCount
}

// printChecklist prints check results as a human-readable checklist.
func printChecklist(results []checkResult, fixMode, dryRun bool) error {
	anyFailed := false
	fixableCount := 0
	fixedCount := 0

	for _, result := range results {
		prefix := strings.ToUpper(string(result.Status))
		fmt.Fprintf(os.Stdout, "[%-5s]  %-40s  %s\n", prefix, result.Name, result.Message)

		switch result.Status {
		case statusFail:
			anyFailed = true
			if result.FixHint != "" {
				fixableCount++
				if dryRun {
					fmt.Fprintf(os.Stdout, "         %-40s  would fix: %s\n", "", result.FixHint)
				}
			}
		case statusFixed:
			fixedCount++
		}
	}

	fmt.Fprintln(os.Stdout)

	if anyFailed {
		if dryRun && fixableCount > 0 {
			fmt.Fprintf(os.Stdout, "%d issue(s) would be repaired. Run without --dry-run to apply.\n", fixableCount)
		} else if !fixMode && fixableCount > 0 {
			fmt.Fprintf(os.Stdout, "Run with --fix to repair %d issue(s).\n", fixableCount)
		} else {
			fmt.Fprintln(os.Stdout, "Some checks failed.")
		}
		return &cli.ExitError{Code: 1}
	}

	if fixedCount > 0 {
		fmt.Fprintf(os.Stdout, "%d issue(s) repaired.\n", fixedCount)
		return nil
	}

	fmt.Fprintln(os.Stdout, "All checks passed.")
	return nil
}

// doctorJSONOutput is the JSON output structure for the doctor command.
type doctorJSONOutput struct {
	Checks []checkResult `json:"checks"`
	OK     bool          `json:"ok"`
	DryRun bool          `json:"dry_run,omitempty"`
}

// doctorJSON builds the JSON output struct for doctor results.
func doctorJSON(results []checkResult, dryRun bool) doctorJSONOutput {
	anyFailed := false
	for _, result := range results {
		if result.Status == statusFail {
			anyFailed = true
			break
		}
	}
	return doctorJSONOutput{
		Checks: results,
		OK:     !anyFailed,
		DryRun: dryRun,
	}
}
