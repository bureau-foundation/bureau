// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

import (
	"encoding/json"
	"testing"
)

func TestParseEventID(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		// Valid: room version 4+ hash-based IDs.
		{"$abc123xyz", false},
		{"$VGhpcyBpcyBhIHRlc3Q", false},
		// Valid: legacy format with server.
		{"$something:server.local", false},
		// Invalid: empty.
		{"", true},
		// Invalid: wrong sigil.
		{"!abc123", true},
		{"@abc123", true},
		{"#abc123", true},
		{"abc123", true},
		// Invalid: only the prefix.
		{"$", true},
	}

	for _, test := range tests {
		_, err := ParseEventID(test.input)
		if (err != nil) != test.wantErr {
			t.Errorf("ParseEventID(%q): err=%v, wantErr=%v", test.input, err, test.wantErr)
		}
	}
}

func TestEventIDRoundTrip(t *testing.T) {
	original := MustParseEventID("$abc123xyz")

	if original.String() != "$abc123xyz" {
		t.Errorf("String() = %q, want %q", original.String(), "$abc123xyz")
	}
	if original.IsZero() {
		t.Error("IsZero() = true for valid EventID")
	}

	// JSON round-trip.
	type wrapper struct {
		EventID EventID `json:"event_id"`
	}
	data, err := json.Marshal(wrapper{EventID: original})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"event_id":"$abc123xyz"}`
	if string(data) != want {
		t.Errorf("Marshal = %s, want %s", data, want)
	}

	var decoded wrapper
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.EventID != original {
		t.Errorf("round-trip: got %q, want %q", decoded.EventID, original)
	}
}

func TestEventIDZeroValue(t *testing.T) {
	var zero EventID
	if !zero.IsZero() {
		t.Error("zero value should be IsZero()")
	}
	if zero.String() != "" {
		t.Errorf("zero String() = %q, want empty", zero.String())
	}

	// Unmarshal empty string produces zero value.
	type wrapper struct {
		EventID EventID `json:"event_id"`
	}
	var decoded wrapper
	if err := json.Unmarshal([]byte(`{"event_id":""}`), &decoded); err != nil {
		t.Fatalf("Unmarshal empty: %v", err)
	}
	if !decoded.EventID.IsZero() {
		t.Error("empty string should unmarshal to zero value")
	}
}

func TestMustParseEventIDPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustParseEventID should panic on invalid input")
		}
	}()
	MustParseEventID("")
}
