// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/authorization"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
	"github.com/bureau-foundation/bureau/lib/testutil"
	"github.com/bureau-foundation/bureau/messaging"
)

// newCredentialTestDaemon sets up a Daemon with a running credential service
// socket and a mock launcher that handles provision-credential requests. Returns
// the daemon, the mock launcher's captured requests channel, and a helper to mint
// valid client tokens for calling the credential service.
func newCredentialTestDaemon(t *testing.T) (
	*Daemon,
	chan launcherIPCRequest,
	func(subject string, grants []servicetoken.Grant) []byte,
) {
	t.Helper()

	daemon, _ := newTestDaemon(t)

	// Generate Ed25519 keypair for service token signing/verification.
	publicKey, privateKey, err := servicetoken.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair (Ed25519): %v", err)
	}
	daemon.tokenSigningPublicKey = publicKey
	daemon.tokenSigningPrivateKey = privateKey
	daemon.stateDir = t.TempDir()

	// Generate a real age keypair for the launcher mock. The keypair's
	// private key is used by the mock launcher goroutine, so it must
	// outlive newCredentialTestDaemon — register cleanup on the test.
	keypair, err := sealed.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair (age): %v", err)
	}
	t.Cleanup(func() { keypair.Close() })
	daemon.machinePublicKey = keypair.PublicKey

	// Set up daemon identity fields.
	daemon.machine, daemon.fleet = testMachineSetup(t, "test", "test.local")
	daemon.configRoomID = mustRoomID("!config:test.local")

	// Set up launcher mock.
	socketDir := testutil.SocketDir(t)
	daemon.launcherSocket = filepath.Join(socketDir, "launcher.sock")

	captured := make(chan launcherIPCRequest, 10)
	listener := startMockLauncher(t, daemon.launcherSocket, func(request launcherIPCRequest) launcherIPCResponse {
		captured <- request

		if request.Action == "provision-credential" {
			// Decrypt → merge → re-encrypt cycle, mirroring the real launcher.
			var credentials map[string]string
			if request.EncryptedCredentials != "" {
				decryptedBuffer, decryptError := sealed.Decrypt(request.EncryptedCredentials, keypair.PrivateKey)
				if decryptError != nil {
					return launcherIPCResponse{OK: false, Error: decryptError.Error()}
				}
				// Copy out of the mmap-backed buffer before closing it.
				decrypted := make([]byte, len(decryptedBuffer.Bytes()))
				copy(decrypted, decryptedBuffer.Bytes())
				decryptedBuffer.Close()
				if marshalError := json.Unmarshal(decrypted, &credentials); marshalError != nil {
					return launcherIPCResponse{OK: false, Error: marshalError.Error()}
				}
			} else {
				credentials = make(map[string]string)
			}

			credentials[request.KeyName] = request.KeyValue

			updatedJSON, marshalError := json.Marshal(credentials)
			if marshalError != nil {
				return launcherIPCResponse{OK: false, Error: marshalError.Error()}
			}

			ciphertext, encryptError := sealed.Encrypt(updatedJSON, request.RecipientKeys)
			if encryptError != nil {
				return launcherIPCResponse{OK: false, Error: encryptError.Error()}
			}

			return launcherIPCResponse{
				OK:                true,
				UpdatedCiphertext: ciphertext,
				UpdatedKeys:       sortedKeys(credentials),
			}
		}

		return launcherIPCResponse{OK: false, Error: "unknown action: " + request.Action}
	})
	t.Cleanup(func() { listener.Close() })

	// Token helper: mints a signed service token with the given grants.
	// Timestamps are relative to the daemon's fake clock epoch.
	mintToken := func(subject string, grants []servicetoken.Grant) []byte {
		token := &servicetoken.Token{
			Subject:   subject,
			Machine:   daemon.machine.Localpart(),
			Audience:  credentialServiceRole,
			Grants:    grants,
			ID:        "test-token-id",
			IssuedAt:  testDaemonEpoch.Add(-5 * time.Minute).Unix(),
			ExpiresAt: testDaemonEpoch.Add(5 * time.Minute).Unix(),
		}
		tokenBytes, mintError := servicetoken.Mint(daemon.tokenSigningPrivateKey, token)
		if mintError != nil {
			t.Fatalf("mintToken: %v", mintError)
		}
		return tokenBytes
	}

	return daemon, captured, mintToken
}

func TestCredentialService_ProvisionNewKey(t *testing.T) {
	t.Parallel()

	daemon, captured, mintToken := newCredentialTestDaemon(t)

	// Mock Matrix state: no existing credentials (404), accept state event writes.
	state := newMockMatrixState()
	matrixServer := httptest.NewServer(state.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	daemon.session = session

	// Start the credential service.
	socketDir := testutil.SocketDir(t)
	daemon.runDir = socketDir
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socketPath, err := daemon.startCredentialService(ctx)
	if err != nil {
		t.Fatalf("startCredentialService: %v", err)
	}
	waitForSocket(t, socketPath)

	// Mint a token with credential/provision/key/GITHUB_TOKEN grant targeting agent/builder.
	tokenBytes := mintToken("connector/github", []servicetoken.Grant{
		{
			Actions: []string{"credential/provision/key/GITHUB_TOKEN"},
			Targets: []string{"agent/builder"},
		},
	})

	// Call the credential service.
	serviceClient := service.NewServiceClientFromToken(socketPath, tokenBytes)
	var result provisionResponse
	err = serviceClient.Call(ctx, "provision-credential", map[string]any{
		"principal": "agent/builder",
		"key":       "GITHUB_TOKEN",
		"value":     "ghp_secret123",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Principal != "agent/builder" {
		t.Errorf("Principal = %q, want %q", result.Principal, "agent/builder")
	}
	if result.Key != "GITHUB_TOKEN" {
		t.Errorf("Key = %q, want %q", result.Key, "GITHUB_TOKEN")
	}
	if len(result.Keys) != 1 || result.Keys[0] != "GITHUB_TOKEN" {
		t.Errorf("Keys = %v, want [GITHUB_TOKEN]", result.Keys)
	}

	// Verify the launcher received the correct IPC request. The mock
	// launcher writes to the channel synchronously before returning its
	// response, so by the time Call() returns the request is already buffered.
	select {
	case request := <-captured:
		if request.Action != "provision-credential" {
			t.Errorf("Action = %q, want %q", request.Action, "provision-credential")
		}
		if request.KeyName != "GITHUB_TOKEN" {
			t.Errorf("KeyName = %q, want %q", request.KeyName, "GITHUB_TOKEN")
		}
		if request.KeyValue != "ghp_secret123" {
			t.Errorf("KeyValue = %q, want %q", request.KeyValue, "ghp_secret123")
		}
		if len(request.RecipientKeys) != 1 || request.RecipientKeys[0] != daemon.machinePublicKey {
			t.Errorf("RecipientKeys = %v, want [%s]", request.RecipientKeys, daemon.machinePublicKey)
		}
		if request.EncryptedCredentials != "" {
			t.Errorf("EncryptedCredentials should be empty for fresh bundle, got %d bytes", len(request.EncryptedCredentials))
		}
	default:
		t.Fatal("no launcher IPC request captured")
	}
}

func TestCredentialService_ProvisionMergesExistingCredentials(t *testing.T) {
	t.Parallel()

	daemon, captured, mintToken := newCredentialTestDaemon(t)

	// Create an existing encrypted credential bundle with OPENAI_API_KEY.
	keypair, err := sealed.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	defer keypair.Close()

	existingCredentials := map[string]string{"OPENAI_API_KEY": "sk-existing"}
	existingJSON, _ := json.Marshal(existingCredentials)
	existingCiphertext, err := sealed.Encrypt(existingJSON, []string{daemon.machinePublicKey})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Mock Matrix state with existing credentials. The state key must be
	// fleet-scoped because handleProvisionCredential constructs
	// fleet.Localpart() + "/" + request.Principal for the lookup.
	fleetScopedPrincipal := daemon.fleet.Localpart() + "/agent/builder"
	state := newMockMatrixState()
	state.setStateEvent(daemon.configRoomID.String(), schema.EventTypeCredentials, fleetScopedPrincipal, schema.Credentials{
		Version:    1,
		Principal:  "agent/builder",
		Keys:       []string{"OPENAI_API_KEY"},
		Ciphertext: existingCiphertext,
	})
	matrixServer := httptest.NewServer(state.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	daemon.session = session

	// Start the credential service.
	socketDir := testutil.SocketDir(t)
	daemon.runDir = socketDir
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socketPath, err := daemon.startCredentialService(ctx)
	if err != nil {
		t.Fatalf("startCredentialService: %v", err)
	}
	waitForSocket(t, socketPath)

	// Provision a new GITHUB_TOKEN alongside the existing OPENAI_API_KEY.
	tokenBytes := mintToken("connector/github", []servicetoken.Grant{
		{
			Actions: []string{"credential/provision/key/GITHUB_TOKEN"},
			Targets: []string{"agent/builder"},
		},
	})

	serviceClient := service.NewServiceClientFromToken(socketPath, tokenBytes)
	var result provisionResponse
	err = serviceClient.Call(ctx, "provision-credential", map[string]any{
		"principal": "agent/builder",
		"key":       "GITHUB_TOKEN",
		"value":     "ghp_newtoken",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(result.Keys) != 2 {
		t.Fatalf("Keys = %v, want 2 keys (GITHUB_TOKEN, OPENAI_API_KEY)", result.Keys)
	}
	// Keys should be sorted.
	if result.Keys[0] != "GITHUB_TOKEN" || result.Keys[1] != "OPENAI_API_KEY" {
		t.Errorf("Keys = %v, want [GITHUB_TOKEN OPENAI_API_KEY]", result.Keys)
	}

	// Verify the launcher received the existing ciphertext.
	select {
	case request := <-captured:
		if request.EncryptedCredentials == "" {
			t.Error("EncryptedCredentials should be non-empty for existing bundle")
		}
	default:
		t.Fatal("no launcher IPC request captured")
	}
}

func TestCredentialService_DeniedWrongKeyGrant(t *testing.T) {
	t.Parallel()

	daemon, _, mintToken := newCredentialTestDaemon(t)

	// Set up no-op session (won't be called because auth check fails first).
	daemon.session = newNoopSession(t)

	socketDir := testutil.SocketDir(t)
	daemon.runDir = socketDir
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socketPath, err := daemon.startCredentialService(ctx)
	if err != nil {
		t.Fatalf("startCredentialService: %v", err)
	}
	waitForSocket(t, socketPath)

	// Token grants GITHUB_TOKEN but request asks to provision FORGEJO_TOKEN.
	tokenBytes := mintToken("connector/github", []servicetoken.Grant{
		{
			Actions: []string{"credential/provision/key/GITHUB_TOKEN"},
			Targets: []string{"agent/builder"},
		},
	})

	serviceClient := service.NewServiceClientFromToken(socketPath, tokenBytes)
	err = serviceClient.Call(ctx, "provision-credential", map[string]any{
		"principal": "agent/builder",
		"key":       "FORGEJO_TOKEN",
		"value":     "forgejo_secret",
	}, nil)

	if err == nil {
		t.Fatal("expected error for wrong key grant, got nil")
	}
	serviceError, ok := err.(*service.ServiceError)
	if !ok {
		t.Fatalf("expected *service.ServiceError, got %T: %v", err, err)
	}
	if serviceError.Action != "provision-credential" {
		t.Errorf("ServiceError.Action = %q, want %q", serviceError.Action, "provision-credential")
	}
}

func TestCredentialService_DeniedWrongTargetPrincipal(t *testing.T) {
	t.Parallel()

	daemon, _, mintToken := newCredentialTestDaemon(t)
	daemon.session = newNoopSession(t)

	socketDir := testutil.SocketDir(t)
	daemon.runDir = socketDir
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socketPath, err := daemon.startCredentialService(ctx)
	if err != nil {
		t.Fatalf("startCredentialService: %v", err)
	}
	waitForSocket(t, socketPath)

	// Token targets agent/builder but request targets agent/reviewer.
	tokenBytes := mintToken("connector/github", []servicetoken.Grant{
		{
			Actions: []string{"credential/provision/key/GITHUB_TOKEN"},
			Targets: []string{"agent/builder"},
		},
	})

	serviceClient := service.NewServiceClientFromToken(socketPath, tokenBytes)
	err = serviceClient.Call(ctx, "provision-credential", map[string]any{
		"principal": "agent/reviewer",
		"key":       "GITHUB_TOKEN",
		"value":     "ghp_secret",
	}, nil)

	if err == nil {
		t.Fatal("expected error for wrong target principal, got nil")
	}
	if _, ok := err.(*service.ServiceError); !ok {
		t.Fatalf("expected *service.ServiceError, got %T: %v", err, err)
	}
}

func TestCredentialService_MissingRequiredFields(t *testing.T) {
	t.Parallel()

	daemon, _, mintToken := newCredentialTestDaemon(t)
	daemon.session = newNoopSession(t)

	socketDir := testutil.SocketDir(t)
	daemon.runDir = socketDir
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socketPath, err := daemon.startCredentialService(ctx)
	if err != nil {
		t.Fatalf("startCredentialService: %v", err)
	}
	waitForSocket(t, socketPath)

	// Broad grant that matches everything — we're testing field validation,
	// not authorization.
	tokenBytes := mintToken("connector/test", []servicetoken.Grant{
		{Actions: []string{"**"}, Targets: []string{"**"}},
	})

	tests := []struct {
		name   string
		fields map[string]any
	}{
		{
			name: "missing principal",
			fields: map[string]any{
				"key":   "GITHUB_TOKEN",
				"value": "secret",
			},
		},
		{
			name: "missing key",
			fields: map[string]any{
				"principal": "agent/builder",
				"value":     "secret",
			},
		},
		{
			name: "missing value",
			fields: map[string]any{
				"principal": "agent/builder",
				"key":       "GITHUB_TOKEN",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serviceClient := service.NewServiceClientFromToken(socketPath, tokenBytes)
			err := serviceClient.Call(ctx, "provision-credential", test.fields, nil)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", test.name)
			}
			if _, ok := err.(*service.ServiceError); !ok {
				t.Fatalf("expected *service.ServiceError, got %T: %v", err, err)
			}
		})
	}
}

func TestCredentialService_NoMachinePublicKey(t *testing.T) {
	t.Parallel()

	daemon, _, mintToken := newCredentialTestDaemon(t)

	// Clear the machine public key to simulate a daemon that started
	// before the launcher completed first-boot.
	daemon.machinePublicKey = ""

	// Set up a session that returns 404 for credentials (fresh bundle path).
	state := newMockMatrixState()
	matrixServer := httptest.NewServer(state.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	daemon.session = session

	socketDir := testutil.SocketDir(t)
	daemon.runDir = socketDir
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socketPath, err := daemon.startCredentialService(ctx)
	if err != nil {
		t.Fatalf("startCredentialService: %v", err)
	}
	waitForSocket(t, socketPath)

	tokenBytes := mintToken("connector/test", []servicetoken.Grant{
		{Actions: []string{"**"}, Targets: []string{"**"}},
	})

	serviceClient := service.NewServiceClientFromToken(socketPath, tokenBytes)
	err = serviceClient.Call(ctx, "provision-credential", map[string]any{
		"principal": "agent/builder",
		"key":       "TEST_KEY",
		"value":     "test_value",
	}, nil)

	if err == nil {
		t.Fatal("expected error when machine public key is not loaded, got nil")
	}
}

func TestHasCredentialGrants(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)

	tests := []struct {
		name     string
		grants   []schema.Grant
		expected bool
	}{
		{
			name:     "no grants",
			grants:   nil,
			expected: false,
		},
		{
			name: "unrelated grants",
			grants: []schema.Grant{
				{Actions: []string{"ticket/create"}, Targets: []string{"**"}},
			},
			expected: false,
		},
		{
			name: "credential provision grant",
			grants: []schema.Grant{
				{Actions: []string{"credential/provision/key/GITHUB_TOKEN"}, Targets: []string{"agent/builder"}},
			},
			expected: true,
		},
		{
			name: "broad credential grant",
			grants: []schema.Grant{
				{Actions: []string{"credential/**"}, Targets: []string{"**"}},
			},
			expected: true,
		},
		{
			name: "wildcard grant matches credential",
			grants: []schema.Grant{
				{Actions: []string{"**"}, Targets: []string{"**"}},
			},
			expected: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			index := authorization.NewIndex()
			if len(test.grants) > 0 {
				index.SetPrincipal("connector/test", schema.AuthorizationPolicy{
					Grants: test.grants,
				})
			}
			daemon.authorizationIndex = index

			result := daemon.hasCredentialGrants(testEntity(t, daemon.fleet, "connector/test"))
			if result != test.expected {
				t.Errorf("hasCredentialGrants() = %v, want %v", result, test.expected)
			}
		})
	}
}

func TestAppendCredentialServiceMount(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.runDir = "/run/bureau"

	// Principal with no credential grants: mounts unchanged.
	existing := []launcherServiceMount{
		{Role: "ticket", SocketPath: "/tmp/ticket.sock"},
	}
	result := daemon.appendCredentialServiceMount(testEntity(t, daemon.fleet, "agent/plain"), existing)
	if len(result) != 1 {
		t.Fatalf("expected 1 mount for principal without credential grants, got %d", len(result))
	}

	// Principal with credential grants: mount added.
	daemon.authorizationIndex.SetPrincipal("connector/github", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"credential/provision/key/GITHUB_TOKEN"}, Targets: []string{"agent/**"}},
		},
	})
	result = daemon.appendCredentialServiceMount(testEntity(t, daemon.fleet, "connector/github"), existing)
	if len(result) != 2 {
		t.Fatalf("expected 2 mounts for principal with credential grants, got %d", len(result))
	}
	credentialMount := result[1]
	if credentialMount.Role != credentialServiceRole {
		t.Errorf("Role = %q, want %q", credentialMount.Role, credentialServiceRole)
	}
	expectedPath := "/run/bureau/credential.sock"
	if credentialMount.SocketPath != expectedPath {
		t.Errorf("SocketPath = %q, want %q", credentialMount.SocketPath, expectedPath)
	}
}

func TestCredentialServiceRoles(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)

	// No credential grants → nil.
	roles := daemon.credentialServiceRoles(testEntity(t, daemon.fleet, "agent/plain"))
	if roles != nil {
		t.Errorf("expected nil for principal without credential grants, got %v", roles)
	}

	// With credential grants → ["credential"].
	daemon.authorizationIndex.SetPrincipal("connector/github", schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"credential/provision/key/GITHUB_TOKEN"}, Targets: []string{"agent/**"}},
		},
	})
	roles = daemon.credentialServiceRoles(testEntity(t, daemon.fleet, "connector/github"))
	if len(roles) != 1 || roles[0] != credentialServiceRole {
		t.Errorf("expected [credential], got %v", roles)
	}
}

func TestCredentialRecipientKeys(t *testing.T) {
	t.Parallel()

	daemon, _ := newTestDaemon(t)
	daemon.machinePublicKey = "age1testmachinekey0000000000000000000000000000000000000000000"

	// No existing credentials → just machine key.
	keys, err := daemon.credentialRecipientKeys(nil)
	if err != nil {
		t.Fatalf("credentialRecipientKeys(nil): %v", err)
	}
	if len(keys) != 1 || keys[0] != daemon.machinePublicKey {
		t.Errorf("keys = %v, want [%s]", keys, daemon.machinePublicKey)
	}

	// Existing credentials with additional escrow key.
	existing := &schema.Credentials{
		EncryptedFor: []string{
			daemon.machinePublicKey,
			"age1escrowkey00000000000000000000000000000000000000000000000",
		},
	}
	keys, err = daemon.credentialRecipientKeys(existing)
	if err != nil {
		t.Fatalf("credentialRecipientKeys(existing): %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d: %v", len(keys), keys)
	}
	if keys[0] != daemon.machinePublicKey {
		t.Errorf("keys[0] = %q, want machine key", keys[0])
	}
	if keys[1] != "age1escrowkey00000000000000000000000000000000000000000000000" {
		t.Errorf("keys[1] = %q, want escrow key", keys[1])
	}

	// Machine key not loaded → error.
	daemon.machinePublicKey = ""
	_, err = daemon.credentialRecipientKeys(nil)
	if err == nil {
		t.Fatal("expected error when machine public key is empty")
	}
}

func TestCredentialService_WildcardGrant(t *testing.T) {
	t.Parallel()

	daemon, captured, mintToken := newCredentialTestDaemon(t)

	state := newMockMatrixState()
	matrixServer := httptest.NewServer(state.handler())
	t.Cleanup(matrixServer.Close)

	client, err := messaging.NewClient(messaging.ClientConfig{HomeserverURL: matrixServer.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := client.SessionFromToken(daemon.machine.UserID(), "test-token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	daemon.session = session

	socketDir := testutil.SocketDir(t)
	daemon.runDir = socketDir
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socketPath, err := daemon.startCredentialService(ctx)
	if err != nil {
		t.Fatalf("startCredentialService: %v", err)
	}
	waitForSocket(t, socketPath)

	// A wildcard credential/** grant should authorize any key name.
	tokenBytes := mintToken("connector/admin", []servicetoken.Grant{
		{
			Actions: []string{"credential/**"},
			Targets: []string{"**"},
		},
	})

	serviceClient := service.NewServiceClientFromToken(socketPath, tokenBytes)
	var result provisionResponse
	err = serviceClient.Call(ctx, "provision-credential", map[string]any{
		"principal": "agent/anything",
		"key":       "ARBITRARY_KEY",
		"value":     "arbitrary_value",
	}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if result.Key != "ARBITRARY_KEY" {
		t.Errorf("Key = %q, want %q", result.Key, "ARBITRARY_KEY")
	}

	select {
	case request := <-captured:
		if request.KeyName != "ARBITRARY_KEY" {
			t.Errorf("KeyName = %q, want %q", request.KeyName, "ARBITRARY_KEY")
		}
	default:
		t.Fatal("no launcher IPC request captured")
	}
}
