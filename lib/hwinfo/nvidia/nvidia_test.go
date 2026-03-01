// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package nvidia

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bureau-foundation/bureau/lib/hwinfo"
)

// writeSyntheticFile creates a file at the given path within root,
// creating parent directories as needed.
func writeSyntheticFile(t *testing.T, root, path, content string) {
	t.Helper()
	fullPath := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(fullPath), err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", fullPath, err)
	}
}

// writeSyntheticSymlink creates a symlink at the given path within root.
func writeSyntheticSymlink(t *testing.T, root, path, target string) {
	t.Helper()
	fullPath := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(fullPath), err)
	}
	if err := os.Symlink(target, fullPath); err != nil {
		t.Fatalf("symlink %s -> %s: %v", fullPath, target, err)
	}
}

// createSyntheticNVIDIA sets up a synthetic sysfs tree for one nvidia
// proprietary driver device.
func createSyntheticNVIDIA(t *testing.T, root string, cardIndex int, pciSlot string) {
	t.Helper()

	cardName := "card" + string(rune('0'+cardIndex))
	cardPath := filepath.Join("sys/class/drm", cardName)

	// Driver symlink pointing to nvidia driver directory.
	driverDir := filepath.Join(root, "sys/bus/pci/drivers/nvidia")
	if err := os.MkdirAll(driverDir, 0755); err != nil {
		t.Fatalf("mkdir driver: %v", err)
	}
	deviceDir := filepath.Join(root, cardPath, "device")
	if err := os.MkdirAll(deviceDir, 0755); err != nil {
		t.Fatalf("mkdir device: %v", err)
	}
	writeSyntheticSymlink(t, root, filepath.Join(cardPath, "device", "driver"), driverDir)

	// PCI uevent.
	writeSyntheticFile(t, root, filepath.Join(cardPath, "device", "uevent"),
		"DRIVER=nvidia\nPCI_CLASS=30000\nPCI_ID=10DE:2684\nPCI_SUBSYS_ID=10DE:16A1\nPCI_SLOT_NAME="+pciSlot+"\n")

	// PCIe link width.
	writeSyntheticFile(t, root, filepath.Join(cardPath, "device", "current_link_width"), "16\n")
}

// createSyntheticNouveau sets up a synthetic sysfs tree for one nouveau
// driver device.
func createSyntheticNouveau(t *testing.T, root string, cardIndex int, pciSlot string) {
	t.Helper()

	cardName := "card" + string(rune('0'+cardIndex))
	cardPath := filepath.Join("sys/class/drm", cardName)

	driverDir := filepath.Join(root, "sys/bus/pci/drivers/nouveau")
	if err := os.MkdirAll(driverDir, 0755); err != nil {
		t.Fatalf("mkdir driver: %v", err)
	}
	deviceDir := filepath.Join(root, cardPath, "device")
	if err := os.MkdirAll(deviceDir, 0755); err != nil {
		t.Fatalf("mkdir device: %v", err)
	}
	writeSyntheticSymlink(t, root, filepath.Join(cardPath, "device", "driver"), driverDir)

	writeSyntheticFile(t, root, filepath.Join(cardPath, "device", "uevent"),
		"DRIVER=nouveau\nPCI_CLASS=30000\nPCI_ID=10DE:1CB3\nPCI_SUBSYS_ID=10DE:11BE\nPCI_SLOT_NAME="+pciSlot+"\n")

	writeSyntheticFile(t, root, filepath.Join(cardPath, "device", "current_link_width"), "8\n")

	// Nouveau exposes hwmon thermal limits.
	hwmonDir := filepath.Join(cardPath, "device", "hwmon", "hwmon0")
	writeSyntheticFile(t, root, filepath.Join(hwmonDir, "temp1_crit"), "97000\n")
	writeSyntheticFile(t, root, filepath.Join(hwmonDir, "name"), "nouveau\n")
}

// createSyntheticProcNVIDIA creates a synthetic /proc/driver/nvidia/gpus
// information file for the proprietary driver.
func createSyntheticProcNVIDIA(t *testing.T, root, pciSlot string) {
	t.Helper()
	infoContent := `Model:           NVIDIA GeForce RTX 4090
IRQ:             189
GPU UUID:        GPU-12345678-abcd-efgh-ijkl-123456789abc
Video BIOS:      95.02.3c.80.b8
Bus Type:        PCIe
DMA Size:        47 bits
DMA Mask:        0x7fffffffffff
Bus Location:    ` + pciSlot + `
Device Minor:    0
GPU Excluded:    No
`
	writeSyntheticFile(t, root, filepath.Join("proc/driver/nvidia/gpus", pciSlot, "information"), infoContent)
}

func TestEnumerateNVIDIAProprietaryDriver(t *testing.T) {
	root := t.TempDir()
	createSyntheticNVIDIA(t, root, 0, "0000:01:00.0")
	createSyntheticProcNVIDIA(t, root, "0000:01:00.0")

	prober := newProberFrom(filepath.Join(root, "sys"), filepath.Join(root, "proc"))
	gpus := prober.Enumerate()

	if len(gpus) != 1 {
		t.Fatalf("Enumerate() returned %d GPUs, want 1", len(gpus))
	}

	gpu := gpus[0]
	if gpu.Vendor != "NVIDIA" {
		t.Errorf("Vendor = %q, want NVIDIA", gpu.Vendor)
	}
	if gpu.PCIDeviceID != "0x2684" {
		t.Errorf("PCIDeviceID = %q, want 0x2684", gpu.PCIDeviceID)
	}
	if gpu.PCISlot != "0000:01:00.0" {
		t.Errorf("PCISlot = %q, want 0000:01:00.0", gpu.PCISlot)
	}
	if gpu.Driver != "nvidia" {
		t.Errorf("Driver = %q, want nvidia", gpu.Driver)
	}
	if gpu.PCIeLinkWidth != 16 {
		t.Errorf("PCIeLinkWidth = %d, want 16", gpu.PCIeLinkWidth)
	}
	// Enriched from /proc/driver/nvidia/.
	if gpu.ModelName != "NVIDIA GeForce RTX 4090" {
		t.Errorf("ModelName = %q, want NVIDIA GeForce RTX 4090", gpu.ModelName)
	}
	if gpu.UniqueID != "GPU-12345678-abcd-efgh-ijkl-123456789abc" {
		t.Errorf("UniqueID = %q, want GPU-12345678-abcd-efgh-ijkl-123456789abc", gpu.UniqueID)
	}
	if gpu.VBIOSVersion != "95.02.3c.80.b8" {
		t.Errorf("VBIOSVersion = %q, want 95.02.3c.80.b8", gpu.VBIOSVersion)
	}
}

func TestEnumerateNouveauDriver(t *testing.T) {
	root := t.TempDir()
	createSyntheticNouveau(t, root, 0, "0000:01:00.0")

	prober := newProberFrom(filepath.Join(root, "sys"), filepath.Join(root, "proc"))
	gpus := prober.Enumerate()

	if len(gpus) != 1 {
		t.Fatalf("Enumerate() returned %d GPUs, want 1", len(gpus))
	}

	gpu := gpus[0]
	if gpu.Vendor != "NVIDIA" {
		t.Errorf("Vendor = %q, want NVIDIA", gpu.Vendor)
	}
	if gpu.PCIDeviceID != "0x1cb3" {
		t.Errorf("PCIDeviceID = %q, want 0x1cb3", gpu.PCIDeviceID)
	}
	if gpu.Driver != "nouveau" {
		t.Errorf("Driver = %q, want nouveau", gpu.Driver)
	}
	if gpu.PCIeLinkWidth != 8 {
		t.Errorf("PCIeLinkWidth = %d, want 8", gpu.PCIeLinkWidth)
	}
	if gpu.ThermalLimitCriticalMillidegrees != 97000 {
		t.Errorf("ThermalLimitCritical = %d, want 97000", gpu.ThermalLimitCriticalMillidegrees)
	}
	// Nouveau does not populate /proc/driver/nvidia/, so these should be empty.
	if gpu.ModelName != "" {
		t.Errorf("ModelName = %q, want empty (nouveau has no /proc info)", gpu.ModelName)
	}
	if gpu.UniqueID != "" {
		t.Errorf("UniqueID = %q, want empty (nouveau has no /proc info)", gpu.UniqueID)
	}
}

func TestEnumerateSkipsAMDGPU(t *testing.T) {
	root := t.TempDir()
	createSyntheticNVIDIA(t, root, 0, "0000:01:00.0")

	// Create an amdgpu card that should be ignored.
	amdDriverDir := filepath.Join(root, "sys/bus/pci/drivers/amdgpu")
	if err := os.MkdirAll(amdDriverDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cardPath := "sys/class/drm/card1"
	deviceDir := filepath.Join(root, cardPath, "device")
	if err := os.MkdirAll(deviceDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSyntheticSymlink(t, root, filepath.Join(cardPath, "device", "driver"), amdDriverDir)

	prober := newProberFrom(filepath.Join(root, "sys"), filepath.Join(root, "proc"))
	gpus := prober.Enumerate()

	if len(gpus) != 1 {
		t.Fatalf("Enumerate() returned %d GPUs, want 1 (should skip amdgpu)", len(gpus))
	}
	if gpus[0].Vendor != "NVIDIA" {
		t.Errorf("Vendor = %q, want NVIDIA", gpus[0].Vendor)
	}
}

func TestEnumerateNoGPUs(t *testing.T) {
	root := t.TempDir()
	prober := newProberFrom(filepath.Join(root, "sys"), filepath.Join(root, "proc"))
	gpus := prober.Enumerate()

	if len(gpus) != 0 {
		t.Errorf("Enumerate() returned %d GPUs, want 0", len(gpus))
	}
}

func TestEnumerateNoProcFallback(t *testing.T) {
	// NVIDIA proprietary driver card, but /proc/driver/nvidia/ is missing.
	// Should still enumerate with PCI-level info, just no model name/UUID.
	root := t.TempDir()
	createSyntheticNVIDIA(t, root, 0, "0000:01:00.0")

	prober := newProberFrom(filepath.Join(root, "sys"), filepath.Join(root, "proc"))
	gpus := prober.Enumerate()

	if len(gpus) != 1 {
		t.Fatalf("Enumerate() returned %d GPUs, want 1", len(gpus))
	}

	gpu := gpus[0]
	if gpu.Vendor != "NVIDIA" {
		t.Errorf("Vendor = %q, want NVIDIA", gpu.Vendor)
	}
	if gpu.PCIDeviceID != "0x2684" {
		t.Errorf("PCIDeviceID = %q, want 0x2684", gpu.PCIDeviceID)
	}
	// Without /proc, enrichment fields should be empty.
	if gpu.ModelName != "" {
		t.Errorf("ModelName = %q, want empty (no /proc)", gpu.ModelName)
	}
	if gpu.UniqueID != "" {
		t.Errorf("UniqueID = %q, want empty (no /proc)", gpu.UniqueID)
	}
}

func TestEnumerateMultipleNVIDIA(t *testing.T) {
	root := t.TempDir()
	createSyntheticNVIDIA(t, root, 0, "0000:01:00.0")
	createSyntheticNVIDIA(t, root, 1, "0000:41:00.0")
	createSyntheticProcNVIDIA(t, root, "0000:01:00.0")
	createSyntheticProcNVIDIA(t, root, "0000:41:00.0")

	prober := newProberFrom(filepath.Join(root, "sys"), filepath.Join(root, "proc"))
	gpus := prober.Enumerate()

	if len(gpus) != 2 {
		t.Fatalf("Enumerate() returned %d GPUs, want 2", len(gpus))
	}

	slots := make(map[string]bool)
	for _, gpu := range gpus {
		slots[gpu.PCISlot] = true
		if gpu.ModelName != "NVIDIA GeForce RTX 4090" {
			t.Errorf("GPU at %s: ModelName = %q, want NVIDIA GeForce RTX 4090", gpu.PCISlot, gpu.ModelName)
		}
	}
	if !slots["0000:01:00.0"] {
		t.Error("missing GPU at PCI slot 0000:01:00.0")
	}
	if !slots["0000:41:00.0"] {
		t.Error("missing GPU at PCI slot 0000:41:00.0")
	}
}

func TestEnumerateSkipsConnectors(t *testing.T) {
	root := t.TempDir()
	createSyntheticNVIDIA(t, root, 0, "0000:01:00.0")

	// Create connector entries that should be ignored.
	connectorPath := filepath.Join(root, "sys/class/drm/card0-HDMI-A-1")
	if err := os.MkdirAll(connectorPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	prober := newProberFrom(filepath.Join(root, "sys"), filepath.Join(root, "proc"))
	gpus := prober.Enumerate()

	if len(gpus) != 1 {
		t.Fatalf("Enumerate() returned %d GPUs, want 1", len(gpus))
	}
}

func TestCollectorStubReturnsNil(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	collector := NewCollector(logger)
	defer collector.Close()

	stats := collector.Collect()
	if stats != nil {
		t.Errorf("Collect() = %v, want nil (stub collector)", stats)
	}
}

func TestIsCardDeviceViaParent(t *testing.T) {
	// Verify that the shared hwinfo.IsCardDevice works correctly
	// for NVIDIA-relevant patterns.
	tests := []struct {
		name string
		want bool
	}{
		{"card0", true},
		{"card1", true},
		{"card0-HDMI-A-1", false},
		{"card0-DP-1", false},
		{"renderD128", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hwinfo.IsCardDevice(test.name); got != test.want {
				t.Errorf("IsCardDevice(%q) = %v, want %v", test.name, got, test.want)
			}
		})
	}
}

// TestLiveEnumerate runs against real sysfs on a machine with NVIDIA
// devices. Skipped when no NVIDIA devices are present.
func TestLiveEnumerate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping: requires Linux sysfs")
	}

	prober := NewProber()
	gpus := prober.Enumerate()

	if len(gpus) == 0 {
		t.Skip("skipping: no NVIDIA devices found")
	}

	for i, gpu := range gpus {
		if gpu.Vendor != "NVIDIA" {
			t.Errorf("GPU[%d].Vendor = %q, want NVIDIA", i, gpu.Vendor)
		}
		if gpu.PCISlot == "" {
			t.Errorf("GPU[%d].PCISlot is empty", i)
		}
		if gpu.PCIDeviceID == "" {
			t.Errorf("GPU[%d].PCIDeviceID is empty", i)
		}
		if gpu.Driver != "nvidia" && gpu.Driver != "nouveau" {
			t.Errorf("GPU[%d].Driver = %q, want nvidia or nouveau", i, gpu.Driver)
		}
		t.Logf("GPU[%d]: %s model=%q device=%s slot=%s driver=%s uuid=%s",
			i, gpu.Vendor, gpu.ModelName, gpu.PCIDeviceID, gpu.PCISlot,
			gpu.Driver, gpu.UniqueID)
	}
}
