// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Pipeline executor for Bureau sandboxes. Reads a pipeline definition
// (from a file, a Matrix pipeline ref, or the sandbox payload), resolves
// variables, sequences steps, and reports progress via the proxy's
// structured /v1/matrix/* API.
//
// The executor's only interface to the outside world is the proxy Unix
// socket at /run/bureau/proxy.sock. It uses the structured API for all
// Matrix operations — whoami, alias resolution, state events, messages.
//
// Pipeline resolution has two tiers:
//
//  1. CLI argument — file path (/path/to/pipeline.jsonc) or pipeline ref
//     (bureau/pipeline:dev-workspace-init). For development and explicit
//     invocation.
//  2. Payload keys — /run/bureau/payload.json contains either pipeline_ref
//     (string) or pipeline_inline (PipelineContent JSON object). For
//     daemon-driven execution via PrincipalAssignment.
//
// If neither tier resolves a pipeline, the executor exits with a clear
// error. There is no embedded fallback — using a stale compiled-in
// pipeline when the Matrix version was deleted or updated would silently
// produce wrong behavior.
//
// Step execution supports:
//
//   - run: shell commands via sh -c (PATH lookup) with inherited stdout/stderr
//   - publish: state event publication via proxy putState
//   - when: guard conditions that skip steps on non-zero exit
//   - check: post-run health checks that fail steps on non-zero exit
//   - optional: failed optional steps log a warning and continue
//   - timeout: per-step deadlines (default 5 minutes)
//
// When the pipeline's log section is configured, the executor creates a
// Matrix thread in the specified room and posts per-step progress updates.
// Thread creation failure is fatal — observation is a core Bureau
// requirement, and running with no record of what happened is worse than
// not running at all.
package main
