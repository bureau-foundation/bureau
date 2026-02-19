// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/messaging"
	"github.com/bureau-foundation/bureau/proxy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		printUsage()
		return fmt.Errorf("subcommand required")
	}

	subcommand := os.Args[1]
	switch subcommand {
	case "keygen":
		return runKeygen()
	case "provision":
		return runProvision(os.Args[2:])
	case "assign":
		return runAssign(os.Args[2:])
	case "list":
		return runList(os.Args[2:])
	case "version":
		fmt.Printf("bureau-credentials %s\n", version.Info())
		return nil
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown subcommand: %q", subcommand)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: bureau-credentials <subcommand> [flags]

Subcommands:
  keygen      Generate an age keypair (for operator escrow)
  provision   Encrypt credentials and publish to Matrix
  assign      Assign a principal to a machine
  list        List provisioned credential bundles
  version     Print version information

Run 'bureau-credentials <subcommand> --help' for subcommand flags.
`)
}

// runKeygen generates a new age keypair and prints it.
// The public key goes to stdout (for sharing/embedding).
// The private key goes to stderr (or a file, for safekeeping).
func runKeygen() error {
	keypair, err := sealed.GenerateKeypair()
	if err != nil {
		return fmt.Errorf("generating keypair: %w", err)
	}
	defer keypair.Close()

	fmt.Fprintf(os.Stderr, "# Private key (keep this secret — store securely):\n")
	fmt.Fprintf(os.Stderr, "%s\n", keypair.PrivateKey.String())
	fmt.Fprintf(os.Stdout, "%s\n", keypair.PublicKey)
	return nil
}

// matrixConfig holds the Matrix connection parameters read from the config file.
type matrixConfig struct {
	HomeserverURL string
	AdminToken    string
	AdminUserID   string
	ServerName    string
}

// loadMatrixConfig reads Matrix connection parameters from a credential file
// (the same format written by bureau-matrix-setup).
func loadMatrixConfig(configPath, serverName string) (*matrixConfig, error) {
	source := &proxy.FileCredentialSource{Path: configPath}
	defer source.Close()

	homeserverURLBuffer := source.Get("matrix-homeserver-url")
	if homeserverURLBuffer == nil {
		return nil, fmt.Errorf("MATRIX_HOMESERVER_URL not found in %s", configPath)
	}

	adminTokenBuffer := source.Get("matrix-admin-token")
	if adminTokenBuffer == nil {
		return nil, fmt.Errorf("MATRIX_ADMIN_TOKEN not found in %s", configPath)
	}

	adminUserIDBuffer := source.Get("matrix-admin-user")
	if adminUserIDBuffer == nil {
		return nil, fmt.Errorf("MATRIX_ADMIN_USER not found in %s", configPath)
	}

	return &matrixConfig{
		HomeserverURL: homeserverURLBuffer.String(),
		AdminToken:    adminTokenBuffer.String(),
		AdminUserID:   adminUserIDBuffer.String(),
		ServerName:    serverName,
	}, nil
}

// createSession creates an authenticated Matrix session from the config.
func createSession(config *matrixConfig) (*messaging.DirectSession, error) {
	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: config.HomeserverURL,
		Logger:        slog.Default(),
	})
	if err != nil {
		return nil, fmt.Errorf("creating matrix client: %w", err)
	}

	return client.SessionFromToken(config.AdminUserID, config.AdminToken)
}

// runProvision encrypts credentials and publishes them to Matrix.
func runProvision(args []string) error {
	flags := flag.NewFlagSet("provision", flag.ExitOnError)
	var (
		configPath    string
		machineName   string
		principalName string
		serverName    string
		fleetPrefix   string
		escrowKey     string
		fromFile      string
	)

	flags.StringVar(&configPath, "config", "", "path to bureau-matrix-setup credential file (required)")
	flags.StringVar(&machineName, "machine", "", "machine localpart (e.g., machine/workstation) (required)")
	flags.StringVar(&principalName, "principal", "", "principal localpart (e.g., iree/amdgpu/pm) (required)")
	flags.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flags.StringVar(&fleetPrefix, "fleet", "", "fleet prefix (e.g., bureau/fleet/prod) (required)")
	flags.StringVar(&escrowKey, "escrow-key", "", "operator escrow age public key (optional)")
	flags.StringVar(&fromFile, "from-file", "", "read credentials from file instead of stdin (JSON object)")
	flags.Parse(args)

	if configPath == "" || machineName == "" || principalName == "" || fleetPrefix == "" {
		flags.Usage()
		return fmt.Errorf("--config, --machine, --principal, and --fleet are required")
	}

	if err := principal.ValidateLocalpart(principalName); err != nil {
		return fmt.Errorf("invalid principal name: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Read credentials from stdin or file.
	var credentialReader io.Reader
	if fromFile != "" {
		file, err := os.Open(fromFile)
		if err != nil {
			return fmt.Errorf("opening credential file: %w", err)
		}
		defer file.Close()
		credentialReader = file
	} else {
		credentialReader = os.Stdin
	}

	credentialData, err := io.ReadAll(credentialReader)
	if err != nil {
		return fmt.Errorf("reading credentials: %w", err)
	}
	defer func() { secret.Zero(credentialData) }()

	if len(credentialData) == 0 {
		return fmt.Errorf("no credential data provided (pipe JSON to stdin or use --from-file)")
	}

	// Parse the credential JSON to validate and extract key names.
	var credentials map[string]string
	if err := json.Unmarshal(credentialData, &credentials); err != nil {
		return fmt.Errorf("parsing credential JSON: %w", err)
	}
	if len(credentials) == 0 {
		return fmt.Errorf("credential JSON is empty")
	}

	credentialKeys := make([]string, 0, len(credentials))
	for key := range credentials {
		credentialKeys = append(credentialKeys, key)
	}

	// Connect to Matrix.
	matrixConf, err := loadMatrixConfig(configPath, serverName)
	if err != nil {
		return err
	}

	session, err := createSession(matrixConf)
	if err != nil {
		return err
	}

	// Parse the fleet prefix to derive the fleet machine room alias.
	fleet, err := ref.ParseFleet(fleetPrefix, serverName)
	if err != nil {
		return fmt.Errorf("parsing fleet prefix: %w", err)
	}

	// Fetch the machine's public key from the fleet machine room.
	machineRoomID, err := session.ResolveAlias(ctx, fleet.MachineRoomAlias())
	if err != nil {
		return fmt.Errorf("resolving fleet machine room: %w", err)
	}

	machineKeyContent, err := session.GetStateEvent(ctx, machineRoomID, schema.EventTypeMachineKey, machineName)
	if err != nil {
		return fmt.Errorf("fetching machine key for %q: %w", machineName, err)
	}

	var machineKey schema.MachineKey
	if err := json.Unmarshal(machineKeyContent, &machineKey); err != nil {
		return fmt.Errorf("parsing machine key: %w", err)
	}

	if machineKey.Algorithm != "age-x25519" {
		return fmt.Errorf("unsupported machine key algorithm: %q (expected age-x25519)", machineKey.Algorithm)
	}
	if err := sealed.ParsePublicKey(machineKey.PublicKey); err != nil {
		return fmt.Errorf("invalid machine public key: %w", err)
	}

	// Parse the machine ref for typed identity construction.
	machineRef, err := ref.ParseMachine(machineName, serverName)
	if err != nil {
		return fmt.Errorf("invalid machine name: %w", err)
	}

	// Build the recipient list: machine key + optional escrow key.
	recipientKeys := []string{machineKey.PublicKey}
	encryptedFor := []string{machineRef.UserID()}

	if escrowKey != "" {
		if err := sealed.ParsePublicKey(escrowKey); err != nil {
			return fmt.Errorf("invalid escrow key: %w", err)
		}
		recipientKeys = append(recipientKeys, escrowKey)
		encryptedFor = append(encryptedFor, "escrow:operator")
	}

	// Encrypt the credential bundle.
	ciphertext, err := sealed.EncryptJSON(credentialData, recipientKeys)
	if err != nil {
		return fmt.Errorf("encrypting credentials: %w", err)
	}

	// Resolve the config room. The config room must already exist (created
	// by machine provisioning or the daemon's first boot). If it doesn't
	// exist, that indicates the machine hasn't been provisioned.
	configRoomAlias := machineRef.RoomAlias()
	configRoomID, err := session.ResolveAlias(ctx, configRoomAlias)
	if err != nil {
		return fmt.Errorf("config room %s does not exist — provision the machine first: %w", configRoomAlias, err)
	}

	// Publish the credentials state event.
	principalUserID := principal.MatrixUserID(principalName, serverName)
	credEvent := schema.Credentials{
		Version:       1,
		Principal:     principalUserID,
		EncryptedFor:  encryptedFor,
		Keys:          credentialKeys,
		Ciphertext:    ciphertext,
		ProvisionedBy: matrixConf.AdminUserID,
		ProvisionedAt: time.Now().UTC().Format(time.RFC3339),
	}

	eventID, err := session.SendStateEvent(ctx, configRoomID, schema.EventTypeCredentials, principalName, credEvent)
	if err != nil {
		return fmt.Errorf("publishing credentials: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Provisioned credentials for %s on %s\n", principalName, machineName)
	fmt.Fprintf(os.Stderr, "  Keys: %s\n", strings.Join(credentialKeys, ", "))
	fmt.Fprintf(os.Stderr, "  Encrypted for: %s\n", strings.Join(encryptedFor, ", "))
	fmt.Fprintf(os.Stderr, "  Config room: %s\n", configRoomID)
	fmt.Fprintf(os.Stderr, "  Event: %s\n", eventID)

	return nil
}

// runAssign writes a MachineConfig state event to assign a principal to a machine.
func runAssign(args []string) error {
	flags := flag.NewFlagSet("assign", flag.ExitOnError)
	var (
		configPath    string
		machineName   string
		principalName string
		template      string
		serverName    string
		autoStart     bool
	)

	flags.StringVar(&configPath, "config", "", "path to bureau-matrix-setup credential file (required)")
	flags.StringVar(&machineName, "machine", "", "machine localpart (required)")
	flags.StringVar(&principalName, "principal", "", "principal localpart to assign (required)")
	flags.StringVar(&template, "template", "", "sandbox template name (required)")
	flags.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flags.BoolVar(&autoStart, "auto-start", true, "start the principal's sandbox automatically")
	flags.Parse(args)

	if configPath == "" || machineName == "" || principalName == "" || template == "" {
		flags.Usage()
		return fmt.Errorf("--config, --machine, --principal, and --template are required")
	}

	machineRef, err := ref.ParseMachine(machineName, serverName)
	if err != nil {
		return fmt.Errorf("invalid machine name: %w", err)
	}
	if err := principal.ValidateLocalpart(principalName); err != nil {
		return fmt.Errorf("invalid principal name: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	matrixConf, err := loadMatrixConfig(configPath, serverName)
	if err != nil {
		return err
	}

	session, err := createSession(matrixConf)
	if err != nil {
		return err
	}

	// Resolve the config room. The config room must already exist (created
	// by machine provisioning or the daemon's first boot).
	configRoomAlias := machineRef.RoomAlias()
	configRoomID, err := session.ResolveAlias(ctx, configRoomAlias)
	if err != nil {
		return fmt.Errorf("config room %s does not exist — provision the machine first: %w", configRoomAlias, err)
	}

	// Read the current MachineConfig (if any) to merge the new assignment.
	var config schema.MachineConfig
	existingContent, err := session.GetStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineName)
	if err == nil {
		if err := json.Unmarshal(existingContent, &config); err != nil {
			return fmt.Errorf("parsing existing machine config: %w", err)
		}
	} else if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return fmt.Errorf("reading machine config: %w", err)
	}

	// Check if the principal is already assigned.
	found := false
	for i, assignment := range config.Principals {
		if assignment.Localpart == principalName {
			// Update the existing assignment.
			config.Principals[i].Template = template
			config.Principals[i].AutoStart = autoStart
			found = true
			break
		}
	}

	if !found {
		config.Principals = append(config.Principals, schema.PrincipalAssignment{
			Localpart: principalName,
			Template:  template,
			AutoStart: autoStart,
		})
	}

	eventID, err := session.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineName, config)
	if err != nil {
		return fmt.Errorf("publishing machine config: %w", err)
	}

	action := "Assigned"
	if found {
		action = "Updated assignment for"
	}
	fmt.Fprintf(os.Stderr, "%s %s on %s (template=%s, auto_start=%v)\n",
		action, principalName, machineName, template, autoStart)
	fmt.Fprintf(os.Stderr, "  Config room: %s\n", configRoomID)
	fmt.Fprintf(os.Stderr, "  Event: %s\n", eventID)

	return nil
}

// runList shows provisioned credential bundles for a machine.
func runList(args []string) error {
	flags := flag.NewFlagSet("list", flag.ExitOnError)
	var (
		configPath  string
		machineName string
		serverName  string
	)

	flags.StringVar(&configPath, "config", "", "path to bureau-matrix-setup credential file (required)")
	flags.StringVar(&machineName, "machine", "", "machine localpart (required)")
	flags.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name")
	flags.Parse(args)

	if configPath == "" || machineName == "" {
		flags.Usage()
		return fmt.Errorf("--config and --machine are required")
	}

	machineRef, err := ref.ParseMachine(machineName, serverName)
	if err != nil {
		return fmt.Errorf("invalid machine name: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	matrixConf, err := loadMatrixConfig(configPath, serverName)
	if err != nil {
		return err
	}

	session, err := createSession(matrixConf)
	if err != nil {
		return err
	}

	// Resolve the config room.
	configRoomAlias := machineRef.RoomAlias()
	configRoomID, err := session.ResolveAlias(ctx, configRoomAlias)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			fmt.Fprintf(os.Stderr, "No config room found for %s\n", machineName)
			return nil
		}
		return fmt.Errorf("resolving config room: %w", err)
	}

	// Read all state events to find MachineConfig and Credentials.
	events, err := session.GetRoomState(ctx, configRoomID)
	if err != nil {
		return fmt.Errorf("reading room state: %w", err)
	}

	// Display machine config.
	fmt.Printf("Machine: %s\n", machineName)
	fmt.Printf("Config room: %s\n\n", configRoomID)

	for _, event := range events {
		if event.Type == schema.EventTypeMachineConfig && event.StateKey != nil {
			contentJSON, _ := json.Marshal(event.Content)
			var config schema.MachineConfig
			json.Unmarshal(contentJSON, &config)

			fmt.Printf("Assigned principals:\n")
			if len(config.Principals) == 0 {
				fmt.Printf("  (none)\n")
			}
			for _, assignment := range config.Principals {
				autoStartStr := ""
				if assignment.AutoStart {
					autoStartStr = " [auto-start]"
				}
				fmt.Printf("  %s (template=%s)%s\n", assignment.Localpart, assignment.Template, autoStartStr)
			}
			fmt.Println()
		}
	}

	// Display credential bundles.
	fmt.Printf("Credential bundles:\n")
	credentialCount := 0
	for _, event := range events {
		if event.Type == schema.EventTypeCredentials && event.StateKey != nil {
			contentJSON, _ := json.Marshal(event.Content)
			var creds schema.Credentials
			json.Unmarshal(contentJSON, &creds)

			fmt.Printf("  %s:\n", *event.StateKey)
			fmt.Printf("    Keys: %s\n", strings.Join(creds.Keys, ", "))
			fmt.Printf("    Encrypted for: %s\n", strings.Join(creds.EncryptedFor, ", "))
			fmt.Printf("    Provisioned by: %s\n", creds.ProvisionedBy)
			if creds.ProvisionedAt != "" {
				fmt.Printf("    Provisioned at: %s\n", creds.ProvisionedAt)
			}
			credentialCount++
		}
	}
	if credentialCount == 0 {
		fmt.Printf("  (none)\n")
	}

	return nil
}
