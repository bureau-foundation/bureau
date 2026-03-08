// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// separate types for compile-time safety. The struct shape and parse/format
// patterns are identical by design — the shared logic (parseRef, formatRef,
// refRoomAlias) is already extracted into template_ref.go.
//
//nolint:dupl // CredentialRef, PipelineRef, and TemplateRef are deliberately
package schema

import "github.com/bureau-foundation/bureau/lib/ref"

// PipelineRef is a parsed pipeline reference identifying a specific pipeline
// in a specific room. The wire format is:
//
//	<room-alias-localpart>[@<server>]:<pipeline-name>
//
// Examples:
//
//	bureau/pipeline:dev-workspace-init
//	iree/pipeline:build-release
//	iree/pipeline@other.example:deploy
//	iree/pipeline@other.example:8448:migrate-v2
//
// The last colon is the separator between the room reference and the pipeline
// name. This handles server addresses that include port numbers (e.g.,
// "other.example:8448"). Matrix localparts cannot contain colons, and
// pipeline names cannot contain colons, so the last colon is always
// unambiguous.
type PipelineRef struct {
	// Room is the room alias localpart (e.g., "bureau/pipeline",
	// "iree/pipeline"). This maps to the Matrix room alias
	// #<Room>:<server>.
	Room string

	// Pipeline is the pipeline name, used as the state key for the
	// m.bureau.pipeline event (e.g., "dev-workspace-init", "deploy").
	Pipeline string

	// Server is the optional homeserver for federated deployments
	// (e.g., "other.example", "other.example:8448"). Empty means
	// use the local homeserver.
	Server string
}

// ParsePipelineRef parses a pipeline reference string into its components.
// The format is "<room-alias-localpart>[@<server>]:<pipeline-name>". The
// last colon separates the room reference from the pipeline name, which
// allows server addresses with port numbers.
//
// Returns an error if the reference is empty, contains no colon, or has
// an empty room or pipeline component.
func ParsePipelineRef(reference string) (PipelineRef, error) {
	room, name, server, err := parseRef(reference, "pipeline")
	if err != nil {
		return PipelineRef{}, err
	}
	return PipelineRef{
		Room:     room,
		Pipeline: name,
		Server:   server,
	}, nil
}

// String returns the canonical wire-format representation of the pipeline
// reference. Round-trips through ParsePipelineRef: for any valid ref,
// ParsePipelineRef(ref.String()) returns an equivalent PipelineRef.
func (pipelineRef PipelineRef) String() string {
	return formatRef(pipelineRef.Room, pipelineRef.Pipeline, pipelineRef.Server)
}

// RoomAlias returns the full Matrix room alias for this pipeline reference,
// using defaultServer when the reference doesn't specify a server.
func (pipelineRef PipelineRef) RoomAlias(defaultServer ref.ServerName) ref.RoomAlias {
	return refRoomAlias(pipelineRef.Room, pipelineRef.Server, defaultServer)
}
