// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstBootNeeded_NoPublicKey(t *testing.T) {
	stateDir := t.TempDir()
	if !firstBootNeeded(stateDir) {
		t.Error("firstBootNeeded should return true when machine-key.pub does not exist")
	}
}

func TestFirstBootNeeded_PublicKeyExists(t *testing.T) {
	stateDir := t.TempDir()
	publicKeyPath := filepath.Join(stateDir, "machine-key.pub")
	if err := os.WriteFile(publicKeyPath, []byte("age1..."), 0600); err != nil {
		t.Fatalf("write public key: %v", err)
	}

	if firstBootNeeded(stateDir) {
		t.Error("firstBootNeeded should return false when machine-key.pub exists")
	}
}

func TestFirstBootNeeded_PublicKeyIsDirectory(t *testing.T) {
	// Edge case: something created a directory named machine-key.pub.
	// firstBootNeeded should treat this as not-a-file and return true.
	stateDir := t.TempDir()
	publicKeyPath := filepath.Join(stateDir, "machine-key.pub")
	if err := os.Mkdir(publicKeyPath, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if !firstBootNeeded(stateDir) {
		t.Error("firstBootNeeded should return true when machine-key.pub is a directory, not a file")
	}
}

func TestFileExists_RegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "testfile")
	if err := os.WriteFile(path, []byte("data"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if !fileExists(path) {
		t.Error("fileExists should return true for a regular file")
	}
}

func TestFileExists_Directory(t *testing.T) {
	if fileExists(t.TempDir()) {
		t.Error("fileExists should return false for a directory")
	}
}

func TestFileExists_Missing(t *testing.T) {
	if fileExists(filepath.Join(t.TempDir(), "nonexistent")) {
		t.Error("fileExists should return false for a missing path")
	}
}

func TestDeployCommand_RequiresBootstrapFile(t *testing.T) {
	command := deployCommand()
	err := command.Execute([]string{"local"})
	if err == nil {
		t.Fatal("expected error when --bootstrap-file is not provided")
	}
	if !strings.Contains(err.Error(), "--bootstrap-file is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDeployCommand_UnexpectedArg(t *testing.T) {
	command := deployCommand()
	err := command.Execute([]string{"local", "--bootstrap-file", "/dev/null", "extra"})
	if err == nil {
		t.Fatal("expected error for unexpected argument")
	}
	if !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDeployCommand_InvalidBootstrapFile(t *testing.T) {
	command := deployCommand()
	err := command.Execute([]string{"local", "--bootstrap-file", "/nonexistent/bootstrap.json"})
	if err == nil {
		t.Fatal("expected error for nonexistent bootstrap file")
	}
}

func TestDeployCommand_MalformedBootstrapFile(t *testing.T) {
	bootstrapPath := filepath.Join(t.TempDir(), "bootstrap.json")
	if err := os.WriteFile(bootstrapPath, []byte("not json"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	command := deployCommand()
	err := command.Execute([]string{"local", "--bootstrap-file", bootstrapPath})
	if err == nil {
		t.Fatal("expected error for malformed bootstrap file")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("expected parse error, got: %v", err)
	}
}
