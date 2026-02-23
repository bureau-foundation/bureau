// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/messaging"
)

// LoginCommand returns the "login" command for authenticating an operator.
// This performs a Matrix login, verifies the session via WhoAmI, and saves
// the resulting session to the well-known path (~/.config/bureau/session.json).
// Subsequent CLI commands (observe, list, dashboard) load this session
// transparently, like SSH keys.
// loginParams holds the parameters for the login command. All flags are
// infrastructure-only (credential handling) and excluded from MCP schema.
type loginParams struct {
	HomeserverURL string `json:"-" flag:"homeserver"    desc:"Matrix homeserver URL" default:"http://localhost:6167"`
	PasswordFile  string `json:"-" flag:"password-file" desc:"path to file containing password, or - to prompt interactively (default: prompt)"`
}

func LoginCommand() *Command {
	var params loginParams

	return &Command{
		Name:    "login",
		Summary: "Authenticate as an operator",
		Description: `Log in to a Bureau deployment and save the session locally.

After login, commands like "bureau observe" and "bureau list" use the saved
session transparently — no flags needed. This is the operator equivalent of
SSH keys: authenticate once, then access is seamless.

The session file is stored at ~/.config/bureau/session.json (or
$BUREAU_SESSION_FILE if set, or $XDG_CONFIG_HOME/bureau/session.json).
The file is written with mode 0600 (owner-only read/write) since it
contains an access token.

The password can be provided via --password-file (a path to a file containing
the password) or prompted interactively if --password-file is "-" or omitted.`,
		Usage: "bureau login <username> [flags]",
		Examples: []Example{
			{
				Description: "Log in interactively (prompts for password)",
				Command:     "bureau login ben",
			},
			{
				Description: "Log in with explicit homeserver",
				Command:     "bureau login ben --homeserver http://matrix.example.com:6167",
			},
			{
				Description: "Log in with password from file",
				Command:     "bureau login ben --password-file /path/to/password",
			},
		},
		Params: func() any { return &params },
		Run: func(_ context.Context, args []string, _ *slog.Logger) error {
			if len(args) < 1 {
				return Validation("username is required\n\nUsage: bureau login <username> [flags]")
			}
			username := args[0]
			if len(args) > 1 {
				return Validation("unexpected argument: %s", args[1])
			}

			// Read the password. Default to interactive prompt if no
			// --password-file is given.
			passwordBuffer, err := readLoginPassword(params.PasswordFile)
			if err != nil {
				return Internal("read password: %w", err)
			}
			defer passwordBuffer.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			client, err := messaging.NewClient(messaging.ClientConfig{
				HomeserverURL: params.HomeserverURL,
			})
			if err != nil {
				return Internal("create matrix client: %w", err)
			}

			session, err := client.Login(ctx, username, passwordBuffer)
			if err != nil {
				return Internal("login failed: %w", err)
			}
			defer session.Close()

			// Verify the session works before saving.
			userID, err := session.WhoAmI(ctx)
			if err != nil {
				return Internal("session verification failed: %w", err)
			}

			operatorSession := &OperatorSession{
				UserID:      userID.String(),
				AccessToken: session.AccessToken(),
				Homeserver:  params.HomeserverURL,
			}

			if err := SaveSession(operatorSession); err != nil {
				return Internal("save session: %w", err)
			}

			path := SessionFilePath()
			fmt.Fprintf(os.Stderr, "Logged in as %s\n", userID)
			fmt.Fprintf(os.Stderr, "Session saved to %s\n", path)
			return nil
		},
	}
}

// readLoginPassword reads a password for the login command. If passwordFile
// is empty, prompts interactively on the terminal. If passwordFile is "-",
// also prompts interactively. Otherwise, reads from the file path.
//
// Unlike the matrix user create command, login does not require password
// confirmation — the homeserver validates the password immediately.
func readLoginPassword(passwordFile string) (*secret.Buffer, error) {
	if passwordFile != "" && passwordFile != "-" {
		return readSecretFile(passwordFile)
	}

	// Interactive prompt — read from terminal with echo disabled.
	stdinFileDescriptor := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFileDescriptor) {
		return nil, Validation("no terminal available for interactive password prompt (use --password-file)")
	}

	fmt.Fprint(os.Stderr, "Password: ")
	passwordBytes, err := term.ReadPassword(stdinFileDescriptor)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, Internal("reading password: %w", err)
	}

	buffer, err := secret.NewFromBytes(passwordBytes)
	if err != nil {
		secret.Zero(passwordBytes)
		return nil, err
	}
	return buffer, nil
}

// readSecretFile reads a secret from a file path into a secret.Buffer.
// Strips trailing newlines (common with echo/printf pipelines).
func readSecretFile(path string) (*secret.Buffer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, Internal("reading %s: %w", path, err)
	}

	// Strip trailing newlines — files often end with one.
	for len(data) > 0 && (data[len(data)-1] == '\n' || data[len(data)-1] == '\r') {
		data = data[:len(data)-1]
	}

	if len(data) == 0 {
		secret.Zero(data)
		return nil, Validation("file %s is empty (after stripping trailing newlines)", path)
	}

	buffer, err := secret.NewFromBytes(data)
	if err != nil {
		secret.Zero(data)
		return nil, err
	}
	return buffer, nil
}
