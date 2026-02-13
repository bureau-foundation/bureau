// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
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
}
