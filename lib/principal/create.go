// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package principal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/messaging"
)

// ProvisionFunc encrypts and publishes a credential bundle for a principal
// on a machine. Returns the config room ID where credentials were published.
//
// The machineRoomID parameter is the fleet-scoped machine room where the
// machine's age public key is published. This is a required parameter in
// the function signature (not just a struct field) so that missing it
// produces a compile error rather than a runtime error.
//
// The standard implementation is credential.AsProvisionFunc(), which calls
// credential.Provision under the hood. This function type breaks the import
// cycle between lib/principal (which defines Create) and lib/credential
// (which implements the encryption and publishing workflow).
type ProvisionFunc func(ctx context.Context, session messaging.Session, machine ref.Machine, principal ref.Entity, machineRoomID ref.RoomID, credentials map[string]string) (configRoomID ref.RoomID, err error)

// CreateParams holds the parameters for creating and deploying a principal.
type CreateParams struct {
	// Machine identifies the target machine for this principal.
	Machine ref.Machine

	// Principal identifies the target principal as a fleet-scoped entity
	// reference. The Entity's Localpart() is used as the Matrix registration
	// username, and UserID() as the canonical identity.
	Principal ref.Entity

	// TemplateRef identifies which template to use for this principal (e.g.,
	// bureau/template:claude-dev). The template must already exist in Matrix.
	// Callers parse the string at the CLI boundary via schema.ParseTemplateRef.
	TemplateRef schema.TemplateRef

	// ValidateTemplate verifies that the referenced template exists and is
	// resolvable before Create proceeds with account registration and
	// credential provisioning. This breaks the import dependency between
	// lib/principal and lib/template: callers that already have a
	// messaging.Session close over template.Fetch to provide the validation.
	// Required — Create returns an error if nil.
	ValidateTemplate func(ctx context.Context, templateRef schema.TemplateRef, serverName ref.ServerName) error

	// HomeserverURL is the Matrix homeserver URL included in the encrypted
	// credential bundle. The launcher uses this to configure the proxy's
	// Matrix connection for the principal.
	HomeserverURL string

	// AutoStart controls whether the daemon starts the principal's sandbox
	// immediately. Defaults to true when zero-valued because the struct
	// is constructed by the caller — callers must explicitly set false.
	AutoStart bool

	// StartCondition gates this principal's launch on the existence of a
	// specific state event. When set, the daemon only starts the sandbox
	// if the referenced event exists and matches the content criteria.
	// Use this for lifecycle orchestration (e.g., workspace state
	// transitions). For one-shot principals that should not restart
	// after completing their work, use RestartPolicy "on-failure" instead.
	StartCondition *schema.StartCondition

	// ExtraCredentials are additional key-value pairs included in the
	// encrypted credential bundle alongside the Matrix token and user ID.
	// Use this for LLM API keys or other service credentials the principal
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

	// MachineRoomID is the fleet-scoped machine room where the machine's
	// age public key is published. Required for credential provisioning.
	MachineRoomID ref.RoomID

	// Labels are free-form key-value metadata for organizational purposes.
	Labels map[string]string

	// MatrixPolicy controls which self-service Matrix operations this
	// principal's proxy allows (join, invite, create room). When nil,
	// the daemon uses default-deny.
	MatrixPolicy *schema.MatrixPolicy

	// ServiceVisibility is a list of glob patterns that control which
	// services this principal can discover via GET /v1/services. An
	// empty or nil list means the principal cannot see any services.
	ServiceVisibility []string

	// RestartPolicy controls what happens when a sandbox exits.
	// Empty defaults to RestartPolicyAlways at runtime.
	RestartPolicy schema.RestartPolicy
}

// CreateResult holds the result of a successful principal creation.
type CreateResult struct {
	// PrincipalUserID is the full Matrix user ID of the created principal
	// (e.g., "@agent/code-review:bureau.local").
	PrincipalUserID ref.UserID

	// Machine is the machine the principal was assigned to.
	Machine ref.Machine

	// TemplateRef is the parsed template reference.
	TemplateRef schema.TemplateRef

	// ConfigRoomID is the Matrix room ID of the machine's config room.
	ConfigRoomID ref.RoomID

	// ConfigEventID is the event ID of the published MachineConfig state
	// event that includes this principal's assignment.
	ConfigEventID ref.EventID

	// AccessToken is the Matrix access token for the newly created
	// principal account. This is the same token that was sealed into the
	// credential bundle. Callers that need to perform Matrix operations
	// as the principal (e.g., joining additional rooms) can use this
	// token to create a session. The CLI should NOT display this in output.
	AccessToken string `json:"-"`
}

// Create performs the full principal deployment sequence: registers a Matrix
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
// config publishing). The provision function handles credential encryption
// and publishing — pass credential.AsProvisionFunc() for production use.
//
// If the account already exists (M_USER_IN_USE), Create returns an error.
// Re-provisioning an existing principal requires explicit credential rotation
// via bureau credential provision.
func Create(ctx context.Context, client *messaging.Client, session messaging.Session, registrationToken *secret.Buffer, provision ProvisionFunc, params CreateParams) (*CreateResult, error) {
	results, err := CreateMultiple(ctx, client, session, registrationToken, provision, []CreateParams{params})
	if err != nil {
		return nil, err
	}
	return results[0], nil
}

// CreateMultiple performs the full deployment sequence for multiple principals
// on the same machine: registers Matrix accounts, provisions encrypted
// credentials, invites principals to the config room, and publishes a single
// atomic MachineConfig with all assignments.
//
// This is more efficient than calling Create in a loop because the
// MachineConfig is read, modified with all principals, and written back in
// one operation — instead of N separate read-modify-write cycles.
//
// All principals must target the same machine (same Machine and MachineRoomID
// values in all CreateParams entries). Returns one CreateResult per input
// param in the same order.
func CreateMultiple(ctx context.Context, client *messaging.Client, session messaging.Session, registrationToken *secret.Buffer, provision ProvisionFunc, params []CreateParams) ([]*CreateResult, error) {
	if len(params) == 0 {
		return nil, fmt.Errorf("at least one CreateParams is required")
	}

	// Validate that all params target the same machine.
	machine := params[0].Machine
	machineRoomID := params[0].MachineRoomID
	for i, p := range params {
		if p.Machine != machine {
			return nil, fmt.Errorf("params[%d]: machine %s differs from params[0] machine %s — all principals must target the same machine", i, p.Machine, machine)
		}
		if p.MachineRoomID != machineRoomID {
			return nil, fmt.Errorf("params[%d]: machine room ID differs from params[0] — all principals must target the same machine", i)
		}
	}

	// Phase 1: Register accounts and provision credentials for each principal.
	type registeredPrincipal struct {
		principalUserID ref.UserID
		accessToken     string
		configRoomID    ref.RoomID
	}
	registered := make([]registeredPrincipal, len(params))

	for i, p := range params {
		rp, err := registerAndProvision(ctx, client, session, registrationToken, provision, p)
		if err != nil {
			return nil, fmt.Errorf("params[%d] (%s): %w", i, p.Principal.UserID(), err)
		}
		registered[i] = rp
	}

	// Phase 2: Atomic MachineConfig write — read existing config, add all
	// principals, write back in a single state event.
	configRoomID := registered[0].configRoomID
	configEventID, err := AssignPrincipals(ctx, session, configRoomID, params)
	if err != nil {
		return nil, err
	}

	// Build results.
	results := make([]*CreateResult, len(params))
	for i, p := range params {
		results[i] = &CreateResult{
			PrincipalUserID: registered[i].principalUserID,
			Machine:         p.Machine,
			TemplateRef:     p.TemplateRef,
			ConfigRoomID:    configRoomID,
			ConfigEventID:   configEventID,
			AccessToken:     registered[i].accessToken,
		}
	}

	return results, nil
}

// registerAndProvision handles account registration, credential provisioning,
// and config room membership for a single principal. This is the per-principal
// work that CreateMultiple performs before the atomic MachineConfig write.
func registerAndProvision(ctx context.Context, client *messaging.Client, session messaging.Session, registrationToken *secret.Buffer, provision ProvisionFunc, params CreateParams) (struct {
	principalUserID ref.UserID
	accessToken     string
	configRoomID    ref.RoomID
}, error) {
	type result = struct {
		principalUserID ref.UserID
		accessToken     string
		configRoomID    ref.RoomID
	}
	var zero result
	if params.Machine.IsZero() {
		return zero, fmt.Errorf("machine is required")
	}
	if params.Principal.IsZero() {
		return zero, fmt.Errorf("principal is required")
	}
	if params.TemplateRef == (schema.TemplateRef{}) {
		return zero, fmt.Errorf("template reference is required")
	}
	if params.HomeserverURL == "" {
		return zero, fmt.Errorf("homeserver URL is required")
	}
	if params.MachineRoomID.IsZero() {
		return zero, fmt.Errorf("machine room ID is required for credential provisioning")
	}

	if params.ValidateTemplate == nil {
		return zero, fmt.Errorf("ValidateTemplate callback is required")
	}

	serverName := params.Machine.Server()

	// Verify the template exists in Matrix before creating any accounts or state.
	if err := params.ValidateTemplate(ctx, params.TemplateRef, serverName); err != nil {
		return zero, fmt.Errorf("template %q not found: %w", params.TemplateRef, err)
	}

	// Register the principal's Matrix account. Generate a random password —
	// the principal never logs in with it; it receives its access token via
	// the encrypted credential bundle.
	passwordBytes := make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		return zero, fmt.Errorf("generate random password: %w", err)
	}
	hexPassword := []byte(hex.EncodeToString(passwordBytes))
	secret.Zero(passwordBytes)
	password, err := secret.NewFromBytes(hexPassword)
	if err != nil {
		return zero, fmt.Errorf("protecting password: %w", err)
	}
	defer password.Close()

	principalSession, err := client.Register(ctx, messaging.RegisterRequest{
		Username:          params.Principal.Localpart(),
		Password:          password,
		RegistrationToken: registrationToken,
	})
	if err != nil {
		return zero, fmt.Errorf("register principal account %q: %w", params.Principal.UserID(), err)
	}
	defer principalSession.Close()

	principalUserID := principalSession.UserID()
	principalToken := principalSession.AccessToken()

	// Build the credential bundle: Matrix identity + homeserver URL + extras.
	credentials := map[string]string{
		"MATRIX_TOKEN":          principalToken,
		"MATRIX_USER_ID":        principalUserID.String(),
		"MATRIX_HOMESERVER_URL": params.HomeserverURL,
	}
	for key, value := range params.ExtraCredentials {
		if _, reserved := credentials[key]; reserved {
			return zero, fmt.Errorf("extra credential key %q conflicts with auto-provisioned Matrix credentials", key)
		}
		credentials[key] = value
	}

	// Provision encrypted credentials to the machine's config room.
	configRoomID, err := provision(ctx, session, params.Machine, params.Principal, params.MachineRoomID, credentials)
	if err != nil {
		return zero, fmt.Errorf("provision credentials: %w", err)
	}

	// Invite the principal to the config room and join on its behalf. The
	// principal needs config room membership to post lifecycle messages
	// (agent-ready, completion summaries). We join now using the
	// registration session so the principal is already a member when the
	// sandbox starts — no need for the proxy or principal process to handle
	// invite acceptance.
	if err := session.InviteUser(ctx, configRoomID, principalUserID); err != nil {
		if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
			return zero, fmt.Errorf("invite principal to config room: %w", err)
		}
		// Already invited — not an error.
	}
	if _, err := principalSession.JoinRoom(ctx, configRoomID); err != nil {
		return zero, fmt.Errorf("principal join config room: %w", err)
	}

	// Services need membership in the system room (token signing key
	// lookup) and fleet service room (service registration and
	// deregistration). Invite here using the admin session for the
	// standard create path. The daemon also invites service principals
	// to these rooms during reconciliation (covering HA failover, fleet
	// placement, and manual config edits). Both invites are idempotent.
	// The proxy's acceptPendingInvites joins at sandbox startup.
	if params.Principal.EntityType() == "service" {
		fleetRef := params.Principal.Fleet()

		systemRoomAlias := fleetRef.Namespace().SystemRoomAlias()
		systemRoomID, err := session.ResolveAlias(ctx, systemRoomAlias)
		if err != nil {
			return zero, fmt.Errorf("resolve system room %q for service invitation: %w", systemRoomAlias, err)
		}
		if err := session.InviteUser(ctx, systemRoomID, principalUserID); err != nil {
			if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
				return zero, fmt.Errorf("invite service to system room: %w", err)
			}
		}

		serviceRoomAlias := fleetRef.ServiceRoomAlias()
		serviceRoomID, err := session.ResolveAlias(ctx, serviceRoomAlias)
		if err != nil {
			return zero, fmt.Errorf("resolve fleet service room %q for service invitation: %w", serviceRoomAlias, err)
		}
		if err := session.InviteUser(ctx, serviceRoomID, principalUserID); err != nil {
			if !messaging.IsMatrixError(err, messaging.ErrCodeForbidden) {
				return zero, fmt.Errorf("invite service to fleet service room: %w", err)
			}
		}
	}

	return result{
		principalUserID: principalUserID,
		accessToken:     principalToken,
		configRoomID:    configRoomID,
	}, nil
}

// AssignPrincipals reads the current MachineConfig, merges all new
// PrincipalAssignments, and publishes the updated config in a single state
// event. If any principal is already assigned, its entry is updated in place.
//
// This is called internally by CreateMultiple after account registration,
// but is also exported for callers that need to ensure a MachineConfig
// assignment exists independently of account creation — for example, when
// re-running an "enable" command after the service account already exists.
func AssignPrincipals(ctx context.Context, session messaging.Session, configRoomID ref.RoomID, params []CreateParams) (ref.EventID, error) {
	machineLocalpart := params[0].Machine.Localpart()

	config, err := messaging.GetState[schema.MachineConfig](ctx, session, configRoomID, schema.EventTypeMachineConfig, machineLocalpart)
	if err != nil && !messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
		return ref.EventID{}, fmt.Errorf("read machine config for %s: %w", machineLocalpart, err)
	}

	for _, p := range params {
		assignment := schema.PrincipalAssignment{
			Principal:                 p.Principal,
			Template:                  p.TemplateRef.String(),
			AutoStart:                 p.AutoStart,
			StartCondition:            p.StartCondition,
			RestartPolicy:             p.RestartPolicy,
			Labels:                    p.Labels,
			MatrixPolicy:              p.MatrixPolicy,
			ServiceVisibility:         p.ServiceVisibility,
			Authorization:             p.Authorization,
			ExtraEnvironmentVariables: p.ExtraEnvironmentVariables,
			Payload:                   p.Payload,
		}

		// Check if the principal is already assigned; update in place if so.
		found := false
		for i, existing := range config.Principals {
			if existing.Principal.UserID() == p.Principal.UserID() {
				config.Principals[i] = assignment
				found = true
				break
			}
		}
		if !found {
			config.Principals = append(config.Principals, assignment)
		}
	}

	eventID, err := session.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineLocalpart, config)
	if err != nil {
		return ref.EventID{}, fmt.Errorf("publish machine config for %s: %w", machineLocalpart, err)
	}

	return eventID, nil
}
