// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// SetupCommand returns the "setup" subcommand for bootstrapping a Matrix
// homeserver. This is the only matrix subcommand that talks directly to
// the homeserver (no proxy) since it runs before any agent infrastructure
// exists.
func SetupCommand() *cli.Command {
	var (
		homeserverURL         string
		registrationTokenFile string
		credentialFile        string
		serverName            string
		adminUsername         string
		inviteUsers           []string
	)

	return &cli.Command{
		Name:    "setup",
		Summary: "Bootstrap a Matrix homeserver for Bureau",
		Description: `Bootstrap a Matrix homeserver for Bureau. Creates the admin account,
Bureau space, and standard rooms. Safe to re-run: all operations are
idempotent.

The registration token is read from a file (or stdin with "-") to avoid
exposing secrets in CLI arguments, process listings, or shell history.

Standard rooms created:
  bureau/agents      Agent registry and presence
  bureau/system      Operational messages
  bureau/machines    Machine keys and status
  bureau/services    Service directory`,
		Usage: "bureau matrix setup [flags]",
		Examples: []cli.Example{
			{
				Description: "Bootstrap with token from a file",
				Command:     "bureau matrix setup --registration-token-file /run/secrets/matrix-token --credential-file /etc/bureau/matrix-creds",
			},
			{
				Description: "Bootstrap with token from stdin",
				Command:     "echo $TOKEN | bureau matrix setup --registration-token-file - --credential-file ./creds",
			},
			{
				Description: "Bootstrap and invite a user to all rooms",
				Command:     "bureau matrix setup --registration-token-file ./token --credential-file ./creds --invite @alice:bureau.local",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("setup", pflag.ContinueOnError)
			flagSet.StringVar(&homeserverURL, "homeserver", "http://localhost:6167", "Matrix homeserver URL")
			flagSet.StringVar(&registrationTokenFile, "registration-token-file", "", "path to file containing registration token, or - for stdin (required)")
			flagSet.StringVar(&credentialFile, "credential-file", "", "path to write Bureau credentials (required)")
			flagSet.StringVar(&serverName, "server-name", "bureau.local", "Matrix server name for constructing user/room IDs")
			flagSet.StringVar(&adminUsername, "admin-user", "bureau-admin", "admin account username")
			flagSet.StringArrayVar(&inviteUsers, "invite", nil, "Matrix user ID to invite to all Bureau rooms (repeatable)")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}
			if registrationTokenFile == "" {
				return fmt.Errorf("--registration-token-file is required (use - for stdin)")
			}
			if credentialFile == "" {
				return fmt.Errorf("--credential-file is required")
			}

			registrationToken, err := readSecret(registrationTokenFile)
			if err != nil {
				return fmt.Errorf("read registration token: %w", err)
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

			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()

			return runSetup(ctx, logger, setupConfig{
				homeserverURL:     homeserverURL,
				registrationToken: registrationToken,
				credentialFile:    credentialFile,
				serverName:        serverName,
				adminUsername:     adminUsername,
				inviteUsers:       inviteUsers,
			})
		},
	}
}

type setupConfig struct {
	homeserverURL     string
	registrationToken string
	credentialFile    string
	serverName        string
	adminUsername     string
	inviteUsers       []string
}

func runSetup(ctx context.Context, logger *slog.Logger, config setupConfig) error {
	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: config.homeserverURL,
		Logger:        logger,
	})
	if err != nil {
		return fmt.Errorf("create matrix client: %w", err)
	}

	// Step 1: Register or login the admin account.
	adminPassword := deriveAdminPassword(config.registrationToken)
	session, err := registerOrLogin(ctx, client, config.adminUsername, adminPassword, config.registrationToken)
	if err != nil {
		return fmt.Errorf("get admin session: %w", err)
	}
	logger.Info("admin session established", "user_id", session.UserID())

	// Step 2: Create Bureau space.
	spaceRoomID, err := ensureSpace(ctx, session, config.serverName, logger)
	if err != nil {
		return fmt.Errorf("create bureau space: %w", err)
	}
	logger.Info("bureau space ready", "room_id", spaceRoomID)

	// Step 3: Create standard rooms.
	agentsRoomID, err := ensureRoom(ctx, session, "bureau/agents", "Bureau Agents", "Agent registry and presence",
		spaceRoomID, config.serverName, nil, logger)
	if err != nil {
		return fmt.Errorf("create agents room: %w", err)
	}
	logger.Info("agents room ready", "room_id", agentsRoomID)

	systemRoomID, err := ensureRoom(ctx, session, "bureau/system", "Bureau System", "Operational messages",
		spaceRoomID, config.serverName, nil, logger)
	if err != nil {
		return fmt.Errorf("create system room: %w", err)
	}
	logger.Info("system room ready", "room_id", systemRoomID)

	machinesRoomID, err := ensureRoom(ctx, session, "bureau/machines", "Bureau Machines", "Machine keys and status",
		spaceRoomID, config.serverName,
		[]string{schema.EventTypeMachineKey, schema.EventTypeMachineStatus}, logger)
	if err != nil {
		return fmt.Errorf("create machines room: %w", err)
	}
	logger.Info("machines room ready", "room_id", machinesRoomID)

	servicesRoomID, err := ensureRoom(ctx, session, "bureau/services", "Bureau Services", "Service directory",
		spaceRoomID, config.serverName,
		[]string{schema.EventTypeService}, logger)
	if err != nil {
		return fmt.Errorf("create services room: %w", err)
	}
	logger.Info("services room ready", "room_id", servicesRoomID)

	// Step 4: Invite users to all Bureau rooms.
	if len(config.inviteUsers) > 0 {
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
		for _, userID := range config.inviteUsers {
			for _, room := range allRooms {
				if err := session.InviteUser(ctx, room.roomID, userID); err != nil {
					if messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
						logger.Info("user already in room or invite not needed",
							"user_id", userID,
							"room", room.name,
						)
						continue
					}
					return fmt.Errorf("invite %q to %s (%s): %w", userID, room.name, room.roomID, err)
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
	if err := writeCredentials(config.credentialFile, config.homeserverURL, session, config.registrationToken,
		spaceRoomID, agentsRoomID, systemRoomID, machinesRoomID, servicesRoomID); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	logger.Info("credentials written", "path", config.credentialFile)

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
func registerOrLogin(ctx context.Context, client *messaging.Client, username, password, registrationToken string) (*messaging.Session, error) {
	session, err := client.Register(ctx, messaging.RegisterRequest{
		Username:          username,
		Password:          password,
		RegistrationToken: registrationToken,
	})
	if err == nil {
		return session, nil
	}

	if messaging.IsMatrixError(err, messaging.ErrCodeUserInUse) {
		slog.Info("admin account already exists, logging in", "username", username)
		return client.Login(ctx, username, password)
	}

	return nil, err
}

// ensureSpace creates the Bureau space if it doesn't exist.
func ensureSpace(ctx context.Context, session *messaging.Session, serverName string, logger *slog.Logger) (string, error) {
	alias := fmt.Sprintf("#bureau:%s", serverName)

	roomID, err := session.ResolveAlias(ctx, alias)
	if err == nil {
		logger.Info("bureau space already exists", "alias", alias, "room_id", roomID)
		return roomID, nil
	}
	if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return "", fmt.Errorf("resolve alias %q: %w", alias, err)
	}

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
		return "", fmt.Errorf("create bureau space: %w", err)
	}
	return response.RoomID, nil
}

// ensureRoom creates a room if it doesn't exist and adds it as a child of the space.
//
// memberSettableEventTypes lists Bureau-specific state event types that room
// members at the default power level (0) are allowed to set.
func ensureRoom(ctx context.Context, session *messaging.Session, aliasLocal, name, topic, spaceRoomID, serverName string, memberSettableEventTypes []string, logger *slog.Logger) (string, error) {
	alias := fmt.Sprintf("#%s:%s", aliasLocal, serverName)

	roomID, err := session.ResolveAlias(ctx, alias)
	if err == nil {
		logger.Info("room already exists", "alias", alias, "room_id", roomID)
		return roomID, nil
	}
	if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return "", fmt.Errorf("resolve alias %q: %w", alias, err)
	}

	response, err := session.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:                      name,
		Alias:                     aliasLocal,
		Topic:                     topic,
		Preset:                    "private_chat",
		Visibility:                "private",
		PowerLevelContentOverride: adminOnlyPowerLevels(session.UserID(), memberSettableEventTypes),
	})
	if err != nil {
		return "", fmt.Errorf("create room %q: %w", aliasLocal, err)
	}

	_, err = session.SendStateEvent(ctx, spaceRoomID, "m.space.child", response.RoomID,
		map[string]any{
			"via": []string{serverName},
		})
	if err != nil {
		return "", fmt.Errorf("add room %q as child of space: %w", aliasLocal, err)
	}

	return response.RoomID, nil
}

// writeCredentials writes Bureau credentials to a file in key=value format
// compatible with proxy/credentials.go:FileCredentialSource.
func writeCredentials(path, homeserverURL string, session *messaging.Session, registrationToken, spaceRoomID, agentsRoomID, systemRoomID, machinesRoomID, servicesRoomID string) error {
	var builder strings.Builder
	builder.WriteString("# Bureau Matrix credentials\n")
	builder.WriteString("# Written by bureau matrix setup. Do not edit manually.\n")
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
// perform administrative actions. Members at power level 0 can send messages.
//
// memberSettableEventTypes lists state event types that members at power level
// 0 are allowed to set. This is used for Bureau-specific events: machines
// publish their own m.bureau.machine_key in #bureau/machines, services register
// via m.bureau.service in #bureau/services.
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
// registration token via SHA-256 with a domain separator. This ensures re-running
// setup with the same token produces the same password (idempotency).
//
// Acceptable because the registration token is high-entropy random material
// (openssl rand -hex 32) and the password only needs to resist online attacks
// rate-limited by the homeserver.
func deriveAdminPassword(registrationToken string) string {
	hash := sha256.Sum256([]byte("bureau-admin-password:" + registrationToken))
	return hex.EncodeToString(hash[:])
}
