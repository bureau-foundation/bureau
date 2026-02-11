// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/messaging"
)

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
