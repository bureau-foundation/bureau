// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// configStore is the subset of *messaging.Session needed for reading
// and writing machine config state events. Tests substitute a fake.
type configStore interface {
	GetStateEvent(ctx context.Context, roomID, eventType, stateKey string) (json.RawMessage, error)
	SendStateEvent(ctx context.Context, roomID, eventType, stateKey string, content any) (string, error)
}

// readMachineConfig reads the current MachineConfig from a machine's
// config room. Returns an empty MachineConfig if no config event
// exists yet.
func (fc *FleetController) readMachineConfig(ctx context.Context, machineLocalpart string) (*schema.MachineConfig, error) {
	configRoomID, exists := fc.configRooms[machineLocalpart]
	if !exists {
		return nil, fmt.Errorf("no config room for machine %s", machineLocalpart)
	}

	raw, err := fc.configStore.GetStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineLocalpart)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return &schema.MachineConfig{}, nil
		}
		return nil, fmt.Errorf("reading machine config for %s: %w", machineLocalpart, err)
	}

	var config schema.MachineConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("parsing machine config for %s: %w", machineLocalpart, err)
	}
	return &config, nil
}

// writeMachineConfig writes a MachineConfig state event to a machine's
// config room and returns the event ID assigned by the homeserver.
func (fc *FleetController) writeMachineConfig(ctx context.Context, machineLocalpart string, config *schema.MachineConfig) (string, error) {
	configRoomID, exists := fc.configRooms[machineLocalpart]
	if !exists {
		return "", fmt.Errorf("no config room for machine %s", machineLocalpart)
	}

	eventID, err := fc.configStore.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineLocalpart, config)
	if err != nil {
		return "", fmt.Errorf("writing machine config for %s: %w", machineLocalpart, err)
	}
	return eventID, nil
}

// buildAssignment constructs a PrincipalAssignment for a fleet-managed
// service. The assignment is tagged with the fleet controller's
// principalName so it can identify its own assignments later.
func (fc *FleetController) buildAssignment(serviceLocalpart string, definition *schema.FleetServiceContent) schema.PrincipalAssignment {
	return schema.PrincipalAssignment{
		Localpart: serviceLocalpart,
		Template:  definition.Template,
		AutoStart: true,
		Labels: map[string]string{
			"fleet_managed": fc.principalName,
		},
	}
}

// place assigns a fleet service to a machine by adding a
// PrincipalAssignment to the machine's MachineConfig. Reads the current
// config from Matrix, appends the new assignment (preserving all
// existing principals), and writes the updated config back.
//
// Caller must hold fc.mu.
func (fc *FleetController) place(ctx context.Context, serviceLocalpart, machineLocalpart string) error {
	serviceState, exists := fc.services[serviceLocalpart]
	if !exists {
		return fmt.Errorf("service %s not found", serviceLocalpart)
	}
	if serviceState.definition == nil {
		return fmt.Errorf("service %s has no definition", serviceLocalpart)
	}

	machine, exists := fc.machines[machineLocalpart]
	if !exists {
		return fmt.Errorf("machine %s not found", machineLocalpart)
	}
	if machine.configRoomID == "" {
		return fmt.Errorf("machine %s has no config room", machineLocalpart)
	}

	if _, alreadyPlaced := machine.assignments[serviceLocalpart]; alreadyPlaced {
		return fmt.Errorf("service %s is already placed on machine %s", serviceLocalpart, machineLocalpart)
	}

	config, err := fc.readMachineConfig(ctx, machineLocalpart)
	if err != nil {
		return err
	}

	assignment := fc.buildAssignment(serviceLocalpart, serviceState.definition)
	config.Principals = append(config.Principals, assignment)

	eventID, err := fc.writeMachineConfig(ctx, machineLocalpart, config)
	if err != nil {
		return err
	}

	// Record the pending echo so that processMachineConfigEvent
	// skips stale /sync events until this write's echo arrives.
	machine.pendingEchoEventID = eventID

	// Update in-memory model.
	assignmentCopy := assignment
	machine.assignments[serviceLocalpart] = &assignmentCopy
	serviceState.instances[machineLocalpart] = &assignmentCopy

	fc.logger.Info("service placed",
		"service", serviceLocalpart,
		"machine", machineLocalpart,
	)
	return nil
}

// unplace removes a fleet-managed service from a machine by removing
// its PrincipalAssignment from the machine's MachineConfig. Only
// removes assignments tagged with this fleet controller's principalName.
// Preserves all other principals.
//
// Caller must hold fc.mu.
func (fc *FleetController) unplace(ctx context.Context, serviceLocalpart, machineLocalpart string) error {
	machine, exists := fc.machines[machineLocalpart]
	if !exists {
		return fmt.Errorf("machine %s not found", machineLocalpart)
	}
	if machine.configRoomID == "" {
		return fmt.Errorf("machine %s has no config room", machineLocalpart)
	}

	assignment, exists := machine.assignments[serviceLocalpart]
	if !exists {
		return fmt.Errorf("service %s is not placed on machine %s", serviceLocalpart, machineLocalpart)
	}
	if assignment.Labels["fleet_managed"] != fc.principalName {
		return fmt.Errorf("service %s on machine %s is managed by %q, not %q",
			serviceLocalpart, machineLocalpart,
			assignment.Labels["fleet_managed"], fc.principalName)
	}

	config, err := fc.readMachineConfig(ctx, machineLocalpart)
	if err != nil {
		return err
	}

	// Remove only the matching principal, preserve everything else.
	filtered := make([]schema.PrincipalAssignment, 0, len(config.Principals))
	for _, principal := range config.Principals {
		if principal.Localpart == serviceLocalpart {
			continue
		}
		filtered = append(filtered, principal)
	}
	config.Principals = filtered

	eventID, err := fc.writeMachineConfig(ctx, machineLocalpart, config)
	if err != nil {
		return err
	}

	// Record the pending echo so that processMachineConfigEvent
	// skips stale /sync events until this write's echo arrives.
	machine.pendingEchoEventID = eventID

	// Update in-memory model.
	delete(machine.assignments, serviceLocalpart)
	if serviceState, exists := fc.services[serviceLocalpart]; exists {
		delete(serviceState.instances, machineLocalpart)
	}

	fc.logger.Info("service unplaced",
		"service", serviceLocalpart,
		"machine", machineLocalpart,
	)
	return nil
}

// reconcile compares desired state (FleetServiceContent definitions)
// with current state (tracked instances) and triggers placements or
// removals to bring the fleet into alignment.
//
// Under-replicated services get new instances placed on the highest-
// scored eligible machines. Over-replicated services (above Replicas.Max)
// get excess instances removed starting with the lowest-scored machines.
//
// Errors from individual place/unplace operations are logged but do not
// stop reconciliation of other services. This avoids one misbehaving
// config room from blocking the entire fleet.
//
// Caller must hold fc.mu.
func (fc *FleetController) reconcile(ctx context.Context) {
	for serviceLocalpart, serviceState := range fc.services {
		if serviceState.definition == nil {
			continue
		}

		currentCount := len(serviceState.instances)
		desiredMin := serviceState.definition.Replicas.Min
		desiredMax := serviceState.definition.Replicas.Max

		if currentCount < desiredMin {
			fc.reconcilePlaceDeficit(ctx, serviceLocalpart, serviceState, desiredMin-currentCount)
		}

		if desiredMax > 0 && currentCount > desiredMax {
			fc.reconcileRemoveExcess(ctx, serviceLocalpart, serviceState, currentCount-desiredMax)
		}
	}
}

// reconcilePlaceDeficit places up to `deficit` new instances of a
// service on the highest-scored eligible machines that don't already
// host the service.
func (fc *FleetController) reconcilePlaceDeficit(ctx context.Context, serviceLocalpart string, serviceState *fleetServiceState, deficit int) {
	candidates := fc.scorePlacement(serviceState.definition)

	// Filter out machines that already host this service.
	var available []placementCandidate
	for _, candidate := range candidates {
		if _, hasInstance := serviceState.instances[candidate.machineLocalpart]; !hasInstance {
			available = append(available, candidate)
		}
	}

	placed := 0
	for _, candidate := range available {
		if placed >= deficit {
			break
		}
		if err := fc.place(ctx, serviceLocalpart, candidate.machineLocalpart); err != nil {
			fc.logger.Error("reconcile: placement failed",
				"service", serviceLocalpart,
				"machine", candidate.machineLocalpart,
				"error", err,
			)
			continue
		}
		placed++
	}

	if placed < deficit {
		fc.logger.Warn("reconcile: insufficient eligible machines",
			"service", serviceLocalpart,
			"desired_min", serviceState.definition.Replicas.Min,
			"current", len(serviceState.instances)-placed,
			"placed", placed,
			"still_needed", deficit-placed,
		)
	}
}

// reconcileRemoveExcess removes up to `excess` instances of a service,
// starting with the lowest-scored machines. Ties are broken by machine
// localpart for determinism.
func (fc *FleetController) reconcileRemoveExcess(ctx context.Context, serviceLocalpart string, serviceState *fleetServiceState, excess int) {
	type scoredInstance struct {
		machineLocalpart string
		score            int
	}

	var instances []scoredInstance
	for machineLocalpart := range serviceState.instances {
		score := fc.scoreMachine(machineLocalpart, serviceState.definition)
		instances = append(instances, scoredInstance{machineLocalpart, score})
	}

	// Sort ascending by score so weakest instances are removed first.
	sort.Slice(instances, func(i, j int) bool {
		if instances[i].score != instances[j].score {
			return instances[i].score < instances[j].score
		}
		return instances[i].machineLocalpart < instances[j].machineLocalpart
	})

	removed := 0
	for _, instance := range instances {
		if removed >= excess {
			break
		}
		if err := fc.unplace(ctx, serviceLocalpart, instance.machineLocalpart); err != nil {
			fc.logger.Error("reconcile: removal failed",
				"service", serviceLocalpart,
				"machine", instance.machineLocalpart,
				"error", err,
			)
			continue
		}
		removed++
	}
}
