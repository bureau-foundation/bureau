// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package amdgpu

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// gpuDevice holds the state for collecting metrics from a single GPU.
type gpuDevice struct {
	// pciSlot is the PCI slot address (join key with GPUInfo/GPUStatus).
	pciSlot string

	// renderFile is the open render node file descriptor. Held open for
	// the daemon's lifetime to avoid repeated open/close on each heartbeat.
	// Nil if the render node could not be opened (missing permissions).
	renderFile *os.File

	// devicePath is the sysfs device path for reading VRAM usage
	// (mem_info_vram_used is not available via ioctl).
	devicePath string
}

// Collector implements hwinfo.GPUCollector for AMD GPUs. It holds open
// file descriptors on /dev/dri/renderD* nodes and reads GPU metrics via
// DRM ioctls on each Collect() call. VRAM usage is read from sysfs
// because it is not available through the sensor ioctl interface.
type Collector struct {
	devices []gpuDevice
	logger  *slog.Logger
}

// NewCollector creates a Collector that discovers and opens all amdgpu
// render nodes on the system. Each render node file descriptor is held
// open for the lifetime of the Collector to avoid per-heartbeat open
// overhead. Call Close() at daemon shutdown to release resources.
//
// If a render node cannot be opened (e.g., the daemon is not in the
// video/render group), that GPU is logged as a warning and excluded
// from collection — other GPUs are still collected.
func NewCollector(logger *slog.Logger) *Collector {
	return newCollectorFrom("/sys", logger)
}

// newCollectorFrom creates a Collector with a custom sysfs root for
// testing. In tests, render nodes are not opened (the test verifies
// sysfs reading only; ioctl testing requires a live GPU).
func newCollectorFrom(sysRoot string, logger *slog.Logger) *Collector {
	collector := &Collector{logger: logger}

	drmBase := filepath.Join(sysRoot, "class/drm")
	entries, err := os.ReadDir(drmBase)
	if err != nil {
		return collector
	}

	for _, entry := range entries {
		name := entry.Name()
		if !isCardDevice(name) {
			continue
		}

		devicePath := filepath.Join(drmBase, name, "device")
		driver := readDriverName(devicePath)
		if driver != "amdgpu" {
			continue
		}

		// Read PCI slot for this GPU (join key with GPUInfo).
		_, _, pciSlot := parsePCIUevent(devicePath)
		if pciSlot == "" {
			continue
		}

		device := gpuDevice{
			pciSlot:    pciSlot,
			devicePath: devicePath,
		}

		// Find and open the corresponding render node.
		renderPath := renderNodeForDevice(devicePath, sysRoot)
		if renderPath != "" {
			file, err := os.OpenFile(renderPath, os.O_RDWR, 0)
			if err != nil {
				logger.Warn("cannot open amdgpu render node, GPU metrics will be unavailable for this device",
					"render_node", renderPath,
					"pci_slot", pciSlot,
					"error", err)
			} else {
				device.renderFile = file
			}
		} else {
			logger.Warn("no render node found for amdgpu device",
				"pci_slot", pciSlot)
		}

		collector.devices = append(collector.devices, device)
	}

	if len(collector.devices) > 0 {
		withIoctl := 0
		for _, device := range collector.devices {
			if device.renderFile != nil {
				withIoctl++
			}
		}
		logger.Info("amdgpu collector initialized",
			"gpu_count", len(collector.devices),
			"ioctl_capable", withIoctl)
	}

	return collector
}

// Collect returns current dynamic stats for all amdgpu devices. Each
// stat entry is identified by PCI slot for correlation with the static
// GPUInfo in MachineInfo.
//
// Sensor queries that fail (e.g., unsupported on a specific ASIC) log
// a debug message and leave the field at zero rather than failing the
// entire collection.
func (c *Collector) Collect() []schema.GPUStatus {
	if len(c.devices) == 0 {
		return nil
	}

	stats := make([]schema.GPUStatus, 0, len(c.devices))
	for _, device := range c.devices {
		status := schema.GPUStatus{
			PCISlot: device.pciSlot,
		}

		// VRAM usage from sysfs (not available via ioctl).
		status.VRAMUsedBytes = readSysfsInt64(filepath.Join(device.devicePath, "mem_info_vram_used"))

		// Sensor queries via ioctl (requires open render node).
		if device.renderFile != nil {
			fd := device.renderFile.Fd()

			if value, err := querySensor(fd, SensorGPULoad); err == nil {
				status.UtilizationPercent = int(value)
			} else {
				c.logger.Debug("sensor query GPU_LOAD failed", "pci_slot", device.pciSlot, "error", err)
			}

			if value, err := querySensor(fd, SensorGPUTemp); err == nil {
				status.TemperatureMillidegrees = int(value)
			} else {
				c.logger.Debug("sensor query GPU_TEMP failed", "pci_slot", device.pciSlot, "error", err)
			}

			if value, err := querySensor(fd, SensorGPUAvgPower); err == nil {
				status.PowerDrawWatts = int(value)
			} else {
				c.logger.Debug("sensor query GPU_AVG_POWER failed", "pci_slot", device.pciSlot, "error", err)
			}

			if value, err := querySensor(fd, SensorGFXSCLK); err == nil {
				status.GraphicsClockMHz = int(value)
			} else {
				c.logger.Debug("sensor query GFX_SCLK failed", "pci_slot", device.pciSlot, "error", err)
			}

			if value, err := querySensor(fd, SensorGFXMCLK); err == nil {
				status.MemoryClockMHz = int(value)
			} else {
				c.logger.Debug("sensor query GFX_MCLK failed", "pci_slot", device.pciSlot, "error", err)
			}
		}

		stats = append(stats, status)
	}

	return stats
}

// Close releases all open render node file descriptors.
func (c *Collector) Close() {
	for _, device := range c.devices {
		if device.renderFile != nil {
			device.renderFile.Close()
		}
	}
	c.devices = nil
}
