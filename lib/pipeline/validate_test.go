// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		content        *schema.PipelineContent
		expectedIssues int
		wantSubstrings []string
	}{
		{
			name: "valid single run step",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "hello", Run: "echo hello"},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "valid publish step",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name: "publish-ready",
						Publish: &schema.PipelinePublish{
							EventType: "m.bureau.workspace.ready",
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
			content: &schema.PipelineContent{
				Description: "Full pipeline",
				Variables: map[string]schema.PipelineVariable{
					"REPO": {Description: "Repository URL", Required: true},
				},
				Steps: []schema.PipelineStep{
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
						Publish: &schema.PipelinePublish{
							EventType: "m.bureau.workspace.ready",
							Room:      "!room:bureau.local",
							Content:   map[string]any{"status": "ready"},
						},
					},
				},
				Log: &schema.PipelineLog{Room: "!room:bureau.local"},
			},
			expectedIssues: 0,
		},
		{
			name: "no steps",
			content: &schema.PipelineContent{
				Description: "Empty pipeline",
			},
			expectedIssues: 1,
			wantSubstrings: []string{"no steps"},
		},
		{
			name: "step missing name",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Run: "echo hello"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"name is required"},
		},
		{
			name: "step with neither run nor publish",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "empty-step"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"must set either run or publish"},
		},
		{
			name: "step with both run and publish",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name: "both",
						Run:  "echo hello",
						Publish: &schema.PipelinePublish{
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
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name:  "bad-publish",
						Check: "test -f /tmp/done",
						Publish: &schema.PipelinePublish{
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
			name: "when on publish step",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name: "bad-publish",
						When: "test -n '${REPO}'",
						Publish: &schema.PipelinePublish{
							EventType: "m.test",
							Room:      "!room:test",
							Content:   map[string]any{},
						},
					},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"when is only valid on run steps"},
		},
		{
			name: "interactive on publish step",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name:        "bad-publish",
						Interactive: true,
						Publish: &schema.PipelinePublish{
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
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name: "bad-publish",
						Publish: &schema.PipelinePublish{
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
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name: "bad-publish",
						Publish: &schema.PipelinePublish{
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
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name: "bad-publish",
						Publish: &schema.PipelinePublish{
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
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "bad-timeout", Run: "echo hello", Timeout: "5 minutes"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"invalid timeout"},
		},
		{
			name: "valid grace_period on run step",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "graceful", Run: "echo hello", Timeout: "5m", GracePeriod: "30s"},
				},
			},
			expectedIssues: 0,
		},
		{
			name: "invalid grace_period",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "bad-grace", Run: "echo hello", GracePeriod: "thirty seconds"},
				},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"invalid grace_period"},
		},
		{
			name: "grace_period on publish step",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{
						Name:        "bad-publish",
						GracePeriod: "30s",
						Publish: &schema.PipelinePublish{
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
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Name: "hello", Run: "echo hello"},
				},
				Log: &schema.PipelineLog{},
			},
			expectedIssues: 1,
			wantSubstrings: []string{"log.room is required"},
		},
		{
			name: "multiple issues",
			content: &schema.PipelineContent{
				Steps: []schema.PipelineStep{
					{Run: "echo orphan"}, // missing name
					{Name: "empty"},      // neither run nor publish
					{Name: "bad", Run: "x", Publish: &schema.PipelinePublish{ // both
						EventType: "m.test",
						Room:      "!room:test",
						Content:   map[string]any{},
					}},
				},
				Log: &schema.PipelineLog{},
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
