// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCPUPercentDelta(t *testing.T) {
	tests := []struct {
		name     string
		previous *cpuReading
		current  *cpuReading
		expected float64
	}{
		{
			name:     "50 percent utilization",
			previous: &cpuReading{busy: 100, idle: 100},
			current:  &cpuReading{busy: 200, idle: 200},
			expected: 50,
		},
		{
			name:     "100 percent utilization",
			previous: &cpuReading{busy: 100, idle: 100},
			current:  &cpuReading{busy: 200, idle: 100},
			expected: 100,
		},
		{
			name:     "0 percent utilization",
			previous: &cpuReading{busy: 100, idle: 100},
			current:  &cpuReading{busy: 100, idle: 200},
			expected: 0,
		},
		{
			name:     "75 percent utilization",
			previous: &cpuReading{busy: 0, idle: 0},
			current:  &cpuReading{busy: 75, idle: 25},
			expected: 75,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := cpuPercent(test.previous, test.current)
			if got != test.expected {
				t.Errorf("cpuPercent() = %f, want %f", got, test.expected)
			}
		})
	}
}

func TestCPUPercentNilInputs(t *testing.T) {
	current := &cpuReading{busy: 100, idle: 100}

	if got := cpuPercent(nil, current); got != 0 {
		t.Errorf("cpuPercent(nil, current) = %f, want 0", got)
	}
	if got := cpuPercent(current, nil); got != 0 {
		t.Errorf("cpuPercent(current, nil) = %f, want 0", got)
	}
	if got := cpuPercent(nil, nil); got != 0 {
		t.Errorf("cpuPercent(nil, nil) = %f, want 0", got)
	}
}

func TestCPUPercentZeroDelta(t *testing.T) {
	reading := &cpuReading{busy: 100, idle: 100}
	if got := cpuPercent(reading, reading); got != 0 {
		t.Errorf("cpuPercent with identical readings = %f, want 0", got)
	}
}

func TestReadCPUStatsFromSyntheticFile(t *testing.T) {
	directory := t.TempDir()
	statPath := filepath.Join(directory, "stat")

	// Realistic /proc/stat content (first line is the aggregate CPU line).
	content := "cpu  851491738 26345625 738865283 5623198410 28471623 0 15284567 2345678 0 0\n" +
		"cpu0 106436467 3293203 92358160 702899801 3558952 0 1910570 293209 0 0\n"

	if err := os.WriteFile(statPath, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reading := readCPUStatsFrom(statPath)
	if reading == nil {
		t.Fatal("readCPUStatsFrom returned nil for valid /proc/stat content")
	}

	// busy = user(851491738) + nice(26345625) + system(738865283) + irq(0) + softirq(15284567) + steal(2345678)
	expectedBusy := uint64(851491738 + 26345625 + 738865283 + 0 + 15284567 + 2345678)
	// idle = idle(5623198410) + iowait(28471623)
	expectedIdle := uint64(5623198410 + 28471623)

	if reading.busy != expectedBusy {
		t.Errorf("busy = %d, want %d", reading.busy, expectedBusy)
	}
	if reading.idle != expectedIdle {
		t.Errorf("idle = %d, want %d", reading.idle, expectedIdle)
	}
}

func TestReadCPUStatsFromMalformedFile(t *testing.T) {
	directory := t.TempDir()

	tests := []struct {
		name    string
		content string
	}{
		{"empty file", ""},
		{"wrong label", "mem  123 456 789 0 0 0 0 0\n"},
		{"too few fields", "cpu  123 456\n"},
		{"non-numeric field", "cpu  123 abc 789 0 0 0 0 0\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			statPath := filepath.Join(directory, test.name+".stat")
			if err := os.WriteFile(statPath, []byte(test.content), 0644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			reading := readCPUStatsFrom(statPath)
			if reading != nil {
				t.Errorf("readCPUStatsFrom should return nil for malformed input, got %+v", reading)
			}
		})
	}
}

func TestReadCPUStatsFromMissingFile(t *testing.T) {
	reading := readCPUStatsFrom("/nonexistent/proc/stat")
	if reading != nil {
		t.Errorf("readCPUStatsFrom should return nil for missing file, got %+v", reading)
	}
}

func TestReadCPUStatsLiveSystem(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping: /proc/stat only available on Linux")
	}

	reading := readCPUStats()
	if reading == nil {
		t.Fatal("readCPUStats returned nil on a Linux system")
	}

	// Sanity: on any running Linux system, some CPU time has been consumed.
	if reading.busy == 0 && reading.idle == 0 {
		t.Error("both busy and idle are zero, which is impossible on a running system")
	}
}

func TestMemoryUsedGB(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping: syscall.Sysinfo only available on Linux")
	}

	used := memoryUsedGB()
	if used <= 0 {
		t.Errorf("memoryUsedGB = %f, expected positive value", used)
	}
	// Sanity: used memory should be less than 100 TB (catches unit conversion bugs).
	if used > 100*1024 {
		t.Errorf("memoryUsedGB = %f, unreasonably large (unit conversion bug?)", used)
	}
}
