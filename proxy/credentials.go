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
	"sync"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
)

// EnvCredentialSource reads credentials from environment variables.
// Useful for development and testing. Results are cached in mmap-backed
// buffers on first access — the env var string briefly touches the heap
// during os.Getenv, but the cached copy is protected.
type EnvCredentialSource struct {
	// Prefix is prepended to credential names when looking up env vars.
	// Example: Prefix="BUREAU_" means Get("github-pat") looks up BUREAU_GITHUB_PAT.
	Prefix string

	mu    sync.Mutex
	cache map[string]*secret.Buffer
}

// Get retrieves a credential from environment variables.
func (s *EnvCredentialSource) Get(name string) *secret.Buffer {
	// Convert credential name to env var format: github-pat -> GITHUB_PAT
	envName := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	if s.Prefix != "" {
		envName = s.Prefix + envName
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache != nil {
		if buffer, ok := s.cache[name]; ok {
			return buffer
		}
	}

	value := os.Getenv(envName)
	if value == "" {
		return nil
	}

	buffer, err := secret.NewFromBytes([]byte(value))
	if err != nil {
		return nil
	}
	if s.cache == nil {
		s.cache = make(map[string]*secret.Buffer)
	}
	s.cache[name] = buffer
	return buffer
}

// Close releases all cached credential buffers.
func (s *EnvCredentialSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, buffer := range s.cache {
		buffer.Close()
		delete(s.cache, name)
	}
	return nil
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
	credentials map[string]*secret.Buffer
	loaded      bool
}

// Get retrieves a credential from the file.
func (s *FileCredentialSource) Get(name string) *secret.Buffer {
	if !s.loaded {
		s.load()
	}
	// Convert credential name to file key format: github-pat -> GITHUB_PAT
	key := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	return s.credentials[key]
}

// Close releases all credential buffers.
func (s *FileCredentialSource) Close() error {
	for key, buffer := range s.credentials {
		buffer.Close()
		delete(s.credentials, key)
	}
	return nil
}

// load parses the credentials file.
func (s *FileCredentialSource) load() {
	s.loaded = true
	s.credentials = make(map[string]*secret.Buffer)

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
		if index := strings.Index(line, "="); index > 0 {
			key := strings.TrimSpace(line[:index])
			value := strings.TrimSpace(line[index+1:])
			buffer, err := secret.NewFromBytes([]byte(value))
			if err != nil {
				continue
			}
			s.credentials[key] = buffer
		}
	}
}

// SystemdCredentialSource reads credentials from systemd's credential directory.
// See: https://systemd.io/CREDENTIALS/
type SystemdCredentialSource struct {
	// Directory is the path to the credentials directory.
	// Defaults to $CREDENTIALS_DIRECTORY if empty.
	Directory string

	mu    sync.Mutex
	cache map[string]*secret.Buffer
}

// Get retrieves a credential from the systemd credentials directory.
func (s *SystemdCredentialSource) Get(name string) *secret.Buffer {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache != nil {
		if buffer, ok := s.cache[name]; ok {
			return buffer
		}
	}

	directory := s.Directory
	if directory == "" {
		directory = os.Getenv("CREDENTIALS_DIRECTORY")
	}
	if directory == "" {
		return nil
	}

	path := filepath.Join(directory, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	// Credential files commonly have trailing newlines — strip whitespace
	// before moving into mmap-backed memory.
	trimmed := []byte(strings.TrimSpace(string(data)))
	// Zero the original read before discarding it.
	for index := range data {
		data[index] = 0
	}
	if len(trimmed) == 0 {
		return nil
	}

	buffer, err := secret.NewFromBytes(trimmed)
	if err != nil {
		return nil
	}

	if s.cache == nil {
		s.cache = make(map[string]*secret.Buffer)
	}
	s.cache[name] = buffer
	return buffer
}

// Close releases all cached credential buffers.
func (s *SystemdCredentialSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, buffer := range s.cache {
		buffer.Close()
		delete(s.cache, name)
	}
	return nil
}

// MapCredentialSource provides credentials from mmap-backed buffers.
// Use NewMapCredentialSource to construct from a string map.
type MapCredentialSource struct {
	credentials map[string]*secret.Buffer
}

// NewMapCredentialSource creates a MapCredentialSource from string values.
// Each value is copied into an mmap-backed buffer. Returns an error if
// any buffer allocation fails.
func NewMapCredentialSource(values map[string]string) (*MapCredentialSource, error) {
	credentials := make(map[string]*secret.Buffer, len(values))
	for key, value := range values {
		buffer, err := secret.NewFromBytes([]byte(value))
		if err != nil {
			// Clean up already-created buffers.
			for _, existing := range credentials {
				existing.Close()
			}
			return nil, fmt.Errorf("creating credential buffer for %q: %w", key, err)
		}
		credentials[key] = buffer
	}
	return &MapCredentialSource{credentials: credentials}, nil
}

// Get retrieves a credential from the map.
func (s *MapCredentialSource) Get(name string) *secret.Buffer {
	if s.credentials == nil {
		return nil
	}
	return s.credentials[name]
}

// Close releases all credential buffers.
func (s *MapCredentialSource) Close() error {
	for key, buffer := range s.credentials {
		buffer.Close()
		delete(s.credentials, key)
	}
	return nil
}

// ChainCredentialSource tries multiple credential sources in order.
// Returns the first non-nil value found.
type ChainCredentialSource struct {
	Sources []CredentialSource
}

// Get tries each source in order and returns the first non-nil value.
func (s *ChainCredentialSource) Get(name string) *secret.Buffer {
	for _, source := range s.Sources {
		if value := source.Get(name); value != nil {
			return value
		}
	}
	return nil
}

// Close closes all child credential sources.
func (s *ChainCredentialSource) Close() error {
	for _, source := range s.Sources {
		source.Close()
	}
	return nil
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
	credentials  map[string]*secret.Buffer
	matrixPolicy *schema.MatrixPolicy
}

// pipeCredentialPayload is the JSON structure read from stdin.
type pipeCredentialPayload struct {
	MatrixHomeserverURL string               `json:"matrix_homeserver_url"`
	MatrixToken         string               `json:"matrix_token"`
	MatrixUserID        string               `json:"matrix_user_id"`
	Credentials         map[string]string     `json:"credentials"`
	MatrixPolicy        *schema.MatrixPolicy `json:"matrix_policy,omitempty"`
}

// ReadPipeCredentials reads a JSON credential payload from reader and returns
// a PipeCredentialSource. The reader is read to completion (stdin is one-shot).
// The raw buffer is zeroed after parsing. Returns an error if the payload
// cannot be read or parsed, or if required fields are missing.
func ReadPipeCredentials(reader io.Reader) (*PipeCredentialSource, error) {
	rawBuffer, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("reading credential payload: %w", err)
	}

	// Ensure we zero the buffer regardless of parse outcome.
	defer func() {
		for i := range rawBuffer {
			rawBuffer[i] = 0
		}
	}()

	if len(rawBuffer) == 0 {
		return nil, fmt.Errorf("credential payload is empty")
	}

	var payload pipeCredentialPayload
	if err := json.Unmarshal(rawBuffer, &payload); err != nil {
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

	credentials := make(map[string]*secret.Buffer, len(payload.Credentials)+4)

	// Helper to wrap a string value into a secret buffer, cleaning up on failure.
	wrapValue := func(key, value string) error {
		buffer, err := secret.NewFromBytes([]byte(value))
		if err != nil {
			for _, existing := range credentials {
				existing.Close()
			}
			return fmt.Errorf("creating credential buffer for %q: %w", key, err)
		}
		credentials[key] = buffer
		return nil
	}

	for key, value := range payload.Credentials {
		if err := wrapValue(key, value); err != nil {
			return nil, err
		}
	}
	// Top-level fields are set AFTER the credentials map so they cannot
	// be overridden by a key collision in the credentials object.
	if err := wrapValue("MATRIX_HOMESERVER_URL", payload.MatrixHomeserverURL); err != nil {
		return nil, err
	}
	if err := wrapValue("MATRIX_TOKEN", payload.MatrixToken); err != nil {
		return nil, err
	}
	if err := wrapValue("MATRIX_BEARER", "Bearer "+payload.MatrixToken); err != nil {
		return nil, err
	}
	if err := wrapValue("MATRIX_USER_ID", payload.MatrixUserID); err != nil {
		return nil, err
	}

	return &PipeCredentialSource{
		credentials:  credentials,
		matrixPolicy: payload.MatrixPolicy,
	}, nil
}

// Get retrieves a credential by exact name. No normalization is applied —
// the key must match exactly as it appeared in the JSON payload (or
// "MATRIX_TOKEN" / "MATRIX_USER_ID" for the top-level fields).
func (s *PipeCredentialSource) Get(name string) *secret.Buffer {
	return s.credentials[name]
}

// Close releases all credential buffers.
func (s *PipeCredentialSource) Close() error {
	for key, buffer := range s.credentials {
		buffer.Close()
		delete(s.credentials, key)
	}
	return nil
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
