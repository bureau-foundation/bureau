// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// TestTemplateRefConstruction verifies that namespace + template name
// produces valid, parseable template references. This is the ref
// construction logic used by publishFlakeTemplates.
func TestTemplateRefConstruction(t *testing.T) {
	t.Parallel()

	server := ref.MustParseServerName("bureau.local")
	namespace, err := ref.NewNamespace(server, "iree")
	if err != nil {
		t.Fatalf("NewNamespace: %v", err)
	}

	tests := []struct {
		name     string
		expected string
	}{
		{"claude-dev", "iree/template:claude-dev"},
		{"base-networked", "iree/template:base-networked"},
		{"hf-model-ingest", "iree/template:hf-model-ingest"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			refString := namespace.TemplateRoomAliasLocalpart() + ":" + testCase.name
			if refString != testCase.expected {
				t.Fatalf("ref string = %q, want %q", refString, testCase.expected)
			}

			templateRef, err := schema.ParseTemplateRef(refString)
			if err != nil {
				t.Fatalf("ParseTemplateRef(%q): %v", refString, err)
			}
			if templateRef.Template != testCase.name {
				t.Errorf("template name = %q, want %q", templateRef.Template, testCase.name)
			}
		})
	}
}

// TestPipelineRefConstruction verifies that namespace + pipeline name
// produces valid, parseable pipeline references.
func TestPipelineRefConstruction(t *testing.T) {
	t.Parallel()

	server := ref.MustParseServerName("bureau.local")
	namespace, err := ref.NewNamespace(server, "bureau")
	if err != nil {
		t.Fatalf("NewNamespace: %v", err)
	}

	tests := []struct {
		name     string
		expected string
	}{
		{"environment-compose", "bureau/pipeline:environment-compose"},
		{"model-ingest", "bureau/pipeline:model-ingest"},
		{"cloudflare-tunnel-setup", "bureau/pipeline:cloudflare-tunnel-setup"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			refString := namespace.PipelineRoomAliasLocalpart() + ":" + testCase.name
			if refString != testCase.expected {
				t.Fatalf("ref string = %q, want %q", refString, testCase.expected)
			}

			pipelineRef, err := schema.ParsePipelineRef(refString)
			if err != nil {
				t.Fatalf("ParsePipelineRef(%q): %v", refString, err)
			}
			if pipelineRef.Pipeline != testCase.name {
				t.Errorf("pipeline name = %q, want %q", pipelineRef.Pipeline, testCase.name)
			}
		})
	}
}

// TestTemplateRefRoomAlias verifies that the constructed template ref
// resolves to the correct room alias for a given server.
func TestTemplateRefRoomAlias(t *testing.T) {
	t.Parallel()

	server := ref.MustParseServerName("bureau.local")
	namespace, err := ref.NewNamespace(server, "myorg")
	if err != nil {
		t.Fatalf("NewNamespace: %v", err)
	}

	refString := namespace.TemplateRoomAliasLocalpart() + ":agent-base"
	templateRef, err := schema.ParseTemplateRef(refString)
	if err != nil {
		t.Fatalf("ParseTemplateRef: %v", err)
	}

	roomAlias := templateRef.RoomAlias(server)
	expected := ref.MustParseRoomAlias("#myorg/template:bureau.local")
	if roomAlias != expected {
		t.Errorf("room alias = %q, want %q", roomAlias, expected)
	}
}
