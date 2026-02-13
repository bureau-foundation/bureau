// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package schema defines the Matrix state event types and content
// structures that constitute the Bureau protocol. Event type constants
// (EventType*) are Matrix event type strings; Go structs define the
// JSON content. State keys are the principal's localpart.
//
// Key event types:
//
//   - [EventTypeMachineKey], [EventTypeMachineStatus],
//     [EventTypeMachineConfig] -- machine lifecycle
//   - [EventTypeCredentials] -- age-encrypted credential bundles
//   - [EventTypeTemplate] -- sandbox templates with inheritance
//   - [EventTypeProject], [EventTypeWorkspace] -- workspace lifecycle
//   - [EventTypeService] -- service directory
//   - [EventTypeWebRTCOffer], [EventTypeWebRTCAnswer] -- signaling
//   - [EventTypeLayout] -- tmux session structure for observation
//   - [EventTypeTicket], [EventTypeTicketConfig] -- work item tracking
//   - [EventTypeArtifactScope] -- artifact service integration config
//
// [SandboxSpec] is the fully-resolved sandbox configuration sent over
// the daemon-to-launcher IPC socket. [TemplateRef] and
// [ParseTemplateRef] handle template reference strings for cross-room
// template inheritance. [ConfigRoomPowerLevels],
// [WorkspaceRoomPowerLevels], [PipelineRoomPowerLevels], and
// [ArtifactRoomPowerLevels] produce Matrix power level content for
// Bureau's room types.
//
// This package depends on no other Bureau packages.
package schema
