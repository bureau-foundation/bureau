// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forge

// Event category names used in ForgeConfig.Events and stream
// filtering. These are the common category names shared across all
// forge providers. Provider-specific webhook event names are
// translated to these categories at ingestion time.
const (
	EventCategoryPush        = "push"
	EventCategoryPullRequest = "pull_request"
	EventCategoryIssues      = "issues"
	EventCategoryReview      = "review"
	EventCategoryComment     = "comment"
	EventCategoryCIStatus    = "ci_status"
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
	Provider     string `cbor:"provider"`
	Repo         string `cbor:"repo"`
	Number       int    `cbor:"number"`
	Action       string `cbor:"action"` // see PullRequestAction constants
	Title        string `cbor:"title"`
	Author       string `cbor:"author"` // forge username
	BureauEntity string `cbor:"bureau_entity,omitempty"`
	HeadRef      string `cbor:"head_ref"`
	BaseRef      string `cbor:"base_ref"`
	HeadSHA      string `cbor:"head_sha"`
	Draft        bool   `cbor:"draft"`
	Summary      string `cbor:"summary"`
	URL          string `cbor:"url"`
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
	Provider     string   `cbor:"provider"`
	Repo         string   `cbor:"repo"`
	Number       int      `cbor:"number"`
	Action       string   `cbor:"action"` // see IssueAction constants
	Title        string   `cbor:"title"`
	Author       string   `cbor:"author"`
	BureauEntity string   `cbor:"bureau_entity,omitempty"`
	Labels       []string `cbor:"labels,omitempty"`
	Summary      string   `cbor:"summary"`
	URL          string   `cbor:"url"`
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
	Provider     string `cbor:"provider"`
	Repo         string `cbor:"repo"`
	PRNumber     int    `cbor:"pr_number"`
	Reviewer     string `cbor:"reviewer"` // forge username
	BureauEntity string `cbor:"bureau_entity,omitempty"`
	State        string `cbor:"state"` // see ReviewState constants
	Body         string `cbor:"body"`
	Summary      string `cbor:"summary"`
	URL          string `cbor:"url"`
}

// CommentEvent represents a comment on an issue or PR.
type CommentEvent struct {
	Provider     string `cbor:"provider"`
	Repo         string `cbor:"repo"`
	EntityType   string `cbor:"entity_type"` // "issue" or "pull_request"
	EntityNumber int    `cbor:"entity_number"`
	Author       string `cbor:"author"`
	BureauEntity string `cbor:"bureau_entity,omitempty"`
	Body         string `cbor:"body"`
	Summary      string `cbor:"summary"`
	URL          string `cbor:"url"`
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
	Provider   string `cbor:"provider"`
	Repo       string `cbor:"repo"`
	RunID      string `cbor:"run_id"`     // provider-specific run identifier
	Workflow   string `cbor:"workflow"`   // workflow/pipeline name
	Status     string `cbor:"status"`     // see CIStatus constants
	Conclusion string `cbor:"conclusion"` // see CIConclusion constants; empty when not completed
	HeadSHA    string `cbor:"head_sha"`
	Branch     string `cbor:"branch"`
	PRNumber   int    `cbor:"pr_number,omitempty"`
	URL        string `cbor:"url"`
	Summary    string `cbor:"summary"`
}

// --- Discriminated event union ---

// Event is a discriminated union of forge event types. The Type field
// identifies which event pointer is populated. Exactly one event
// pointer is non-nil for a valid Event.
type Event struct {
	Type        string            `cbor:"type"` // matches EventCategory* constants
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
			if e.Comment.EntityType == string(EntityTypePullRequest) {
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
		return e.Issue != nil && e.Issue.Action == string(IssueClosed)
	case EventCategoryPullRequest:
		return e.PullRequest != nil &&
			(e.PullRequest.Action == string(PullRequestClosed) ||
				e.PullRequest.Action == string(PullRequestMerged))
	case EventCategoryCIStatus:
		return e.CIStatus != nil && e.CIStatus.Status == string(CIStatusCompleted)
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
