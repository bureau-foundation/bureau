// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/cron"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

const (
	// TicketContentVersion is the current schema version for
	// TicketContent events. Increment when adding fields that
	// existing code must not silently drop during read-modify-write.
	TicketContentVersion = 3

	// TicketConfigVersion is the current schema version for
	// TicketConfigContent events.
	TicketConfigVersion = 1
)

// MinTimerRecurrence is the minimum allowed recurrence period for
// recurring timer gates. Anything more frequent should be a service
// loop, not a scheduled ticket.
const MinTimerRecurrence = 30 * time.Second

// TicketContent is the content of an EventTypeTicket state event. Each
// ticket is a work item tracked in a room. The ticket service maintains
// an indexed cache of these events for fast queries via its unix socket
// API, and writes mutations back to Matrix as state event PUTs.
//
// Multiple ticket service instances in a fleet may operate on overlapping
// rooms. The Version field and CanModify guard prevent silent data loss
// during rolling upgrades where different instances run different code
// versions. See CanModify for details.
//
// State key: ticket ID (e.g., "tkt-a3f9")
// Room: any room with ticket management enabled
type TicketContent struct {
	// Version is the schema version (see TicketContentVersion).
	// Code that modifies this event must call CanModify() first; if
	// Version exceeds TicketContentVersion, the modification is
	// refused to prevent silent field loss. Readers may process any
	// version (unknown fields are harmlessly ignored by Go's JSON
	// unmarshaler).
	Version int `json:"version"`

	// Title is a short summary of the work item.
	Title string `json:"title"`

	// Body is the full description, supporting markdown.
	Body string `json:"body,omitempty"`

	// Status is the lifecycle state: "open", "in_progress",
	// "review", "blocked", "closed". The ticket service computes
	// derived readiness from status + dependency graph + gate
	// satisfaction, but "blocked" is also a valid explicit status
	// for agents to signal blockage on things not tracked as ticket
	// dependencies.
	//
	// Contention detection: the ticket service rejects transitions
	// to "in_progress" when the ticket is already "in_progress"
	// (and "review" when already "review"), returning a conflict
	// error with the current assignee. This forces agents to claim
	// work atomically (open → in_progress) before beginning any
	// planning or implementation. Assignee must be set in the same
	// mutation that transitions to "in_progress".
	//
	// Review: tickets in "review" are waiting for reviewer feedback.
	// The ticket must have a non-nil Review field with at least one
	// reviewer to enter this status. The assignee (author) is
	// preserved from in_progress → review and review → in_progress
	// transitions. See TicketReview for the review model.
	Status string `json:"status"`

	// Priority is 0-4: 0=critical, 1=high, 2=medium, 3=low,
	// 4=backlog.
	Priority int `json:"priority"`

	// Type categorizes the work: "task", "bug", "feature",
	// "epic", "chore", "docs", "question", or "pipeline".
	// Pipeline tickets represent pipeline executions and carry
	// type-specific content in the Pipeline field.
	Type string `json:"type"`

	// Labels are free-form tags for filtering and grouping.
	Labels []string `json:"labels,omitempty"`

	// Assignee is the Matrix user ID of the principal working
	// on this ticket (e.g., "@iree/amdgpu/pm:bureau.local").
	// Single assignee: Bureau agents are principals with unique
	// identities, one agent works one ticket. Multiple people
	// on something means sub-tickets (parent-child).
	Assignee ref.UserID `json:"assignee,omitempty"`

	// Parent is the ticket ID of the parent work item (e.g., an
	// epic). Enables hierarchical breakdown: the ticket service
	// computes children sets from the reverse mapping for progress
	// tracking and cascading operations.
	Parent string `json:"parent,omitempty"`

	// BlockedBy lists ticket IDs (in this room) that must be
	// closed before this ticket is considered ready. The ticket
	// service computes the transitive closure for dependency
	// queries and detects cycles on mutation.
	BlockedBy []string `json:"blocked_by,omitempty"`

	// Gates are async coordination conditions that must all be
	// satisfied before the ticket is considered ready. See
	// TicketGate for the gate evaluation model.
	Gates []TicketGate `json:"gates,omitempty"`

	// Notes are short annotations attached to the ticket by
	// agents, services, or humans. Each note is a self-contained
	// piece of context that travels with the ticket — warnings,
	// references, analysis results. Notes are embedded (not
	// Matrix threads) for single-read completeness: an agent
	// reading a ticket gets all context without a second fetch.
	Notes []TicketNote `json:"notes,omitempty"`

	// Attachments are references to artifacts stored outside the
	// ticket. The ticket stores references, not content.
	Attachments []TicketAttachment `json:"attachments,omitempty"`

	// CreatedBy is the Matrix user ID of the ticket creator.
	CreatedBy ref.UserID `json:"created_by"`

	// CreatedAt is an ISO 8601 timestamp.
	CreatedAt string `json:"created_at"`

	// UpdatedAt is an ISO 8601 timestamp of the last modification.
	UpdatedAt string `json:"updated_at"`

	// ClosedAt is set when status transitions to "closed".
	ClosedAt string `json:"closed_at,omitempty"`

	// CloseReason explains why the ticket was closed.
	CloseReason string `json:"close_reason,omitempty"`

	// ContextID is the ctx-* identifier of the context commit that
	// was active when the ticket was last worked on. Links the
	// ticket to a point in an agent's understanding chain, enabling
	// context restoration when resuming work. Set by the agent
	// service or agent wrappers when updating ticket state.
	ContextID string `json:"context_id,omitempty"`

	// Deadline is the target completion time for this ticket
	// (RFC 3339 UTC). The ticket service monitors deadlines and
	// emits warnings when tickets remain open past their deadline.
	// Deadlines are informational — they do not affect readiness.
	Deadline string `json:"deadline,omitempty"`

	// Origin tracks where this ticket came from when imported
	// from an external system (GitHub, beads JSONL, etc.).
	Origin *TicketOrigin `json:"origin,omitempty"`

	// Review carries reviewer tracking for tickets in (or entering)
	// the "review" status. Must be non-nil with at least one reviewer
	// when status is "review". May be pre-populated on tickets in
	// other statuses to set up reviewers before requesting review.
	Review *TicketReview `json:"review,omitempty"`

	// Pipeline carries type-specific content for pipeline
	// execution tickets (Type == "pipeline"). Must be set when
	// Type is "pipeline" and must be nil for all other types.
	Pipeline *PipelineExecutionContent `json:"pipeline,omitempty"`

	// Extra is a documented extension namespace for experimental or
	// preview fields before promotion to top-level schema fields in
	// a version bump. Keys are field names; values are arbitrary
	// JSON. Application code should not read or write Extra directly
	// in production — it exists for forward compatibility and field
	// staging. Extra is NOT a round-trip preservation mechanism for
	// unknown top-level fields; the Version/CanModify guard handles
	// that by refusing modification of events with unrecognized
	// versions.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// Validate checks that all required fields are present and well-formed.
// Returns an error describing the first invalid field found, or nil if
// the content is valid. Recursively validates embedded gates, notes,
// attachments, and origin.
func (t *TicketContent) Validate() error {
	if t.Version < 1 {
		return fmt.Errorf("ticket content: version must be >= 1, got %d", t.Version)
	}
	if t.Title == "" {
		return errors.New("ticket content: title is required")
	}
	switch t.Status {
	case "open", "in_progress", "review", "blocked", "closed":
		// Valid.
	case "":
		return errors.New("ticket content: status is required")
	default:
		return fmt.Errorf("ticket content: unknown status %q", t.Status)
	}
	if t.Priority < 0 || t.Priority > 4 {
		return fmt.Errorf("ticket content: priority must be 0-4, got %d", t.Priority)
	}
	switch t.Type {
	case "task", "bug", "feature", "epic", "chore", "docs", "question", "pipeline":
		// Valid.
	case "":
		return errors.New("ticket content: type is required")
	default:
		return fmt.Errorf("ticket content: unknown type %q", t.Type)
	}
	if t.Type == "pipeline" {
		if t.Pipeline == nil {
			return errors.New("ticket content: pipeline content is required when type is \"pipeline\"")
		}
		if err := t.Pipeline.Validate(); err != nil {
			return fmt.Errorf("ticket content: pipeline: %w", err)
		}
	} else if t.Pipeline != nil {
		return fmt.Errorf("ticket content: pipeline content must be nil when type is %q", t.Type)
	}
	if t.Status == "review" && (t.Review == nil || len(t.Review.Reviewers) == 0) {
		return errors.New("ticket content: review with at least one reviewer is required when status is \"review\"")
	}
	if t.Review != nil {
		if err := t.Review.Validate(); err != nil {
			return fmt.Errorf("ticket content: review: %w", err)
		}
	}
	if t.CreatedBy.IsZero() {
		return errors.New("ticket content: created_by is required")
	}
	if t.CreatedAt == "" {
		return errors.New("ticket content: created_at is required")
	}
	if t.UpdatedAt == "" {
		return errors.New("ticket content: updated_at is required")
	}
	for i := range t.Gates {
		if err := t.Gates[i].Validate(); err != nil {
			return fmt.Errorf("ticket content: gates[%d]: %w", i, err)
		}
	}
	for i := range t.Notes {
		if err := t.Notes[i].Validate(); err != nil {
			return fmt.Errorf("ticket content: notes[%d]: %w", i, err)
		}
	}
	for i := range t.Attachments {
		if err := t.Attachments[i].Validate(); err != nil {
			return fmt.Errorf("ticket content: attachments[%d]: %w", i, err)
		}
	}
	if t.Deadline != "" {
		if _, err := time.Parse(time.RFC3339, t.Deadline); err != nil {
			return fmt.Errorf("ticket content: deadline must be RFC 3339: %w", err)
		}
	}
	if t.Origin != nil {
		if err := t.Origin.Validate(); err != nil {
			return fmt.Errorf("ticket content: origin: %w", err)
		}
	}
	return nil
}

// CanModify checks whether this code version can safely perform a
// read-modify-write cycle on this event. Returns nil if safe, or an
// error explaining why modification would risk data loss.
//
// If the event's Version exceeds TicketContentVersion, this code does
// not understand all fields in the event. Marshaling the modified struct
// back to JSON would silently drop the unknown fields. The caller must
// either upgrade the ticket service or refuse the operation.
//
// Read-only access does not require CanModify — unknown fields are
// harmlessly ignored during display, listing, and search.
func (t *TicketContent) CanModify() error {
	if t.Version > TicketContentVersion {
		return fmt.Errorf(
			"ticket content version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade the ticket service before modifying this event",
			t.Version, TicketContentVersion,
		)
	}
	return nil
}

// TicketGate is an async coordination condition on a ticket. Each gate
// represents something that must happen before the ticket is ready: a
// CI pipeline must pass, a human must approve, a timer must expire,
// another ticket must close, or an arbitrary Matrix state event must
// appear.
//
// The ticket service evaluates gates via its /sync loop — no polling.
// Gate types map to evaluation strategies:
//
//   - "human": No automatic evaluation. Resolved explicitly via
//     bureau ticket gate resolve. The ticket service records who
//     approved and when.
//
//   - "pipeline": Watches for m.bureau.pipeline_result state events
//     where the pipeline ref matches PipelineRef and the conclusion
//     matches Conclusion. Syntactic sugar for a state_event gate.
//
//   - "state_event": Watches for a Matrix state event matching
//     EventType + StateKey + RoomAlias + ContentMatch. This is the
//     general-purpose gate — pipeline and ticket gates are special
//     cases of it.
//
//   - "ticket": Watches for m.bureau.ticket with the given TicketID
//     transitioning to status "closed" in the same room.
//
//   - "timer": Fires when the current time reaches the gate's Target.
//     Supports one-shot delays (Duration or Target alone), absolute
//     scheduling (Target), and recurring schedules (Schedule or
//     Interval). The ticket service evaluates timer gates via a
//     min-heap ordered by Target, not polling. See tickets.md
//     "Scheduling and Recurrence" for the full design.
type TicketGate struct {
	// ID uniquely identifies this gate within the ticket (e.g.,
	// "ci-pass", "lead-approval"). Used for targeted updates.
	ID string `json:"id"`

	// Type determines how the condition is evaluated: "human",
	// "pipeline", "state_event", "ticket", or "timer".
	Type string `json:"type"`

	// Status is "pending" or "satisfied". The ticket service
	// transitions this when the condition is met.
	Status string `json:"status"`

	// Description is a human-readable explanation of what this
	// gate waits for (e.g., "CI pipeline must pass", "24h soak
	// period"). Shown in ticket listings and notifications.
	Description string `json:"description,omitempty"`

	// --- Type-specific condition fields ---

	// PipelineRef identifies the pipeline to watch (type
	// "pipeline"). Matches against the pipeline_ref field in
	// m.bureau.pipeline_result events.
	PipelineRef string `json:"pipeline_ref,omitempty"`

	// Conclusion is the required pipeline result (type
	// "pipeline"). Typically "success". If empty, any completed
	// result satisfies the gate.
	Conclusion string `json:"conclusion,omitempty"`

	// EventType is the Matrix state event type to watch (type
	// "state_event"). Same semantics as StartCondition.EventType.
	EventType ref.EventType `json:"event_type,omitempty"`

	// StateKey is the state key to match (type "state_event").
	StateKey string `json:"state_key,omitempty"`

	// RoomAlias is the room to watch (type "state_event"). When
	// empty, watches the ticket's own room.
	RoomAlias ref.RoomAlias `json:"room_alias,omitempty"`

	// ContentMatch specifies criteria that must be satisfied in the
	// watched event's content (type "state_event"). Same semantics
	// as StartCondition.ContentMatch: bare scalars for equality,
	// $-prefixed operators for comparisons and set membership.
	ContentMatch schema.ContentMatch `json:"content_match,omitempty"`

	// TicketID is the ticket to watch (type "ticket"). The gate
	// is satisfied when that ticket's status becomes "closed".
	TicketID string `json:"ticket_id,omitempty"`

	// Duration is a relative delay for timer gates (e.g., "24h",
	// "30m", "90s"). Parsed by time.ParseDuration. At gate creation
	// time the ticket service converts Duration to an absolute
	// Target using the base time determined by the Base field.
	// Either Duration or Target (or both) must be set for timer
	// gates.
	Duration string `json:"duration,omitempty"`

	// Target is the absolute UTC time at which a timer gate fires
	// (RFC 3339). This is the evaluation field: the service fires
	// the gate when now >= Target. When the caller provides Duration
	// without Target, the service computes Target = base_time +
	// Duration at creation time. For recurring gates, Target is
	// updated on each re-arm to the next occurrence.
	Target string `json:"target,omitempty"`

	// Base determines what time Duration is measured from for timer
	// gates. "created" (default): the gate's CreatedAt timestamp.
	// "unblocked": the time when all of the ticket's blocked_by
	// dependencies were satisfied. When Base is "unblocked" and the
	// ticket still has open blockers, the timer has not started and
	// Target remains unset until blockers clear.
	Base string `json:"base,omitempty"`

	// Schedule is a cron expression for fixed-calendar recurrence
	// on timer gates (e.g., "0 7 * * *" for daily at 7am UTC).
	// Standard five-field cron syntax. After the gate fires and the
	// ticket is closed, the service computes the next occurrence,
	// updates Target, and re-arms the gate. Mutually exclusive with
	// Interval.
	Schedule string `json:"schedule,omitempty"`

	// Interval is a Go duration for drift-based recurrence on timer
	// gates (e.g., "4h"). Unlike Schedule, the next occurrence is
	// relative to when the ticket is closed, not anchored to
	// wall-clock time. Mutually exclusive with Schedule.
	Interval string `json:"interval,omitempty"`

	// LastFiredAt is set by the service each time this timer gate
	// transitions to "satisfied". For recurring gates this tracks
	// the most recent fire across cycles.
	LastFiredAt string `json:"last_fired_at,omitempty"`

	// FireCount is the total number of times this timer gate has
	// fired. Incremented by the service on each satisfaction.
	FireCount int `json:"fire_count,omitempty"`

	// MaxOccurrences limits how many times a recurring timer gate
	// fires. After the Nth fire the gate stays satisfied and does
	// not re-arm, allowing the ticket to be closed normally. Zero
	// means unlimited. Only meaningful when Schedule or Interval is
	// set.
	MaxOccurrences int `json:"max_occurrences,omitempty"`

	// --- Lifecycle metadata ---

	// CreatedAt is when this gate was added. For gates present
	// at ticket creation, equals the ticket's CreatedAt. For
	// gates added later, this is when the gate was appended.
	// Used as the base time for timer gates.
	CreatedAt string `json:"created_at,omitempty"`

	// SatisfiedAt is set when the gate transitions to "satisfied".
	SatisfiedAt string `json:"satisfied_at,omitempty"`

	// SatisfiedBy records what satisfied the gate: an event ID
	// for state_event/pipeline/ticket gates, a Matrix user ID
	// for human gates, "timer" for timer gates.
	SatisfiedBy string `json:"satisfied_by,omitempty"`
}

// Validate checks that the gate has a valid type, status, and the
// type-specific fields required for its gate type.
func (g *TicketGate) Validate() error {
	if g.ID == "" {
		return errors.New("gate: id is required")
	}
	switch g.Type {
	case "human":
		// No type-specific fields required.
	case "pipeline":
		if g.PipelineRef == "" {
			return fmt.Errorf("gate %q: pipeline_ref is required for pipeline gates", g.ID)
		}
	case "state_event":
		if g.EventType == "" {
			return fmt.Errorf("gate %q: event_type is required for state_event gates", g.ID)
		}
	case "ticket":
		if g.TicketID == "" {
			return fmt.Errorf("gate %q: ticket_id is required for ticket gates", g.ID)
		}
	case "review":
		// Review gates have no type-specific fields. They implicitly
		// watch the ticket they are attached to and are satisfied when
		// all reviewers have disposition "approved".
	case "timer":
		if err := g.validateTimer(); err != nil {
			return err
		}
	case "":
		return fmt.Errorf("gate %q: type is required", g.ID)
	default:
		return fmt.Errorf("gate %q: unknown type %q", g.ID, g.Type)
	}
	switch g.Status {
	case "pending", "satisfied":
		// Valid.
	case "":
		return fmt.Errorf("gate %q: status is required", g.ID)
	default:
		return fmt.Errorf("gate %q: unknown status %q", g.ID, g.Status)
	}
	return nil
}

// validateTimer checks timer-specific field constraints. Called from
// Validate when Type is "timer".
func (g *TicketGate) validateTimer() error {
	if g.Target == "" && g.Duration == "" {
		return fmt.Errorf("gate %q: target or duration is required for timer gates", g.ID)
	}
	if g.Target != "" {
		if _, err := time.Parse(time.RFC3339, g.Target); err != nil {
			return fmt.Errorf("gate %q: target must be RFC 3339: %w", g.ID, err)
		}
	}
	if g.Duration != "" {
		duration, err := time.ParseDuration(g.Duration)
		if err != nil {
			return fmt.Errorf("gate %q: invalid duration: %w", g.ID, err)
		}
		if duration <= 0 {
			return fmt.Errorf("gate %q: duration must be positive", g.ID)
		}
	}
	if g.Schedule != "" && g.Interval != "" {
		return fmt.Errorf("gate %q: schedule and interval are mutually exclusive", g.ID)
	}
	if g.Schedule != "" {
		if err := validateCronExpression(g.Schedule); err != nil {
			return fmt.Errorf("gate %q: invalid schedule: %w", g.ID, err)
		}
	}
	if g.Interval != "" {
		interval, err := time.ParseDuration(g.Interval)
		if err != nil {
			return fmt.Errorf("gate %q: invalid interval: %w", g.ID, err)
		}
		if interval < MinTimerRecurrence {
			return fmt.Errorf("gate %q: interval must be >= %s", g.ID, MinTimerRecurrence)
		}
	}
	switch g.Base {
	case "", "created", "unblocked":
		// Valid. Empty defaults to "created".
	default:
		return fmt.Errorf("gate %q: unknown base %q (must be \"created\" or \"unblocked\")", g.ID, g.Base)
	}
	if g.MaxOccurrences < 0 {
		return fmt.Errorf("gate %q: max_occurrences must be >= 0", g.ID)
	}
	if g.MaxOccurrences > 0 && g.Schedule == "" && g.Interval == "" {
		return fmt.Errorf("gate %q: max_occurrences requires schedule or interval", g.ID)
	}
	return nil
}

// IsRecurring reports whether this timer gate has a recurring schedule
// (Schedule or Interval set).
func (g *TicketGate) IsRecurring() bool {
	return g.Type == "timer" && (g.Schedule != "" || g.Interval != "")
}

// validateCronExpression parses and validates a cron expression using
// the cron package. Returns an error if the expression is malformed
// or contains out-of-range values.
func validateCronExpression(expression string) error {
	_, err := cron.Parse(expression)
	return err
}

// TicketNote is a short annotation on a ticket. Notes are for context
// that should travel with the ticket: warnings, references, analysis
// results, review comments. They are not conversations — use Matrix
// thread replies for discussion.
//
// Notes are append-only from the caller's perspective: the ticket
// service assigns IDs and timestamps. Notes can be removed by ID
// but not edited (append a correction instead).
type TicketNote struct {
	// ID uniquely identifies this note within the ticket.
	// Assigned by the ticket service (e.g., "n-1", "n-2").
	ID string `json:"id"`

	// Author is the Matrix user ID of the note creator.
	Author ref.UserID `json:"author"`

	// CreatedAt is an ISO 8601 timestamp.
	CreatedAt string `json:"created_at"`

	// Body is the note content, supporting markdown. Keep notes
	// concise — for content that exceeds a few hundred bytes,
	// store it as an artifact and reference it from a note.
	Body string `json:"body"`

	// ContextID is the ctx-* identifier of the context commit the
	// agent was working from when it created this note. Ties the
	// note to a specific point in the agent's understanding chain,
	// enabling traceability from ticket annotations back to the
	// conversation state that produced them.
	ContextID string `json:"context_id,omitempty"`
}

// Validate checks that all required fields are present.
func (n *TicketNote) Validate() error {
	if n.ID == "" {
		return errors.New("note: id is required")
	}
	if n.Author.IsZero() {
		return errors.New("note: author is required")
	}
	if n.CreatedAt == "" {
		return errors.New("note: created_at is required")
	}
	if n.Body == "" {
		return errors.New("note: body is required")
	}
	return nil
}

// TicketAttachment is a reference to an artifact stored outside the
// ticket state event. The ticket service stores references, not content.
type TicketAttachment struct {
	// Ref is the artifact reference (e.g., "art-<hash>").
	Ref string `json:"ref"`

	// Label is a human-readable description shown in listings
	// (e.g., "stack trace", "screenshot of rendering bug").
	Label string `json:"label,omitempty"`

	// ContentType is the MIME type of the referenced content
	// (e.g., "text/plain", "image/png").
	ContentType string `json:"content_type,omitempty"`
}

// Validate checks that the ref field is present and uses a supported
// format. MXC URIs are not supported — all blob data goes through the
// artifact service.
func (a *TicketAttachment) Validate() error {
	if a.Ref == "" {
		return errors.New("attachment: ref is required")
	}
	if strings.HasPrefix(a.Ref, "mxc://") {
		return errors.New("attachment: mxc:// refs are not supported, use the artifact service")
	}
	return nil
}

// TicketReview tracks reviewer feedback on a ticket. When a ticket
// enters the "review" status, the Review field carries the list of
// reviewers and an optional scope describing what to review.
//
// Review is for structured multi-reviewer feedback: N people examine
// work and provide individual dispositions. This is distinct from
// gates, which are binary coordination conditions (one approval,
// one pipeline pass). A review gate type (planned) bridges the two
// by watching for all-approved dispositions.
//
// The ticket service does not automatically transition status based
// on dispositions. The author reads all feedback and decides when to
// iterate (review → in_progress) or ship (review → closed). This
// preserves author agency and supports workflows where a post-review
// step follows approval.
type TicketReview struct {
	// Reviewers is the list of principals providing feedback. Each
	// reviewer tracks their own disposition independently. Must be
	// non-empty when the ticket is in "review" status.
	Reviewers []ReviewerEntry `json:"reviewers"`

	// Scope describes what the reviewers should examine. Optional —
	// the scope might be implicit in the ticket body. Use whichever
	// fields apply: Base/Head/Worktree for code review, ArtifactRef
	// for document review, Description for freeform requests.
	Scope *ReviewScope `json:"scope,omitempty"`
}

// Validate checks that the review has at least one reviewer and all
// entries are well-formed.
func (r *TicketReview) Validate() error {
	if len(r.Reviewers) == 0 {
		return errors.New("at least one reviewer is required")
	}
	for i := range r.Reviewers {
		if err := r.Reviewers[i].Validate(); err != nil {
			return fmt.Errorf("reviewers[%d]: %w", i, err)
		}
	}
	return nil
}

// ReviewerEntry tracks a single reviewer's feedback on a ticket.
type ReviewerEntry struct {
	// UserID is the Matrix user ID of the reviewer
	// (e.g., "@iree/amdgpu/engineer:bureau.local").
	UserID ref.UserID `json:"user_id"`

	// Disposition is the reviewer's current assessment: "pending"
	// (not yet reviewed), "approved" (looks good), "changes_requested"
	// (must fix before proceeding), or "commented" (feedback provided,
	// no blocking opinion).
	Disposition string `json:"disposition"`

	// UpdatedAt is the ISO 8601 timestamp of the last disposition
	// change. Empty when disposition is "pending" (initial state).
	UpdatedAt string `json:"updated_at,omitempty"`
}

// validDispositions is the set of recognized reviewer dispositions.
var validDispositions = map[string]bool{
	"pending":           true,
	"approved":          true,
	"changes_requested": true,
	"commented":         true,
}

// IsValidDisposition reports whether the given string is a recognized
// reviewer disposition.
func IsValidDisposition(disposition string) bool {
	return validDispositions[disposition]
}

// Validate checks that the reviewer entry has a valid user ID and
// disposition.
func (r *ReviewerEntry) Validate() error {
	if r.UserID.IsZero() {
		return errors.New("user_id is required")
	}
	if !IsValidDisposition(r.Disposition) {
		return fmt.Errorf("unknown disposition %q", r.Disposition)
	}
	return nil
}

// ReviewScope describes what reviewers should examine. All fields are
// optional — callers use whichever apply to their use case. Code review
// uses Base/Head/Worktree/Files. Document review uses ArtifactRef. PM
// review requests use Description. The scope might also be implicit in
// the ticket body, in which case ReviewScope can be nil entirely.
type ReviewScope struct {
	// Base is the starting point for diff-based review: a commit
	// hash, branch name, or tag. Used with Head to define a commit
	// range.
	Base string `json:"base,omitempty"`

	// Head is the end point for diff-based review.
	Head string `json:"head,omitempty"`

	// Worktree is the worktree path containing the changes under
	// review (e.g., "feature/auth-refactor"). Enables reviewers to
	// check out the code.
	Worktree string `json:"worktree,omitempty"`

	// Files lists specific files to focus on. When empty and
	// Base/Head are set, the reviewer examines the entire diff.
	Files []string `json:"files,omitempty"`

	// ArtifactRef is a reference to a stored artifact (document,
	// configuration, rendered output) to review.
	ArtifactRef string `json:"artifact_ref,omitempty"`

	// Description provides freeform context about what to review
	// when the scope isn't captured by the structured fields.
	Description string `json:"description,omitempty"`
}

// TicketOrigin records the provenance of an imported ticket.
type TicketOrigin struct {
	// Source identifies the external system ("github", "beads",
	// "linear", etc.).
	Source string `json:"source"`

	// ExternalRef is the identifier in the source system
	// (e.g., "bureau-foundation/bureau#42" for GitHub, "PROJ-123"
	// for Jira/Linear).
	ExternalRef string `json:"external_ref"`

	// SourceRoom is the room ID where this ticket was originally
	// created, if it was moved between rooms.
	SourceRoom string `json:"source_room,omitempty"`
}

// Validate checks that the required source and external_ref fields
// are present.
func (o *TicketOrigin) Validate() error {
	if o.Source == "" {
		return errors.New("origin: source is required")
	}
	if o.ExternalRef == "" {
		return errors.New("origin: external_ref is required")
	}
	return nil
}

// PipelineExecutionContent carries the type-specific content for a
// pipeline execution ticket (TicketContent.Type == "pipeline"). This
// is the tagged-pointer discriminated union: pipeline tickets carry
// structured execution state that generic tickets do not need.
//
// The executor updates these fields as the pipeline runs. Observers
// (ticket UI, subscription endpoints) can read current_step and
// conclusion without parsing pipeline result events.
type PipelineExecutionContent struct {
	// PipelineRef identifies the pipeline definition being executed
	// (e.g., "dev-workspace-init"). Matches the state key of the
	// m.bureau.pipeline event that defines the step sequence.
	PipelineRef string `json:"pipeline_ref"`

	// Variables are the resolved variable values for this execution.
	// Keyed by variable name (e.g., "REPOSITORY", "BRANCH"). These
	// are the actual values used, not the declarations from the
	// pipeline definition.
	Variables map[string]string `json:"variables,omitempty"`

	// CurrentStep is the 1-based index of the step currently
	// executing. Zero before execution starts. Updated by the
	// executor as it progresses through steps.
	CurrentStep int `json:"current_step,omitempty"`

	// TotalSteps is the total number of steps in the pipeline
	// definition. Set when the executor claims the ticket.
	TotalSteps int `json:"total_steps"`

	// CurrentStepName is the human-readable name of the step
	// currently executing (e.g., "clone-repository"). Empty before
	// execution starts or after completion. Updated alongside
	// CurrentStep.
	CurrentStepName string `json:"current_step_name,omitempty"`

	// Conclusion is the terminal outcome after execution completes:
	// "success", "failure", "aborted", or "cancelled". Empty while
	// the pipeline is still running. Mirrors the conclusion in the
	// companion m.bureau.pipeline_result event.
	Conclusion string `json:"conclusion,omitempty"`
}

// Validate checks that all required fields are present and well-formed.
func (p *PipelineExecutionContent) Validate() error {
	if p.PipelineRef == "" {
		return errors.New("pipeline_ref is required")
	}
	if p.TotalSteps < 0 {
		return fmt.Errorf("total_steps must be >= 0, got %d", p.TotalSteps)
	}
	if p.CurrentStep < 0 {
		return fmt.Errorf("current_step must be >= 0, got %d", p.CurrentStep)
	}
	if p.CurrentStep > p.TotalSteps {
		return fmt.Errorf("current_step (%d) exceeds total_steps (%d)", p.CurrentStep, p.TotalSteps)
	}
	switch p.Conclusion {
	case "", "success", "failure", "aborted", "cancelled":
		// Valid. Empty means still running.
	default:
		return fmt.Errorf("unknown conclusion %q", p.Conclusion)
	}
	return nil
}

// validTicketTypes is the set of recognized ticket types. Used for
// validation in both TicketContent and TicketConfigContent.AllowedTypes.
var validTicketTypes = map[string]bool{
	"task":     true,
	"bug":      true,
	"feature":  true,
	"epic":     true,
	"chore":    true,
	"docs":     true,
	"question": true,
	"pipeline": true,
}

// IsValidType reports whether the given string is a recognized ticket type.
func IsValidType(ticketType string) bool {
	return validTicketTypes[ticketType]
}

// PrefixForType returns the ticket ID prefix for a given ticket type.
// Pipeline tickets use "pip", all other types use the room's configured
// prefix (defaulting to "tkt"). Returns the type-specific prefix if one
// exists, or empty string to indicate the room default should be used.
func PrefixForType(ticketType string) string {
	switch ticketType {
	case "pipeline":
		return "pip"
	default:
		return ""
	}
}

// TicketConfigContent enables and configures ticket management for a
// room. Rooms without this event do not accept ticket operations from
// the ticket service. Published by the admin via "bureau ticket enable".
//
// State key: "" (singleton per room)
// Room: any room that wants ticket management
type TicketConfigContent struct {
	// Version is the schema version (see TicketConfigVersion).
	// Same semantics as TicketContent.Version — call CanModify()
	// before any read-modify-write cycle.
	Version int `json:"version"`

	// Prefix is the ticket ID prefix for this room. Defaults
	// to "tkt" if empty. The ticket service generates IDs as
	// prefix + "-" + short hash.
	Prefix string `json:"prefix,omitempty"`

	// AllowedTypes restricts which ticket types can be created in
	// this room. When empty, all types are allowed. When set, only
	// the listed types are accepted by the ticket service. This
	// enables dedicated rooms for specific ticket types: a pipeline
	// execution room might set AllowedTypes to ["pipeline"] to
	// prevent manual task tickets from being filed there.
	AllowedTypes []string `json:"allowed_types,omitempty"`

	// DefaultLabels are applied to new tickets that don't
	// explicitly specify labels.
	DefaultLabels []string `json:"default_labels,omitempty"`

	// Extra is a documented extension namespace. Same semantics as
	// TicketContent.Extra.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// Validate checks that the config has a valid version and that
// AllowedTypes (if set) contains only recognized ticket types.
func (c *TicketConfigContent) Validate() error {
	if c.Version < 1 {
		return fmt.Errorf("ticket config: version must be >= 1, got %d", c.Version)
	}
	for i, typeName := range c.AllowedTypes {
		if !IsValidType(typeName) {
			return fmt.Errorf("ticket config: allowed_types[%d]: unknown type %q", i, typeName)
		}
	}
	return nil
}

// CanModify checks whether this code version can safely modify this
// config event. Same semantics as TicketContent.CanModify.
func (c *TicketConfigContent) CanModify() error {
	if c.Version > TicketConfigVersion {
		return fmt.Errorf(
			"ticket config version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade the ticket service before modifying this event",
			c.Version, TicketConfigVersion,
		)
	}
	return nil
}
