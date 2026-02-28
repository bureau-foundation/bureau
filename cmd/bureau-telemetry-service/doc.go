// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-telemetry-service is the fleet-wide telemetry aggregation
// service. It receives CBOR-encoded [telemetry.TelemetryBatch]
// messages from per-machine relays over streaming connections, verifies
// service tokens, persists output deltas as CAS artifacts, and tracks
// them in m.bureau.log state events. A local Unix socket serves
// streaming ingestion, query, and session lifecycle actions.
//
// The service uses [service.BootstrapViaProxy] with audience "telemetry"
// and registers in the fleet service directory so relays can discover
// it via the daemon's cross-machine service routing.
//
// # Startup
//
// The service runs inside a daemon-managed sandbox with a proxy that
// injects Matrix credentials. It resolves the fleet service room for
// discovery, registers itself, creates an artifact store client for
// output persistence, and starts the Unix socket server.
//
// # Output Delta Persistence
//
// OutputDelta messages (raw terminal bytes from sandboxed processes)
// are buffered per-session and flushed to the artifact store either
// when the buffer exceeds 1 MB or on a 10-second periodic timer. Each
// flush stores a CAS artifact and updates the session's m.bureau.log
// state event in the source's machine config room.
//
// Sessions transition through lifecycle states: active (receiving
// deltas), complete (sandbox exited, all data flushed), or rotating
// (long-lived service with chunk eviction).
//
// # Ingestion Protocol
//
// Relays open a streaming connection to the service's "ingest" action.
// After the initial CBOR handshake (action + service token), the
// service sends a readiness ack. The relay then streams TelemetryBatch
// CBOR values on the connection. The service decodes each batch,
// routes output deltas to the log manager for persistence, updates
// stats, fans out to tail subscribers, and sends a per-batch ack. The
// connection stays open until the relay disconnects or the service
// shuts down.
//
// # Tail Protocol
//
// Clients open a streaming connection to the service's "tail" action.
// After authentication (telemetry/tail grant required), the service
// sends a readiness ack. The client then sends tailControl messages
// to subscribe to sources by glob pattern (matching entity localparts
// via [principal.MatchPattern]). The service pushes matching
// TelemetryBatch frames as they are ingested. Heartbeat frames are
// sent periodically to detect dead connections. The client can
// dynamically add and remove patterns mid-stream.
//
// # Socket API
//
// The CLI and agents connect to the service's Unix socket and send
// CBOR requests. Supported actions:
//
//   - ingest (authenticated, streaming): relay batch ingestion
//   - tail (authenticated, streaming): live telemetry subscription
//     with dynamic glob-pattern source filtering
//   - complete-log (authenticated): flush remaining output and mark
//     a session's log entity as complete
//   - status (unauthenticated): ingestion stats, connected relay
//     count, uptime
package main
