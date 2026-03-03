// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"github.com/bureau-foundation/bureau/lib/schema"
	fleetschema "github.com/bureau-foundation/bureau/lib/schema/fleet"
)

// Fleet controller socket response types. These are CBOR wire types
// mirroring the fleet controller's response structs in
// cmd/bureau-fleet-controller/socket.go. Any CLI package that calls
// the fleet controller socket decodes into these types.

// ServiceEntry is a service summary from the fleet controller's
// list-services response.
type ServiceEntry struct {
	Localpart string `cbor:"localpart"`
	Template  string `cbor:"template"`
	Replicas  int    `cbor:"replicas_min"`
	Instances int    `cbor:"instances"`
	Failover  string `cbor:"failover"`
	Priority  int    `cbor:"priority"`
}

// ListServicesResponse is the fleet controller's response to the
// "list-services" action.
type ListServicesResponse struct {
	Services []ServiceEntry `cbor:"services"`
}

// ShowServiceInstance is a single placement instance within a
// ShowServiceResponse.
type ShowServiceInstance struct {
	Machine    string                      `cbor:"machine"`
	Assignment *schema.PrincipalAssignment `cbor:"assignment"`
}

// ShowServiceResponse is the fleet controller's response to the
// "show-service" action.
type ShowServiceResponse struct {
	Localpart  string                           `cbor:"localpart"`
	Definition *fleetschema.FleetServiceContent `cbor:"definition"`
	Instances  []ShowServiceInstance            `cbor:"instances"`
}

// PlaceResponse is the fleet controller's response to the "place" action.
type PlaceResponse struct {
	Service string `cbor:"service"`
	Machine string `cbor:"machine"`
	Score   int    `cbor:"score"`
}

// UnplaceResponse is the fleet controller's response to the "unplace" action.
type UnplaceResponse struct {
	Service string `cbor:"service"`
	Machine string `cbor:"machine"`
}

// PlanCandidate is a scored machine in the fleet controller's plan response.
type PlanCandidate struct {
	Machine string `cbor:"machine"`
	Score   int    `cbor:"score"`
}

// PlanResponse is the fleet controller's response to the "plan" action.
type PlanResponse struct {
	Service         string          `cbor:"service"`
	Candidates      []PlanCandidate `cbor:"candidates"`
	CurrentMachines []string        `cbor:"current_machines"`
}

// FormatStringList joins a string slice for display, returning "-" for
// empty slices.
func FormatStringList(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	result := items[0]
	for _, item := range items[1:] {
		result += ", " + item
	}
	return result
}
