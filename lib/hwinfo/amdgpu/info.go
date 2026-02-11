// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package amdgpu provides GPU enumeration and telemetry collection for
// AMD GPUs using the amdgpu kernel driver. Static information is read
// from sysfs (/sys/class/drm/card*). Dynamic metrics (utilization,
// temperature, power, clocks) are collected via DRM ioctls on render
// nodes (/dev/dri/renderD*), which is the same interface used by
// rocm-smi internally. Requires video or render group membership for
// ioctl access.
//
// No cgo is required — all ioctl calls use golang.org/x/sys/unix
// with struct layouts matching the upstream Linux kernel UAPI headers
// (include/uapi/drm/amdgpu_drm.h), which are stable ABI.
package amdgpu

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/bureau-foundation/bureau/lib/hwinfo"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// Prober implements hwinfo.GPUProber for AMD GPUs using the amdgpu
// kernel driver. It enumerates GPUs by walking /sys/class/drm/card*
// and reading sysfs attributes.
type Prober struct {
	// sysRoot is the root of the sysfs filesystem. Defaults to "/sys"
	// in production; overridden in tests with synthetic filesystems.
	sysRoot string
}

// NewProber creates a Prober that reads from the real /sys filesystem.
func NewProber() *Prober {
	return &Prober{sysRoot: "/sys"}
}

// newProberFrom creates a Prober with a custom sysfs root for testing.
func newProberFrom(sysRoot string) *Prober {
	return &Prober{sysRoot: sysRoot}
}

// Enumerate returns static GPU information for all AMD GPUs managed
// by the amdgpu driver on this system. Returns nil if no amdgpu
// devices are found.
func (p *Prober) Enumerate() []schema.GPUInfo {
	drmBase := filepath.Join(p.sysRoot, "class/drm")
	entries, err := os.ReadDir(drmBase)
	if err != nil {
		return nil
	}

	var gpus []schema.GPUInfo
	for _, entry := range entries {
		name := entry.Name()
		// Match card0, card1, ... but not card0-DP-1, renderD128, etc.
		if !hwinfo.IsCardDevice(name) {
			continue
		}

		cardPath := filepath.Join(drmBase, name)
		devicePath := filepath.Join(cardPath, "device")

		// Check that this card uses the amdgpu driver.
		driver := hwinfo.ReadDriverName(devicePath)
		if driver != "amdgpu" {
			continue
		}

		gpu := readGPUInfo(devicePath, driver)
		gpus = append(gpus, gpu)
	}

	return gpus
}

// readGPUInfo reads static GPU attributes from sysfs for a single card.
func readGPUInfo(devicePath, driver string) schema.GPUInfo {
	gpu := schema.GPUInfo{
		Driver: driver,
	}

	// PCI identity from uevent.
	gpu.Vendor, gpu.PCIDeviceID, gpu.PCISlot = hwinfo.ParsePCIUevent(devicePath)

	// VRAM total (bytes).
	gpu.VRAMTotalBytes = hwinfo.ReadSysfsInt64(filepath.Join(devicePath, "mem_info_vram_total"))

	// Hardware serial number.
	gpu.UniqueID = hwinfo.ReadSysfsString(filepath.Join(devicePath, "unique_id"))

	// Firmware version.
	gpu.VBIOSVersion = hwinfo.ReadSysfsString(filepath.Join(devicePath, "vbios_version"))

	// VRAM manufacturer.
	gpu.VRAMVendor = hwinfo.ReadSysfsString(filepath.Join(devicePath, "mem_info_vram_vendor"))

	// PCIe link width.
	gpu.PCIeLinkWidth = hwinfo.ReadSysfsInt(filepath.Join(devicePath, "current_link_width"))

	// Thermal limits from hwmon.
	gpu.ThermalLimitCriticalMillidegrees, gpu.ThermalLimitEmergencyMillidegrees =
		hwinfo.ReadThermalLimits(devicePath)

	return gpu
}

// renderNodeForDevice finds the /dev/dri/renderD* path that
// corresponds to the same PCI device as a card. This is necessary
// because the card index and render node index don't necessarily
// match (e.g., card0 may be renderD129, not renderD128).
func renderNodeForDevice(devicePath, sysRoot string) string {
	// Resolve the PCI device path for this card.
	cardPCIPath, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return ""
	}

	drmBase := filepath.Join(sysRoot, "class/drm")
	entries, err := os.ReadDir(drmBase)
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "renderD") {
			continue
		}
		renderDevicePath := filepath.Join(drmBase, name, "device")
		renderPCIPath, err := filepath.EvalSymlinks(renderDevicePath)
		if err != nil {
			continue
		}
		if renderPCIPath == cardPCIPath {
			return filepath.Join("/dev/dri", name)
		}
	}
	return ""
}
