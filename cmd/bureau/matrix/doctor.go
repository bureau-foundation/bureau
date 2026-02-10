// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/messaging"
)

// DoctorCommand returns the "doctor" subcommand for checking Bureau Matrix
// infrastructure health.
func DoctorCommand() *cli.Command {
	var (
		session    SessionConfig
		serverName string
		jsonOutput bool
	)

	return &cli.Command{
		Name:    "doctor",
		Summary: "Check Bureau Matrix infrastructure health",
		Description: `Validate the Bureau Matrix homeserver state: reachability, authentication,
spaces, rooms, hierarchy, and power levels.

Runs a series of checks against the expected state created by "bureau matrix setup"
and reports pass/fail/warn for each. Exits with code 1 if any check fails.

Use --json for machine-readable output suitable for monitoring or CI.`,
		Usage: "bureau matrix doctor [flags]",
		Examples: []cli.Example{
			{
				Description: "Check with credential file",
				Command:     "bureau matrix doctor --credential-file ./creds",
			},
			{
				Description: "Check with explicit flags",
				Command:     "bureau matrix doctor --homeserver http://localhost:6167 --token syt_... --user-id @bureau-admin:bureau.local",
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
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			// Resolve homeserver URL from flags or credential file, before
			// creating the session, so we can do an unauthenticated reachability
			// check first.
			homeserverURL, err := session.resolveHomeserverURL()
			if err != nil {
				return err
			}

			// Load credential file for cross-referencing room IDs.
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

			results := runDoctor(ctx, client, sess, serverName, storedCredentials)

			if jsonOutput {
				return printDoctorJSON(results)
			}
			return printChecklist(results)
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

// checkStatus is the outcome of a single health check.
type checkStatus string

const (
	statusPass checkStatus = "pass"
	statusFail checkStatus = "fail"
	statusWarn checkStatus = "warn"
	statusSkip checkStatus = "skip"
)

// checkResult holds the outcome of a single health check.
type checkResult struct {
	Name    string      `json:"name"`
	Status  checkStatus `json:"status"`
	Message string      `json:"message"`
}

func pass(name, message string) checkResult {
	return checkResult{Name: name, Status: statusPass, Message: message}
}

func fail(name, message string) checkResult {
	return checkResult{Name: name, Status: statusFail, Message: message}
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
	name                     string   // human name for check output
	credentialKey            string   // key in credential file (e.g., "MATRIX_MACHINES_ROOM")
	memberSettableEventTypes []string // event types that members should be able to set
}

var standardRooms = []standardRoom{
	{alias: "bureau/system", name: "system room", credentialKey: "MATRIX_SYSTEM_ROOM"},
	{alias: "bureau/machines", name: "machines room", credentialKey: "MATRIX_MACHINES_ROOM", memberSettableEventTypes: []string{"m.bureau.machine_key", "m.bureau.machine_status"}},
	{alias: "bureau/services", name: "services room", credentialKey: "MATRIX_SERVICES_ROOM", memberSettableEventTypes: []string{"m.bureau.service"}},
}

// runDoctor executes all health checks and returns the results.
func runDoctor(ctx context.Context, client *messaging.Client, session *messaging.Session, serverName string, storedCredentials map[string]string) []checkResult {
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
	results = append(results, spaceResult)

	roomIDs := make(map[string]string) // local alias → room ID
	for _, room := range standardRooms {
		fullAlias := fmt.Sprintf("#%s:%s", room.alias, serverName)
		result, roomID := checkRoomExists(ctx, session, room.name, fullAlias)
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
					continue // room doesn't exist; already reported
				}
				results = append(results, checkSpaceChild(room.name, roomID, spaceChildIDs))
			}
		}
	}

	// Section 5: Power levels.
	if spaceRoomID != "" {
		results = append(results, checkPowerLevels(ctx, session, "bureau space", spaceRoomID, session.UserID(), nil)...)
	}
	for _, room := range standardRooms {
		roomID, ok := roomIDs[room.alias]
		if !ok {
			continue
		}
		results = append(results, checkPowerLevels(ctx, session, room.name, roomID, session.UserID(), room.memberSettableEventTypes)...)
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
// event types at power level 0.
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

	// Check admin user power level.
	adminLevel := getUserPowerLevel(powerLevels, adminUserID)
	if adminLevel == 100 {
		results = append(results, pass(name+" admin power", fmt.Sprintf("%s has power level 100", adminUserID)))
	} else {
		results = append(results, fail(name+" admin power", fmt.Sprintf("%s has power level %.0f, expected 100", adminUserID, adminLevel)))
	}

	// Check state_default (should be 100 — only admin can set arbitrary state).
	stateDefault := getNumericField(powerLevels, "state_default")
	if stateDefault == 100 {
		results = append(results, pass(name+" state_default", "state_default is 100"))
	} else {
		results = append(results, fail(name+" state_default", fmt.Sprintf("state_default is %.0f, expected 100", stateDefault)))
	}

	// Check that each member-settable event type is at power level 0.
	for _, eventType := range memberSettableEventTypes {
		level := getEventPowerLevel(powerLevels, eventType)
		if level == 0 {
			results = append(results, pass(name+" "+eventType, fmt.Sprintf("members can set %s (level 0)", eventType)))
		} else {
			results = append(results, fail(name+" "+eventType, fmt.Sprintf("%s requires power level %.0f, expected 0", eventType, level)))
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

// printChecklist prints check results as a human-readable checklist.
func printChecklist(results []checkResult) error {
	anyFailed := false

	for _, result := range results {
		var prefix string
		switch result.Status {
		case statusPass:
			prefix = "PASS"
		case statusFail:
			prefix = "FAIL"
			anyFailed = true
		case statusWarn:
			prefix = "WARN"
		case statusSkip:
			prefix = "SKIP"
		}
		fmt.Fprintf(os.Stdout, "[%s]  %-30s  %s\n", prefix, result.Name, result.Message)
	}

	fmt.Fprintln(os.Stdout)
	if anyFailed {
		fmt.Fprintln(os.Stdout, "Some checks failed. Run 'bureau matrix setup' to fix.")
		return &exitError{code: 1}
	}

	fmt.Fprintln(os.Stdout, "All checks passed.")
	return nil
}

// printDoctorJSON prints check results as JSON. Named differently from
// printJSON in state.go to avoid a collision within the same package.
func printDoctorJSON(results []checkResult) error {
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
	}{
		Checks: results,
		OK:     !anyFailed,
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
