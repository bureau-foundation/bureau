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

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// DoctorCommand returns the "doctor" subcommand for checking Bureau Matrix
// infrastructure health and optionally repairing fixable issues.
func DoctorCommand() *cli.Command {
	var (
		session    SessionConfig
		serverName string
		jsonOutput bool
		fix        bool
		dryRun     bool
	)

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
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("doctor", pflag.ContinueOnError)
			session.AddFlags(flagSet)
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name for constructing aliases")
			flagSet.BoolVar(&jsonOutput, "json", false, "machine-readable JSON output")
			flagSet.BoolVar(&fix, "fix", false, "automatically repair fixable issues")
			flagSet.BoolVar(&dryRun, "dry-run", false, "preview repairs without executing (requires --fix)")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}
			if dryRun && !fix {
				return fmt.Errorf("--dry-run requires --fix")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			homeserverURL, err := session.resolveHomeserverURL()
			if err != nil {
				return err
			}

			var storedCredentials map[string]string
			if session.CredentialFile != "" {
				storedCredentials, err = readCredentialFile(session.CredentialFile)
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

			sess, err := session.Connect(ctx)
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}

			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
				Level: slog.LevelInfo,
			}))

			results := runDoctor(ctx, client, sess, serverName, storedCredentials, logger)

			if fix {
				executeFixes(ctx, sess, results, dryRun)
			}

			if jsonOutput {
				return printDoctorJSON(results, fix, dryRun)
			}
			return printChecklist(results, fix, dryRun)
		},
	}
}

// resolveHomeserverURL extracts the homeserver URL from flags or credential file
// without creating a full session. Used by doctor to perform an unauthenticated
// reachability check before attempting authentication.
func (c *SessionConfig) resolveHomeserverURL() (string, error) {
	if c.HomeserverURL != "" {
		return c.HomeserverURL, nil
	}
	if c.CredentialFile != "" {
		creds, err := readCredentialFile(c.CredentialFile)
		if err != nil {
			return "", fmt.Errorf("read credential file: %w", err)
		}
		if url, ok := creds["MATRIX_HOMESERVER_URL"]; ok && url != "" {
			return url, nil
		}
		return "", fmt.Errorf("credential file missing MATRIX_HOMESERVER_URL")
	}
	return "", fmt.Errorf("--homeserver or --credential-file is required")
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
	alias                    string   // local alias (e.g., "bureau/machines")
	displayName              string   // room name for CreateRoom (e.g., "Bureau Machines")
	topic                    string   // room topic for CreateRoom
	name                     string   // human name for check output
	credentialKey            string   // key in credential file (e.g., "MATRIX_MACHINES_ROOM")
	memberSettableEventTypes []string // event types that members should be able to set
}

var standardRooms = []standardRoom{
	{
		alias:         "bureau/system",
		displayName:   "Bureau System",
		topic:         "Operational messages",
		name:          "system room",
		credentialKey: "MATRIX_SYSTEM_ROOM",
	},
	{
		alias:                    "bureau/machines",
		displayName:              "Bureau Machines",
		topic:                    "Machine keys and status",
		name:                     "machines room",
		credentialKey:            "MATRIX_MACHINES_ROOM",
		memberSettableEventTypes: []string{schema.EventTypeMachineKey, schema.EventTypeMachineStatus},
	},
	{
		alias:                    "bureau/services",
		displayName:              "Bureau Services",
		topic:                    "Service directory",
		name:                     "services room",
		credentialKey:            "MATRIX_SERVICES_ROOM",
		memberSettableEventTypes: []string{schema.EventTypeService},
	},
}

// runDoctor executes all health checks and returns the results. Fixable
// failures carry fix closures that executeFixes can invoke.
func runDoctor(ctx context.Context, client *messaging.Client, session *messaging.Session, serverName string, storedCredentials map[string]string, logger *slog.Logger) []checkResult {
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
					spaceRoomID, serverName, room.memberSettableEventTypes, logger)
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

	// Section 3: Credential file cross-reference.
	if storedCredentials != nil {
		if spaceRoomID != "" {
			results = append(results, checkCredentialMatch("bureau space", "MATRIX_SPACE_ROOM", spaceRoomID, storedCredentials))
		}
		for _, room := range standardRooms {
			if roomID, ok := roomIDs[room.alias]; ok {
				results = append(results, checkCredentialMatch(room.name, room.credentialKey, roomID, storedCredentials))
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
		results = append(results, checkPowerLevels(ctx, session, "bureau space", spaceRoomID, adminUserID, nil)...)
	}
	for _, room := range standardRooms {
		roomID, ok := roomIDs[room.alias]
		if !ok {
			continue
		}
		results = append(results, checkPowerLevels(ctx, session, room.name, roomID, adminUserID, room.memberSettableEventTypes)...)
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
	results = append(results, checkMachineMembership(ctx, session, roomIDs)...)

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
// that resets the entire power levels state to the standard Bureau configuration;
// subsequent sub-checks share that single fix.
func checkPowerLevels(ctx context.Context, session *messaging.Session, name, roomID, adminUserID string, memberSettableEventTypes []string) []checkResult {
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
			adminOnlyPowerLevels(adminUserID, memberSettableEventTypes))
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

// checkMachineMembership verifies that machine accounts (those with
// m.bureau.machine_key state in the machines room) are members of the
// system and services rooms. Missing memberships are fixable via invite.
func checkMachineMembership(ctx context.Context, session *messaging.Session, roomIDs map[string]string) []checkResult {
	machinesRoomID, ok := roomIDs["bureau/machines"]
	if !ok {
		return nil
	}

	// Find machine users from m.bureau.machine_key state events.
	events, err := session.GetRoomState(ctx, machinesRoomID)
	if err != nil {
		return []checkResult{fail("machine membership", fmt.Sprintf("cannot read machines room state: %v", err))}
	}

	var machineUsers []string
	for _, event := range events {
		if event.Type == schema.EventTypeMachineKey && event.StateKey != nil && *event.StateKey != "" {
			machineUsers = append(machineUsers, *event.StateKey)
		}
	}

	if len(machineUsers) == 0 {
		return nil
	}

	var results []checkResult
	checkRooms := []struct {
		alias string
		name  string
	}{
		{"bureau/system", "system room"},
		{"bureau/services", "services room"},
	}

	for _, checkRoom := range checkRooms {
		roomID, ok := roomIDs[checkRoom.alias]
		if !ok {
			continue
		}

		members, err := session.GetRoomMembers(ctx, roomID)
		if err != nil {
			results = append(results, fail(
				checkRoom.name+" membership",
				fmt.Sprintf("cannot read members: %v", err),
			))
			continue
		}

		memberSet := make(map[string]bool)
		for _, member := range members {
			if member.Membership == "join" {
				memberSet[member.UserID] = true
			}
		}

		for _, machineUser := range machineUsers {
			checkName := fmt.Sprintf("%s in %s", machineUser, checkRoom.name)
			if memberSet[machineUser] {
				results = append(results, pass(checkName, "member"))
			} else {
				capturedRoomID := roomID
				capturedUser := machineUser
				capturedRoomName := checkRoom.name
				results = append(results, failWithFix(
					checkName,
					fmt.Sprintf("not a member of %s", capturedRoomName),
					fmt.Sprintf("invite %s to %s", capturedUser, capturedRoomName),
					func(ctx context.Context, session *messaging.Session) error {
						return session.InviteUser(ctx, capturedRoomID, capturedUser)
					},
				))
			}
		}
	}

	return results
}

// executeFixes runs the fix action for each fixable failure, updating
// results in place. In dry-run mode, no fixes are executed.
func executeFixes(ctx context.Context, session *messaging.Session, results []checkResult, dryRun bool) {
	if dryRun {
		return
	}
	for i := range results {
		if results[i].Status != statusFail || results[i].fix == nil {
			continue
		}
		if err := results[i].fix(ctx, session); err != nil {
			results[i].Message = fmt.Sprintf("%s (fix failed: %v)", results[i].Message, err)
		} else {
			results[i].Status = statusFixed
		}
	}
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
		return &exitError{code: 1}
	}

	if fixedCount > 0 {
		fmt.Fprintf(os.Stdout, "%d issue(s) repaired.\n", fixedCount)
		return nil
	}

	fmt.Fprintln(os.Stdout, "All checks passed.")
	return nil
}

// printDoctorJSON prints check results as JSON. Named differently from
// printJSON in state.go to avoid a collision within the same package.
func printDoctorJSON(results []checkResult, fixMode, dryRun bool) error {
	anyFailed := false
	for _, result := range results {
		if result.Status == statusFail {
			anyFailed = true
			break
		}
	}

	output := struct {
		Checks []checkResult `json:"checks"`
		OK     bool          `json:"ok"`
		DryRun bool          `json:"dry_run,omitempty"`
	}{
		Checks: results,
		OK:     !anyFailed,
		DryRun: dryRun,
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	fmt.Fprintln(os.Stdout, string(data))

	if anyFailed {
		return &exitError{code: 1}
	}
	return nil
}

// exitError signals a non-zero exit code without printing an extra error
// message. The CLI framework can check for this via ExitCode() to distinguish
// "command failed with output already printed" from "command failed with an
// error to display".
type exitError struct {
	code int
}

func (e *exitError) Error() string {
	return fmt.Sprintf("exit code %d", e.code)
}

// ExitCode returns the exit code. When the CLI framework encounters an error
// implementing this interface, it exits with the returned code without printing
// the error message (since the command already printed its own output).
func (e *exitError) ExitCode() int {
	return e.code
}

// Ensure exitError implements the error interface at compile time.
var _ error = (*exitError)(nil)
