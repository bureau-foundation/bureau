// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-matrix-setup bootstraps a Matrix homeserver for Bureau.
// It creates the admin account, Bureau space, and standard rooms.
// Safe to re-run: all operations are idempotent.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/messaging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// stringSliceFlag accumulates values from repeated flag occurrences.
// Usage: --invite @alice:local --invite @bob:local
type stringSliceFlag []string

func (f *stringSliceFlag) String() string { return strings.Join(*f, ", ") }
func (f *stringSliceFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func run() error {
	var (
		homeserverURL     string
		registrationToken string
		credentialFile    string
		serverName        string
		adminUsername     string
		showVersion       bool
		inviteUsers       stringSliceFlag
	)

	var registrationTokenFile string

	flag.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
	flag.StringVar(&registrationTokenFile, "registration-token-file", "", "path to file containing registration token, or - for stdin (required)")
	flag.StringVar(&credentialFile, "credential-file", "", "path to write Bureau credentials (required)")
	flag.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name for constructing user/room IDs")
	flag.StringVar(&adminUsername, "admin-user", "bureau-admin", "admin account username")
	flag.BoolVar(&showVersion, "version", false, "print version information and exit")
	flag.Var(&inviteUsers, "invite", "Matrix user ID to invite to all Bureau rooms (repeatable)")
	flag.Parse()

	if showVersion {
		fmt.Printf("bureau-matrix-setup %s\n", version.Info())
		return nil
	}

	if registrationTokenFile == "" {
		return fmt.Errorf("--registration-token-file is required (use - for stdin)")
	}
	if credentialFile == "" {
		return fmt.Errorf("--credential-file is required")
	}

	var err error
	registrationToken, err = readSecret(registrationTokenFile)
	if err != nil {
		return fmt.Errorf("failed to read registration token: %w", err)
	}
	if registrationToken == "" {
		return fmt.Errorf("registration token is empty")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Add a generous timeout for the entire setup process.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
		Logger:        logger,
	})
	if err != nil {
		return fmt.Errorf("failed to create matrix client: %w", err)
	}

	// Step 1: Register or login the admin account.
	adminPassword := deriveAdminPassword(registrationToken)
	session, err := registerOrLogin(ctx, client, adminUsername, adminPassword, registrationToken)
	if err != nil {
		return fmt.Errorf("failed to get admin session: %w", err)
	}
	logger.Info("admin session established", "user_id", session.UserID())

	// Step 2: Create Bureau space.
	spaceRoomID, err := ensureSpace(ctx, session, serverName, logger)
	if err != nil {
		return fmt.Errorf("failed to create bureau space: %w", err)
	}
	logger.Info("bureau space ready", "room_id", spaceRoomID)

	// Step 3: Create standard rooms.
	//
	// All Bureau infrastructure rooms live under the bureau/ namespace.
	// Machines and services rooms allow members to publish their own
	// Bureau-specific state events (machine keys, service registrations).
	agentsRoomID, err := ensureRoom(ctx, session, "bureau/agents", "Bureau Agents", "Agent registry and presence", spaceRoomID, serverName, nil, logger)
	if err != nil {
		return fmt.Errorf("failed to create agents room: %w", err)
	}
	logger.Info("agents room ready", "room_id", agentsRoomID)

	systemRoomID, err := ensureRoom(ctx, session, "bureau/system", "Bureau System", "Operational messages", spaceRoomID, serverName, nil, logger)
	if err != nil {
		return fmt.Errorf("failed to create system room: %w", err)
	}
	logger.Info("system room ready", "room_id", systemRoomID)

	machinesRoomID, err := ensureRoom(ctx, session, "bureau/machines", "Bureau Machines", "Machine keys and status", spaceRoomID, serverName,
		[]string{schema.EventTypeMachineKey, schema.EventTypeMachineStatus}, logger)
	if err != nil {
		return fmt.Errorf("failed to create machines room: %w", err)
	}
	logger.Info("machines room ready", "room_id", machinesRoomID)

	servicesRoomID, err := ensureRoom(ctx, session, "bureau/services", "Bureau Services", "Service directory", spaceRoomID, serverName,
		[]string{schema.EventTypeService}, logger)
	if err != nil {
		return fmt.Errorf("failed to create services room: %w", err)
	}
	logger.Info("services room ready", "room_id", servicesRoomID)

	// Step 4: Invite users to all Bureau rooms.
	if len(inviteUsers) > 0 {
		allRooms := []struct {
			name   string
			roomID string
		}{
			{"bureau (space)", spaceRoomID},
			{"bureau/agents", agentsRoomID},
			{"bureau/system", systemRoomID},
			{"bureau/machines", machinesRoomID},
			{"bureau/services", servicesRoomID},
		}
		for _, userID := range inviteUsers {
			for _, room := range allRooms {
				if err := session.InviteUser(ctx, room.roomID, userID); err != nil {
					// Inviting a user who is already in the room is not an error
					// in Matrix (the homeserver returns M_FORBIDDEN or ignores it
					// depending on implementation), but we log and continue rather
					// than failing the entire setup.
					if messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
						logger.Info("user already in room or invite not needed",
							"user_id", userID,
							"room", room.name,
						)
						continue
					}
					return fmt.Errorf("failed to invite %q to %s (%s): %w", userID, room.name, room.roomID, err)
				}
				logger.Info("invited user to room",
					"user_id", userID,
					"room", room.name,
					"room_id", room.roomID,
				)
			}
		}
	}

	// Step 5: Write credentials.
	if err := writeCredentials(credentialFile, homeserverURL, session, registrationToken, spaceRoomID, agentsRoomID, systemRoomID, machinesRoomID, servicesRoomID); err != nil {
		return fmt.Errorf("failed to write credentials: %w", err)
	}
	logger.Info("credentials written", "path", credentialFile)

	logger.Info("bureau matrix setup complete",
		"admin_user", session.UserID(),
		"space", spaceRoomID,
		"agents_room", agentsRoomID,
		"system_room", systemRoomID,
		"machines_room", machinesRoomID,
		"services_room", servicesRoomID,
	)
	return nil
}

// registerOrLogin registers a new account, or logs in if it already exists.
// This makes the setup idempotent.
func registerOrLogin(ctx context.Context, client *messaging.Client, username, password, registrationToken string) (*messaging.Session, error) {
	session, err := client.Register(ctx, messaging.RegisterRequest{
		Username:          username,
		Password:          password,
		RegistrationToken: registrationToken,
	})
	if err == nil {
		return session, nil
	}

	// If the user already exists, log in instead.
	if messaging.IsMatrixError(err, messaging.ErrCodeUserInUse) {
		slog.Info("admin account already exists, logging in", "username", username)
		return client.Login(ctx, username, password)
	}

	return nil, err
}

// ensureSpace creates the Bureau space if it doesn't exist.
// A Matrix space is a room with creation_content.type = "m.space".
func ensureSpace(ctx context.Context, session *messaging.Session, serverName string, logger *slog.Logger) (string, error) {
	alias := fmt.Sprintf("#bureau:%s", serverName)

	// Check if the space already exists.
	roomID, err := session.ResolveAlias(ctx, alias)
	if err == nil {
		logger.Info("bureau space already exists", "alias", alias, "room_id", roomID)
		return roomID, nil
	}
	if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return "", fmt.Errorf("failed to resolve alias %q: %w", alias, err)
	}

	// Create the space.
	response, err := session.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:       "Bureau",
		Alias:      "bureau",
		Topic:      "Bureau agent orchestration",
		Preset:     "private_chat",
		Visibility: "private",
		CreationContent: map[string]any{
			"type": "m.space",
		},
		PowerLevelContentOverride: adminOnlyPowerLevels(session.UserID(), nil),
	})
	if err != nil {
		return "", fmt.Errorf("failed to create bureau space: %w", err)
	}
	return response.RoomID, nil
}

// ensureRoom creates a room if it doesn't exist and adds it as a child of the space.
//
// memberSettableEventTypes lists Bureau-specific state event types that room
// members at the default power level (0) are allowed to set. This lets machines
// publish their own keys in #bureau/machines and services register in
// #bureau/services. Standard room state events remain admin-only regardless.
func ensureRoom(ctx context.Context, session *messaging.Session, aliasLocal, name, topic, spaceRoomID, serverName string, memberSettableEventTypes []string, logger *slog.Logger) (string, error) {
	alias := fmt.Sprintf("#%s:%s", aliasLocal, serverName)

	// Check if the room already exists.
	roomID, err := session.ResolveAlias(ctx, alias)
	if err == nil {
		logger.Info("room already exists", "alias", alias, "room_id", roomID)
		return roomID, nil
	}
	if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return "", fmt.Errorf("failed to resolve alias %q: %w", alias, err)
	}

	// Create the room.
	response, err := session.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:       name,
		Alias:      aliasLocal,
		Topic:      topic,
		Preset:     "private_chat",
		Visibility: "private",
		PowerLevelContentOverride: adminOnlyPowerLevels(session.UserID(), memberSettableEventTypes),
	})
	if err != nil {
		return "", fmt.Errorf("failed to create room %q: %w", aliasLocal, err)
	}

	// Add as child of the Bureau space.
	_, err = session.SendStateEvent(ctx, spaceRoomID, "m.space.child", response.RoomID,
		map[string]any{
			"via": []string{serverName},
		})
	if err != nil {
		return "", fmt.Errorf("failed to add room %q as child of space: %w", aliasLocal, err)
	}

	return response.RoomID, nil
}

// writeCredentials writes the Bureau credentials to a file in key=value format
// compatible with proxy/credentials.go:FileCredentialSource.
func writeCredentials(path, homeserverURL string, session *messaging.Session, registrationToken, spaceRoomID, agentsRoomID, systemRoomID, machinesRoomID, servicesRoomID string) error {
	var builder strings.Builder
	builder.WriteString("# Bureau Matrix credentials\n")
	builder.WriteString("# Written by bureau-matrix-setup. Do not edit manually.\n")
	builder.WriteString("#\n")
	fmt.Fprintf(&builder, "MATRIX_HOMESERVER_URL=%s\n", homeserverURL)
	fmt.Fprintf(&builder, "MATRIX_ADMIN_USER=%s\n", session.UserID())
	fmt.Fprintf(&builder, "MATRIX_ADMIN_TOKEN=%s\n", session.AccessToken())
	fmt.Fprintf(&builder, "MATRIX_REGISTRATION_TOKEN=%s\n", registrationToken)
	fmt.Fprintf(&builder, "MATRIX_SPACE_ROOM=%s\n", spaceRoomID)
	fmt.Fprintf(&builder, "MATRIX_AGENTS_ROOM=%s\n", agentsRoomID)
	fmt.Fprintf(&builder, "MATRIX_SYSTEM_ROOM=%s\n", systemRoomID)
	fmt.Fprintf(&builder, "MATRIX_MACHINES_ROOM=%s\n", machinesRoomID)
	fmt.Fprintf(&builder, "MATRIX_SERVICES_ROOM=%s\n", servicesRoomID)

	return os.WriteFile(path, []byte(builder.String()), 0600)
}

// adminOnlyPowerLevels returns power level settings where only the admin can
// perform administrative actions (invite, kick, ban, change room state).
// Members at the default power level (0) can send messages but nothing else.
// This prevents one member from inviting another, or modifying room settings
// to weaken access controls.
//
// memberSettableEventTypes lists state event types that members at power level
// 0 are allowed to set. This is used for Bureau-specific events: machines
// publish their own m.bureau.machine_key in #bureau/machines, services register
// via m.bureau.service in #bureau/services. Standard room state events remain
// admin-only regardless of this parameter.
func adminOnlyPowerLevels(adminUserID string, memberSettableEventTypes []string) map[string]any {
	events := map[string]any{
		"m.room.avatar":             100,
		"m.room.canonical_alias":    100,
		"m.room.encryption":         100,
		"m.room.history_visibility": 100,
		"m.room.join_rules":         100,
		"m.room.name":               100,
		"m.room.power_levels":       100,
		"m.room.server_acl":         100,
		"m.room.tombstone":          100,
		"m.room.topic":              100,
		"m.space.child":             100,
	}
	for _, eventType := range memberSettableEventTypes {
		events[eventType] = 0
	}
	return map[string]any{
		"ban":            100,
		"invite":         100,
		"kick":           100,
		"redact":         50,
		"events_default": 0,
		"state_default":  100,
		"notifications":  map[string]any{"room": 50},
		"users":          map[string]any{adminUserID: 100},
		"users_default":  0,
		"events":         events,
	}
}

// readSecret reads a secret from a file path, or from stdin if path is "-".
// The returned string is trimmed of leading/trailing whitespace. This avoids
// putting secrets in CLI arguments (visible in /proc/*/cmdline, ps, shell
// history, and LLM conversation context).
func readSecret(path string) (string, error) {
	var data []byte
	var err error

	if path == "-" {
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			if scanErr := scanner.Err(); scanErr != nil {
				return "", fmt.Errorf("reading stdin: %w", scanErr)
			}
			return "", fmt.Errorf("stdin is empty")
		}
		data = scanner.Bytes()
	} else {
		data, err = os.ReadFile(path)
		if err != nil {
			return "", err
		}
	}

	return strings.TrimSpace(string(data)), nil
}

// deriveAdminPassword deterministically derives the admin password from the
// registration token. This ensures that re-running setup with the same token
// produces the same password, making the tool idempotent.
//
// The derivation uses SHA-256 with a domain separator. This is acceptable here
// because the registration token is high-entropy random material (generated by
// `openssl rand -hex 32`), and the password only needs to resist online attacks
// (rate-limited by the homeserver), not offline brute-force.
func deriveAdminPassword(registrationToken string) string {
	hash := sha256.Sum256([]byte("bureau-admin-password:" + registrationToken))
	return hex.EncodeToString(hash[:])
}
