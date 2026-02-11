// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package hwinfo probes system hardware and produces static inventory
// data for Bureau's m.bureau.machine_info state events. It reads CPU
// topology, memory, NUMA, board identity, and kernel version from
// /proc and /sys on Linux. GPU enumeration is delegated to per-vendor
// subpackages (hwinfo/amdgpu, hwinfo/nvidia) that implement the
// GPUProber interface.
package hwinfo

import "github.com/bureau-foundation/bureau/lib/schema"

// GPUProber enumerates GPU hardware for a specific vendor. Each vendor
// subpackage (amdgpu, nvidia) implements this interface. Probe() calls
// all registered probers to build the GPUs list in MachineInfo.
type GPUProber interface {
	// Enumerate returns static GPU information for all GPUs managed
	// by this vendor's driver. Returns nil (not an error) if no GPUs
	// are detected for this vendor.
	Enumerate() []schema.GPUInfo
}

// GPUCollector reads dynamic GPU metrics for periodic heartbeat
// reporting. Each vendor subpackage provides an implementation that
// opens the appropriate device nodes at init and reads sensor values
// on each Collect() call.
type GPUCollector interface {
	// Collect returns current dynamic stats for all GPUs managed by
	// this vendor's driver. Returns nil if no GPUs are available or
	// monitoring is not possible (e.g., missing device permissions).
	Collect() []schema.GPUStatus

	// Close releases any held resources (open file descriptors on
	// render nodes, etc.). Called at daemon shutdown.
	Close()
}
