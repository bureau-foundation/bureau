// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package stewardship

import (
	"fmt"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema/ticket"
)

const (
	// StewardshipContentVersion is the current schema version for
	// StewardshipContent events. Increment when adding fields that
	// existing code must not silently drop during read-modify-write.
	StewardshipContentVersion = 1
)

// StewardshipContent is the content of an m.bureau.stewardship state
// event. Each stewardship declaration maps resource patterns to
// responsible principals with tiered review escalation. The ticket
// service resolves these declarations against tickets' affects fields
// to auto-configure review gates or queue notifications.
//
// State key: resource identifier (e.g., "fleet/gpu", "workspace/lib/schema")
// Room: any room the ticket service is a member of
type StewardshipContent struct {
	// Version is the schema version (see StewardshipContentVersion).
	// Code that modifies this event must call CanModify() first; if
	// Version exceeds StewardshipContentVersion, the modification is
	// refused to prevent silent field loss.
	Version int `json:"version"`

	// ResourcePatterns is a list of glob patterns defining the resource
	// scope. A ticket's affects entries are matched against these
	// patterns using the same hierarchical /‐separated glob syntax as
	// principal localparts and authorization actions. Multiple patterns
	// allow a single declaration to cover related resources.
	ResourcePatterns []string `json:"resource_patterns"`

	// Description is a human-readable explanation of what this resource
	// is and why it has stewards. Agents reading stewardship policies
	// can use this to understand the governance context.
	Description string `json:"description,omitempty"`

	// GateTypes lists ticket types that trigger a review gate when a
	// ticket affects this resource. When a ticket with a matching type
	// declares affects entries that match any pattern in
	// ResourcePatterns, the ticket service auto-configures a review
	// gate from the stewardship tiers.
	GateTypes []ticket.TicketType `json:"gate_types,omitempty"`

	// NotifyTypes lists ticket types that trigger steward notification
	// without a gate. Stewards are informed but not required to approve.
	NotifyTypes []ticket.TicketType `json:"notify_types,omitempty"`

	// Tiers is an ordered list of reviewer tiers. Each tier specifies
	// which principals are involved, the approval threshold, and when
	// the tier activates. See StewardshipTier for the tier model.
	Tiers []StewardshipTier `json:"tiers"`

	// OverlapPolicy controls how this declaration composes with other
	// declarations matching the same ticket. "independent" (default)
	// produces a separate review gate that must be satisfied regardless
	// of other declarations. "cooperative" pools reviewers with other
	// cooperative declarations into a single merged gate.
	OverlapPolicy string `json:"overlap_policy,omitempty"`

	// DigestInterval is the duration between notification digests
	// (e.g., "1h", "4h", "24h"). Zero or absent means immediate
	// notification (no batching). Uses Go time.ParseDuration format,
	// consistent with TicketGate.Interval and TicketGate.Duration.
	DigestInterval string `json:"digest_interval,omitempty"`
}

// Validate checks that all required fields are present and well-formed.
// Returns an error describing the first invalid field found, or nil if
// the content is valid.
func (s *StewardshipContent) Validate() error {
	if s.Version < 1 {
		return fmt.Errorf("stewardship: version must be >= 1, got %d", s.Version)
	}
	if len(s.ResourcePatterns) == 0 {
		return fmt.Errorf("stewardship: resource_patterns is required and must be non-empty")
	}
	for i, pattern := range s.ResourcePatterns {
		if pattern == "" {
			return fmt.Errorf("stewardship: resource_patterns[%d]: pattern cannot be empty", i)
		}
	}
	if len(s.Tiers) == 0 {
		return fmt.Errorf("stewardship: tiers is required and must be non-empty")
	}
	for i := range s.Tiers {
		if err := s.Tiers[i].Validate(); err != nil {
			return fmt.Errorf("stewardship: tiers[%d]: %w", i, err)
		}
	}
	for i, typeName := range s.GateTypes {
		if !typeName.IsKnown() {
			return fmt.Errorf("stewardship: gate_types[%d]: unknown ticket type %q", i, typeName)
		}
	}
	for i, typeName := range s.NotifyTypes {
		if !typeName.IsKnown() {
			return fmt.Errorf("stewardship: notify_types[%d]: unknown ticket type %q", i, typeName)
		}
	}
	if err := validateNoOverlap(s.GateTypes, s.NotifyTypes); err != nil {
		return err
	}
	switch s.OverlapPolicy {
	case "", "independent", "cooperative":
		// Valid. Empty defaults to "independent" at runtime.
	default:
		return fmt.Errorf("stewardship: unknown overlap_policy %q (must be \"independent\" or \"cooperative\")", s.OverlapPolicy)
	}
	if s.DigestInterval != "" {
		duration, err := time.ParseDuration(s.DigestInterval)
		if err != nil {
			return fmt.Errorf("stewardship: invalid digest_interval %q: %w", s.DigestInterval, err)
		}
		if duration < 0 {
			return fmt.Errorf("stewardship: digest_interval must be non-negative, got %s", s.DigestInterval)
		}
	}
	return nil
}

// CanModify checks whether this code version can safely perform a
// read-modify-write cycle on this event. Returns nil if safe, or an
// error explaining why modification would risk data loss.
//
// If the event's Version exceeds StewardshipContentVersion, this code
// does not understand all fields in the event. Marshaling the modified
// struct back to JSON would silently drop the unknown fields. The
// caller must either upgrade the service or refuse the operation.
//
// Read-only access does not require CanModify — unknown fields are
// harmlessly ignored.
func (s *StewardshipContent) CanModify() error {
	if s.Version > StewardshipContentVersion {
		return fmt.Errorf(
			"stewardship version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade the service before modifying this event",
			s.Version, StewardshipContentVersion,
		)
	}
	return nil
}

// StewardshipTier defines a reviewer group within a stewardship
// declaration. Tiers are ordered: Tier 0 is tried first, higher tiers
// are activated according to their escalation policy.
type StewardshipTier struct {
	// Principals is a list of user ID patterns identifying the
	// reviewers in this tier. Each pattern uses the format
	// "localpart_pattern:server_pattern" — the same format as
	// authorization grant targets. Bare localpart patterns (without
	// a server component) are rejected: in a federated system, every
	// identity reference must be explicit about which servers it
	// applies to.
	//
	// Examples:
	//   "iree/amdgpu/pm:bureau.local"        — exact match
	//   "bureau/dev/*/tpm:bureau.local"       — wildcard match
	//   "bureau/fleet/**/agent/**:*"          — recursive wildcard
	Principals []string `json:"principals"`

	// Threshold is the number of approvals required from this tier.
	// Nil or absent means "all principals must approve" (backward
	// compatible with review gates that have no threshold). A non-nil
	// value of N means "at least N principals must approve."
	Threshold *int `json:"threshold,omitempty"`

	// Escalation controls when this tier's reviewers are notified.
	// "immediate" (default) means the tier is notified when the review
	// gate is created. "last_pending" means the tier is notified only
	// when it is the last unsatisfied tier — all earlier tiers have
	// met their thresholds.
	Escalation string `json:"escalation,omitempty"`
}

// Validate checks that the tier has valid principals, threshold, and
// escalation.
func (t *StewardshipTier) Validate() error {
	if len(t.Principals) == 0 {
		return fmt.Errorf("principals is required and must be non-empty")
	}
	for i, pattern := range t.Principals {
		if pattern == "" {
			return fmt.Errorf("principals[%d]: pattern cannot be empty", i)
		}
		if !strings.Contains(pattern, ":") {
			return fmt.Errorf(
				"principals[%d]: pattern %q is a bare localpart; "+
					"use \"localpart_pattern:server_pattern\" format "+
					"(e.g., %q)",
				i, pattern, pattern+":bureau.local",
			)
		}
	}
	if t.Threshold != nil && *t.Threshold < 1 {
		return fmt.Errorf(
			"threshold must be >= 1 when set (got %d); "+
				"omit the field to require all principals to approve",
			*t.Threshold,
		)
	}
	switch t.Escalation {
	case "", "immediate", "last_pending":
		// Valid. Empty defaults to "immediate" at runtime.
	default:
		return fmt.Errorf("unknown escalation %q (must be \"immediate\" or \"last_pending\")", t.Escalation)
	}
	return nil
}

// validateNoOverlap checks that no ticket type appears in both
// gate_types and notify_types. A ticket type should trigger exactly one
// involvement mode per stewardship declaration.
func validateNoOverlap(gateTypes, notifyTypes []ticket.TicketType) error {
	if len(gateTypes) == 0 || len(notifyTypes) == 0 {
		return nil
	}
	gateSet := make(map[ticket.TicketType]bool, len(gateTypes))
	for _, typeName := range gateTypes {
		gateSet[typeName] = true
	}
	for _, typeName := range notifyTypes {
		if gateSet[typeName] {
			return fmt.Errorf(
				"stewardship: ticket type %q appears in both gate_types and notify_types; "+
					"a type must trigger either a gate or a notification, not both",
				typeName,
			)
		}
	}
	return nil
}
