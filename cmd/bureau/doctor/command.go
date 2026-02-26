// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/cli/doctor"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// commandParams holds the parameters for the top-level doctor command.
type commandParams struct {
	cli.JSONOutput
}

// Command returns the top-level "bureau doctor" command for diagnosing
// the operator's environment.
func Command() *cli.Command {
	var params commandParams

	return &cli.Command{
		Name:    "doctor",
		Summary: "Diagnose the operator environment end-to-end",
		Description: `Check the full Bureau operator experience: session, machine configuration,
homeserver reachability, local services, socket connectivity, and fleet
access. Requires no flags — discovers everything automatically and reports
what's working and what's broken.

For each failure, prints the specific command to fix it. This is the
"I'm lost" command for operators who don't know where to start.

For domain-specific deep checks and repairs:
  bureau machine doctor    System user, directories, binaries, systemd units
  bureau matrix doctor     Rooms, power levels, templates, pipelines`,
		Usage: "bureau doctor [flags]",
		Examples: []cli.Example{
			{
				Description: "Check operator environment health",
				Command:     "bureau doctor",
			},
			{
				Description: "Machine-readable output",
				Command:     "bureau doctor --json",
			},
		},
		Annotations: cli.ReadOnly(),
		Output:      func() any { return &doctor.JSONOutput{} },
		Params:      func() any { return &params },
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}

			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			return runDoctor(ctx, params, logger)
		},
	}
}

// checkState accumulates discovered state across checks so later checks
// can use results from earlier ones without repeating work.
type checkState struct {
	// operatorSession is set when the operator session loads successfully.
	operatorSession *cli.OperatorSession

	// homeserverURL is the discovered homeserver URL (from operator
	// session or machine.conf).
	homeserverURL string

	// serverName is the Matrix server name extracted from the operator's
	// user ID (e.g., "bureau.local").
	serverName string

	// machineConfig holds parsed values from /etc/bureau/machine.conf.
	machineConfig map[string]string

	// session is an authenticated Matrix session for fleet checks.
	session messaging.Session

	// servicesRunning is true if both launcher and daemon are active.
	servicesRunning bool
}

func runDoctor(ctx context.Context, params commandParams, logger *slog.Logger) error {
	var state checkState
	var results []doctor.Result

	// Section 1: Operator session.
	results = append(results, checkOperatorSession(ctx, &state, logger)...)

	// Section 2: Machine configuration.
	results = append(results, checkMachineConfiguration(&state)...)

	// Section 3: Homeserver reachability.
	results = append(results, checkHomeserver(ctx, &state)...)

	// Section 4: Local services.
	results = append(results, checkLocalServices(&state)...)

	// Section 5: Socket connectivity.
	results = append(results, checkLocalSockets(&state)...)

	// Section 6: Bureau space and fleet access.
	results = append(results, checkBureauSpace(ctx, &state)...)

	// Close the session if we opened one.
	if state.session != nil {
		state.session.Close()
	}

	// Produce output.
	outcome := doctor.Outcome{} // no fixes, no elevated skips
	if done, err := params.EmitJSON(doctor.BuildJSON(results, false, outcome)); done {
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

	checklistError := doctor.PrintChecklist(results, false, false, outcome)

	// Print guidance section after the checklist.
	printGuidance(results)

	return checklistError
}

// --- Section 1: Operator session ---

func checkOperatorSession(ctx context.Context, state *checkState, logger *slog.Logger) []doctor.Result {
	sessionPath := cli.SessionFilePath()

	operatorSession, err := cli.LoadSession()
	if err != nil {
		return []doctor.Result{doctor.Fail("operator session",
			fmt.Sprintf("no valid session at %s — run \"bureau login <username>\"", sessionPath))}
	}

	state.operatorSession = operatorSession
	state.homeserverURL = operatorSession.Homeserver

	// Extract server name from user ID for later use.
	userID, parseError := ref.ParseUserID(operatorSession.UserID)
	if parseError != nil {
		return []doctor.Result{doctor.Fail("operator session",
			fmt.Sprintf("session has invalid user_id %q — run \"bureau login <username>\"", operatorSession.UserID))}
	}
	state.serverName = userID.Server().String()

	// Verify the session is still valid via WhoAmI.
	client, clientError := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: operatorSession.Homeserver,
	})
	if clientError != nil {
		return []doctor.Result{doctor.Fail("operator session",
			fmt.Sprintf("cannot create client for %s: %v", operatorSession.Homeserver, clientError))}
	}

	session, sessionError := client.SessionFromToken(userID, operatorSession.AccessToken)
	if sessionError != nil {
		return []doctor.Result{doctor.Fail("operator session",
			fmt.Sprintf("cannot create session: %v — run \"bureau login <username>\"", sessionError))}
	}

	whoami, whoamiError := session.WhoAmI(ctx)
	if whoamiError != nil {
		session.Close()
		if messaging.IsMatrixError(whoamiError, messaging.ErrCodeUnknownToken) {
			return []doctor.Result{doctor.Fail("operator session",
				fmt.Sprintf("session expired at %s — run \"bureau login <username>\"", sessionPath))}
		}
		// WhoAmI failed for a non-auth reason. The session might still be
		// valid but the homeserver is unreachable. Report what we know and
		// let the homeserver check provide more detail.
		return []doctor.Result{doctor.Warn("operator session",
			fmt.Sprintf("loaded from %s as %s but WhoAmI failed: %v", sessionPath, operatorSession.UserID, whoamiError))}
	}

	// Keep the session for fleet checks.
	state.session = session

	return []doctor.Result{doctor.Pass("operator session",
		fmt.Sprintf("authenticated as %s", whoami))}
}

// --- Section 2: Machine configuration ---

// machineConfPath is the canonical location for machine configuration.
// Variable rather than constant to allow test overrides.
var machineConfPath = "/etc/bureau/machine.conf"

// machineConfRequiredKeys are the keys that must be present in machine.conf.
var machineConfRequiredKeys = []string{
	"BUREAU_HOMESERVER_URL",
	"BUREAU_MACHINE_NAME",
	"BUREAU_SERVER_NAME",
	"BUREAU_FLEET",
}

func checkMachineConfiguration(state *checkState) []doctor.Result {
	credentials, err := cli.ReadCredentialFile(machineConfPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []doctor.Result{doctor.Fail("machine configuration",
				fmt.Sprintf("%s does not exist — run \"sudo bureau machine doctor --fix\" with bootstrap flags", machineConfPath))}
		}
		return []doctor.Result{doctor.Fail("machine configuration",
			fmt.Sprintf("cannot read %s: %v", machineConfPath, err))}
	}

	state.machineConfig = credentials

	// If we didn't get a homeserver URL from the operator session, fall
	// back to machine.conf.
	if state.homeserverURL == "" {
		state.homeserverURL = credentials["BUREAU_HOMESERVER_URL"]
	}
	if state.serverName == "" {
		state.serverName = credentials["BUREAU_SERVER_NAME"]
	}

	var missingKeys []string
	for _, key := range machineConfRequiredKeys {
		if credentials[key] == "" {
			missingKeys = append(missingKeys, key)
		}
	}

	if len(missingKeys) > 0 {
		return []doctor.Result{doctor.Fail("machine configuration",
			fmt.Sprintf("missing keys in %s: %s", machineConfPath, strings.Join(missingKeys, ", ")))}
	}

	fleet := credentials["BUREAU_FLEET"]
	machine := credentials["BUREAU_MACHINE_NAME"]
	return []doctor.Result{doctor.Pass("machine configuration",
		fmt.Sprintf("fleet=%s machine=%s", fleet, machine))}
}

// --- Section 3: Homeserver reachability ---

func checkHomeserver(ctx context.Context, state *checkState) []doctor.Result {
	if state.homeserverURL == "" {
		return []doctor.Result{doctor.Skip("homeserver reachable",
			"skipped: no homeserver URL discovered (no operator session or machine.conf)")}
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: state.homeserverURL,
	})
	if err != nil {
		return []doctor.Result{doctor.Fail("homeserver reachable",
			fmt.Sprintf("cannot create client for %s: %v", state.homeserverURL, err))}
	}

	versions, err := client.ServerVersions(ctx)
	if err != nil {
		return []doctor.Result{doctor.Fail("homeserver reachable",
			fmt.Sprintf("%s unreachable: %v", state.homeserverURL, err))}
	}

	return []doctor.Result{doctor.Pass("homeserver reachable",
		fmt.Sprintf("%s (versions: %s)", state.homeserverURL, strings.Join(versions.Versions, ", ")))}
}

// --- Section 4: Local services ---

func checkLocalServices(state *checkState) []doctor.Result {
	var results []doctor.Result

	services := []string{"bureau-launcher", "bureau-daemon"}
	allRunning := true

	for _, service := range services {
		output, err := exec.Command("systemctl", "is-active", service+".service").Output()
		status := strings.TrimSpace(string(output))
		if err != nil || (status != "active" && status != "activating") {
			allRunning = false
			if status == "" {
				status = "unknown"
			}
			results = append(results, doctor.Fail(service,
				fmt.Sprintf("not running (status: %s) — run \"sudo bureau machine doctor --fix\"", status)))
		} else {
			results = append(results, doctor.Pass(service, "running"))
		}
	}

	state.servicesRunning = allRunning
	return results
}

// --- Section 5: Socket connectivity ---

func checkLocalSockets(state *checkState) []doctor.Result {
	if !state.servicesRunning {
		return []doctor.Result{
			doctor.Skip("launcher.sock", "skipped: services not running"),
			doctor.Skip("observe.sock", "skipped: services not running"),
		}
	}

	sockets := []struct {
		name string
		path string
	}{
		{"launcher.sock", "/run/bureau/launcher.sock"},
		{"observe.sock", "/run/bureau/observe.sock"},
	}

	var results []doctor.Result
	for _, socket := range sockets {
		info, err := os.Stat(socket.path)
		if err != nil {
			results = append(results, doctor.Fail(socket.name,
				fmt.Sprintf("%s: %v — run \"sudo bureau machine doctor --fix\"", socket.path, err)))
			continue
		}
		if info.Mode().Type()&os.ModeSocket == 0 {
			results = append(results, doctor.Fail(socket.name,
				fmt.Sprintf("%s exists but is not a socket", socket.path)))
			continue
		}

		conn, err := net.DialTimeout("unix", socket.path, 2*time.Second)
		if err != nil {
			results = append(results, doctor.Fail(socket.name,
				fmt.Sprintf("cannot connect to %s: %v — run \"sudo bureau machine doctor --fix\"", socket.path, err)))
			continue
		}
		conn.Close()

		results = append(results, doctor.Pass(socket.name, "connectable"))
	}

	return results
}

// --- Section 6: Bureau space and fleet access ---

func checkBureauSpace(ctx context.Context, state *checkState) []doctor.Result {
	if state.session == nil {
		return []doctor.Result{
			doctor.Skip("bureau space", "skipped: no authenticated operator session"),
		}
	}
	if state.serverName == "" {
		return []doctor.Result{
			doctor.Skip("bureau space", "skipped: cannot determine server name"),
		}
	}

	var results []doctor.Result

	// Resolve the Bureau space.
	spaceAlias, err := ref.ParseRoomAlias("#bureau:" + state.serverName)
	if err != nil {
		return []doctor.Result{doctor.Fail("bureau space",
			fmt.Sprintf("invalid space alias #bureau:%s: %v", state.serverName, err))}
	}

	_, err = state.session.ResolveAlias(ctx, spaceAlias)
	if err != nil {
		results = append(results, doctor.Fail("bureau space",
			fmt.Sprintf("cannot resolve %s: %v — run \"bureau matrix doctor\" to check Matrix infrastructure", spaceAlias, err)))
		return results
	}

	results = append(results, doctor.Pass("bureau space",
		fmt.Sprintf("%s resolved", spaceAlias)))

	// Check standard rooms.
	standardRooms := []string{
		"bureau/system",
		"bureau/template",
		"bureau/pipeline",
		"bureau/artifact",
	}

	failedRooms := 0
	for _, room := range standardRooms {
		alias, parseError := ref.ParseRoomAlias("#" + room + ":" + state.serverName)
		if parseError != nil {
			results = append(results, doctor.Fail(room,
				fmt.Sprintf("invalid alias: %v", parseError)))
			failedRooms++
			continue
		}

		_, resolveError := state.session.ResolveAlias(ctx, alias)
		if resolveError != nil {
			results = append(results, doctor.Fail(room,
				fmt.Sprintf("cannot resolve %s", alias)))
			failedRooms++
			continue
		}

		results = append(results, doctor.Pass(room, "accessible"))
	}

	if failedRooms > 0 {
		// Replace the last failing room's message with guidance.
		for i := len(results) - 1; i >= 0; i-- {
			if results[i].Status == doctor.StatusFail {
				results[i].Message += " — run \"bureau matrix doctor --fix\" to repair"
				break
			}
		}
	}

	return results
}

// --- Guidance ---

// printGuidance prints a "next steps" section after the checklist when
// there are failures. Each failure domain gets a specific actionable
// command.
func printGuidance(results []doctor.Result) {
	type guidance struct {
		command     string
		description string
	}

	// Collect unique guidance based on which checks failed.
	var steps []guidance
	seen := make(map[string]bool)

	addStep := func(command, description string) {
		if seen[command] {
			return
		}
		seen[command] = true
		steps = append(steps, guidance{command, description})
	}

	anyFailed := false
	for _, result := range results {
		if result.Status != doctor.StatusFail {
			continue
		}
		anyFailed = true

		switch result.Name {
		case "operator session":
			addStep("bureau login <username>", "Authenticate as an operator")
		case "machine configuration":
			addStep("sudo bureau machine doctor --fix", "Repair machine infrastructure")
		case "bureau-launcher", "bureau-daemon",
			"launcher.sock", "observe.sock":
			addStep("sudo bureau machine doctor --fix", "Repair machine infrastructure")
		case "homeserver reachable":
			addStep("sudo bureau machine doctor --fix", "Repair machine infrastructure")
		case "bureau space",
			"bureau/system", "bureau/template",
			"bureau/pipeline", "bureau/artifact":
			addStep("bureau matrix doctor --fix --credential-file ./bureau-creds", "Repair Matrix infrastructure")
		}
	}

	if !anyFailed {
		// Print sub-doctor references for healthy systems.
		fmt.Fprintln(os.Stdout, "For detailed domain-specific checks:")
		fmt.Fprintln(os.Stdout, "  bureau machine doctor    System user, directories, binaries, systemd units")
		fmt.Fprintln(os.Stdout, "  bureau matrix doctor     Rooms, power levels, templates, pipelines")
		return
	}

	if len(steps) > 0 {
		fmt.Fprintln(os.Stdout, "Next steps:")
		maxCommandLength := 0
		for _, step := range steps {
			if len(step.command) > maxCommandLength {
				maxCommandLength = len(step.command)
			}
		}
		for _, step := range steps {
			fmt.Fprintf(os.Stdout, "  %-*s  %s\n", maxCommandLength, step.command, step.description)
		}
	}
}
