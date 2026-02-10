// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// reconcile reads the current MachineConfig from Matrix and ensures the
// running sandboxes match the desired state.
func (d *Daemon) reconcile(ctx context.Context) error {
	config, err := d.readMachineConfig(ctx)
	if err != nil {
		if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
			// No config yet — nothing to do.
			d.logger.Info("no machine config found, waiting for assignment")
			return nil
		}
		return fmt.Errorf("reading machine config: %w", err)
	}

	// Cache the config for observation authorization. This is the only
	// place that updates lastConfig — it's always consistent with the
	// daemon's running state.
	d.lastConfig = config

	// Determine the desired set of principals.
	desired := make(map[string]schema.PrincipalAssignment, len(config.Principals))
	for _, assignment := range config.Principals {
		if assignment.AutoStart {
			desired[assignment.Localpart] = assignment
		}
	}

	// Create sandboxes for principals that should be running but aren't.
	for localpart, assignment := range desired {
		if d.running[localpart] {
			continue
		}

		d.logger.Info("starting principal", "principal", localpart)

		// Read the credentials for this principal.
		credentials, err := d.readCredentials(ctx, localpart)
		if err != nil {
			if messaging.IsMatrixError(err, messaging.ErrCodeNotFound) {
				d.logger.Warn("no credentials found for principal, skipping", "principal", localpart)
				continue
			}
			d.logger.Error("reading credentials", "principal", localpart, "error", err)
			continue
		}

		// Send create-sandbox to the launcher.
		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:               "create-sandbox",
			Principal:            localpart,
			EncryptedCredentials: credentials.Ciphertext,
			MatrixPolicy:         assignment.MatrixPolicy,
		})
		if err != nil {
			d.logger.Error("create-sandbox IPC failed", "principal", localpart, "error", err)
			continue
		}
		if !response.OK {
			d.logger.Error("create-sandbox rejected", "principal", localpart, "error", response.Error)
			continue
		}

		d.running[localpart] = true
		d.lastActivityAt = time.Now()
		d.logger.Info("principal started", "principal", localpart)

		// Start watching the tmux session for layout changes. This also
		// restores any previously saved layout from Matrix.
		d.startLayoutWatcher(ctx, localpart)

		// Register all known local service routes on the new consumer's
		// proxy so it can reach services that were discovered before it
		// started. The proxy socket is created synchronously by Start(),
		// so it should be accepting connections by the time the launcher
		// responds to create-sandbox.
		d.configureConsumerProxy(ctx, localpart)

		// Push the service directory so the new consumer's agent can
		// discover services via GET /v1/services.
		directory := d.buildServiceDirectory()
		if err := d.pushDirectoryToProxy(ctx, localpart, directory); err != nil {
			d.logger.Error("failed to push service directory to new consumer proxy",
				"consumer", localpart,
				"error", err,
			)
		}
	}

	// Destroy sandboxes for principals that should not be running.
	for localpart := range d.running {
		if _, shouldRun := desired[localpart]; shouldRun {
			continue
		}

		d.logger.Info("stopping principal", "principal", localpart)

		// Stop the layout watcher before destroying the sandbox. This
		// ensures a clean shutdown rather than having the watcher see
		// the tmux session disappear underneath it.
		d.stopLayoutWatcher(localpart)

		response, err := d.launcherRequest(ctx, launcherIPCRequest{
			Action:    "destroy-sandbox",
			Principal: localpart,
		})
		if err != nil {
			d.logger.Error("destroy-sandbox IPC failed", "principal", localpart, "error", err)
			continue
		}
		if !response.OK {
			d.logger.Error("destroy-sandbox rejected", "principal", localpart, "error", response.Error)
			continue
		}

		delete(d.running, localpart)
		d.lastActivityAt = time.Now()
		d.logger.Info("principal stopped", "principal", localpart)
	}

	return nil
}

// readMachineConfig reads the MachineConfig state event from the config room.
func (d *Daemon) readMachineConfig(ctx context.Context) (*schema.MachineConfig, error) {
	content, err := d.session.GetStateEvent(ctx, d.configRoomID, schema.EventTypeMachineConfig, d.machineName)
	if err != nil {
		return nil, err
	}

	var config schema.MachineConfig
	if err := json.Unmarshal(content, &config); err != nil {
		return nil, fmt.Errorf("parsing machine config: %w", err)
	}
	return &config, nil
}

// readCredentials reads the Credentials state event for a specific principal.
func (d *Daemon) readCredentials(ctx context.Context, principalLocalpart string) (*schema.Credentials, error) {
	content, err := d.session.GetStateEvent(ctx, d.configRoomID, schema.EventTypeCredentials, principalLocalpart)
	if err != nil {
		return nil, err
	}

	var credentials schema.Credentials
	if err := json.Unmarshal(content, &credentials); err != nil {
		return nil, fmt.Errorf("parsing credentials for %q: %w", principalLocalpart, err)
	}
	return &credentials, nil
}

// launcherIPCRequest mirrors the launcher's IPCRequest type. Defined here to
// avoid importing cmd/bureau-launcher (which is a main package and cannot be
// imported). The JSON wire format is the contract between daemon and launcher.
type launcherIPCRequest struct {
	Action               string               `json:"action"`
	Principal            string               `json:"principal,omitempty"`
	EncryptedCredentials string               `json:"encrypted_credentials,omitempty"`
	MatrixPolicy         *schema.MatrixPolicy `json:"matrix_policy,omitempty"`
}

// launcherIPCResponse mirrors the launcher's IPCResponse type.
type launcherIPCResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	ProxyPID int    `json:"proxy_pid,omitempty"`
}

// launcherRequest sends a request to the launcher and reads the response.
func (d *Daemon) launcherRequest(ctx context.Context, request launcherIPCRequest) (*launcherIPCResponse, error) {
	// Connect to the launcher's unix socket.
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", d.launcherSocket)
	if err != nil {
		return nil, fmt.Errorf("connecting to launcher at %s: %w", d.launcherSocket, err)
	}
	defer conn.Close()

	// Use the context's deadline if set, otherwise fall back to 30 seconds
	// (matching the launcher's handler timeout).
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	conn.SetDeadline(deadline)

	// Send the request.
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return nil, fmt.Errorf("sending request to launcher: %w", err)
	}

	// Read the response.
	var response launcherIPCResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return nil, fmt.Errorf("reading response from launcher: %w", err)
	}

	return &response, nil
}
