// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-telemetry-service is the fleet-wide telemetry aggregation
// service. It receives CBOR-encoded [telemetry.TelemetryBatch]
// messages from per-machine relays over streaming connections, verifies
// service tokens, and logs ingestion statistics. A local Unix socket
// serves both the streaming ingestion and CBOR query actions.
//
// The service uses [service.BootstrapViaProxy] with audience "telemetry"
// and registers in the fleet service directory so relays can discover
// it via the daemon's cross-machine service routing.
//
// # Startup
//
// The service runs inside a daemon-managed sandbox with a proxy that
// injects Matrix credentials. It resolves the fleet service room for
// discovery, registers itself, and starts the Unix socket server with
// the "ingest" stream handler and the "status" query handler.
//
// # Ingestion Protocol
//
// Relays open a streaming connection to the service's "ingest" action.
// After the initial CBOR handshake (action + service token), the
// service sends a readiness ack. The relay then streams TelemetryBatch
// CBOR values on the connection. The service decodes each batch,
// updates stats, and sends a per-batch ack. The connection stays open
// until the relay disconnects or the service shuts down.
//
// # Socket API
//
// The CLI and agents connect to the service's Unix socket and send
// CBOR requests. Currently supports:
//
//   - ingest (authenticated, streaming): relay batch ingestion
//   - status (unauthenticated): ingestion stats, connected relay
//     count, uptime
package main
