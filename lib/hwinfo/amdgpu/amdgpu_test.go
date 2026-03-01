// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package amdgpu

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

// createSyntheticAMDGPU sets up a synthetic sysfs tree for one amdgpu device.
func createSyntheticAMDGPU(t *testing.T, root string, cardIndex int, pciSlot string) {
	t.Helper()

	cardName := "card" + string(rune('0'+cardIndex))
	cardPath := filepath.Join("sys/class/drm", cardName)

	// The driver symlink (readDriverName follows this).
	// Create a fake driver directory and symlink to it.
	driverDir := filepath.Join(root, "sys/bus/pci/drivers/amdgpu")
	if err := os.MkdirAll(driverDir, 0755); err != nil {
		t.Fatalf("mkdir driver: %v", err)
	}
	// Create the device directory first (symlink target).
	deviceDir := filepath.Join(root, cardPath, "device")
	if err := os.MkdirAll(deviceDir, 0755); err != nil {
		t.Fatalf("mkdir device: %v", err)
	}
	writeSyntheticSymlink(t, root, filepath.Join(cardPath, "device", "driver"), driverDir)

	// PCI uevent.
	writeSyntheticFile(t, root, filepath.Join(cardPath, "device", "uevent"),
		"DRIVER=amdgpu\nPCI_CLASS=30000\nPCI_ID=1002:744A\nPCI_SUBSYS_ID=1458:241A\nPCI_SLOT_NAME="+pciSlot+"\n")

	// VRAM.
	writeSyntheticFile(t, root, filepath.Join(cardPath, "device", "mem_info_vram_total"), "48301604864\n")
	writeSyntheticFile(t, root, filepath.Join(cardPath, "device", "mem_info_vram_used"), "27860992\n")
	writeSyntheticFile(t, root, filepath.Join(cardPath, "device", "mem_info_vram_vendor"), "samsung\n")

	// Identity.
	writeSyntheticFile(t, root, filepath.Join(cardPath, "device", "unique_id"), "30437a849c458574\n")
	writeSyntheticFile(t, root, filepath.Join(cardPath, "device", "vbios_version"), "113-APM7489-DS2-100\n")
	writeSyntheticFile(t, root, filepath.Join(cardPath, "device", "current_link_width"), "16\n")

	// Hwmon thermal limits.
	hwmonDir := filepath.Join(cardPath, "device", "hwmon", "hwmon0")
	writeSyntheticFile(t, root, filepath.Join(hwmonDir, "temp1_crit"), "100000\n")
	writeSyntheticFile(t, root, filepath.Join(hwmonDir, "temp1_emergency"), "105000\n")
	writeSyntheticFile(t, root, filepath.Join(hwmonDir, "name"), "amdgpu\n")
}

func TestEnumerateSingleGPU(t *testing.T) {
	root := t.TempDir()
	createSyntheticAMDGPU(t, root, 0, "0000:c3:00.0")

	prober := newProberFrom(filepath.Join(root, "sys"))
	gpus := prober.Enumerate()

	if len(gpus) != 1 {
		t.Fatalf("Enumerate() returned %d GPUs, want 1", len(gpus))
	}

	gpu := gpus[0]
	if gpu.Vendor != "AMD" {
		t.Errorf("Vendor = %q, want AMD", gpu.Vendor)
	}
	if gpu.PCIDeviceID != "0x744a" {
		t.Errorf("PCIDeviceID = %q, want 0x744a", gpu.PCIDeviceID)
	}
	if gpu.PCISlot != "0000:c3:00.0" {
		t.Errorf("PCISlot = %q, want 0000:c3:00.0", gpu.PCISlot)
	}
	if gpu.VRAMTotalBytes != 48301604864 {
		t.Errorf("VRAMTotalBytes = %d, want 48301604864", gpu.VRAMTotalBytes)
	}
	if gpu.UniqueID != "30437a849c458574" {
		t.Errorf("UniqueID = %q, want 30437a849c458574", gpu.UniqueID)
	}
	if gpu.VBIOSVersion != "113-APM7489-DS2-100" {
		t.Errorf("VBIOSVersion = %q, want 113-APM7489-DS2-100", gpu.VBIOSVersion)
	}
	if gpu.VRAMVendor != "samsung" {
		t.Errorf("VRAMVendor = %q, want samsung", gpu.VRAMVendor)
	}
	if gpu.PCIeLinkWidth != 16 {
		t.Errorf("PCIeLinkWidth = %d, want 16", gpu.PCIeLinkWidth)
	}
	if gpu.ThermalLimitCriticalMillidegrees != 100000 {
		t.Errorf("ThermalLimitCritical = %d, want 100000", gpu.ThermalLimitCriticalMillidegrees)
	}
	if gpu.ThermalLimitEmergencyMillidegrees != 105000 {
		t.Errorf("ThermalLimitEmergency = %d, want 105000", gpu.ThermalLimitEmergencyMillidegrees)
	}
	if gpu.Driver != "amdgpu" {
		t.Errorf("Driver = %q, want amdgpu", gpu.Driver)
	}
}

func TestEnumerateMultipleGPUs(t *testing.T) {
	root := t.TempDir()
	createSyntheticAMDGPU(t, root, 0, "0000:c3:00.0")
	createSyntheticAMDGPU(t, root, 1, "0000:e3:00.0")

	prober := newProberFrom(filepath.Join(root, "sys"))
	gpus := prober.Enumerate()

	if len(gpus) != 2 {
		t.Fatalf("Enumerate() returned %d GPUs, want 2", len(gpus))
	}

	// Verify both PCI slots are present (order may vary).
	slots := make(map[string]bool)
	for _, gpu := range gpus {
		slots[gpu.PCISlot] = true
	}
	if !slots["0000:c3:00.0"] {
		t.Error("missing GPU at PCI slot 0000:c3:00.0")
	}
	if !slots["0000:e3:00.0"] {
		t.Error("missing GPU at PCI slot 0000:e3:00.0")
	}
}

func TestEnumerateSkipsNonAMDGPU(t *testing.T) {
	root := t.TempDir()
	createSyntheticAMDGPU(t, root, 0, "0000:c3:00.0")

	// Create a non-amdgpu card (AST BMC graphics).
	cardPath := "sys/class/drm/card1"
	astDriverDir := filepath.Join(root, "sys/bus/pci/drivers/ast")
	if err := os.MkdirAll(astDriverDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	deviceDir := filepath.Join(root, cardPath, "device")
	if err := os.MkdirAll(deviceDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSyntheticSymlink(t, root, filepath.Join(cardPath, "device", "driver"), astDriverDir)

	prober := newProberFrom(filepath.Join(root, "sys"))
	gpus := prober.Enumerate()

	if len(gpus) != 1 {
		t.Fatalf("Enumerate() returned %d GPUs, want 1 (should skip ast)", len(gpus))
	}
	if gpus[0].Vendor != "AMD" {
		t.Errorf("Vendor = %q, want AMD", gpus[0].Vendor)
	}
}

func TestEnumerateNoGPUs(t *testing.T) {
	root := t.TempDir()
	// Empty sysfs — no DRM directory at all.
	prober := newProberFrom(filepath.Join(root, "sys"))
	gpus := prober.Enumerate()

	if len(gpus) != 0 {
		t.Errorf("Enumerate() returned %d GPUs, want 0", len(gpus))
	}
}

func TestEnumerateSkipsConnectors(t *testing.T) {
	root := t.TempDir()
	createSyntheticAMDGPU(t, root, 0, "0000:c3:00.0")

	// Create connector entries that should be ignored (card0-DP-1, etc.).
	connectorPath := filepath.Join(root, "sys/class/drm/card0-DP-1")
	if err := os.MkdirAll(connectorPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	prober := newProberFrom(filepath.Join(root, "sys"))
	gpus := prober.Enumerate()

	if len(gpus) != 1 {
		t.Fatalf("Enumerate() returned %d GPUs, want 1", len(gpus))
	}
}

func TestIsCardDevice(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"card0", true},
		{"card1", true},
		{"card12", true},
		{"card0-DP-1", false},
		{"card0-Writeback-2", false},
		{"renderD128", false},
		{"card", false},
		{"", false},
		{"version", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hwinfo.IsCardDevice(test.name); got != test.want {
				t.Errorf("IsCardDevice(%q) = %v, want %v", test.name, got, test.want)
			}
		})
	}
}

func TestPCIVendorName(t *testing.T) {
	tests := []struct {
		vendorID string
		want     string
	}{
		{"1002", "AMD"},
		{"10de", "NVIDIA"},
		{"8086", "Intel"},
		{"1a03", "0x1a03"},
		{"", ""},
	}
	for _, test := range tests {
		t.Run(test.vendorID, func(t *testing.T) {
			if got := hwinfo.PCIVendorName(test.vendorID); got != test.want {
				t.Errorf("PCIVendorName(%q) = %q, want %q", test.vendorID, got, test.want)
			}
		})
	}
}

func TestCollectorSysfsOnly(t *testing.T) {
	// Test that the collector can read VRAM usage from synthetic sysfs
	// even without a real render node (ioctl metrics will be zero).
	root := t.TempDir()
	createSyntheticAMDGPU(t, root, 0, "0000:c3:00.0")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	collector := newCollectorFrom(filepath.Join(root, "sys"), logger)
	defer collector.Close()

	stats := collector.Collect()
	if len(stats) != 1 {
		t.Fatalf("Collect() returned %d stats, want 1", len(stats))
	}

	if stats[0].PCISlot != "0000:c3:00.0" {
		t.Errorf("PCISlot = %q, want 0000:c3:00.0", stats[0].PCISlot)
	}
	if stats[0].VRAMUsedBytes != 27860992 {
		t.Errorf("VRAMUsedBytes = %d, want 27860992", stats[0].VRAMUsedBytes)
	}
	// Ioctl metrics should be zero (no render node available in test).
	if stats[0].UtilizationPercent != 0 {
		t.Errorf("UtilizationPercent = %d, want 0 (no render node)", stats[0].UtilizationPercent)
	}
}

func TestCollectorNoDevices(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	collector := newCollectorFrom(filepath.Join(root, "sys"), logger)
	defer collector.Close()

	stats := collector.Collect()
	if stats != nil {
		t.Errorf("Collect() = %v, want nil", stats)
	}
}

// TestLiveEnumerate runs against real sysfs on a machine with amdgpu devices.
// Skipped when no amdgpu devices are present.
func TestLiveEnumerate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping: requires Linux sysfs")
	}

	prober := NewProber()
	gpus := prober.Enumerate()

	if len(gpus) == 0 {
		t.Skip("skipping: no amdgpu devices found")
	}

	for i, gpu := range gpus {
		if gpu.Vendor != "AMD" {
			t.Errorf("GPU[%d].Vendor = %q, want AMD", i, gpu.Vendor)
		}
		if gpu.PCISlot == "" {
			t.Errorf("GPU[%d].PCISlot is empty", i)
		}
		if gpu.PCIDeviceID == "" {
			t.Errorf("GPU[%d].PCIDeviceID is empty", i)
		}
		if gpu.Driver != "amdgpu" {
			t.Errorf("GPU[%d].Driver = %q, want amdgpu", i, gpu.Driver)
		}
		if gpu.VRAMTotalBytes <= 0 {
			t.Errorf("GPU[%d].VRAMTotalBytes = %d, want > 0", i, gpu.VRAMTotalBytes)
		}
		t.Logf("GPU[%d]: %s device=%s slot=%s vram=%d MB uid=%s",
			i, gpu.Vendor, gpu.PCIDeviceID, gpu.PCISlot,
			gpu.VRAMTotalBytes/(1024*1024), gpu.UniqueID)
	}
}

// TestLiveCollect runs against real GPU hardware via DRM ioctls.
// Skipped when no amdgpu devices are present.
func TestLiveCollect(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping: requires Linux DRM")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	collector := NewCollector(logger)
	defer collector.Close()

	stats := collector.Collect()
	if len(stats) == 0 {
		t.Skip("skipping: no amdgpu devices found or no render node access")
	}

	for i, status := range stats {
		if status.PCISlot == "" {
			t.Errorf("stats[%d].PCISlot is empty", i)
		}
		// VRAM used should be non-negative (even idle GPUs allocate some VRAM).
		if status.VRAMUsedBytes < 0 {
			t.Errorf("stats[%d].VRAMUsedBytes = %d, want >= 0", i, status.VRAMUsedBytes)
		}
		// Temperature should be plausible (1-120C = 1000-120000 millidegrees).
		if status.TemperatureMillidegrees > 0 && status.TemperatureMillidegrees < 1000 {
			t.Errorf("stats[%d].TemperatureMillidegrees = %d, implausibly low", i, status.TemperatureMillidegrees)
		}
		t.Logf("stats[%d]: slot=%s util=%d%% vram_used=%d MB temp=%d mC power=%d W gfx=%d MHz mem=%d MHz",
			i, status.PCISlot, status.UtilizationPercent,
			status.VRAMUsedBytes/(1024*1024),
			status.TemperatureMillidegrees, status.PowerDrawWatts,
			status.GraphicsClockMHz, status.MemoryClockMHz)
	}
}
