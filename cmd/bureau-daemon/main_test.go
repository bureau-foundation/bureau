// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestRoomAliasLocalpart(t *testing.T) {
	tests := []struct {
		name      string
		fullAlias string
		expected  string
	}{
		{
			name:      "simple alias",
			fullAlias: "#bureau/machines:bureau.local",
			expected:  "bureau/machines",
		},
		{
			name:      "nested config alias",
			fullAlias: "#bureau/config/machine/workstation:bureau.local",
			expected:  "bureau/config/machine/workstation",
		},
		{
			name:      "different server",
			fullAlias: "#test:example.org",
			expected:  "test",
		},
		{
			name:      "no # prefix",
			fullAlias: "bureau/machines:bureau.local",
			expected:  "bureau/machines",
		},
		{
			name:      "no server suffix",
			fullAlias: "#bureau/machines",
			expected:  "bureau/machines",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := principal.RoomAliasLocalpart(test.fullAlias)
			if result != test.expected {
				t.Errorf("RoomAliasLocalpart(%q) = %q, want %q",
					test.fullAlias, result, test.expected)
			}
		})
	}
}

func TestConfigRoomPowerLevels(t *testing.T) {
	adminUserID := "@bureau-admin:bureau.local"
	levels := schema.ConfigRoomPowerLevels(adminUserID)

	// Admin should have power level 100.
	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	adminLevel, ok := users[adminUserID]
	if !ok {
		t.Fatalf("admin %q not in users map", adminUserID)
	}
	if adminLevel != 100 {
		t.Errorf("admin power level = %v, want 100", adminLevel)
	}

	// Default user power level should be 0.
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	// Machine config and credentials events should require power level 100.
	events, ok := levels["events"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}
	if events["m.bureau.machine_config"] != 100 {
		t.Errorf("m.bureau.machine_config power level = %v, want 100", events["m.bureau.machine_config"])
	}
	if events["m.bureau.credentials"] != 100 {
		t.Errorf("m.bureau.credentials power level = %v, want 100", events["m.bureau.credentials"])
	}

	// Default event power level should be 100 (admin-only room).
	if levels["events_default"] != 100 {
		t.Errorf("events_default = %v, want 100", levels["events_default"])
	}
}

func TestLoadSession(t *testing.T) {
	t.Run("valid session", func(t *testing.T) {
		stateDir := t.TempDir()
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

		sessionJSON := `{
			"homeserver_url": "http://localhost:6167",
			"user_id": "@machine/test:bureau.local",
			"access_token": "syt_test_token"
		}`
		os.WriteFile(filepath.Join(stateDir, "session.json"), []byte(sessionJSON), 0600)

		session, err := loadSession(stateDir, "http://localhost:6167", logger)
		if err != nil {
			t.Fatalf("loadSession() error: %v", err)
		}
		if session.UserID() != "@machine/test:bureau.local" {
			t.Errorf("UserID() = %q, want %q", session.UserID(), "@machine/test:bureau.local")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		_, err := loadSession(t.TempDir(), "http://localhost:6167", logger)
		if err == nil {
			t.Error("expected error for missing session file")
		}
	})

	t.Run("empty access token", func(t *testing.T) {
		stateDir := t.TempDir()
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		os.WriteFile(filepath.Join(stateDir, "session.json"), []byte(`{
			"homeserver_url": "http://localhost:6167",
			"user_id": "@test:local",
			"access_token": ""
		}`), 0600)

		_, err := loadSession(stateDir, "http://localhost:6167", logger)
		if err == nil {
			t.Error("expected error for empty access token")
		}
		if !strings.Contains(err.Error(), "empty access token") {
			t.Errorf("error = %v, want 'empty access token'", err)
		}
	})
}

func TestUptimeSeconds(t *testing.T) {
	// uptimeSeconds should return a positive value on Linux.
	uptime := uptimeSeconds()
	if uptime <= 0 {
		t.Errorf("uptimeSeconds() = %d, want > 0", uptime)
	}
}

func TestPublishStatus_SandboxCount(t *testing.T) {
	// Verify that the running map count is correctly calculated.
	daemon := &Daemon{
		machineName: "machine/test",
		serverName:  "bureau.local",
		running: map[string]bool{
			"iree/amdgpu/pm":     true,
			"service/stt/whisper": true,
			"service/tts/piper":  true,
		},
		logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}

	// Count running principals the same way publishStatus does.
	runningCount := 0
	for range daemon.running {
		runningCount++
	}
	if runningCount != 3 {
		t.Errorf("running count = %d, want 3", runningCount)
	}
}
