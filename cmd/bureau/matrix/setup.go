// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"bufio"
	"bytes"
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
	"github.com/bureau-foundation/bureau/lib/content"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
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
  bureau/system      Operational messages
  bureau/machines    Machine keys and status
  bureau/services    Service directory
  bureau/template    Sandbox templates (base, base-networked)
  bureau/pipeline    Pipeline definitions (dev-workspace-init, dev-workspace-deinit)`,
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
			defer registrationToken.Close()

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
	registrationToken *secret.Buffer
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
	adminPassword, err := deriveAdminPassword(config.registrationToken.String())
	if err != nil {
		return fmt.Errorf("derive admin password: %w", err)
	}
	defer adminPassword.Close()
	session, err := registerOrLogin(ctx, client, config.adminUsername, adminPassword.String(), config.registrationToken.String())
	if err != nil {
		return fmt.Errorf("get admin session: %w", err)
	}
	defer session.Close()
	logger.Info("admin session established", "user_id", session.UserID())

	// Step 2: Create Bureau space.
	spaceRoomID, err := ensureSpace(ctx, session, config.serverName, logger)
	if err != nil {
		return fmt.Errorf("create bureau space: %w", err)
	}
	logger.Info("bureau space ready", "room_id", spaceRoomID)

	// Step 3: Create standard rooms.
	systemRoomID, err := ensureRoom(ctx, session, "bureau/system", "Bureau System", "Operational messages",
		spaceRoomID, config.serverName, adminOnlyPowerLevels(session.UserID(), nil), logger)
	if err != nil {
		return fmt.Errorf("create system room: %w", err)
	}
	logger.Info("system room ready", "room_id", systemRoomID)

	machinesRoomID, err := ensureRoom(ctx, session, "bureau/machines", "Bureau Machines", "Machine keys and status",
		spaceRoomID, config.serverName,
		adminOnlyPowerLevels(session.UserID(), []string{
			schema.EventTypeMachineKey,
			schema.EventTypeMachineInfo,
			schema.EventTypeMachineStatus,
			schema.EventTypeWebRTCOffer,
			schema.EventTypeWebRTCAnswer,
		}), logger)
	if err != nil {
		return fmt.Errorf("create machines room: %w", err)
	}
	logger.Info("machines room ready", "room_id", machinesRoomID)

	servicesRoomID, err := ensureRoom(ctx, session, "bureau/services", "Bureau Services", "Service directory",
		spaceRoomID, config.serverName,
		adminOnlyPowerLevels(session.UserID(), []string{schema.EventTypeService}), logger)
	if err != nil {
		return fmt.Errorf("create services room: %w", err)
	}
	logger.Info("services room ready", "room_id", servicesRoomID)

	templateRoomID, err := ensureRoom(ctx, session, "bureau/template", "Bureau Template", "Sandbox templates",
		spaceRoomID, config.serverName, adminOnlyPowerLevels(session.UserID(), nil), logger)
	if err != nil {
		return fmt.Errorf("create template room: %w", err)
	}
	logger.Info("template room ready", "room_id", templateRoomID)

	// Step 3b: Publish base templates into the template room.
	if err := publishBaseTemplates(ctx, session, templateRoomID, logger); err != nil {
		return fmt.Errorf("publish base templates: %w", err)
	}

	pipelineRoomID, err := ensureRoom(ctx, session, "bureau/pipeline", "Bureau Pipeline", "Pipeline definitions",
		spaceRoomID, config.serverName, schema.PipelineRoomPowerLevels(session.UserID()), logger)
	if err != nil {
		return fmt.Errorf("create pipeline room: %w", err)
	}
	logger.Info("pipeline room ready", "room_id", pipelineRoomID)

	// Step 3c: Publish base pipelines into the pipeline room.
	if err := publishBasePipelines(ctx, session, pipelineRoomID, logger); err != nil {
		return fmt.Errorf("publish base pipelines: %w", err)
	}

	// Step 4: Invite users to all Bureau rooms.
	if len(config.inviteUsers) > 0 {
		allRooms := []struct {
			name   string
			roomID string
		}{
			{"bureau (space)", spaceRoomID},
			{"bureau/system", systemRoomID},
			{"bureau/machines", machinesRoomID},
			{"bureau/services", servicesRoomID},
			{"bureau/template", templateRoomID},
			{"bureau/pipeline", pipelineRoomID},
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
		spaceRoomID, systemRoomID, machinesRoomID, servicesRoomID, templateRoomID, pipelineRoomID); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	logger.Info("credentials written", "path", config.credentialFile)

	logger.Info("bureau matrix setup complete",
		"admin_user", session.UserID(),
		"space", spaceRoomID,
		"system_room", systemRoomID,
		"machines_room", machinesRoomID,
		"services_room", servicesRoomID,
		"template_room", templateRoomID,
		"pipeline_room", pipelineRoomID,
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
// The powerLevels parameter sets the room's power level structure directly.
func ensureRoom(ctx context.Context, session *messaging.Session, aliasLocal, name, topic, spaceRoomID, serverName string, powerLevels map[string]any, logger *slog.Logger) (string, error) {
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
		PowerLevelContentOverride: powerLevels,
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
func writeCredentials(path, homeserverURL string, session *messaging.Session, registrationToken *secret.Buffer, spaceRoomID, systemRoomID, machinesRoomID, servicesRoomID, templateRoomID, pipelineRoomID string) error {
	var builder strings.Builder
	builder.WriteString("# Bureau Matrix credentials\n")
	builder.WriteString("# Written by bureau matrix setup. Do not edit manually.\n")
	builder.WriteString("#\n")
	fmt.Fprintf(&builder, "MATRIX_HOMESERVER_URL=%s\n", homeserverURL)
	fmt.Fprintf(&builder, "MATRIX_ADMIN_USER=%s\n", session.UserID())
	fmt.Fprintf(&builder, "MATRIX_ADMIN_TOKEN=%s\n", session.AccessToken())
	fmt.Fprintf(&builder, "MATRIX_REGISTRATION_TOKEN=%s\n", registrationToken.String())
	fmt.Fprintf(&builder, "MATRIX_SPACE_ROOM=%s\n", spaceRoomID)
	fmt.Fprintf(&builder, "MATRIX_SYSTEM_ROOM=%s\n", systemRoomID)
	fmt.Fprintf(&builder, "MATRIX_MACHINES_ROOM=%s\n", machinesRoomID)
	fmt.Fprintf(&builder, "MATRIX_SERVICES_ROOM=%s\n", servicesRoomID)
	fmt.Fprintf(&builder, "MATRIX_TEMPLATE_ROOM=%s\n", templateRoomID)
	fmt.Fprintf(&builder, "MATRIX_PIPELINE_ROOM=%s\n", pipelineRoomID)

	return os.WriteFile(path, []byte(builder.String()), 0600)
}

// publishBaseTemplates publishes the built-in sandbox templates to the
// template room as m.bureau.template state events. Idempotent: re-publishing
// overwrites the existing state event with the same content.
func publishBaseTemplates(ctx context.Context, session *messaging.Session, templateRoomID string, logger *slog.Logger) error {
	for _, template := range baseTemplates() {
		_, err := session.SendStateEvent(ctx, templateRoomID, schema.EventTypeTemplate, template.name, template.content)
		if err != nil {
			return fmt.Errorf("publishing template %q: %w", template.name, err)
		}
		logger.Info("published template", "name", template.name, "room_id", templateRoomID)
	}
	return nil
}

// publishBasePipelines publishes the embedded pipeline definitions to the
// pipeline room as m.bureau.pipeline state events. Idempotent: re-publishing
// overwrites the existing state event with the same content.
func publishBasePipelines(ctx context.Context, session *messaging.Session, pipelineRoomID string, logger *slog.Logger) error {
	pipelines, err := content.Pipelines()
	if err != nil {
		return fmt.Errorf("loading embedded pipelines: %w", err)
	}
	for _, pipeline := range pipelines {
		_, err := session.SendStateEvent(ctx, pipelineRoomID, schema.EventTypePipeline, pipeline.Name, pipeline.Content)
		if err != nil {
			return fmt.Errorf("publishing pipeline %q: %w", pipeline.Name, err)
		}
		logger.Info("published pipeline", "name", pipeline.Name, "room_id", pipelineRoomID)
	}
	return nil
}

// namedTemplate pairs a template state key (name) with its content.
type namedTemplate struct {
	name    string
	content schema.TemplateContent
}

// baseTemplates returns the built-in Bureau sandbox templates.
//
// "base" is the minimal sandbox skeleton: full namespace isolation, strict
// security defaults, and only the bare minimum mounts (/proc, /dev, tmpfs
// /tmp). It intentionally has no opinions about OS libraries — those come
// from the Nix EnvironmentPath or from template-level mounts that inherit
// from base.
//
// "base-networked" inherits from base and disables network namespace
// isolation. Use for sandboxes that need direct host network access (e.g.,
// services binding to host ports, agents that need raw TCP rather than
// going through the proxy/bridge).
func baseTemplates() []namedTemplate {
	return []namedTemplate{
		{
			name: "base",
			content: schema.TemplateContent{
				Description: "Minimal sandbox: full namespace isolation, strict security, bare mounts",
				Namespaces: &schema.TemplateNamespaces{
					PID: true,
					Net: true,
					IPC: true,
					UTS: true,
				},
				Security: &schema.TemplateSecurity{
					NewSession:    true,
					DieWithParent: true,
					NoNewPrivs:    true,
				},
				Filesystem: []schema.TemplateMount{
					{Dest: "/tmp", Type: "tmpfs"},
				},
				CreateDirs: []string{
					"/tmp",
					"/var/tmp",
					"/run/bureau",
				},
				EnvironmentVariables: map[string]string{
					"HOME": "/workspace",
					"TERM": "xterm-256color",
				},
			},
		},
		{
			name: "base-networked",
			content: schema.TemplateContent{
				Description: "Base sandbox with host network access (no network namespace isolation)",
				Inherits:    "bureau/template:base",
				Namespaces: &schema.TemplateNamespaces{
					PID: true,
					Net: false,
					IPC: true,
					UTS: true,
				},
			},
		},
	}
}

// adminOnlyPowerLevels returns power level settings where only the admin can
// perform administrative actions. Members at power level 0 can send messages.
//
// memberSettableEventTypes lists state event types that members at power level
// 0 are allowed to set. This is used for Bureau-specific events: machines
// publish m.bureau.machine_key, m.bureau.machine_info, and
// m.bureau.machine_status; daemons exchange WebRTC signaling
// (m.bureau.webrtc_offer/answer); and services register via m.bureau.service.
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
// The returned buffer is mmap-backed (locked into RAM, excluded from core
// dumps) and must be closed by the caller. Leading/trailing whitespace is
// trimmed before storing. Returns an error if the source is empty after
// trimming.
func readSecret(path string) (*secret.Buffer, error) {
	var data []byte

	if path == "-" {
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			if scanErr := scanner.Err(); scanErr != nil {
				return nil, fmt.Errorf("reading stdin: %w", scanErr)
			}
			return nil, fmt.Errorf("stdin is empty")
		}
		data = scanner.Bytes()
	} else {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		for index := range data {
			data[index] = 0
		}
		return nil, fmt.Errorf("secret is empty")
	}

	// NewFromBytes copies into mmap-backed memory and zeros trimmed.
	buffer, err := secret.NewFromBytes(trimmed)
	// Zero remaining bytes (whitespace prefix/suffix) not covered by trimmed.
	for index := range data {
		data[index] = 0
	}
	if err != nil {
		return nil, err
	}
	return buffer, nil
}

// deriveAdminPassword deterministically derives the admin password from the
// registration token via SHA-256 with a domain separator. The result is
// returned in an mmap-backed buffer; the caller must close it.
//
// This ensures re-running setup with the same token produces the same
// password (idempotency). Acceptable because the registration token is
// high-entropy random material (openssl rand -hex 32) and the password
// only needs to resist online attacks rate-limited by the homeserver.
func deriveAdminPassword(registrationToken string) (*secret.Buffer, error) {
	hash := sha256.Sum256([]byte("bureau-admin-password:" + registrationToken))
	hexBytes := []byte(hex.EncodeToString(hash[:]))
	return secret.NewFromBytes(hexBytes)
}
