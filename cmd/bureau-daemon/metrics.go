// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// cpuReading captures cumulative CPU time from /proc/stat for delta
// computation. The first line of /proc/stat aggregates all CPUs:
//
//	cpu  user nice system idle iowait irq softirq steal guest guest_nice
//
// busy = user + nice + system + irq + softirq + steal
// idle = idle + iowait
//
// guest and guest_nice are already included in user/nice (kernel
// accounting) so they are not added separately.
type cpuReading struct {
	busy uint64
	idle uint64
}

// readCPUStats parses the first line of /proc/stat and returns the
// cumulative busy and idle jiffies. Returns nil on any parse failure
// (the caller treats nil as "no reading available, report 0%").
func readCPUStats() *cpuReading {
	return readCPUStatsFrom("/proc/stat")
}

// readCPUStatsFrom is the testable version of readCPUStats that accepts
// a file path. Exported only within the package for testing.
func readCPUStatsFrom(path string) *cpuReading {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return nil
	}
	line := scanner.Text()

	// Expected format: "cpu  user nice system idle iowait irq softirq steal [guest guest_nice]"
	// The leading "cpu" label and at least 8 numeric fields must be present.
	fields := strings.Fields(line)
	if len(fields) < 9 || fields[0] != "cpu" {
		return nil
	}

	values := make([]uint64, len(fields)-1)
	for i := 1; i < len(fields); i++ {
		parsed, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return nil
		}
		values[i-1] = parsed
	}

	// Fields (0-indexed after stripping "cpu"):
	//   0=user, 1=nice, 2=system, 3=idle, 4=iowait,
	//   5=irq, 6=softirq, 7=steal
	busy := values[0] + values[1] + values[2] + values[5] + values[6] + values[7]
	idle := values[3] + values[4]

	return &cpuReading{busy: busy, idle: idle}
}

// cpuPercent computes the CPU utilization percentage from two sequential
// /proc/stat readings. Returns 0 if either reading is nil or the delta
// is zero (no time has passed).
func cpuPercent(previous, current *cpuReading) float64 {
	if previous == nil || current == nil {
		return 0
	}
	busyDelta := current.busy - previous.busy
	idleDelta := current.idle - previous.idle
	totalDelta := busyDelta + idleDelta
	if totalDelta == 0 {
		return 0
	}
	return float64(busyDelta) / float64(totalDelta) * 100
}

// memoryUsedMB returns the current system memory usage in megabytes.
// Uses syscall.Sysinfo (same call used by uptimeSeconds in main.go).
// Returns 0 if the syscall fails. Integer megabytes comply with Matrix
// canonical JSON (which forbids fractional numbers).
func memoryUsedMB() int {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0
	}
	totalBytes := uint64(info.Totalram) * uint64(info.Unit)
	freeBytes := uint64(info.Freeram) * uint64(info.Unit)
	if totalBytes < freeBytes {
		return 0
	}
	return int((totalBytes - freeBytes) / (1024 * 1024))
}

// cgroupCPUReading captures cumulative CPU time from a cgroup v2
// cpu.stat file for delta computation across heartbeat intervals.
type cgroupCPUReading struct {
	usageUsec uint64
}

// readCgroupCPUStats reads usage_usec from a cgroup v2 cpu.stat file.
// The cpu.stat format is a series of "key value" lines; this function
// extracts the usage_usec field (total CPU time in microseconds).
// Returns nil if the file doesn't exist or the usage_usec line is
// absent or unparseable.
func readCgroupCPUStats(cgroupPath string) *cgroupCPUReading {
	data, err := os.ReadFile(filepath.Join(cgroupPath, "cpu.stat"))
	if err != nil {
		return nil
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "usage_usec" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return nil
			}
			return &cgroupCPUReading{usageUsec: value}
		}
	}
	return nil
}

// cgroupCPUPercent computes cgroup CPU utilization from two sequential
// cpu.stat readings over the given interval. Returns the percentage
// of one CPU core: a single-core fully utilized cgroup returns 100;
// a cgroup using 2.5 cores returns 250. Returns 0 if either reading
// is nil or the interval is zero.
func cgroupCPUPercent(previous, current *cgroupCPUReading, intervalMicroseconds uint64) int {
	if previous == nil || current == nil || intervalMicroseconds == 0 {
		return 0
	}
	if current.usageUsec < previous.usageUsec {
		return 0
	}
	deltaUsec := current.usageUsec - previous.usageUsec
	return int(deltaUsec * 100 / intervalMicroseconds)
}

// readCgroupMemoryBytes reads memory.current from a cgroup v2 directory.
// The file contains a single integer: the current memory usage in bytes.
// Returns 0 if the file doesn't exist or can't be parsed.
func readCgroupMemoryBytes(cgroupPath string) uint64 {
	data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.current"))
	if err != nil {
		return 0
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// cgroupDefaultPath returns the default cgroup v2 path for a Bureau
// sandbox. The launcher creates sandboxes in a dedicated cgroup
// hierarchy under /sys/fs/cgroup/bureau/<localpart>/, with slashes
// in the localpart replaced by dashes (e.g., "service/stt/whisper"
// becomes "service-stt-whisper").
func cgroupDefaultPath(localpart string) string {
	return "/sys/fs/cgroup/bureau/" + strings.ReplaceAll(localpart, "/", "-")
}

// derivePrincipalStatus determines the lifecycle status of a principal
// from its cgroup metrics.
//   - "running" if CPU > 1% (actively working)
//   - "idle" if CPU <= 1% and we have a previous reading (has been running
//     long enough for a delta computation)
//   - "starting" if no previous CPU reading exists (first heartbeat after
//     sandbox creation, no baseline for delta)
func derivePrincipalStatus(cpuPercent int, hasPreviousReading bool) string {
	if !hasPreviousReading {
		return "starting"
	}
	if cpuPercent > 1 {
		return "running"
	}
	return "idle"
}
