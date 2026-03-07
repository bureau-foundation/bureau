// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"errors"
	"fmt"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// ReservationContent carries type-specific content for resource
// request tickets (TicketContent.Type == "resource_request"). Must be
// set when Type is "resource_request" and must be nil for all other
// types. Added in TicketContentVersion 5.
//
// A reservation is a time-bounded claim on one or more scarce
// resources (machines, API budgets, human time). The ticket service
// relays each claim to the appropriate ops room and mirrors status
// back to the workspace ticket for observability.
type ReservationContent struct {
	// Claims is the set of resources being reserved. Each claim is
	// relayed independently to the appropriate ops room. All must
	// be granted for the reservation to be fulfilled.
	//
	// Single-resource case: one entry (the common case).
	// Multi-resource case: N entries, one per resource needed. The
	// ticket service creates N relay tickets and N state_event
	// gates. The ticket is ready only when all claims are granted.
	Claims []ResourceClaim `json:"claims"`

	// MaxDuration is the maximum time the reservation is valid
	// (Go duration string, e.g., "2h", "30m"). Applies to the
	// overall reservation, not per-claim. The resource owner for
	// each claim enforces duration on its own grant.
	MaxDuration string `json:"max_duration"`
}

// Validate checks that the reservation content is well-formed.
func (r *ReservationContent) Validate() error {
	if len(r.Claims) == 0 {
		return errors.New("reservation: at least one claim is required")
	}
	for i := range r.Claims {
		if err := r.Claims[i].Validate(); err != nil {
			return fmt.Errorf("reservation: claims[%d]: %w", i, err)
		}
	}
	if r.MaxDuration == "" {
		return errors.New("reservation: max_duration is required")
	}
	duration, err := time.ParseDuration(r.MaxDuration)
	if err != nil {
		return fmt.Errorf("reservation: invalid max_duration: %w", err)
	}
	if duration <= 0 {
		return errors.New("reservation: max_duration must be positive")
	}
	return nil
}

// ResourceClaim is a request for a single resource within a
// reservation. The ticket service relays each claim to the
// appropriate ops room and updates the claim's status as the resource
// owner acts on it.
type ResourceClaim struct {
	// Resource identifies what is being reserved.
	Resource schema.ResourceRef `json:"resource"`

	// Mode is how the resource is claimed.
	Mode schema.ReservationMode `json:"mode"`

	// Status is the current state of this claim's lifecycle. Set
	// and updated by the ticket service as it processes the relay.
	// Agents read this to understand reservation progress without
	// accessing ops rooms.
	Status schema.ClaimStatus `json:"status"`

	// StatusAt is the RFC 3339 UTC timestamp of the last status
	// transition.
	StatusAt string `json:"status_at,omitempty"`

	// StatusReason provides context for the current status.
	// For queued: queue position ("Position 2 of 3").
	// For denied: denial reason ("Budget exhausted").
	// For preempted: preemption reason ("Duration exceeded").
	// Filtered through the relay policy's outbound filter.
	StatusReason string `json:"status_reason,omitempty"`

	// Resource-type-specific parameters. Exactly one must be set,
	// matching Resource.Type. The validator rejects mismatches.

	// Machine carries machine-specific reservation parameters.
	Machine *MachineReservation `json:"machine,omitempty"`

	// Quota carries budget/quota-specific reservation parameters.
	Quota *QuotaReservation `json:"quota,omitempty"`
}

// Validate checks that the claim has valid resource, mode, status,
// and that exactly the right type-specific parameters are set.
func (c *ResourceClaim) Validate() error {
	if err := c.Resource.Validate(); err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if c.Mode == "" {
		return errors.New("claim: mode is required")
	}
	if !c.Mode.IsKnown() {
		return fmt.Errorf("claim: unknown mode %q", c.Mode)
	}
	if c.Status == "" {
		return errors.New("claim: status is required")
	}
	if !c.Status.IsKnown() {
		return fmt.Errorf("claim: unknown status %q", c.Status)
	}
	if c.StatusAt != "" {
		if _, err := time.Parse(time.RFC3339, c.StatusAt); err != nil {
			return fmt.Errorf("claim: status_at must be RFC 3339: %w", err)
		}
	}

	// Validate type-specific parameter consistency.
	switch c.Resource.Type {
	case schema.ResourceMachine:
		if c.Quota != nil {
			return errors.New("claim: quota must be nil for machine resources")
		}
	case schema.ResourceQuota:
		if c.Machine != nil {
			return errors.New("claim: machine must be nil for quota resources")
		}
	case schema.ResourceHuman, schema.ResourceService:
		if c.Machine != nil {
			return fmt.Errorf("claim: machine must be nil for %s resources", c.Resource.Type)
		}
		if c.Quota != nil {
			return fmt.Errorf("claim: quota must be nil for %s resources", c.Resource.Type)
		}
	}

	if c.Machine != nil {
		if err := c.Machine.Validate(c.Mode); err != nil {
			return fmt.Errorf("claim: %w", err)
		}
	}
	if c.Quota != nil {
		if err := c.Quota.Validate(); err != nil {
			return fmt.Errorf("claim: %w", err)
		}
	}
	return nil
}

// MachineReservation carries machine-specific parameters for a
// resource claim. The fleet controller reads these to determine what
// drain/wake actions are needed.
type MachineReservation struct {
	// DrainServices lists service types to gracefully drain before
	// granting access. Only meaningful for exclusive mode. Services
	// not listed continue running. Empty means drain all
	// fleet-managed services on the machine.
	DrainServices []string `json:"drain_services,omitempty"`

	// Resources specifies partial resource claims. Only meaningful
	// for partial mode.
	Resources *PartialMachineResources `json:"resources,omitempty"`
}

// Validate checks machine reservation consistency with the claim
// mode.
func (m *MachineReservation) Validate(mode schema.ReservationMode) error {
	if mode == schema.ModePartial && m.Resources == nil {
		return errors.New("machine reservation: resources is required for partial mode")
	}
	if mode != schema.ModePartial && m.Resources != nil {
		return fmt.Errorf("machine reservation: resources must be nil for %s mode", mode)
	}
	if m.Resources != nil {
		if err := m.Resources.Validate(); err != nil {
			return fmt.Errorf("machine reservation: %w", err)
		}
	}
	return nil
}

// PartialMachineResources specifies fractional machine resource
// claims for partial-mode reservations. The fleet controller
// translates these into cgroup configurations and GPU shims.
type PartialMachineResources struct {
	// CPUCores is the number of CPU cores to reserve.
	CPUCores int `json:"cpu_cores,omitempty"`

	// NUMANodes is the list of NUMA node indices to reserve.
	NUMANodes []int `json:"numa_nodes,omitempty"`

	// GPUFraction is the fraction of GPU compute to reserve
	// (0.0-1.0). The fleet controller configures rate-limiting
	// for other consumers.
	GPUFraction float64 `json:"gpu_fraction,omitempty"`
}

// Validate checks that at least one partial resource constraint is
// specified and values are in valid ranges.
func (p *PartialMachineResources) Validate() error {
	if p.CPUCores == 0 && len(p.NUMANodes) == 0 && p.GPUFraction == 0 {
		return errors.New("partial machine resources: at least one resource constraint is required")
	}
	if p.CPUCores < 0 {
		return fmt.Errorf("partial machine resources: cpu_cores must be >= 0, got %d", p.CPUCores)
	}
	for i, node := range p.NUMANodes {
		if node < 0 {
			return fmt.Errorf("partial machine resources: numa_nodes[%d] must be >= 0, got %d", i, node)
		}
	}
	if p.GPUFraction < 0 || p.GPUFraction > 1 {
		return fmt.Errorf("partial machine resources: gpu_fraction must be in [0, 1], got %f", p.GPUFraction)
	}
	return nil
}

// QuotaReservation carries budget/quota-specific parameters for a
// resource claim. The budget controller reads these to determine
// allocation.
type QuotaReservation struct {
	// TokenBudget is the number of API tokens to allocate.
	TokenBudget int64 `json:"token_budget,omitempty"`

	// CostLimit is the maximum cost in the budget's currency (e.g.,
	// "50.00"). The budget controller enforces this via the proxy's
	// rate limiting.
	CostLimit string `json:"cost_limit,omitempty"`
}

// Validate checks that at least one budget constraint is specified.
func (q *QuotaReservation) Validate() error {
	if q.TokenBudget == 0 && q.CostLimit == "" {
		return errors.New("quota reservation: at least one of token_budget or cost_limit is required")
	}
	if q.TokenBudget < 0 {
		return fmt.Errorf("quota reservation: token_budget must be >= 0, got %d", q.TokenBudget)
	}
	return nil
}
