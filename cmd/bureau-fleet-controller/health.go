// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/fleet"
)

// Health state constants for machines in the fleet.
const (
	healthOnline  = "online"
	healthSuspect = "suspect"
	healthOffline = "offline"
)

// defaultHeartbeatInterval is the expected interval between machine
// heartbeats when no fleet config overrides it.
const defaultHeartbeatInterval = 30 * time.Second

// heartbeatInterval returns the configured heartbeat interval from the
// fleet config. Falls back to defaultHeartbeatInterval if no config
// exists or the value is zero.
func (fc *FleetController) heartbeatInterval() time.Duration {
	for _, cfg := range fc.config {
		if cfg.HeartbeatIntervalSeconds > 0 {
			return time.Duration(cfg.HeartbeatIntervalSeconds) * time.Second
		}
	}
	return defaultHeartbeatInterval
}

// checkMachineHealth evaluates all tracked machines for heartbeat
// staleness and transitions their health states. Machines that
// transition to offline trigger failover of their fleet-managed
// services.
//
// Health state transitions:
//   - Heartbeat within 1x interval: online
//   - Heartbeat between 1x and 3x interval: suspect
//   - Heartbeat older than 3x interval: offline (triggers failover)
//
// Machines with no lastHeartbeat (never reported status) are left at
// their current health state — they may have been discovered through
// a config room but haven't published a status event yet.
//
// Caller must hold fc.mu.
func (fc *FleetController) checkMachineHealth(ctx context.Context) {
	now := fc.clock.Now()
	interval := fc.heartbeatInterval()
	suspectThreshold := interval
	offlineThreshold := 3 * interval

	for machineLocalpart, machine := range fc.machines {
		if machine.lastHeartbeat.IsZero() {
			continue
		}

		staleness := now.Sub(machine.lastHeartbeat)
		previousState := machine.healthState

		switch {
		case staleness <= suspectThreshold:
			machine.healthState = healthOnline
		case staleness <= offlineThreshold:
			if previousState != healthSuspect {
				fc.logger.Warn("machine heartbeat suspect",
					"machine", machineLocalpart,
					"staleness", staleness,
					"threshold", suspectThreshold,
				)
			}
			machine.healthState = healthSuspect
		default:
			machine.healthState = healthOffline
			if previousState != healthOffline {
				fc.logger.Error("machine offline",
					"machine", machineLocalpart,
					"staleness", staleness,
					"threshold", offlineThreshold,
				)
				fc.executeFailover(ctx, machineLocalpart, machine)
			}
		}
	}
}

// executeFailover removes all fleet-managed services from an offline
// machine and publishes a fleet alert for each removal. The services
// will be re-placed by reconcile (called later in the same sync cycle
// or on the next cycle).
//
// Caller must hold fc.mu.
func (fc *FleetController) executeFailover(ctx context.Context, machineLocalpart string, machine *machineState) {
	// Snapshot the service localparts before iterating because
	// unplace modifies machine.assignments.
	servicesToFailover := make([]string, 0, len(machine.assignments))
	for serviceLocalpart := range machine.assignments {
		servicesToFailover = append(servicesToFailover, serviceLocalpart)
	}

	for _, serviceLocalpart := range servicesToFailover {
		if err := fc.unplace(ctx, serviceLocalpart, machineLocalpart); err != nil {
			fc.logger.Error("failover: unplace failed",
				"service", serviceLocalpart,
				"machine", machineLocalpart,
				"error", err,
			)
			continue
		}

		fc.publishFleetAlert(ctx, fleet.FleetAlertContent{
			AlertType: "failover",
			Fleet:     fc.principalName,
			Service:   serviceLocalpart,
			Machine:   machineLocalpart,
			Message: fmt.Sprintf("machine %s offline, removed service %s",
				machineLocalpart, serviceLocalpart),
			ProposedActions: []fleet.ProposedAction{
				{
					Action:      "place",
					Service:     serviceLocalpart,
					FromMachine: machineLocalpart,
				},
			},
		})
	}
}

// publishFleetAlert writes a FleetAlertContent event to the fleet room
// as a state event. The state key combines the alert type, service, and
// machine to allow multiple active alerts without collision.
func (fc *FleetController) publishFleetAlert(ctx context.Context, alert fleet.FleetAlertContent) {
	stateKey := alertStateKey(alert)
	_, err := fc.configStore.SendStateEvent(ctx, fc.fleetRoomID,
		schema.EventTypeFleetAlert, stateKey, alert)
	if err != nil {
		fc.logger.Error("failed to publish fleet alert",
			"alert_type", alert.AlertType,
			"service", alert.Service,
			"machine", alert.Machine,
			"error", err,
		)
	}
}

// alertStateKey constructs a unique state key for a fleet alert from
// its type, service, and machine.
func alertStateKey(alert fleet.FleetAlertContent) string {
	key := alert.AlertType
	if alert.Service != "" {
		key += "/" + alert.Service
	}
	if alert.Machine != "" {
		key += "/" + alert.Machine
	}
	return key
}
