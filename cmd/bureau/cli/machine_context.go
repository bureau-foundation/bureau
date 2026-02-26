// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import "os"

// DefaultMachineConfPath is the canonical location for machine
// configuration, written by "bureau machine doctor --fix".
const DefaultMachineConfPath = "/etc/bureau/machine.conf"

// MachineConf holds the parsed contents of /etc/bureau/machine.conf.
// The zero value is a valid empty configuration representing the absence
// of machine.conf — normal on developer laptops and remote admin
// workstations where the Bureau daemon is not running.
type MachineConf struct {
	HomeserverURL  string
	MachineName    string
	ServerName     string
	Fleet          string
	SystemUser     string
	OperatorsGroup string
}

// MachineConfPath returns the path to the machine configuration file.
// Checks the BUREAU_MACHINE_CONF environment variable first, falling
// back to /etc/bureau/machine.conf.
func MachineConfPath() string {
	if envPath := os.Getenv("BUREAU_MACHINE_CONF"); envPath != "" {
		return envPath
	}
	return DefaultMachineConfPath
}

// LoadMachineConf reads the machine configuration file and returns a
// typed representation. Returns a zero-value MachineConf if the file
// does not exist or cannot be read — absence of machine.conf is the
// normal case on developer laptops and is not an error.
//
// The file format is KEY=VALUE (same as credential files), with keys:
//   - BUREAU_HOMESERVER_URL — Matrix homeserver URL
//   - BUREAU_MACHINE_NAME — machine localpart
//   - BUREAU_SERVER_NAME — Matrix server name
//   - BUREAU_FLEET — fleet localpart
//   - BUREAU_SYSTEM_USER — system user name (optional)
//   - BUREAU_OPERATORS_GROUP — operators group name (optional)
func LoadMachineConf() MachineConf {
	credentials, err := ReadCredentialFile(MachineConfPath())
	if err != nil {
		return MachineConf{}
	}
	return MachineConf{
		HomeserverURL:  credentials["BUREAU_HOMESERVER_URL"],
		MachineName:    credentials["BUREAU_MACHINE_NAME"],
		ServerName:     credentials["BUREAU_SERVER_NAME"],
		Fleet:          credentials["BUREAU_FLEET"],
		SystemUser:     credentials["BUREAU_SYSTEM_USER"],
		OperatorsGroup: credentials["BUREAU_OPERATORS_GROUP"],
	}
}

// ResolveServerName returns the Matrix server name to use. Resolution
// priority:
//  1. explicit — non-empty value from a CLI flag or argument
//  2. machine.conf — BUREAU_SERVER_NAME from /etc/bureau/machine.conf
//  3. "bureau.local" — hardcoded default for single-machine deployments
func ResolveServerName(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if conf := LoadMachineConf(); conf.ServerName != "" {
		return conf.ServerName
	}
	return "bureau.local"
}

// ResolveFleet returns the fleet localpart to use. Resolution priority:
//  1. explicit — non-empty value from a CLI flag or argument
//  2. machine.conf — BUREAU_FLEET from /etc/bureau/machine.conf
//  3. "" — empty string; the caller must handle the missing-fleet case
//
// Unlike ResolveServerName, this function does not provide a hardcoded
// fallback because there is no universal default fleet name.
func ResolveFleet(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if conf := LoadMachineConf(); conf.Fleet != "" {
		return conf.Fleet
	}
	return ""
}

// ResolveHomeserverURL returns the Matrix homeserver URL to use.
// Resolution priority:
//  1. explicit — non-empty value from a CLI flag
//  2. machine.conf — BUREAU_HOMESERVER_URL from /etc/bureau/machine.conf
//  3. fallback — caller-provided default (e.g., "http://localhost:6167")
func ResolveHomeserverURL(explicit, fallback string) string {
	if explicit != "" {
		return explicit
	}
	if conf := LoadMachineConf(); conf.HomeserverURL != "" {
		return conf.HomeserverURL
	}
	return fallback
}
