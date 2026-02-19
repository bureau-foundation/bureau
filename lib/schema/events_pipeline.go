// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"errors"
	"fmt"
)

// PipelineResultContentVersion is the current schema version for
// PipelineResultContent events. Increment when adding fields that
// existing code must not silently drop during read-modify-write.
const PipelineResultContentVersion = 1

// PipelineContent is the content of an EventTypePipeline state event.
// It defines a reusable automation sequence: a list of steps executed
// in order by the pipeline executor inside a sandbox. Steps can run
// shell commands, publish Matrix state events, or launch interactive
// sessions.
//
// Pipelines are the automation primitive for Bureau operations:
// workspace setup, service lifecycle, maintenance, deployment, and
// any structured task that benefits from observability, idempotency,
// and Matrix-native logging.
//
// Variable substitution (${NAME}) is applied to all string fields in
// steps before execution. Variables are resolved from step-level env,
// pipeline payload, Bureau runtime variables, and process environment.
type PipelineContent struct {
	// Description is a human-readable summary of what this pipeline
	// does (e.g., "Clone repository and prepare project workspace").
	Description string `json:"description,omitempty"`

	// Variables declares the variables this pipeline expects, with
	// optional defaults and required flags. The executor validates
	// required variables before starting execution. This is the
	// declaration — actual values come from payload, environment,
	// and step-level overrides at runtime.
	Variables map[string]PipelineVariable `json:"variables,omitempty"`

	// Steps is the ordered list of steps to execute. At least one
	// step is required. Steps run sequentially; the executor does
	// not support parallel steps (use shell backgrounding if needed).
	Steps []PipelineStep `json:"steps"`

	// OnFailure is a list of steps to execute when a non-optional step
	// fails. Typically used to publish a failure state event so that
	// observers can detect the failure and the resource doesn't get
	// stuck in a transitional state (e.g., "creating" forever).
	//
	// On_failure steps are inherently best-effort: if an on_failure
	// step itself fails, the failure is logged and the executor
	// continues with remaining on_failure steps. The original error
	// is preserved.
	//
	// On_failure steps do NOT run when a pipeline is aborted (an
	// assert_state step with on_mismatch "abort" triggers a clean
	// exit, not a failure).
	//
	// The variables FAILED_STEP (name of the step that failed) and
	// FAILED_ERROR (error message) are injected into the variable
	// context for on_failure steps.
	OnFailure []PipelineStep `json:"on_failure,omitempty"`

	// Outputs declares the pipeline's return values — which step outputs
	// are promoted to the pipeline result. Each entry maps an output name
	// to a PipelineOutput with a description and a value expression that
	// references step outputs via ${OUTPUT_<step>_<name>} variables.
	//
	// Pipeline outputs are resolved after all steps succeed. On failure
	// or abort, no pipeline-level outputs are produced.
	Outputs map[string]PipelineOutput `json:"outputs,omitempty"`

	// Log configures Matrix thread logging for pipeline executions.
	// When set, the executor creates a thread in the specified room
	// at pipeline start and posts step progress as thread replies.
	// When nil, the executor logs only to stdout (visible via
	// bureau observe).
	Log *PipelineLog `json:"log,omitempty"`
}

// PipelineVariable declares an expected variable for a pipeline.
// Variables are informational for documentation and validation —
// the executor resolves actual values from payload, environment,
// and step-level overrides.
type PipelineVariable struct {
	// Description explains what this variable is for (shown by
	// bureau pipeline show).
	Description string `json:"description,omitempty"`

	// Default is the fallback value when the variable is not
	// provided in any source. Empty string is a valid default.
	Default string `json:"default,omitempty"`

	// Required means the executor must fail if this variable has
	// no value from any source (including Default). A variable
	// with both Required and Default set uses the default only
	// when no explicit value is provided.
	Required bool `json:"required,omitempty"`
}

// PipelineOutput declares a pipeline-level output value. Pipeline outputs
// are the externally visible return values — they appear in
// PipelineResultContent and CommandResultMessage so initiators and
// observers can consume structured results.
//
// The Value field is a variable expression (e.g.,
// "${OUTPUT_clone_repository_head_sha}") that references a step output.
// It is expanded after all steps succeed, using the same variable
// substitution as step fields.
type PipelineOutput struct {
	// Description is a human-readable explanation of what this output
	// represents (e.g., "HEAD commit SHA of the cloned repository").
	// Used in bureau pipeline show and in result events for
	// observability.
	Description string `json:"description,omitempty"`

	// Value is a variable expression that resolves to the output value.
	// Typically references a step output: "${OUTPUT_clone_repo_head_sha}".
	// Expanded after all steps succeed.
	Value string `json:"value"`
}

// PipelineStep is a single step in a pipeline. Exactly one of Run,
// Publish, or AssertState must be set:
//   - Run: execute a shell command
//   - Publish: publish a Matrix state event
//   - AssertState: read a Matrix state event and check a condition
type PipelineStep struct {
	// Name is a human-readable identifier for this step, used in
	// log output and status messages (e.g., "clone-repository",
	// "publish-ready"). Required.
	Name string `json:"name"`

	// Run is a shell command executed via /bin/sh -c. Multi-line
	// strings are supported. Variable substitution (${NAME}) is
	// applied before execution. Mutually exclusive with Publish.
	Run string `json:"run,omitempty"`

	// Check is a post-step health check command. Runs after Run
	// succeeds; if Check exits non-zero, the step is treated as
	// failed. Catches cases where a command "succeeds" but
	// doesn't produce the expected result. Only valid with Run.
	Check string `json:"check,omitempty"`

	// When is a guard condition command. Runs before Run; if it
	// exits non-zero, the step is skipped (not failed). Use for
	// conditional steps: when: "test -n '${REPOSITORY}'" skips
	// clone for non-git workspaces.
	When string `json:"when,omitempty"`

	// Optional means step failure doesn't abort the pipeline.
	// The failure is logged but execution continues. Use for
	// best-effort steps like project-specific init scripts that
	// may not exist.
	Optional bool `json:"optional,omitempty"`

	// Publish sends a Matrix state event instead of running a
	// shell command. The executor connects to the proxy Unix
	// socket directly. Mutually exclusive with Run and AssertState.
	Publish *PipelinePublish `json:"publish,omitempty"`

	// AssertState reads a Matrix state event and checks a field
	// against an expected value. Used for precondition checks
	// (e.g., verifying a workspace is still in "teardown" before
	// proceeding with destructive operations) and advisory CAS
	// (e.g., verifying no one else is already removing a worktree).
	//
	// Mutually exclusive with Run and Publish.
	AssertState *PipelineAssertState `json:"assert_state,omitempty"`

	// Timeout is the maximum duration for this step (e.g., "5m",
	// "30s", "1h"). Parsed by time.ParseDuration. The executor
	// kills the step if it exceeds this duration. When empty,
	// defaults to 5 minutes at runtime.
	Timeout string `json:"timeout,omitempty"`

	// GracePeriod is the duration between SIGTERM and SIGKILL when
	// a step's timeout expires. When set, the executor sends SIGTERM
	// to the process group first, waits up to this duration for the
	// process to exit gracefully, then escalates to SIGKILL. When
	// empty, the executor sends SIGKILL immediately on timeout.
	//
	// Use this for steps that perform irreversible operations
	// (database writes, external API calls with side effects) where
	// abrupt termination could leave state inconsistent. Most sandbox
	// steps should use the default (immediate SIGKILL) since sandbox
	// processes are ephemeral and hold no durable state.
	//
	// Parsed by time.ParseDuration. Only valid on run steps.
	GracePeriod string `json:"grace_period,omitempty"`

	// Env sets additional environment variables for this step
	// only. Merged with pipeline-level variables; step values
	// take precedence on conflict.
	Env map[string]string `json:"env,omitempty"`

	// Outputs declares files to capture as named output values after
	// the step's run command (and check, if present) succeeds. Each
	// map entry is either a string (file path for inline capture) or
	// a JSON object (PipelineStepOutput with artifact mode and
	// metadata). Only valid on run steps.
	//
	// Inline outputs read the file and store its content as a string
	// value (64 KB limit, trailing whitespace trimmed). Artifact
	// outputs stream the file to the artifact service and store the
	// returned art-* reference as the value.
	//
	// Output values are injected as variables for subsequent steps:
	// OUTPUT_<step_name>_<output_name> (dashes in step names become
	// underscores).
	//
	// The value type is json.RawMessage to support both string and
	// object forms. Use ParseStepOutputs to resolve into typed
	// PipelineStepOutput structs.
	Outputs map[string]json.RawMessage `json:"outputs,omitempty"`

	// Interactive means this step expects terminal interaction.
	// The executor allocates a PTY and does not capture stdout.
	// The operator interacts via bureau observe (readwrite mode).
	// Only valid with Run.
	Interactive bool `json:"interactive,omitempty"`
}

// PipelineStepOutput declares how to capture a single output value
// from a file produced by a run step.
type PipelineStepOutput struct {
	// Path is the filesystem path to read after the step succeeds.
	// Supports ${VARIABLE} substitution (expanded before execution,
	// same as all other step string fields).
	Path string `json:"path"`

	// Artifact means the file should be stored in the artifact
	// service rather than read inline. The output value becomes the
	// art-* content-addressed reference returned by the artifact
	// service. Use this for large or binary outputs (model weights,
	// compiled binaries, log files) that should be durably stored.
	// When false (default), the file is read as an inline string
	// (64 KB limit, trailing whitespace trimmed).
	Artifact bool `json:"artifact,omitempty"`

	// ContentType is the MIME type hint for artifact storage (e.g.,
	// "text/plain", "application/gzip"). Only meaningful when
	// Artifact is true. When empty, the artifact service
	// auto-detects based on content.
	ContentType string `json:"content_type,omitempty"`

	// Description is a human-readable explanation of what this
	// output represents (e.g., "HEAD commit SHA after clone").
	// Used in bureau pipeline show and in result events for
	// observability.
	Description string `json:"description,omitempty"`
}

// ParseStepOutputs parses the raw output declarations from a PipelineStep
// into typed PipelineStepOutput structs. Each entry in the map is either:
//   - A JSON string: interpreted as an inline file path →
//     PipelineStepOutput{Path: <string>}
//   - A JSON object: unmarshaled directly into PipelineStepOutput
//
// Returns an error if any entry is neither a string nor a valid object.
func ParseStepOutputs(raw map[string]json.RawMessage) (map[string]PipelineStepOutput, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	result := make(map[string]PipelineStepOutput, len(raw))
	for name, rawValue := range raw {
		parsed, err := parseOneStepOutput(name, rawValue)
		if err != nil {
			return nil, err
		}
		result[name] = parsed
	}
	return result, nil
}

// parseOneStepOutput parses a single output declaration from its raw
// JSON representation (string or object form).
func parseOneStepOutput(name string, raw json.RawMessage) (PipelineStepOutput, error) {
	// Try string form first (most common).
	var path string
	if err := json.Unmarshal(raw, &path); err == nil {
		return PipelineStepOutput{Path: path}, nil
	}

	// Try object form.
	var output PipelineStepOutput
	if err := json.Unmarshal(raw, &output); err != nil {
		return PipelineStepOutput{}, fmt.Errorf("output %q: must be a string (file path) or object (PipelineStepOutput), got: %s", name, string(raw))
	}
	return output, nil
}

// PipelinePublish describes a Matrix state event to publish as a
// pipeline step. The executor connects to the proxy Unix socket
// and PUTs the event directly (same mechanism as bureau-proxy-call).
// All string fields support variable substitution (${NAME}).
type PipelinePublish struct {
	// EventType is the Matrix state event type (e.g.,
	// "m.bureau.workspace").
	EventType string `json:"event_type"`

	// Room is the target room alias or ID. Supports variable
	// substitution (e.g., "${WORKSPACE_ROOM_ID}").
	Room string `json:"room"`

	// StateKey is the state key for the event. Empty string is
	// valid (singleton events like m.bureau.workspace).
	StateKey string `json:"state_key,omitempty"`

	// Content is the event content as a JSON-compatible map.
	// String values support variable substitution.
	Content map[string]any `json:"content"`
}

// PipelineAssertState describes a state event assertion — a precondition
// check that reads a Matrix state event and verifies a field matches an
// expected value. Used in pipeline steps to guard against stale state
// (e.g., a deinit pipeline verifying the resource is still in "removing"
// status before proceeding with destructive operations).
//
// Exactly one condition field must be set: Equals, NotEquals, In, or NotIn.
//
// All string fields support variable substitution (${NAME}).
type PipelineAssertState struct {
	// Room is the Matrix room alias or ID containing the state event.
	Room string `json:"room"`

	// EventType is the Matrix state event type to read (e.g.,
	// "m.bureau.worktree", "m.bureau.workspace").
	EventType string `json:"event_type"`

	// StateKey is the state key for the event. Empty string is valid
	// (for singleton events like m.bureau.workspace).
	StateKey string `json:"state_key,omitempty"`

	// Field is the top-level JSON field name to extract from the event
	// content (e.g., "status"). The extracted value is stringified for
	// comparison.
	Field string `json:"field"`

	// Equals asserts the field value equals this string exactly.
	Equals string `json:"equals,omitempty"`

	// NotEquals asserts the field value does not equal this string.
	NotEquals string `json:"not_equals,omitempty"`

	// In asserts the field value is one of the listed strings.
	In []string `json:"in,omitempty"`

	// NotIn asserts the field value is not any of the listed strings.
	NotIn []string `json:"not_in,omitempty"`

	// OnMismatch controls behavior when the assertion fails:
	//   - "fail" (default): the step fails, the pipeline fails, and
	//     on_failure steps run. Use for error conditions.
	//   - "abort": the pipeline exits cleanly with exit code 0 and
	//     on_failure steps do NOT run. Use for benign precondition
	//     mismatches (e.g., "someone else is already handling this").
	OnMismatch string `json:"on_mismatch,omitempty"`

	// Message is a human-readable explanation logged when the assertion
	// fails (e.g., "workspace status is no longer 'teardown'").
	Message string `json:"message,omitempty"`
}

// PipelineLog configures Matrix thread logging for a pipeline.
// When present on a PipelineContent, the executor creates a thread
// in the specified room and logs step progress as thread replies.
type PipelineLog struct {
	// Room is the Matrix room alias or ID where execution threads
	// are created. Supports variable substitution (e.g.,
	// "${WORKSPACE_ROOM_ID}"). The executor resolves aliases via
	// the proxy.
	Room string `json:"room"`
}

// PipelineResultContent is the content of an EventTypePipelineResult state
// event. Published by the pipeline executor when execution finishes. This
// is the Matrix-native public format; the JSONL result log
// (BUREAU_RESULT_PATH) is a separate internal format for daemon tailing.
//
// The ticket service evaluates pipeline gates against these events:
// TicketGate.PipelineRef matches PipelineRef, TicketGate.Conclusion
// matches Conclusion. Gate evaluation happens via /sync — when the
// ticket service sees a PipelineResult state event change, it re-checks
// all pending pipeline gates in that room.
type PipelineResultContent struct {
	// Version is the schema version (see PipelineResultContentVersion).
	// Same semantics as TicketContent.Version — call CanModify() before
	// any read-modify-write cycle.
	Version int `json:"version"`

	// PipelineRef identifies which pipeline was executed (e.g.,
	// "dev-workspace-init"). This is the name/ref used when the
	// pipeline was resolved. TicketGate.PipelineRef matches against
	// this field.
	PipelineRef string `json:"pipeline_ref"`

	// Conclusion is the terminal outcome: "success", "failure", or
	// "aborted". TicketGate.Conclusion matches against this field.
	// An empty Conclusion in a gate means "any completed result".
	Conclusion string `json:"conclusion"`

	// StartedAt is an ISO 8601 timestamp of when execution began.
	StartedAt string `json:"started_at"`

	// CompletedAt is an ISO 8601 timestamp of when execution finished.
	CompletedAt string `json:"completed_at"`

	// DurationMS is the total execution wall-clock time in milliseconds.
	DurationMS int64 `json:"duration_ms"`

	// StepCount is the total number of steps in the pipeline definition.
	StepCount int `json:"step_count"`

	// StepResults records the outcome of each step that executed. Steps
	// that were never reached (due to earlier failure or abort) are not
	// included. Ordered by execution order.
	StepResults []PipelineStepResult `json:"step_results,omitempty"`

	// FailedStep is the name of the step that caused a failure. Empty
	// when Conclusion is "success".
	FailedStep string `json:"failed_step,omitempty"`

	// ErrorMessage is the error text from the failed or aborted step.
	// Empty when Conclusion is "success".
	ErrorMessage string `json:"error_message,omitempty"`

	// LogEventID is the Matrix event ID of the thread root message in
	// the log room. Provides a direct link from the result summary to
	// the detailed step-by-step execution log. Empty when the pipeline
	// has no log room configured (which should not happen since the
	// result event itself is published to the log room).
	LogEventID string `json:"log_event_id,omitempty"`

	// Outputs contains the pipeline's resolved output values. Each
	// entry maps an output name to its string value (either inline
	// file content or an art-* artifact reference). Only populated
	// when Conclusion is "success" — failed and aborted pipelines
	// produce no outputs.
	Outputs map[string]string `json:"outputs,omitempty"`

	// Extra is a documented extension namespace for experimental or
	// preview fields before promotion to top-level schema fields in
	// a version bump. Same semantics as TicketContent.Extra.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// PipelineStepResult records the outcome of a single pipeline step.
type PipelineStepResult struct {
	// Name is the step's human-readable identifier from the pipeline
	// definition.
	Name string `json:"name"`

	// Status is the step outcome: "ok", "failed", "skipped", or
	// "aborted". "failed (optional)" is recorded when an optional
	// step fails but execution continues.
	Status string `json:"status"`

	// DurationMS is the step execution wall-clock time in milliseconds.
	DurationMS int64 `json:"duration_ms"`

	// Error is the error message when the step failed or aborted.
	// Empty for successful or skipped steps.
	Error string `json:"error,omitempty"`

	// Outputs contains the captured output values for this step.
	// Each entry maps an output name to its string value (inline
	// file content or art-* artifact reference). Only populated
	// for steps with status "ok" that declared outputs.
	Outputs map[string]string `json:"outputs,omitempty"`
}

// Validate checks that all required fields are present and well-formed.
// Returns an error describing the first invalid field found, or nil if
// the content is valid.
func (p *PipelineResultContent) Validate() error {
	if p.Version < 1 {
		return fmt.Errorf("pipeline result: version must be >= 1, got %d", p.Version)
	}
	if p.PipelineRef == "" {
		return errors.New("pipeline result: pipeline_ref is required")
	}
	switch p.Conclusion {
	case "success", "failure", "aborted":
		// Valid.
	case "":
		return errors.New("pipeline result: conclusion is required")
	default:
		return fmt.Errorf("pipeline result: unknown conclusion %q", p.Conclusion)
	}
	if p.StartedAt == "" {
		return errors.New("pipeline result: started_at is required")
	}
	if p.CompletedAt == "" {
		return errors.New("pipeline result: completed_at is required")
	}
	if p.StepCount < 1 {
		return fmt.Errorf("pipeline result: step_count must be >= 1, got %d", p.StepCount)
	}
	for i := range p.StepResults {
		if err := p.StepResults[i].Validate(); err != nil {
			return fmt.Errorf("pipeline result: step_results[%d]: %w", i, err)
		}
	}
	return nil
}

// CanModify checks whether this code version can safely perform a
// read-modify-write cycle on this event. Returns nil if safe, or an
// error explaining why modification would risk data loss.
//
// Pipeline result events are typically write-once (each execution
// overwrites the previous result), but CanModify is provided for
// consistency with the schema pattern and to protect against future
// use cases where results might be annotated.
func (p *PipelineResultContent) CanModify() error {
	if p.Version > PipelineResultContentVersion {
		return fmt.Errorf(
			"pipeline result version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade the pipeline executor before modifying this event",
			p.Version, PipelineResultContentVersion,
		)
	}
	return nil
}

// Validate checks that the step result has valid required fields.
func (s *PipelineStepResult) Validate() error {
	if s.Name == "" {
		return errors.New("step result: name is required")
	}
	switch s.Status {
	case "ok", "failed", "failed (optional)", "skipped", "aborted":
		// Valid.
	case "":
		return errors.New("step result: status is required")
	default:
		return fmt.Errorf("step result: unknown status %q", s.Status)
	}
	return nil
}
