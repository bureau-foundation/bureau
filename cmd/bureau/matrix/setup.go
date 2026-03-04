// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/content"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/messaging"
)

// SetupCommand returns the "setup" subcommand for bootstrapping a Matrix
// homeserver. This is the only matrix subcommand that talks directly to
// the homeserver (no proxy) since it runs before any agent infrastructure
// exists.
// setupParams holds the parameters for the matrix setup command. Credential and
// file path flags are excluded from MCP schema since they involve reading
// secrets from files/stdin.
type setupParams struct {
	HomeserverURL         string   `json:"-"            flag:"homeserver"              desc:"Matrix homeserver URL" default:"http://localhost:6167"`
	RegistrationTokenFile string   `json:"-"            flag:"registration-token-file" desc:"path to file containing registration token, or - for stdin"`
	CredentialFile        string   `json:"-"            flag:"credential-file"         desc:"path to Bureau credentials file (read on re-run, written on first run; required)"`
	ServerName            string   `json:"server_name"  flag:"server-name"             desc:"Matrix server name for constructing user/room IDs (auto-detected from machine.conf)"`
	Namespace             string   `json:"namespace"    flag:"namespace"               desc:"namespace prefix for room aliases (space, system, template, etc.)" default:"bureau"`
	AdminUsername         string   `json:"admin_user"   flag:"admin-user"              desc:"admin account username" default:"bureau-admin"`
	InviteUsers           []string `json:"invite_users" flag:"invite"                  desc:"Matrix user ID to invite to all Bureau rooms (repeatable)"`
}

func SetupCommand() *cli.Command {
	var params setupParams

	return &cli.Command{
		Name:    "setup",
		Summary: "Bootstrap a Matrix homeserver for Bureau",
		Description: `Bootstrap a Matrix homeserver for Bureau. Creates the admin account,
namespace space, and standard rooms. Safe to re-run: all operations are
idempotent.

The registration token is read from a file (or stdin with "-") to avoid
exposing secrets in CLI arguments, process listings, or shell history.

The --namespace flag controls the room alias prefix (default: "bureau").
Standard rooms created under the namespace:
  <namespace>/system      Operational messages
  <namespace>/template    Sandbox templates (base, base-networked)
  <namespace>/pipeline    Pipeline definitions (dev-workspace-init, dev-workspace-deinit)
  <namespace>/artifact    Artifact coordination
  <namespace>/dev         Development team coordination and work routing

Fleet-scoped rooms (machine, service, fleet config) are created per-fleet
by "bureau fleet enable" and resolved via the fleet prefix.`,
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
		Annotations:    cli.Create(),
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/setup"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}
			if params.CredentialFile == "" {
				return cli.Validation("--credential-file is required")
			}

			var registrationToken *secret.Buffer
			if params.RegistrationTokenFile != "" {
				var err error
				registrationToken, err = secret.ReadFromPath(params.RegistrationTokenFile)
				if err != nil {
					return cli.Internal("read registration token: %w", err)
				}
			} else {
				// No --registration-token-file: read from the credential file.
				credentials, err := cli.ReadCredentialFile(params.CredentialFile)
				if err != nil {
					return cli.Validation("--registration-token-file not provided and credential file unreadable: %w", err)
				}
				token := credentials["MATRIX_REGISTRATION_TOKEN"]
				if token == "" {
					return cli.Validation("--registration-token-file is required (credential file has no MATRIX_REGISTRATION_TOKEN)")
				}
				registrationToken, err = secret.NewFromString(token)
				if err != nil {
					return cli.Internal("protecting registration token from credential file: %w", err)
				}
			}
			defer registrationToken.Close()

			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()

			params.ServerName = cli.ResolveServerName(params.ServerName)

			serverName, err := ref.ParseServerName(params.ServerName)
			if err != nil {
				return cli.Validation("invalid --server-name %q: %w", params.ServerName, err)
			}

			return runSetup(ctx, logger, setupConfig{
				homeserverURL:     params.HomeserverURL,
				registrationToken: registrationToken,
				credentialFile:    params.CredentialFile,
				serverName:        serverName,
				namespace:         params.Namespace,
				adminUsername:     params.AdminUsername,
				inviteUsers:       params.InviteUsers,
			})
		},
	}
}

type setupConfig struct {
	homeserverURL     string
	registrationToken *secret.Buffer
	credentialFile    string
	serverName        ref.ServerName
	namespace         string
	adminUsername     string
	inviteUsers       []string
}

func runSetup(ctx context.Context, logger *slog.Logger, config setupConfig) error {
	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: config.homeserverURL,
		Logger:        logger,
	})
	if err != nil {
		return cli.Internal("create matrix client: %w", err)
	}

	// Step 1: Register or login the admin account.
	adminPassword, err := cli.DeriveAdminPassword(config.registrationToken)
	if err != nil {
		return cli.Internal("derive admin password: %w", err)
	}
	defer adminPassword.Close()
	session, err := registerOrLogin(ctx, client, config.adminUsername, adminPassword, config.registrationToken)
	if err != nil {
		return cli.Internal("get admin session: %w", err)
	}
	defer session.Close()
	logger.Info("admin session established", "user_id", session.UserID())

	// Step 2: Create Bureau space.
	spaceRoomID, err := ensureSpace(ctx, session, config.namespace, config.serverName, logger)
	if err != nil {
		return cli.Internal("create bureau space: %w", err)
	}
	logger.Info("bureau space ready", "room_id", spaceRoomID)

	// Step 3: Create standard rooms. Room definitions come from
	// standardRoomsForNamespace — the single source of truth for room
	// aliases, names, topics, and power level structures.
	rooms := standardRoomsForNamespace(config.namespace)
	roomIDs := make(map[string]ref.RoomID, len(rooms))
	for _, room := range rooms {
		roomID, err := ensureRoom(ctx, session, room.alias, room.displayName, room.topic,
			spaceRoomID, config.serverName, room.powerLevels(session.UserID()), logger)
		if err != nil {
			return cli.Internal("create %s: %w", room.name, err)
		}
		logger.Info(room.name+" ready", "room_id", roomID)
		roomIDs[room.alias] = roomID
	}

	// Publish dev team metadata on all standard rooms. The convention
	// alias #bureau/dev:<server> points to the Bureau project's own
	// dev team room. The room may not exist yet — the alias is a
	// stable pointer that resolves when the dev team is created.
	bureauNamespace, err := ref.NewNamespace(config.serverName, config.namespace)
	if err != nil {
		return cli.Internal("construct namespace: %w", err)
	}
	devTeamContent := schema.DevTeamContent{Room: schema.DevTeamRoomAlias(bureauNamespace)}
	for _, room := range rooms {
		roomID := roomIDs[room.alias]
		if _, err := session.SendStateEvent(ctx, roomID, schema.EventTypeDevTeam, "", devTeamContent); err != nil {
			return cli.Internal("publish dev team metadata on %s: %w", room.alias, err)
		}
	}
	// Also publish on the Bureau space itself.
	if _, err := session.SendStateEvent(ctx, spaceRoomID, schema.EventTypeDevTeam, "", devTeamContent); err != nil {
		return cli.Internal("publish dev team metadata on bureau space: %w", err)
	}
	logger.Info("dev team metadata published on all bureau rooms", "dev_team", devTeamContent.Room.String())

	// Publish base templates into the template room.
	templateAlias := config.namespace + "/template"
	if templateRoomID, ok := roomIDs[templateAlias]; ok {
		if err := publishBaseTemplates(ctx, session, templateRoomID, templateAlias, logger); err != nil {
			return cli.Internal("publish base templates: %w", err)
		}
	}

	// Publish base pipelines into the pipeline room.
	pipelineAlias := config.namespace + "/pipeline"
	if pipelineRoomID, ok := roomIDs[pipelineAlias]; ok {
		if err := publishBasePipelines(ctx, session, pipelineRoomID, logger); err != nil {
			return cli.Internal("publish base pipelines: %w", err)
		}
	}

	// Step 4: Invite users to all Bureau rooms.
	if len(config.inviteUsers) > 0 {
		for _, userIDString := range config.inviteUsers {
			parsedUserID, err := ref.ParseUserID(userIDString)
			if err != nil {
				return cli.Internal("parse invite user ID %q: %w", userIDString, err)
			}
			// Invite to the space first.
			if err := inviteIfNeeded(ctx, session, spaceRoomID, "bureau (space)", parsedUserID, logger); err != nil {
				return err
			}
			for _, room := range rooms {
				roomID, ok := roomIDs[room.alias]
				if !ok {
					continue
				}
				if err := inviteIfNeeded(ctx, session, roomID, room.alias, parsedUserID, logger); err != nil {
					return err
				}
			}
		}
	}

	// Step 5: Write credentials.
	if err := writeCredentials(config.credentialFile, config.homeserverURL, session, config.registrationToken,
		spaceRoomID, rooms, roomIDs); err != nil {
		return cli.Internal("write credentials: %w", err)
	}
	logger.Info("credentials written", "path", config.credentialFile)

	logArgs := []any{
		"admin_user", session.UserID(),
		"space", spaceRoomID,
	}
	for _, room := range rooms {
		logArgs = append(logArgs, room.name, roomIDs[room.alias])
	}
	logger.Info("bureau matrix setup complete", logArgs...)
	return nil
}

// registerOrLogin registers a new account, or logs in if it already exists.
// Password and registrationToken are read but not closed — the caller retains ownership.
func registerOrLogin(ctx context.Context, client *messaging.Client, username string, password, registrationToken *secret.Buffer) (*messaging.DirectSession, error) {
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

// ensureSpace creates the namespace space if it doesn't exist.
func ensureSpace(ctx context.Context, session messaging.Session, namespacePrefix string, serverName ref.ServerName, logger *slog.Logger) (ref.RoomID, error) {
	alias := ref.MustParseRoomAlias(schema.FullRoomAlias(namespacePrefix, serverName))

	roomID, err := session.ResolveAlias(ctx, alias)
	if err == nil {
		logger.Info("space already exists", "alias", alias, "room_id", roomID)
		return roomID, nil
	}
	if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return ref.RoomID{}, cli.Internal("resolve alias %q: %w", alias, err)
	}

	response, err := session.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:       namespacePrefix,
		Alias:      namespacePrefix,
		Topic:      "Bureau agent orchestration",
		Preset:     "private_chat",
		Visibility: "private",
		CreationContent: map[string]any{
			"type": "m.space",
		},
		PowerLevelContentOverride: adminOnlyPowerLevels(session.UserID(), nil),
	})
	if err != nil {
		return ref.RoomID{}, cli.Internal("create bureau space: %w", err)
	}
	return response.RoomID, nil
}

// ensureRoom creates a room if it doesn't exist and adds it as a child of the space.
// The powerLevels parameter sets the room's power level structure directly.
func ensureRoom(ctx context.Context, session messaging.Session, aliasLocal, name, topic string, spaceRoomID ref.RoomID, serverName ref.ServerName, powerLevels map[string]any, logger *slog.Logger) (ref.RoomID, error) {
	alias := ref.MustParseRoomAlias(schema.FullRoomAlias(aliasLocal, serverName))

	roomID, err := session.ResolveAlias(ctx, alias)
	if err == nil {
		logger.Info("room already exists", "alias", alias, "room_id", roomID)
		return roomID, nil
	}
	if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return ref.RoomID{}, cli.Internal("resolve alias %q: %w", alias, err)
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
		return ref.RoomID{}, cli.Internal("create room %q: %w", aliasLocal, err)
	}

	_, err = session.SendStateEvent(ctx, spaceRoomID, "m.space.child", response.RoomID.String(),
		map[string]any{
			"via": []string{serverName.String()},
		})
	if err != nil {
		return ref.RoomID{}, cli.Internal("add room %q as child of space: %w", aliasLocal, err)
	}

	return response.RoomID, nil
}

// inviteIfNeeded invites a user to a room, ignoring "already joined" errors.
func inviteIfNeeded(ctx context.Context, session messaging.Session, roomID ref.RoomID, roomName string, userID ref.UserID, logger *slog.Logger) error {
	if err := session.InviteUser(ctx, roomID, userID); err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			logger.Info("user already in room or invite not needed",
				"user_id", userID,
				"room", roomName,
			)
			return nil
		}
		return cli.Internal("invite %q to %s (%s): %w", userID, roomName, roomID, err)
	}
	logger.Info("invited user to room",
		"user_id", userID,
		"room", roomName,
		"room_id", roomID,
	)
	return nil
}

// writeCredentials writes Bureau credentials to a file in key=value format
// compatible with proxy/credentials.go:FileCredentialSource. Room IDs are
// written in standard room order for consistency.
func writeCredentials(path, homeserverURL string, session *messaging.DirectSession, registrationToken *secret.Buffer, spaceRoomID ref.RoomID, rooms []standardRoom, roomIDs map[string]ref.RoomID) error {
	var builder strings.Builder
	builder.WriteString("# Bureau Matrix credentials\n")
	builder.WriteString("# Written by bureau matrix setup. Do not edit manually.\n")
	builder.WriteString("#\n")
	fmt.Fprintf(&builder, "MATRIX_HOMESERVER_URL=%s\n", homeserverURL)
	fmt.Fprintf(&builder, "MATRIX_ADMIN_USER=%s\n", session.UserID())
	fmt.Fprintf(&builder, "MATRIX_ADMIN_TOKEN=%s\n", session.AccessToken())
	fmt.Fprintf(&builder, "MATRIX_REGISTRATION_TOKEN=%s\n", registrationToken.String())
	fmt.Fprintf(&builder, "MATRIX_SPACE_ROOM=%s\n", spaceRoomID)
	for _, room := range rooms {
		if roomID, ok := roomIDs[room.alias]; ok {
			fmt.Fprintf(&builder, "%s=%s\n", room.credentialKey, roomID)
		}
	}

	return os.WriteFile(path, []byte(builder.String()), 0600)
}

// publishBaseTemplates publishes the built-in sandbox templates to the
// template room as m.bureau.template state events. Idempotent: re-publishing
// overwrites the existing state event with the same content.
func publishBaseTemplates(ctx context.Context, session messaging.Session, templateRoomID ref.RoomID, templatePrefix string, logger *slog.Logger) error {
	templates, err := content.Templates(templatePrefix)
	if err != nil {
		return cli.Internal("loading embedded templates: %w", err)
	}
	for _, template := range templates {
		_, err := session.SendStateEvent(ctx, templateRoomID, schema.EventTypeTemplate, template.Name, template.Content)
		if err != nil {
			return cli.Internal("publishing template %q: %w", template.Name, err)
		}
		logger.Info("published template", "name", template.Name, "room_id", templateRoomID)
	}
	return nil
}

// publishBasePipelines publishes the embedded pipeline definitions to the
// pipeline room as m.bureau.pipeline state events. Idempotent: re-publishing
// overwrites the existing state event with the same content.
func publishBasePipelines(ctx context.Context, session messaging.Session, pipelineRoomID ref.RoomID, logger *slog.Logger) error {
	pipelines, err := content.Pipelines()
	if err != nil {
		return cli.Internal("loading embedded pipelines: %w", err)
	}
	for _, pipeline := range pipelines {
		_, err := session.SendStateEvent(ctx, pipelineRoomID, schema.EventTypePipeline, pipeline.Name, pipeline.Content)
		if err != nil {
			return cli.Internal("publishing pipeline %q: %w", pipeline.Name, err)
		}
		logger.Info("published pipeline", "name", pipeline.Name, "room_id", pipelineRoomID)
	}
	return nil
}

// adminOnlyPowerLevels returns power level settings where only the admin can
// perform administrative actions. Members at power level 0 can send messages.
//
// memberSettableEventTypes lists state event types that members at power level
// 0 are allowed to set. This is used for Bureau-specific events: machine room
// members publish m.bureau.machine_key, m.bureau.machine_info, and
// m.bureau.machine_status; daemons exchange WebRTC signaling
// (m.bureau.webrtc_offer/answer); and service room members register via
// m.bureau.service.
func adminOnlyPowerLevels(adminUserID ref.UserID, memberSettableEventTypes []ref.EventType) map[string]any {
	events := schema.AdminProtectedEvents()
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
		"users":          map[string]any{adminUserID.String(): 100},
		"users_default":  0,
		"events":         events,
	}
}
