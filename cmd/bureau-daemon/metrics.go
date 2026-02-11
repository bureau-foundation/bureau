// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"fmt"
	"os"
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

// memoryUsedGB returns the current system memory usage in gigabytes.
// Uses syscall.Sysinfo (same call used by uptimeSeconds in main.go).
// Returns 0 if the syscall fails.
func memoryUsedGB() float64 {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0
	}
	totalBytes := uint64(info.Totalram) * uint64(info.Unit)
	freeBytes := uint64(info.Freeram) * uint64(info.Unit)
	if totalBytes < freeBytes {
		return 0
	}
	return float64(totalBytes-freeBytes) / (1024 * 1024 * 1024)
}

// formatMemoryGB formats a memory value in GB with two decimal places,
// matching the precision appropriate for MachineStatus reporting.
func formatMemoryGB(gigabytes float64) string {
	return fmt.Sprintf("%.2f", gigabytes)
}
