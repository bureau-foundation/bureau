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

	if len(templates) != 9 {
		t.Fatalf("expected 9 base templates, got %d", len(templates))
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
	if len(base.Inherits) != 0 {
		t.Errorf("base template should not inherit, got %v", base.Inherits)
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
	if len(networked.Inherits) != 1 {
		t.Fatalf("base-networked template should inherit from exactly one parent, got %v", networked.Inherits)
	}
	ref, err := schema.ParseTemplateRef(networked.Inherits[0])
	if err != nil {
		t.Fatalf("base-networked Inherits[0] %q is not a valid template reference: %v",
			networked.Inherits[0], err)
	}
	if ref.Template != "base" {
		t.Errorf("base-networked should inherit from 'base', got template name %q", ref.Template)
	}
	if ref.Room != "bureau/template" {
		t.Errorf("base-networked should inherit from 'bureau/template' room, got %q", ref.Room)
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

	// Verify the "agent-base" template.
	agentBase, ok := byName["agent-base"]
	if !ok {
		t.Fatal("missing 'agent-base' template")
	}
	if agentBase.Description == "" {
		t.Error("agent-base template has empty description")
	}

	// Should inherit from base-networked.
	if len(agentBase.Inherits) != 1 {
		t.Fatalf("agent-base template should inherit from exactly one parent, got %v", agentBase.Inherits)
	}
	agentRef, err := schema.ParseTemplateRef(agentBase.Inherits[0])
	if err != nil {
		t.Fatalf("agent-base Inherits[0] %q is not a valid template reference: %v",
			agentBase.Inherits[0], err)
	}
	if agentRef.Template != "base-networked" {
		t.Errorf("agent-base should inherit from 'base-networked', got template name %q", agentRef.Template)
	}

	// Should expose proxy socket, machine name, and server name via environment.
	for _, key := range []string{"BUREAU_PROXY_SOCKET", "BUREAU_MACHINE_NAME", "BUREAU_SERVER_NAME"} {
		if agentBase.EnvironmentVariables[key] == "" {
			t.Errorf("agent-base missing environment variable %q", key)
		}
	}

	// Verify the "service-base" template.
	serviceBase, ok := byName["service-base"]
	if !ok {
		t.Fatal("missing 'service-base' template")
	}
	if serviceBase.Description == "" {
		t.Error("service-base template has empty description")
	}

	// Should inherit from base-networked.
	if len(serviceBase.Inherits) != 1 {
		t.Fatalf("service-base should inherit from exactly one parent, got %v", serviceBase.Inherits)
	}
	serviceBaseRef, err := schema.ParseTemplateRef(serviceBase.Inherits[0])
	if err != nil {
		t.Fatalf("service-base Inherits[0] %q is not a valid template reference: %v",
			serviceBase.Inherits[0], err)
	}
	if serviceBaseRef.Template != "base-networked" {
		t.Errorf("service-base should inherit from 'base-networked', got %q", serviceBaseRef.Template)
	}

	// Should expose the same bootstrap env vars as agent-base.
	for _, key := range []string{"BUREAU_PROXY_SOCKET", "BUREAU_MACHINE_NAME", "BUREAU_SERVER_NAME"} {
		if serviceBase.EnvironmentVariables[key] == "" {
			t.Errorf("service-base missing environment variable %q", key)
		}
	}

	// Verify each individual service template inherits from service-base
	// and has a bare binary command name.
	serviceTemplates := []struct {
		name    string
		command string
	}{
		{"ticket-service", "bureau-ticket-service"},
		{"fleet-controller", "bureau-fleet-controller"},
		{"artifact-service", "bureau-artifact-service"},
		{"agent-service", "bureau-agent-service"},
		{"telemetry-service", "bureau-telemetry-service"},
	}

	for _, serviceTemplate := range serviceTemplates {
		template, ok := byName[serviceTemplate.name]
		if !ok {
			t.Fatalf("missing %q template", serviceTemplate.name)
		}
		if template.Description == "" {
			t.Errorf("%s template has empty description", serviceTemplate.name)
		}
		if len(template.Inherits) != 1 {
			t.Fatalf("%s should inherit from exactly one parent, got %v", serviceTemplate.name, template.Inherits)
		}
		templateRef, err := schema.ParseTemplateRef(template.Inherits[0])
		if err != nil {
			t.Fatalf("%s Inherits[0] %q is not a valid template reference: %v",
				serviceTemplate.name, template.Inherits[0], err)
		}
		if templateRef.Template != "service-base" {
			t.Errorf("%s should inherit from 'service-base', got %q", serviceTemplate.name, templateRef.Template)
		}
		if len(template.Command) != 1 || template.Command[0] != serviceTemplate.command {
			t.Errorf("%s command = %v, want [%q]", serviceTemplate.name, template.Command, serviceTemplate.command)
		}
	}
}
