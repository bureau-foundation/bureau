// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package schema defines the Matrix state event types and content
// structures that constitute the Bureau protocol. Event type constants
// (EventType*) are Matrix event type strings; Go structs define the
// JSON content. State keys are the principal's localpart.
//
// Event type constants are split between this package and domain
// sub-packages. Constants that are referenced by this package's power
// level functions (WorkspaceRoomPowerLevels, ConfigRoomPowerLevels,
// PipelineRoomPowerLevels) or whose content types are defined here
// remain in this package. Constants whose content types live in
// sub-packages and are not needed by power level functions have been
// moved to those sub-packages:
//
//   - schema/agent:       EventTypeAgentSession, EventTypeAgentContext, etc.
//   - schema/artifact:    EventTypeArtifactScope
//   - schema/log:         EventTypeLog
//   - schema/pipeline:    EventTypePipelineConfig, EventTypePipelineResult
//
// Constants remaining in this package:
//
//   - [EventTypeMachineKey], [EventTypeMachineStatus],
//     [EventTypeMachineConfig] -- machine lifecycle
//   - [EventTypeCredentials] -- age-encrypted credential bundles
//   - [EventTypeTemplate] -- sandbox templates with inheritance
//   - [EventTypeProject], [EventTypeWorkspace] -- workspace lifecycle
//   - [EventTypePipeline] -- pipeline definitions (used by PipelineRoomPowerLevels)
//   - [EventTypeService] -- service directory
//   - [EventTypeWebRTCOffer], [EventTypeWebRTCAnswer] -- signaling
//   - [EventTypeLayout] -- tmux session structure for observation
//   - [EventTypeTicket], [EventTypeTicketConfig] -- work item tracking
//   - [EventTypeStewardship] -- resource governance
//
// [SandboxSpec] is the fully-resolved sandbox configuration sent over
// the daemon-to-launcher IPC socket. [TemplateRef] and
// [ParseTemplateRef] handle template reference strings for cross-room
// template inheritance. [ConfigRoomPowerLevels],
// [WorkspaceRoomPowerLevels], [PipelineRoomPowerLevels], and
// [ArtifactRoomPowerLevels] produce Matrix power level content for
// Bureau's room types.
package schema
