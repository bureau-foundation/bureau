// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantCommand  []string
		wantExitFile string
		wantCapture  bool
		wantErr      bool
	}{
		{
			name:    "no arguments",
			args:    nil,
			wantErr: true,
		},
		{
			name:    "empty arguments",
			args:    []string{},
			wantErr: true,
		},
		{
			name:    "only separator",
			args:    []string{"--"},
			wantErr: true,
		},
		{
			name:        "command without separator",
			args:        []string{"/bin/sh", "-c", "echo hello"},
			wantCommand: []string{"/bin/sh", "-c", "echo hello"},
		},
		{
			name:        "command with separator",
			args:        []string{"--", "/bin/sh", "-c", "echo hello"},
			wantCommand: []string{"/bin/sh", "-c", "echo hello"},
		},
		{
			name:        "single command",
			args:        []string{"/path/to/sandbox.sh"},
			wantCommand: []string{"/path/to/sandbox.sh"},
		},
		{
			name:        "command starting with dash",
			args:        []string{"--", "--version"},
			wantCommand: []string{"--version"},
		},
		{
			name:         "exit code file flag with separator",
			args:         []string{"--exit-code-file=/tmp/exit-code", "--", "/bin/sh"},
			wantCommand:  []string{"/bin/sh"},
			wantExitFile: "/tmp/exit-code",
		},
		{
			name:         "exit code file flag without separator",
			args:         []string{"--exit-code-file=/tmp/exit-code", "/bin/sh", "-c", "exit 42"},
			wantCommand:  []string{"/bin/sh", "-c", "exit 42"},
			wantExitFile: "/tmp/exit-code",
		},
		{
			name:    "exit code file flag with empty path",
			args:    []string{"--exit-code-file=", "/bin/sh"},
			wantErr: true,
		},
		{
			name:    "only exit code file flag",
			args:    []string{"--exit-code-file=/tmp/exit-code", "--"},
			wantErr: true,
		},
		{
			name: "capture mode all flags",
			args: []string{
				"--relay=/run/bureau/telemetry.sock",
				"--token=/run/bureau/token",
				"--fleet=#bureau/fleet/prod:bureau.local",
				"--machine=@bureau/fleet/prod/machine/workstation:bureau.local",
				"--source=@bureau/fleet/prod/agent/code-reviewer:bureau.local",
				"--session-id=sess-abc123",
				"--", "/bin/sh",
			},
			wantCommand: []string{"/bin/sh"},
			wantCapture: true,
		},
		{
			name: "capture mode without separator",
			args: []string{
				"--relay=/run/bureau/telemetry.sock",
				"--token=/run/bureau/token",
				"--fleet=#bureau/fleet/prod:bureau.local",
				"--machine=@bureau/fleet/prod/machine/workstation:bureau.local",
				"--source=@bureau/fleet/prod/agent/code-reviewer:bureau.local",
				"--session-id=sess-abc123",
				"/bin/sh", "-c", "echo hello",
			},
			wantCommand: []string{"/bin/sh", "-c", "echo hello"},
			wantCapture: true,
		},
		{
			name: "capture mode with exit code file",
			args: []string{
				"--exit-code-file=/tmp/ec",
				"--relay=/run/bureau/telemetry.sock",
				"--token=/run/bureau/token",
				"--fleet=#bureau/fleet/prod:bureau.local",
				"--machine=@bureau/fleet/prod/machine/workstation:bureau.local",
				"--source=@bureau/fleet/prod/agent/code-reviewer:bureau.local",
				"--session-id=sess-abc123",
				"--", "/bin/sh",
			},
			wantCommand:  []string{"/bin/sh"},
			wantExitFile: "/tmp/ec",
			wantCapture:  true,
		},
		{
			name: "partial capture flags — missing session-id",
			args: []string{
				"--relay=/run/bureau/telemetry.sock",
				"--token=/run/bureau/token",
				"--fleet=#bureau/fleet/prod:bureau.local",
				"--machine=@bureau/fleet/prod/machine/workstation:bureau.local",
				"--source=@bureau/fleet/prod/agent/code-reviewer:bureau.local",
				"/bin/sh",
			},
			wantErr: true,
		},
		{
			name: "partial capture flags — only relay",
			args: []string{
				"--relay=/run/bureau/telemetry.sock",
				"/bin/sh",
			},
			wantErr: true,
		},
		{
			name:    "capture flag with empty value",
			args:    []string{"--relay=", "/bin/sh"},
			wantErr: true,
		},
		{
			name: "invalid fleet ref",
			args: []string{
				"--relay=/run/bureau/telemetry.sock",
				"--token=/run/bureau/token",
				"--fleet=not-a-valid-fleet-ref",
				"--machine=@bureau/fleet/prod/machine/workstation:bureau.local",
				"--source=@bureau/fleet/prod/agent/code-reviewer:bureau.local",
				"--session-id=sess-abc123",
				"/bin/sh",
			},
			wantErr: true,
		},
		{
			name: "invalid machine ref",
			args: []string{
				"--relay=/run/bureau/telemetry.sock",
				"--token=/run/bureau/token",
				"--fleet=#bureau/fleet/prod:bureau.local",
				"--machine=not-a-valid-machine-ref",
				"--source=@bureau/fleet/prod/agent/code-reviewer:bureau.local",
				"--session-id=sess-abc123",
				"/bin/sh",
			},
			wantErr: true,
		},
		{
			name: "invalid source ref",
			args: []string{
				"--relay=/run/bureau/telemetry.sock",
				"--token=/run/bureau/token",
				"--fleet=#bureau/fleet/prod:bureau.local",
				"--machine=@bureau/fleet/prod/machine/workstation:bureau.local",
				"--source=not-a-valid-entity-ref",
				"--session-id=sess-abc123",
				"/bin/sh",
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := parseArgs(test.args)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(config.command) != len(test.wantCommand) {
				t.Fatalf("got %d command args, want %d: %v vs %v",
					len(config.command), len(test.wantCommand), config.command, test.wantCommand)
			}
			for i := range config.command {
				if config.command[i] != test.wantCommand[i] {
					t.Errorf("command[%d] = %q, want %q", i, config.command[i], test.wantCommand[i])
				}
			}
			if config.exitCodeFile != test.wantExitFile {
				t.Errorf("exitCodeFile = %q, want %q", config.exitCodeFile, test.wantExitFile)
			}
			if config.captureMode() != test.wantCapture {
				t.Errorf("captureMode() = %v, want %v", config.captureMode(), test.wantCapture)
			}
		})
	}
}

func TestParseArgsCaptureIdentity(t *testing.T) {
	// Verify that parsed identity refs round-trip correctly.
	config, err := parseArgs([]string{
		"--relay=/run/bureau/telemetry.sock",
		"--token=/run/bureau/token",
		"--fleet=#bureau/fleet/prod:bureau.local",
		"--machine=@bureau/fleet/prod/machine/workstation:bureau.local",
		"--source=@bureau/fleet/prod/agent/code-reviewer:bureau.local",
		"--session-id=sess-abc123",
		"--", "echo", "hello",
	})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}

	fleetText, _ := config.fleet.MarshalText()
	if string(fleetText) != "#bureau/fleet/prod:bureau.local" {
		t.Errorf("fleet = %q, want %q", fleetText, "#bureau/fleet/prod:bureau.local")
	}

	machineText, _ := config.machine.MarshalText()
	if string(machineText) != "@bureau/fleet/prod/machine/workstation:bureau.local" {
		t.Errorf("machine = %q, want %q", machineText, "@bureau/fleet/prod/machine/workstation:bureau.local")
	}

	sourceText, _ := config.source.MarshalText()
	if string(sourceText) != "@bureau/fleet/prod/agent/code-reviewer:bureau.local" {
		t.Errorf("source = %q, want %q", sourceText, "@bureau/fleet/prod/agent/code-reviewer:bureau.local")
	}

	if config.sessionID != "sess-abc123" {
		t.Errorf("sessionID = %q, want %q", config.sessionID, "sess-abc123")
	}
}

func TestOutputBufferAppendAndSwap(t *testing.T) {
	buffer := &outputBuffer{}

	// Swap on empty buffer returns nil.
	data, sequence := buffer.swap()
	if data != nil {
		t.Errorf("swap on empty buffer returned %d bytes, want nil", len(data))
	}
	if sequence != 0 {
		t.Errorf("swap on empty buffer returned sequence %d, want 0", sequence)
	}

	// Append data.
	buffer.append([]byte("hello "))
	buffer.append([]byte("world"))

	// Swap returns accumulated data and increments sequence.
	data, sequence = buffer.swap()
	if string(data) != "hello world" {
		t.Errorf("swap data = %q, want %q", data, "hello world")
	}
	if sequence != 0 {
		t.Errorf("swap sequence = %d, want 0 (first swap)", sequence)
	}

	// Buffer is empty after swap.
	data, sequence = buffer.swap()
	if data != nil {
		t.Errorf("second swap returned %d bytes, want nil", len(data))
	}

	// Next swap gets sequence 1.
	buffer.append([]byte("more"))
	data, sequence = buffer.swap()
	if string(data) != "more" {
		t.Errorf("third swap data = %q, want %q", data, "more")
	}
	if sequence != 1 {
		t.Errorf("third swap sequence = %d, want 1", sequence)
	}
}

func TestOutputBufferSizeThreshold(t *testing.T) {
	buffer := &outputBuffer{}

	// Append below threshold.
	chunk := make([]byte, 1024)
	for i := range chunk {
		chunk[i] = 'A'
	}
	thresholdReached := buffer.append(chunk)
	if thresholdReached {
		t.Error("threshold reached after 1 KB, want false")
	}

	// Fill to exactly the threshold.
	remaining := flushSizeThreshold - 1024
	bigChunk := make([]byte, remaining)
	for i := range bigChunk {
		bigChunk[i] = 'B'
	}
	thresholdReached = buffer.append(bigChunk)
	if !thresholdReached {
		t.Error("threshold not reached at 64 KB, want true")
	}
}

func TestOutputBufferSequenceMonotonic(t *testing.T) {
	buffer := &outputBuffer{}

	for i := range 10 {
		buffer.append([]byte("data"))
		_, sequence := buffer.swap()
		if sequence != uint64(i) {
			t.Errorf("swap %d: sequence = %d, want %d", i, sequence, i)
		}
	}
}

func TestWriteExitCode(t *testing.T) {
	directory := t.TempDir()
	path := directory + "/exit-code"

	if err := writeExitCode(path, 42); err != nil {
		t.Fatalf("writeExitCode: %v", err)
	}

	content, err := readFileContent(path)
	if err != nil {
		t.Fatalf("reading exit code file: %v", err)
	}
	if content != "42\n" {
		t.Errorf("exit code file content = %q, want %q", content, "42\n")
	}

	// Verify temp file was cleaned up.
	tempPath := path + ".tmp"
	if _, err := readFileContent(tempPath); err == nil {
		t.Error("temp file should not exist after successful rename")
	}
}

func TestWriteExitCodeZero(t *testing.T) {
	directory := t.TempDir()
	path := directory + "/exit-code"

	if err := writeExitCode(path, 0); err != nil {
		t.Fatalf("writeExitCode: %v", err)
	}

	content, err := readFileContent(path)
	if err != nil {
		t.Fatalf("reading exit code file: %v", err)
	}
	if content != "0\n" {
		t.Errorf("exit code file content = %q, want %q", content, "0\n")
	}
}

func TestOpenPTY(t *testing.T) {
	master, slavePath, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()

	// Slave path should be a /dev/pts/<N> path.
	if slavePath == "" {
		t.Fatal("slavePath is empty")
	}

	// Open the slave to verify the path is valid.
	slave, err := os.OpenFile(slavePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening slave %s: %v", slavePath, err)
	}
	slave.Close()

	// Write to master and read from a new slave fd (verifies the
	// master/slave pair is connected).
	slave, err = os.OpenFile(slavePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening slave for I/O test: %v", err)
	}
	defer slave.Close()

	// Write to the slave and read from the master to verify the
	// pair is connected. Writing slave→master avoids line discipline
	// transformations that would mangle the data in canonical mode.
	testData := []byte("hello")
	if _, err := slave.Write(testData); err != nil {
		t.Fatalf("writing to slave: %v", err)
	}

	readBuffer := make([]byte, 256)
	count, err := master.Read(readBuffer)
	if err != nil {
		t.Fatalf("reading from master: %v", err)
	}
	// The master sees the data after line discipline processing.
	// In default (cooked) mode, output from the slave to the master
	// has onlcr applied, but raw byte writes go through as-is.
	if string(readBuffer[:count]) != string(testData) {
		t.Errorf("master read %q, want %q", readBuffer[:count], testData)
	}
}

func readFileContent(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
