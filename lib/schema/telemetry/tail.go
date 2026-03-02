// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import "github.com/bureau-foundation/bureau/lib/codec"

// TailFrame is the CBOR frame sent from the telemetry service to a
// tail client over a streaming connection. The Type field distinguishes
// batch data from heartbeat keepalives.
//
// Wire protocol after handshake:
//
//	Server → Client: TailFrame{Type: "batch", Batch: <raw CBOR>}
//	Server → Client: TailFrame{Type: "heartbeat"}                (periodic)
//	Client → Server: TailControl{Subscribe: [...]}               (dynamic)
//	Client → Server: TailControl{Unsubscribe: [...]}             (dynamic)
type TailFrame struct {
	Type  string           `json:"type"`
	Batch codec.RawMessage `json:"batch,omitempty"`
}

// TailControl is the CBOR message sent from a tail client to the
// telemetry service to dynamically adjust the source filter. Subscribe
// adds glob patterns; Unsubscribe removes them. Patterns use the same
// syntax as [principal.MatchPattern]: *, **, and ? on hierarchical
// localpart paths.
//
// Examples of patterns:
//
//   - "my_bureau/fleet/prod/**"             — everything in a fleet
//   - "**/machine/gpu-*"                    — all machines named gpu-*
//   - "my_bureau/fleet/prod/service/relay"  — exact service match
//   - "**"                                  — all sources
type TailControl struct {
	Subscribe   []string `json:"subscribe,omitempty"`
	Unsubscribe []string `json:"unsubscribe,omitempty"`
}
