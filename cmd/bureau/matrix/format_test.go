// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

func TestMatchesTypeFilter_EmptyFilters(t *testing.T) {
	if !matchesTypeFilter("m.room.message", nil) {
		t.Error("empty filter should match everything")
	}
	if !matchesTypeFilter("m.bureau.ticket", []string{}) {
		t.Error("empty slice filter should match everything")
	}
}

func TestMatchesTypeFilter_ExactMatch(t *testing.T) {
	filters := []string{"m.room.message", "m.bureau.ticket"}
	if !matchesTypeFilter("m.room.message", filters) {
		t.Error("exact match should succeed")
	}
	if !matchesTypeFilter("m.bureau.ticket", filters) {
		t.Error("exact match should succeed")
	}
	if matchesTypeFilter("m.room.topic", filters) {
		t.Error("non-matching type should fail")
	}
}

func TestMatchesTypeFilter_PrefixGlob(t *testing.T) {
	filters := []string{"m.bureau.*"}
	if !matchesTypeFilter("m.bureau.ticket", filters) {
		t.Error("prefix glob should match")
	}
	if !matchesTypeFilter("m.bureau.machine_key", filters) {
		t.Error("prefix glob should match")
	}
	if matchesTypeFilter("m.room.message", filters) {
		t.Error("prefix glob should not match unrelated type")
	}
}

func TestMatchesTypeFilter_MixedFilters(t *testing.T) {
	filters := []string{"m.room.message", "m.bureau.*"}
	if !matchesTypeFilter("m.room.message", filters) {
		t.Error("exact match in mixed filter should succeed")
	}
	if !matchesTypeFilter("m.bureau.service", filters) {
		t.Error("prefix match in mixed filter should succeed")
	}
	if matchesTypeFilter("m.room.topic", filters) {
		t.Error("non-matching type should fail in mixed filter")
	}
}

func TestFormatTimestamp(t *testing.T) {
	// 2026-02-17 00:00:00 UTC in milliseconds.
	ts := int64(1771200000000)
	result := formatTimestamp(ts)
	// The exact output depends on local timezone, but it should contain
	// a date in 2026-02-1x format.
	if !strings.HasPrefix(result, "2026-02-") {
		t.Errorf("unexpected timestamp format: %s", result)
	}
	if len(result) != 19 {
		t.Errorf("expected 19-char timestamp, got %d: %s", len(result), result)
	}
}

func TestAbbreviateContent_Empty(t *testing.T) {
	result := abbreviateContent(nil, 100)
	if result != "{}" {
		t.Errorf("expected {} for nil content, got %s", result)
	}
	result = abbreviateContent(map[string]any{}, 100)
	if result != "{}" {
		t.Errorf("expected {} for empty content, got %s", result)
	}
}

func TestAbbreviateContent_Short(t *testing.T) {
	content := map[string]any{"key": "val"}
	result := abbreviateContent(content, 100)
	if result != `{"key":"val"}` {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestAbbreviateContent_Truncated(t *testing.T) {
	content := map[string]any{"long_key": "this is a fairly long value that should exceed the limit"}
	result := abbreviateContent(content, 30)
	if len(result) > 30 {
		t.Errorf("result exceeds max length: len=%d, result=%s", len(result), result)
	}
	if !strings.HasSuffix(result, "...") {
		t.Errorf("truncated result should end with ...: %s", result)
	}
}

func TestAbbreviateContent_ZeroMaxLength(t *testing.T) {
	result := abbreviateContent(map[string]any{"a": "b"}, 0)
	if result != "" {
		t.Errorf("expected empty string for zero max length, got %s", result)
	}
}

func TestFormatEventOneLine_Message(t *testing.T) {
	event := messaging.Event{
		Type:           "m.room.message",
		Sender:         ref.MustParseUserID("@user:local"),
		OriginServerTS: 1771200000000,
		Content:        map[string]any{"msgtype": "m.text", "body": "hello world"},
	}
	result := formatEventOneLine(event)
	if !strings.Contains(result, "hello world") {
		t.Errorf("message body should appear in output: %s", result)
	}
	if !strings.Contains(result, "@user:local") {
		t.Errorf("sender should appear in output: %s", result)
	}
	if !strings.Contains(result, "m.room.message") {
		t.Errorf("event type should appear in output: %s", result)
	}
}

func TestFormatEventOneLine_StateEvent(t *testing.T) {
	stateKey := "machine/workstation"
	event := messaging.Event{
		Type:           "m.bureau.machine_key",
		Sender:         ref.MustParseUserID("@machine/workstation:local"),
		OriginServerTS: 1771200000000,
		StateKey:       &stateKey,
		Content:        map[string]any{"public_key": "age1abc123"},
	}
	result := formatEventOneLine(event)
	if !strings.Contains(result, "[machine/workstation]") {
		t.Errorf("state key should appear in brackets: %s", result)
	}
	if !strings.Contains(result, "m.bureau.machine_key") {
		t.Errorf("event type should appear in output: %s", result)
	}
}

func TestFormatEventOneLine_OtherEvent(t *testing.T) {
	event := messaging.Event{
		Type:           "m.custom.event",
		Sender:         ref.MustParseUserID("@bot:local"),
		OriginServerTS: 1771200000000,
		Content:        map[string]any{"action": "test"},
	}
	result := formatEventOneLine(event)
	if !strings.Contains(result, `"action":"test"`) {
		t.Errorf("JSON content should appear in output: %s", result)
	}
}
