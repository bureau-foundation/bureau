// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"fmt"
	"strings"
)

// Matrix m.room.message msgtype constants for Bureau daemon notification
// messages. Notifications are m.room.message events with custom msgtypes
// posted by the daemon to config rooms. Each carries structured fields
// alongside a human-readable body for display in standard Matrix clients.
// Tests match on typed fields, never on body text.
const (
	// MsgTypeServiceDirectoryUpdated is posted after the daemon syncs
	// service room state changes and pushes the updated directory to
	// running proxies.
	MsgTypeServiceDirectoryUpdated = "m.bureau.service_directory_updated"

	// MsgTypeGrantsUpdated is posted after the daemon hot-reloads
	// authorization grants for a running principal's proxy.
	MsgTypeGrantsUpdated = "m.bureau.grants_updated"

	// MsgTypePayloadUpdated is posted after the daemon writes an
	// updated payload file for a running principal.
	MsgTypePayloadUpdated = "m.bureau.payload_updated"

	// MsgTypePrincipalAdopted is posted when the daemon discovers and
	// adopts a running sandbox from a previous daemon instance.
	MsgTypePrincipalAdopted = "m.bureau.principal_adopted"

	// MsgTypeSandboxExited is posted when a sandbox process terminates.
	// Includes exit code and captured terminal output for diagnosis.
	MsgTypeSandboxExited = "m.bureau.sandbox_exited"

	// MsgTypeCredentialsRotated is posted during credential rotation
	// lifecycle: when rotation starts, completes, or fails.
	MsgTypeCredentialsRotated = "m.bureau.credentials_rotated"

	// MsgTypeProxyCrash is posted when a proxy process exits
	// unexpectedly and the daemon attempts recovery.
	MsgTypeProxyCrash = "m.bureau.proxy_crash"

	// MsgTypeHealthCheck is posted when a health check fails and the
	// daemon takes corrective action (rollback or destroy).
	MsgTypeHealthCheck = "m.bureau.health_check"

	// MsgTypeDaemonSelfUpdate is posted during daemon binary
	// self-update lifecycle: in-progress, succeeded, or failed.
	MsgTypeDaemonSelfUpdate = "m.bureau.daemon_self_update"

	// MsgTypeNixPrefetchFailed is posted when the daemon fails to
	// prefetch a principal's Nix environment closure. The prefetch
	// retries automatically on the next reconciliation cycle.
	MsgTypeNixPrefetchFailed = "m.bureau.nix_prefetch_failed"

	// MsgTypePrincipalStartFailed is posted when the daemon cannot
	// start a principal (service resolution failure, token minting
	// failure, or other sandbox creation error).
	MsgTypePrincipalStartFailed = "m.bureau.principal_start_failed"

	// MsgTypePrincipalRestarted is posted when the daemon destroys
	// and recreates a principal because its sandbox configuration
	// changed (template update, environment change).
	MsgTypePrincipalRestarted = "m.bureau.principal_restarted"

	// MsgTypeBureauVersionUpdate is posted when the daemon reconciles
	// a BureauVersion state event — either reporting a prefetch failure
	// or summarizing what binary updates were applied.
	MsgTypeBureauVersionUpdate = "m.bureau.bureau_version_update"
)

// CredRotationStatus is the lifecycle state of a credential rotation.
// Values are self-describing strings that serialize directly to JSON.
type CredRotationStatus string

const (
	// CredRotationRestarting means the principal is being restarted
	// with updated credentials.
	CredRotationRestarting CredRotationStatus = "restarting"

	// CredRotationCompleted means the principal was successfully
	// restarted with new credentials.
	CredRotationCompleted CredRotationStatus = "completed"

	// CredRotationFailed means the restart after credential rotation
	// did not succeed.
	CredRotationFailed CredRotationStatus = "failed"

	// CredRotationRolledBack means the health check failed after
	// credential rotation and the daemon rolled back to the previous
	// working credentials.
	CredRotationRolledBack CredRotationStatus = "rolled_back"

	// CredRotationRollbackFailed means the daemon attempted to roll
	// back after credential rotation but the previous credentials
	// were unavailable (daemon restarted during rollback window) or
	// also failed health checks. The operator must re-provision.
	CredRotationRollbackFailed CredRotationStatus = "rollback_failed"
)

// IsKnown reports whether s is one of the defined CredRotationStatus values.
func (s CredRotationStatus) IsKnown() bool {
	switch s {
	case CredRotationRestarting, CredRotationCompleted, CredRotationFailed,
		CredRotationRolledBack, CredRotationRollbackFailed:
		return true
	}
	return false
}

// ProxyCrashStatus is the lifecycle state of proxy crash recovery.
// Values are self-describing strings that serialize directly to JSON.
type ProxyCrashStatus string

const (
	// ProxyCrashDetected means the proxy process exited unexpectedly
	// and the daemon is attempting recovery.
	ProxyCrashDetected ProxyCrashStatus = "detected"

	// ProxyCrashRecovered means the sandbox was successfully recreated
	// after the proxy crash.
	ProxyCrashRecovered ProxyCrashStatus = "recovered"

	// ProxyCrashFailed means recovery after the proxy crash was
	// unsuccessful.
	ProxyCrashFailed ProxyCrashStatus = "failed"

	// ProxyCrashBackingOff means the immediate reconciliation did not
	// restore the principal and a deferred retry is scheduled with
	// exponential backoff.
	ProxyCrashBackingOff ProxyCrashStatus = "backing_off"
)

// IsKnown reports whether s is one of the defined ProxyCrashStatus values.
func (s ProxyCrashStatus) IsKnown() bool {
	switch s {
	case ProxyCrashDetected, ProxyCrashRecovered, ProxyCrashFailed, ProxyCrashBackingOff:
		return true
	}
	return false
}

// HealthCheckOutcome is the result of a health check failure response.
// Values are self-describing strings that serialize directly to JSON.
type HealthCheckOutcome string

const (
	// HealthCheckDestroyed means the principal was destroyed because
	// no previous working configuration was available for rollback.
	HealthCheckDestroyed HealthCheckOutcome = "destroyed"

	// HealthCheckRolledBack means the principal was reverted to its
	// previous working configuration after the health check failure.
	HealthCheckRolledBack HealthCheckOutcome = "rolled_back"

	// HealthCheckRollbackFailed means the rollback to the previous
	// configuration was attempted but failed.
	HealthCheckRollbackFailed HealthCheckOutcome = "rollback_failed"
)

// IsKnown reports whether o is one of the defined HealthCheckOutcome values.
func (o HealthCheckOutcome) IsKnown() bool {
	switch o {
	case HealthCheckDestroyed, HealthCheckRolledBack, HealthCheckRollbackFailed:
		return true
	}
	return false
}

// SelfUpdateStatus is the lifecycle state of a daemon binary self-update.
// Values are self-describing strings that serialize directly to JSON.
type SelfUpdateStatus string

const (
	// SelfUpdateInProgress means the daemon is exec()'ing the new binary.
	SelfUpdateInProgress SelfUpdateStatus = "in_progress"

	// SelfUpdateSucceeded means the new binary started successfully.
	SelfUpdateSucceeded SelfUpdateStatus = "succeeded"

	// SelfUpdateFailed means the update failed and the daemon reverted.
	SelfUpdateFailed SelfUpdateStatus = "failed"
)

// IsKnown reports whether s is one of the defined SelfUpdateStatus values.
func (s SelfUpdateStatus) IsKnown() bool {
	switch s {
	case SelfUpdateInProgress, SelfUpdateSucceeded, SelfUpdateFailed:
		return true
	}
	return false
}

// VersionUpdateStatus is the outcome of a BureauVersion reconciliation.
// Values are self-describing strings that serialize directly to JSON.
type VersionUpdateStatus string

const (
	// VersionUpdatePrefetchFailed means store path prefetching failed
	// and will be retried on the next reconciliation cycle.
	VersionUpdatePrefetchFailed VersionUpdateStatus = "prefetch_failed"

	// VersionUpdateReconciled means the binary updates were
	// successfully applied.
	VersionUpdateReconciled VersionUpdateStatus = "reconciled"
)

// IsKnown reports whether s is one of the defined VersionUpdateStatus values.
func (s VersionUpdateStatus) IsKnown() bool {
	switch s {
	case VersionUpdatePrefetchFailed, VersionUpdateReconciled:
		return true
	}
	return false
}

// --------------------------------------------------------------------
// Daemon notification message types
// --------------------------------------------------------------------
//
// Each notification type is an m.room.message event with a custom
// msgtype. The daemon posts these to config rooms so that operators
// and integration tests can observe system state changes. Every struct
// carries a human-readable Body for Matrix client display alongside
// typed fields for programmatic consumption. Tests must match on
// typed fields, never on Body content.

// ServiceDirectoryUpdatedMessage is the content of an m.room.message
// event with msgtype MsgTypeServiceDirectoryUpdated. Posted after the
// daemon syncs service room state and pushes the updated directory to
// all running proxies.
type ServiceDirectoryUpdatedMessage struct {
	MsgType string   `json:"msgtype"`
	Body    string   `json:"body"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Updated []string `json:"updated,omitempty"`
}

// NewServiceDirectoryUpdatedMessage constructs a ServiceDirectoryUpdatedMessage
// with a human-readable body summarizing the changes.
func NewServiceDirectoryUpdatedMessage(added, removed, updated []string) ServiceDirectoryUpdatedMessage {
	var parts []string
	for _, name := range added {
		parts = append(parts, "added "+name)
	}
	for _, name := range removed {
		parts = append(parts, "removed "+name)
	}
	for _, name := range updated {
		parts = append(parts, "updated "+name)
	}
	return ServiceDirectoryUpdatedMessage{
		MsgType: MsgTypeServiceDirectoryUpdated,
		Body:    "Service directory updated: " + strings.Join(parts, ", "),
		Added:   added,
		Removed: removed,
		Updated: updated,
	}
}

// GrantsUpdatedMessage is the content of an m.room.message event with
// msgtype MsgTypeGrantsUpdated. Posted after the daemon hot-reloads
// authorization grants for a running principal's proxy.
type GrantsUpdatedMessage struct {
	MsgType    string `json:"msgtype"`
	Body       string `json:"body"`
	Principal  string `json:"principal"`
	GrantCount int    `json:"grant_count"`
}

// NewGrantsUpdatedMessage constructs a GrantsUpdatedMessage.
func NewGrantsUpdatedMessage(principal string, grantCount int) GrantsUpdatedMessage {
	return GrantsUpdatedMessage{
		MsgType:    MsgTypeGrantsUpdated,
		Body:       fmt.Sprintf("Authorization grants updated for %s (%d grants)", principal, grantCount),
		Principal:  principal,
		GrantCount: grantCount,
	}
}

// PayloadUpdatedMessage is the content of an m.room.message event with
// msgtype MsgTypePayloadUpdated. Posted after the daemon writes an
// updated payload file for a running principal.
type PayloadUpdatedMessage struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	Principal string `json:"principal"`
}

// NewPayloadUpdatedMessage constructs a PayloadUpdatedMessage.
func NewPayloadUpdatedMessage(principal string) PayloadUpdatedMessage {
	return PayloadUpdatedMessage{
		MsgType:   MsgTypePayloadUpdated,
		Body:      fmt.Sprintf("Payload updated for %s", principal),
		Principal: principal,
	}
}

// PrincipalAdoptedMessage is the content of an m.room.message event
// with msgtype MsgTypePrincipalAdopted. Posted when the daemon
// discovers and adopts a running sandbox from a previous daemon
// instance during startup recovery.
type PrincipalAdoptedMessage struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	Principal string `json:"principal"`
}

// NewPrincipalAdoptedMessage constructs a PrincipalAdoptedMessage.
func NewPrincipalAdoptedMessage(principal string) PrincipalAdoptedMessage {
	return PrincipalAdoptedMessage{
		MsgType:   MsgTypePrincipalAdopted,
		Body:      fmt.Sprintf("Adopted %s from previous daemon instance", principal),
		Principal: principal,
	}
}

// SandboxExitedMessage is the content of an m.room.message event with
// msgtype MsgTypeSandboxExited. Posted when a sandbox process terminates.
// Non-zero exit codes include the exit description and captured terminal
// output for diagnosis.
type SandboxExitedMessage struct {
	MsgType         string `json:"msgtype"`
	Body            string `json:"body"`
	Principal       string `json:"principal"`
	ExitCode        int    `json:"exit_code"`
	ExitDescription string `json:"exit_description,omitempty"`
	CapturedOutput  string `json:"captured_output,omitempty"`
}

// NewSandboxExitedMessage constructs a SandboxExitedMessage with a
// human-readable body that includes exit status and captured output.
func NewSandboxExitedMessage(principal string, exitCode int, exitDescription, capturedOutput string) SandboxExitedMessage {
	status := "exited normally"
	if exitCode != 0 {
		status = fmt.Sprintf("exited with code %d", exitCode)
		if exitDescription != "" {
			status += fmt.Sprintf(" (%s)", exitDescription)
		}
	}
	body := fmt.Sprintf("Sandbox %s %s", principal, status)
	if exitCode != 0 && capturedOutput != "" {
		body += "\n\nCaptured output:\n" + capturedOutput
	}
	return SandboxExitedMessage{
		MsgType:         MsgTypeSandboxExited,
		Body:            body,
		Principal:       principal,
		ExitCode:        exitCode,
		ExitDescription: exitDescription,
		CapturedOutput:  capturedOutput,
	}
}

// CredentialsRotatedMessage is the content of an m.room.message event
// with msgtype MsgTypeCredentialsRotated. Posted during the credential
// rotation lifecycle: when a principal is being restarted for rotation,
// when the restart completes, or when it fails.
type CredentialsRotatedMessage struct {
	MsgType   string             `json:"msgtype"`
	Body      string             `json:"body"`
	Principal string             `json:"principal"`
	Status    CredRotationStatus `json:"status"`
	Error     string             `json:"error,omitempty"`
}

// NewCredentialsRotatedMessage constructs a CredentialsRotatedMessage.
// For CredRotationFailed, pass the error message; for other statuses
// pass empty string.
func NewCredentialsRotatedMessage(principal string, status CredRotationStatus, errorMessage string) CredentialsRotatedMessage {
	var body string
	switch status {
	case CredRotationRestarting:
		body = fmt.Sprintf("Restarting %s: credentials updated", principal)
	case CredRotationCompleted:
		body = fmt.Sprintf("Restarted %s with new credentials", principal)
	case CredRotationFailed:
		body = fmt.Sprintf("FAILED to restart %s after credential rotation: %s", principal, errorMessage)
	case CredRotationRolledBack:
		body = fmt.Sprintf("Rolled back %s to previous credentials after health check failure", principal)
	case CredRotationRollbackFailed:
		body = fmt.Sprintf("CRITICAL: %s credential rollback failed: %s. Re-provisioning required.", principal, errorMessage)
	default:
		body = fmt.Sprintf("Credential rotation for %s: %s", principal, status)
	}
	return CredentialsRotatedMessage{
		MsgType:   MsgTypeCredentialsRotated,
		Body:      body,
		Principal: principal,
		Status:    status,
		Error:     errorMessage,
	}
}

// ProxyCrashMessage is the content of an m.room.message event with
// msgtype MsgTypeProxyCrash. Posted when a proxy process exits
// unexpectedly and the daemon attempts recovery. The lifecycle is:
// detected (proxy crashed), then either recovered (sandbox recreated)
// or failed (recovery unsuccessful).
type ProxyCrashMessage struct {
	MsgType   string           `json:"msgtype"`
	Body      string           `json:"body"`
	Principal string           `json:"principal"`
	ExitCode  int              `json:"exit_code,omitempty"`
	Status    ProxyCrashStatus `json:"status"`
	Error     string           `json:"error,omitempty"`
}

// NewProxyCrashMessage constructs a ProxyCrashMessage.
func NewProxyCrashMessage(principal string, status ProxyCrashStatus, exitCode int, errorMessage string) ProxyCrashMessage {
	var body string
	switch status {
	case ProxyCrashDetected:
		body = fmt.Sprintf("CRITICAL: Proxy for %s exited unexpectedly (code %d). Sandbox destroyed, re-reconciling.", principal, exitCode)
	case ProxyCrashRecovered:
		body = fmt.Sprintf("Recovered %s after proxy crash", principal)
	case ProxyCrashFailed:
		body = fmt.Sprintf("FAILED to recover %s after proxy crash: %s", principal, errorMessage)
	case ProxyCrashBackingOff:
		body = fmt.Sprintf("Proxy crash recovery for %s deferred: %s", principal, errorMessage)
	default:
		body = fmt.Sprintf("Proxy crash for %s: %s", principal, status)
	}
	return ProxyCrashMessage{
		MsgType:   MsgTypeProxyCrash,
		Body:      body,
		Principal: principal,
		ExitCode:  exitCode,
		Status:    status,
		Error:     errorMessage,
	}
}

// HealthCheckMessage is the content of an m.room.message event with
// msgtype MsgTypeHealthCheck. Posted when a health check fails and the
// daemon takes corrective action. Outcomes: "destroyed" (no rollback
// available), "rolled_back" (reverted to previous working config),
// "rollback_failed" (rollback attempted but failed).
type HealthCheckMessage struct {
	MsgType   string             `json:"msgtype"`
	Body      string             `json:"body"`
	Principal string             `json:"principal"`
	Outcome   HealthCheckOutcome `json:"outcome"`
	Error     string             `json:"error,omitempty"`
}

// NewHealthCheckMessage constructs a HealthCheckMessage.
func NewHealthCheckMessage(principal string, outcome HealthCheckOutcome, errorMessage string) HealthCheckMessage {
	var body string
	switch outcome {
	case HealthCheckDestroyed:
		body = fmt.Sprintf("CRITICAL: %s health check failed, no previous working configuration. Principal destroyed.", principal)
	case HealthCheckRolledBack:
		body = fmt.Sprintf("Rolled back %s to previous working configuration after health check failure.", principal)
	case HealthCheckRollbackFailed:
		body = fmt.Sprintf("CRITICAL: %s rollback failed: %s. Principal destroyed.", principal, errorMessage)
	default:
		body = fmt.Sprintf("Health check for %s: %s", principal, outcome)
	}
	return HealthCheckMessage{
		MsgType:   MsgTypeHealthCheck,
		Body:      body,
		Principal: principal,
		Outcome:   outcome,
		Error:     errorMessage,
	}
}

// DaemonSelfUpdateMessage is the content of an m.room.message event
// with msgtype MsgTypeDaemonSelfUpdate. Posted during daemon binary
// self-update: when exec() is initiated, when the new binary starts
// successfully, or when the update fails and reverts.
type DaemonSelfUpdateMessage struct {
	MsgType        string           `json:"msgtype"`
	Body           string           `json:"body"`
	PreviousBinary string           `json:"previous_binary"`
	NewBinary      string           `json:"new_binary"`
	Status         SelfUpdateStatus `json:"status"`
	Error          string           `json:"error,omitempty"`
}

// NewDaemonSelfUpdateMessage constructs a DaemonSelfUpdateMessage.
func NewDaemonSelfUpdateMessage(previousBinary, newBinary string, status SelfUpdateStatus, errorMessage string) DaemonSelfUpdateMessage {
	var body string
	switch status {
	case SelfUpdateInProgress:
		body = fmt.Sprintf("Daemon self-updating: exec() %s (was %s)", newBinary, previousBinary)
	case SelfUpdateSucceeded:
		body = fmt.Sprintf("Daemon self-update succeeded: now running %s (was %s)", newBinary, previousBinary)
	case SelfUpdateFailed:
		body = fmt.Sprintf("Daemon self-update failed: %s (was %s, continuing with %s)", errorMessage, previousBinary, previousBinary)
	default:
		body = fmt.Sprintf("Daemon self-update %s: %s → %s", status, previousBinary, newBinary)
	}
	return DaemonSelfUpdateMessage{
		MsgType:        MsgTypeDaemonSelfUpdate,
		Body:           body,
		PreviousBinary: previousBinary,
		NewBinary:      newBinary,
		Status:         status,
		Error:          errorMessage,
	}
}

// NixPrefetchFailedMessage is the content of an m.room.message event
// with msgtype MsgTypeNixPrefetchFailed. Posted when the daemon fails
// to prefetch a principal's Nix environment closure. The prefetch
// retries automatically on the next reconciliation cycle.
type NixPrefetchFailedMessage struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	Principal string `json:"principal"`
	StorePath string `json:"store_path"`
	Error     string `json:"error"`
}

// NewNixPrefetchFailedMessage constructs a NixPrefetchFailedMessage.
func NewNixPrefetchFailedMessage(principal, storePath, errorMessage string) NixPrefetchFailedMessage {
	return NixPrefetchFailedMessage{
		MsgType:   MsgTypeNixPrefetchFailed,
		Body:      fmt.Sprintf("Failed to prefetch Nix environment for %s: %s (will retry on next reconcile cycle)", principal, errorMessage),
		Principal: principal,
		StorePath: storePath,
		Error:     errorMessage,
	}
}

// PrincipalStartFailedMessage is the content of an m.room.message
// event with msgtype MsgTypePrincipalStartFailed. Posted when the
// daemon cannot start a principal — service resolution failure, token
// minting failure, or other sandbox creation error.
type PrincipalStartFailedMessage struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	Principal string `json:"principal"`
	Error     string `json:"error"`
}

// NewPrincipalStartFailedMessage constructs a PrincipalStartFailedMessage.
func NewPrincipalStartFailedMessage(principal, errorMessage string) PrincipalStartFailedMessage {
	return PrincipalStartFailedMessage{
		MsgType:   MsgTypePrincipalStartFailed,
		Body:      fmt.Sprintf("Cannot start %s: %s", principal, errorMessage),
		Principal: principal,
		Error:     errorMessage,
	}
}

// PrincipalRestartedMessage is the content of an m.room.message event
// with msgtype MsgTypePrincipalRestarted. Posted when the daemon
// destroys and recreates a principal because its sandbox configuration
// changed (template update, environment change, etc.).
type PrincipalRestartedMessage struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	Principal string `json:"principal"`
	Template  string `json:"template"`
}

// NewPrincipalRestartedMessage constructs a PrincipalRestartedMessage.
func NewPrincipalRestartedMessage(principal, template string) PrincipalRestartedMessage {
	return PrincipalRestartedMessage{
		MsgType:   MsgTypePrincipalRestarted,
		Body:      fmt.Sprintf("Restarting %s: sandbox configuration changed (template %s)", principal, template),
		Principal: principal,
		Template:  template,
	}
}

// BureauVersionUpdateMessage is the content of an m.room.message event
// with msgtype MsgTypeBureauVersionUpdate. Posted when the daemon
// reconciles a BureauVersion state event. Status "prefetch_failed"
// indicates store path prefetching failed (retries next cycle). Status
// "reconciled" summarizes which binary updates were applied.
type BureauVersionUpdateMessage struct {
	MsgType         string              `json:"msgtype"`
	Body            string              `json:"body"`
	Status          VersionUpdateStatus `json:"status"`
	Error           string              `json:"error,omitempty"`
	ProxyChanged    bool                `json:"proxy_changed,omitempty"`
	LauncherChanged bool                `json:"launcher_changed,omitempty"`
}

// NewBureauVersionPrefetchFailedMessage constructs a BureauVersionUpdateMessage
// for a store path prefetch failure.
func NewBureauVersionPrefetchFailedMessage(errorMessage string) BureauVersionUpdateMessage {
	return BureauVersionUpdateMessage{
		MsgType: MsgTypeBureauVersionUpdate,
		Body:    fmt.Sprintf("Failed to prefetch BureauVersion store paths: %s (will retry on next reconcile cycle)", errorMessage),
		Status:  VersionUpdatePrefetchFailed,
		Error:   errorMessage,
	}
}

// NewBureauVersionReconciledMessage constructs a BureauVersionUpdateMessage
// summarizing which binary updates were applied.
func NewBureauVersionReconciledMessage(proxyChanged, launcherChanged bool) BureauVersionUpdateMessage {
	var parts []string
	if proxyChanged {
		parts = append(parts, "proxy binary updated for future sandbox creation")
	}
	if launcherChanged {
		parts = append(parts, "launcher exec() initiated")
	}
	return BureauVersionUpdateMessage{
		MsgType:         MsgTypeBureauVersionUpdate,
		Body:            "BureauVersion: " + strings.Join(parts, "; ") + ".",
		Status:          VersionUpdateReconciled,
		ProxyChanged:    proxyChanged,
		LauncherChanged: launcherChanged,
	}
}
