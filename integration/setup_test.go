// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDoctorPassesAfterSetup(t *testing.T) {
	output := runBureauOrFail(t, "matrix", "doctor",
		"--credential-file", credentialFile,
		"--server-name", testServerName,
	)
	if !strings.Contains(output, "All checks passed") {
		t.Errorf("expected 'All checks passed' in doctor output:\n%s", output)
	}
}

func TestSetupIsIdempotent(t *testing.T) {
	// Run setup a second time — should succeed without errors.
	if err := runBureauSetup(); err != nil {
		t.Fatalf("setup re-run failed: %v", err)
	}

	// Doctor should still pass.
	output := runBureauOrFail(t, "matrix", "doctor",
		"--credential-file", credentialFile,
		"--server-name", testServerName,
	)
	if !strings.Contains(output, "All checks passed") {
		t.Errorf("expected 'All checks passed' after idempotent setup:\n%s", output)
	}
}

func TestRoomsExistViaAPI(t *testing.T) {
	session := adminSession(t)
	defer session.Close()

	expectedAliases := []string{
		"#bureau:" + testServerName,
		"#bureau/system:" + testServerName,
		"#bureau/machines:" + testServerName,
		"#bureau/services:" + testServerName,
		"#bureau/templates:" + testServerName,
	}

	for _, alias := range expectedAliases {
		roomID, err := session.ResolveAlias(t.Context(), alias)
		if err != nil {
			t.Errorf("alias %s: %v", alias, err)
			continue
		}
		if roomID == "" {
			t.Errorf("alias %s resolved to empty room ID", alias)
		}
	}
}

func TestSpaceHierarchy(t *testing.T) {
	session := adminSession(t)
	defer session.Close()

	spaceRoomID, err := session.ResolveAlias(t.Context(), "#bureau:"+testServerName)
	if err != nil {
		t.Fatalf("resolve space alias: %v", err)
	}

	// Read space state to find m.space.child events.
	events, err := session.GetRoomState(t.Context(), spaceRoomID)
	if err != nil {
		t.Fatalf("get space state: %v", err)
	}

	children := make(map[string]bool)
	for _, event := range events {
		if event.Type == "m.space.child" && event.StateKey != nil && *event.StateKey != "" {
			children[*event.StateKey] = true
		}
	}

	// Verify each standard room is a child of the space.
	childRooms := []string{
		"#bureau/system:" + testServerName,
		"#bureau/machines:" + testServerName,
		"#bureau/services:" + testServerName,
		"#bureau/templates:" + testServerName,
	}

	for _, alias := range childRooms {
		roomID, err := session.ResolveAlias(t.Context(), alias)
		if err != nil {
			t.Errorf("resolve %s: %v", alias, err)
			continue
		}
		if !children[roomID] {
			t.Errorf("room %s (%s) is not a child of the Bureau space", alias, roomID)
		}
	}
}

func TestTemplatesPublished(t *testing.T) {
	session := adminSession(t)
	defer session.Close()

	templatesRoomID, err := session.ResolveAlias(t.Context(), "#bureau/templates:"+testServerName)
	if err != nil {
		t.Fatalf("resolve templates alias: %v", err)
	}

	// Read base template state event.
	content, err := session.GetStateEvent(t.Context(), templatesRoomID, "m.bureau.template", "base")
	if err != nil {
		t.Fatalf("get base template: %v", err)
	}

	var template map[string]any
	if err := json.Unmarshal(content, &template); err != nil {
		t.Fatalf("unmarshal base template: %v", err)
	}

	description, ok := template["description"].(string)
	if !ok || description == "" {
		t.Errorf("base template missing description, got: %v", template)
	}

	// Verify base-networked template exists and inherits from base.
	content, err = session.GetStateEvent(t.Context(), templatesRoomID, "m.bureau.template", "base-networked")
	if err != nil {
		t.Fatalf("get base-networked template: %v", err)
	}

	if err := json.Unmarshal(content, &template); err != nil {
		t.Fatalf("unmarshal base-networked template: %v", err)
	}

	inherits, ok := template["inherits"].(string)
	if !ok || inherits != "bureau/templates:base" {
		t.Errorf("expected base-networked to inherit from bureau/templates:base, got %q", inherits)
	}
}

func TestJoinRulesAreInviteOnly(t *testing.T) {
	session := adminSession(t)
	defer session.Close()

	rooms := []string{
		"#bureau:" + testServerName,
		"#bureau/system:" + testServerName,
		"#bureau/machines:" + testServerName,
		"#bureau/services:" + testServerName,
		"#bureau/templates:" + testServerName,
	}

	for _, alias := range rooms {
		roomID, err := session.ResolveAlias(t.Context(), alias)
		if err != nil {
			t.Errorf("resolve %s: %v", alias, err)
			continue
		}

		content, err := session.GetStateEvent(t.Context(), roomID, "m.room.join_rules", "")
		if err != nil {
			t.Errorf("get join rules for %s: %v", alias, err)
			continue
		}

		var joinRules map[string]any
		if err := json.Unmarshal(content, &joinRules); err != nil {
			t.Errorf("unmarshal join rules for %s: %v", alias, err)
			continue
		}

		if rule, _ := joinRules["join_rule"].(string); rule != "invite" {
			t.Errorf("room %s: join_rule is %q, expected invite", alias, rule)
		}
	}
}

func TestDoctorJSONOutput(t *testing.T) {
	output := runBureauOrFail(t, "matrix", "doctor",
		"--credential-file", credentialFile,
		"--server-name", testServerName,
		"--json",
	)

	var result struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("cannot parse doctor JSON output: %v\noutput:\n%s", err, output)
	}

	if !result.OK {
		t.Error("doctor JSON reports ok=false")
		for _, check := range result.Checks {
			if check.Status == "fail" {
				t.Errorf("  [FAIL] %s", check.Name)
			}
		}
	}

	if len(result.Checks) == 0 {
		t.Error("doctor JSON returned no checks")
	}
}
