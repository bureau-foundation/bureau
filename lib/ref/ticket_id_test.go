// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ref

import (
	"encoding/json"
	"testing"
)

func TestParseTicketID(t *testing.T) {
	t.Parallel()

	valid := []struct {
		input  string
		prefix string
	}{
		{"pip-a3f9", "pip"},
		{"tkt-12ab", "tkt"},
		{"tkt-abcdef123456", "tkt"},
		{"wt-beef", "wt"},
		{"custom-1234", "custom"},
		{"x-y", "x"},
	}
	for _, test := range valid {
		ticketID, err := ParseTicketID(test.input)
		if err != nil {
			t.Errorf("ParseTicketID(%q) unexpected error: %v", test.input, err)
			continue
		}
		if ticketID.String() != test.input {
			t.Errorf("ParseTicketID(%q).String() = %q", test.input, ticketID.String())
		}
		if ticketID.IsZero() {
			t.Errorf("ParseTicketID(%q).IsZero() = true", test.input)
		}
		if ticketID.Prefix() != test.prefix {
			t.Errorf("ParseTicketID(%q).Prefix() = %q, want %q", test.input, ticketID.Prefix(), test.prefix)
		}
	}

	invalid := []string{
		"",
		"nope",
		"-suffix",
		"prefix-",
		"!room:server",
		"$event123",
		"@user:server",
	}
	for _, input := range invalid {
		_, err := ParseTicketID(input)
		if err == nil {
			t.Errorf("ParseTicketID(%q) expected error, got nil", input)
		}
	}
}

func TestTicketIDZeroValue(t *testing.T) {
	t.Parallel()

	var zero TicketID
	if !zero.IsZero() {
		t.Error("zero TicketID.IsZero() = false")
	}
	if zero.String() != "" {
		t.Errorf("zero TicketID.String() = %q", zero.String())
	}
	if zero.Prefix() != "" {
		t.Errorf("zero TicketID.Prefix() = %q", zero.Prefix())
	}
}

func TestTicketIDMarshalJSON(t *testing.T) {
	t.Parallel()

	type wrapper struct {
		Ticket TicketID `json:"ticket"`
	}

	ticketID := MustParseTicketID("pip-a3f9")
	data, err := json.Marshal(wrapper{Ticket: ticketID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"ticket":"pip-a3f9"}`
	if string(data) != want {
		t.Errorf("marshal = %s, want %s", data, want)
	}

	var roundTripped wrapper
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundTripped.Ticket != ticketID {
		t.Errorf("round-trip = %v, want %v", roundTripped.Ticket, ticketID)
	}
}

func TestTicketIDUnmarshalEmpty(t *testing.T) {
	t.Parallel()

	type wrapper struct {
		Ticket TicketID `json:"ticket"`
	}

	var result wrapper
	if err := json.Unmarshal([]byte(`{"ticket":""}`), &result); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if !result.Ticket.IsZero() {
		t.Errorf("empty string should unmarshal to zero value, got %v", result.Ticket)
	}
}

func TestMustParseTicketIDPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error("MustParseTicketID with invalid input should panic")
		}
	}()
	MustParseTicketID("")
}
