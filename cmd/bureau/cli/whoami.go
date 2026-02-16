// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/messaging"
)

// whoamiParams holds the parameters for the whoami command.
type whoamiParams struct {
	JSONOutput
	Verify bool `json:"verify"  flag:"verify"  desc:"verify the session against the homeserver"`
}

// whoamiOutput is the JSON output for the whoami command.
type whoamiOutput struct {
	UserID      string `json:"user_id"              desc:"Matrix user ID"`
	Homeserver  string `json:"homeserver"           desc:"Matrix homeserver URL"`
	SessionFile string `json:"session_file"         desc:"local session file path"`
	Status      string `json:"status,omitempty"     desc:"session verification status"`
}

// WhoAmICommand returns the "whoami" command for displaying the current
// operator identity. Shows the saved session's user ID, homeserver, and
// session file path. With --verify, checks the token against the homeserver
// to confirm the session is still valid.
func WhoAmICommand() *Command {
	var params whoamiParams

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
		Params: func() any { return &params },
		Output: func() any { return &whoamiOutput{} },
		Run: func(args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unexpected argument: %s", args[0])
			}

			path := SessionFilePath()
			session, err := LoadSession()
			if err != nil {
				return err
			}

			output := whoamiOutput{
				UserID:      session.UserID,
				Homeserver:  session.Homeserver,
				SessionFile: path,
			}

			if params.Verify {
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
					output.Status = "invalid"
					if done, err := params.EmitJSON(output); done {
						return err
					}
					fmt.Fprintf(os.Stdout, "User ID:      %s\n", output.UserID)
					fmt.Fprintf(os.Stdout, "Homeserver:   %s\n", output.Homeserver)
					fmt.Fprintf(os.Stdout, "Session file: %s\n", output.SessionFile)
					fmt.Fprintf(os.Stdout, "Status:       INVALID (token rejected by homeserver)\n")
					return fmt.Errorf("session expired or revoked — run \"bureau login\" to refresh")
				}

				output.Status = fmt.Sprintf("valid (verified as %s)", verifiedUserID)
			}

			if done, err := params.EmitJSON(output); done {
				return err
			}

			fmt.Fprintf(os.Stdout, "User ID:      %s\n", output.UserID)
			fmt.Fprintf(os.Stdout, "Homeserver:   %s\n", output.Homeserver)
			fmt.Fprintf(os.Stdout, "Session file: %s\n", output.SessionFile)
			if output.Status != "" {
				fmt.Fprintf(os.Stdout, "Status:       %s\n", output.Status)
			}

			return nil
		},
	}
}
