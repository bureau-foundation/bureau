// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ipc

import (
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// LauncherAction identifies the type of IPC request from the daemon to the
// launcher. Using a typed enum gives compile-time checking for typos that
// would otherwise be silent no-ops (the launcher's handler switch has a
// default error path, but the daemon constructing a misspelled action
// would not be caught until runtime).
type LauncherAction string

const (
	// ActionStatus returns the launcher's binary hash and current proxy
	// binary path. Used by the daemon to determine if a self-update is
	// needed.
	ActionStatus LauncherAction = "status"

	// ActionListSandboxes lists running sandboxes. Used by the daemon to
	// discover principals that survived a daemon restart while the launcher
	// continued running.
	ActionListSandboxes LauncherAction = "list-sandboxes"

	// ActionCreateSandbox creates a new sandbox for a principal: decrypts
	// credentials, spawns the proxy, optionally creates a bwrap namespace.
	ActionCreateSandbox LauncherAction = "create-sandbox"

	// ActionDestroySandbox tears down a running sandbox and its proxy.
	ActionDestroySandbox LauncherAction = "destroy-sandbox"

	// ActionSignalSandbox sends a signal to a sandbox's process group.
	ActionSignalSandbox LauncherAction = "signal-sandbox"

	// ActionUpdatePayload atomically rewrites a sandbox's payload file.
	ActionUpdatePayload LauncherAction = "update-payload"

	// ActionUpdateProxyBinary switches the proxy binary for future sandbox
	// creation. Existing proxies continue running their current binary.
	ActionUpdateProxyBinary LauncherAction = "update-proxy-binary"

	// ActionUpdateLogRelayBinary switches the log-relay binary for future
	// sandbox creation. Existing sandboxes continue running their current
	// log-relay binary.
	ActionUpdateLogRelayBinary LauncherAction = "update-log-relay-binary"

	// ActionProvisionCredential upserts a key-value pair into a principal's
	// encrypted credential bundle and returns the re-encrypted ciphertext.
	ActionProvisionCredential LauncherAction = "provision-credential"

	// ActionExecUpdate replaces the launcher process image with a new
	// binary. The response is sent before exec() so the daemon knows the
	// request was accepted.
	ActionExecUpdate LauncherAction = "exec-update"

	// ActionWaitSandbox blocks until a sandbox exits and returns its exit
	// code and captured output. Handled before the mutex lock (long-polling).
	ActionWaitSandbox LauncherAction = "wait-sandbox"

	// ActionWaitProxy blocks until a sandbox's proxy process exits.
	// Handled before the mutex lock (long-polling).
	ActionWaitProxy LauncherAction = "wait-proxy"
)

// IsKnown reports whether a is one of the defined LauncherAction values.
func (a LauncherAction) IsKnown() bool {
	switch a {
	case ActionStatus, ActionListSandboxes, ActionCreateSandbox,
		ActionDestroySandbox, ActionSignalSandbox, ActionUpdatePayload,
		ActionUpdateProxyBinary, ActionUpdateLogRelayBinary,
		ActionProvisionCredential, ActionExecUpdate,
		ActionWaitSandbox, ActionWaitProxy:
		return true
	}
	return false
}

// Request is a CBOR-encoded request from the daemon to the launcher,
// sent over the launcher's Unix IPC socket.
type Request struct {
	// Action is the request type.
	Action LauncherAction `cbor:"action"`

	// Principal is the localpart of the principal to operate on.
	Principal string `cbor:"principal,omitempty"`

	// EncryptedCredentials is the base64-encoded age ciphertext from
	// the m.bureau.credentials state event (for create-sandbox). The
	// launcher decrypts this using its keypair.
	//
	// Mutually exclusive with DirectCredentials — set one or neither.
	EncryptedCredentials string `cbor:"encrypted_credentials,omitempty"`

	// DirectCredentials provides plaintext credentials for the proxy
	// (for create-sandbox). Used when the daemon spawns ephemeral
	// sandboxes using its own Matrix session, bypassing age encryption.
	// Safe because the IPC socket is local-only and the daemon is a
	// trusted process on the same machine.
	//
	// Expected keys: MATRIX_TOKEN, MATRIX_USER_ID. Additional keys
	// (OPENAI_API_KEY, etc.) are forwarded to the proxy as external
	// credentials.
	//
	// Mutually exclusive with EncryptedCredentials — set one or neither.
	DirectCredentials map[string]string `cbor:"direct_credentials,omitempty"`

	// Grants are the pre-resolved authorization grants for this principal's
	// proxy. The daemon resolves these from the authorization index (when
	// DefaultPolicy or per-principal Authorization is configured) or
	// synthesizes them from the shorthand MatrixPolicy/ServiceVisibility
	// fields on PrincipalAssignment. The launcher includes them in the
	// credential payload piped to the proxy subprocess, giving the proxy
	// grant-based enforcement from the moment it starts accepting requests.
	Grants []schema.Grant `cbor:"grants,omitempty"`

	// SandboxSpec is the fully-resolved sandbox configuration produced by
	// the daemon's template resolution pipeline. When set, the launcher
	// uses this to build the bwrap command line and configure the sandbox
	// environment. When nil, the launcher spawns only the proxy process
	// without a bwrap sandbox.
	SandboxSpec *schema.SandboxSpec `cbor:"sandbox_spec,omitempty"`

	// Payload is the new payload data for update-payload requests. The
	// launcher atomically rewrites the payload file that is bind-mounted
	// into the sandbox at /run/bureau/payload.json.
	Payload map[string]any `cbor:"payload,omitempty"`

	// TriggerContent is the raw JSON content of the state event that
	// satisfied the principal's StartCondition. Written to
	// /run/bureau/trigger.json inside the sandbox as a read-only bind
	// mount. Enables event-triggered principals to read context from
	// the event that caused their launch. Carried as opaque bytes —
	// the launcher writes them verbatim to the trigger file.
	//
	// nil when the principal has no StartCondition or when the condition
	// has no associated event content.
	TriggerContent []byte `cbor:"trigger_content,omitempty"`

	// ServiceMounts lists service sockets that should be bind-mounted
	// into the sandbox. Each entry maps a service role to a host-side
	// socket path. The launcher creates a bind-mount for each at
	// /run/bureau/service/<role>.sock inside the sandbox.
	ServiceMounts []ServiceMount `cbor:"service_mounts,omitempty"`

	// TokenDirectory is the host-side directory containing pre-minted
	// service tokens for this principal. When non-empty, the launcher
	// bind-mounts it read-only at /run/bureau/service/token/ inside
	// the sandbox. Each file is named <role>.token (e.g., "ticket.token",
	// "artifact.token") and contains the raw signed token bytes. The
	// directory mount ensures atomic token refresh (write+rename on
	// host) is visible inside the sandbox via VFS path traversal.
	TokenDirectory string `cbor:"token_directory,omitempty"`

	// TelemetrySocketPath is the host-side path to the telemetry
	// relay's CBOR socket. The launcher passes this to the proxy
	// subprocess via the credential payload so the proxy can submit
	// spans and metrics directly to the relay. Empty when telemetry
	// is not deployed.
	TelemetrySocketPath string `cbor:"telemetry_socket_path,omitempty"`

	// TelemetryTokenPath is the host-side path to a service token
	// file that authenticates the proxy to the telemetry relay.
	// The proxy reads this file at startup and includes the token
	// in its CBOR submit requests. Empty when telemetry is not
	// deployed.
	TelemetryTokenPath string `cbor:"telemetry_token_path,omitempty"`

	// BinaryPath is a filesystem path used by the "update-proxy-binary"
	// action. The launcher validates the path exists and is executable,
	// then switches to it for future sandbox creation. Existing proxy
	// processes continue running their current binary.
	BinaryPath string `cbor:"binary_path,omitempty"`

	// KeyName is the credential key to upsert (for "provision-credential").
	// Combined with KeyValue, this defines the key-value pair to merge
	// into the principal's credential bundle.
	KeyName string `cbor:"key_name,omitempty"`

	// KeyValue is the plaintext credential value (for "provision-credential").
	// Safe over local IPC — same trust boundary as DirectCredentials.
	// The re-encrypted JSON bundle is zeroed after use; the Go string
	// itself is immutable and relies on GC for cleanup.
	KeyValue string `cbor:"key_value,omitempty"`

	// RecipientKeys lists age public key strings (age1... format) to
	// encrypt the updated credential bundle to (for "provision-credential").
	// The daemon resolves these from the machine's public key file and
	// any configured escrow keys before sending the IPC request.
	RecipientKeys []string `cbor:"recipient_keys,omitempty"`

	// Signal is the signal number to send to the sandbox process (for
	// "signal-sandbox"). Uses raw signal numbers (e.g., 15 for SIGTERM,
	// 9 for SIGKILL) rather than signal names for wire-format simplicity.
	// The launcher converts to syscall.Signal before delivery via
	// tmux.Server.SignalPane.
	Signal int `cbor:"signal,omitempty"`
}

// ServiceMount describes a service socket to bind-mount into a sandbox.
// The daemon resolves service roles to socket paths and passes them via
// IPC. The launcher mounts each socket at
// /run/bureau/service/<Role>.sock inside the sandbox.
type ServiceMount struct {
	// Role is the service role name (e.g., "ticket", "rag"). Determines
	// the in-sandbox socket path: /run/bureau/service/<role>.sock.
	Role string `cbor:"role"`

	// SocketPath is the host-side socket path to bind-mount. For local
	// services this is the provider's principal socket; for remote
	// services this is the daemon's tunnel socket for that service.
	SocketPath string `cbor:"socket_path"`
}

// Response is a CBOR-encoded response from the launcher to the daemon.
type Response struct {
	// OK indicates whether the request succeeded.
	OK bool `cbor:"ok"`

	// Error contains the error message if OK is false.
	Error string `cbor:"error,omitempty"`

	// ProxyPID is the PID of the spawned proxy process (for create-sandbox).
	ProxyPID int `cbor:"proxy_pid,omitempty"`

	// BinaryHash is the SHA256 hex digest of the launcher's own binary.
	// Returned by the "status" action so the daemon can compare against
	// the desired BureauVersion without restarting the launcher.
	BinaryHash string `cbor:"binary_hash,omitempty"`

	// ProxyBinaryPath is the filesystem path of the proxy binary the
	// launcher is currently using for new sandbox creation. Returned by
	// the "status" action so the daemon can hash-compare the current
	// proxy against the desired version.
	ProxyBinaryPath string `cbor:"proxy_binary_path,omitempty"`

	// LogRelayBinaryPath is the filesystem path of the log-relay binary
	// the launcher is currently using for new sandbox creation. Returned
	// by the "status" action so the daemon can hash-compare the current
	// log-relay against the desired version.
	LogRelayBinaryPath string `cbor:"log_relay_binary_path,omitempty"`

	// ExitCode is the process exit code returned by "wait-sandbox".
	// Uses a pointer to distinguish "exit code 0" (success) from
	// "field not present" (non-wait-sandbox responses).
	ExitCode *int `cbor:"exit_code,omitempty"`

	// Output is the captured terminal output from the sandbox process,
	// returned by "wait-sandbox" when the process exits with a non-zero
	// exit code. Contains the tail of the tmux pane scrollback to aid
	// debugging startup failures and crashes. Empty on success to avoid
	// leaking sandbox activity into daemon logs during normal operation.
	Output string `cbor:"output,omitempty"`

	// Sandboxes lists running sandboxes. Returned by "list-sandboxes"
	// so the daemon can discover principals that survived a daemon
	// restart while the launcher continued running.
	Sandboxes []SandboxListEntry `cbor:"sandboxes,omitempty"`

	// UpdatedCiphertext is the re-encrypted credential bundle returned
	// by "provision-credential". The daemon publishes this as the new
	// Ciphertext field on the m.bureau.credentials state event.
	UpdatedCiphertext string `cbor:"updated_ciphertext,omitempty"`

	// UpdatedKeys lists all credential key names present in the updated
	// bundle (for "provision-credential"). The daemon publishes this as
	// the Keys field on the m.bureau.credentials state event, allowing
	// credential auditing without decryption.
	UpdatedKeys []string `cbor:"updated_keys,omitempty"`
}

// SandboxListEntry describes a running sandbox returned by "list-sandboxes".
type SandboxListEntry struct {
	Localpart string `cbor:"localpart"`
	ProxyPID  int    `cbor:"proxy_pid"`
}

// ProxyCredentialPayload is the CBOR structure piped from the launcher to
// the proxy process's stdin. It carries Matrix credentials, external API
// keys, and pre-resolved authorization grants. The proxy reads this once
// at startup and uses the grants for enforcement from the first request.
//
// This is the canonical definition — both the launcher (writer) and the
// proxy (reader) import it from here rather than maintaining duplicates.
type ProxyCredentialPayload struct {
	// MatrixHomeserverURL is the Matrix homeserver base URL.
	MatrixHomeserverURL string `cbor:"matrix_homeserver_url"`

	// MatrixToken is the raw Matrix access token (without "Bearer " prefix).
	MatrixToken string `cbor:"matrix_token"`

	// MatrixUserID is the principal's full Matrix user ID.
	MatrixUserID string `cbor:"matrix_user_id"`

	// Credentials is a map of additional credential key-value pairs
	// (OPENAI_API_KEY, ANTHROPIC_API_KEY, etc.).
	Credentials map[string]string `cbor:"credentials"`

	// Grants are the pre-resolved authorization grants for this proxy.
	// The daemon resolves these before sandbox creation and passes them
	// through the launcher. Controls Matrix API gating (matrix/join,
	// matrix/invite, matrix/create-room) and service directory filtering
	// (service/discover).
	Grants []schema.Grant `cbor:"grants,omitempty"`

	// Fleet identifies the fleet this proxy belongs to. Used as the
	// envelope-level identity when submitting telemetry to the relay.
	// Zero when telemetry is not configured.
	Fleet ref.Fleet `cbor:"fleet,omitempty"`

	// Machine identifies the machine this proxy runs on. Used as the
	// envelope-level identity when submitting telemetry to the relay.
	// Zero when telemetry is not configured.
	Machine ref.Machine `cbor:"machine,omitempty"`

	// TelemetrySocketPath is the host path to the telemetry relay's
	// Unix socket. Empty when telemetry is not configured for this
	// proxy.
	TelemetrySocketPath string `cbor:"telemetry_socket_path,omitempty"`

	// TelemetryTokenPath is the host path to the service token file
	// for authenticating with the telemetry relay. Empty when
	// telemetry is not configured.
	TelemetryTokenPath string `cbor:"telemetry_token_path,omitempty"`
}
