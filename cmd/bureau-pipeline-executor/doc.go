// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Pipeline executor for Bureau sandboxes. Reads a pipeline execution
// ticket, resolves the pipeline definition from the ticket's pipeline
// ref via the proxy's structured /v1/matrix/* API, sequences steps,
// and reports progress via both the ticket service and Matrix thread
// logging.
//
// The executor operates on two interfaces:
//
//   - Proxy socket (/run/bureau/proxy.sock) for Matrix operations:
//     pipeline ref resolution (room alias → room ID → state event),
//     thread logging, and pipeline result state event publishing.
//
//   - Ticket service socket (/run/bureau/service/ticket.sock) for
//     ticket lifecycle: claiming the pip- ticket, posting step
//     progress updates, adding step completion notes, and closing
//     the ticket with the pipeline's terminal outcome.
//
// Ticket context is provided via environment variables set by the
// daemon when launching the executor sandbox:
//
//   - BUREAU_TICKET_ID: the pip- ticket state key (e.g., "pip-a3f9")
//   - BUREAU_TICKET_ROOM: the room ID where the ticket lives
//
// The trigger file (/run/bureau/trigger.json) contains the full
// TicketContent JSON. The executor extracts the pipeline ref and
// execution variables from the ticket's Pipeline field, and also
// converts top-level TicketContent fields to EVENT_-prefixed
// variables for use in pipeline variable expressions.
//
// Pipeline execution lifecycle:
//
//  1. Claim the ticket (open → in_progress). Exit cleanly on
//     contention (another executor already claimed it).
//  2. Resolve the pipeline definition via the proxy.
//  3. Execute steps with progress updates and step completion notes.
//  4. On completion: set conclusion, close ticket, publish result.
//
// External cancellation is supported: a background goroutine polls
// the ticket status every 5 seconds. When the ticket is closed
// externally, the running step is killed and the executor exits
// with conclusion "cancelled".
//
// Step execution supports:
//
//   - run: shell commands via sh -c (PATH lookup) with inherited stdout/stderr
//   - publish: state event publication via proxy putState
//   - assert_state: precondition checks against Matrix state events
//   - when: guard conditions that skip steps on non-zero exit
//   - check: post-run health checks that fail steps on non-zero exit
//   - optional: failed optional steps log a warning and continue
//   - timeout: per-step deadlines (default 5 minutes)
//   - grace_period: SIGTERM before SIGKILL for graceful shutdown
package main
