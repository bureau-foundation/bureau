// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package quickstart implements the "bureau quickstart" command — a one-command
// on-ramp that deploys an agent into a running Bureau instance and attaches
// observation.
//
// The quickstart flow:
//   - Run preflight checks (homeserver, launcher, daemon, credentials)
//   - Publish agent templates to the template room
//   - Register the sysadmin principal's Matrix account
//   - Seal credentials and push to the config room
//   - Push machine config with the principal assignment
//   - Wait for the proxy socket (proves sandbox creation)
//   - Exec into "bureau observe" to attach to the agent's terminal
package quickstart

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/messaging"
)

// Command returns the "quickstart" CLI command.
func Command() *cli.Command {
	var (
		agent          string
		homeserverURL  string
		serverName     string
		credentialFile string
		runDir         string
		machineName    string
	)

	return &cli.Command{
		Name:    "quickstart",
		Summary: "Deploy an agent and observe it — one command from zero to running",
		Description: `Deploy a sandboxed agent on this machine and attach to its terminal.

Quickstart is the fastest way to get a Bureau agent running. It publishes
the agent template, registers a Matrix account, seals credentials, pushes
configuration, and waits for the sandbox to start — then opens observation
so you can watch the agent work.

Prerequisites: the homeserver (Continuwuity), launcher, and daemon must be
running. If "bureau matrix setup" hasn't been run yet, quickstart can do it
automatically.

Supported agents:
  test     Built-in test agent that validates the full stack
  claude   Claude Code in a sandboxed terminal`,
		Usage: "bureau quickstart [--agent=test|claude] [flags]",
		Examples: []cli.Example{
			{
				Description: "Run the built-in test agent to validate the stack",
				Command:     "bureau quickstart --agent=test --credential-file ./bureau-creds",
			},
			{
				Description: "Launch Claude Code in a sandbox",
				Command:     "bureau quickstart --agent=claude --credential-file ./bureau-creds",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("quickstart", pflag.ContinueOnError)
			flagSet.StringVar(&agent, "agent", "test", "agent type to deploy (test, claude)")
			flagSet.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Continuwuity homeserver URL")
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
			flagSet.StringVar(&credentialFile, "credential-file", "", "path to Bureau credential file (required)")
			flagSet.StringVar(&runDir, "run-dir", "/run/bureau", "Bureau runtime directory (launcher/daemon must match)")
			flagSet.StringVar(&machineName, "machine-name", "", "machine localpart (auto-detected from launcher if not set)")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}
			if credentialFile == "" {
				return fmt.Errorf("--credential-file is required\n\nUsage: bureau quickstart [flags]")
			}

			return run(Config{
				Agent:          agent,
				HomeserverURL:  homeserverURL,
				ServerName:     serverName,
				CredentialFile: credentialFile,
				RunDir:         runDir,
				MachineName:    machineName,
			})
		},
	}
}

// Config holds the quickstart parameters. Exported for testing.
type Config struct {
	Agent          string
	HomeserverURL  string
	ServerName     string
	CredentialFile string
	RunDir         string
	MachineName    string
}

func run(config Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Validate agent type.
	templateName, recognized := templateForAgent(config.Agent)
	if !recognized {
		return fmt.Errorf("unknown agent type %q (supported: test, claude)", config.Agent)
	}

	// Run preflight checks.
	fmt.Fprintf(os.Stderr, "Running preflight checks...\n")
	results := runPreflight(ctx, preflightConfig{
		HomeserverURL:  config.HomeserverURL,
		RunDir:         config.RunDir,
		CredentialFile: config.CredentialFile,
		Agent:          config.Agent,
	})

	for _, result := range results {
		if result.Passed {
			fmt.Fprintf(os.Stderr, "  [ok] %s: %s\n", result.Name, result.Message)
		} else {
			fmt.Fprintf(os.Stderr, "  [FAIL] %s: %s\n", result.Name, result.Message)
		}
	}

	if !preflightPassed(results) {
		return fmt.Errorf("preflight checks failed — fix the issues above and retry")
	}
	fmt.Fprintf(os.Stderr, "\n")

	// Load admin credentials.
	credentials, err := cli.ReadCredentialFile(config.CredentialFile)
	if err != nil {
		return fmt.Errorf("read credential file: %w", err)
	}

	adminUserID := credentials["MATRIX_ADMIN_USER"]
	adminToken := credentials["MATRIX_ADMIN_TOKEN"]
	registrationToken := credentials["MATRIX_REGISTRATION_TOKEN"]
	if adminUserID == "" || adminToken == "" {
		return fmt.Errorf("credential file missing MATRIX_ADMIN_USER or MATRIX_ADMIN_TOKEN")
	}
	if registrationToken == "" {
		return fmt.Errorf("credential file missing MATRIX_REGISTRATION_TOKEN")
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: config.HomeserverURL,
	})
	if err != nil {
		return fmt.Errorf("create matrix client: %w", err)
	}

	admin, err := client.SessionFromToken(adminUserID, adminToken)
	if err != nil {
		return fmt.Errorf("create admin session: %w", err)
	}
	defer admin.Close()

	// Determine the machine name. If not specified, detect from the launcher
	// by resolving the machines room and looking for a machine_key event
	// whose key matches this RunDir.
	machineName := config.MachineName
	if machineName == "" {
		return fmt.Errorf("--machine-name is required (auto-detection not yet implemented)")
	}

	// Resolve the template room and publish quickstart templates.
	fmt.Fprintf(os.Stderr, "Publishing agent templates...\n")
	templateRoomAlias := principal.RoomAlias("bureau/template", config.ServerName)
	templateRoomID, err := admin.ResolveAlias(ctx, templateRoomAlias)
	if err != nil {
		return fmt.Errorf("resolve template room %q: %w", templateRoomAlias, err)
	}

	templates := agentTemplates()
	for name, content := range templates {
		_, err := admin.SendStateEvent(ctx, templateRoomID,
			schema.EventTypeTemplate, name, content)
		if err != nil {
			return fmt.Errorf("publish template %q: %w", name, err)
		}
		fmt.Fprintf(os.Stderr, "  Published: %s\n", name)
	}

	// Resolve the config room for this machine.
	configAlias := principal.RoomAlias("bureau/config/"+machineName, config.ServerName)
	configRoomID, err := admin.ResolveAlias(ctx, configAlias)
	if err != nil {
		return fmt.Errorf("resolve config room %q: %w", configAlias, err)
	}

	// Join the config room as admin (may already be a member).
	if _, err := admin.JoinRoom(ctx, configRoomID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			return fmt.Errorf("join config room: %w", err)
		}
	}

	// Register the sysadmin principal.
	sysadminLocalpart := "sysadmin/" + machineName
	fmt.Fprintf(os.Stderr, "Registering principal %s...\n", sysadminLocalpart)

	sysadminSession, err := client.Register(ctx, messaging.RegisterRequest{
		Username:          sysadminLocalpart,
		Password:          "quickstart-" + registrationToken,
		RegistrationToken: registrationToken,
	})
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeUserInUse) {
			// Already registered — log in instead.
			sysadminSession, err = client.Login(ctx, sysadminLocalpart, "quickstart-"+registrationToken)
			if err != nil {
				return fmt.Errorf("login existing sysadmin principal: %w", err)
			}
		} else {
			return fmt.Errorf("register sysadmin principal: %w", err)
		}
	}
	sysadminToken := sysadminSession.AccessToken()
	sysadminUserID := sysadminSession.UserID()
	fmt.Fprintf(os.Stderr, "  Principal: %s\n", sysadminUserID)

	// Invite the sysadmin to the config room and join. The proxy's
	// default-deny MatrixPolicy blocks JoinRoom from inside the sandbox,
	// so we establish membership here before the sandbox starts.
	if err := admin.InviteUser(ctx, configRoomID, sysadminUserID); err != nil {
		if !messaging.IsMatrixError(err, "M_FORBIDDEN") {
			sysadminSession.Close()
			return fmt.Errorf("invite sysadmin to config room: %w", err)
		}
	}
	if _, err := sysadminSession.JoinRoom(ctx, configRoomID); err != nil {
		sysadminSession.Close()
		return fmt.Errorf("sysadmin join config room: %w", err)
	}
	sysadminSession.Close()

	// Retrieve the machine's public key for credential sealing.
	machinesAlias := principal.RoomAlias("bureau/machines", config.ServerName)
	machinesRoomID, err := admin.ResolveAlias(ctx, machinesAlias)
	if err != nil {
		return fmt.Errorf("resolve machines room: %w", err)
	}

	machineKeyJSON, err := admin.GetStateEvent(ctx, machinesRoomID,
		schema.EventTypeMachineKey, machineName)
	if err != nil {
		return fmt.Errorf("get machine key for %s: %w", machineName, err)
	}
	var machineKey struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(machineKeyJSON, &machineKey); err != nil {
		return fmt.Errorf("unmarshal machine key: %w", err)
	}
	if machineKey.PublicKey == "" {
		return fmt.Errorf("machine %s has an empty public key", machineName)
	}

	// Seal and push credentials.
	fmt.Fprintf(os.Stderr, "Sealing credentials...\n")
	credentialBundle := map[string]string{
		"MATRIX_TOKEN":          sysadminToken,
		"MATRIX_USER_ID":        sysadminUserID,
		"MATRIX_HOMESERVER_URL": config.HomeserverURL,
	}
	credentialJSON, err := json.Marshal(credentialBundle)
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}

	ciphertext, err := sealed.Encrypt(credentialJSON, []string{machineKey.PublicKey})
	if err != nil {
		return fmt.Errorf("encrypt credentials: %w", err)
	}

	machineUserID := principal.MatrixUserID(machineName, config.ServerName)
	_, err = admin.SendStateEvent(ctx, configRoomID,
		"m.bureau.credentials", sysadminLocalpart, map[string]any{
			"version":        1,
			"principal":      sysadminUserID,
			"encrypted_for":  []string{machineUserID},
			"keys":           []string{"MATRIX_TOKEN", "MATRIX_USER_ID", "MATRIX_HOMESERVER_URL"},
			"ciphertext":     ciphertext,
			"provisioned_by": adminUserID,
			"provisioned_at": time.Now().UTC().Format(time.RFC3339),
		})
	if err != nil {
		return fmt.Errorf("push credentials: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  Credentials sealed and pushed\n")

	// Push machine config with the principal assignment.
	fmt.Fprintf(os.Stderr, "Deploying agent (template: %s)...\n", templateName)
	templateRef := "bureau/template:" + templateName
	_, err = admin.SendStateEvent(ctx, configRoomID,
		schema.EventTypeMachineConfig, machineName, schema.MachineConfig{
			Principals: []schema.PrincipalAssignment{
				{
					Localpart: sysadminLocalpart,
					Template:  templateRef,
					AutoStart: true,
				},
			},
		})
	if err != nil {
		return fmt.Errorf("push machine config: %w", err)
	}

	// Wait for the proxy socket (proves sandbox was created).
	fmt.Fprintf(os.Stderr, "Waiting for sandbox...\n")
	proxySocketPath := principal.RunDirSocketPath(config.RunDir, sysadminLocalpart)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(proxySocketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for proxy socket at %s (30s)\n"+
				"  Check launcher and daemon logs for errors", proxySocketPath)
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "  Sandbox running\n")

	fmt.Fprintf(os.Stderr, "\nAgent deployed. Observe with:\n")
	fmt.Fprintf(os.Stderr, "  bureau observe %s\n", sysadminLocalpart)

	return nil
}
