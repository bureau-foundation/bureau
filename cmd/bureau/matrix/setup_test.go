// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestBaseTemplates(t *testing.T) {
	t.Parallel()

	templates := baseTemplates()

	if len(templates) != 2 {
		t.Fatalf("expected 2 base templates, got %d", len(templates))
	}

	// Find templates by name.
	byName := make(map[string]schema.TemplateContent, len(templates))
	for _, template := range templates {
		if template.name == "" {
			t.Fatal("template has empty name")
		}
		byName[template.name] = template.content
	}

	// Verify the "base" template.
	base, ok := byName["base"]
	if !ok {
		t.Fatal("missing 'base' template")
	}
	if base.Description == "" {
		t.Error("base template has empty description")
	}
	if base.Inherits != "" {
		t.Errorf("base template should not inherit, got %q", base.Inherits)
	}
	if base.Namespaces == nil {
		t.Fatal("base template missing namespaces")
	}
	if !base.Namespaces.PID || !base.Namespaces.Net || !base.Namespaces.IPC || !base.Namespaces.UTS {
		t.Errorf("base template should unshare PID, Net, IPC, UTS; got %+v", base.Namespaces)
	}
	if base.Security == nil {
		t.Fatal("base template missing security")
	}
	if !base.Security.NewSession || !base.Security.DieWithParent || !base.Security.NoNewPrivs {
		t.Errorf("base template should enable all security options; got %+v", base.Security)
	}
	// Should have /tmp tmpfs.
	hasTmpfs := false
	for _, mount := range base.Filesystem {
		if mount.Dest == "/tmp" && mount.Type == "tmpfs" {
			hasTmpfs = true
		}
	}
	if !hasTmpfs {
		t.Error("base template missing /tmp tmpfs mount")
	}
	// Should create /run/bureau directory.
	hasBureauDir := false
	for _, dir := range base.CreateDirs {
		if dir == "/run/bureau" {
			hasBureauDir = true
		}
	}
	if !hasBureauDir {
		t.Error("base template missing /run/bureau in CreateDirs")
	}

	// Verify the "base-networked" template.
	networked, ok := byName["base-networked"]
	if !ok {
		t.Fatal("missing 'base-networked' template")
	}
	if networked.Description == "" {
		t.Error("base-networked template has empty description")
	}

	// Should inherit from base via a valid template reference.
	if networked.Inherits == "" {
		t.Fatal("base-networked template should inherit from base")
	}
	ref, err := schema.ParseTemplateRef(networked.Inherits)
	if err != nil {
		t.Fatalf("base-networked Inherits %q is not a valid template reference: %v",
			networked.Inherits, err)
	}
	if ref.Template != "base" {
		t.Errorf("base-networked should inherit from 'base', got template name %q", ref.Template)
	}
	if ref.Room != "bureau/templates" {
		t.Errorf("base-networked should inherit from 'bureau/templates' room, got %q", ref.Room)
	}

	// Should disable network namespace.
	if networked.Namespaces == nil {
		t.Fatal("base-networked template missing namespaces")
	}
	if networked.Namespaces.Net {
		t.Error("base-networked should have Net=false (disable network namespace)")
	}
	// Other namespaces should still be isolated.
	if !networked.Namespaces.PID || !networked.Namespaces.IPC || !networked.Namespaces.UTS {
		t.Errorf("base-networked should unshare PID, IPC, UTS; got %+v", networked.Namespaces)
	}
}
