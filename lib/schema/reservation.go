// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"errors"
	"fmt"
	"time"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// Reservation and relay event type constants. These events live in ops
// rooms — operational coordination spaces that sit alongside config
// rooms. Ops rooms allow services to coordinate without accessing
// credentials.
const (
	// EventTypeReservation records an active reservation grant on a
	// resource. Published by the resource owner (fleet controller,
	// budget controller) when a reservation is granted. Cleared when
	// the reservation is released.
	//
	// State key: holder principal user ID
	// Room: resource ops room (e.g., #.../machine/<name>/ops)
	EventTypeReservation ref.EventType = "m.bureau.reservation"

	// EventTypeRelayLink tracks the bidirectional connection between
	// a relay ticket in an ops room and its origin ticket. Published
	// by the ticket service alongside the relay ticket. The resource
	// owner reads this to understand provenance; the ticket service
	// reads it to cascade closure.
	//
	// State key: relay ticket ID
	// Room: resource ops room
	EventTypeRelayLink ref.EventType = "m.bureau.relay_link"

	// EventTypeRelayPolicy declares which rooms may relay tickets
	// into this ops room and what information crosses the boundary.
	// Admin-only (PL 100): controls the authorization boundary
	// between origin rooms and ops rooms. A malicious rewrite
	// could open the ops room to unauthorized relay sources or
	// suppress outbound filtering.
	//
	// State key: "" (singleton per ops room)
	// Room: any ops room
	EventTypeRelayPolicy ref.EventType = "m.bureau.relay_policy"

	// EventTypeMachineDrain instructs services on a machine to stop
	// accepting new work and drain in-flight operations. Published
	// by the fleet controller when granting an exclusive machine
	// reservation. Cleared when the reservation is released and
	// services should resume.
	//
	// State key: "" (singleton per ops room)
	// Room: machine ops room
	EventTypeMachineDrain ref.EventType = "m.bureau.machine_drain"

	// EventTypeDrainStatus reports a service's or daemon's drain
	// progress in response to a machine drain event. Published by
	// each service and the machine daemon after observing a drain
	// event. The fleet controller watches these to determine when
	// all services have quiesced before granting the reservation.
	//
	// State key: reporting entity's full user ID
	// Room: machine ops room
	EventTypeDrainStatus ref.EventType = "m.bureau.drain_status"
)

// ResourceType identifies the category of a reservable resource.
type ResourceType string

const (
	// ResourceMachine is a physical or virtual machine.
	ResourceMachine ResourceType = "machine"

	// ResourceQuota is an API token budget or cost allocation.
	ResourceQuota ResourceType = "quota"

	// ResourceHuman is a human's time and attention.
	ResourceHuman ResourceType = "human"

	// ResourceService is a specific service instance or capacity.
	ResourceService ResourceType = "service"
)

// IsKnown reports whether this is a resource type value understood by
// this version of the code.
func (t ResourceType) IsKnown() bool {
	switch t {
	case ResourceMachine, ResourceQuota, ResourceHuman, ResourceService:
		return true
	}
	return false
}

// ResourceRef identifies a reservable resource.
type ResourceRef struct {
	// Type is the resource category. Determines which type-specific
	// claim fields are expected and which resource owner handles the
	// reservation.
	Type ResourceType `json:"type"`

	// Target identifies the specific resource within the type.
	// For machines: machine localpart or pool name.
	// For quotas: budget name.
	// For humans: Matrix user ID.
	// For services: service localpart.
	Target string `json:"target"`
}

// Validate checks that both type and target are present and well-formed.
func (r *ResourceRef) Validate() error {
	if r.Type == "" {
		return errors.New("resource ref: type is required")
	}
	if !r.Type.IsKnown() {
		return fmt.Errorf("resource ref: unknown type %q", r.Type)
	}
	if r.Target == "" {
		return errors.New("resource ref: target is required")
	}
	return nil
}

// ReservationMode describes how a resource is claimed.
type ReservationMode string

const (
	// ModeExclusive drains other consumers and grants sole access.
	// For machines: services are quiesced. For quotas: the full
	// budget is allocated to one consumer.
	ModeExclusive ReservationMode = "exclusive"

	// ModeInclusive brings the resource online without displacing
	// other consumers. For machines: wake an on-demand instance.
	// For quotas: allocate from a shared pool.
	ModeInclusive ReservationMode = "inclusive"

	// ModePartial claims a fraction of the resource. Other
	// consumers retain their share. For machines: specific CPU
	// cores, NUMA nodes, or GPU fraction.
	ModePartial ReservationMode = "partial"
)

// IsKnown reports whether this is a reservation mode value understood
// by this version of the code.
func (m ReservationMode) IsKnown() bool {
	switch m {
	case ModeExclusive, ModeInclusive, ModePartial:
		return true
	}
	return false
}

// ClaimStatus represents the lifecycle state of a resource claim
// within a reservation. The ticket service drives transitions by
// watching ops rooms and mirroring state back to origin tickets.
type ClaimStatus string

const (
	// ClaimPending means the relay ticket has not yet been created
	// or is awaiting the resource owner's attention.
	ClaimPending ClaimStatus = "pending"

	// ClaimQueued means the resource owner acknowledged the request
	// but the resource is held by another reservation. The claim
	// will be granted when the current holder releases.
	ClaimQueued ClaimStatus = "queued"

	// ClaimApproved means the resource owner accepted and
	// preparation is in progress (draining services, waking
	// machine, allocating budget). The resource is not yet
	// available.
	ClaimApproved ClaimStatus = "approved"

	// ClaimGranted means the resource is available. The
	// corresponding state_event gate on the origin ticket is
	// satisfied.
	ClaimGranted ClaimStatus = "granted"

	// ClaimDenied means the resource owner rejected the request.
	// The ticket service closes all other relay tickets for this
	// reservation (partial fulfillment is not useful).
	ClaimDenied ClaimStatus = "denied"

	// ClaimReleased means the claim was granted and has now been
	// released through normal completion (pipeline finished,
	// ticket closed).
	ClaimReleased ClaimStatus = "released"

	// ClaimPreempted means the claim was granted but reclaimed by
	// the resource owner (duration exceeded, higher priority
	// request, operator override).
	ClaimPreempted ClaimStatus = "preempted"
)

// IsKnown reports whether this is a claim status value understood by
// this version of the code.
func (s ClaimStatus) IsKnown() bool {
	switch s {
	case ClaimPending, ClaimQueued, ClaimApproved, ClaimGranted,
		ClaimDenied, ClaimReleased, ClaimPreempted:
		return true
	}
	return false
}

// IsTerminal reports whether this claim status is a terminal state
// from which no further transitions occur.
func (s ClaimStatus) IsTerminal() bool {
	switch s {
	case ClaimDenied, ClaimReleased, ClaimPreempted:
		return true
	}
	return false
}

// ReservationGrant is the content of an EventTypeReservation state
// event. Published by the resource owner when a reservation is
// granted. Cleared (empty content) when the reservation is released.
//
// The ticket service watches for this event with a state_event gate
// on the origin ticket: when the gate's content_match finds a
// matching holder, the gate is satisfied.
type ReservationGrant struct {
	// Holder is the principal that holds the reservation.
	Holder ref.Entity `json:"holder"`

	// Resource identifies what was reserved.
	Resource ResourceRef `json:"resource"`

	// Mode is the granted reservation mode.
	Mode ReservationMode `json:"mode"`

	// GrantedAt is the RFC 3339 UTC timestamp when the reservation
	// was granted.
	GrantedAt string `json:"granted_at"`

	// ExpiresAt is the RFC 3339 UTC timestamp when the reservation
	// expires. The resource owner enforces this via a duration
	// watchdog.
	ExpiresAt string `json:"expires_at"`

	// RelayTicket is the ticket ID of the relay ticket in this ops
	// room that initiated the reservation.
	RelayTicket string `json:"relay_ticket"`
}

// Validate checks that all required fields are present and well-formed.
func (g *ReservationGrant) Validate() error {
	if g.Holder.IsZero() {
		return errors.New("reservation grant: holder is required")
	}
	if err := g.Resource.Validate(); err != nil {
		return fmt.Errorf("reservation grant: %w", err)
	}
	if g.Mode == "" {
		return errors.New("reservation grant: mode is required")
	}
	if !g.Mode.IsKnown() {
		return fmt.Errorf("reservation grant: unknown mode %q", g.Mode)
	}
	if g.GrantedAt == "" {
		return errors.New("reservation grant: granted_at is required")
	}
	if _, err := time.Parse(time.RFC3339, g.GrantedAt); err != nil {
		return fmt.Errorf("reservation grant: granted_at must be RFC 3339: %w", err)
	}
	if g.ExpiresAt == "" {
		return errors.New("reservation grant: expires_at is required")
	}
	if _, err := time.Parse(time.RFC3339, g.ExpiresAt); err != nil {
		return fmt.Errorf("reservation grant: expires_at must be RFC 3339: %w", err)
	}
	if g.RelayTicket == "" {
		return errors.New("reservation grant: relay_ticket is required")
	}
	return nil
}

// RelayLink tracks the connection between a relay ticket in an ops
// room and its origin ticket. Published by the ticket service
// alongside the relay ticket for lifecycle tracking.
type RelayLink struct {
	// OriginRoom is the room ID containing the original ticket.
	OriginRoom ref.RoomID `json:"origin_room"`

	// OriginTicket is the ticket ID in the origin room.
	OriginTicket string `json:"origin_ticket"`

	// Requester is the principal that created the original ticket.
	Requester ref.Entity `json:"requester"`
}

// Validate checks that all required fields are present.
func (l *RelayLink) Validate() error {
	if l.OriginRoom.IsZero() {
		return errors.New("relay link: origin_room is required")
	}
	if l.OriginTicket == "" {
		return errors.New("relay link: origin_ticket is required")
	}
	if l.Requester.IsZero() {
		return errors.New("relay link: requester is required")
	}
	return nil
}

// RelayPolicy controls which rooms can create relay tickets in this
// ops room and what information crosses the boundary. Published by
// the admin as EventTypeRelayPolicy.
type RelayPolicy struct {
	// Sources declares which rooms may relay tickets here. Empty
	// means no relay is accepted (default-deny).
	Sources []RelaySource `json:"sources"`

	// AllowedTypes restricts which ticket types can be relayed.
	// Values are ticket type strings (e.g., "resource_request").
	// Empty means all types are accepted. The ticket service
	// validates these against known ticket types at evaluation
	// time.
	AllowedTypes []string `json:"allowed_types,omitempty"`

	// OutboundFilter controls what information from the relay
	// ticket is mirrored back to the origin ticket. When nil,
	// the ticket service uses default filtering (status,
	// close_reason, and status_reason only).
	OutboundFilter *RelayFilter `json:"outbound_filter,omitempty"`
}

// Validate checks that the policy has at least one source and all
// sources are well-formed.
func (p *RelayPolicy) Validate() error {
	if len(p.Sources) == 0 {
		return errors.New("relay policy: at least one source is required")
	}
	for i := range p.Sources {
		if err := p.Sources[i].Validate(); err != nil {
			return fmt.Errorf("relay policy: sources[%d]: %w", i, err)
		}
	}
	return nil
}

// RelaySourceMatch identifies the authorization strategy for a relay
// source.
type RelaySourceMatch string

const (
	// RelayMatchFleetMember authorizes any room in the named fleet.
	RelayMatchFleetMember RelaySourceMatch = "fleet_member"

	// RelayMatchRoom authorizes a specific room by ID.
	RelayMatchRoom RelaySourceMatch = "room"

	// RelayMatchNamespace authorizes any room in the named
	// namespace.
	RelayMatchNamespace RelaySourceMatch = "namespace"
)

// IsKnown reports whether this is a recognized relay source match
// strategy.
func (m RelaySourceMatch) IsKnown() bool {
	switch m {
	case RelayMatchFleetMember, RelayMatchRoom, RelayMatchNamespace:
		return true
	}
	return false
}

// RelaySource identifies rooms authorized to relay tickets into an
// ops room. The Match field determines the authorization strategy;
// the corresponding detail field (Fleet, Room, or Namespace) must be
// set.
type RelaySource struct {
	// Match is the authorization strategy.
	Match RelaySourceMatch `json:"match"`

	// Fleet is the fleet alias (when Match is "fleet_member").
	// Any room belonging to this fleet may relay.
	Fleet string `json:"fleet,omitempty"`

	// Room is the room ID (when Match is "room"). Only this
	// specific room may relay.
	Room ref.RoomID `json:"room,omitempty"`

	// Namespace is the namespace alias (when Match is
	// "namespace"). Any room in this namespace may relay.
	Namespace string `json:"namespace,omitempty"`
}

// Validate checks that the source has a valid match strategy and the
// corresponding detail field is set.
func (s *RelaySource) Validate() error {
	switch s.Match {
	case RelayMatchFleetMember:
		if s.Fleet == "" {
			return errors.New("relay source: fleet is required when match is \"fleet_member\"")
		}
	case RelayMatchRoom:
		if s.Room.IsZero() {
			return errors.New("relay source: room is required when match is \"room\"")
		}
	case RelayMatchNamespace:
		if s.Namespace == "" {
			return errors.New("relay source: namespace is required when match is \"namespace\"")
		}
	case "":
		return errors.New("relay source: match is required")
	default:
		return fmt.Errorf("relay source: unknown match %q", s.Match)
	}
	return nil
}

// RelayFilter specifies which fields cross the relay boundary when
// mirroring ops room state back to origin tickets.
type RelayFilter struct {
	// Include lists fields mirrored back to the origin ticket.
	// When empty with a non-nil filter, all fields are allowed
	// (minus Exclude). The nil filter case is handled by Allows:
	// only "status", "close_reason", and "status_reason" cross
	// by default.
	Include []string `json:"include,omitempty"`

	// Exclude lists fields that stay in the ops room and are not
	// relayed. Takes precedence over Include.
	Exclude []string `json:"exclude,omitempty"`
}

// Allows reports whether the named field is permitted to cross the
// relay boundary. Nil-receiver-safe: a nil filter uses the default
// policy where only "status", "close_reason", and "status_reason"
// are allowed.
//
// When the filter is non-nil:
//   - Exclude takes precedence over Include.
//   - An empty Include list means all fields are allowed (minus Exclude).
//   - A populated Include list means only those fields are allowed
//     (minus Exclude).
func (f *RelayFilter) Allows(field string) bool {
	if f == nil {
		return field == "status" || field == "close_reason" || field == "status_reason"
	}
	for _, excluded := range f.Exclude {
		if excluded == field {
			return false
		}
	}
	if len(f.Include) == 0 {
		return true
	}
	for _, included := range f.Include {
		if included == field {
			return true
		}
	}
	return false
}

// MachineDrainContent is the content of an EventTypeMachineDrain
// state event. Published by the fleet controller when granting an
// exclusive machine reservation. Services watch this event and
// gracefully drain when listed. Cleared (empty content) when the
// reservation is released and services should resume.
type MachineDrainContent struct {
	// Services lists services to gracefully drain, identified by
	// their full Matrix user IDs. Services not listed continue
	// running. Empty means drain all fleet-managed services on the
	// machine.
	Services []ref.UserID `json:"services,omitempty"`

	// ReservationHolder is the principal that holds the reservation
	// causing the drain.
	ReservationHolder ref.Entity `json:"reservation_holder"`

	// RequestedAt is the RFC 3339 UTC timestamp when the drain was
	// requested.
	RequestedAt string `json:"requested_at"`
}

// Validate checks that required fields are present and well-formed.
func (d *MachineDrainContent) Validate() error {
	if d.ReservationHolder.IsZero() {
		return errors.New("machine drain: reservation_holder is required")
	}
	if d.RequestedAt == "" {
		return errors.New("machine drain: requested_at is required")
	}
	if _, err := time.Parse(time.RFC3339, d.RequestedAt); err != nil {
		return fmt.Errorf("machine drain: requested_at must be RFC 3339: %w", err)
	}
	return nil
}

// DrainStatusContent is the content of an EventTypeDrainStatus state
// event. Published by services and the machine daemon to report drain
// progress. The fleet controller aggregates these to determine when
// all services have quiesced before granting an exclusive reservation.
//
// A service publishes this with Acknowledged=true when it sees a drain
// event, updates InFlight as operations complete, and sets DrainedAt
// when InFlight reaches zero. When the drain is cleared (reservation
// released), services publish empty content to clear the state event.
type DrainStatusContent struct {
	// Acknowledged is true when the service has seen and accepted
	// the drain request. A false value means the service has not
	// yet processed the drain event.
	Acknowledged bool `json:"acknowledged"`

	// InFlight is the count of operations still in progress. For
	// the daemon this is running sandboxes; for the ticket service
	// this is active relay tickets in the ops room.
	InFlight int `json:"in_flight"`

	// DrainedAt is the RFC 3339 UTC timestamp when InFlight
	// reached zero. Empty while operations are still in progress.
	DrainedAt string `json:"drained_at,omitempty"`
}

// Validate checks that the drain status content is internally
// consistent. When Acknowledged is true and InFlight is zero,
// DrainedAt must be present and valid RFC 3339.
func (d *DrainStatusContent) Validate() error {
	if d.InFlight < 0 {
		return fmt.Errorf("drain status: in_flight must be non-negative, got %d", d.InFlight)
	}
	if d.Acknowledged && d.InFlight == 0 {
		if d.DrainedAt == "" {
			return errors.New("drain status: drained_at is required when acknowledged and in_flight is 0")
		}
		if _, err := time.Parse(time.RFC3339, d.DrainedAt); err != nil {
			return fmt.Errorf("drain status: drained_at must be RFC 3339: %w", err)
		}
	}
	if d.DrainedAt != "" && d.InFlight > 0 {
		return fmt.Errorf("drain status: drained_at must be empty when in_flight is %d", d.InFlight)
	}
	return nil
}

// OpsRoomPowerLevels returns the power level content for resource ops
// rooms. Ops rooms use per-event-type PL overrides to give each member
// write access only to the events it owns:
//
//   - m.bureau.ticket, m.bureau.relay_link: PL 25 (ticket service)
//   - m.bureau.reservation, m.bureau.machine_drain: PL 50 (resource owner)
//   - m.bureau.relay_policy: PL 100 (admin only)
//   - events_default: 100 (deny by default)
//
// The resource owner (fleet controller at PL 50) publishes reservation
// grants and drain requests. The ticket service (PL 25) creates relay
// tickets and lifecycle links. The admin (PL 100) configures relay
// policy.
func OpsRoomPowerLevels(adminUserID ref.UserID) map[string]any {
	events := AdminProtectedEvents()

	// Ticket service and drain status operations (PL 25). The ticket
	// service creates relay tickets, enables ticket management, and
	// publishes relay links that track origin↔ops room ticket pairs.
	// Drain status is published by both the ticket service (PL 25)
	// and the machine daemon (PL 50) — PL 25 ensures both can write.
	for _, eventType := range []ref.EventType{
		EventTypeTicket,
		EventTypeTicketConfig,
		EventTypeRelayLink,
		EventTypeDrainStatus,
	} {
		events[eventType] = 25
	}

	// Resource owner operations (PL 50). The fleet controller
	// publishes reservation grants and drain requests. The machine
	// daemon reports machine state. Both run at PL 50.
	for _, eventType := range []ref.EventType{
		EventTypeReservation,
		EventTypeMachineDrain,
	} {
		events[eventType] = 50
	}

	// Admin-only configuration.
	events[EventTypeRelayPolicy] = 100

	// Timeline messages at PL 0 for operational audit trail
	// (fleet controller status updates, ticket service notifications,
	// service readiness announcements).
	events[MatrixEventTypeMessage] = 0
	events[EventTypeServiceReady] = 0

	return roomPowerLevels(powerLevelSpec{
		users: map[string]any{
			adminUserID.String(): 100,
		},
		events:        events,
		eventsDefault: 100,
		stateDefault:  100,
		invite:        100,
	})
}
