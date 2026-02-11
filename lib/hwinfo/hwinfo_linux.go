// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package hwinfo

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/version"
)

// Probe collects static system inventory and returns a MachineInfo
// suitable for publishing as an m.bureau.machine_info state event.
// The principal and gpuProbers parameters are provided by the caller:
// principal is the machine's full Matrix user ID, and gpuProbers are
// the vendor-specific GPU enumerators (amdgpu.Prober, nvidia.Prober,
// etc.).
//
// Probe never returns an error — missing or unreadable files produce
// zero-valued fields rather than failures. A headless VM with no DMI,
// no GPUs, and no NUMA is a valid machine that should still report
// its CPU and memory.
func Probe(principal string, gpuProbers ...GPUProber) schema.MachineInfo {
	return probeFrom(principal, "/proc", "/sys", gpuProbers...)
}

// probeFrom is the testable implementation of Probe. It accepts root
// paths for /proc and /sys so tests can point at synthetic filesystems.
func probeFrom(principal, procRoot, sysRoot string, gpuProbers ...GPUProber) schema.MachineInfo {
	info := schema.MachineInfo{
		Principal:     principal,
		DaemonVersion: version.Short(),
	}

	info.Hostname, _ = os.Hostname()
	info.KernelVersion = readKernelVersion()
	info.BoardVendor = ReadSysfsString(filepath.Join(sysRoot, "class/dmi/id/sys_vendor"))
	info.BoardName = ReadSysfsString(filepath.Join(sysRoot, "class/dmi/id/board_name"))
	info.CPU = probeCPU(procRoot, sysRoot)
	info.MemoryTotalMB, info.SwapTotalMB = probeMemory()
	info.NUMANodes = countNUMANodes(sysRoot)

	for _, prober := range gpuProbers {
		info.GPUs = append(info.GPUs, prober.Enumerate()...)
	}

	return info
}

// readKernelVersion returns the kernel release string from uname(2).
func readKernelVersion() string {
	var utsname syscall.Utsname
	if err := syscall.Uname(&utsname); err != nil {
		return ""
	}
	return utsNameToString(utsname.Release)
}

// utsNameToString converts a [65]int8 (or [65]byte on some platforms)
// from syscall.Utsname to a Go string, stopping at the first null byte.
func utsNameToString(field [65]int8) string {
	var buffer []byte
	for _, value := range field {
		if value == 0 {
			break
		}
		buffer = append(buffer, byte(value))
	}
	return string(buffer)
}

// probeCPU reads CPU topology from /proc/cpuinfo and /sys/devices/system/cpu/.
func probeCPU(procRoot, sysRoot string) schema.CPUInfo {
	info := schema.CPUInfo{}
	info.Model = readCPUModel(filepath.Join(procRoot, "cpuinfo"))

	cpuBase := filepath.Join(sysRoot, "devices/system/cpu")

	// Count unique physical_package_id values for socket count.
	sockets := countUniqueTopologyValues(cpuBase, "physical_package_id")
	if sockets > 0 {
		info.Sockets = sockets
	}

	// Count unique core_id values for cores per socket. On a multi-socket
	// system, core IDs repeat across sockets — we count per-socket by
	// dividing total unique (package_id, core_id) pairs by socket count.
	// On single-socket systems this simplifies to just counting unique core_ids.
	coresTotal := countUniqueCoreIDs(cpuBase)
	if coresTotal > 0 && info.Sockets > 0 {
		info.CoresPerSocket = coresTotal / info.Sockets
	}

	// Threads per core: count threads sharing the same core.
	info.ThreadsPerCore = probeThreadsPerCore(cpuBase)

	// L3 cache size from cpu0's cache index3.
	info.L3CacheKB = readCacheSize(filepath.Join(cpuBase, "cpu0/cache/index3/size"))

	return info
}

// readCPUModel extracts the first "model name" line from /proc/cpuinfo.
func readCPUModel(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

// countUniqueTopologyValues counts unique values of a topology field
// (e.g., physical_package_id) across all CPU directories.
func countUniqueTopologyValues(cpuBase, field string) int {
	entries, err := os.ReadDir(cpuBase)
	if err != nil {
		return 0
	}

	unique := make(map[string]struct{})
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "cpu") {
			continue
		}
		// Filter to cpuN directories (skip cpufreq, cpuidle, etc.)
		suffix := name[3:]
		if len(suffix) == 0 {
			continue
		}
		if suffix[0] < '0' || suffix[0] > '9' {
			continue
		}

		value := ReadSysfsString(filepath.Join(cpuBase, name, "topology", field))
		if value != "" {
			unique[value] = struct{}{}
		}
	}
	return len(unique)
}

// countUniqueCoreIDs counts unique (physical_package_id, core_id) pairs
// across all CPUs. This gives the total physical core count across all
// sockets.
func countUniqueCoreIDs(cpuBase string) int {
	entries, err := os.ReadDir(cpuBase)
	if err != nil {
		return 0
	}

	type coreKey struct {
		packageID string
		coreID    string
	}
	unique := make(map[coreKey]struct{})

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "cpu") {
			continue
		}
		suffix := name[3:]
		if len(suffix) == 0 || suffix[0] < '0' || suffix[0] > '9' {
			continue
		}

		topologyDir := filepath.Join(cpuBase, name, "topology")
		packageID := ReadSysfsString(filepath.Join(topologyDir, "physical_package_id"))
		coreID := ReadSysfsString(filepath.Join(topologyDir, "core_id"))
		if packageID != "" && coreID != "" {
			unique[coreKey{packageID, coreID}] = struct{}{}
		}
	}
	return len(unique)
}

// probeThreadsPerCore determines threads per core from the first CPU's
// thread_siblings_list. The format is "0,96" meaning CPUs 0 and 96
// share a core — so 2 threads per core. A value of "0" alone means 1.
func probeThreadsPerCore(cpuBase string) int {
	siblings := ReadSysfsString(filepath.Join(cpuBase, "cpu0/topology/thread_siblings_list"))
	if siblings == "" {
		return 1
	}
	// Count comma-separated entries. "0,96" → 2 entries, "0" → 1 entry.
	count := strings.Count(siblings, ",") + 1
	if count < 1 {
		return 1
	}
	return count
}

// readCacheSize parses a cache size file (e.g., "32768K") and returns
// the value in kilobytes.
func readCacheSize(path string) int {
	value := ReadSysfsString(path)
	if value == "" {
		return 0
	}
	// Strip trailing 'K' (the standard unit in Linux sysfs cache files).
	value = strings.TrimSuffix(value, "K")
	kilobytes, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return kilobytes
}

// probeMemory returns total RAM and swap in megabytes from sysinfo(2).
func probeMemory() (memoryTotalMB, swapTotalMB int) {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0, 0
	}
	unit := uint64(info.Unit)
	memoryTotalMB = int(uint64(info.Totalram) * unit / (1024 * 1024))
	swapTotalMB = int(uint64(info.Totalswap) * unit / (1024 * 1024))
	return memoryTotalMB, swapTotalMB
}

// countNUMANodes counts /sys/devices/system/node/node* directories.
func countNUMANodes(sysRoot string) int {
	nodeBase := filepath.Join(sysRoot, "devices/system/node")
	entries, err := os.ReadDir(nodeBase)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "node") {
			suffix := entry.Name()[4:]
			if len(suffix) > 0 && suffix[0] >= '0' && suffix[0] <= '9' {
				count++
			}
		}
	}
	return count
}
