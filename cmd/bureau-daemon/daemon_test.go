// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/authorization"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// testDaemonEpoch is the fixed starting time for the fake clock in
// newTestDaemon. Using a constant avoids tests depending on wall-clock
// time.
var testDaemonEpoch = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// newTestDaemon creates a Daemon with all map fields initialized, a
// deterministic fake clock, and a no-op logger. Returns both the
// daemon and the fake clock so tests can advance time.
//
// This is the single construction point for Daemon in tests. Do not
// construct Daemon struct literals directly — new map fields added to
// Daemon only need updating here.
func newTestDaemon(t *testing.T) (*Daemon, *clock.FakeClock) {
	t.Helper()
	fakeClock := clock.Fake(testDaemonEpoch)
	daemon := &Daemon{
		clock:              fakeClock,
		authorizationIndex: authorization.NewIndex(),
		logger:             slog.New(slog.NewJSONHandler(io.Discard, nil)),

		// All map fields — adding a new map to Daemon means adding
		// it here so no test panics on nil map write.
		failedExecPaths:       make(map[string]bool),
		startFailures:         make(map[string]*startFailure),
		running:               make(map[string]bool),
		exitWatchers:          make(map[string]context.CancelFunc),
		proxyExitWatchers:     make(map[string]context.CancelFunc),
		lastCredentials:       make(map[string]string),
		lastGrants:            make(map[string][]schema.Grant),
		lastTokenMint:         make(map[string]time.Time),
		activeTokens:          make(map[string][]activeToken),
		lastServiceMounts:     make(map[string][]launcherServiceMount),
		lastObserveAllowances: make(map[string][]schema.Allowance),
		lastSpecs:             make(map[string]*schema.SandboxSpec),
		previousSpecs:         make(map[string]*schema.SandboxSpec),
		lastTemplates:         make(map[string]*schema.TemplateContent),
		healthMonitors:        make(map[string]*healthMonitor),
		services:              make(map[string]*schema.Service),
		proxyRoutes:           make(map[string]string),
		peerAddresses:         make(map[string]string),
		peerTransports:        make(map[string]http.RoundTripper),
		layoutWatchers:        make(map[string]*layoutWatcher),
		tunnels:               make(map[string]*tunnelInstance),
	}
	return daemon, fakeClock
}
