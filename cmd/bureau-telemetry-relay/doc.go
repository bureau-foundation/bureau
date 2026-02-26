// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-telemetry-relay is the per-machine telemetry collection
// agent. It runs alongside the daemon on each machine and collects
// spans, metrics, log records, and output deltas from local Bureau
// services (proxy, launcher, pipeline executor, observation relay,
// etc.) via its CBOR service socket.
//
// Collected records are accumulated, batched, and shipped to the
// fleet-wide telemetry service through the standard service socket
// protocol. The daemon's transport layer handles cross-machine
// routing when the telemetry service runs on a different machine.
//
// Data flow:
//
//	local service → "submit" action → accumulator → buffer → shipper → telemetry service "ingest"
//
// Flush triggers:
//   - Timer: the flush loop drains the accumulator every --flush-interval (default 5s)
//   - Threshold: the submit handler flushes inline when the accumulator
//     exceeds --flush-threshold-bytes (default 256 KB)
//
// The buffer provides backpressure: when the shipper can't keep up,
// oldest batches are dropped rather than exhausting memory. The
// shipper retries with exponential backoff (1s → 30s cap) on
// transient failures.
//
// See telemetry.md for the full architecture.
package main
