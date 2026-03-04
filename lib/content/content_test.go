// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package content

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestPipelines(t *testing.T) {
	t.Parallel()

	pipelines, err := Pipelines()
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}
	if len(pipelines) == 0 {
		t.Fatal("Pipelines returned no pipelines")
	}

	seen := make(map[string]bool, len(pipelines))
	for _, pipeline := range pipelines {
		if seen[pipeline.Name] {
			t.Errorf("duplicate pipeline name: %s", pipeline.Name)
		}
		seen[pipeline.Name] = true

		if pipeline.Content.Description == "" {
			t.Errorf("pipeline %q has empty description", pipeline.Name)
		}

		if len(pipeline.SourceHash) != sha256.Size*2 {
			t.Errorf("pipeline %q SourceHash length = %d, want %d", pipeline.Name, len(pipeline.SourceHash), sha256.Size*2)
		}
		if _, err := hex.DecodeString(pipeline.SourceHash); err != nil {
			t.Errorf("pipeline %q SourceHash is not valid hex: %v", pipeline.Name, err)
		}
	}
}

func TestPipelinesSourceHashStable(t *testing.T) {
	t.Parallel()

	first, err := Pipelines()
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := Pipelines()
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("count changed: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].SourceHash != second[i].SourceHash {
			t.Errorf("pipeline %q hash changed between calls: %s vs %s",
				first[i].Name, first[i].SourceHash, second[i].SourceHash)
		}
	}
}

func TestTemplates(t *testing.T) {
	t.Parallel()

	const prefix = "test/template"
	templates, err := Templates(prefix)
	if err != nil {
		t.Fatalf("Templates: %v", err)
	}
	if len(templates) == 0 {
		t.Fatal("Templates returned no templates")
	}

	byName := make(map[string]Template, len(templates))
	for _, template := range templates {
		if byName[template.Name].Name != "" {
			t.Errorf("duplicate template name: %s", template.Name)
		}
		byName[template.Name] = template

		if template.Content.Description == "" {
			t.Errorf("template %q has empty description", template.Name)
		}

		if len(template.SourceHash) != sha256.Size*2 {
			t.Errorf("template %q SourceHash length = %d, want %d", template.Name, len(template.SourceHash), sha256.Size*2)
		}
		if _, err := hex.DecodeString(template.SourceHash); err != nil {
			t.Errorf("template %q SourceHash is not valid hex: %v", template.Name, err)
		}
	}

	// Inheritance references must use the substituted prefix and point
	// to templates that exist in the set. Checked after building the
	// full map so iteration order doesn't matter.
	for _, template := range templates {
		for _, parent := range template.Content.Inherits {
			if !strings.HasPrefix(parent, prefix+":") {
				t.Errorf("template %q inherits %q: expected prefix %q:", template.Name, parent, prefix)
			}
			parts := strings.SplitN(parent, ":", 2)
			if len(parts) == 2 {
				if _, exists := byName[parts[1]]; !exists {
					t.Errorf("template %q inherits %q, but %q not found", template.Name, parent, parts[1])
				}
			}
		}
	}

	// "base" must be the root (no inheritance).
	if base, ok := byName["base"]; ok {
		if len(base.Content.Inherits) != 0 {
			t.Errorf("base should not inherit, got %v", base.Content.Inherits)
		}
	} else {
		t.Error("missing root 'base' template")
	}
}

func TestTemplatesSourceHashStable(t *testing.T) {
	t.Parallel()

	first, err := Templates("bureau/template")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := Templates("bureau/template")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("count changed: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].SourceHash != second[i].SourceHash {
			t.Errorf("template %q hash changed between calls: %s vs %s",
				first[i].Name, first[i].SourceHash, second[i].SourceHash)
		}
	}
}

func TestTemplatesVariableSubstitution(t *testing.T) {
	t.Parallel()

	prefixA := "alpha/template"
	prefixB := "beta/template"

	templatesA, err := Templates(prefixA)
	if err != nil {
		t.Fatalf("Templates(%q): %v", prefixA, err)
	}
	templatesB, err := Templates(prefixB)
	if err != nil {
		t.Fatalf("Templates(%q): %v", prefixB, err)
	}

	if len(templatesA) != len(templatesB) {
		t.Fatalf("different counts: %d vs %d", len(templatesA), len(templatesB))
	}

	for i := range templatesA {
		// Source hashes are computed before substitution — must match.
		if templatesA[i].SourceHash != templatesB[i].SourceHash {
			t.Errorf("template %q: source hash differs between prefixes", templatesA[i].Name)
		}

		// Inherits references must use the respective prefix.
		for _, parent := range templatesA[i].Content.Inherits {
			if !strings.HasPrefix(parent, prefixA+":") {
				t.Errorf("template %q with prefix %q: inherits %q uses wrong prefix",
					templatesA[i].Name, prefixA, parent)
			}
		}
		for _, parent := range templatesB[i].Content.Inherits {
			if !strings.HasPrefix(parent, prefixB+":") {
				t.Errorf("template %q with prefix %q: inherits %q uses wrong prefix",
					templatesB[i].Name, prefixB, parent)
			}
		}
	}
}
