// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/authorization"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/hwinfo"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// mustRoomID parses a raw room ID string, panicking on invalid input.
// Test-only: provides concise room ID construction for struct literals
// and function arguments without per-call error handling.
func mustRoomID(raw string) ref.RoomID {
	roomID, err := ref.ParseRoomID(raw)
	if err != nil {
		panic(fmt.Sprintf("invalid test room ID %q: %v", raw, err))
	}
	return roomID
}

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

	namespace, err := ref.NewNamespace(ref.MustParseServerName("test.local"), "bureau")
	if err != nil {
		t.Fatalf("create test namespace: %v", err)
	}
	fleet, err := ref.NewFleet(namespace, "test")
	if err != nil {
		t.Fatalf("create test fleet: %v", err)
	}
	machine, err := ref.NewMachine(fleet, "testmachine")
	if err != nil {
		t.Fatalf("create test machine: %v", err)
	}

	daemon := &Daemon{
		clock:              fakeClock,
		machine:            machine,
		fleet:              fleet,
		authorizationIndex: authorization.NewIndex(),
		operatorsGID:       -1, // no bureau-operators group in test environments
		logger:             slog.New(slog.NewJSONHandler(io.Discard, nil)),

		// All map fields — adding a new map to Daemon means adding
		// it here so no test panics on nil map write.
		failedExecPaths:       make(map[string]bool),
		startFailures:         make(map[ref.Entity]*startFailure),
		running:               make(map[ref.Entity]bool),
		drainGracePeriod:      defaultDrainGracePeriod,
		maxIdleInterval:       defaultMaxIdleInterval,
		dynamicPrincipals:     make(map[ref.Entity]bool),
		completed:             make(map[ref.Entity]bool),
		draining:              make(map[ref.Entity]context.CancelFunc),
		pipelineTickets:       make(map[string]ref.Entity),
		pipelineEnabledRooms:  make(map[ref.RoomID]bool),
		exitWatchers:          make(map[ref.Entity]context.CancelFunc),
		proxyExitWatchers:     make(map[ref.Entity]context.CancelFunc),
		lastCredentials:       make(map[ref.Entity]string),
		lastGrants:            make(map[ref.Entity][]schema.Grant),
		lastTokenMint:         make(map[ref.Entity]time.Time),
		activeTokens:          make(map[ref.Entity][]activeToken),
		lastServiceMounts:     make(map[ref.Entity][]launcherServiceMount),
		lastObserveAllowances: make(map[ref.Entity][]schema.Allowance),
		lastSpecs:             make(map[ref.Entity]*schema.SandboxSpec),
		previousSpecs:         make(map[ref.Entity]*schema.SandboxSpec),
		lastTemplates:         make(map[ref.Entity]*schema.TemplateContent),
		healthMonitors:        make(map[ref.Entity]*healthMonitor),
		previousCgroupCPU:     make(map[ref.Entity]*hwinfo.CgroupCPUReading),
		services:              make(map[string]*schema.Service),
		proxyRoutes:           make(map[string]string),
		peerAddresses:         make(map[string]string),
		peerTransports:        make(map[string]http.RoundTripper),
		layoutWatchers:        make(map[ref.Entity]*layoutWatcher),
		tunnels:               make(map[string]*tunnelInstance),
		validateCommandFunc:   func(string, string) error { return nil },
	}
	return daemon, fakeClock
}

// mustParseUserID parses a raw user ID string, panicking on invalid input.
// Test-only: provides concise user ID construction for SessionFromToken
// calls and assertions without per-call error handling.
func mustParseUserID(raw string) ref.UserID {
	userID, err := ref.ParseUserID(raw)
	if err != nil {
		panic(fmt.Sprintf("invalid test user ID %q: %v", raw, err))
	}
	return userID
}

// testMachineSetup constructs a valid ref.Machine and ref.Fleet for tests
// that need a specific machine name and server. Uses namespace "bureau"
// and fleet "test" for all test machines.
func testMachineSetup(t *testing.T, name, server string) (ref.Machine, ref.Fleet) {
	t.Helper()
	namespace, err := ref.NewNamespace(ref.MustParseServerName(server), "bureau")
	if err != nil {
		t.Fatalf("create test namespace: %v", err)
	}
	fleet, err := ref.NewFleet(namespace, "test")
	if err != nil {
		t.Fatalf("create test fleet: %v", err)
	}
	machine, err := ref.NewMachine(fleet, name)
	if err != nil {
		t.Fatalf("create test machine: %v", err)
	}
	return machine, fleet
}

// testEntity constructs a ref.Entity from a fleet and a bare account
// localpart (e.g., "agent/test", "service/stt/whisper"). Fatal on error.
func testEntity(t *testing.T, fleet ref.Fleet, accountLocalpart string) ref.Entity {
	t.Helper()
	entity, err := ref.NewEntityFromAccountLocalpart(fleet, accountLocalpart)
	if err != nil {
		t.Fatalf("testEntity(%q): %v", accountLocalpart, err)
	}
	return entity
}
