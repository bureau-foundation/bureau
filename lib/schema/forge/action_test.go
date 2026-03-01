// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forge

import "testing"

func TestProviderAction(t *testing.T) {
	tests := []struct {
		provider Provider
		action   string
		want     string
	}{
		{ProviderGitHub, ActionSubscribe, "github/subscribe"},
		{ProviderForgejo, ActionCreateIssue, "forgejo/create-issue"},
		{ProviderGitLab, ActionMergePR, "gitlab/merge-pr"},
		{ProviderGitHub, ActionTriggerWorkflow, "github/trigger-workflow"},
		{ProviderForgejo, ActionReportStatus, "forgejo/report-status"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := ProviderAction(tt.provider, tt.action)
			if got != tt.want {
				t.Errorf("ProviderAction(%q, %q) = %q, want %q",
					tt.provider, tt.action, got, tt.want)
			}
		})
	}
}
