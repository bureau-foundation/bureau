// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package nvidia provides GPU enumeration for NVIDIA GPUs using the
// nvidia (proprietary) or nouveau (open-source) kernel drivers. Static
// information is read from sysfs (/sys/class/drm/card*) and, when the
// proprietary driver is loaded, from /proc/driver/nvidia/gpus/.
//
// Dynamic metrics (utilization, temperature, power) are not available
// without NVML (libnvidia-ml.so), which would require either cgo or
// purego-style dlopen. The Collector stub returns empty stats; this is
// an extension point for future NVML integration if NVIDIA telemetry
// becomes a requirement.
//
// NVIDIA's kernel ioctl interface (NV_ESC_* on /dev/nvidiactl) requires
// version negotiation and changes between driver versions, so direct
// ioctl access (like we do for amdgpu) is not viable.
package nvidia

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/bureau-foundation/bureau/lib/hwinfo"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// Prober implements hwinfo.GPUProber for NVIDIA GPUs using either the
// proprietary nvidia driver or the open-source nouveau driver. It
// enumerates GPUs by walking /sys/class/drm/card* and reading sysfs
// attributes. When the proprietary driver is loaded, additional
// information (GPU name, UUID, VBIOS) is read from /proc/driver/nvidia/.
type Prober struct {
	// sysRoot is the root of the sysfs filesystem. Defaults to "/sys"
	// in production; overridden in tests with synthetic filesystems.
	sysRoot string

	// procRoot is the root of the proc filesystem. Defaults to "/proc"
	// in production; overridden in tests.
	procRoot string
}

// NewProber creates a Prober that reads from the real /sys and /proc
// filesystems.
func NewProber() *Prober {
	return &Prober{sysRoot: "/sys", procRoot: "/proc"}
}

// newProberFrom creates a Prober with custom filesystem roots for testing.
func newProberFrom(sysRoot, procRoot string) *Prober {
	return &Prober{sysRoot: sysRoot, procRoot: procRoot}
}

// Enumerate returns static GPU information for all NVIDIA GPUs managed
// by the nvidia or nouveau drivers on this system. Returns nil if no
// NVIDIA devices are found.
func (p *Prober) Enumerate() []schema.GPUInfo {
	drmBase := filepath.Join(p.sysRoot, "class/drm")
	entries, err := os.ReadDir(drmBase)
	if err != nil {
		return nil
	}

	var gpus []schema.GPUInfo
	for _, entry := range entries {
		name := entry.Name()
		if !hwinfo.IsCardDevice(name) {
			continue
		}

		devicePath := filepath.Join(drmBase, name, "device")
		driver := hwinfo.ReadDriverName(devicePath)
		if driver != "nvidia" && driver != "nouveau" {
			continue
		}

		gpu := readGPUInfo(devicePath, driver)

		// When the proprietary driver is loaded, enrich with
		// /proc/driver/nvidia/ data (GPU name, UUID, VBIOS version).
		if driver == "nvidia" && gpu.PCISlot != "" {
			p.enrichFromProc(&gpu)
		}

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

	// PCIe link width.
	gpu.PCIeLinkWidth = hwinfo.ReadSysfsInt(filepath.Join(devicePath, "current_link_width"))

	// Thermal limits from hwmon (nouveau exposes these; nvidia proprietary
	// typically does not, since it manages thermals internally).
	gpu.ThermalLimitCriticalMillidegrees, gpu.ThermalLimitEmergencyMillidegrees =
		hwinfo.ReadThermalLimits(devicePath)

	return gpu
}

// enrichFromProc reads additional GPU information from
// /proc/driver/nvidia/gpus/<pci-slot>/information, which the
// proprietary nvidia driver provides. The file contains key-value
// lines like:
//
//	Model:           NVIDIA GeForce RTX 4090
//	GPU UUID:        GPU-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
//	Video BIOS:      95.02.3c.80.b8
func (p *Prober) enrichFromProc(gpu *schema.GPUInfo) {
	infoPath := filepath.Join(p.procRoot, "driver/nvidia/gpus", gpu.PCISlot, "information")
	data, err := os.ReadFile(infoPath)
	if err != nil {
		return
	}

	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "Model":
			gpu.ModelName = value
		case "GPU UUID":
			gpu.UniqueID = value
		case "Video BIOS":
			gpu.VBIOSVersion = value
		}
	}
}
