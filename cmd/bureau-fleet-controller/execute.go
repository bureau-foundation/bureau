// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/fleet"
	"github.com/bureau-foundation/bureau/messaging"
)

// configStore is the subset of messaging.Session needed for reading
// and writing machine config state events. Tests substitute a fake.
type configStore interface {
	GetStateEvent(ctx context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string) (json.RawMessage, error)
	SendStateEvent(ctx context.Context, roomID ref.RoomID, eventType ref.EventType, stateKey string, content any) (ref.EventID, error)
}

// readMachineConfig reads the current MachineConfig from a machine's
// config room. Returns an empty MachineConfig if no config event
// exists yet.
func (fc *FleetController) readMachineConfig(ctx context.Context, machineUserID ref.UserID) (*schema.MachineConfig, error) {
	configRoomID, exists := fc.configRooms[machineUserID]
	if !exists {
		return nil, fmt.Errorf("no config room for machine %s", machineUserID)
	}

	raw, err := fc.configStore.GetStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineUserID.StateKey())
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return &schema.MachineConfig{}, nil
		}
		return nil, fmt.Errorf("reading machine config for %s: %w", machineUserID, err)
	}

	var config schema.MachineConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("parsing machine config for %s: %w", machineUserID, err)
	}
	return &config, nil
}

// writeMachineConfig writes a MachineConfig state event to a machine's
// config room and returns the event ID assigned by the homeserver.
func (fc *FleetController) writeMachineConfig(ctx context.Context, machineUserID ref.UserID, config *schema.MachineConfig) (ref.EventID, error) {
	configRoomID, exists := fc.configRooms[machineUserID]
	if !exists {
		return ref.EventID{}, fmt.Errorf("no config room for machine %s", machineUserID)
	}

	eventID, err := fc.configStore.SendStateEvent(ctx, configRoomID, schema.EventTypeMachineConfig, machineUserID.StateKey(), config)
	if err != nil {
		return ref.EventID{}, fmt.Errorf("writing machine config for %s: %w", machineUserID, err)
	}
	return eventID, nil
}

// buildAssignment constructs a PrincipalAssignment for a fleet-managed
// service. The assignment is tagged with the fleet controller's
// principalName so it can identify its own assignments later.
// Authorization fields (MatrixPolicy, ServiceVisibility, Authorization)
// and Payload are propagated from the fleet service definition.
func (fc *FleetController) buildAssignment(serviceLocalpart string, definition *fleet.FleetServiceContent) (schema.PrincipalAssignment, error) {
	// FleetServiceContent.Payload is json.RawMessage (opaque Matrix
	// storage) while PrincipalAssignment.Payload is map[string]any
	// (Go-side manipulation). Convert here.
	payload, err := fleetPayloadToMap(definition.Payload)
	if err != nil {
		return schema.PrincipalAssignment{}, fmt.Errorf("parsing payload for %s: %w", serviceLocalpart, err)
	}

	// Construct a full entity reference from the bare account localpart
	// (e.g., "service/stt/whisper") and the fleet controller's fleet context.
	entity, err := ref.NewEntityFromAccountLocalpart(fc.fleet, serviceLocalpart)
	if err != nil {
		return schema.PrincipalAssignment{}, fmt.Errorf("constructing entity for %s: %w", serviceLocalpart, err)
	}

	return schema.PrincipalAssignment{
		Principal:         entity,
		Template:          definition.Template,
		AutoStart:         true,
		Payload:           payload,
		MatrixPolicy:      definition.MatrixPolicy,
		ServiceVisibility: definition.ServiceVisibility,
		Authorization:     definition.Authorization,
		Labels: map[string]string{
			"fleet_managed": fc.principalName,
		},
	}, nil
}

// fleetPayloadToMap converts a json.RawMessage payload from a
// FleetServiceContent to the map[string]any used by PrincipalAssignment.
// Returns nil if the input is nil or empty.
func fleetPayloadToMap(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parsing fleet service payload: %w", err)
	}
	return result, nil
}

// place assigns a fleet service to a machine by adding a
// PrincipalAssignment to the machine's MachineConfig. Reads the current
// config from Matrix, appends the new assignment (preserving all
// existing principals), and writes the updated config back.
//
// Caller must hold fc.mu.
func (fc *FleetController) place(ctx context.Context, serviceUserID, machineUserID ref.UserID) error {
	serviceState, exists := fc.services[serviceUserID]
	if !exists {
		return fmt.Errorf("service %s not found", serviceUserID)
	}
	if serviceState.definition == nil {
		return fmt.Errorf("service %s has no definition", serviceUserID)
	}

	machine, exists := fc.machines[machineUserID]
	if !exists {
		return fmt.Errorf("machine %s not found", machineUserID)
	}
	if machine.configRoomID.IsZero() {
		return fmt.Errorf("machine %s has no config room", machineUserID)
	}

	if _, alreadyPlaced := machine.assignments[serviceUserID]; alreadyPlaced {
		return fmt.Errorf("service %s is already placed on machine %s", serviceUserID, machineUserID)
	}

	config, err := fc.readMachineConfig(ctx, machineUserID)
	if err != nil {
		return err
	}

	// buildAssignment takes the bare account localpart (e.g.,
	// "service/stt/whisper") for entity construction. Extract it
	// from the full user ID via the Entity's AccountLocalpart.
	serviceEntity, err := ref.ParseEntityUserID(serviceUserID.String())
	if err != nil {
		return fmt.Errorf("parsing service user ID %s: %w", serviceUserID, err)
	}
	assignment, err := fc.buildAssignment(serviceEntity.AccountLocalpart(), serviceState.definition)
	if err != nil {
		return err
	}
	config.Principals = append(config.Principals, assignment)

	eventID, err := fc.writeMachineConfig(ctx, machineUserID, config)
	if err != nil {
		return err
	}

	// Record the pending echo so that processMachineConfigEvent
	// skips stale /sync events until this write's echo arrives.
	machine.pendingEchoEventID = eventID

	// Update in-memory model.
	assignmentCopy := assignment
	machine.assignments[serviceUserID] = &assignmentCopy
	serviceState.instances[machineUserID] = &assignmentCopy

	fc.logger.Info("service placed",
		"service", serviceUserID,
		"machine", machineUserID,
	)
	return nil
}

// unplace removes a fleet-managed service from a machine by removing
// its PrincipalAssignment from the machine's MachineConfig. Only
// removes assignments tagged with this fleet controller's principalName.
// Preserves all other principals.
//
// Caller must hold fc.mu.
func (fc *FleetController) unplace(ctx context.Context, serviceUserID, machineUserID ref.UserID) error {
	machine, exists := fc.machines[machineUserID]
	if !exists {
		return fmt.Errorf("machine %s not found", machineUserID)
	}
	if machine.configRoomID.IsZero() {
		return fmt.Errorf("machine %s has no config room", machineUserID)
	}

	assignment, exists := machine.assignments[serviceUserID]
	if !exists {
		return fmt.Errorf("service %s is not placed on machine %s", serviceUserID, machineUserID)
	}
	if assignment.Labels["fleet_managed"] != fc.principalName {
		return fmt.Errorf("service %s on machine %s is managed by %q, not %q",
			serviceUserID, machineUserID,
			assignment.Labels["fleet_managed"], fc.principalName)
	}

	config, err := fc.readMachineConfig(ctx, machineUserID)
	if err != nil {
		return err
	}

	// Remove only the matching principal, preserve everything else.
	// Compare by user ID to match regardless of whether the localpart
	// or user ID was used in the original assignment.
	filtered := make([]schema.PrincipalAssignment, 0, len(config.Principals))
	for _, principal := range config.Principals {
		if principal.Principal.UserID() == serviceUserID {
			continue
		}
		filtered = append(filtered, principal)
	}
	config.Principals = filtered

	eventID, err := fc.writeMachineConfig(ctx, machineUserID, config)
	if err != nil {
		return err
	}

	// Record the pending echo so that processMachineConfigEvent
	// skips stale /sync events until this write's echo arrives.
	machine.pendingEchoEventID = eventID

	// Update in-memory model.
	delete(machine.assignments, serviceUserID)
	if serviceState, exists := fc.services[serviceUserID]; exists {
		delete(serviceState.instances, machineUserID)
	}

	fc.logger.Info("service unplaced",
		"service", serviceUserID,
		"machine", machineUserID,
	)
	return nil
}

// cordonMachine sets the "cordoned" label on a machine's MachineInfo
// state event in the fleet's machine room, making the machine ineligible
// for new placements. Also updates the in-memory model so the scoring
// engine sees the cordon immediately. Returns true if the machine was
// already cordoned (no write performed).
//
// Caller must hold fc.mu.
func (fc *FleetController) cordonMachine(ctx context.Context, machineUserID ref.UserID) (bool, error) {
	machine, exists := fc.machines[machineUserID]
	if !exists {
		return false, fmt.Errorf("machine %s not found", machineUserID)
	}

	// Check in-memory model first — avoids a Matrix round-trip if
	// already cordoned.
	if machine.info != nil && machine.info.Labels["cordoned"] != "" {
		return true, nil
	}

	// Read the canonical MachineInfo from the machine room.
	raw, err := fc.configStore.GetStateEvent(ctx, fc.machineRoomID, schema.EventTypeMachineInfo, machineUserID.StateKey())
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			return false, fmt.Errorf("no MachineInfo for machine %s in machine room", machineUserID)
		}
		return false, fmt.Errorf("reading MachineInfo for %s: %w", machineUserID, err)
	}

	var info schema.MachineInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return false, fmt.Errorf("parsing MachineInfo for %s: %w", machineUserID, err)
	}

	if info.Labels == nil {
		info.Labels = make(map[string]string)
	}
	info.Labels["cordoned"] = "true"

	if _, err := fc.configStore.SendStateEvent(ctx, fc.machineRoomID, schema.EventTypeMachineInfo, machineUserID.StateKey(), &info); err != nil {
		return false, fmt.Errorf("writing cordoned label for %s: %w", machineUserID, err)
	}

	// Update in-memory model to match.
	if machine.info != nil {
		if machine.info.Labels == nil {
			machine.info.Labels = make(map[string]string)
		}
		machine.info.Labels["cordoned"] = "true"
	}

	fc.logger.Info("machine cordoned by drain", "machine", machineUserID)
	return false, nil
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
	for serviceUserID, serviceState := range fc.services {
		if serviceState.definition == nil {
			continue
		}

		currentCount := len(serviceState.instances)
		desiredMin := serviceState.definition.Replicas.Min
		desiredMax := serviceState.definition.Replicas.Max

		if currentCount < desiredMin {
			fc.reconcilePlaceDeficit(ctx, serviceUserID, serviceState, desiredMin-currentCount)
		}

		if desiredMax > 0 && currentCount > desiredMax {
			fc.reconcileRemoveExcess(ctx, serviceUserID, serviceState, currentCount-desiredMax)
		}
	}
}

// reconcilePlaceDeficit places up to `deficit` new instances of a
// service on the highest-scored eligible machines that don't already
// host the service.
func (fc *FleetController) reconcilePlaceDeficit(ctx context.Context, serviceUserID ref.UserID, serviceState *fleetServiceState, deficit int) {
	candidates := fc.scorePlacement(serviceState.definition)

	// Filter out machines that already host this service.
	var available []placementCandidate
	for _, candidate := range candidates {
		if _, hasInstance := serviceState.instances[candidate.machineUserID]; !hasInstance {
			available = append(available, candidate)
		}
	}

	placed := 0
	for _, candidate := range available {
		if placed >= deficit {
			break
		}
		if err := fc.place(ctx, serviceUserID, candidate.machineUserID); err != nil {
			fc.logger.Error("reconcile: placement failed",
				"service", serviceUserID,
				"machine", candidate.machineUserID,
				"error", err,
			)
			continue
		}
		placed++
	}

	if placed < deficit {
		fc.logger.Warn("reconcile: insufficient eligible machines",
			"service", serviceUserID,
			"desired_min", serviceState.definition.Replicas.Min,
			"current", len(serviceState.instances)-placed,
			"placed", placed,
			"still_needed", deficit-placed,
		)
	}
}

// reconcileRemoveExcess removes up to `excess` instances of a service,
// starting with the lowest-scored machines. Ties are broken by machine
// user ID string for determinism.
func (fc *FleetController) reconcileRemoveExcess(ctx context.Context, serviceUserID ref.UserID, serviceState *fleetServiceState, excess int) {
	type scoredInstance struct {
		machineUserID ref.UserID
		score         int
	}

	var instances []scoredInstance
	for machineUserID := range serviceState.instances {
		score := fc.scoreMachine(machineUserID, serviceState.definition)
		instances = append(instances, scoredInstance{machineUserID, score})
	}

	// Sort ascending by score so weakest instances are removed first.
	sort.Slice(instances, func(i, j int) bool {
		if instances[i].score != instances[j].score {
			return instances[i].score < instances[j].score
		}
		return instances[i].machineUserID.String() < instances[j].machineUserID.String()
	})

	removed := 0
	for _, instance := range instances {
		if removed >= excess {
			break
		}
		if err := fc.unplace(ctx, serviceUserID, instance.machineUserID); err != nil {
			fc.logger.Error("reconcile: removal failed",
				"service", serviceUserID,
				"machine", instance.machineUserID,
				"error", err,
			)
			continue
		}
		removed++
	}
}
