// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// registerActions registers all socket API actions on the server.
// The "status" action is unauthenticated (pure liveness check).
// All other actions use HandleAuth and require a valid service token.
func (fc *FleetController) registerActions(server *service.SocketServer) {
	server.Handle("status", fc.handleStatus)
	server.HandleAuth("info", fc.handleInfo)
	server.HandleAuth("list-machines", fc.handleListMachines)
	server.HandleAuth("list-services", fc.handleListServices)
	server.HandleAuth("show-machine", fc.handleShowMachine)
	server.HandleAuth("show-service", fc.handleShowService)
	server.HandleAuth("place", fc.handlePlace)
	server.HandleAuth("unplace", fc.handleUnplace)
	server.HandleAuth("plan", fc.handlePlan)
}

// --- Authorization helper ---

// requireGrant checks that the token carries a grant for the given
// action pattern (e.g., "fleet/info"). Returns nil if authorized,
// or an error suitable for returning to the client.
func requireGrant(token *servicetoken.Token, action string) error {
	if !servicetoken.GrantsAllow(token.Grants, action, "") {
		return fmt.Errorf("access denied: missing grant for %s", action)
	}
	return nil
}

// --- Unauthenticated actions ---

// statusResponse is the response to the "status" action. Contains
// only liveness information — no machine counts, service counts, or
// other data that could disclose what the fleet controller is tracking.
type statusResponse struct {
	// UptimeSeconds is how long the service has been running.
	UptimeSeconds int `cbor:"uptime_seconds"`
}

// handleStatus returns a minimal liveness response. This is the only
// unauthenticated action — it reveals nothing about the fleet
// controller's state beyond "I am alive."
func (fc *FleetController) handleStatus(ctx context.Context, raw []byte) (any, error) {
	uptime := fc.clock.Now().Sub(fc.startedAt)
	return statusResponse{
		UptimeSeconds: int(uptime.Seconds()),
	}, nil
}

// --- Authenticated diagnostic action ---

// infoResponse is the response to the "info" action. Contains
// aggregate model counts and uptime for fleet health monitoring.
type infoResponse struct {
	UptimeSeconds int `cbor:"uptime_seconds"`
	Machines      int `cbor:"machines"`
	Services      int `cbor:"services"`
	Definitions   int `cbor:"definitions"`
	ConfigRooms   int `cbor:"config_rooms"`
}

// handleInfo returns diagnostic counts about the fleet model. Requires
// the "fleet/info" grant — model counts are operational metadata that
// should not be disclosed to unprivileged callers.
func (fc *FleetController) handleInfo(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "fleet/info"); err != nil {
		return nil, err
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	uptime := fc.clock.Now().Sub(fc.startedAt)
	return infoResponse{
		UptimeSeconds: int(uptime.Seconds()),
		Machines:      len(fc.machines),
		Services:      len(fc.services),
		Definitions:   len(fc.definitions),
		ConfigRooms:   len(fc.configRooms),
	}, nil
}

// --- List machines ---

// listMachinesResponse is the response to the "list-machines" action.
type listMachinesResponse struct {
	Machines []machineSummary `cbor:"machines"`
}

// machineSummary is a compact representation of a tracked machine for
// list views. Fields are extracted from MachineInfo and MachineStatus,
// with zero values for machines that have not yet published one or
// both events.
type machineSummary struct {
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

// handleListMachines returns a sorted list of all tracked machines.
// Machines with incomplete state (nil info or status) are included
// with zero values for the missing fields. Requires "fleet/list-machines".
func (fc *FleetController) handleListMachines(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "fleet/list-machines"); err != nil {
		return nil, err
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	summaries := make([]machineSummary, 0, len(fc.machines))
	for localpart, machine := range fc.machines {
		summary := machineSummary{
			Localpart:    localpart,
			Assignments:  len(machine.assignments),
			ConfigRoomID: machine.configRoomID,
		}

		if machine.info != nil {
			summary.Hostname = machine.info.Hostname
			summary.MemoryTotalMB = machine.info.MemoryTotalMB
			summary.GPUCount = len(machine.info.GPUs)
			summary.Labels = machine.info.Labels
		}

		if machine.status != nil {
			summary.CPUPercent = machine.status.CPUPercent
			summary.MemoryUsedMB = machine.status.MemoryUsedMB
		}

		summaries = append(summaries, summary)
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Localpart < summaries[j].Localpart
	})

	return listMachinesResponse{Machines: summaries}, nil
}

// --- List services ---

// listServicesResponse is the response to the "list-services" action.
type listServicesResponse struct {
	Services []serviceSummary `cbor:"services"`
}

// serviceSummary is a compact representation of a fleet-managed service
// for list views.
type serviceSummary struct {
	Localpart string `cbor:"localpart"`
	Template  string `cbor:"template"`
	Replicas  int    `cbor:"replicas_min"`
	Instances int    `cbor:"instances"`
	Failover  string `cbor:"failover"`
	Priority  int    `cbor:"priority"`
}

// handleListServices returns a sorted list of all fleet-managed services.
// Requires "fleet/list-services".
func (fc *FleetController) handleListServices(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "fleet/list-services"); err != nil {
		return nil, err
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	summaries := make([]serviceSummary, 0, len(fc.services))
	for localpart, serviceState := range fc.services {
		summary := serviceSummary{
			Localpart: localpart,
			Instances: len(serviceState.instances),
		}

		if serviceState.definition != nil {
			summary.Template = serviceState.definition.Template
			summary.Replicas = serviceState.definition.Replicas.Min
			summary.Failover = serviceState.definition.Failover
			summary.Priority = serviceState.definition.Priority
		}

		summaries = append(summaries, summary)
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Localpart < summaries[j].Localpart
	})

	return listServicesResponse{Services: summaries}, nil
}

// --- Show machine ---

// showMachineRequest identifies the machine to inspect.
type showMachineRequest struct {
	Machine string `cbor:"machine"`
}

// showMachineResponse is the full detail view of a single machine.
type showMachineResponse struct {
	Localpart    string                       `cbor:"localpart"`
	Info         *schema.MachineInfo          `cbor:"info"`
	Status       *schema.MachineStatus        `cbor:"status"`
	Assignments  []schema.PrincipalAssignment `cbor:"assignments"`
	ConfigRoomID string                       `cbor:"config_room_id"`
}

// handleShowMachine returns the full state of a single machine.
// Requires "fleet/show-machine". Returns an error if the machine
// is not tracked.
func (fc *FleetController) handleShowMachine(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "fleet/show-machine"); err != nil {
		return nil, err
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	var request showMachineRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}
	if request.Machine == "" {
		return nil, fmt.Errorf("missing required field: machine")
	}

	machine, exists := fc.machines[request.Machine]
	if !exists {
		return nil, fmt.Errorf("machine %s not found", request.Machine)
	}

	// Collect assignments into a sorted slice for deterministic output.
	assignments := make([]schema.PrincipalAssignment, 0, len(machine.assignments))
	for _, assignment := range machine.assignments {
		assignments = append(assignments, *assignment)
	}
	sort.Slice(assignments, func(i, j int) bool {
		return assignments[i].Localpart < assignments[j].Localpart
	})

	return showMachineResponse{
		Localpart:    request.Machine,
		Info:         machine.info,
		Status:       machine.status,
		Assignments:  assignments,
		ConfigRoomID: machine.configRoomID,
	}, nil
}

// --- Show service ---

// showServiceRequest identifies the service to inspect.
type showServiceRequest struct {
	Service string `cbor:"service"`
}

// showServiceResponse is the full detail view of a single fleet service.
type showServiceResponse struct {
	Localpart  string                      `cbor:"localpart"`
	Definition *schema.FleetServiceContent `cbor:"definition"`
	Instances  []serviceInstance           `cbor:"instances"`
}

// serviceInstance pairs a machine localpart with the PrincipalAssignment
// the fleet controller wrote for this service on that machine.
type serviceInstance struct {
	Machine    string                      `cbor:"machine"`
	Assignment *schema.PrincipalAssignment `cbor:"assignment"`
}

// handleShowService returns the full state of a single fleet service.
// Requires "fleet/show-service". Returns an error if the service
// is not tracked.
func (fc *FleetController) handleShowService(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "fleet/show-service"); err != nil {
		return nil, err
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	var request showServiceRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}
	if request.Service == "" {
		return nil, fmt.Errorf("missing required field: service")
	}

	serviceState, exists := fc.services[request.Service]
	if !exists {
		return nil, fmt.Errorf("service %s not found", request.Service)
	}

	// Collect instances into a sorted slice for deterministic output.
	instances := make([]serviceInstance, 0, len(serviceState.instances))
	for machineLocalpart, assignment := range serviceState.instances {
		instances = append(instances, serviceInstance{
			Machine:    machineLocalpart,
			Assignment: assignment,
		})
	}
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].Machine < instances[j].Machine
	})

	return showServiceResponse{
		Localpart:  request.Service,
		Definition: serviceState.definition,
		Instances:  instances,
	}, nil
}

// --- Place service ---

// placeRequest identifies the service to place and optionally a
// target machine. If Machine is empty, the scoring engine selects
// the best candidate.
type placeRequest struct {
	Service string `cbor:"service"`
	Machine string `cbor:"machine"`
}

// placeResponse confirms where a service was placed.
type placeResponse struct {
	Service string `cbor:"service"`
	Machine string `cbor:"machine"`
	Score   int    `cbor:"score"`
}

// handlePlace places a fleet service on a machine. If no machine is
// specified, the scoring engine selects the best eligible candidate.
// Manual placement (with a machine specified) bypasses scoring but
// still validates eligibility. Requires "fleet/place".
func (fc *FleetController) handlePlace(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "fleet/place"); err != nil {
		return nil, err
	}

	var request placeRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}
	if request.Service == "" {
		return nil, fmt.Errorf("missing required field: service")
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	// Determine the target machine.
	targetMachine := request.Machine
	score := -1

	if targetMachine == "" {
		// Use scoring engine to select the best machine.
		serviceState, exists := fc.services[request.Service]
		if !exists {
			return nil, fmt.Errorf("service %s not found", request.Service)
		}
		if serviceState.definition == nil {
			return nil, fmt.Errorf("service %s has no definition", request.Service)
		}

		candidates := fc.scorePlacement(serviceState.definition)

		// Filter out machines that already host this service.
		var available []placementCandidate
		for _, candidate := range candidates {
			if _, hasInstance := serviceState.instances[candidate.machineLocalpart]; !hasInstance {
				available = append(available, candidate)
			}
		}

		if len(available) == 0 {
			return nil, fmt.Errorf("no eligible machines for service %s", request.Service)
		}
		targetMachine = available[0].machineLocalpart
		score = available[0].score
	}

	if err := fc.place(ctx, request.Service, targetMachine); err != nil {
		return nil, err
	}

	return placeResponse{
		Service: request.Service,
		Machine: targetMachine,
		Score:   score,
	}, nil
}

// --- Unplace service ---

// unplaceRequest identifies the service and machine to unplace.
type unplaceRequest struct {
	Service string `cbor:"service"`
	Machine string `cbor:"machine"`
}

// unplaceResponse confirms the removal.
type unplaceResponse struct {
	Service string `cbor:"service"`
	Machine string `cbor:"machine"`
}

// handleUnplace removes a fleet-managed service from a machine.
// Requires "fleet/unplace".
func (fc *FleetController) handleUnplace(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "fleet/unplace"); err != nil {
		return nil, err
	}

	var request unplaceRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}
	if request.Service == "" {
		return nil, fmt.Errorf("missing required field: service")
	}
	if request.Machine == "" {
		return nil, fmt.Errorf("missing required field: machine")
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	if err := fc.unplace(ctx, request.Service, request.Machine); err != nil {
		return nil, err
	}

	return unplaceResponse{
		Service: request.Service,
		Machine: request.Machine,
	}, nil
}

// --- Plan (dry-run scoring) ---

// planRequest identifies the service to evaluate.
type planRequest struct {
	Service string `cbor:"service"`
}

// planResponse returns the scoring results and current placement.
type planResponse struct {
	Service         string          `cbor:"service"`
	Candidates      []planCandidate `cbor:"candidates"`
	CurrentMachines []string        `cbor:"current_machines"`
}

// planCandidate is a scored machine from the placement engine.
type planCandidate struct {
	Machine string `cbor:"machine"`
	Score   int    `cbor:"score"`
}

// handlePlan returns a dry-run placement evaluation: all eligible
// machines with their scores, plus the current placement. No state
// is modified. Requires "fleet/plan".
func (fc *FleetController) handlePlan(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if err := requireGrant(token, "fleet/plan"); err != nil {
		return nil, err
	}

	var request planRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}
	if request.Service == "" {
		return nil, fmt.Errorf("missing required field: service")
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	serviceState, exists := fc.services[request.Service]
	if !exists {
		return nil, fmt.Errorf("service %s not found", request.Service)
	}
	if serviceState.definition == nil {
		return nil, fmt.Errorf("service %s has no definition", request.Service)
	}

	// Score all eligible machines.
	scored := fc.scorePlacement(serviceState.definition)
	candidates := make([]planCandidate, len(scored))
	for i, candidate := range scored {
		candidates[i] = planCandidate{
			Machine: candidate.machineLocalpart,
			Score:   candidate.score,
		}
	}

	// Collect current placement (sorted for determinism).
	currentMachines := make([]string, 0, len(serviceState.instances))
	for machineLocalpart := range serviceState.instances {
		currentMachines = append(currentMachines, machineLocalpart)
	}
	sort.Strings(currentMachines)

	return planResponse{
		Service:         request.Service,
		Candidates:      candidates,
		CurrentMachines: currentMachines,
	}, nil
}
