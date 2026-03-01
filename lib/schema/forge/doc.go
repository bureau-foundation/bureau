// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package forge defines the Bureau forge connector protocol types:
// provider-agnostic event types for subscription streams, Matrix state
// event content for repository bindings, per-room configuration,
// identity mapping, work identity, and auto-subscribe rules.
//
// These types are shared by all forge connectors (GitHub, Forgejo,
// GitLab). Provider-specific translation happens at webhook ingestion
// time — agents receive the same typed structs regardless of which
// forge hosts the repository.
package forge
