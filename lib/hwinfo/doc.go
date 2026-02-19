// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package hwinfo probes system hardware and collects runtime metrics
// for Bureau's machine status reporting.
//
// # Static inventory
//
// Reads CPU topology, memory, NUMA, board identity, and kernel version
// from /proc and /sys on Linux for m.bureau.machine_info state events.
// GPU enumeration is delegated to per-vendor subpackages that implement
// the [GPUProber] interface.
//
// # Runtime metrics
//
// Provides system-level and per-sandbox resource metrics for periodic
// heartbeat publishing (m.bureau.machine_status):
//
//   - Host CPU utilization from /proc/stat ([ReadCPUStats], [CPUPercent])
//   - Host memory usage via syscall.Sysinfo ([MemoryUsedMB])
//   - Per-sandbox CPU and memory from cgroup v2 ([ReadCgroupCPUStats],
//     [CgroupCPUPercent], [ReadCgroupMemoryBytes])
//   - Principal lifecycle status derivation ([DerivePrincipalStatus])
//   - Bureau cgroup path convention ([CgroupDefaultPath])
//
// # DRM helpers
//
// Shared sysfs/DRM helpers (drm.go) used by all GPU vendor subpackages:
// card device filtering, PCI uevent parsing, driver identification,
// thermal limit reading, and generic sysfs integer/string file reading.
//
// # Subpackages
//
//   - hwinfo/amdgpu: Full GPU telemetry via DRM ioctls (pure Go, no
//     cgo). Static enumeration from sysfs, dynamic metrics (utilization,
//     temperature, power, clocks) from the AMDGPU_INFO_SENSOR ioctl on
//     render nodes. VRAM usage from sysfs (not available via ioctl).
//
//   - hwinfo/nvidia: Static enumeration from sysfs and
//     /proc/driver/nvidia/ (proprietary driver) or sysfs-only (nouveau).
//     Dynamic metrics require NVML and are stubbed — the collector
//     returns nil.
package hwinfo
