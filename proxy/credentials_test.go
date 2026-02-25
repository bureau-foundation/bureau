// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/ipc"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/secret"
)

func TestEnvCredentialSource_Get(t *testing.T) {
	// Set up test env vars
	os.Setenv("TEST_GITHUB_PAT", "secret-token")
	os.Setenv("BUREAU_TEST_API_KEY", "api-key-value")
	defer func() {
		os.Unsetenv("TEST_GITHUB_PAT")
		os.Unsetenv("BUREAU_TEST_API_KEY")
	}()

	tests := []struct {
		name   string
		source EnvCredentialSource
		key    string
		want   string
	}{
		{
			name:   "no prefix",
			source: EnvCredentialSource{},
			key:    "test-github-pat",
			want:   "secret-token",
		},
		{
			name:   "with prefix",
			source: EnvCredentialSource{Prefix: "BUREAU_"},
			key:    "test-api-key",
			want:   "api-key-value",
		},
		{
			name:   "not found",
			source: EnvCredentialSource{},
			key:    "nonexistent",
			want:   "",
		},
	}

	for i := range tests {
		test := &tests[i]
		t.Run(test.name, func(t *testing.T) {
			got := test.source.Get(test.key)
			assertSecretBuffer(t, "EnvCredentialSource", test.key, got, test.want)
		})
	}
}

// assertSecretBuffer checks that a *secret.Buffer matches the expected string
// value. An empty want means the buffer should be nil (not found).
func assertSecretBuffer(t *testing.T, source, key string, got *secret.Buffer, want string) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Errorf("%s.Get(%q) = %q, want nil", source, key, got.String())
		}
		return
	}
	if got == nil {
		t.Errorf("%s.Get(%q) = nil, want %q", source, key, want)
		return
	}
	if got.String() != want {
		t.Errorf("%s.Get(%q) = %q, want %q", source, key, got.String(), want)
	}
}

func TestSystemdCredentialSource_Get(t *testing.T) {
	// Create temp directory with credential files
	tempDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tempDir, "github-pat"), []byte("gh-token\n"), 0600); err != nil {
		t.Fatalf("failed to write test credential: %v", err)
	}

	tests := []struct {
		name   string
		source SystemdCredentialSource
		key    string
		want   string
	}{
		{
			name:   "found credential",
			source: SystemdCredentialSource{Directory: tempDir},
			key:    "github-pat",
			want:   "gh-token",
		},
		{
			name:   "not found",
			source: SystemdCredentialSource{Directory: tempDir},
			key:    "nonexistent",
			want:   "",
		},
		{
			name:   "no directory",
			source: SystemdCredentialSource{},
			key:    "anything",
			want:   "",
		},
	}

	for i := range tests {
		test := &tests[i]
		t.Run(test.name, func(t *testing.T) {
			got := test.source.Get(test.key)
			assertSecretBuffer(t, "SystemdCredentialSource", test.key, got, test.want)
		})
	}
}

func TestMapCredentialSource_Get(t *testing.T) {
	source := testCredentials(t, map[string]string{
		"key1": "value1",
		"key2": "value2",
	})

	tests := []struct {
		key  string
		want string
	}{
		{"key1", "value1"},
		{"key2", "value2"},
		{"key3", ""},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := source.Get(tt.key)
			assertSecretBuffer(t, "MapCredentialSource", tt.key, got, tt.want)
		})
	}
}

func TestMapCredentialSource_NilMap(t *testing.T) {
	source := &MapCredentialSource{}
	if got := source.Get("anything"); got != nil {
		t.Errorf("MapCredentialSource.Get() with nil map = %q, want nil", got.String())
	}
}

func TestReadPipeCredentials(t *testing.T) {
	payload := ipc.ProxyCredentialPayload{
		MatrixHomeserverURL: "http://localhost:6167",
		MatrixToken:         "syt_test_token_123",
		MatrixUserID:        "@iree/amdgpu/pm:bureau.local",
		Credentials: map[string]string{
			"OPENAI_API_KEY":    "sk-test-openai",
			"ANTHROPIC_API_KEY": "sk-ant-test",
		},
	}
	data, err := codec.Marshal(payload)
	if err != nil {
		t.Fatalf("codec.Marshal() error: %v", err)
	}

	source, err := ReadPipeCredentials(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadPipeCredentials() error: %v", err)
	}

	tests := []struct {
		key  string
		want string
	}{
		{"MATRIX_HOMESERVER_URL", "http://localhost:6167"},
		{"MATRIX_TOKEN", "syt_test_token_123"},
		{"MATRIX_BEARER", "Bearer syt_test_token_123"},
		{"MATRIX_USER_ID", "@iree/amdgpu/pm:bureau.local"},
		{"OPENAI_API_KEY", "sk-test-openai"},
		{"ANTHROPIC_API_KEY", "sk-ant-test"},
		{"NONEXISTENT", ""},
	}

	for _, test := range tests {
		t.Run(test.key, func(t *testing.T) {
			got := source.Get(test.key)
			assertSecretBuffer(t, "PipeCredentialSource", test.key, got, test.want)
		})
	}
}

func TestReadPipeCredentials_EmptyCredentialsMap(t *testing.T) {
	payload := ipc.ProxyCredentialPayload{
		MatrixHomeserverURL: "http://localhost:6167",
		MatrixToken:         "syt_token",
		MatrixUserID:        "@alice:bureau.local",
	}
	data, err := codec.Marshal(payload)
	if err != nil {
		t.Fatalf("codec.Marshal() error: %v", err)
	}

	source, err := ReadPipeCredentials(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadPipeCredentials() error: %v", err)
	}

	assertSecretBuffer(t, "PipeCredentialSource", "MATRIX_HOMESERVER_URL", source.Get("MATRIX_HOMESERVER_URL"), "http://localhost:6167")
	assertSecretBuffer(t, "PipeCredentialSource", "MATRIX_TOKEN", source.Get("MATRIX_TOKEN"), "syt_token")
	assertSecretBuffer(t, "PipeCredentialSource", "MATRIX_BEARER", source.Get("MATRIX_BEARER"), "Bearer syt_token")
	assertSecretBuffer(t, "PipeCredentialSource", "MATRIX_USER_ID", source.Get("MATRIX_USER_ID"), "@alice:bureau.local")
}

func TestReadPipeCredentials_ZerosBuffer(t *testing.T) {
	payload := ipc.ProxyCredentialPayload{
		MatrixHomeserverURL: "http://h:6167",
		MatrixToken:         "secret",
		MatrixUserID:        "@a:b",
	}
	data, err := codec.Marshal(payload)
	if err != nil {
		t.Fatalf("codec.Marshal() error: %v", err)
	}

	// ReadPipeCredentials reads via io.ReadAll which copies the data, so
	// we can't directly observe the zeroing of the internal buffer. But
	// we CAN verify the source works correctly after construction.
	source, err := ReadPipeCredentials(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadPipeCredentials() error: %v", err)
	}

	assertSecretBuffer(t, "PipeCredentialSource", "MATRIX_TOKEN", source.Get("MATRIX_TOKEN"), "secret")
}

func TestReadPipeCredentials_Errors(t *testing.T) {
	// Helper to CBOR-encode a payload for test cases that need valid CBOR
	// but with missing/empty fields.
	mustMarshal := func(t *testing.T, v any) []byte {
		t.Helper()
		data, err := codec.Marshal(v)
		if err != nil {
			t.Fatalf("codec.Marshal() error: %v", err)
		}
		return data
	}

	tests := []struct {
		name    string
		input   []byte
		wantErr string
	}{
		{
			name:    "empty input",
			input:   nil,
			wantErr: "credential payload is empty",
		},
		{
			name:    "invalid cbor",
			input:   []byte("not cbor"),
			wantErr: "parsing credential payload",
		},
		{
			name: "missing matrix_homeserver_url",
			input: mustMarshal(t, ipc.ProxyCredentialPayload{
				MatrixToken:  "tok",
				MatrixUserID: "@a:b",
			}),
			wantErr: "missing required field: matrix_homeserver_url",
		},
		{
			name: "missing matrix_token",
			input: mustMarshal(t, ipc.ProxyCredentialPayload{
				MatrixHomeserverURL: "http://h:6167",
				MatrixUserID:        "@a:b",
			}),
			wantErr: "missing required field: matrix_token",
		},
		{
			name: "missing matrix_user_id",
			input: mustMarshal(t, ipc.ProxyCredentialPayload{
				MatrixHomeserverURL: "http://h:6167",
				MatrixToken:         "tok",
			}),
			wantErr: "missing required field: matrix_user_id",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ReadPipeCredentials(bytes.NewReader(test.input))
			if err == nil {
				t.Fatalf("ReadPipeCredentials() = nil error, want error containing %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("ReadPipeCredentials() error = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestReadPipeCredentials_TopLevelFieldsCannotBeOverridden(t *testing.T) {
	// If the credentials map contains MATRIX_TOKEN, MATRIX_USER_ID,
	// MATRIX_BEARER, or MATRIX_HOMESERVER_URL, the top-level fields take
	// precedence. This prevents a malformed or malicious credential bundle
	// from overriding the principal's Matrix identity or homeserver.
	payload := ipc.ProxyCredentialPayload{
		MatrixHomeserverURL: "http://real:6167",
		MatrixToken:         "top-level-token",
		MatrixUserID:        "@top:level",
		Credentials: map[string]string{
			"MATRIX_HOMESERVER_URL": "http://attacker:666",
			"MATRIX_TOKEN":          "attempted-override",
			"MATRIX_BEARER":         "Bearer attempted-override",
			"MATRIX_USER_ID":        "@attacker:evil",
			"OTHER_KEY":             "other-value",
		},
	}
	data, err := codec.Marshal(payload)
	if err != nil {
		t.Fatalf("codec.Marshal() error: %v", err)
	}

	source, err := ReadPipeCredentials(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadPipeCredentials() error: %v", err)
	}

	assertSecretBuffer(t, "PipeCredentialSource", "MATRIX_HOMESERVER_URL", source.Get("MATRIX_HOMESERVER_URL"), "http://real:6167")
	assertSecretBuffer(t, "PipeCredentialSource", "MATRIX_TOKEN", source.Get("MATRIX_TOKEN"), "top-level-token")
	assertSecretBuffer(t, "PipeCredentialSource", "MATRIX_BEARER", source.Get("MATRIX_BEARER"), "Bearer top-level-token")
	assertSecretBuffer(t, "PipeCredentialSource", "MATRIX_USER_ID", source.Get("MATRIX_USER_ID"), "@top:level")
	assertSecretBuffer(t, "PipeCredentialSource", "OTHER_KEY", source.Get("OTHER_KEY"), "other-value")
}

func TestChainCredentialSource_Get(t *testing.T) {
	source := &ChainCredentialSource{
		Sources: []CredentialSource{
			testCredentials(t, map[string]string{"key1": "from-first"}),
			testCredentials(t, map[string]string{"key1": "from-second", "key2": "from-second"}),
		},
	}

	tests := []struct {
		key  string
		want string
	}{
		{"key1", "from-first"},  // First source wins
		{"key2", "from-second"}, // Falls through to second
		{"key3", ""},            // Not found in any
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := source.Get(tt.key)
			assertSecretBuffer(t, "ChainCredentialSource", tt.key, got, tt.want)
		})
	}
}

func TestPipeCredentialSource_Grants(t *testing.T) {
	t.Run("grants present", func(t *testing.T) {
		payload := ipc.ProxyCredentialPayload{
			MatrixHomeserverURL: "http://localhost:6167",
			MatrixToken:         "syt_test",
			MatrixUserID:        "@test:bureau.local",
			Grants: []schema.Grant{
				{Actions: []string{schema.ActionMatrixJoin, schema.ActionMatrixInvite}},
				{Actions: []string{schema.ActionServiceDiscover}, Targets: []string{"service/stt/**"}},
			},
		}
		data, err := codec.Marshal(payload)
		if err != nil {
			t.Fatalf("codec.Marshal() error: %v", err)
		}

		source, err := ReadPipeCredentials(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("ReadPipeCredentials() error: %v", err)
		}

		grants := source.Grants()
		if len(grants) != 2 {
			t.Fatalf("Grants() returned %d grants, want 2", len(grants))
		}
		if grants[0].Actions[0] != schema.ActionMatrixJoin || grants[0].Actions[1] != schema.ActionMatrixInvite {
			t.Errorf("grants[0].Actions = %v, want [%s, %s]", grants[0].Actions, schema.ActionMatrixJoin, schema.ActionMatrixInvite)
		}
		if grants[1].Targets[0] != "service/stt/**" {
			t.Errorf("grants[1].Targets = %v, want [service/stt/**]", grants[1].Targets)
		}
	})

	t.Run("grants absent", func(t *testing.T) {
		payload := ipc.ProxyCredentialPayload{
			MatrixHomeserverURL: "http://localhost:6167",
			MatrixToken:         "syt_test",
			MatrixUserID:        "@test:bureau.local",
		}
		data, err := codec.Marshal(payload)
		if err != nil {
			t.Fatalf("codec.Marshal() error: %v", err)
		}

		source, err := ReadPipeCredentials(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("ReadPipeCredentials() error: %v", err)
		}

		if source.Grants() != nil {
			t.Errorf("Grants() = %v, want nil for absent grants", source.Grants())
		}
	})
}
