// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/bureau-foundation/bureau/lib/authorization"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// credentialAccessResult is the outcome of a credential access check.
// It describes who has access (if anyone) and why, for audit logging.
type credentialAccessResult struct {
	// Allowed is true if access is granted.
	Allowed bool

	// Reason is a short human-readable explanation of the decision.
	// For grants, it describes which condition was satisfied.
	// For denials, it describes what was checked without revealing
	// credential internals (room IDs, key names, power levels).
	Reason string
}

// readCredentialsByRef reads an m.bureau.credentials state event from
// the room referenced by a CredentialRef. The daemon must be a member
// of the referenced room. Returns the credential event content and
// the resolved room ID.
//
// The room alias is resolved against the daemon's server name. Variable
// substitution (e.g., ${FLEET_ROOM}) must be performed on the raw
// credential_ref string before calling this function — the CredentialRef
// must contain a literal room reference.
func (d *Daemon) readCredentialsByRef(ctx context.Context, credentialRef schema.CredentialRef) (*schema.Credentials, ref.RoomID, error) {
	var zeroRoomID ref.RoomID
	roomAlias := credentialRef.RoomAlias(d.machine.Server())

	roomID, err := d.session.ResolveAlias(ctx, roomAlias)
	if err != nil {
		// Intentionally vague: do not reveal whether the room exists
		// or the daemon is not a member. Both produce the same error.
		return nil, zeroRoomID, fmt.Errorf("cannot access credential room for ref %q", credentialRef.String())
	}

	credentials, credErr := messaging.GetState[schema.Credentials](
		ctx, d.session, roomID,
		schema.EventTypeCredentials, credentialRef.StateKey,
	)
	if credErr != nil {
		return nil, zeroRoomID, fmt.Errorf("cannot read credentials for ref %q", credentialRef.String())
	}

	return &credentials, roomID, nil
}

// checkCredentialAccess verifies that at least one principal in the
// execution chain has access to the referenced credentials. The check
// is: has_credential_access(requester) OR has_credential_access(templateAuthor).
//
// This is the launch-time authorization check for template-bound
// credentials. It must be called BEFORE fetching the credential
// ciphertext to avoid leaking credential existence via timing.
//
// The credentialRef is the resolved (variable-substituted) reference.
// The credentialRoomID is the resolved room ID (from readCredentialsByRef
// or alias resolution). requester is the sender of the pipeline.execute
// command. templateAuthor is the sender of the template state event
// that introduced the credential_ref.
func (d *Daemon) checkCredentialAccess(
	ctx context.Context,
	credentialRef schema.CredentialRef,
	credentialRoomID ref.RoomID,
	requester ref.UserID,
	templateAuthor ref.UserID,
) credentialAccessResult {
	action := schema.ActionCredentialUsePrefix + credentialRef.String()

	// Check requester first.
	if result := d.hasCredentialAccess(ctx, credentialRoomID, requester, credentialRef, action); result.Allowed {
		return result
	}

	// Fall back to template author.
	if templateAuthor != requester && !templateAuthor.IsZero() {
		if result := d.hasCredentialAccess(ctx, credentialRoomID, templateAuthor, credentialRef, action); result.Allowed {
			return credentialAccessResult{
				Allowed: true,
				Reason:  "template author: " + result.Reason,
			}
		}
	}

	return credentialAccessResult{
		Allowed: false,
		Reason:  "credential access denied for the referenced credentials",
	}
}

// hasCredentialAccess checks whether a single actor has access to
// credentials in the given room. Three conditions are checked:
//
//  1. Actor has admin power level (100) in the credential room.
//  2. Actor has a grant with action matching credential/use/<ref>.
//  3. Actor IS the credential identity (full user ID matches state key).
//
// The first matching condition wins.
func (d *Daemon) hasCredentialAccess(
	ctx context.Context,
	credentialRoomID ref.RoomID,
	actor ref.UserID,
	credentialRef schema.CredentialRef,
	action string,
) credentialAccessResult {
	// Condition 1: admin power level in the credential room.
	// Fetch power levels from the credential room and check the
	// actor's level. Admin (PL 100) means full credential access.
	powerLevels, powerErr := messaging.GetState[schema.PowerLevels](
		ctx, d.session, credentialRoomID,
		schema.MatrixEventTypePowerLevels, "",
	)
	if powerErr == nil {
		actorLevel := powerLevels.UserLevel(actor.String())
		if actorLevel >= 100 {
			return credentialAccessResult{
				Allowed: true,
				Reason:  "admin power level in credential room",
			}
		}
	}

	// Condition 2: explicit credential/use grant.
	// Credential usage is a self-service action (no target principal),
	// so only action patterns are checked. Denials are evaluated
	// after grants: a matching denial overrides a matching grant.
	grants := d.authorizationIndex.Grants(actor)
	denials := d.authorizationIndex.Denials(actor)

	hasGrant := false
	for _, grant := range grants {
		for _, pattern := range grant.Actions {
			if authorization.MatchAction(pattern, action) {
				hasGrant = true
				break
			}
		}
		if hasGrant {
			break
		}
	}

	if hasGrant {
		// Check for overriding denial.
		for _, denial := range denials {
			for _, pattern := range denial.Actions {
				if authorization.MatchAction(pattern, action) {
					// Denial overrides grant.
					hasGrant = false
					break
				}
			}
			if !hasGrant {
				break
			}
		}
	}

	if hasGrant {
		return credentialAccessResult{
			Allowed: true,
			Reason:  "credential/use grant",
		}
	}

	// Condition 3: actor IS the credential identity.
	// The credential state key matches the actor's full user ID.
	if actor.String() == credentialRef.StateKey {
		return credentialAccessResult{
			Allowed: true,
			Reason:  "actor is credential identity",
		}
	}

	return credentialAccessResult{Allowed: false}
}

// substituteCredentialRefVariables replaces known variables in a raw
// credential_ref string before parsing. Templates declare credential
// references with variables like ${FLEET_ROOM} that must be resolved
// to literal room alias localparts before the reference can be parsed
// and used to resolve a Matrix room alias.
//
// Currently supported variables:
//   - ${FLEET_ROOM}: replaced with the daemon's fleet room alias localpart
func (d *Daemon) substituteCredentialRefVariables(credentialRef string) string {
	return strings.ReplaceAll(credentialRef, "${FLEET_ROOM}", d.fleet.Localpart())
}

// checkAllowedPipelines verifies that a pipeline reference is in the
// template's allowed list. Returns nil if allowed, an error if rejected.
//
// When AllowedPipelines is nil (absent), all pipelines are allowed.
// When AllowedPipelines is non-nil but empty, no pipeline is allowed
// (deny-all). When non-nil and non-empty, only listed pipelines pass.
//
// The pipeline ref may be a full qualified reference (e.g.,
// "bureau/pipeline:environment-compose") or a simple name. The
// allowed_pipelines list is matched against the pipeline name (the
// portion after the last colon) to support both forms.
func checkAllowedPipelines(allowedPipelines *[]string, pipelineRef string) error {
	if allowedPipelines == nil {
		return nil
	}

	// Extract the pipeline name from the ref. Pipeline refs use the
	// format "<room-alias-localpart>:<name>". Match against the name
	// portion so that allowed_pipelines entries like "environment-compose"
	// match full refs like "bureau/pipeline:environment-compose".
	pipelineName := pipelineRef
	if index := strings.LastIndex(pipelineRef, ":"); index >= 0 {
		pipelineName = pipelineRef[index+1:]
	}

	for _, allowed := range *allowedPipelines {
		if allowed == pipelineName || allowed == pipelineRef {
			return nil
		}
	}
	return fmt.Errorf("pipeline %q is not in the template's allowed_pipelines list", pipelineRef)
}
