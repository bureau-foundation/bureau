// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package hwinfo probes system hardware and produces static inventory
// data for Bureau's m.bureau.machine_info state events. It reads CPU
// topology, memory, NUMA, board identity, and kernel version from
// /proc and /sys on Linux. GPU enumeration is delegated to per-vendor
// subpackages (hwinfo/amdgpu, hwinfo/nvidia) that implement the
// GPUProber interface.
//
// The package also provides shared sysfs/DRM helpers (drm.go) used by
// all GPU vendor subpackages: card device filtering, PCI uevent
// parsing, driver identification, thermal limit reading, and generic
// sysfs integer/string file reading.
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
