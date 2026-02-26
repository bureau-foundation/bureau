// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/lib/proxyclient"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/secret"
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
//	    Run: func(_ context.Context, args []string, _ *slog.Logger) error {
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
//
// When no explicit credentials are provided and BUREAU_PROXY_SOCKET is set,
// Connect falls through to a proxy-routed path: it creates a
// proxyclient.ProxySession that delegates to the proxy's structured /v1/matrix/*
// endpoints. The proxy injects credentials on outgoing Matrix API requests.
// This is the connection path for CLI commands running inside a Bureau sandbox
// (both via MCP tools and direct shell invocation).
func (c *SessionConfig) Connect(ctx context.Context) (messaging.Session, error) {
	homeserverURL := c.HomeserverURL
	token := c.Token
	userID := c.UserID

	// Load from credential file if provided.
	if c.CredentialFile != "" {
		credentials, err := ReadCredentialFile(c.CredentialFile)
		if err != nil {
			return nil, Internal("read credential file %s: %w", c.CredentialFile, err).
				WithHint("Check that the file exists and is readable. " +
					"Credential files are created by 'bureau matrix setup'.")
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

	// When no credentials are available, try the proxy socket. Inside a
	// Bureau sandbox, all Matrix access goes through the proxy's structured
	// /v1/matrix/* endpoints. The proxy injects the principal's access
	// token. No credentials needed in the sandbox.
	if homeserverURL == "" && token == "" && userID == "" {
		return c.connectViaProxy(ctx)
	}

	// Direct connection with explicit credentials.
	credentialHint := "Pass --credential-file with the file from 'bureau matrix setup', " +
		"or set all three flags: --homeserver, --token, --user-id."
	if homeserverURL == "" {
		return nil, Validation("--homeserver is required (or use --credential-file)").
			WithHint(credentialHint)
	}
	if token == "" {
		return nil, Validation("--token is required (or use --credential-file)").
			WithHint(credentialHint)
	}
	if userID == "" {
		return nil, Validation("--user-id is required (or use --credential-file)").
			WithHint(credentialHint)
	}

	logger := NewClientLogger(slog.LevelInfo)

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
		Logger:        logger,
	})
	if err != nil {
		return nil, Transient("cannot connect to homeserver at %s: %w", homeserverURL, err).
			WithHint("Check that the homeserver is running and the URL is correct. " +
				"Run 'bureau matrix doctor' to diagnose homeserver connectivity.")
	}

	parsedUserID, err := ref.ParseUserID(userID)
	if err != nil {
		return nil, Validation("invalid user ID %q: %w", userID, err).
			WithHint("User IDs have the form @localpart:servername (e.g., @admin:bureau.local).")
	}

	return client.SessionFromToken(parsedUserID, token)
}

// connectViaProxy creates a Matrix session routed through the Bureau proxy
// Unix socket. Returns a ProxySession that delegates to the proxy's
// structured /v1/matrix/* endpoints. The proxy handles credential injection
// — no token is needed in the sandbox.
func (c *SessionConfig) connectViaProxy(ctx context.Context) (messaging.Session, error) {
	socketPath := os.Getenv("BUREAU_PROXY_SOCKET")
	if socketPath == "" {
		return nil, Validation("no credentials provided and BUREAU_PROXY_SOCKET is not set").
			WithHint("Use --credential-file with the file from 'bureau matrix setup', " +
				"or run inside a Bureau sandbox where the proxy socket is available.")
	}

	proxy := proxyclient.New(socketPath, ref.ServerName{})

	// Discover the principal's identity from the proxy. The proxy knows
	// who we are (the daemon told it) — we just need to ask.
	identity, err := proxy.Identity(ctx)
	if err != nil {
		return nil, Transient("proxy identity request failed on %s: %w", socketPath, err).
			WithHint("The proxy socket exists but is not responding. " +
				"Check that the Bureau daemon is running and the proxy process is healthy.")
	}

	proxyUserID, err := ref.ParseUserID(identity.UserID)
	if err != nil {
		return nil, Internal("proxy returned invalid user ID %q: %w", identity.UserID, err).
			WithHint("The proxy and daemon may be running different versions. " +
				"Restart the daemon and check for version mismatches.")
	}

	return proxyclient.NewProxySession(proxy, proxyUserID), nil
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
			return "", Internal("read credential file %s: %w", c.CredentialFile, err).
				WithHint("Check that the file exists and is readable. " +
					"Credential files are created by 'bureau matrix setup'.")
		}
		if url, ok := credentials["MATRIX_HOMESERVER_URL"]; ok && url != "" {
			return url, nil
		}
		return "", Validation("credential file %s is missing MATRIX_HOMESERVER_URL", c.CredentialFile).
			WithHint("The credential file may be incomplete. " +
				"Re-run 'bureau matrix setup' to regenerate it, or add MATRIX_HOMESERVER_URL=<url> to the file.")
	}
	return "", Validation("--homeserver or --credential-file is required").
		WithHint("Pass --homeserver <url> (e.g., http://localhost:6167) or " +
			"--credential-file with the file from 'bureau matrix setup'.")
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
			return nil, Internal("line %d: expected KEY=VALUE, got %q", lineNumber, line)
		}
		credentials[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, Internal("reading credential file: %w", err)
	}

	return credentials, nil
}

// UpdateCredentialFile reads an existing credential file, updates or adds
// the specified key=value pairs, and writes the file back. Existing keys
// are updated in place (preserving ordering and comments). New keys are
// appended before the final trailing newline. The file permissions are
// preserved.
func UpdateCredentialFile(path string, updates map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return Internal("read credential file: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	seen := make(map[string]bool)

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, _, found := strings.Cut(trimmed, "=")
		if !found {
			continue
		}
		if newValue, ok := updates[key]; ok {
			lines[i] = key + "=" + newValue
			seen[key] = true
		}
	}

	// Append any keys that weren't already in the file. Insert before
	// the final empty line (trailing newline) if present.
	var toAppend []string
	for key, value := range updates {
		if !seen[key] {
			toAppend = append(toAppend, key+"="+value)
		}
	}
	if len(toAppend) > 0 {
		// Sort for deterministic output.
		sort.Strings(toAppend)
		// Find insertion point: before trailing empty lines.
		insertAt := len(lines)
		for insertAt > 0 && strings.TrimSpace(lines[insertAt-1]) == "" {
			insertAt--
		}
		lines = append(lines[:insertAt], append(toAppend, lines[insertAt:]...)...)
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600)
}

// DeriveAdminPassword deterministically derives an account password from the
// registration token via SHA-256 with a domain separator. The result is
// returned in an mmap-backed buffer; the caller must close it.
//
// This ensures re-running setup with the same token produces the same
// password (idempotency). Acceptable because the registration token is
// high-entropy random material (openssl rand -hex 32) and the password
// only needs to resist online attacks rate-limited by the homeserver.
func DeriveAdminPassword(registrationToken *secret.Buffer) (*secret.Buffer, error) {
	preimage, err := secret.Concat("bureau-admin-password:", registrationToken)
	if err != nil {
		return nil, Internal("building password preimage: %w", err)
	}
	defer preimage.Close()
	hash := sha256.Sum256(preimage.Bytes())
	hexBytes := []byte(hex.EncodeToString(hash[:]))
	return secret.NewFromBytes(hexBytes)
}
