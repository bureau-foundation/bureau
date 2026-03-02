// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forgesub

import (
	"slices"

	"github.com/bureau-foundation/bureau/lib/schema/forge"
)

// matchesForgeConfig checks whether an event passes a room's forge
// config filters. Returns true if the event's category is listed in
// config.Events AND the event passes the TriageFilter (if present).
//
// An empty Events list means no categories are enabled: the room
// receives nothing. This is default-deny — rooms must explicitly opt
// into event categories.
//
// If config is nil, no events pass (same as empty Events).
func matchesForgeConfig(config *forge.ForgeConfig, event *forge.Event) bool {
	if config == nil {
		return false
	}
	if len(config.Events) == 0 {
		return false
	}
	if !slices.Contains(config.Events, event.Type) {
		return false
	}
	return matchesTriageFilter(config.TriageFilter, event)
}

// matchesTriageFilter checks whether an event passes the triage
// filter. A nil filter passes everything.
//
// When both Labels and EventTypes are set, both must match
// (conjunctive). When only one is set, only that criterion applies.
//
// Label matching: for issue events, at least one event label must
// appear in the filter's label set. Event types that have no labels
// (push, review, comment, CI) pass the label filter vacuously — they
// cannot be filtered by a property they do not carry. This prevents
// label filters from accidentally blocking all non-issue events.
//
// EventType matching: the event's compound type string (e.g.,
// "issue_opened", "pr_merged", "ci_completed") must appear in the
// filter's EventTypes list. The compound string is constructed as
// category_prefix + "_" + action. Push events use "push" with no
// action suffix.
func matchesTriageFilter(filter *forge.TriageFilter, event *forge.Event) bool {
	if filter == nil {
		return true
	}

	if len(filter.Labels) > 0 {
		labels, supportsLabels := eventLabelsIfSupported(event)
		if supportsLabels {
			// This event type carries labels. The event must have
			// at least one label matching the filter.
			if !hasOverlap(labels, filter.Labels) {
				return false
			}
		}
		// Event types that don't carry labels (push, review,
		// comment, CI) pass vacuously — they can't be filtered
		// by a property they don't have.
	}

	if len(filter.EventTypes) > 0 {
		compound := eventTypeString(event)
		if compound == "" || !slices.Contains(filter.EventTypes, compound) {
			return false
		}
	}

	return true
}

// eventTypeString constructs the compound "category_action" string
// for triage filter matching. Returns "" for invalid or unpopulated
// events.
func eventTypeString(event *forge.Event) string {
	switch event.Type {
	case forge.EventCategoryPush:
		return "push"
	case forge.EventCategoryIssues:
		if event.Issue != nil {
			return "issue_" + event.Issue.Action
		}
	case forge.EventCategoryPullRequest:
		if event.PullRequest != nil {
			return "pr_" + event.PullRequest.Action
		}
	case forge.EventCategoryReview:
		if event.Review != nil {
			return "review_" + event.Review.State
		}
	case forge.EventCategoryComment:
		return "comment_created"
	case forge.EventCategoryCIStatus:
		if event.CIStatus != nil {
			return "ci_" + event.CIStatus.Status
		}
	}
	return ""
}

// eventLabelsIfSupported extracts labels from the event and reports
// whether the event type supports labels. Only issue events currently
// carry labels. The second return value distinguishes "event type
// doesn't have labels" (false) from "event type has labels but the
// list is empty" (true, nil).
func eventLabelsIfSupported(event *forge.Event) ([]string, bool) {
	if event.Type == forge.EventCategoryIssues && event.Issue != nil {
		return event.Issue.Labels, true
	}
	return nil, false
}

// hasOverlap reports whether any element in a appears in b.
func hasOverlap(a, b []string) bool {
	// For the expected cardinality (a few labels, a few filter
	// entries), linear scan is faster than building a set.
	for _, value := range a {
		if slices.Contains(b, value) {
			return true
		}
	}
	return false
}
