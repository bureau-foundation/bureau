// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bureau-foundation/bureau/lib/secret"
)

// credentialStore maps credential_ref names (from ModelAccountContent)
// to API key values. Credentials are read from a directory where each
// file is named by the credential ref and contains the API key.
//
// The launcher creates this directory at sandbox creation time by
// decrypting the model service's sealed credential bundle and writing
// each entry as a separate file.
//
// Thread-safe: the credential map is immutable after construction.
type credentialStore struct {
	credentials map[string]*secret.Buffer
}

// loadCredentials reads model provider API keys from the directory
// specified by BUREAU_MODEL_CREDENTIALS_DIR. Each file in the
// directory is one credential: the filename is the credential_ref
// name, the file content is the API key.
//
// Returns an empty store (not an error) if the env var is unset. This
// allows the model service to start for local-only providers that
// don't need external API keys.
func loadCredentials() (*credentialStore, error) {
	directory := os.Getenv("BUREAU_MODEL_CREDENTIALS_DIR")
	if directory == "" {
		return &credentialStore{
			credentials: make(map[string]*secret.Buffer),
		}, nil
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("reading model credentials directory %s: %w", directory, err)
	}

	credentials := make(map[string]*secret.Buffer, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()

		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			// Clean up already-loaded credentials.
			for _, buffer := range credentials {
				buffer.Close()
			}
			return nil, fmt.Errorf("reading credential %q from %s: %w", name, directory, err)
		}

		// Credential files may have trailing newlines — strip
		// whitespace before copying into protected memory.
		trimmed := []byte(strings.TrimSpace(string(data)))
		secret.Zero(data)
		if len(trimmed) == 0 {
			continue
		}

		buffer, err := secret.NewFromBytes(trimmed)
		secret.Zero(trimmed)
		if err != nil {
			for _, existing := range credentials {
				existing.Close()
			}
			return nil, fmt.Errorf("creating credential buffer for %q: %w", name, err)
		}
		credentials[name] = buffer
	}

	return &credentialStore{credentials: credentials}, nil
}

// Get returns the API key for the given credential_ref name. Returns
// an empty string if the credential is not found.
func (store *credentialStore) Get(name string) string {
	buffer := store.credentials[name]
	if buffer == nil {
		return ""
	}
	return string(buffer.Bytes())
}

// Close releases all credential buffers.
func (store *credentialStore) Close() error {
	for name, buffer := range store.credentials {
		buffer.Close()
		delete(store.credentials, name)
	}
	return nil
}
