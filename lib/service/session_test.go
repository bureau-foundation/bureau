// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

		client, session, err := LoadSession(stateDir, "http://localhost:6167", logger)
		if err != nil {
			t.Fatalf("LoadSession() error: %v", err)
		}
		if client == nil {
			t.Error("LoadSession() returned nil client")
		}
		if session.UserID() != "@machine/test:bureau.local" {
			t.Errorf("UserID() = %q, want %q", session.UserID(), "@machine/test:bureau.local")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		_, _, err := LoadSession(t.TempDir(), "http://localhost:6167", logger)
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

		_, _, err := LoadSession(stateDir, "http://localhost:6167", logger)
		if err == nil {
			t.Error("expected error for empty access token")
		}
		if !strings.Contains(err.Error(), "empty access token") {
			t.Errorf("error = %v, want 'empty access token'", err)
		}
	})

	t.Run("homeserver URL override", func(t *testing.T) {
		stateDir := t.TempDir()
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		os.WriteFile(filepath.Join(stateDir, "session.json"), []byte(`{
			"homeserver_url": "http://original:6167",
			"user_id": "@test:local",
			"access_token": "syt_test_token"
		}`), 0600)

		// When homeserverURL is non-empty, it overrides the stored value.
		client, _, err := LoadSession(stateDir, "http://override:6167", logger)
		if err != nil {
			t.Fatalf("LoadSession() error: %v", err)
		}
		if client == nil {
			t.Error("LoadSession() returned nil client")
		}
	})

	t.Run("homeserver URL from file", func(t *testing.T) {
		stateDir := t.TempDir()
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		os.WriteFile(filepath.Join(stateDir, "session.json"), []byte(`{
			"homeserver_url": "http://from-file:6167",
			"user_id": "@test:local",
			"access_token": "syt_test_token"
		}`), 0600)

		// When homeserverURL is empty, the file's value is used.
		client, _, err := LoadSession(stateDir, "", logger)
		if err != nil {
			t.Fatalf("LoadSession() error: %v", err)
		}
		if client == nil {
			t.Error("LoadSession() returned nil client")
		}
	})
}

func TestSaveSession(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		stateDir := t.TempDir()
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

		// Seed a session via LoadSession so we have a *messaging.DirectSession.
		os.WriteFile(filepath.Join(stateDir, "session.json"), []byte(`{
			"homeserver_url": "http://localhost:6167",
			"user_id": "@machine/test:bureau.local",
			"access_token": "syt_round_trip_token"
		}`), 0600)

		_, session, err := LoadSession(stateDir, "http://localhost:6167", logger)
		if err != nil {
			t.Fatalf("LoadSession() error: %v", err)
		}
		defer session.Close()

		// Write to a fresh directory via SaveSession.
		outputDir := t.TempDir()
		if err := SaveSession(outputDir, "http://saved:6167", session); err != nil {
			t.Fatalf("SaveSession() error: %v", err)
		}

		// Read back and verify the JSON contents.
		data, err := os.ReadFile(filepath.Join(outputDir, "session.json"))
		if err != nil {
			t.Fatalf("reading saved session: %v", err)
		}
		var saved SessionData
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatalf("parsing saved session: %v", err)
		}
		if saved.HomeserverURL != "http://saved:6167" {
			t.Errorf("HomeserverURL = %q, want %q", saved.HomeserverURL, "http://saved:6167")
		}
		if saved.UserID != "@machine/test:bureau.local" {
			t.Errorf("UserID = %q, want %q", saved.UserID, "@machine/test:bureau.local")
		}
		if saved.AccessToken != "syt_round_trip_token" {
			t.Errorf("AccessToken = %q, want %q", saved.AccessToken, "syt_round_trip_token")
		}

		// Verify file permissions are owner-only.
		info, err := os.Stat(filepath.Join(outputDir, "session.json"))
		if err != nil {
			t.Fatalf("stat saved session: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("file permissions = %o, want 0600", perm)
		}
	})

	t.Run("reload saved session", func(t *testing.T) {
		stateDir := t.TempDir()
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

		// Seed a session.
		os.WriteFile(filepath.Join(stateDir, "session.json"), []byte(`{
			"homeserver_url": "http://localhost:6167",
			"user_id": "@machine/test:bureau.local",
			"access_token": "syt_reload_token"
		}`), 0600)

		_, session, err := LoadSession(stateDir, "http://localhost:6167", logger)
		if err != nil {
			t.Fatalf("LoadSession() error: %v", err)
		}
		defer session.Close()

		// Save to a fresh directory and reload.
		outputDir := t.TempDir()
		if err := SaveSession(outputDir, "http://localhost:6167", session); err != nil {
			t.Fatalf("SaveSession() error: %v", err)
		}

		_, reloaded, err := LoadSession(outputDir, "http://localhost:6167", logger)
		if err != nil {
			t.Fatalf("LoadSession() after SaveSession() error: %v", err)
		}
		defer reloaded.Close()

		if reloaded.UserID() != "@machine/test:bureau.local" {
			t.Errorf("reloaded UserID() = %q, want %q", reloaded.UserID(), "@machine/test:bureau.local")
		}
		if reloaded.AccessToken() != "syt_reload_token" {
			t.Errorf("reloaded AccessToken() = %q, want %q", reloaded.AccessToken(), "syt_reload_token")
		}
	})
}
