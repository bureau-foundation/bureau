// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/messaging"
)

// RegistrationContext holds the resources needed for Matrix account
// registration: a guarded registration token and an unauthenticated
// Matrix client. These two things always appear together — every command
// that registers a principal needs both.
//
// The caller must defer Close() to release the guarded token memory.
//
// Usage:
//
//	reg, err := cli.NewRegistrationContext(sessionConfig)
//	if err != nil { return err }
//	defer reg.Close()
//	// reg.Client for unauthenticated registration
//	// reg.Token for the registration token
type RegistrationContext struct {
	// Token is the registration token in guarded memory.
	Token *secret.Buffer

	// Client is an unauthenticated Matrix client suitable for
	// calling client.Register().
	Client *messaging.Client

	// HomeserverURL is the resolved homeserver URL string, preserved
	// for callers that need it (e.g., principal.CreateParams).
	HomeserverURL string
}

// Close releases the guarded memory holding the registration token.
func (r *RegistrationContext) Close() {
	if r.Token != nil {
		r.Token.Close()
	}
}

// NewRegistrationContext reads the credential file referenced by the
// SessionConfig, extracts the MATRIX_REGISTRATION_TOKEN, wraps it in
// guarded memory, resolves the homeserver URL, and creates an
// unauthenticated Matrix client for account registration.
//
// Returns an error if the credential file is unreadable, missing the
// registration token, or the homeserver URL cannot be resolved.
func NewRegistrationContext(config *SessionConfig) (*RegistrationContext, error) {
	if config.CredentialFile == "" {
		return nil, Validation("--credential-file is required")
	}

	credentials, err := ReadCredentialFile(config.CredentialFile)
	if err != nil {
		return nil, Internal("read credential file: %w", err)
	}

	registrationToken := credentials["MATRIX_REGISTRATION_TOKEN"]
	if registrationToken == "" {
		return nil, Validation("credential file missing MATRIX_REGISTRATION_TOKEN").
			WithHint("The credential file must contain MATRIX_REGISTRATION_TOKEN. " +
				"Re-run 'bureau matrix setup' to regenerate the credential file.")
	}

	tokenBuffer, err := secret.NewFromString(registrationToken)
	if err != nil {
		return nil, Internal("protecting registration token: %w", err)
	}

	homeserverURL, err := config.ResolveHomeserverURL()
	if err != nil {
		tokenBuffer.Close()
		return nil, err
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
	})
	if err != nil {
		tokenBuffer.Close()
		return nil, Internal("create matrix client: %w", err)
	}

	return &RegistrationContext{
		Token:         tokenBuffer,
		Client:        client,
		HomeserverURL: homeserverURL,
	}, nil
}
