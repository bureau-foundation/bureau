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

// SessionConfig holds the shared flags for connecting to a Matrix homeserver.
// Used by CLI commands that need Matrix access (doctor, workspace create,
// matrix room create, etc.).
//
// Connect resolves credentials in priority order:
//  1. Explicit flags (--homeserver, --token, --user-id)
//  2. Credential file (--credential-file, the admin key=value file from
//     "bureau matrix setup" — only when explicitly specified)
//  3. Operator session (~/.config/bureau/session.json from "bureau login")
//  4. Proxy socket (BUREAU_PROXY_SOCKET, inside Bureau sandboxes)
//
// Most commands need zero flags: if the operator has run "bureau login",
// Connect finds their session automatically. The credential file flags
// are overrides for admin operations that require the bootstrap credentials.
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
// on the given flag set. All flags are optional when the operator has run
// "bureau login" — Connect auto-discovers the operator session. Flags
// override auto-discovered values.
func (c *SessionConfig) AddFlags(flagSet *pflag.FlagSet) {
	flagSet.StringVar(&c.CredentialFile, "credential-file", "", "path to admin credential file from 'bureau matrix setup' (overrides operator session)")
	flagSet.StringVar(&c.HomeserverURL, "homeserver", "", "Matrix homeserver URL (overrides all other sources)")
	flagSet.StringVar(&c.Token, "token", "", "Matrix access token (overrides operator session and credential file)")
	flagSet.StringVar(&c.UserID, "user-id", "", "Matrix user ID (overrides operator session and credential file)")
}

// Connect creates an authenticated Matrix session. Resolution order:
//
//  1. Explicit flags (--homeserver, --token, --user-id)
//  2. Credential file (--credential-file, if specified)
//  3. Operator session (~/.config/bureau/session.json from "bureau login")
//  4. Proxy socket (BUREAU_PROXY_SOCKET, for commands inside sandboxes)
//
// Individual flags override values from any lower-priority source.
func (c *SessionConfig) Connect(ctx context.Context) (messaging.Session, error) {
	homeserverURL := c.HomeserverURL
	token := c.Token
	userID := c.UserID

	// Load from credential file if explicitly provided.
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

	// Try operator session for any still-missing values. The operator
	// session is per-user, created by "bureau login", and carries only
	// the operator's own credentials (not server admin keys).
	if homeserverURL == "" || token == "" || userID == "" {
		if operatorSession, err := LoadSession(); err == nil {
			if homeserverURL == "" {
				homeserverURL = operatorSession.Homeserver
			}
			if token == "" {
				token = operatorSession.AccessToken
			}
			if userID == "" {
				userID = operatorSession.UserID
			}
		}
	}

	// When nothing is available, try the proxy socket. Inside a Bureau
	// sandbox, all Matrix access goes through the proxy's structured
	// /v1/matrix/* endpoints.
	if homeserverURL == "" && token == "" && userID == "" {
		return c.connectViaProxy(ctx)
	}

	// Direct connection — report what's missing.
	if homeserverURL == "" || token == "" || userID == "" {
		missing := []string{}
		if homeserverURL == "" {
			missing = append(missing, "homeserver URL")
		}
		if token == "" {
			missing = append(missing, "access token")
		}
		if userID == "" {
			missing = append(missing, "user ID")
		}
		return nil, Validation("missing %s", strings.Join(missing, ", ")).
			WithHint("Run 'bureau login' to authenticate, or pass --credential-file " +
				"with the admin credential file from 'bureau matrix setup'.")
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
		return nil, Validation("no credentials available").
			WithHint("Run 'bureau login' to authenticate, or pass --credential-file " +
				"with the admin credential file from 'bureau matrix setup'.")
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

// ResolveHomeserverURL extracts the homeserver URL without creating a
// full session. Useful for performing unauthenticated checks (like
// server version probing) before attempting authentication.
//
// Resolution order:
//  1. --homeserver flag
//  2. --credential-file (if specified)
//  3. Operator session (~/.config/bureau/session.json)
//  4. machine.conf (BUREAU_HOMESERVER_URL)
func (c *SessionConfig) ResolveHomeserverURL() (string, error) {
	if c.HomeserverURL != "" {
		return c.HomeserverURL, nil
	}

	// Explicit credential file: error if unreadable or missing the URL.
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

	// Operator session.
	if operatorSession, err := LoadSession(); err == nil {
		if operatorSession.Homeserver != "" {
			return operatorSession.Homeserver, nil
		}
	}

	// Machine config.
	if conf := LoadMachineConf(); conf.HomeserverURL != "" {
		return conf.HomeserverURL, nil
	}

	return "", Validation("cannot determine homeserver URL").
		WithHint("Run 'bureau login' to authenticate, or pass --homeserver <url>.")
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
