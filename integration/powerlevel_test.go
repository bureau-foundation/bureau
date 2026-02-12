// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// TestPowerLevelEnforcement verifies that Continuwuity actually enforces the
// power level constraints Bureau configures in its rooms. Previous tests check
// that Bureau sends the right power levels and that the proxy gates membership
// operations, but nothing confirmed that the homeserver rejects writes from
// underprivileged users. This test creates users at each power level tier
// (admin=100, machine=50, agent=0) and exercises state event writes, message
// sends, and invites against all four room types Bureau uses.
func TestPowerLevelEnforcement(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	admin := adminSession(t)
	defer admin.Close()

	adminUserID := "@bureau-admin:" + testServerName

	// Register test users. These get default power level 0 in any room they
	// join. The machine user will be promoted to PL 50 via room power levels.
	machineAccount := registerPrincipal(t, "test/pl-machine", "machine-test-pw")
	agentAccount := registerPrincipal(t, "test/pl-agent", "agent-test-pw")
	// A third user for invite tests — registered but never joined to rooms.
	bystander := registerPrincipal(t, "test/pl-bystander", "bystander-test-pw")

	machineSession := principalSession(t, machineAccount)
	defer machineSession.Close()
	agentSession := principalSession(t, agentAccount)
	defer agentSession.Close()

	// --- Create test rooms with Bureau's real power level structures ---

	configRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:                      "PL Test Config Room",
		Preset:                    "private_chat",
		Invite:                    []string{machineAccount.UserID, agentAccount.UserID},
		PowerLevelContentOverride: schema.ConfigRoomPowerLevels(adminUserID, machineAccount.UserID),
	})
	if err != nil {
		t.Fatalf("create config room: %v", err)
	}

	workspaceRoom, err := admin.CreateRoom(ctx, messaging.CreateRoomRequest{
		Name:                      "PL Test Workspace Room",
		Preset:                    "private_chat",
		Invite:                    []string{machineAccount.UserID, agentAccount.UserID},
		PowerLevelContentOverride: schema.WorkspaceRoomPowerLevels(adminUserID, machineAccount.UserID),
	})
	if err != nil {
		t.Fatalf("create workspace room: %v", err)
	}

	// Join both users to both rooms.
	if _, err := machineSession.JoinRoom(ctx, configRoom.RoomID); err != nil {
		t.Fatalf("machine join config room: %v", err)
	}
	if _, err := agentSession.JoinRoom(ctx, configRoom.RoomID); err != nil {
		t.Fatalf("agent join config room: %v", err)
	}
	if _, err := machineSession.JoinRoom(ctx, workspaceRoom.RoomID); err != nil {
		t.Fatalf("machine join workspace room: %v", err)
	}
	if _, err := agentSession.JoinRoom(ctx, workspaceRoom.RoomID); err != nil {
		t.Fatalf("agent join workspace room: %v", err)
	}

	// Resolve global rooms (created by TestMain's bureau matrix setup) and
	// join the test users so we can test their power levels there too.
	machineRoomID, err := admin.ResolveAlias(ctx, "#bureau/machine:"+testServerName)
	if err != nil {
		t.Fatalf("resolve machine room: %v", err)
	}
	serviceRoomID, err := admin.ResolveAlias(ctx, "#bureau/service:"+testServerName)
	if err != nil {
		t.Fatalf("resolve service room: %v", err)
	}

	if err := admin.InviteUser(ctx, machineRoomID, agentAccount.UserID); err != nil {
		t.Fatalf("invite agent to machine room: %v", err)
	}
	if _, err := agentSession.JoinRoom(ctx, machineRoomID); err != nil {
		t.Fatalf("agent join machine room: %v", err)
	}
	if err := admin.InviteUser(ctx, serviceRoomID, agentAccount.UserID); err != nil {
		t.Fatalf("invite agent to service room: %v", err)
	}
	if _, err := agentSession.JoinRoom(ctx, serviceRoomID); err != nil {
		t.Fatalf("agent join service room: %v", err)
	}

	// --- Config room tests ---
	// Power levels: admin=100, machine=50, agent=0
	// events_default: 100, state_default: 100
	// m.bureau.machine_config: 100, m.bureau.credentials: 100
	// m.bureau.layout: 0, m.room.message: 0

	t.Run("ConfigRoom", func(t *testing.T) {
		t.Run("AdminCanSetConfig", func(t *testing.T) {
			_, err := admin.SendStateEvent(ctx, configRoom.RoomID,
				schema.EventTypeMachineConfig, "test-key", map[string]any{
					"principals": []any{},
				})
			if err != nil {
				t.Fatalf("admin should be able to set machine config: %v", err)
			}
		})

		t.Run("MachineCannotSetConfig", func(t *testing.T) {
			_, err := machineSession.SendStateEvent(ctx, configRoom.RoomID,
				schema.EventTypeMachineConfig, "test-key", map[string]any{
					"principals": []any{},
				})
			assertForbidden(t, err, "machine (PL 50) setting m.bureau.machine_config (PL 100)")
		})

		t.Run("AgentCannotSetConfig", func(t *testing.T) {
			_, err := agentSession.SendStateEvent(ctx, configRoom.RoomID,
				schema.EventTypeMachineConfig, "test-key", map[string]any{
					"principals": []any{},
				})
			assertForbidden(t, err, "agent (PL 0) setting m.bureau.machine_config (PL 100)")
		})

		t.Run("AgentCanSendMessage", func(t *testing.T) {
			_, err := agentSession.SendMessage(ctx, configRoom.RoomID,
				messaging.NewTextMessage("power level test message"))
			if err != nil {
				t.Fatalf("agent should be able to send messages (PL 0): %v", err)
			}
		})

		t.Run("AgentCannotSetArbitraryState", func(t *testing.T) {
			// events_default is 100 in config rooms, so unlisted state event
			// types require PL 100.
			_, err := agentSession.SendStateEvent(ctx, configRoom.RoomID,
				"m.bureau.unknown_type", "test-key", map[string]any{
					"data": "should be rejected",
				})
			assertForbidden(t, err, "agent (PL 0) setting unlisted state event (events_default: 100)")
		})

		t.Run("MachineCanSetLayout", func(t *testing.T) {
			_, err := machineSession.SendStateEvent(ctx, configRoom.RoomID,
				schema.EventTypeLayout, "test-key", map[string]any{
					"sessions": []any{},
				})
			if err != nil {
				t.Fatalf("machine should be able to set layout (PL 0): %v", err)
			}
		})

		t.Run("AgentCannotSetCredentials", func(t *testing.T) {
			_, err := agentSession.SendStateEvent(ctx, configRoom.RoomID,
				schema.EventTypeCredentials, "test-key", map[string]any{
					"version":    1,
					"ciphertext": "fake",
				})
			assertForbidden(t, err, "agent (PL 0) setting m.bureau.credentials (PL 100)")
		})
	})

	// --- Workspace room tests ---
	// Power levels: admin=100, machine=50, agent=0
	// events_default: 0, state_default: 100
	// m.bureau.workspace: 0, m.bureau.layout: 0
	// m.bureau.project: 100, m.room.power_levels: 100
	// invite: 50

	t.Run("WorkspaceRoom", func(t *testing.T) {
		t.Run("AgentCanSendMessage", func(t *testing.T) {
			_, err := agentSession.SendMessage(ctx, workspaceRoom.RoomID,
				messaging.NewTextMessage("workspace power level test"))
			if err != nil {
				t.Fatalf("agent should be able to send messages (events_default: 0): %v", err)
			}
		})

		t.Run("AgentCanSetWorkspaceState", func(t *testing.T) {
			_, err := agentSession.SendStateEvent(ctx, workspaceRoom.RoomID,
				schema.EventTypeWorkspace, "", map[string]any{
					"status": "active",
				})
			if err != nil {
				t.Fatalf("agent should be able to set workspace state (PL 0): %v", err)
			}
		})

		t.Run("AgentCannotSetProject", func(t *testing.T) {
			_, err := agentSession.SendStateEvent(ctx, workspaceRoom.RoomID,
				schema.EventTypeProject, "test-project", map[string]any{
					"repository": "https://example.com/repo",
				})
			assertForbidden(t, err, "agent (PL 0) setting m.bureau.project (PL 100)")
		})

		t.Run("AgentCannotModifyPowerLevels", func(t *testing.T) {
			_, err := agentSession.SendStateEvent(ctx, workspaceRoom.RoomID,
				"m.room.power_levels", "", map[string]any{
					"users_default": 100,
				})
			assertForbidden(t, err, "agent (PL 0) modifying m.room.power_levels (PL 100)")
		})

		t.Run("MachineCanInvite", func(t *testing.T) {
			err := machineSession.InviteUser(ctx, workspaceRoom.RoomID, bystander.UserID)
			if err != nil {
				t.Fatalf("machine (PL 50) should be able to invite (invite: 50): %v", err)
			}
		})

		t.Run("AgentCannotInvite", func(t *testing.T) {
			// Use a different user ID to avoid "already invited" errors
			// conflicting with the forbidden check. Register a throwaway user.
			throwaway := registerPrincipal(t, "test/pl-throwaway", "throwaway-pw")
			err := agentSession.InviteUser(ctx, workspaceRoom.RoomID, throwaway.UserID)
			assertForbidden(t, err, "agent (PL 0) inviting to workspace room (invite: 50)")
		})

		t.Run("AgentCanSetLayout", func(t *testing.T) {
			_, err := agentSession.SendStateEvent(ctx, workspaceRoom.RoomID,
				schema.EventTypeLayout, "test-key", map[string]any{
					"sessions": []any{},
				})
			if err != nil {
				t.Fatalf("agent should be able to set layout (PL 0): %v", err)
			}
		})
	})

	// --- Machine room tests (global, from setup) ---
	// Power levels: admin=100, users_default=0
	// events_default: 0, state_default: 100
	// Member-settable (PL 0): machine_key, machine_info, machine_status,
	//   webrtc_offer, webrtc_answer
	// All room metadata (m.room.*): 100

	t.Run("MachineRoom", func(t *testing.T) {
		t.Run("MemberCanSetMachineKey", func(t *testing.T) {
			_, err := agentSession.SendStateEvent(ctx, machineRoomID,
				schema.EventTypeMachineKey, "test/pl-agent", map[string]any{
					"public_key": "age1test-fake-key-for-power-level-test",
				})
			if err != nil {
				t.Fatalf("member should be able to set machine_key (PL 0): %v", err)
			}
		})

		t.Run("MemberCanSetMachineStatus", func(t *testing.T) {
			_, err := agentSession.SendStateEvent(ctx, machineRoomID,
				schema.EventTypeMachineStatus, "test/pl-agent", map[string]any{
					"uptime_seconds": 42,
				})
			if err != nil {
				t.Fatalf("member should be able to set machine_status (PL 0): %v", err)
			}
		})

		t.Run("MemberCannotSetRoomName", func(t *testing.T) {
			_, err := agentSession.SendStateEvent(ctx, machineRoomID,
				"m.room.name", "", map[string]any{
					"name": "hijacked room name",
				})
			assertForbidden(t, err, "member (PL 0) setting m.room.name (PL 100)")
		})

		t.Run("MemberCannotSetArbitraryState", func(t *testing.T) {
			// state_default is 100, so unlisted state types should be rejected.
			_, err := agentSession.SendStateEvent(ctx, machineRoomID,
				"m.bureau.malicious_event", "test-key", map[string]any{
					"data": "should be rejected",
				})
			assertForbidden(t, err, "member (PL 0) setting unlisted state event (state_default: 100)")
		})
	})

	// --- Service room tests (global, from setup) ---
	// Power levels: admin=100, users_default=0
	// events_default: 0, state_default: 100
	// Member-settable (PL 0): m.bureau.service

	t.Run("ServiceRoom", func(t *testing.T) {
		t.Run("MemberCanRegisterService", func(t *testing.T) {
			_, err := agentSession.SendStateEvent(ctx, serviceRoomID,
				schema.EventTypeService, "test/pl-agent", map[string]any{
					"service_type": "test",
					"endpoint":     "unix:///run/bureau/service/test.sock",
				})
			if err != nil {
				t.Fatalf("member should be able to register service (PL 0): %v", err)
			}
		})

		t.Run("MemberCannotSetRoomTopic", func(t *testing.T) {
			_, err := agentSession.SendStateEvent(ctx, serviceRoomID,
				"m.room.topic", "", map[string]any{
					"topic": "hijacked topic",
				})
			assertForbidden(t, err, "member (PL 0) setting m.room.topic (PL 100)")
		})
	})
}

// assertForbidden checks that err is a Matrix M_FORBIDDEN error. If the
// operation succeeded (nil error) or returned a different error, it fails
// the test with a message describing what was expected to be forbidden.
func assertForbidden(t *testing.T, err error, operation string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected M_FORBIDDEN for %s, but operation succeeded", operation)
	}
	if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
		t.Fatalf("expected M_FORBIDDEN for %s, got: %v", operation, err)
	}
}
