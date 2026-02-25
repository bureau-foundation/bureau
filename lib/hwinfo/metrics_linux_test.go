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

func TestCPUPercentDelta(t *testing.T) {
	tests := []struct {
		name     string
		previous *CPUReading
		current  *CPUReading
		expected float64
	}{
		{
			name:     "50 percent utilization",
			previous: &CPUReading{Busy: 100, Idle: 100},
			current:  &CPUReading{Busy: 200, Idle: 200},
			expected: 50,
		},
		{
			name:     "100 percent utilization",
			previous: &CPUReading{Busy: 100, Idle: 100},
			current:  &CPUReading{Busy: 200, Idle: 100},
			expected: 100,
		},
		{
			name:     "0 percent utilization",
			previous: &CPUReading{Busy: 100, Idle: 100},
			current:  &CPUReading{Busy: 100, Idle: 200},
			expected: 0,
		},
		{
			name:     "75 percent utilization",
			previous: &CPUReading{Busy: 0, Idle: 0},
			current:  &CPUReading{Busy: 75, Idle: 25},
			expected: 75,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := CPUPercent(test.previous, test.current)
			if got != test.expected {
				t.Errorf("CPUPercent() = %f, want %f", got, test.expected)
			}
		})
	}
}

func TestCPUPercentNilInputs(t *testing.T) {
	current := &CPUReading{Busy: 100, Idle: 100}

	if got := CPUPercent(nil, current); got != 0 {
		t.Errorf("CPUPercent(nil, current) = %f, want 0", got)
	}
	if got := CPUPercent(current, nil); got != 0 {
		t.Errorf("CPUPercent(current, nil) = %f, want 0", got)
	}
	if got := CPUPercent(nil, nil); got != 0 {
		t.Errorf("CPUPercent(nil, nil) = %f, want 0", got)
	}
}

func TestCPUPercentZeroDelta(t *testing.T) {
	reading := &CPUReading{Busy: 100, Idle: 100}
	if got := CPUPercent(reading, reading); got != 0 {
		t.Errorf("CPUPercent with identical readings = %f, want 0", got)
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

	if reading.Busy != expectedBusy {
		t.Errorf("Busy = %d, want %d", reading.Busy, expectedBusy)
	}
	if reading.Idle != expectedIdle {
		t.Errorf("Idle = %d, want %d", reading.Idle, expectedIdle)
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

	reading := ReadCPUStats()
	if reading == nil {
		t.Fatal("ReadCPUStats returned nil on a Linux system")
	}

	// Sanity: on any running Linux system, some CPU time has been consumed.
	if reading.Busy == 0 && reading.Idle == 0 {
		t.Error("both Busy and Idle are zero, which is impossible on a running system")
	}
}

func TestMemoryUsedMB(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping: syscall.Sysinfo only available on Linux")
	}

	used := MemoryUsedMB()
	if used <= 0 {
		t.Errorf("MemoryUsedMB = %d, expected positive value", used)
	}
	// Sanity: used memory should be less than 100 TB in MB (catches unit conversion bugs).
	if used > 100*1024*1024 {
		t.Errorf("MemoryUsedMB = %d, unreasonably large (unit conversion bug?)", used)
	}
}

func TestReadCgroupCPUStats(t *testing.T) {
	directory := t.TempDir()

	// Realistic cgroup v2 cpu.stat content.
	content := "usage_usec 123456789\n" +
		"user_usec 100000000\n" +
		"system_usec 23456789\n" +
		"nr_periods 0\n" +
		"nr_throttled 0\n" +
		"throttled_usec 0\n"

	if err := os.WriteFile(filepath.Join(directory, "cpu.stat"), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reading := ReadCgroupCPUStats(directory)
	if reading == nil {
		t.Fatal("ReadCgroupCPUStats returned nil for valid cpu.stat content")
	}
	if reading.UsageUsec != 123456789 {
		t.Errorf("UsageUsec = %d, want 123456789", reading.UsageUsec)
	}
}

func TestReadCgroupCPUStatsMissingFile(t *testing.T) {
	reading := ReadCgroupCPUStats("/nonexistent/cgroup/path")
	if reading != nil {
		t.Errorf("ReadCgroupCPUStats should return nil for missing file, got %+v", reading)
	}
}

func TestReadCgroupCPUStatsEmptyFile(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "cpu.stat"), []byte(""), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reading := ReadCgroupCPUStats(directory)
	if reading != nil {
		t.Errorf("ReadCgroupCPUStats should return nil for empty file, got %+v", reading)
	}
}

func TestReadCgroupCPUStatsMissingUsageUsec(t *testing.T) {
	directory := t.TempDir()

	// cpu.stat without usage_usec line.
	content := "user_usec 100000000\n" +
		"system_usec 23456789\n"

	if err := os.WriteFile(filepath.Join(directory, "cpu.stat"), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reading := ReadCgroupCPUStats(directory)
	if reading != nil {
		t.Errorf("ReadCgroupCPUStats should return nil when usage_usec is absent, got %+v", reading)
	}
}

func TestCgroupCPUPercent(t *testing.T) {
	tests := []struct {
		name                 string
		previous             *CgroupCPUReading
		current              *CgroupCPUReading
		intervalMicroseconds uint64
		expected             int
	}{
		{
			name:                 "50 percent of one core",
			previous:             &CgroupCPUReading{UsageUsec: 1000000},
			current:              &CgroupCPUReading{UsageUsec: 1500000},
			intervalMicroseconds: 1000000,
			expected:             50,
		},
		{
			name:                 "100 percent of one core",
			previous:             &CgroupCPUReading{UsageUsec: 0},
			current:              &CgroupCPUReading{UsageUsec: 1000000},
			intervalMicroseconds: 1000000,
			expected:             100,
		},
		{
			name:                 "250 percent multi-core",
			previous:             &CgroupCPUReading{UsageUsec: 0},
			current:              &CgroupCPUReading{UsageUsec: 2500000},
			intervalMicroseconds: 1000000,
			expected:             250,
		},
		{
			name:                 "zero delta",
			previous:             &CgroupCPUReading{UsageUsec: 5000},
			current:              &CgroupCPUReading{UsageUsec: 5000},
			intervalMicroseconds: 1000000,
			expected:             0,
		},
		{
			name:                 "realistic 60s interval at 30 percent",
			previous:             &CgroupCPUReading{UsageUsec: 100000000},
			current:              &CgroupCPUReading{UsageUsec: 118000000},
			intervalMicroseconds: 60000000,
			expected:             30,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := CgroupCPUPercent(test.previous, test.current, test.intervalMicroseconds)
			if got != test.expected {
				t.Errorf("CgroupCPUPercent() = %d, want %d", got, test.expected)
			}
		})
	}
}

func TestCgroupCPUPercentNilReadings(t *testing.T) {
	current := &CgroupCPUReading{UsageUsec: 1000000}

	if got := CgroupCPUPercent(nil, current, 1000000); got != 0 {
		t.Errorf("CgroupCPUPercent(nil, current, ...) = %d, want 0", got)
	}
	if got := CgroupCPUPercent(current, nil, 1000000); got != 0 {
		t.Errorf("CgroupCPUPercent(current, nil, ...) = %d, want 0", got)
	}
	if got := CgroupCPUPercent(nil, nil, 1000000); got != 0 {
		t.Errorf("CgroupCPUPercent(nil, nil, ...) = %d, want 0", got)
	}
}

func TestCgroupCPUPercentZeroInterval(t *testing.T) {
	previous := &CgroupCPUReading{UsageUsec: 1000000}
	current := &CgroupCPUReading{UsageUsec: 2000000}
	if got := CgroupCPUPercent(previous, current, 0); got != 0 {
		t.Errorf("CgroupCPUPercent with zero interval = %d, want 0", got)
	}
}

func TestCgroupCPUPercentBackwardsClock(t *testing.T) {
	// If current < previous (shouldn't happen, but handle gracefully).
	previous := &CgroupCPUReading{UsageUsec: 2000000}
	current := &CgroupCPUReading{UsageUsec: 1000000}
	if got := CgroupCPUPercent(previous, current, 1000000); got != 0 {
		t.Errorf("CgroupCPUPercent with backwards clock = %d, want 0", got)
	}
}

func TestReadCgroupMemoryBytes(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "memory.current"), []byte("12345678\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := ReadCgroupMemoryBytes(directory)
	if got != 12345678 {
		t.Errorf("ReadCgroupMemoryBytes = %d, want 12345678", got)
	}
}

func TestReadCgroupMemoryBytesLargeValue(t *testing.T) {
	directory := t.TempDir()
	// 8 GB in bytes.
	if err := os.WriteFile(filepath.Join(directory, "memory.current"), []byte("8589934592\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := ReadCgroupMemoryBytes(directory)
	if got != 8589934592 {
		t.Errorf("ReadCgroupMemoryBytes = %d, want 8589934592", got)
	}
}

func TestReadCgroupMemoryBytesMissingFile(t *testing.T) {
	got := ReadCgroupMemoryBytes("/nonexistent/cgroup/path")
	if got != 0 {
		t.Errorf("ReadCgroupMemoryBytes for missing file = %d, want 0", got)
	}
}

func TestReadCgroupMemoryBytesEmptyFile(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "memory.current"), []byte(""), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := ReadCgroupMemoryBytes(directory)
	if got != 0 {
		t.Errorf("ReadCgroupMemoryBytes for empty file = %d, want 0", got)
	}
}

func TestReadCgroupMemoryBytesNonNumeric(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "memory.current"), []byte("not-a-number\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := ReadCgroupMemoryBytes(directory)
	if got != 0 {
		t.Errorf("ReadCgroupMemoryBytes for non-numeric = %d, want 0", got)
	}
}

func TestDerivePrincipalStatus(t *testing.T) {
	tests := []struct {
		name               string
		cpuPercent         int
		hasPreviousReading bool
		expected           schema.PrincipalRunStatus
	}{
		{
			name:               "running with high cpu",
			cpuPercent:         50,
			hasPreviousReading: true,
			expected:           schema.PrincipalRunning,
		},
		{
			name:               "running at threshold",
			cpuPercent:         2,
			hasPreviousReading: true,
			expected:           schema.PrincipalRunning,
		},
		{
			name:               "idle with low cpu",
			cpuPercent:         1,
			hasPreviousReading: true,
			expected:           schema.PrincipalIdle,
		},
		{
			name:               "idle with zero cpu",
			cpuPercent:         0,
			hasPreviousReading: true,
			expected:           schema.PrincipalIdle,
		},
		{
			name:               "starting without previous reading",
			cpuPercent:         0,
			hasPreviousReading: false,
			expected:           schema.PrincipalStarting,
		},
		{
			name:               "starting even with high cpu but no previous",
			cpuPercent:         80,
			hasPreviousReading: false,
			expected:           schema.PrincipalStarting,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := DerivePrincipalStatus(test.cpuPercent, test.hasPreviousReading)
			if got != test.expected {
				t.Errorf("DerivePrincipalStatus(%d, %v) = %q, want %q",
					test.cpuPercent, test.hasPreviousReading, got, test.expected)
			}
		})
	}
}

func TestCgroupDefaultPath(t *testing.T) {
	tests := []struct {
		name      string
		localpart string
		expected  string
	}{
		{
			name:      "fleet-scoped agent",
			localpart: "bureau/fleet/prod/agent/codegen",
			expected:  "/sys/fs/cgroup/bureau/bureau-fleet-prod-agent-codegen",
		},
		{
			name:      "fleet-scoped service with hierarchy",
			localpart: "bureau/fleet/prod/service/stt/whisper",
			expected:  "/sys/fs/cgroup/bureau/bureau-fleet-prod-service-stt-whisper",
		},
		{
			name:      "fleet-scoped machine",
			localpart: "bureau/fleet/prod/machine/workstation",
			expected:  "/sys/fs/cgroup/bureau/bureau-fleet-prod-machine-workstation",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := CgroupDefaultPath(test.localpart)
			if got != test.expected {
				t.Errorf("CgroupDefaultPath(%q) = %q, want %q",
					test.localpart, got, test.expected)
			}
		})
	}
}
