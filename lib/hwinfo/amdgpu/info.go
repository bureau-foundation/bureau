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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
		if !isCardDevice(name) {
			continue
		}

		cardPath := filepath.Join(drmBase, name)
		devicePath := filepath.Join(cardPath, "device")

		// Check that this card uses the amdgpu driver.
		driver := readDriverName(devicePath)
		if driver != "amdgpu" {
			continue
		}

		gpu := readGPUInfo(devicePath, driver)
		gpus = append(gpus, gpu)
	}

	return gpus
}

// isCardDevice returns true for DRM card device names (card0, card1, ...)
// but not connectors (card0-DP-1) or render nodes (renderD128).
func isCardDevice(name string) bool {
	if !strings.HasPrefix(name, "card") {
		return false
	}
	suffix := name[4:]
	if len(suffix) == 0 {
		return false
	}
	// All remaining characters must be digits.
	for _, character := range suffix {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

// readDriverName returns the kernel driver name for a PCI device by
// reading the basename of the "driver" symlink in the device directory.
func readDriverName(devicePath string) string {
	link, err := os.Readlink(filepath.Join(devicePath, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}

// readGPUInfo reads static GPU attributes from sysfs for a single card.
func readGPUInfo(devicePath, driver string) schema.GPUInfo {
	gpu := schema.GPUInfo{
		Driver: driver,
	}

	// PCI identity from uevent.
	gpu.Vendor, gpu.PCIDeviceID, gpu.PCISlot = parsePCIUevent(devicePath)

	// VRAM total (bytes).
	gpu.VRAMTotalBytes = readSysfsInt64(filepath.Join(devicePath, "mem_info_vram_total"))

	// Hardware serial number.
	gpu.UniqueID = readSysfsString(filepath.Join(devicePath, "unique_id"))

	// Firmware version.
	gpu.VBIOSVersion = readSysfsString(filepath.Join(devicePath, "vbios_version"))

	// VRAM manufacturer.
	gpu.VRAMVendor = readSysfsString(filepath.Join(devicePath, "mem_info_vram_vendor"))

	// PCIe link width.
	gpu.PCIeLinkWidth = readSysfsInt(filepath.Join(devicePath, "current_link_width"))

	// Thermal limits from hwmon.
	gpu.ThermalLimitCriticalMillidegrees, gpu.ThermalLimitEmergencyMillidegrees =
		readThermalLimits(devicePath)

	return gpu
}

// parsePCIUevent extracts vendor name, device ID, and PCI slot from
// the device's uevent file. The uevent file contains lines like:
//
//	PCI_ID=1002:744A
//	PCI_SLOT_NAME=0000:c3:00.0
func parsePCIUevent(devicePath string) (vendor, deviceID, pciSlot string) {
	data, err := os.ReadFile(filepath.Join(devicePath, "uevent"))
	if err != nil {
		return "", "", ""
	}

	var rawVendorID, rawDeviceID string

	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, value := parts[0], parts[1]
		switch key {
		case "PCI_ID":
			// Format: "1002:744A" (vendor:device, uppercase hex).
			ids := strings.SplitN(value, ":", 2)
			if len(ids) == 2 {
				rawVendorID = strings.ToLower(ids[0])
				rawDeviceID = strings.ToLower(ids[1])
			}
		case "PCI_SLOT_NAME":
			pciSlot = value
		}
	}

	vendor = pciVendorName(rawVendorID)
	if rawDeviceID != "" {
		deviceID = "0x" + rawDeviceID
	}
	return vendor, deviceID, pciSlot
}

// pciVendorName maps a PCI vendor ID to a human-readable name.
func pciVendorName(vendorID string) string {
	switch vendorID {
	case "1002":
		return "AMD"
	case "10de":
		return "NVIDIA"
	case "8086":
		return "Intel"
	default:
		if vendorID != "" {
			return fmt.Sprintf("0x%s", vendorID)
		}
		return ""
	}
}

// readThermalLimits reads the critical and emergency temperature
// thresholds from the GPU's hwmon directory. Looks for temp1_crit
// and temp1_emergency (the "edge" sensor). Values are in millidegrees
// Celsius.
func readThermalLimits(devicePath string) (critical, emergency int) {
	hwmonBase := filepath.Join(devicePath, "hwmon")
	entries, err := os.ReadDir(hwmonBase)
	if err != nil {
		return 0, 0
	}

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "hwmon") {
			continue
		}
		hwmonDir := filepath.Join(hwmonBase, entry.Name())
		critical = readSysfsInt(filepath.Join(hwmonDir, "temp1_crit"))
		emergency = readSysfsInt(filepath.Join(hwmonDir, "temp1_emergency"))
		if critical != 0 || emergency != 0 {
			return critical, emergency
		}
	}
	return 0, 0
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

// readSysfsString reads a single-line sysfs file and returns its
// trimmed content. Returns "" on any error.
func readSysfsString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readSysfsInt reads an integer from a sysfs file. Returns 0 on error.
func readSysfsInt(path string) int {
	value := readSysfsString(path)
	if value == "" {
		return 0
	}
	result, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return result
}

// readSysfsInt64 reads a 64-bit integer from a sysfs file. Returns 0 on error.
func readSysfsInt64(path string) int64 {
	value := readSysfsString(path)
	if value == "" {
		return 0
	}
	result, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return result
}
