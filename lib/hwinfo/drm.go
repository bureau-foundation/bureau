// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package hwinfo

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// IsCardDevice returns true for DRM card device names (card0, card1, ...)
// but not connectors (card0-DP-1) or render nodes (renderD128).
func IsCardDevice(name string) bool {
	if !strings.HasPrefix(name, "card") {
		return false
	}
	suffix := name[4:]
	if len(suffix) == 0 {
		return false
	}
	for _, character := range suffix {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

// ReadDriverName returns the kernel driver name for a PCI device by
// reading the basename of the "driver" symlink in the device directory.
func ReadDriverName(devicePath string) string {
	link, err := os.Readlink(filepath.Join(devicePath, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}

// ParsePCIUevent extracts vendor name, device ID, and PCI slot from
// the device's uevent file. The uevent file contains lines like:
//
//	PCI_ID=1002:744A
//	PCI_SLOT_NAME=0000:c3:00.0
func ParsePCIUevent(devicePath string) (vendor, deviceID, pciSlot string) {
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

	vendor = PCIVendorName(rawVendorID)
	if rawDeviceID != "" {
		deviceID = "0x" + rawDeviceID
	}
	return vendor, deviceID, pciSlot
}

// PCIVendorName maps a PCI vendor ID to a human-readable name.
func PCIVendorName(vendorID string) string {
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

// ReadThermalLimits reads the critical and emergency temperature
// thresholds from a GPU's hwmon directory. Looks for temp1_crit
// and temp1_emergency. Values are in millidegrees Celsius.
func ReadThermalLimits(devicePath string) (critical, emergency int) {
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
		critical = ReadSysfsInt(filepath.Join(hwmonDir, "temp1_crit"))
		emergency = ReadSysfsInt(filepath.Join(hwmonDir, "temp1_emergency"))
		if critical != 0 || emergency != 0 {
			return critical, emergency
		}
	}
	return 0, 0
}

// ReadSysfsString reads a single-line sysfs file and returns its
// trimmed content. Returns "" on any error.
func ReadSysfsString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ReadSysfsInt reads an integer from a sysfs file. Returns 0 on error.
func ReadSysfsInt(path string) int {
	value := ReadSysfsString(path)
	if value == "" {
		return 0
	}
	result, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return result
}

// ReadSysfsInt64 reads a 64-bit integer from a sysfs file. Returns 0 on error.
func ReadSysfsInt64(path string) int64 {
	value := ReadSysfsString(path)
	if value == "" {
		return 0
	}
	result, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return result
}
