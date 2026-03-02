// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forge

import "fmt"

// EventCategory identifies the kind of forge event in the
// discriminated Event union and in ForgeConfig.Events filtering.
// Provider-specific webhook event names are translated to these
// categories at ingestion time.
type EventCategory string

const (
	EventCategoryPush        EventCategory = "push"
	EventCategoryPullRequest EventCategory = "pull_request"
	EventCategoryIssues      EventCategory = "issues"
	EventCategoryReview      EventCategory = "review"
	EventCategoryComment     EventCategory = "comment"
	EventCategoryCIStatus    EventCategory = "ci_status"
)

// --- Provider-agnostic event types ---
//
// Each forge connector translates its native webhook payloads into
// these common types. Agents receive the same typed structs regardless
// of which forge hosts the repository.
//
// All event types use CBOR tags because they are transmitted over
// CBOR-encoded socket streams, not stored as Matrix state events.

// Commit represents a single commit in a push event.
type Commit struct {
	SHA       string   `cbor:"sha"`
	Message   string   `cbor:"message"`
	Author    string   `cbor:"author"`    // "Name <email>"
	Timestamp string   `cbor:"timestamp"` // RFC3339
	URL       string   `cbor:"url"`
	Added     []string `cbor:"added,omitempty"`
	Modified  []string `cbor:"modified,omitempty"`
	Removed   []string `cbor:"removed,omitempty"`
}

// PushEvent represents a push to a repository branch.
type PushEvent struct {
	Provider     string   `cbor:"provider"`
	Repo         string   `cbor:"repo"`   // "owner/repo"
	Ref          string   `cbor:"ref"`    // "refs/heads/main"
	Before       string   `cbor:"before"` // previous HEAD SHA
	After        string   `cbor:"after"`  // new HEAD SHA
	Commits      []Commit `cbor:"commits"`
	Sender       string   `cbor:"sender"`                  // forge username
	BureauEntity string   `cbor:"bureau_entity,omitempty"` // mapped Bureau entity
	Summary      string   `cbor:"summary"`                 // human-readable with links
	URL          string   `cbor:"url"`                     // web URL to diff/compare
}

// PullRequestAction enumerates the common actions on a pull request.
// Connectors translate provider-specific action strings to these
// values.
type PullRequestAction string

const (
	PullRequestOpened           PullRequestAction = "opened"
	PullRequestClosed           PullRequestAction = "closed"
	PullRequestMerged           PullRequestAction = "merged"
	PullRequestSynchronize      PullRequestAction = "synchronize"
	PullRequestReviewRequested  PullRequestAction = "review_requested"
	PullRequestReadyForReview   PullRequestAction = "ready_for_review"
	PullRequestConvertedToDraft PullRequestAction = "converted_to_draft"
	PullRequestReopened         PullRequestAction = "reopened"
	PullRequestEdited           PullRequestAction = "edited"
)

// PullRequestEvent represents a change to a pull request (or merge
// request on GitLab).
type PullRequestEvent struct {
	Provider     string            `cbor:"provider"`
	Repo         string            `cbor:"repo"`
	Number       int               `cbor:"number"`
	Action       PullRequestAction `cbor:"action"`
	Title        string            `cbor:"title"`
	Author       string            `cbor:"author"` // forge username
	BureauEntity string            `cbor:"bureau_entity,omitempty"`
	HeadRef      string            `cbor:"head_ref"`
	BaseRef      string            `cbor:"base_ref"`
	HeadSHA      string            `cbor:"head_sha"`
	Draft        bool              `cbor:"draft"`
	Summary      string            `cbor:"summary"`
	URL          string            `cbor:"url"`
}

// IssueAction enumerates the common actions on an issue.
type IssueAction string

const (
	IssueOpened   IssueAction = "opened"
	IssueClosed   IssueAction = "closed"
	IssueReopened IssueAction = "reopened"
	IssueLabeled  IssueAction = "labeled"
	IssueAssigned IssueAction = "assigned"
	IssueEdited   IssueAction = "edited"
)

// IssueEvent represents a change to an issue.
type IssueEvent struct {
	Provider     string      `cbor:"provider"`
	Repo         string      `cbor:"repo"`
	Number       int         `cbor:"number"`
	Action       IssueAction `cbor:"action"`
	Title        string      `cbor:"title"`
	Body         string      `cbor:"body,omitempty"` // issue description (markdown)
	Author       string      `cbor:"author"`
	BureauEntity string      `cbor:"bureau_entity,omitempty"`
	Labels       []string    `cbor:"labels,omitempty"`
	Summary      string      `cbor:"summary"`
	URL          string      `cbor:"url"`
}

// ReviewState enumerates the common review dispositions.
type ReviewState string

const (
	ReviewApproved         ReviewState = "approved"
	ReviewChangesRequested ReviewState = "changes_requested"
	ReviewCommented        ReviewState = "commented"
)

// ReviewEvent represents a code review submission.
type ReviewEvent struct {
	Provider     string      `cbor:"provider"`
	Repo         string      `cbor:"repo"`
	PRNumber     int         `cbor:"pr_number"`
	Reviewer     string      `cbor:"reviewer"` // forge username
	BureauEntity string      `cbor:"bureau_entity,omitempty"`
	State        ReviewState `cbor:"state"`
	Body         string      `cbor:"body"`
	Summary      string      `cbor:"summary"`
	URL          string      `cbor:"url"`
}

// CommentEvent represents a comment on an issue or PR.
type CommentEvent struct {
	Provider     string     `cbor:"provider"`
	Repo         string     `cbor:"repo"`
	EntityType   EntityType `cbor:"entity_type"`
	EntityNumber int        `cbor:"entity_number"`
	Author       string     `cbor:"author"`
	BureauEntity string     `cbor:"bureau_entity,omitempty"`
	Body         string     `cbor:"body"`
	Summary      string     `cbor:"summary"`
	URL          string     `cbor:"url"`
}

// CIStatus enumerates the common CI/CD run statuses.
type CIStatus string

const (
	CIStatusQueued     CIStatus = "queued"
	CIStatusInProgress CIStatus = "in_progress"
	CIStatusCompleted  CIStatus = "completed"
)

// CIConclusion enumerates the common CI/CD run conclusions.
type CIConclusion string

const (
	CIConclusionSuccess   CIConclusion = "success"
	CIConclusionFailure   CIConclusion = "failure"
	CIConclusionCancelled CIConclusion = "cancelled"
)

// CIStatusEvent represents a CI/CD pipeline or workflow run status
// change. Covers GitHub Actions, Forgejo Actions, GitLab CI.
type CIStatusEvent struct {
	Provider   string       `cbor:"provider"`
	Repo       string       `cbor:"repo"`
	RunID      string       `cbor:"run_id"`   // provider-specific run identifier
	Workflow   string       `cbor:"workflow"` // workflow/pipeline name
	Status     CIStatus     `cbor:"status"`
	Conclusion CIConclusion `cbor:"conclusion"` // empty when not completed
	HeadSHA    string       `cbor:"head_sha"`
	Branch     string       `cbor:"branch"`
	PRNumber   int          `cbor:"pr_number,omitempty"`
	URL        string       `cbor:"url"`
	Summary    string       `cbor:"summary"`
}

// --- Discriminated event union ---

// Event is a discriminated union of forge event types. The Type field
// identifies which event pointer is populated. Exactly one event
// pointer is non-nil for a valid Event.
type Event struct {
	Type        EventCategory     `cbor:"type"`
	Push        *PushEvent        `cbor:"push,omitempty"`
	PullRequest *PullRequestEvent `cbor:"pull_request,omitempty"`
	Issue       *IssueEvent       `cbor:"issue,omitempty"`
	Review      *ReviewEvent      `cbor:"review,omitempty"`
	Comment     *CommentEvent     `cbor:"comment,omitempty"`
	CIStatus    *CIStatusEvent    `cbor:"ci_status,omitempty"`
}

// --- Entity references ---

// EntityType identifies the kind of forge entity an EntityRef points
// to. Used in subscription targeting and entity_closed frames.
type EntityType string

const (
	EntityTypeIssue       EntityType = "issue"
	EntityTypePullRequest EntityType = "pull_request"
	EntityTypeWorkflowRun EntityType = "workflow_run"
)

// EntityRef identifies a specific entity on a forge.
type EntityRef struct {
	Provider   string     `cbor:"provider"`
	Repo       string     `cbor:"repo"` // "owner/repo"
	EntityType EntityType `cbor:"entity_type"`
	Number     int        `cbor:"number,omitempty"` // issue/PR number
	RunID      string     `cbor:"run_id,omitempty"` // CI run ID (for workflow_run)
}

// IsZero reports whether the EntityRef is the zero value.
func (r EntityRef) IsZero() bool {
	return r == EntityRef{}
}

// Validate checks that the EntityRef is well-formed: non-empty
// provider and repo, valid entity type, and the correct identifier
// field set for the entity type (Number for issues/PRs, RunID for
// workflow runs).
func (r EntityRef) Validate() error {
	if r.Provider == "" {
		return fmt.Errorf("entity ref: provider is required")
	}
	if r.Repo == "" {
		return fmt.Errorf("entity ref: repo is required")
	}
	switch r.EntityType {
	case EntityTypeIssue, EntityTypePullRequest:
		if r.Number <= 0 {
			return fmt.Errorf("entity ref: number is required for %s entity type", r.EntityType)
		}
	case EntityTypeWorkflowRun:
		if r.RunID == "" {
			return fmt.Errorf("entity ref: run_id is required for workflow_run entity type")
		}
	default:
		return fmt.Errorf("entity ref: unknown entity type %q", r.EntityType)
	}
	return nil
}

// Validate checks that the Event is a well-formed discriminated union:
// the Type field must identify a known category, exactly one event
// variant must be populated, and the populated variant must match Type.
// Each populated variant must also have non-empty Provider and Repo
// fields.
func (e *Event) Validate() error {
	type variantInfo struct {
		category EventCategory
		present  bool
		provider string
		repo     string
	}

	variants := []variantInfo{
		{EventCategoryPush, e.Push != nil, "", ""},
		{EventCategoryPullRequest, e.PullRequest != nil, "", ""},
		{EventCategoryIssues, e.Issue != nil, "", ""},
		{EventCategoryReview, e.Review != nil, "", ""},
		{EventCategoryComment, e.Comment != nil, "", ""},
		{EventCategoryCIStatus, e.CIStatus != nil, "", ""},
	}

	// Fill in provider/repo for populated variants.
	if e.Push != nil {
		variants[0].provider, variants[0].repo = e.Push.Provider, e.Push.Repo
	}
	if e.PullRequest != nil {
		variants[1].provider, variants[1].repo = e.PullRequest.Provider, e.PullRequest.Repo
	}
	if e.Issue != nil {
		variants[2].provider, variants[2].repo = e.Issue.Provider, e.Issue.Repo
	}
	if e.Review != nil {
		variants[3].provider, variants[3].repo = e.Review.Provider, e.Review.Repo
	}
	if e.Comment != nil {
		variants[4].provider, variants[4].repo = e.Comment.Provider, e.Comment.Repo
	}
	if e.CIStatus != nil {
		variants[5].provider, variants[5].repo = e.CIStatus.Provider, e.CIStatus.Repo
	}

	populatedCount := 0
	var matchedType EventCategory
	for _, variant := range variants {
		if variant.present {
			populatedCount++
			matchedType = variant.category
			if variant.provider == "" {
				return fmt.Errorf("event: %s variant has empty provider", variant.category)
			}
			if variant.repo == "" {
				return fmt.Errorf("event: %s variant has empty repo", variant.category)
			}
		}
	}

	if populatedCount == 0 {
		return fmt.Errorf("event: no variant populated (type is %q)", e.Type)
	}
	if populatedCount > 1 {
		return fmt.Errorf("event: multiple variants populated (%d), expected exactly one", populatedCount)
	}
	if e.Type != matchedType {
		return fmt.Errorf("event: type %q does not match populated variant %q", e.Type, matchedType)
	}

	return nil
}

// --- Event accessor methods ---
//
// These extract common metadata from the discriminated Event union
// without requiring callers to switch on every event type. Each method
// returns the zero value if the event is invalid (no variant populated
// or Type does not match the populated variant).

// Repo returns the "owner/repo" string from the event.
func (e *Event) Repo() string {
	_, repo := e.providerAndRepo()
	return repo
}

// Provider returns the forge provider string from the event.
func (e *Event) Provider() string {
	provider, _ := e.providerAndRepo()
	return provider
}

// providerAndRepo extracts the provider and repo from whichever event
// variant is populated. Returns zero values if the event is invalid.
func (e *Event) providerAndRepo() (string, string) {
	switch e.Type {
	case EventCategoryPush:
		if e.Push != nil {
			return e.Push.Provider, e.Push.Repo
		}
	case EventCategoryPullRequest:
		if e.PullRequest != nil {
			return e.PullRequest.Provider, e.PullRequest.Repo
		}
	case EventCategoryIssues:
		if e.Issue != nil {
			return e.Issue.Provider, e.Issue.Repo
		}
	case EventCategoryReview:
		if e.Review != nil {
			return e.Review.Provider, e.Review.Repo
		}
	case EventCategoryComment:
		if e.Comment != nil {
			return e.Comment.Provider, e.Comment.Repo
		}
	case EventCategoryCIStatus:
		if e.CIStatus != nil {
			return e.CIStatus.Provider, e.CIStatus.Repo
		}
	}
	return "", ""
}

// EntityRefFromEvent extracts the entity reference from the event, if
// one exists. Push events have no entity and return a zero EntityRef
// with false.
//
// Review and Comment events produce entity refs pointing at their
// parent entity: a review on PR #42 returns an EntityRef with
// EntityTypePullRequest and Number 42. This means an agent subscribed
// to PR #42 receives reviews and comments on that PR.
func (e *Event) EntityRefFromEvent() (EntityRef, bool) {
	switch e.Type {
	case EventCategoryPush:
		return EntityRef{}, false
	case EventCategoryPullRequest:
		if e.PullRequest != nil {
			return EntityRef{
				Provider:   e.PullRequest.Provider,
				Repo:       e.PullRequest.Repo,
				EntityType: EntityTypePullRequest,
				Number:     e.PullRequest.Number,
			}, true
		}
	case EventCategoryIssues:
		if e.Issue != nil {
			return EntityRef{
				Provider:   e.Issue.Provider,
				Repo:       e.Issue.Repo,
				EntityType: EntityTypeIssue,
				Number:     e.Issue.Number,
			}, true
		}
	case EventCategoryReview:
		if e.Review != nil {
			return EntityRef{
				Provider:   e.Review.Provider,
				Repo:       e.Review.Repo,
				EntityType: EntityTypePullRequest,
				Number:     e.Review.PRNumber,
			}, true
		}
	case EventCategoryComment:
		if e.Comment != nil {
			entityType := EntityTypeIssue
			if e.Comment.EntityType == EntityTypePullRequest {
				entityType = EntityTypePullRequest
			}
			return EntityRef{
				Provider:   e.Comment.Provider,
				Repo:       e.Comment.Repo,
				EntityType: entityType,
				Number:     e.Comment.EntityNumber,
			}, true
		}
	case EventCategoryCIStatus:
		if e.CIStatus != nil {
			return EntityRef{
				Provider:   e.CIStatus.Provider,
				Repo:       e.CIStatus.Repo,
				EntityType: EntityTypeWorkflowRun,
				RunID:      e.CIStatus.RunID,
			}, true
		}
	}
	return EntityRef{}, false
}

// IsEntityClose reports whether this event represents an entity
// reaching a terminal state: issue closed, PR closed or merged, CI
// run completed. Used by the subscription manager to send
// entity_closed frames and clean up ephemeral subscriptions.
func (e *Event) IsEntityClose() bool {
	switch e.Type {
	case EventCategoryIssues:
		return e.Issue != nil && e.Issue.Action == IssueClosed
	case EventCategoryPullRequest:
		return e.PullRequest != nil &&
			(e.PullRequest.Action == PullRequestClosed ||
				e.PullRequest.Action == PullRequestMerged)
	case EventCategoryCIStatus:
		return e.CIStatus != nil && e.CIStatus.Status == CIStatusCompleted
	}
	return false
}

// --- Subscribe stream protocol ---

// SubscribeFrameType enumerates the frame types in the forge subscribe
// stream protocol. Follows the same pattern as the ticket service
// subscribe stream.
type SubscribeFrameType string

const (
	// FrameEvent indicates a forge event occurred. The Event field
	// contains the typed event struct.
	FrameEvent SubscribeFrameType = "event"

	// FrameEntityClosed indicates the subscribed entity was
	// closed/merged. Ephemeral subscriptions auto-clean. Persistent
	// subscriptions remain (the agent may want reopen events).
	FrameEntityClosed SubscribeFrameType = "entity_closed"

	// FrameHeartbeat is a connection liveness probe (30-second
	// interval). The client should consider the connection dead if
	// no frame of any type arrives within 2x this interval.
	FrameHeartbeat SubscribeFrameType = "heartbeat"

	// FrameResync indicates subscriber buffer overflow. The client
	// should clear local state and expect a fresh event replay.
	FrameResync SubscribeFrameType = "resync"

	// FrameCaughtUp indicates the initial backfill of pending events
	// (for persistent subscriptions with queued events) is complete.
	// Live events follow.
	FrameCaughtUp SubscribeFrameType = "caught_up"

	// FrameError is a terminal error. The connection will close after
	// this frame.
	FrameError SubscribeFrameType = "error"
)

// SubscribeFrame is a single CBOR value written on a forge subscribe
// stream. The Type field discriminates frame semantics.
type SubscribeFrame struct {
	Type      SubscribeFrameType `cbor:"type"`
	Event     *Event             `cbor:"event,omitempty"`      // for FrameEvent
	EntityRef *EntityRef         `cbor:"entity_ref,omitempty"` // for FrameEntityClosed
	Message   string             `cbor:"message,omitempty"`    // for FrameError
}
