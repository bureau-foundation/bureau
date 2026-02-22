// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipelinedef

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		content        *pipeline.PipelineContent
		expectedIssues int
		wantSubstrings []string
	}{
		{
			name: "valid single run step",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{Name: "hello", Run: "echo hello"},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "valid publish step",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "publish-ready",
						Publish: &pipeline.PipelinePublish{
							EventType: "m.bureau.workspace",
							Room:      "!room:bureau.local",
							Content:   map[string]any{"status": "ready"},
						},
					},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "valid mixed steps with all fields",
			content: &pipeline.PipelineContent{
				Description: "Full pipeline",
				Variables: map[string]pipeline.PipelineVariable{
					"REPO": {Description: "Repository URL", Required: true},
				},
				Steps: []pipeline.PipelineStep{
					{
						Name:    "clone",
						Run:     "git clone ${REPO}",
						When:    "test -n '${REPO}'",
						Check:   "test -d .git",
						Timeout: "5m",
						Env:     map[string]string{"GIT_SSH_COMMAND": "ssh -o StrictHostKeyChecking=no"},
					},
					{
						Name:        "interactive-setup",
						Run:         "bash",
						Interactive: true,
						Optional:    true,
						Timeout:     "1h",
					},
					{
						Name: "mark-ready",
						Publish: &pipeline.PipelinePublish{
							EventType: "m.bureau.workspace",
							Room:      "!room:bureau.local",
							Content:   map[string]any{"status": "ready"},
						},
					},
				},
				Log: &pipeline.PipelineLog{Room: "!room:bureau.local"},
			},
			expectedIssues: 0,
		},
		{
			name: "no steps",
			content: &pipeline.PipelineContent{
				Description: "Empty pipeline",
			},
			expectedIssues: 1,
			wantSubstrings: []string{"no steps"},
		},
		{
			name: "step missing name",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{Run: "echo hello"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"name is required"},
		},
		{
			name: "step with neither run nor publish nor assert_state",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{Name: "empty-step"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"must set exactly one of run, publish, or assert_state"},
		},
		{
			name: "step with both run and publish",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "both",
						Run:  "echo hello",
						Publish: &pipeline.PipelinePublish{
							EventType: "m.test",
							Room:      "!room:test",
							Content:   map[string]any{},
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"mutually exclusive"},
		},
		{
			name: "check on publish step",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name:  "bad-publish",
						Check: "test -f /tmp/done",
						Publish: &pipeline.PipelinePublish{
							EventType: "m.test",
							Room:      "!room:test",
							Content:   map[string]any{},
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"check is only valid on run steps"},
		},
		{
			name: "when on publish step is valid",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "conditional-publish",
						When: "test \"${MODE}\" = archive",
						Publish: &pipeline.PipelinePublish{
							EventType: "m.bureau.workspace",
							Room:      "!room:bureau.local",
							Content:   map[string]any{"status": "archived"},
						},
					},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "interactive on publish step",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name:        "bad-publish",
						Interactive: true,
						Publish: &pipeline.PipelinePublish{
							EventType: "m.test",
							Room:      "!room:test",
							Content:   map[string]any{},
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"interactive is only valid on run steps"},
		},
		{
			name: "publish missing event_type",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "bad-publish",
						Publish: &pipeline.PipelinePublish{
							Room:    "!room:test",
							Content: map[string]any{},
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"publish.event_type is required"},
		},
		{
			name: "publish missing room",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "bad-publish",
						Publish: &pipeline.PipelinePublish{
							EventType: "m.test",
							Content:   map[string]any{},
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"publish.room is required"},
		},
		{
			name: "publish missing content",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "bad-publish",
						Publish: &pipeline.PipelinePublish{
							EventType: "m.test",
							Room:      "!room:test",
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"publish.content is required"},
		},
		{
			name: "invalid timeout",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{Name: "bad-timeout", Run: "echo hello", Timeout: "5 minutes"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"invalid timeout"},
		},
		{
			name: "valid grace_period on run step",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{Name: "graceful", Run: "echo hello", Timeout: "5m", GracePeriod: "30s"},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "invalid grace_period",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{Name: "bad-grace", Run: "echo hello", GracePeriod: "thirty seconds"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"invalid grace_period"},
		},
		{
			name: "grace_period on publish step",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name:        "bad-publish",
						GracePeriod: "30s",
						Publish: &pipeline.PipelinePublish{
							EventType: "m.test",
							Room:      "!room:test",
							Content:   map[string]any{},
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"grace_period is only valid on run steps"},
		},
		{
			name: "log without room",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{Name: "hello", Run: "echo hello"},
				},
				Log: &pipeline.PipelineLog{},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"log.room is required"},
		},
		{
			name: "valid assert_state with equals",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "check-status",
						AssertState: &pipeline.PipelineAssertState{
							Room:      "!room:bureau.local",
							EventType: "m.bureau.workspace",
							Field:     "status",
							Equals:    "teardown",
						},
					},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "valid assert_state with not_equals and abort",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "check-not-removing",
						AssertState: &pipeline.PipelineAssertState{
							Room:       "!room:bureau.local",
							EventType:  "m.bureau.worktree",
							StateKey:   "feature/amdgpu",
							Field:      "status",
							NotEquals:  "removing",
							OnMismatch: "abort",
							Message:    "someone else is already removing",
						},
					},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "valid assert_state with in",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "check-status-in-set",
						AssertState: &pipeline.PipelineAssertState{
							Room:      "!room:bureau.local",
							EventType: "m.bureau.workspace",
							Field:     "status",
							In:        []string{"active", "teardown"},
						},
					},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "valid assert_state with not_in",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "check-status-not-terminal",
						AssertState: &pipeline.PipelineAssertState{
							Room:      "!room:bureau.local",
							EventType: "m.bureau.workspace",
							Field:     "status",
							NotIn:     []string{"archived", "removed"},
						},
					},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "assert_state missing room",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "bad-assert",
						AssertState: &pipeline.PipelineAssertState{
							EventType: "m.bureau.workspace",
							Field:     "status",
							Equals:    "active",
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"assert_state.room is required"},
		},
		{
			name: "assert_state missing event_type",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "bad-assert",
						AssertState: &pipeline.PipelineAssertState{
							Room:   "!room:bureau.local",
							Field:  "status",
							Equals: "active",
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"assert_state.event_type is required"},
		},
		{
			name: "assert_state missing field",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "bad-assert",
						AssertState: &pipeline.PipelineAssertState{
							Room:      "!room:bureau.local",
							EventType: "m.bureau.workspace",
							Equals:    "active",
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"assert_state.field is required"},
		},
		{
			name: "assert_state no condition",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "bad-assert",
						AssertState: &pipeline.PipelineAssertState{
							Room:      "!room:bureau.local",
							EventType: "m.bureau.workspace",
							Field:     "status",
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"requires exactly one condition"},
		},
		{
			name: "assert_state multiple conditions",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "bad-assert",
						AssertState: &pipeline.PipelineAssertState{
							Room:      "!room:bureau.local",
							EventType: "m.bureau.workspace",
							Field:     "status",
							Equals:    "active",
							NotEquals: "teardown",
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"conditions are mutually exclusive"},
		},
		{
			name: "assert_state invalid on_mismatch",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "bad-assert",
						AssertState: &pipeline.PipelineAssertState{
							Room:       "!room:bureau.local",
							EventType:  "m.bureau.workspace",
							Field:      "status",
							Equals:     "active",
							OnMismatch: "panic",
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"on_mismatch must be"},
		},
		{
			name: "assert_state combined with run",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "both",
						Run:  "echo hello",
						AssertState: &pipeline.PipelineAssertState{
							Room:      "!room:bureau.local",
							EventType: "m.bureau.workspace",
							Field:     "status",
							Equals:    "active",
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"mutually exclusive"},
		},
		{
			name: "valid on_failure steps",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{Name: "work", Run: "echo hello"},
				},
				OnFailure: []pipeline.PipelineStep{
					{
						Name: "publish-failed",
						Publish: &pipeline.PipelinePublish{
							EventType: "m.bureau.worktree",
							Room:      "!room:bureau.local",
							Content:   map[string]any{"status": "failed"},
						},
					},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "on_failure step with invalid structure",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{Name: "work", Run: "echo hello"},
				},
				OnFailure: []pipeline.PipelineStep{
					{Name: "bad-cleanup"}, // neither run nor publish
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"on_failure[0]"},
		},
		{
			name: "when on assert_state step is valid",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "conditional-assert",
						When: "test \"${MODE}\" = archive",
						AssertState: &pipeline.PipelineAssertState{
							Room:      "!room:bureau.local",
							EventType: "m.bureau.workspace",
							Field:     "status",
							Equals:    "teardown",
						},
					},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "valid run step with string outputs",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name:    "build",
						Run:     "make build",
						Outputs: map[string]json.RawMessage{"head_sha": json.RawMessage(`"/tmp/outputs/sha"`)},
					},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "valid run step with object outputs",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "build",
						Run:  "make build",
						Outputs: map[string]json.RawMessage{
							"build_log": json.RawMessage(`{"path": "/tmp/build.log", "artifact": true, "content_type": "text/plain"}`),
						},
					},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "outputs on publish step",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "publish-ready",
						Publish: &pipeline.PipelinePublish{
							EventType: "m.bureau.workspace",
							Room:      "!room:bureau.local",
							Content:   map[string]any{"status": "ready"},
						},
						Outputs: map[string]json.RawMessage{"bad": json.RawMessage(`"/tmp/bad"`)},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"outputs are only valid on run steps"},
		},
		{
			name: "outputs on on_failure step",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{Name: "work", Run: "echo hello"},
				},
				OnFailure: []pipeline.PipelineStep{
					{
						Name:    "cleanup",
						Run:     "echo cleanup",
						Outputs: map[string]json.RawMessage{"result": json.RawMessage(`"/tmp/result"`)},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"outputs are not allowed on on_failure steps"},
		},
		{
			name: "outputs with invalid name",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name:    "build",
						Run:     "make build",
						Outputs: map[string]json.RawMessage{"123-bad": json.RawMessage(`"/tmp/out"`)},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"valid identifier"},
		},
		{
			name: "outputs with empty path",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name:    "build",
						Run:     "make build",
						Outputs: map[string]json.RawMessage{"result": json.RawMessage(`""`)},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"path is required"},
		},
		{
			name: "content_type without artifact",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{
						Name: "build",
						Run:  "make build",
						Outputs: map[string]json.RawMessage{
							"result": json.RawMessage(`{"path": "/tmp/out", "artifact": false, "content_type": "text/plain"}`),
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"content_type is only valid when artifact is true"},
		},
		{
			name: "duplicate step names",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{Name: "build", Run: "make build"},
					{Name: "build", Run: "make build-again"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"duplicate step name"},
		},
		{
			name: "pipeline output with empty value",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{Name: "build", Run: "make build"},
				},
				Outputs: map[string]pipeline.PipelineOutput{
					"result": {Description: "build result", Value: ""},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"value is required"},
		},
		{
			name: "multiple issues",
			content: &pipeline.PipelineContent{
				Steps: []pipeline.PipelineStep{
					{Run: "echo orphan"}, // missing name
					{Name: "empty"},      // neither run nor publish
					{Name: "bad", Run: "x", Publish: &pipeline.PipelinePublish{ // both
						EventType: "m.test",
						Room:      "!room:test",
						Content:   map[string]any{},
					}},
				},
				Log: &pipeline.PipelineLog{},
			},
			// name is required, must set either run or publish, mutually exclusive, log.room is required
			expectedIssues: 4,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			issues := Validate(testCase.content)
			if len(issues) != testCase.expectedIssues {
				t.Fatalf("got %d issues, want %d:\n%s", len(issues), testCase.expectedIssues, strings.Join(issues, "\n"))
			}

			for _, substring := range testCase.wantSubstrings {
				found := false
				for _, issue := range issues {
					if strings.Contains(issue, substring) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected issue containing %q, got:\n%s", substring, strings.Join(issues, "\n"))
				}
			}
		})
	}
}
