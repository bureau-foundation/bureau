// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package telemetry defines Bureau-native telemetry data types: spans,
// metrics, log records, and the batch wire format used between the
// per-machine relay and the fleet-wide telemetry service. All types
// carry Bureau identity (ref.Fleet, ref.Machine, ref.Entity) as
// first-class typed fields rather than opaque string attributes.
//
// These types are serialized as CBOR on the relay→service wire and on
// service socket query responses. JSON struct tags are used so that
// the fxamacker/cbor library's json-tag fallback provides correct
// field naming for both formats (see lib/codec doc.go for the tagging
// convention).
//
// See docs/design/telemetry.md for the full architecture.
package telemetry
