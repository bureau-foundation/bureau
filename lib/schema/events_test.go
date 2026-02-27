// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/artifact"
	"github.com/bureau-foundation/bureau/lib/schema/log"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
)

func TestEventTypeConstants(t *testing.T) {
	// Verify the event type strings match the Matrix convention (m.bureau.*).
	// These are wire-format identifiers that must never change without a
	// coordinated migration.
	tests := []struct {
		name     string
		constant ref.EventType
		want     string
	}{
		{"machine_key", EventTypeMachineKey, "m.bureau.machine_key"},
		{"machine_info", EventTypeMachineInfo, "m.bureau.machine_info"},
		{"machine_status", EventTypeMachineStatus, "m.bureau.machine_status"},
		{"machine_config", EventTypeMachineConfig, "m.bureau.machine_config"},
		{"credentials", EventTypeCredentials, "m.bureau.credentials"},
		{"service", EventTypeService, "m.bureau.service"},
		{"layout", EventTypeLayout, "m.bureau.layout"},
		{"template", EventTypeTemplate, "m.bureau.template"},
		{"project", EventTypeProject, "m.bureau.project"},
		{"workspace", EventTypeWorkspace, "m.bureau.workspace"},
		{"pipeline", EventTypePipeline, "m.bureau.pipeline"},
		{"pipeline_config", pipeline.EventTypePipelineConfig, "m.bureau.pipeline_config"},
		{"pipeline_result", pipeline.EventTypePipelineResult, "m.bureau.pipeline_result"},
		{"service_binding", EventTypeServiceBinding, "m.bureau.service_binding"},
		{"ticket", EventTypeTicket, "m.bureau.ticket"},
		{"ticket_config", EventTypeTicketConfig, "m.bureau.ticket_config"},
		{"artifact_scope", artifact.EventTypeArtifactScope, "m.bureau.artifact_scope"},
		{"log", log.EventTypeLog, "m.bureau.log"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if string(test.constant) != test.want {
				t.Errorf("%s = %q, want %q", test.name, test.constant, test.want)
			}
		})
	}
}

func TestMatrixEventTypeConstants(t *testing.T) {
	t.Parallel()
	// These are standard Matrix spec event type strings. They are wire-format
	// identifiers defined by the Matrix protocol and must never change.
	tests := []struct {
		name     string
		constant ref.EventType
		want     string
	}{
		{"message", MatrixEventTypeMessage, "m.room.message"},
		{"power_levels", MatrixEventTypePowerLevels, "m.room.power_levels"},
		{"join_rules", MatrixEventTypeJoinRules, "m.room.join_rules"},
		{"name", MatrixEventTypeRoomName, "m.room.name"},
		{"topic", MatrixEventTypeRoomTopic, "m.room.topic"},
		{"space_child", MatrixEventTypeSpaceChild, "m.space.child"},
		{"canonical_alias", MatrixEventTypeCanonicalAlias, "m.room.canonical_alias"},
		{"encryption", MatrixEventTypeEncryption, "m.room.encryption"},
		{"server_acl", MatrixEventTypeServerACL, "m.room.server_acl"},
		{"tombstone", MatrixEventTypeTombstone, "m.room.tombstone"},
		{"avatar", MatrixEventTypeRoomAvatar, "m.room.avatar"},
		{"history_visibility", MatrixEventTypeHistoryVisibility, "m.room.history_visibility"},
		{"member", MatrixEventTypeRoomMember, "m.room.member"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if string(test.constant) != test.want {
				t.Errorf("%s = %q, want %q", test.name, test.constant, test.want)
			}
		})
	}
}

func TestPowerLevelConstants(t *testing.T) {
	t.Parallel()
	if PowerLevelReadOnly != 0 {
		t.Errorf("PowerLevelReadOnly = %d, want 0", PowerLevelReadOnly)
	}
	if PowerLevelOperator != 50 {
		t.Errorf("PowerLevelOperator = %d, want 50", PowerLevelOperator)
	}
	if PowerLevelAdmin != 100 {
		t.Errorf("PowerLevelAdmin = %d, want 100", PowerLevelAdmin)
	}
}

func TestAdminProtectedEventsUsesConstants(t *testing.T) {
	t.Parallel()
	events := AdminProtectedEvents()

	// Every entry should map to power level 100.
	expectedTypes := []ref.EventType{
		MatrixEventTypeRoomAvatar,
		MatrixEventTypeCanonicalAlias,
		MatrixEventTypeEncryption,
		MatrixEventTypeHistoryVisibility,
		MatrixEventTypeJoinRules,
		MatrixEventTypeRoomName,
		MatrixEventTypePowerLevels,
		MatrixEventTypeServerACL,
		MatrixEventTypeTombstone,
		MatrixEventTypeRoomTopic,
		MatrixEventTypeSpaceChild,
	}
	for _, eventType := range expectedTypes {
		powerLevel, exists := events[eventType]
		if !exists {
			t.Errorf("AdminProtectedEvents missing %q", eventType)
			continue
		}
		if powerLevel != 100 {
			t.Errorf("AdminProtectedEvents[%q] = %v, want 100", eventType, powerLevel)
		}
	}
	if len(events) != len(expectedTypes) {
		t.Errorf("AdminProtectedEvents has %d entries, want %d", len(events), len(expectedTypes))
	}
}

func TestVersionConstants(t *testing.T) {
	// Verify version constants are positive. A zero version
	// constant would mean CanModify can never reject anything.
	if artifact.ArtifactScopeVersion < 1 {
		t.Errorf("ArtifactScopeVersion = %d, must be >= 1", artifact.ArtifactScopeVersion)
	}
	if CredentialsVersion < 1 {
		t.Errorf("CredentialsVersion = %d, must be >= 1", CredentialsVersion)
	}
}

// assertField checks that a JSON object has a field with the expected value.
// This is a shared test helper used across all events_*_test.go files in
// this package.
func assertField(t *testing.T, object map[string]any, key string, want any) {
	t.Helper()
	got, ok := object[key]
	if !ok {
		t.Errorf("field %q missing from JSON", key)
		return
	}
	// JSON numbers are float64, booleans are bool, strings are string.
	if got != want {
		t.Errorf("field %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}
