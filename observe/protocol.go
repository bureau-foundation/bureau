// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Message type constants for the observation protocol wire format.
// Each message is a 5-byte header (1 byte type + 4 byte big-endian
// payload length) followed by the payload.
const (
	// MessageTypeData carries raw terminal bytes. Bidirectional: output
	// flows remote→local, input flows local→remote. Payload is opaque
	// bytes passed through unmodified.
	MessageTypeData byte = 0x01

	// MessageTypeResize carries terminal dimensions. Local→remote only.
	// Payload is 4 bytes: columns (uint16 big-endian) then rows (uint16
	// big-endian). The relay applies TIOCSWINSZ to the PTY master.
	MessageTypeResize byte = 0x02

	// MessageTypeHistory carries a ring buffer dump for scrollback
	// replay. Remote→local only, sent once on connect before live data.
	// Payload is raw terminal bytes (escape sequences and all) from the
	// ring buffer.
	MessageTypeHistory byte = 0x03

	// MessageTypeMetadata carries session information as JSON.
	// Remote→local only, sent once on connect before history. Contains
	// session name, principal identity, machine identity, pane list.
	MessageTypeMetadata byte = 0x04
)

// messageHeaderLength is the fixed size of a message header: 1 byte type
// + 4 bytes payload length.
const messageHeaderLength = 5

// maxPayloadLength is the maximum allowed payload size. 16 MB is generous
// for terminal data; a history dump of 1 MB ring buffer is typical.
const maxPayloadLength = 16 * 1024 * 1024

// Message is a single observation protocol message.
type Message struct {
	Type    byte
	Payload []byte
}

// WriteMessage writes a framed message to w. The frame format is:
// [1 byte type] [4 bytes payload length, big-endian uint32] [payload].
func WriteMessage(w io.Writer, message Message) error {
	var header [messageHeaderLength]byte
	header[0] = message.Type
	binary.BigEndian.PutUint32(header[1:5], uint32(len(message.Payload)))
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("write message header: %w", err)
	}
	if len(message.Payload) > 0 {
		if _, err := w.Write(message.Payload); err != nil {
			return fmt.Errorf("write message payload: %w", err)
		}
	}
	return nil
}

// ReadMessage reads a framed message from r. Returns the message type and
// payload. Returns an error if the stream is malformed or the payload
// exceeds maxPayloadLength.
func ReadMessage(r io.Reader) (Message, error) {
	var header [messageHeaderLength]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Message{}, fmt.Errorf("read message header: %w", err)
	}
	messageType := header[0]
	payloadLength := binary.BigEndian.Uint32(header[1:5])
	if payloadLength > maxPayloadLength {
		return Message{}, fmt.Errorf("payload length %d exceeds maximum %d", payloadLength, maxPayloadLength)
	}
	payload := make([]byte, payloadLength)
	if payloadLength > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Message{}, fmt.Errorf("read message payload: %w", err)
		}
	}
	return Message{Type: messageType, Payload: payload}, nil
}

// NewDataMessage creates a data message carrying raw terminal bytes.
func NewDataMessage(data []byte) Message {
	return Message{Type: MessageTypeData, Payload: data}
}

// NewResizeMessage creates a resize message with the given terminal dimensions.
func NewResizeMessage(columns, rows uint16) Message {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint16(payload[0:2], columns)
	binary.BigEndian.PutUint16(payload[2:4], rows)
	return Message{Type: MessageTypeResize, Payload: payload}
}

// ParseResizePayload extracts columns and rows from a resize message payload.
// Returns an error if the payload is not exactly 4 bytes.
func ParseResizePayload(payload []byte) (columns, rows uint16, err error) {
	if len(payload) != 4 {
		return 0, 0, fmt.Errorf("resize payload must be 4 bytes, got %d", len(payload))
	}
	columns = binary.BigEndian.Uint16(payload[0:2])
	rows = binary.BigEndian.Uint16(payload[2:4])
	return columns, rows, nil
}

// NewHistoryMessage creates a history message carrying ring buffer contents.
func NewHistoryMessage(data []byte) Message {
	return Message{Type: MessageTypeHistory, Payload: data}
}

// NewMetadataMessage creates a metadata message from a JSON-encoded payload.
func NewMetadataMessage(jsonPayload []byte) Message {
	return Message{Type: MessageTypeMetadata, Payload: jsonPayload}
}

// ObserveRequest is the initial JSON request sent by the client to the daemon
// when establishing an observation session. Sent on the unix socket before
// switching to the binary observation protocol.
//
// Authentication is mandatory: both Observer and Token must be present.
// The daemon verifies the token against the homeserver and checks the
// verified identity against the target principal's observation allowances.
type ObserveRequest struct {
	// Principal is the localpart of the target to observe
	// (e.g., "iree/amdgpu/pm").
	Principal string `json:"principal"`

	// Mode is "readwrite" or "readonly". The daemon may downgrade
	// readwrite to readonly based on the target's observation allowances —
	// the granted mode is reported in the ObserveResponse.
	Mode string `json:"mode"`

	// Observer is the Matrix user ID of the entity requesting
	// observation (e.g., "@bureau-admin:bureau.local"). This is the
	// asserted identity — the Token field proves it.
	Observer string `json:"observer"`

	// Token is a Matrix access token that authenticates the Observer.
	// The daemon verifies this against the homeserver before granting
	// access. Clients load this from their session file (operators)
	// or receive it from the proxy (agents).
	Token string `json:"token"`
}

// ObserveResponse is the daemon's JSON response to an observation request.
// On success, the socket switches to the binary observation protocol.
// On failure, the connection is closed after sending this response.
type ObserveResponse struct {
	// OK is true if the observation session was established.
	OK bool `json:"ok"`

	// Session is the tmux session name on the remote machine
	// (e.g., "bureau/iree/amdgpu/pm"). Only set when OK is true.
	Session string `json:"session,omitempty"`

	// Machine is the machine identity hosting the principal
	// (e.g., "machine/workstation"). Only set when OK is true.
	Machine string `json:"machine,omitempty"`

	// GrantedMode is the observation mode the daemon actually granted.
	// May differ from the requested mode if the target's allowances permit
	// the observer but only for readonly access. Clients should check
	// this to know whether their input will be forwarded.
	GrantedMode string `json:"granted_mode,omitempty"`

	// Error describes why the request failed. Only set when OK is false.
	Error string `json:"error,omitempty"`
}

// MetadataPayload is the JSON structure carried by metadata messages.
type MetadataPayload struct {
	// Session is the tmux session name (e.g., "bureau/iree/amdgpu/pm").
	Session string `json:"session"`

	// Principal is the full Matrix user ID (e.g., "@iree/amdgpu/pm:bureau.local").
	Principal string `json:"principal"`

	// Machine is the full Matrix user ID of the hosting machine.
	Machine string `json:"machine"`

	// Panes lists the panes in the current tmux window.
	Panes []PaneInfo `json:"panes"`
}

// PaneInfo describes a single pane in the observed tmux session.
type PaneInfo struct {
	// Index is the tmux pane index within the window.
	Index int `json:"index"`

	// Command is the running command in this pane.
	Command string `json:"command"`

	// Width and Height are the pane dimensions in columns and rows.
	Width  int `json:"width"`
	Height int `json:"height"`

	// Active is true if this is the currently selected pane.
	Active bool `json:"active"`
}
