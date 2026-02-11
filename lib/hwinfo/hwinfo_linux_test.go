// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package hwinfo

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
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

// stubGPUProber returns a fixed list of GPUInfo for testing.
type stubGPUProber struct {
	gpus []schema.GPUInfo
}

func (s *stubGPUProber) Enumerate() []schema.GPUInfo {
	return s.gpus
}

func TestProbeFromSyntheticFS(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	sysRoot := filepath.Join(root, "sys")

	// /proc/cpuinfo — two cores, one socket, two threads per core.
	writeSyntheticFile(t, root, "proc/cpuinfo",
		"processor\t: 0\nmodel name\t: AMD EPYC 7763 64-Core Processor\n\n"+
			"processor\t: 1\nmodel name\t: AMD EPYC 7763 64-Core Processor\n\n")

	// CPU topology: 1 socket, 2 cores, 2 threads per core (4 logical CPUs).
	for i, config := range []struct {
		packageID, coreID, siblings string
	}{
		{"0", "0", "0,2"},
		{"0", "1", "1,3"},
		{"0", "0", "0,2"},
		{"0", "1", "1,3"},
	} {
		cpuDir := filepath.Join("sys/devices/system/cpu", "cpu"+string(rune('0'+i)), "topology")
		writeSyntheticFile(t, root, filepath.Join(cpuDir, "physical_package_id"), config.packageID)
		writeSyntheticFile(t, root, filepath.Join(cpuDir, "core_id"), config.coreID)
		writeSyntheticFile(t, root, filepath.Join(cpuDir, "thread_siblings_list"), config.siblings)
	}

	// L3 cache.
	writeSyntheticFile(t, root, "sys/devices/system/cpu/cpu0/cache/index3/size", "32768K")

	// NUMA nodes.
	for _, node := range []string{"node0", "node1"} {
		if err := os.MkdirAll(filepath.Join(sysRoot, "devices/system/node", node), 0755); err != nil {
			t.Fatalf("mkdir node: %v", err)
		}
	}

	// DMI.
	writeSyntheticFile(t, root, "sys/class/dmi/id/sys_vendor", "ASUS\n")
	writeSyntheticFile(t, root, "sys/class/dmi/id/board_name", "Pro WS WRX90E-SAGE SE\n")

	// Stub GPU prober returning one GPU.
	prober := &stubGPUProber{
		gpus: []schema.GPUInfo{
			{
				Vendor:         "AMD",
				PCIDeviceID:    "0x744a",
				PCISlot:        "0000:c3:00.0",
				VRAMTotalBytes: 48301604864,
				Driver:         "amdgpu",
			},
		},
	}

	info := probeFrom("@machine/test:bureau.local", procRoot, sysRoot, prober)

	if info.Principal != "@machine/test:bureau.local" {
		t.Errorf("Principal = %q, want @machine/test:bureau.local", info.Principal)
	}
	if info.BoardVendor != "ASUS" {
		t.Errorf("BoardVendor = %q, want ASUS", info.BoardVendor)
	}
	if info.BoardName != "Pro WS WRX90E-SAGE SE" {
		t.Errorf("BoardName = %q, want Pro WS WRX90E-SAGE SE", info.BoardName)
	}
	if info.CPU.Model != "AMD EPYC 7763 64-Core Processor" {
		t.Errorf("CPU.Model = %q, want AMD EPYC 7763 64-Core Processor", info.CPU.Model)
	}
	if info.CPU.Sockets != 1 {
		t.Errorf("CPU.Sockets = %d, want 1", info.CPU.Sockets)
	}
	if info.CPU.CoresPerSocket != 2 {
		t.Errorf("CPU.CoresPerSocket = %d, want 2", info.CPU.CoresPerSocket)
	}
	if info.CPU.ThreadsPerCore != 2 {
		t.Errorf("CPU.ThreadsPerCore = %d, want 2", info.CPU.ThreadsPerCore)
	}
	if info.CPU.L3CacheKB != 32768 {
		t.Errorf("CPU.L3CacheKB = %d, want 32768", info.CPU.L3CacheKB)
	}
	if info.NUMANodes != 2 {
		t.Errorf("NUMANodes = %d, want 2", info.NUMANodes)
	}
	if len(info.GPUs) != 1 {
		t.Fatalf("GPUs count = %d, want 1", len(info.GPUs))
	}
	if info.GPUs[0].Vendor != "AMD" {
		t.Errorf("GPUs[0].Vendor = %q, want AMD", info.GPUs[0].Vendor)
	}
	if info.GPUs[0].PCISlot != "0000:c3:00.0" {
		t.Errorf("GPUs[0].PCISlot = %q, want 0000:c3:00.0", info.GPUs[0].PCISlot)
	}
	if info.DaemonVersion == "" {
		t.Error("DaemonVersion should not be empty")
	}
}

func TestProbeFromEmptyFS(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	sysRoot := filepath.Join(root, "sys")

	// No files created — simulates a minimal container environment.
	info := probeFrom("@machine/empty:bureau.local", procRoot, sysRoot)

	if info.Principal != "@machine/empty:bureau.local" {
		t.Errorf("Principal = %q, want @machine/empty:bureau.local", info.Principal)
	}
	// CPU fields should be zero/empty, not panicked.
	if info.CPU.Model != "" {
		t.Errorf("CPU.Model = %q, want empty", info.CPU.Model)
	}
	if info.CPU.Sockets != 0 {
		t.Errorf("CPU.Sockets = %d, want 0", info.CPU.Sockets)
	}
	if info.NUMANodes != 0 {
		t.Errorf("NUMANodes = %d, want 0", info.NUMANodes)
	}
	if len(info.GPUs) != 0 {
		t.Errorf("GPUs count = %d, want 0", len(info.GPUs))
	}
}

func TestProbeFromMultiSocket(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	sysRoot := filepath.Join(root, "sys")

	writeSyntheticFile(t, root, "proc/cpuinfo",
		"processor\t: 0\nmodel name\t: Intel Xeon Gold 6248\n\n")

	// Two sockets, 2 cores per socket, 1 thread per core (no SMT).
	configs := []struct {
		cpu, packageID, coreID, siblings string
	}{
		{"cpu0", "0", "0", "0"},
		{"cpu1", "0", "1", "1"},
		{"cpu2", "1", "0", "2"},
		{"cpu3", "1", "1", "3"},
	}
	for _, config := range configs {
		topologyDir := filepath.Join("sys/devices/system/cpu", config.cpu, "topology")
		writeSyntheticFile(t, root, filepath.Join(topologyDir, "physical_package_id"), config.packageID)
		writeSyntheticFile(t, root, filepath.Join(topologyDir, "core_id"), config.coreID)
		writeSyntheticFile(t, root, filepath.Join(topologyDir, "thread_siblings_list"), config.siblings)
	}

	info := probeFrom("@machine/dual:bureau.local", procRoot, sysRoot)

	if info.CPU.Sockets != 2 {
		t.Errorf("CPU.Sockets = %d, want 2", info.CPU.Sockets)
	}
	if info.CPU.CoresPerSocket != 2 {
		t.Errorf("CPU.CoresPerSocket = %d, want 2", info.CPU.CoresPerSocket)
	}
	if info.CPU.ThreadsPerCore != 1 {
		t.Errorf("CPU.ThreadsPerCore = %d, want 1", info.CPU.ThreadsPerCore)
	}
}

func TestReadCacheSize(t *testing.T) {
	directory := t.TempDir()

	tests := []struct {
		name    string
		content string
		want    int
	}{
		{"standard", "32768K", 32768},
		{"small", "256K", 256},
		{"empty", "", 0},
		{"no_suffix", "1024", 1024},
		{"garbage", "fooK", 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(directory, test.name)
			if test.content != "" {
				if err := os.WriteFile(path, []byte(test.content), 0644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			} else {
				path = filepath.Join(directory, "nonexistent")
			}
			got := readCacheSize(path)
			if got != test.want {
				t.Errorf("readCacheSize(%q) = %d, want %d", test.content, got, test.want)
			}
		})
	}
}

func TestCountNUMANodes(t *testing.T) {
	root := t.TempDir()
	sysRoot := filepath.Join(root, "sys")
	nodeBase := filepath.Join(sysRoot, "devices/system/node")

	// No node directory at all.
	if count := countNUMANodes(sysRoot); count != 0 {
		t.Errorf("countNUMANodes(empty) = %d, want 0", count)
	}

	// Create node0, node1, and a non-node directory.
	for _, name := range []string{"node0", "node1", "nodestats"} {
		if err := os.MkdirAll(filepath.Join(nodeBase, name), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// nodestats should not be counted (suffix doesn't start with digit).
	if count := countNUMANodes(sysRoot); count != 2 {
		t.Errorf("countNUMANodes = %d, want 2", count)
	}
}

func TestProbeMultipleGPUProbers(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	sysRoot := filepath.Join(root, "sys")

	amdProber := &stubGPUProber{
		gpus: []schema.GPUInfo{
			{Vendor: "AMD", PCISlot: "0000:c3:00.0", Driver: "amdgpu"},
		},
	}
	nvidiaProber := &stubGPUProber{
		gpus: []schema.GPUInfo{
			{Vendor: "NVIDIA", PCISlot: "0000:41:00.0", Driver: "nvidia"},
			{Vendor: "NVIDIA", PCISlot: "0000:42:00.0", Driver: "nvidia"},
		},
	}

	info := probeFrom("@machine/mixed:bureau.local", procRoot, sysRoot, amdProber, nvidiaProber)

	if len(info.GPUs) != 3 {
		t.Fatalf("GPUs count = %d, want 3", len(info.GPUs))
	}
	if info.GPUs[0].Vendor != "AMD" {
		t.Errorf("GPUs[0].Vendor = %q, want AMD", info.GPUs[0].Vendor)
	}
	if info.GPUs[1].Vendor != "NVIDIA" {
		t.Errorf("GPUs[1].Vendor = %q, want NVIDIA", info.GPUs[1].Vendor)
	}
	if info.GPUs[2].Vendor != "NVIDIA" {
		t.Errorf("GPUs[2].Vendor = %q, want NVIDIA", info.GPUs[2].Vendor)
	}
}

func TestProbeLiveSystem(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping: requires Linux /proc and /sys")
	}

	info := Probe("@machine/live-test:bureau.local")

	// On any running Linux system these should be non-empty.
	if info.Hostname == "" {
		t.Error("Hostname should not be empty on a live system")
	}
	if info.KernelVersion == "" {
		t.Error("KernelVersion should not be empty on a live system")
	}
	if info.CPU.Model == "" {
		t.Error("CPU.Model should not be empty on a live system")
	}
	if info.CPU.Sockets < 1 {
		t.Errorf("CPU.Sockets = %d, want >= 1", info.CPU.Sockets)
	}
	if info.CPU.CoresPerSocket < 1 {
		t.Errorf("CPU.CoresPerSocket = %d, want >= 1", info.CPU.CoresPerSocket)
	}
	if info.CPU.ThreadsPerCore < 1 {
		t.Errorf("CPU.ThreadsPerCore = %d, want >= 1", info.CPU.ThreadsPerCore)
	}
	if info.MemoryTotalMB < 1 {
		t.Errorf("MemoryTotalMB = %d, want >= 1", info.MemoryTotalMB)
	}
	if info.DaemonVersion == "" {
		t.Error("DaemonVersion should not be empty")
	}
}
