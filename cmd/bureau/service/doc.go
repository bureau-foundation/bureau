// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package service implements the "bureau service" CLI commands for
// creating, inspecting, and managing service principals.
//
// Services are principals running inside Bureau sandboxes, typically
// long-running server processes (ticket service, artifact cache, STT
// engine, etc.) that provide capabilities to agents and other services.
//
// Mechanically, services are identical to agents — both are principals
// deployed via MachineConfig with encrypted credentials, sandbox
// isolation, and daemon-managed lifecycle. The difference is in what
// they run: agents run AI agent wrappers, services run server binaries.
//
// All lifecycle operations use the shared library in [lib/principal]:
// Create, Resolve, List, Destroy.
package service
