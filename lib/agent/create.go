// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/credential"
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	libtemplate "github.com/bureau-foundation/bureau/lib/template"
	"github.com/bureau-foundation/bureau/messaging"
)

// CreateParams holds the parameters for creating and deploying an agent.
type CreateParams struct {
	// MachineName is the machine's localpart (e.g., "machine/workstation").
	MachineName string

	// Localpart is the agent principal's localpart (e.g., "agent/code-review").
	Localpart string

	// TemplateRef is the template reference string (e.g.,
	// "bureau/template:claude-dev"). The template must already exist in Matrix.
	TemplateRef string

	// ServerName is the Matrix server name (e.g., "bureau.local").
	ServerName string

	// HomeserverURL is the Matrix homeserver URL included in the encrypted
	// credential bundle. The launcher uses this to configure the proxy's
	// Matrix connection for the agent.
	HomeserverURL string

	// AutoStart controls whether the daemon starts the agent's sandbox
	// immediately. Defaults to true when zero-valued because the struct
	// is constructed by the caller — callers must explicitly set false.
	AutoStart bool

	// ExtraCredentials are additional key-value pairs included in the
	// encrypted credential bundle alongside the Matrix token and user ID.
	// Use this for LLM API keys or other service credentials the agent
	// needs inside its sandbox.
	ExtraCredentials map[string]string

	// ExtraEnvironmentVariables are merged into the template's environment
	// variables for this principal instance.
	ExtraEnvironmentVariables map[string]string

	// Payload is instance-specific data available at
	// /run/bureau/payload.json inside the sandbox.
	Payload map[string]any

	// Authorization is the authorization policy for this principal.
	// When nil, the daemon uses default-deny.
	Authorization *schema.AuthorizationPolicy

	// Labels are free-form key-value metadata for organizational purposes.
	Labels map[string]string
}

// CreateResult holds the result of a successful agent creation.
type CreateResult struct {
	// PrincipalUserID is the full Matrix user ID of the created agent
	// (e.g., "@agent/code-review:bureau.local").
	PrincipalUserID string

	// MachineName is the machine the agent was assigned to.
	MachineName string

	// TemplateRef is the template reference string.
	TemplateRef string

	// ConfigRoomID is the Matrix room ID of the machine's config room.
	ConfigRoomID string

	// ConfigEventID is the event ID of the published MachineConfig state
	// event that includes this agent's assignment.
	ConfigEventID string
}

// Create performs the full agent deployment sequence: registers a Matrix
// account, provisions encrypted credentials, invites the principal to the
// config room, and publishes the MachineConfig assignment.
//
// The template must already exist in Matrix (use lib/template.Push to
// publish it first). Create validates that the template is resolvable
// before proceeding with registration.
//
// The client is used for account registration (unauthenticated operation
// that requires the registration token). The session is used for all
// admin-level Matrix operations (credential provisioning, room management,
// config publishing).
//
// If the account already exists (M_USER_IN_USE), Create returns an error.
// Re-provisioning an existing agent requires explicit credential rotation
// via bureau credential provision.
func Create(ctx context.Context, client *messaging.Client, session *messaging.Session, registrationToken *secret.Buffer, params CreateParams) (*CreateResult, error) {
	if err := principal.ValidateLocalpart(params.MachineName); err != nil {
		return nil, fmt.Errorf("invalid machine name: %w", err)
	}
	if err := principal.ValidateLocalpart(params.Localpart); err != nil {
		return nil, fmt.Errorf("invalid agent localpart: %w", err)
	}
	if params.TemplateRef == "" {
		return nil, fmt.Errorf("template reference is required")
	}
	if params.ServerName == "" {
		return nil, fmt.Errorf("server name is required")
	}
	if params.HomeserverURL == "" {
		return nil, fmt.Errorf("homeserver URL is required")
	}

	// Validate the template reference format and verify the template exists
	// in Matrix before creating any accounts or state.
	templateRef, err := schema.ParseTemplateRef(params.TemplateRef)
	if err != nil {
		return nil, fmt.Errorf("invalid template reference %q: %w", params.TemplateRef, err)
	}
	if _, err := libtemplate.Fetch(ctx, session, templateRef, params.ServerName); err != nil {
		return nil, fmt.Errorf("template %q not found: %w", params.TemplateRef, err)
	}

	// Register the agent's Matrix account. Generate a random password —
	// the agent never logs in with it; it receives its access token via
	// the encrypted credential bundle.
	passwordBytes := make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		return nil, fmt.Errorf("generate random password: %w", err)
	}
	hexPassword := []byte(hex.EncodeToString(passwordBytes))
	secret.Zero(passwordBytes)
	password, err := secret.NewFromBytes(hexPassword)
	if err != nil {
		return nil, fmt.Errorf("protecting password: %w", err)
	}
	defer password.Close()

	agentSession, err := client.Register(ctx, messaging.RegisterRequest{
		Username:          params.Localpart,
		Password:          password,
		RegistrationToken: registrationToken,
	})
	if err != nil {
		return nil, fmt.Errorf("register agent account %q: %w", params.Localpart, err)
	}
	defer agentSession.Close()

	agentUserID := agentSession.UserID()
	agentToken := agentSession.AccessToken()

	// Build the credential bundle: Matrix identity + homeserver URL + extras.
	credentials := map[string]string{
		"MATRIX_TOKEN":          agentToken,
		"MATRIX_USER_ID":        agentUserID,
		"MATRIX_HOMESERVER_URL": params.HomeserverURL,
	}
	for key, value := range params.ExtraCredentials {
		if _, reserved := credentials[key]; reserved {
			return nil, fmt.Errorf("extra credential key %q conflicts with auto-provisioned Matrix credentials", key)
		}
		credentials[key] = value
	}

	// Provision encrypted credentials to the machine's config room.
	provisionResult, err := credential.Provision(ctx, session, credential.ProvisionParams{
		MachineName: params.MachineName,
		Principal:   params.Localpart,
		ServerName:  params.ServerName,
		Credentials: credentials,
	})
	if err != nil {
		return nil, fmt.Errorf("provision credentials: %w", err)
	}

	// Invite the agent to the config room so it can post messages
	// (agent-ready, completion summaries, etc.).
	if err := session.InviteUser(ctx, provisionResult.ConfigRoomID, agentUserID); err != nil {
		if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			return nil, fmt.Errorf("invite agent to config room: %w", err)
		}
		// Already invited — not an error.
	}

	// Read-modify-write the MachineConfig to add this principal's
	// assignment. If the principal is already assigned (re-deploy),
	// update the existing entry.
	configEventID, err := assignPrincipal(ctx, session, provisionResult.ConfigRoomID, params)
	if err != nil {
		return nil, err
	}

	return &CreateResult{
		PrincipalUserID: agentUserID,
		MachineName:     params.MachineName,
		TemplateRef:     params.TemplateRef,
		ConfigRoomID:    provisionResult.ConfigRoomID,
		ConfigEventID:   configEventID,
	}, nil
}

// assignPrincipal reads the current MachineConfig, merges the new
// PrincipalAssignment, and publishes the updated config. If the principal
// is already assigned, its entry is updated in place.
func assignPrincipal(ctx context.Context, session *messaging.Session, configRoomID string, params CreateParams) (string, error) {
	var config schema.MachineConfig
	existingContent, err := session.GetStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, params.MachineName)
	if err == nil {
		if err := json.Unmarshal(existingContent, &config); err != nil {
			return "", fmt.Errorf("parse existing machine config for %s: %w", params.MachineName, err)
		}
	} else if !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return "", fmt.Errorf("read machine config for %s: %w", params.MachineName, err)
	}

	assignment := schema.PrincipalAssignment{
		Localpart:                 params.Localpart,
		Template:                  params.TemplateRef,
		AutoStart:                 params.AutoStart,
		Labels:                    params.Labels,
		Authorization:             params.Authorization,
		ExtraEnvironmentVariables: params.ExtraEnvironmentVariables,
		Payload:                   params.Payload,
	}

	// Check if the principal is already assigned; update in place if so.
	found := false
	for i, existing := range config.Principals {
		if existing.Localpart == params.Localpart {
			config.Principals[i] = assignment
			found = true
			break
		}
	}
	if !found {
		config.Principals = append(config.Principals, assignment)
	}

	eventID, err := session.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, params.MachineName, config)
	if err != nil {
		return "", fmt.Errorf("publish machine config for %s: %w", params.MachineName, err)
	}

	return eventID, nil
}
