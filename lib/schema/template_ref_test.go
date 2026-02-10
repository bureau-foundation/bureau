// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"testing"
)

func TestParseTemplateRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		wantRoom string
		wantTmpl string
		wantSrv  string
	}{
		{
			name:     "built-in base template",
			input:    "bureau/templates:base",
			wantRoom: "bureau/templates",
			wantTmpl: "base",
		},
		{
			name:     "project-specific template",
			input:    "iree/templates:amdgpu-developer",
			wantRoom: "iree/templates",
			wantTmpl: "amdgpu-developer",
		},
		{
			name:     "federated without port",
			input:    "iree/templates@other.example:foo",
			wantRoom: "iree/templates",
			wantTmpl: "foo",
			wantSrv:  "other.example",
		},
		{
			name:     "federated with port",
			input:    "iree/templates@other.example:8448:versioned-agent",
			wantRoom: "iree/templates",
			wantTmpl: "versioned-agent",
			wantSrv:  "other.example:8448",
		},
		{
			name:     "single-segment room",
			input:    "templates:minimal",
			wantRoom: "templates",
			wantTmpl: "minimal",
		},
		{
			name:     "deep room hierarchy",
			input:    "org/team/project/templates:agent-v2",
			wantRoom: "org/team/project/templates",
			wantTmpl: "agent-v2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ref, err := ParseTemplateRef(test.input)
			if err != nil {
				t.Fatalf("ParseTemplateRef(%q) error: %v", test.input, err)
			}
			if ref.Room != test.wantRoom {
				t.Errorf("Room = %q, want %q", ref.Room, test.wantRoom)
			}
			if ref.Template != test.wantTmpl {
				t.Errorf("Template = %q, want %q", ref.Template, test.wantTmpl)
			}
			if ref.Server != test.wantSrv {
				t.Errorf("Server = %q, want %q", ref.Server, test.wantSrv)
			}
		})
	}
}

func TestParseTemplateRefErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{"empty string", ""},
		{"no colon", "bureau/templates"},
		{"empty template name", "bureau/templates:"},
		{"empty room", ":base"},
		{"empty room localpart with server", "@server:base"},
		{"empty server after at", "bureau/templates@:base"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseTemplateRef(test.input)
			if err == nil {
				t.Errorf("ParseTemplateRef(%q) should have returned an error", test.input)
			}
		})
	}
}

func TestTemplateRefString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  TemplateRef
		want string
	}{
		{
			name: "local reference",
			ref:  TemplateRef{Room: "bureau/templates", Template: "base"},
			want: "bureau/templates:base",
		},
		{
			name: "federated reference",
			ref:  TemplateRef{Room: "iree/templates", Template: "foo", Server: "other.example"},
			want: "iree/templates@other.example:foo",
		},
		{
			name: "federated with port",
			ref:  TemplateRef{Room: "iree/templates", Template: "agent", Server: "other.example:8448"},
			want: "iree/templates@other.example:8448:agent",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := test.ref.String()
			if got != test.want {
				t.Errorf("String() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTemplateRefRoundTrip(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"bureau/templates:base",
		"iree/templates:amdgpu-developer",
		"iree/templates@other.example:foo",
		"iree/templates@other.example:8448:versioned-agent",
	}

	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			ref, err := ParseTemplateRef(input)
			if err != nil {
				t.Fatalf("ParseTemplateRef(%q) error: %v", input, err)
			}
			roundTripped := ref.String()
			if roundTripped != input {
				t.Errorf("round-trip failed: %q -> %q", input, roundTripped)
			}
		})
	}
}

func TestTemplateRefRoomAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		ref           TemplateRef
		defaultServer string
		want          string
	}{
		{
			name:          "local uses default server",
			ref:           TemplateRef{Room: "bureau/templates", Template: "base"},
			defaultServer: "bureau.local",
			want:          "#bureau/templates:bureau.local",
		},
		{
			name:          "federated uses explicit server",
			ref:           TemplateRef{Room: "iree/templates", Template: "foo", Server: "other.example"},
			defaultServer: "bureau.local",
			want:          "#iree/templates:other.example",
		},
		{
			name:          "federated with port",
			ref:           TemplateRef{Room: "iree/templates", Template: "agent", Server: "other.example:8448"},
			defaultServer: "bureau.local",
			want:          "#iree/templates:other.example:8448",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := test.ref.RoomAlias(test.defaultServer)
			if got != test.want {
				t.Errorf("RoomAlias(%q) = %q, want %q", test.defaultServer, got, test.want)
			}
		})
	}
}
