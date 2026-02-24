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
		})
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

func readFileContent(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
