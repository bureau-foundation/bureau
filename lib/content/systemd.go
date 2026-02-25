// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package content

import "embed"

//go:embed systemd/bureau-launcher.service systemd/bureau-daemon.service
var systemdFiles embed.FS

// LauncherServiceUnit returns the canonical content of the
// bureau-launcher.service systemd unit file. Machine doctor
// compares installed units against this content and writes it
// when installing units via --fix.
func LauncherServiceUnit() string {
	data, err := systemdFiles.ReadFile("systemd/bureau-launcher.service")
	if err != nil {
		// Embedded at compile time — a read failure here is a build bug.
		panic("embedded bureau-launcher.service missing: " + err.Error())
	}
	return string(data)
}

// DaemonServiceUnit returns the canonical content of the
// bureau-daemon.service systemd unit file. Machine doctor
// compares installed units against this content and writes it
// when installing units via --fix.
func DaemonServiceUnit() string {
	data, err := systemdFiles.ReadFile("systemd/bureau-daemon.service")
	if err != nil {
		panic("embedded bureau-daemon.service missing: " + err.Error())
	}
	return string(data)
}
