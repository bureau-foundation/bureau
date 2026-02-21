// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
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
			input:    "bureau/template:base",
			wantRoom: "bureau/template",
			wantTmpl: "base",
		},
		{
			name:     "project-specific template",
			input:    "iree/template:amdgpu-developer",
			wantRoom: "iree/template",
			wantTmpl: "amdgpu-developer",
		},
		{
			name:     "federated without port",
			input:    "iree/template@other.example:foo",
			wantRoom: "iree/template",
			wantTmpl: "foo",
			wantSrv:  "other.example",
		},
		{
			name:     "federated with port",
			input:    "iree/template@other.example:8448:versioned-agent",
			wantRoom: "iree/template",
			wantTmpl: "versioned-agent",
			wantSrv:  "other.example:8448",
		},
		{
			name:     "single-segment room",
			input:    "template:minimal",
			wantRoom: "template",
			wantTmpl: "minimal",
		},
		{
			name:     "deep room hierarchy",
			input:    "org/team/project/template:agent-v2",
			wantRoom: "org/team/project/template",
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
		{"no colon", "bureau/template"},
		{"empty template name", "bureau/template:"},
		{"empty room", ":base"},
		{"empty room localpart with server", "@server:base"},
		{"empty server after at", "bureau/template@:base"},
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
			ref:  TemplateRef{Room: "bureau/template", Template: "base"},
			want: "bureau/template:base",
		},
		{
			name: "federated reference",
			ref:  TemplateRef{Room: "iree/template", Template: "foo", Server: "other.example"},
			want: "iree/template@other.example:foo",
		},
		{
			name: "federated with port",
			ref:  TemplateRef{Room: "iree/template", Template: "agent", Server: "other.example:8448"},
			want: "iree/template@other.example:8448:agent",
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
		"bureau/template:base",
		"iree/template:amdgpu-developer",
		"iree/template@other.example:foo",
		"iree/template@other.example:8448:versioned-agent",
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
			ref:           TemplateRef{Room: "bureau/template", Template: "base"},
			defaultServer: "bureau.local",
			want:          "#bureau/template:bureau.local",
		},
		{
			name:          "federated uses explicit server",
			ref:           TemplateRef{Room: "iree/template", Template: "foo", Server: "other.example"},
			defaultServer: "bureau.local",
			want:          "#iree/template:other.example",
		},
		{
			name:          "federated with port",
			ref:           TemplateRef{Room: "iree/template", Template: "agent", Server: "other.example:8448"},
			defaultServer: "bureau.local",
			want:          "#iree/template:other.example:8448",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := test.ref.RoomAlias(ref.MustParseServerName(test.defaultServer)).String()
			if got != test.want {
				t.Errorf("RoomAlias(%q) = %q, want %q", test.defaultServer, got, test.want)
			}
		})
	}
}
