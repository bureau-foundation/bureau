// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bureau-foundation/bureau/lib/schema"
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

// PipeCredentialSource reads credentials from a JSON payload piped to an
// io.Reader (typically stdin) at construction time. This is the production
// credential delivery mechanism: the launcher decrypts a credential bundle
// and pipes it to the proxy process's stdin. The raw JSON buffer is zeroed
// after parsing to minimize the time all credentials coexist in a single
// contiguous memory region.
//
// The JSON payload has the following structure:
//
//	{
//	  "matrix_homeserver_url": "http://localhost:6167",
//	  "matrix_token": "syt_...",
//	  "matrix_user_id": "@iree/amdgpu/pm:bureau.local",
//	  "credentials": {
//	    "OPENAI_API_KEY": "sk-...",
//	    "ANTHROPIC_API_KEY": "sk-ant-..."
//	  }
//	}
//
// All fields are flattened into a single credential map with uppercase keys:
// MATRIX_HOMESERVER_URL, MATRIX_TOKEN, MATRIX_BEARER (derived as "Bearer " +
// token for Authorization header injection), MATRIX_USER_ID, and whatever
// keys appear in the credentials object (stored verbatim). Lookup is
// exact-match (no normalization).
type PipeCredentialSource struct {
	credentials  map[string]string
	matrixPolicy *schema.MatrixPolicy
}

// pipeCredentialPayload is the JSON structure read from stdin.
type pipeCredentialPayload struct {
	MatrixHomeserverURL string            `json:"matrix_homeserver_url"`
	MatrixToken         string            `json:"matrix_token"`
	MatrixUserID        string            `json:"matrix_user_id"`
	Credentials         map[string]string `json:"credentials"`
	MatrixPolicy        *schema.MatrixPolicy `json:"matrix_policy,omitempty"`
}

// ReadPipeCredentials reads a JSON credential payload from reader and returns
// a PipeCredentialSource. The reader is read to completion (stdin is one-shot).
// The raw buffer is zeroed after parsing. Returns an error if the payload
// cannot be read or parsed, or if required fields are missing.
func ReadPipeCredentials(reader io.Reader) (*PipeCredentialSource, error) {
	buffer, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("reading credential payload: %w", err)
	}

	// Ensure we zero the buffer regardless of parse outcome.
	defer func() {
		for i := range buffer {
			buffer[i] = 0
		}
	}()

	if len(buffer) == 0 {
		return nil, fmt.Errorf("credential payload is empty")
	}

	var payload pipeCredentialPayload
	if err := json.Unmarshal(buffer, &payload); err != nil {
		return nil, fmt.Errorf("parsing credential payload: %w", err)
	}

	if payload.MatrixHomeserverURL == "" {
		return nil, fmt.Errorf("credential payload missing required field: matrix_homeserver_url")
	}
	if payload.MatrixToken == "" {
		return nil, fmt.Errorf("credential payload missing required field: matrix_token")
	}
	if payload.MatrixUserID == "" {
		return nil, fmt.Errorf("credential payload missing required field: matrix_user_id")
	}

	credentials := make(map[string]string, len(payload.Credentials)+4)
	for key, value := range payload.Credentials {
		credentials[key] = value
	}
	// Top-level fields are set AFTER the credentials map so they cannot
	// be overridden by a key collision in the credentials object.
	credentials["MATRIX_HOMESERVER_URL"] = payload.MatrixHomeserverURL
	credentials["MATRIX_TOKEN"] = payload.MatrixToken
	credentials["MATRIX_BEARER"] = "Bearer " + payload.MatrixToken
	credentials["MATRIX_USER_ID"] = payload.MatrixUserID

	return &PipeCredentialSource{
		credentials:  credentials,
		matrixPolicy: payload.MatrixPolicy,
	}, nil
}

// Get retrieves a credential by exact name. No normalization is applied —
// the key must match exactly as it appeared in the JSON payload (or
// "MATRIX_TOKEN" / "MATRIX_USER_ID" for the top-level fields).
func (s *PipeCredentialSource) Get(name string) string {
	return s.credentials[name]
}

// MatrixPolicy returns the Matrix access policy from the credential payload.
// Returns nil if no policy was specified (the proxy should use default-deny).
func (s *PipeCredentialSource) MatrixPolicy() *schema.MatrixPolicy {
	return s.matrixPolicy
}

// Verify credential sources implement CredentialSource interface.
var (
	_ CredentialSource = (*EnvCredentialSource)(nil)
	_ CredentialSource = (*FileCredentialSource)(nil)
	_ CredentialSource = (*SystemdCredentialSource)(nil)
	_ CredentialSource = (*MapCredentialSource)(nil)
	_ CredentialSource = (*ChainCredentialSource)(nil)
	_ CredentialSource = (*PipeCredentialSource)(nil)
)
