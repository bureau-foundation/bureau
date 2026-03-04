// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"github.com/bureau-foundation/bureau/lib/ref"
)

// FleetScope holds the shared flags for commands that operate within a
// fleet: --machine, --fleet, --server-name. Embed this anonymously in a
// command's params struct to get all three flags registered
// automatically via [BindFlags].
//
// Call [FleetScope.Resolve] in the Run function to resolve the raw flag
// values into typed refs using the machine.conf fallback chain.
//
//	type myParams struct {
//	    cli.SessionConfig
//	    cli.FleetScope
//	    cli.JSONOutput
//	}
type FleetScope struct {
	Machine    string `json:"machine"     flag:"machine"     desc:"machine localpart (optional — auto-discovers if omitted)"`
	Fleet      string `json:"fleet"       flag:"fleet"       desc:"fleet prefix (e.g., bureau/fleet/prod) — required when --machine is omitted"`
	ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name (auto-detected from machine.conf)"`
}

// ResolvedFleetScope holds the typed refs produced by
// [FleetScope.Resolve]. Exactly one of Machine or Fleet is non-zero:
//   - When --machine is given, Machine is set and Fleet is derived from it.
//   - When --machine is omitted, Fleet is set and Machine is zero.
type ResolvedFleetScope struct {
	ServerName ref.ServerName
	Machine    ref.Machine
	Fleet      ref.Fleet
}

// Resolve applies the machine.conf fallback chain for server name and
// fleet, then parses the raw flag values into typed refs.
//
// When --machine is provided, both Machine and Fleet are set (Fleet is
// derived from the machine). When --machine is omitted, only Fleet is
// set (parsed from --fleet or machine.conf).
func (f *FleetScope) Resolve() (*ResolvedFleetScope, error) {
	f.ServerName = ResolveServerName(f.ServerName)
	f.Fleet = ResolveFleet(f.Fleet)

	serverName, err := ref.ParseServerName(f.ServerName)
	if err != nil {
		return nil, Validation("invalid --server-name %q: %w", f.ServerName, err)
	}

	result := &ResolvedFleetScope{ServerName: serverName}

	if f.Machine != "" {
		result.Machine, err = ref.ParseMachine(f.Machine, serverName)
		if err != nil {
			return nil, Validation("invalid machine: %v", err)
		}
		result.Fleet = result.Machine.Fleet()
	} else {
		result.Fleet, err = ref.ParseFleet(f.Fleet, serverName)
		if err != nil {
			return nil, Validation("invalid fleet: %v", err)
		}
	}

	return result, nil
}
