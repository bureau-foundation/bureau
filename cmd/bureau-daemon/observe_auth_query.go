// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"

	"github.com/bureau-foundation/bureau/lib/authorization"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/observe"
)

// handleQueryAuthorization handles a query_authorization request: evaluates
// whether the specified actor can perform the specified action on the
// specified target using the daemon's live authorization index, and returns
// the full evaluation trace.
//
// Authentication is handled by handleObserveClient before dispatch.
func (d *Daemon) handleQueryAuthorization(clientConnection net.Conn, request observeRequest) {
	if request.Actor == "" {
		d.sendObserveError(clientConnection, "actor is required for query_authorization")
		return
	}
	if request.AuthAction == "" {
		d.sendObserveError(clientConnection, "auth_action is required for query_authorization")
		return
	}

	d.logger.Info("query_authorization requested",
		"observer", request.Observer,
		"actor", request.Actor,
		"auth_action", request.AuthAction,
		"target", request.Target,
	)

	result := authorization.Authorized(d.authorizationIndex, request.Actor, request.AuthAction, request.Target)

	response := observe.AuthorizationResponse{
		OK:       true,
		Decision: result.Decision.String(),
	}

	if result.Decision == authorization.Deny {
		response.Reason = result.Reason.String()
	}

	// Evaluation trace: which rules matched.
	response.MatchedGrant = result.MatchedGrant
	response.MatchedDenial = result.MatchedDenial
	response.MatchedAllowance = result.MatchedAllowance
	response.MatchedAllowanceDenial = result.MatchedAllowanceDenial

	// Full policy context for the actor.
	response.ActorGrants = d.authorizationIndex.Grants(request.Actor)
	if response.ActorGrants == nil {
		response.ActorGrants = []schema.Grant{}
	}
	response.ActorDenials = d.authorizationIndex.Denials(request.Actor)
	if response.ActorDenials == nil {
		response.ActorDenials = []schema.Denial{}
	}

	// Full policy context for the target (cross-principal actions only).
	if request.Target != "" {
		response.TargetAllowances = d.authorizationIndex.Allowances(request.Target)
		if response.TargetAllowances == nil {
			response.TargetAllowances = []schema.Allowance{}
		}
		response.TargetAllowanceDenials = d.authorizationIndex.AllowanceDenials(request.Target)
		if response.TargetAllowanceDenials == nil {
			response.TargetAllowanceDenials = []schema.AllowanceDenial{}
		}
	}

	d.sendObserveJSON(clientConnection, response)
}

// handleQueryPrincipalPolicy handles query_grants and query_allowances
// requests: returns the complete resolved policy for one side of the
// authorization model. query_grants returns the subject side (what the
// principal can do), query_allowances returns the target side (who can
// act on the principal).
//
// Authentication is handled by handleObserveClient before dispatch.
func (d *Daemon) handleQueryPrincipalPolicy(clientConnection net.Conn, request observeRequest) {
	if request.Principal == "" {
		d.sendObserveError(clientConnection, "principal is required for "+request.Action)
		return
	}

	d.logger.Info(request.Action+" requested",
		"observer", request.Observer,
		"principal", request.Principal,
	)

	switch request.Action {
	case "query_grants":
		grants := d.authorizationIndex.Grants(request.Principal)
		if grants == nil {
			grants = []schema.Grant{}
		}
		denials := d.authorizationIndex.Denials(request.Principal)
		if denials == nil {
			denials = []schema.Denial{}
		}
		d.sendObserveJSON(clientConnection, observe.GrantsResponse{
			OK:        true,
			Principal: request.Principal,
			Grants:    grants,
			Denials:   denials,
		})

	case "query_allowances":
		allowances := d.authorizationIndex.Allowances(request.Principal)
		if allowances == nil {
			allowances = []schema.Allowance{}
		}
		allowanceDenials := d.authorizationIndex.AllowanceDenials(request.Principal)
		if allowanceDenials == nil {
			allowanceDenials = []schema.AllowanceDenial{}
		}
		d.sendObserveJSON(clientConnection, observe.AllowancesResponse{
			OK:               true,
			Principal:        request.Principal,
			Allowances:       allowances,
			AllowanceDenials: allowanceDenials,
		})
	}
}
