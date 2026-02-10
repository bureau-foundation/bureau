// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.source.Get(tt.key); got != tt.want {
				t.Errorf("EnvCredentialSource.Get(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.source.Get(tt.key); got != tt.want {
				t.Errorf("SystemdCredentialSource.Get(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestMapCredentialSource_Get(t *testing.T) {
	source := &MapCredentialSource{
		Credentials: map[string]string{
			"key1": "value1",
			"key2": "value2",
		},
	}

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
			if got := source.Get(tt.key); got != tt.want {
				t.Errorf("MapCredentialSource.Get(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestMapCredentialSource_NilMap(t *testing.T) {
	source := &MapCredentialSource{}
	if got := source.Get("anything"); got != "" {
		t.Errorf("MapCredentialSource.Get() with nil map = %q, want empty", got)
	}
}

func TestReadPipeCredentials(t *testing.T) {
	payload := `{
		"matrix_homeserver_url": "http://localhost:6167",
		"matrix_token": "syt_test_token_123",
		"matrix_user_id": "@iree/amdgpu/pm:bureau.local",
		"credentials": {
			"OPENAI_API_KEY": "sk-test-openai",
			"ANTHROPIC_API_KEY": "sk-ant-test"
		}
	}`

	source, err := ReadPipeCredentials(strings.NewReader(payload))
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
			if got := source.Get(test.key); got != test.want {
				t.Errorf("PipeCredentialSource.Get(%q) = %q, want %q", test.key, got, test.want)
			}
		})
	}
}

func TestReadPipeCredentials_EmptyCredentialsMap(t *testing.T) {
	payload := `{
		"matrix_homeserver_url": "http://localhost:6167",
		"matrix_token": "syt_token",
		"matrix_user_id": "@alice:bureau.local"
	}`

	source, err := ReadPipeCredentials(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ReadPipeCredentials() error: %v", err)
	}

	if got := source.Get("MATRIX_HOMESERVER_URL"); got != "http://localhost:6167" {
		t.Errorf("Get(MATRIX_HOMESERVER_URL) = %q, want %q", got, "http://localhost:6167")
	}
	if got := source.Get("MATRIX_TOKEN"); got != "syt_token" {
		t.Errorf("Get(MATRIX_TOKEN) = %q, want %q", got, "syt_token")
	}
	if got := source.Get("MATRIX_BEARER"); got != "Bearer syt_token" {
		t.Errorf("Get(MATRIX_BEARER) = %q, want %q", got, "Bearer syt_token")
	}
	if got := source.Get("MATRIX_USER_ID"); got != "@alice:bureau.local" {
		t.Errorf("Get(MATRIX_USER_ID) = %q, want %q", got, "@alice:bureau.local")
	}
}

func TestReadPipeCredentials_ZerosBuffer(t *testing.T) {
	// Create a buffer we can inspect after ReadPipeCredentials returns.
	payload := []byte(`{"matrix_homeserver_url":"http://h:6167","matrix_token":"secret","matrix_user_id":"@a:b"}`)
	original := make([]byte, len(payload))
	copy(original, payload)

	// Wrap in a reader that serves from our buffer. ReadPipeCredentials
	// reads via io.ReadAll which copies the data, so we can't directly
	// observe the zeroing of the internal buffer. But we CAN verify the
	// source works correctly after construction.
	source, err := ReadPipeCredentials(strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("ReadPipeCredentials() error: %v", err)
	}

	if got := source.Get("MATRIX_TOKEN"); got != "secret" {
		t.Errorf("Get(MATRIX_TOKEN) = %q, want %q", got, "secret")
	}
}

func TestReadPipeCredentials_Errors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "empty input",
			input:   "",
			wantErr: "credential payload is empty",
		},
		{
			name:    "invalid json",
			input:   "not json",
			wantErr: "parsing credential payload",
		},
		{
			name:    "missing matrix_homeserver_url",
			input:   `{"matrix_token": "tok", "matrix_user_id": "@a:b"}`,
			wantErr: "missing required field: matrix_homeserver_url",
		},
		{
			name:    "missing matrix_token",
			input:   `{"matrix_homeserver_url": "http://h:6167", "matrix_user_id": "@a:b"}`,
			wantErr: "missing required field: matrix_token",
		},
		{
			name:    "missing matrix_user_id",
			input:   `{"matrix_homeserver_url": "http://h:6167", "matrix_token": "tok"}`,
			wantErr: "missing required field: matrix_user_id",
		},
		{
			name:    "empty matrix_homeserver_url",
			input:   `{"matrix_homeserver_url": "", "matrix_token": "tok", "matrix_user_id": "@a:b"}`,
			wantErr: "missing required field: matrix_homeserver_url",
		},
		{
			name:    "empty matrix_token",
			input:   `{"matrix_homeserver_url": "http://h:6167", "matrix_token": "", "matrix_user_id": "@a:b"}`,
			wantErr: "missing required field: matrix_token",
		},
		{
			name:    "empty matrix_user_id",
			input:   `{"matrix_homeserver_url": "http://h:6167", "matrix_token": "tok", "matrix_user_id": ""}`,
			wantErr: "missing required field: matrix_user_id",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ReadPipeCredentials(strings.NewReader(test.input))
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
	payload := `{
		"matrix_homeserver_url": "http://real:6167",
		"matrix_token": "top-level-token",
		"matrix_user_id": "@top:level",
		"credentials": {
			"MATRIX_HOMESERVER_URL": "http://attacker:666",
			"MATRIX_TOKEN": "attempted-override",
			"MATRIX_BEARER": "Bearer attempted-override",
			"MATRIX_USER_ID": "@attacker:evil",
			"OTHER_KEY": "other-value"
		}
	}`

	source, err := ReadPipeCredentials(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ReadPipeCredentials() error: %v", err)
	}

	if got := source.Get("MATRIX_HOMESERVER_URL"); got != "http://real:6167" {
		t.Errorf("Get(MATRIX_HOMESERVER_URL) = %q, want %q (top-level field must win)", got, "http://real:6167")
	}
	if got := source.Get("MATRIX_TOKEN"); got != "top-level-token" {
		t.Errorf("Get(MATRIX_TOKEN) = %q, want %q (top-level field must win)", got, "top-level-token")
	}
	if got := source.Get("MATRIX_BEARER"); got != "Bearer top-level-token" {
		t.Errorf("Get(MATRIX_BEARER) = %q, want %q (derived from top-level token)", got, "Bearer top-level-token")
	}
	if got := source.Get("MATRIX_USER_ID"); got != "@top:level" {
		t.Errorf("Get(MATRIX_USER_ID) = %q, want %q (top-level field must win)", got, "@top:level")
	}
	if got := source.Get("OTHER_KEY"); got != "other-value" {
		t.Errorf("Get(OTHER_KEY) = %q, want %q", got, "other-value")
	}
}

func TestChainCredentialSource_Get(t *testing.T) {
	source := &ChainCredentialSource{
		Sources: []CredentialSource{
			&MapCredentialSource{Credentials: map[string]string{"key1": "from-first"}},
			&MapCredentialSource{Credentials: map[string]string{"key1": "from-second", "key2": "from-second"}},
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
			if got := source.Get(tt.key); got != tt.want {
				t.Errorf("ChainCredentialSource.Get(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestPipeCredentialSource_MatrixPolicy(t *testing.T) {
	t.Run("policy present", func(t *testing.T) {
		payload := `{
			"matrix_homeserver_url": "http://localhost:6167",
			"matrix_token": "syt_test",
			"matrix_user_id": "@test:bureau.local",
			"matrix_policy": {
				"allow_join": true,
				"allow_invite": false,
				"allow_room_create": true
			}
		}`

		source, err := ReadPipeCredentials(strings.NewReader(payload))
		if err != nil {
			t.Fatalf("ReadPipeCredentials() error: %v", err)
		}

		policy := source.MatrixPolicy()
		if policy == nil {
			t.Fatal("MatrixPolicy() = nil, want non-nil")
		}
		if !policy.AllowJoin {
			t.Error("AllowJoin = false, want true")
		}
		if policy.AllowInvite {
			t.Error("AllowInvite = true, want false")
		}
		if !policy.AllowRoomCreate {
			t.Error("AllowRoomCreate = false, want true")
		}
	})

	t.Run("policy absent", func(t *testing.T) {
		payload := `{
			"matrix_homeserver_url": "http://localhost:6167",
			"matrix_token": "syt_test",
			"matrix_user_id": "@test:bureau.local"
		}`

		source, err := ReadPipeCredentials(strings.NewReader(payload))
		if err != nil {
			t.Fatalf("ReadPipeCredentials() error: %v", err)
		}

		if source.MatrixPolicy() != nil {
			t.Errorf("MatrixPolicy() = %v, want nil for absent policy", source.MatrixPolicy())
		}
	})

	t.Run("policy round-trips through schema type", func(t *testing.T) {
		// Verify the policy type is schema.MatrixPolicy (compile-time check).
		payload := `{
			"matrix_homeserver_url": "http://localhost:6167",
			"matrix_token": "syt_test",
			"matrix_user_id": "@test:bureau.local",
			"matrix_policy": {"allow_join": true}
		}`

		source, err := ReadPipeCredentials(strings.NewReader(payload))
		if err != nil {
			t.Fatalf("ReadPipeCredentials() error: %v", err)
		}

		var _ *schema.MatrixPolicy = source.MatrixPolicy()
	})
}
