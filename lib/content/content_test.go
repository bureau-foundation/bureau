// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package content

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestPipelines(t *testing.T) {
	t.Parallel()

	pipelines, err := Pipelines()
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	if len(pipelines) == 0 {
		t.Fatal("expected at least one embedded pipeline")
	}

	// Index pipelines by name for targeted verification.
	byName := make(map[string]Pipeline, len(pipelines))
	for _, p := range pipelines {
		byName[p.Name] = p
	}

	// Verify all four dev pipelines are present.
	for _, name := range []string{"dev-workspace-init", "dev-workspace-deinit", "dev-worktree-init", "dev-worktree-deinit"} {
		if _, exists := byName[name]; !exists {
			names := make([]string, 0, len(pipelines))
			for _, p := range pipelines {
				names = append(names, p.Name)
			}
			t.Fatalf("%s not found in pipelines: %v", name, names)
		}
	}

	verifyDevWorkspaceInit(t, byName["dev-workspace-init"])
	verifyDevWorkspaceDeinit(t, byName["dev-workspace-deinit"])
	verifyDevWorktreeInit(t, byName["dev-worktree-init"])
	verifyDevWorktreeDeinit(t, byName["dev-worktree-deinit"])
}

func verifyDevWorkspaceInit(t *testing.T, p Pipeline) {
	t.Helper()

	if p.Content.Description == "" {
		t.Error("Description is empty")
	}

	// Verify required variables are declared.
	requiredVariables := []string{"REPOSITORY", "PROJECT", "WORKSPACE_ROOM_ID", "MACHINE", "WORKSPACE_PATH"}
	for _, name := range requiredVariables {
		variable, exists := p.Content.Variables[name]
		if !exists {
			t.Errorf("missing required variable declaration: %s", name)
			continue
		}
		if !variable.Required {
			t.Errorf("variable %s should be marked required", name)
		}
	}

	// Verify step structure.
	if len(p.Content.Steps) < 3 {
		t.Fatalf("expected at least 3 steps, got %d", len(p.Content.Steps))
	}

	// First step should create the project directory.
	if p.Content.Steps[0].Name != "create-project-directory" {
		t.Errorf("first step name = %q, want %q", p.Content.Steps[0].Name, "create-project-directory")
	}

	// Last step should publish workspace active state.
	lastStep := p.Content.Steps[len(p.Content.Steps)-1]
	if lastStep.Name != "publish-active" {
		t.Errorf("last step name = %q, want %q", lastStep.Name, "publish-active")
	}
	if lastStep.Publish == nil {
		t.Error("last step should be a publish step")
	} else if lastStep.Publish.EventType != "m.bureau.workspace" {
		t.Errorf("last step publish event_type = %q", lastStep.Publish.EventType)
	}

	// on_failure should publish workspace failed state.
	if len(p.Content.OnFailure) != 1 {
		t.Fatalf("expected 1 on_failure step, got %d", len(p.Content.OnFailure))
	}
	initOnFailure := p.Content.OnFailure[0]
	if initOnFailure.Publish == nil {
		t.Error("on_failure step should be a publish step")
	} else if initOnFailure.Publish.EventType != "m.bureau.workspace" {
		t.Errorf("on_failure publish event_type = %q, want %q", initOnFailure.Publish.EventType, "m.bureau.workspace")
	}

	// Log should point to the workspace room.
	if p.Content.Log == nil {
		t.Error("Log should be configured")
	} else if p.Content.Log.Room == "" {
		t.Error("Log.Room should be set")
	}

	// SourceHash should be a valid hex-encoded SHA-256.
	if len(p.SourceHash) != sha256.Size*2 {
		t.Errorf("SourceHash length = %d, want %d", len(p.SourceHash), sha256.Size*2)
	}
	if _, err := hex.DecodeString(p.SourceHash); err != nil {
		t.Errorf("SourceHash is not valid hex: %v", err)
	}
}

func verifyDevWorkspaceDeinit(t *testing.T, p Pipeline) {
	t.Helper()

	if p.Content.Description == "" {
		t.Error("Description is empty")
	}

	// Verify required variables.
	requiredVariables := []string{"PROJECT", "WORKSPACE_ROOM_ID", "MACHINE"}
	for _, name := range requiredVariables {
		variable, exists := p.Content.Variables[name]
		if !exists {
			t.Errorf("missing required variable declaration: %s", name)
			continue
		}
		if !variable.Required {
			t.Errorf("variable %s should be marked required", name)
		}
	}

	// EVENT_teardown_mode should have a default of "archive". This variable
	// comes from the trigger event (workspace state with status "teardown"),
	// but the declaration provides a fallback for manual execution.
	modeVariable, exists := p.Content.Variables["EVENT_teardown_mode"]
	if !exists {
		t.Error("missing EVENT_teardown_mode variable declaration")
	} else if modeVariable.Default != "archive" {
		t.Errorf("EVENT_teardown_mode default = %q, want %q", modeVariable.Default, "archive")
	}

	// Should have enough steps for assert + validate + check + cleanup + publish (x2).
	if len(p.Content.Steps) < 6 {
		t.Fatalf("expected at least 6 steps, got %d", len(p.Content.Steps))
	}

	// First step should be the staleness guard (assert_state).
	firstStep := p.Content.Steps[0]
	if firstStep.Name != "assert-still-teardown" {
		t.Errorf("first step name = %q, want %q", firstStep.Name, "assert-still-teardown")
	}
	if firstStep.AssertState == nil {
		t.Error("first step should be an assert_state step")
	} else {
		if firstStep.AssertState.EventType != "m.bureau.workspace" {
			t.Errorf("assert_state event_type = %q, want %q", firstStep.AssertState.EventType, "m.bureau.workspace")
		}
		if firstStep.AssertState.Equals != "teardown" {
			t.Errorf("assert_state equals = %q, want %q", firstStep.AssertState.Equals, "teardown")
		}
		if firstStep.AssertState.OnMismatch != "abort" {
			t.Errorf("assert_state on_mismatch = %q, want %q", firstStep.AssertState.OnMismatch, "abort")
		}
	}

	// Second step should validate the MODE variable.
	if p.Content.Steps[1].Name != "validate-mode" {
		t.Errorf("second step name = %q, want %q", p.Content.Steps[1].Name, "validate-mode")
	}

	// Last two steps should be conditional publish steps (archive/delete).
	lastStep := p.Content.Steps[len(p.Content.Steps)-1]
	secondLastStep := p.Content.Steps[len(p.Content.Steps)-2]
	if secondLastStep.Name != "publish-archived" {
		t.Errorf("second-to-last step name = %q, want %q", secondLastStep.Name, "publish-archived")
	}
	if lastStep.Name != "publish-removed" {
		t.Errorf("last step name = %q, want %q", lastStep.Name, "publish-removed")
	}
	// Both should publish to the unified workspace event type.
	if secondLastStep.Publish == nil {
		t.Error("publish-archived should be a publish step")
	} else if secondLastStep.Publish.EventType != "m.bureau.workspace" {
		t.Errorf("publish-archived event_type = %q, want %q",
			secondLastStep.Publish.EventType, "m.bureau.workspace")
	}
	if lastStep.Publish == nil {
		t.Error("publish-removed should be a publish step")
	} else if lastStep.Publish.EventType != "m.bureau.workspace" {
		t.Errorf("publish-removed event_type = %q, want %q",
			lastStep.Publish.EventType, "m.bureau.workspace")
	}

	// on_failure should publish workspace failed state.
	if len(p.Content.OnFailure) != 1 {
		t.Fatalf("expected 1 on_failure step, got %d", len(p.Content.OnFailure))
	}
	onFailure := p.Content.OnFailure[0]
	if onFailure.Publish == nil {
		t.Error("on_failure step should be a publish step")
	} else if onFailure.Publish.EventType != "m.bureau.workspace" {
		t.Errorf("on_failure publish event_type = %q, want %q", onFailure.Publish.EventType, "m.bureau.workspace")
	}

	// Log should point to the workspace room.
	if p.Content.Log == nil {
		t.Error("Log should be configured")
	} else if p.Content.Log.Room == "" {
		t.Error("Log.Room should be set")
	}

	// SourceHash should be a valid hex-encoded SHA-256.
	if len(p.SourceHash) != sha256.Size*2 {
		t.Errorf("SourceHash length = %d, want %d", len(p.SourceHash), sha256.Size*2)
	}
	if _, err := hex.DecodeString(p.SourceHash); err != nil {
		t.Errorf("SourceHash is not valid hex: %v", err)
	}
}

func TestPipelinesSourceHashStable(t *testing.T) {
	t.Parallel()

	// Calling Pipelines twice should produce identical hashes.
	first, err := Pipelines()
	if err != nil {
		t.Fatalf("first Pipelines call: %v", err)
	}
	second, err := Pipelines()
	if err != nil {
		t.Fatalf("second Pipelines call: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("pipeline count changed: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].SourceHash != second[i].SourceHash {
			t.Errorf("pipeline %q hash changed between calls: %s vs %s",
				first[i].Name, first[i].SourceHash, second[i].SourceHash)
		}
	}
}

func verifyDevWorktreeInit(t *testing.T, p Pipeline) {
	t.Helper()

	if p.Content.Description == "" {
		t.Error("Description is empty")
	}

	// Verify required variables.
	for _, name := range []string{"PROJECT", "WORKTREE_PATH", "WORKSPACE_ROOM_ID", "MACHINE"} {
		variable, exists := p.Content.Variables[name]
		if !exists {
			t.Errorf("missing variable declaration: %s", name)
			continue
		}
		if !variable.Required {
			t.Errorf("variable %s should be marked required", name)
		}
	}

	// Last step should publish worktree active state.
	lastStep := p.Content.Steps[len(p.Content.Steps)-1]
	if lastStep.Name != "publish-active" {
		t.Errorf("last step name = %q, want %q", lastStep.Name, "publish-active")
	}
	if lastStep.Publish == nil {
		t.Error("last step should be a publish step")
	} else {
		if lastStep.Publish.EventType != "m.bureau.worktree" {
			t.Errorf("publish event_type = %q, want %q", lastStep.Publish.EventType, "m.bureau.worktree")
		}
		if lastStep.Publish.StateKey != "${WORKTREE_PATH}" {
			t.Errorf("publish state_key = %q, want %q", lastStep.Publish.StateKey, "${WORKTREE_PATH}")
		}
	}

	// on_failure should publish worktree failed state.
	if len(p.Content.OnFailure) != 1 {
		t.Fatalf("expected 1 on_failure step, got %d", len(p.Content.OnFailure))
	}
	if p.Content.OnFailure[0].Publish == nil {
		t.Error("on_failure step should be a publish step")
	} else if p.Content.OnFailure[0].Publish.EventType != "m.bureau.worktree" {
		t.Errorf("on_failure publish event_type = %q", p.Content.OnFailure[0].Publish.EventType)
	}
}

func verifyDevWorktreeDeinit(t *testing.T, p Pipeline) {
	t.Helper()

	if p.Content.Description == "" {
		t.Error("Description is empty")
	}

	// First step should be the staleness guard (assert_state).
	firstStep := p.Content.Steps[0]
	if firstStep.Name != "assert-still-removing" {
		t.Errorf("first step name = %q, want %q", firstStep.Name, "assert-still-removing")
	}
	if firstStep.AssertState == nil {
		t.Error("first step should be an assert_state step")
	} else {
		if firstStep.AssertState.EventType != "m.bureau.worktree" {
			t.Errorf("assert_state event_type = %q, want %q", firstStep.AssertState.EventType, "m.bureau.worktree")
		}
		if firstStep.AssertState.Equals != "removing" {
			t.Errorf("assert_state equals = %q, want %q", firstStep.AssertState.Equals, "removing")
		}
		if firstStep.AssertState.OnMismatch != "abort" {
			t.Errorf("assert_state on_mismatch = %q, want %q", firstStep.AssertState.OnMismatch, "abort")
		}
	}

	// Last two steps should be conditional publish steps (archived/removed).
	lastStep := p.Content.Steps[len(p.Content.Steps)-1]
	secondLastStep := p.Content.Steps[len(p.Content.Steps)-2]
	if secondLastStep.Name != "publish-archived" {
		t.Errorf("second-to-last step name = %q, want %q", secondLastStep.Name, "publish-archived")
	}
	if lastStep.Name != "publish-removed" {
		t.Errorf("last step name = %q, want %q", lastStep.Name, "publish-removed")
	}
	if secondLastStep.Publish != nil && secondLastStep.Publish.EventType != "m.bureau.worktree" {
		t.Errorf("publish-archived event_type = %q", secondLastStep.Publish.EventType)
	}
	if lastStep.Publish != nil && lastStep.Publish.EventType != "m.bureau.worktree" {
		t.Errorf("publish-removed event_type = %q", lastStep.Publish.EventType)
	}

	// on_failure should publish worktree failed state.
	if len(p.Content.OnFailure) != 1 {
		t.Fatalf("expected 1 on_failure step, got %d", len(p.Content.OnFailure))
	}
	if p.Content.OnFailure[0].Publish == nil {
		t.Error("on_failure step should be a publish step")
	} else if p.Content.OnFailure[0].Publish.EventType != "m.bureau.worktree" {
		t.Errorf("on_failure publish event_type = %q", p.Content.OnFailure[0].Publish.EventType)
	}
}

func TestPipelinesNamesUnique(t *testing.T) {
	t.Parallel()

	pipelines, err := Pipelines()
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	seen := make(map[string]bool, len(pipelines))
	for _, p := range pipelines {
		if seen[p.Name] {
			t.Errorf("duplicate pipeline name: %s", p.Name)
		}
		seen[p.Name] = true
	}
}

// --- Template tests ---

func TestTemplates(t *testing.T) {
	t.Parallel()

	const prefix = "test/template"
	templates, err := Templates(prefix)
	if err != nil {
		t.Fatalf("Templates: %v", err)
	}

	if len(templates) < 12 {
		t.Fatalf("expected at least 12 templates, got %d", len(templates))
	}

	byName := make(map[string]Template, len(templates))
	for _, template := range templates {
		byName[template.Name] = template
	}

	// Every template must have a non-empty description and a valid hash.
	for _, template := range templates {
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

	// Inherits references must use the substituted prefix.
	for _, template := range templates {
		for _, parent := range template.Content.Inherits {
			if !strings.HasPrefix(parent, prefix+":") {
				t.Errorf("template %q inherits %q, expected prefix %q:", template.Name, parent, prefix)
			}
		}
	}

	// Inheritance chain connectivity: every parent name must exist in the set.
	for _, template := range templates {
		for _, parent := range template.Content.Inherits {
			parts := strings.SplitN(parent, ":", 2)
			if len(parts) != 2 {
				t.Errorf("template %q inherits %q: invalid format", template.Name, parent)
				continue
			}
			parentName := parts[1]
			if _, exists := byName[parentName]; !exists {
				t.Errorf("template %q inherits %q, but %q not found in template set",
					template.Name, parent, parentName)
			}
		}
	}

	// "base" is the root — no inherits.
	verifyTemplateBase(t, byName)
	verifyTemplateBaseNetworked(t, byName, prefix)
	verifyTemplateAgentBase(t, byName, prefix)
	verifyTemplateServiceBase(t, byName, prefix)
	verifyTemplateModelService(t, byName, prefix)
	verifyTemplateGithubService(t, byName, prefix)
	verifyServiceTemplateCommands(t, byName, prefix)
}

func verifyTemplateBase(t *testing.T, byName map[string]Template) {
	t.Helper()
	base, ok := byName["base"]
	if !ok {
		t.Fatal("missing 'base' template")
	}
	if len(base.Content.Inherits) != 0 {
		t.Errorf("base should not inherit, got %v", base.Content.Inherits)
	}
	if base.Content.Namespaces == nil {
		t.Fatal("base missing namespaces")
	}
	if !base.Content.Namespaces.PID || !base.Content.Namespaces.Net ||
		!base.Content.Namespaces.IPC || !base.Content.Namespaces.UTS {
		t.Errorf("base should unshare all namespaces; got %+v", base.Content.Namespaces)
	}
	if base.Content.Security == nil {
		t.Fatal("base missing security")
	}
	if !base.Content.Security.NewSession || !base.Content.Security.DieWithParent ||
		!base.Content.Security.NoNewPrivs {
		t.Errorf("base should enable all security options; got %+v", base.Content.Security)
	}
	hasTmpfs := false
	for _, mount := range base.Content.Filesystem {
		if mount.Dest == "/tmp" && mount.Type == "tmpfs" {
			hasTmpfs = true
		}
	}
	if !hasTmpfs {
		t.Error("base missing /tmp tmpfs mount")
	}
	hasBureauDir := false
	for _, dir := range base.Content.CreateDirs {
		if dir == "/run/bureau" {
			hasBureauDir = true
		}
	}
	if !hasBureauDir {
		t.Error("base missing /run/bureau in CreateDirs")
	}
}

func verifyTemplateBaseNetworked(t *testing.T, byName map[string]Template, prefix string) {
	t.Helper()
	networked, ok := byName["base-networked"]
	if !ok {
		t.Fatal("missing 'base-networked' template")
	}
	if len(networked.Content.Inherits) != 1 || networked.Content.Inherits[0] != prefix+":base" {
		t.Errorf("base-networked inherits = %v, want [%q]", networked.Content.Inherits, prefix+":base")
	}
	if networked.Content.Namespaces == nil {
		t.Fatal("base-networked missing namespaces")
	}
	if networked.Content.Namespaces.Net {
		t.Error("base-networked should have Net=false")
	}
	if !networked.Content.Namespaces.PID || !networked.Content.Namespaces.IPC ||
		!networked.Content.Namespaces.UTS {
		t.Errorf("base-networked should unshare PID, IPC, UTS; got %+v", networked.Content.Namespaces)
	}
	// Must mount resolv.conf, ssl, and nsswitch.conf for DNS/TLS.
	mountDests := make(map[string]bool, len(networked.Content.Filesystem))
	for _, mount := range networked.Content.Filesystem {
		mountDests[mount.Dest] = true
	}
	for _, required := range []string{"/etc/resolv.conf", "/etc/ssl", "/etc/nsswitch.conf"} {
		if !mountDests[required] {
			t.Errorf("base-networked missing mount for %s", required)
		}
	}
}

func verifyTemplateAgentBase(t *testing.T, byName map[string]Template, prefix string) {
	t.Helper()
	agentBase, ok := byName["agent-base"]
	if !ok {
		t.Fatal("missing 'agent-base' template")
	}
	if len(agentBase.Content.Inherits) != 1 || agentBase.Content.Inherits[0] != prefix+":base-networked" {
		t.Errorf("agent-base inherits = %v, want [%q]", agentBase.Content.Inherits, prefix+":base-networked")
	}
	for _, key := range []string{"BUREAU_PROXY_SOCKET", "BUREAU_MACHINE_NAME", "BUREAU_SERVER_NAME"} {
		if agentBase.Content.EnvironmentVariables[key] == "" {
			t.Errorf("agent-base missing environment variable %q", key)
		}
	}
}

func verifyTemplateServiceBase(t *testing.T, byName map[string]Template, prefix string) {
	t.Helper()
	serviceBase, ok := byName["service-base"]
	if !ok {
		t.Fatal("missing 'service-base' template")
	}
	if len(serviceBase.Content.Inherits) != 1 || serviceBase.Content.Inherits[0] != prefix+":base-networked" {
		t.Errorf("service-base inherits = %v, want [%q]", serviceBase.Content.Inherits, prefix+":base-networked")
	}
	for _, key := range []string{"BUREAU_PROXY_SOCKET", "BUREAU_MACHINE_NAME", "BUREAU_SERVER_NAME"} {
		if serviceBase.Content.EnvironmentVariables[key] == "" {
			t.Errorf("service-base missing environment variable %q", key)
		}
	}
}

func verifyTemplateModelService(t *testing.T, byName map[string]Template, prefix string) {
	t.Helper()
	model, ok := byName["model-service"]
	if !ok {
		t.Fatal("missing 'model-service' template")
	}
	if model.Content.EnvironmentVariables["BUREAU_MODEL_HTTP_SOCKET"] != "/run/bureau/listen/http.sock" {
		t.Errorf("model-service BUREAU_MODEL_HTTP_SOCKET = %q, want %q",
			model.Content.EnvironmentVariables["BUREAU_MODEL_HTTP_SOCKET"], "/run/bureau/listen/http.sock")
	}
	if len(model.Content.ProxyServices) == 0 {
		t.Error("model-service should have proxy_services")
	}
	for _, name := range []string{"anthropic", "openrouter", "openai"} {
		service, exists := model.Content.ProxyServices[name]
		if !exists {
			t.Errorf("model-service missing proxy_services[%q]", name)
			continue
		}
		if service.Upstream == "" {
			t.Errorf("model-service proxy_services[%q] has empty upstream", name)
		}
	}
}

func verifyTemplateGithubService(t *testing.T, byName map[string]Template, prefix string) {
	t.Helper()
	github, ok := byName["github-service"]
	if !ok {
		t.Fatal("missing 'github-service' template")
	}
	if len(github.Content.Inherits) != 1 || github.Content.Inherits[0] != prefix+":service-base" {
		t.Errorf("github-service inherits = %v, want [%q]", github.Content.Inherits, prefix+":service-base")
	}
	if len(github.Content.Command) != 1 || github.Content.Command[0] != "bureau-github-service" {
		t.Errorf("github-service command = %v, want [bureau-github-service]", github.Content.Command)
	}
}

func verifyServiceTemplateCommands(t *testing.T, byName map[string]Template, prefix string) {
	t.Helper()

	serviceTemplates := []struct {
		name    string
		command string
	}{
		{"ticket-service", "bureau-ticket-service"},
		{"fleet-controller", "bureau-fleet-controller"},
		{"artifact-service", "bureau-artifact-service"},
		{"agent-service", "bureau-agent-service"},
		{"telemetry-service", "bureau-telemetry-service"},
		{"model-service", "bureau-model-service"},
		{"github-service", "bureau-github-service"},
	}

	for _, serviceTemplate := range serviceTemplates {
		template, ok := byName[serviceTemplate.name]
		if !ok {
			t.Errorf("missing %q template", serviceTemplate.name)
			continue
		}
		if len(template.Content.Command) != 1 || template.Content.Command[0] != serviceTemplate.command {
			t.Errorf("%s command = %v, want [%q]", serviceTemplate.name, template.Content.Command, serviceTemplate.command)
		}
		// All service templates should inherit from service-base.
		ref, err := schema.ParseTemplateRef(template.Content.Inherits[0])
		if err != nil {
			t.Errorf("%s Inherits[0] %q: %v", serviceTemplate.name, template.Content.Inherits[0], err)
			continue
		}
		if ref.Template != "service-base" {
			t.Errorf("%s should inherit from 'service-base', got %q", serviceTemplate.name, ref.Template)
		}
	}

	// bureau-agent should inherit from agent-base.
	bureauAgent, ok := byName["bureau-agent"]
	if !ok {
		t.Fatal("missing 'bureau-agent' template")
	}
	ref, err := schema.ParseTemplateRef(bureauAgent.Content.Inherits[0])
	if err != nil {
		t.Fatalf("bureau-agent Inherits[0]: %v", err)
	}
	if ref.Template != "agent-base" {
		t.Errorf("bureau-agent should inherit from 'agent-base', got %q", ref.Template)
	}
	if len(bureauAgent.Content.RequiredServices) < 2 {
		t.Errorf("bureau-agent RequiredServices = %v, want at least [agent, artifact]", bureauAgent.Content.RequiredServices)
	}
}

func TestTemplatesSourceHashStable(t *testing.T) {
	t.Parallel()

	first, err := Templates("bureau/template")
	if err != nil {
		t.Fatalf("first Templates call: %v", err)
	}
	second, err := Templates("bureau/template")
	if err != nil {
		t.Fatalf("second Templates call: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("template count changed: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].SourceHash != second[i].SourceHash {
			t.Errorf("template %q hash changed between calls: %s vs %s",
				first[i].Name, first[i].SourceHash, second[i].SourceHash)
		}
	}
}

func TestTemplatesNamesUnique(t *testing.T) {
	t.Parallel()

	templates, err := Templates("bureau/template")
	if err != nil {
		t.Fatalf("Templates: %v", err)
	}

	seen := make(map[string]bool, len(templates))
	for _, template := range templates {
		if seen[template.Name] {
			t.Errorf("duplicate template name: %s", template.Name)
		}
		seen[template.Name] = true
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
		t.Fatalf("different template counts: %d vs %d", len(templatesA), len(templatesB))
	}

	// Source hashes should be identical (computed before substitution).
	for i := range templatesA {
		if templatesA[i].SourceHash != templatesB[i].SourceHash {
			t.Errorf("template %q: source hash differs between prefixes", templatesA[i].Name)
		}
	}

	// Templates with inherits should use the respective prefix.
	for i := range templatesA {
		for _, parent := range templatesA[i].Content.Inherits {
			if !strings.HasPrefix(parent, prefixA+":") {
				t.Errorf("template %q with prefix %q: inherits %q does not use expected prefix",
					templatesA[i].Name, prefixA, parent)
			}
		}
		for _, parent := range templatesB[i].Content.Inherits {
			if !strings.HasPrefix(parent, prefixB+":") {
				t.Errorf("template %q with prefix %q: inherits %q does not use expected prefix",
					templatesB[i].Name, prefixB, parent)
			}
		}
	}
}
