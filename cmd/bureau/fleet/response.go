// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"strings"

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

// DrainMovedEntry describes a service that was successfully relocated
// during a drain operation.
type DrainMovedEntry struct {
	Service   string `cbor:"service"`
	ToMachine string `cbor:"to_machine"`
	Score     int    `cbor:"score"`
}

// DrainStuckEntry describes a service that could not be relocated
// during a drain operation, with the reason it was stuck.
type DrainStuckEntry struct {
	Service string `cbor:"service"`
	Reason  string `cbor:"reason"`
}

// DrainResponse is the fleet controller's response to the "drain" action.
type DrainResponse struct {
	Machine  string            `cbor:"machine"`
	Moved    []DrainMovedEntry `cbor:"moved"`
	Stuck    []DrainStuckEntry `cbor:"stuck"`
	Cordoned bool              `cbor:"cordoned"`
}

// MachineEntry is a machine summary from the fleet controller's
// list-machines response.
type MachineEntry struct {
	Localpart     string            `cbor:"localpart"`
	Hostname      string            `cbor:"hostname"`
	CPUPercent    int               `cbor:"cpu_percent"`
	MemoryUsedMB  int               `cbor:"memory_used_mb"`
	MemoryTotalMB int               `cbor:"memory_total_mb"`
	GPUCount      int               `cbor:"gpu_count"`
	Labels        map[string]string `cbor:"labels"`
	Assignments   int               `cbor:"assignments"`
	ConfigRoomID  string            `cbor:"config_room_id"`
}

// MachinesResponse is the fleet controller's response to the
// "list-machines" action.
type MachinesResponse struct {
	Machines []MachineEntry `cbor:"machines"`
}

// ShowMachineResponse is the fleet controller's response to the
// "show-machine" action.
type ShowMachineResponse struct {
	Localpart    string                       `cbor:"localpart"`
	Info         *schema.MachineInfo          `cbor:"info"`
	Status       *schema.MachineStatus        `cbor:"status"`
	Assignments  []schema.PrincipalAssignment `cbor:"assignments"`
	ConfigRoomID string                       `cbor:"config_room_id"`
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

// FormatLabels formats a label map as "key=value, ..." for display,
// returning "-" for empty maps.
func FormatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(labels))
	for key, value := range labels {
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, ", ")
}
