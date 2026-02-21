// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

func TestParsePipelineRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        string
		wantRoom     string
		wantPipeline string
		wantServer   string
	}{
		{
			name:         "built-in workspace init pipeline",
			input:        "bureau/pipeline:dev-workspace-init",
			wantRoom:     "bureau/pipeline",
			wantPipeline: "dev-workspace-init",
		},
		{
			name:         "project-specific pipeline",
			input:        "iree/pipeline:build-release",
			wantRoom:     "iree/pipeline",
			wantPipeline: "build-release",
		},
		{
			name:         "federated without port",
			input:        "iree/pipeline@other.example:deploy",
			wantRoom:     "iree/pipeline",
			wantPipeline: "deploy",
			wantServer:   "other.example",
		},
		{
			name:         "federated with port",
			input:        "iree/pipeline@other.example:8448:migrate-v2",
			wantRoom:     "iree/pipeline",
			wantPipeline: "migrate-v2",
			wantServer:   "other.example:8448",
		},
		{
			name:         "single-segment room",
			input:        "pipeline:simple",
			wantRoom:     "pipeline",
			wantPipeline: "simple",
		},
		{
			name:         "deep room hierarchy",
			input:        "org/team/project/pipeline:deploy-canary",
			wantRoom:     "org/team/project/pipeline",
			wantPipeline: "deploy-canary",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ref, err := ParsePipelineRef(test.input)
			if err != nil {
				t.Fatalf("ParsePipelineRef(%q) error: %v", test.input, err)
			}
			if ref.Room != test.wantRoom {
				t.Errorf("Room = %q, want %q", ref.Room, test.wantRoom)
			}
			if ref.Pipeline != test.wantPipeline {
				t.Errorf("Pipeline = %q, want %q", ref.Pipeline, test.wantPipeline)
			}
			if ref.Server != test.wantServer {
				t.Errorf("Server = %q, want %q", ref.Server, test.wantServer)
			}
		})
	}
}

func TestParsePipelineRefErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{"empty string", ""},
		{"no colon", "bureau/pipeline"},
		{"empty pipeline name", "bureau/pipeline:"},
		{"empty room", ":dev-workspace-init"},
		{"empty room localpart with server", "@server:deploy"},
		{"empty server after at", "bureau/pipeline@:init"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParsePipelineRef(test.input)
			if err == nil {
				t.Errorf("ParsePipelineRef(%q) should have returned an error", test.input)
			}
		})
	}
}

func TestPipelineRefString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  PipelineRef
		want string
	}{
		{
			name: "local reference",
			ref:  PipelineRef{Room: "bureau/pipeline", Pipeline: "dev-workspace-init"},
			want: "bureau/pipeline:dev-workspace-init",
		},
		{
			name: "federated reference",
			ref:  PipelineRef{Room: "iree/pipeline", Pipeline: "deploy", Server: "other.example"},
			want: "iree/pipeline@other.example:deploy",
		},
		{
			name: "federated with port",
			ref:  PipelineRef{Room: "iree/pipeline", Pipeline: "migrate", Server: "other.example:8448"},
			want: "iree/pipeline@other.example:8448:migrate",
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

func TestPipelineRefRoundTrip(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"bureau/pipeline:dev-workspace-init",
		"iree/pipeline:build-release",
		"iree/pipeline@other.example:deploy",
		"iree/pipeline@other.example:8448:migrate-v2",
	}

	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			ref, err := ParsePipelineRef(input)
			if err != nil {
				t.Fatalf("ParsePipelineRef(%q) error: %v", input, err)
			}
			roundTripped := ref.String()
			if roundTripped != input {
				t.Errorf("round-trip failed: %q -> %q", input, roundTripped)
			}
		})
	}
}

func TestPipelineRefRoomAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		ref           PipelineRef
		defaultServer string
		want          string
	}{
		{
			name:          "local uses default server",
			ref:           PipelineRef{Room: "bureau/pipeline", Pipeline: "init"},
			defaultServer: "bureau.local",
			want:          "#bureau/pipeline:bureau.local",
		},
		{
			name:          "federated uses explicit server",
			ref:           PipelineRef{Room: "iree/pipeline", Pipeline: "deploy", Server: "other.example"},
			defaultServer: "bureau.local",
			want:          "#iree/pipeline:other.example",
		},
		{
			name:          "federated with port",
			ref:           PipelineRef{Room: "iree/pipeline", Pipeline: "migrate", Server: "other.example:8448"},
			defaultServer: "bureau.local",
			want:          "#iree/pipeline:other.example:8448",
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
