// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/messaging"
)

// OperatorSession holds the operator's Matrix authentication state.
// Stored at the well-known path returned by SessionFilePath and loaded
// automatically by CLI commands that require authentication (observe,
// list, dashboard). Analogous to SSH keys — set up once via
// "bureau login", then transparent.
type OperatorSession struct {
	// UserID is the operator's full Matrix user ID
	// (e.g., "@bureau-admin:bureau.local").
	UserID string `json:"user_id"`

	// AccessToken is the Matrix access token proving the operator's
	// identity. The daemon verifies this via /account/whoami.
	AccessToken string `json:"access_token"`

	// Homeserver is the base URL of the Matrix homeserver
	// (e.g., "http://localhost:6167"). Included so commands that talk
	// directly to the homeserver (like bureau matrix doctor) can use
	// the same session file.
	Homeserver string `json:"homeserver"`
}

// SessionFilePath returns the path to the operator's session file.
// Checks BUREAU_SESSION_FILE environment variable first, then falls
// back to ~/.config/bureau/session.json.
func SessionFilePath() string {
	if envPath := os.Getenv("BUREAU_SESSION_FILE"); envPath != "" {
		return envPath
	}

	configDirectory := os.Getenv("XDG_CONFIG_HOME")
	if configDirectory == "" {
		homeDirectory, err := os.UserHomeDir()
		if err != nil {
			// Fallback — this should rarely happen.
			return filepath.Join("/tmp", "bureau-session.json")
		}
		configDirectory = filepath.Join(homeDirectory, ".config")
	}
	return filepath.Join(configDirectory, "bureau", "session.json")
}

// LoadSession reads the operator session from the well-known path.
// Returns a clear error message directing the user to "bureau login"
// if no session exists.
func LoadSession() (*OperatorSession, error) {
	path := SessionFilePath()
	return LoadSessionFrom(path)
}

// LoadSessionFrom reads an operator session from a specific file path.
func LoadSessionFrom(path string) (*OperatorSession, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no Bureau session found at %s — run \"bureau login\" first", path)
		}
		return nil, fmt.Errorf("reading session file %s: %w", path, err)
	}

	var session OperatorSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("parsing session file %s: %w", path, err)
	}

	if session.UserID == "" {
		return nil, fmt.Errorf("session file %s has no user_id", path)
	}
	if session.AccessToken == "" {
		return nil, fmt.Errorf("session file %s has no access_token", path)
	}
	if session.Homeserver == "" {
		return nil, fmt.Errorf("session file %s has no homeserver", path)
	}

	return &session, nil
}

// SaveSession writes an operator session to the well-known path.
// Creates the parent directory with mode 0700 if it doesn't exist.
// The session file is written with mode 0600 (owner-only read/write)
// since it contains an access token.
func SaveSession(session *OperatorSession) error {
	path := SessionFilePath()
	return SaveSessionTo(session, path)
}

// SaveSessionTo writes an operator session to a specific file path.
func SaveSessionTo(session *OperatorSession, path string) error {
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling session: %w", err)
	}
	data = append(data, '\n')

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("creating session directory %s: %w", directory, err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing session file %s: %w", path, err)
	}

	return nil
}

// SessionConfig holds the shared flags for connecting to a Matrix homeserver
// via a credential file or explicit flags. Used by CLI commands that need
// admin-level Matrix access (workspace create, matrix room create, etc.).
//
// The credential file is the key=value file produced by "bureau matrix setup".
// It contains MATRIX_HOMESERVER_URL, MATRIX_ADMIN_USER, and MATRIX_ADMIN_TOKEN.
//
// Alternatively, --homeserver, --token, and --user-id can be specified directly
// for environments where the credential file is not available.
//
// Usage pattern:
//
//	var session cli.SessionConfig
//	command := &cli.Command{
//	    Flags: func() *pflag.FlagSet {
//	        fs := pflag.NewFlagSet("mycommand", pflag.ContinueOnError)
//	        session.AddFlags(fs)
//	        return fs
//	    },
//	    Run: func(args []string) error {
//	        sess, err := session.Connect(ctx)
//	        ...
//	    },
//	}
type SessionConfig struct {
	CredentialFile string
	HomeserverURL  string
	Token          string
	UserID         string
}

// AddFlags registers --credential-file, --homeserver, --token, and --user-id
// on the given flag set. --credential-file is the primary interface; the
// others are overrides for when you need to specify values directly.
func (c *SessionConfig) AddFlags(flagSet *pflag.FlagSet) {
	flagSet.StringVar(&c.CredentialFile, "credential-file", "", "path to Bureau credential file from 'bureau matrix setup' (required unless --homeserver/--token/--user-id are all set)")
	flagSet.StringVar(&c.HomeserverURL, "homeserver", "", "Matrix homeserver URL (overrides credential file)")
	flagSet.StringVar(&c.Token, "token", "", "Matrix access token (overrides credential file)")
	flagSet.StringVar(&c.UserID, "user-id", "", "Matrix user ID (overrides credential file)")
}

// Connect creates an authenticated Matrix session from the configured flags.
// If --credential-file is set, it reads the credential file and uses those
// values. Individual flags (--homeserver, --token, --user-id) override the
// credential file values.
func (c *SessionConfig) Connect(ctx context.Context) (*messaging.Session, error) {
	homeserverURL := c.HomeserverURL
	token := c.Token
	userID := c.UserID

	// Load from credential file if provided.
	if c.CredentialFile != "" {
		credentials, err := ReadCredentialFile(c.CredentialFile)
		if err != nil {
			return nil, fmt.Errorf("read credential file: %w", err)
		}
		if homeserverURL == "" {
			homeserverURL = credentials["MATRIX_HOMESERVER_URL"]
		}
		if token == "" {
			token = credentials["MATRIX_ADMIN_TOKEN"]
		}
		if userID == "" {
			userID = credentials["MATRIX_ADMIN_USER"]
		}
	}

	// Validate required fields.
	if homeserverURL == "" {
		return nil, fmt.Errorf("--homeserver is required (or use --credential-file)")
	}
	if token == "" {
		return nil, fmt.Errorf("--token is required (or use --credential-file)")
	}
	if userID == "" {
		return nil, fmt.Errorf("--user-id is required (or use --credential-file)")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
		Logger:        logger,
	})
	if err != nil {
		return nil, fmt.Errorf("create matrix client: %w", err)
	}

	return client.SessionFromToken(userID, token)
}

// ResolveHomeserverURL extracts the homeserver URL from flags or credential
// file without creating a full session. Useful for performing unauthenticated
// checks (like server version probing) before attempting authentication.
func (c *SessionConfig) ResolveHomeserverURL() (string, error) {
	if c.HomeserverURL != "" {
		return c.HomeserverURL, nil
	}
	if c.CredentialFile != "" {
		credentials, err := ReadCredentialFile(c.CredentialFile)
		if err != nil {
			return "", fmt.Errorf("read credential file: %w", err)
		}
		if url, ok := credentials["MATRIX_HOMESERVER_URL"]; ok && url != "" {
			return url, nil
		}
		return "", fmt.Errorf("credential file missing MATRIX_HOMESERVER_URL")
	}
	return "", fmt.Errorf("--homeserver or --credential-file is required")
}

// ReadCredentialFile parses a key=value credential file. Lines starting
// with "#" are comments. Empty lines are ignored. This matches the format
// written by "bureau matrix setup".
//
// The returned map holds heap strings containing secrets (access tokens, etc.).
// These strings cannot be zeroed (Go strings are immutable). In the CLI context
// this is acceptable: the map is short-lived, and the access token is moved
// into a *secret.Buffer by SessionFromToken before the map goes out of scope.
func ReadCredentialFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	credentials := make(map[string]string)
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE, got %q", lineNumber, line)
		}
		credentials[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading credential file: %w", err)
	}

	return credentials, nil
}
