// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/bureau-foundation/bureau/messaging"
)

// WhoAmICommand returns the "whoami" command for displaying the current
// operator identity. Shows the saved session's user ID, homeserver, and
// session file path. With --verify, checks the token against the homeserver
// to confirm the session is still valid.
func WhoAmICommand() *Command {
	var verify bool

	return &Command{
		Name:    "whoami",
		Summary: "Show the current operator identity",
		Description: `Display the currently logged-in operator identity.

Shows the Matrix user ID, homeserver URL, and session file path from
the saved session (created by "bureau login").

With --verify, the saved access token is checked against the homeserver
to confirm the session is still valid. Without --verify, only the local
session file is read (no network access).`,
		Usage: "bureau whoami [flags]",
		Examples: []Example{
			{
				Description: "Show current identity",
				Command:     "bureau whoami",
			},
			{
				Description: "Verify the session is still valid",
				Command:     "bureau whoami --verify",
			},
		},
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("whoami", pflag.ContinueOnError)
			flagSet.BoolVar(&verify, "verify", false, "verify the session against the homeserver")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}

			path := SessionFilePath()
			session, err := LoadSession()
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stdout, "User ID:      %s\n", session.UserID)
			fmt.Fprintf(os.Stdout, "Homeserver:   %s\n", session.Homeserver)
			fmt.Fprintf(os.Stdout, "Session file: %s\n", path)

			if verify {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				client, err := messaging.NewClient(messaging.ClientConfig{
					HomeserverURL: session.Homeserver,
				})
				if err != nil {
					return fmt.Errorf("create client: %w", err)
				}

				matrixSession, err := client.SessionFromToken(session.UserID, session.AccessToken)
				if err != nil {
					return fmt.Errorf("create session: %w", err)
				}
				defer matrixSession.Close()

				verifiedUserID, err := matrixSession.WhoAmI(ctx)
				if err != nil {
					fmt.Fprintf(os.Stdout, "Status:       INVALID (token rejected by homeserver)\n")
					return fmt.Errorf("session expired or revoked — run \"bureau login\" to refresh")
				}

				fmt.Fprintf(os.Stdout, "Status:       valid (verified as %s)\n", verifiedUserID)
			}

			return nil
		},
	}
}
