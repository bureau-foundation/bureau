// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"fmt"
	"strings"
)

// TemplateRef is a parsed template reference identifying a specific template
// in a specific room. The wire format is:
//
//	<room-alias-localpart>[@<server>]:<template-name>
//
// Examples:
//
//	bureau/template:base
//	iree/template:amdgpu-developer
//	iree/template@other.example:foo
//	iree/template@other.example:8448:versioned-agent
//
// The last colon is the separator between the room reference and the template
// name. This handles server addresses that include port numbers (e.g.,
// "other.example:8448"). Matrix localparts cannot contain colons, and
// template names cannot contain colons, so the last colon is always
// unambiguous.
type TemplateRef struct {
	// Room is the room alias localpart (e.g., "bureau/template",
	// "iree/template"). This maps to the Matrix room alias
	// #<Room>:<server>.
	Room string

	// Template is the template name, used as the state key for the
	// m.bureau.template event (e.g., "base", "llm-agent").
	Template string

	// Server is the optional homeserver for federated deployments
	// (e.g., "other.example", "other.example:8448"). Empty means
	// use the local homeserver.
	Server string
}

// ParseTemplateRef parses a template reference string into its components.
// The format is "<room-alias-localpart>[@<server>]:<template-name>". The
// last colon separates the room reference from the template name, which
// allows server addresses with port numbers.
//
// Returns an error if the reference is empty, contains no colon, or has
// an empty room or template component.
func ParseTemplateRef(reference string) (TemplateRef, error) {
	room, name, server, err := parseRef(reference, "template")
	if err != nil {
		return TemplateRef{}, err
	}
	return TemplateRef{
		Room:     room,
		Template: name,
		Server:   server,
	}, nil
}

// parseRef extracts room, name, and server from a state event reference
// string. The format is "<room-alias-localpart>[@<server>]:<name>". Both
// TemplateRef and PipelineRef use this same parsing logic — the entityKind
// parameter (e.g., "template", "pipeline") controls error messages.
func parseRef(reference, entityKind string) (room, name, server string, err error) {
	if reference == "" {
		return "", "", "", fmt.Errorf("empty %s reference", entityKind)
	}

	// The last colon separates room reference from name.
	// This handles server:port in federated references.
	lastColon := strings.LastIndex(reference, ":")
	if lastColon < 0 {
		return "", "", "", fmt.Errorf("%s reference %q missing colon separator", entityKind, reference)
	}

	roomReference := reference[:lastColon]
	name = reference[lastColon+1:]

	if roomReference == "" {
		return "", "", "", fmt.Errorf("%s reference %q has empty room", entityKind, reference)
	}
	if name == "" {
		return "", "", "", fmt.Errorf("%s reference %q has empty %s name", entityKind, reference, entityKind)
	}

	// Split room reference into localpart and optional @server.
	if atIndex := strings.Index(roomReference, "@"); atIndex >= 0 {
		room = roomReference[:atIndex]
		server = roomReference[atIndex+1:]
		if room == "" {
			return "", "", "", fmt.Errorf("%s reference %q has empty room localpart", entityKind, reference)
		}
		if server == "" {
			return "", "", "", fmt.Errorf("%s reference %q has empty server after @", entityKind, reference)
		}
	} else {
		room = roomReference
	}

	return room, name, server, nil
}

// String returns the canonical wire-format representation of the template
// reference. Round-trips through ParseTemplateRef: for any valid ref,
// ParseTemplateRef(ref.String()) returns an equivalent TemplateRef.
func (ref TemplateRef) String() string {
	if ref.Server != "" {
		return ref.Room + "@" + ref.Server + ":" + ref.Template
	}
	return ref.Room + ":" + ref.Template
}

// RoomAlias returns the full Matrix room alias for this template reference,
// using defaultServer when the reference doesn't specify a server. The
// returned alias has the format "#<room>:<server>".
func (ref TemplateRef) RoomAlias(defaultServer string) string {
	server := ref.Server
	if server == "" {
		server = defaultServer
	}
	return "#" + ref.Room + ":" + server
}
