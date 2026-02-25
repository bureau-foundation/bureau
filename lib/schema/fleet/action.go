// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

// Authorization action constants for the fleet controller. Enforced
// by the fleet controller (cmd/bureau-fleet-controller/) via service
// token grant verification. See lib/schema/action.go for the action
// system overview and non-fleet-specific actions.
const (
	ActionInfo          = "fleet/info"
	ActionListMachines  = "fleet/list-machines"
	ActionListServices  = "fleet/list-services"
	ActionShowMachine   = "fleet/show-machine"
	ActionShowService   = "fleet/show-service"
	ActionPlace         = "fleet/place"
	ActionUnplace       = "fleet/unplace"
	ActionPlan          = "fleet/plan"
	ActionMachineHealth = "fleet/machine-health"
	ActionAssign        = "fleet/assign"
	ActionProvision     = "fleet/provision"
)

// ActionAll is the wildcard pattern matching all fleet operations.
const ActionAll = "fleet/**"
