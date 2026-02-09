// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// EnvCredentialSource reads credentials from environment variables.
// Useful for development and testing.
type EnvCredentialSource struct {
	// Prefix is prepended to credential names when looking up env vars.
	// Example: Prefix="BUREAU_" means Get("github-pat") looks up BUREAU_GITHUB_PAT.
	Prefix string
}

// Get retrieves a credential from environment variables.
func (s *EnvCredentialSource) Get(name string) string {
	// Convert credential name to env var format: github-pat -> GITHUB_PAT
	envName := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	if s.Prefix != "" {
		envName = s.Prefix + envName
	}
	return os.Getenv(envName)
}

// FileCredentialSource reads credentials from a key=value file.
// This is more secure than environment variables because file contents
// are not visible in /proc/*/environ.
//
// File format (one credential per line):
//
//	ANTHROPIC_API_KEY=sk-ant-...
//	GITHUB_PAT=ghp_...
//
// Lines starting with # are comments. Empty lines are ignored.
type FileCredentialSource struct {
	// Path is the path to the credentials file.
	Path string

	// credentials is the parsed credential map, loaded lazily.
	credentials map[string]string
	loaded      bool
}

// Get retrieves a credential from the file.
func (s *FileCredentialSource) Get(name string) string {
	if !s.loaded {
		s.load()
	}
	// Convert credential name to file key format: github-pat -> GITHUB_PAT
	key := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	return s.credentials[key]
}

// load parses the credentials file.
func (s *FileCredentialSource) load() {
	s.loaded = true
	s.credentials = make(map[string]string)

	if s.Path == "" {
		return
	}

	file, err := os.Open(s.Path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines and comments.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Parse key=value.
		if idx := strings.Index(line, "="); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			value := strings.TrimSpace(line[idx+1:])
			s.credentials[key] = value
		}
	}
}

// SystemdCredentialSource reads credentials from systemd's credential directory.
// See: https://systemd.io/CREDENTIALS/
type SystemdCredentialSource struct {
	// Directory is the path to the credentials directory.
	// Defaults to $CREDENTIALS_DIRECTORY if empty.
	Directory string
}

// Get retrieves a credential from the systemd credentials directory.
func (s *SystemdCredentialSource) Get(name string) string {
	dir := s.Directory
	if dir == "" {
		dir = os.Getenv("CREDENTIALS_DIRECTORY")
	}
	if dir == "" {
		return ""
	}

	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// MapCredentialSource provides credentials from an in-memory map.
// Useful for testing.
type MapCredentialSource struct {
	Credentials map[string]string
}

// Get retrieves a credential from the map.
func (s *MapCredentialSource) Get(name string) string {
	if s.Credentials == nil {
		return ""
	}
	return s.Credentials[name]
}

// ChainCredentialSource tries multiple credential sources in order.
// Returns the first non-empty value found.
type ChainCredentialSource struct {
	Sources []CredentialSource
}

// Get tries each source in order and returns the first non-empty value.
func (s *ChainCredentialSource) Get(name string) string {
	for _, source := range s.Sources {
		if value := source.Get(name); value != "" {
			return value
		}
	}
	return ""
}

// Verify credential sources implement CredentialSource interface.
var (
	_ CredentialSource = (*EnvCredentialSource)(nil)
	_ CredentialSource = (*FileCredentialSource)(nil)
	_ CredentialSource = (*SystemdCredentialSource)(nil)
	_ CredentialSource = (*MapCredentialSource)(nil)
	_ CredentialSource = (*ChainCredentialSource)(nil)
)
