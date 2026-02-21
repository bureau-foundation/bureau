// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bureau-foundation/bureau/lib/bootstrap"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/sealed"
	"github.com/bureau-foundation/bureau/lib/secret"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/messaging"
)

// firstBootSetup registers the machine with Matrix and publishes its key.
func firstBootSetup(ctx context.Context, homeserverURL, registrationTokenFile string, machine ref.Machine, keypair *sealed.Keypair, stateDir string, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	registrationToken, err := secret.ReadFromPath(registrationTokenFile)
	if err != nil {
		return fmt.Errorf("reading registration token: %w", err)
	}
	defer registrationToken.Close()

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: homeserverURL,
		Logger:        logger,
	})
	if err != nil {
		return fmt.Errorf("creating matrix client: %w", err)
	}

	// Register the machine account. Use a password derived from the
	// registration token (deterministic, so re-running is idempotent).
	username := machine.Localpart()
	password, err := derivePassword(registrationToken, username)
	if err != nil {
		return fmt.Errorf("derive machine password: %w", err)
	}
	defer password.Close()
	session, err := registerOrLogin(ctx, client, username, password, registrationToken)
	if err != nil {
		return fmt.Errorf("registering machine account: %w", err)
	}
	logger.Info("machine account ready", "user_id", session.UserID())

	if err := publishKeyAndJoinFleetRooms(ctx, session, machine, keypair, logger); err != nil {
		return err
	}

	// Save the session for subsequent boots.
	if err := service.SaveSession(stateDir, homeserverURL, session); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	return nil
}

// firstBootFromBootstrapConfig performs first boot using a pre-provisioned
// bootstrap config file from "bureau machine provision". Instead of
// registering a new account (which requires the registration token), it
// logs in with the one-time password from the bootstrap config, then
// immediately rotates the password to a value derived from the machine's
// private key material. After rotation, the one-time password is useless
// even if the bootstrap config file is captured.
func firstBootFromBootstrapConfig(ctx context.Context, bootstrapFilePath string, machine ref.Machine, keypair *sealed.Keypair, stateDir string, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	config, err := bootstrap.ReadConfig(bootstrapFilePath)
	if err != nil {
		return fmt.Errorf("reading bootstrap config: %w", err)
	}

	// Validate that the bootstrap config matches the --machine-name flag.
	if config.MachineName != machine.Localpart() {
		return fmt.Errorf("bootstrap config machine_name %q does not match machine localpart %q", config.MachineName, machine.Localpart())
	}
	if config.ServerName != machine.Server().String() {
		return fmt.Errorf("bootstrap config server_name %q does not match --server-name %q", config.ServerName, machine.Server())
	}

	client, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: config.HomeserverURL,
		Logger:        logger,
	})
	if err != nil {
		return fmt.Errorf("creating matrix client: %w", err)
	}

	// Log in with the one-time password (not register — the account was
	// already created by "bureau machine provision").
	username := machine.Localpart()
	bootstrapPassword, err := secret.NewFromString(config.Password)
	if err != nil {
		return fmt.Errorf("protecting bootstrap password: %w", err)
	}
	defer bootstrapPassword.Close()

	session, err := client.Login(ctx, username, bootstrapPassword)
	if err != nil {
		return fmt.Errorf("logging in with bootstrap password: %w", err)
	}
	logger.Info("logged in with bootstrap password", "user_id", session.UserID())

	// Derive a permanent password from the machine's private key material.
	// This is deterministic: if the launcher re-runs with the same keypair,
	// it produces the same password. The private key never leaves the
	// machine, so this password can't be derived by anyone who captured
	// the bootstrap config.
	permanentPassword, err := derivePermanentPassword(username, keypair)
	if err != nil {
		return fmt.Errorf("deriving permanent password: %w", err)
	}
	defer permanentPassword.Close()

	// Rotate the password immediately. After this, the one-time password
	// from the bootstrap config is invalidated.
	if err := session.ChangePassword(ctx, bootstrapPassword, permanentPassword); err != nil {
		return fmt.Errorf("rotating password: %w", err)
	}
	logger.Info("rotated one-time password to permanent password")

	if err := publishKeyAndJoinFleetRooms(ctx, session, machine, keypair, logger); err != nil {
		return err
	}

	// Save the session.
	if err := service.SaveSession(stateDir, config.HomeserverURL, session); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	// Attempt to delete the bootstrap config file. The one-time password
	// has been rotated, so the file is no longer sensitive — but cleaning
	// it up avoids confusion.
	if err := os.Remove(bootstrapFilePath); err != nil {
		logger.Warn("could not delete bootstrap config file (password already rotated, file is no longer sensitive)",
			"path", bootstrapFilePath, "error", err)
	} else {
		logger.Info("deleted bootstrap config file", "path", bootstrapFilePath)
	}

	return nil
}

// publishKeyAndJoinFleetRooms joins the fleet machine room, publishes the
// machine's age public key, and joins the fleet service room. Called by
// both first-boot paths (registration-token and bootstrap) after the
// machine account is established.
//
// Service room join is non-fatal: it requires an admin invitation, and
// the daemon retries on every startup. Machine room join warnings are
// also non-fatal because some homeservers return an error when re-joining
// a room the account is already in.
func publishKeyAndJoinFleetRooms(ctx context.Context, session *messaging.DirectSession, machine ref.Machine, keypair *sealed.Keypair, logger *slog.Logger) error {
	fleet := machine.Fleet()
	username := machine.Localpart()

	machineAlias := fleet.MachineRoomAlias()
	machineRoomID, err := session.ResolveAlias(ctx, machineAlias)
	if err != nil {
		return fmt.Errorf("resolving machine room alias %q: %w", machineAlias, err)
	}

	if _, err := session.JoinRoom(ctx, machineRoomID); err != nil {
		logger.Warn("join machine room returned error (may already be joined)", "error", err)
	}

	machineKeyContent := schema.MachineKey{
		Algorithm: "age-x25519",
		PublicKey: keypair.PublicKey,
	}
	_, err = session.SendStateEvent(ctx, machineRoomID, schema.EventTypeMachineKey, username, machineKeyContent)
	if err != nil {
		return fmt.Errorf("publishing machine key: %w", err)
	}
	logger.Info("machine public key published",
		"room", machineRoomID,
		"public_key", keypair.PublicKey,
	)

	// Join the fleet service room so the daemon can read service directory
	// state events via /sync and GetRoomState. Non-fatal because the
	// daemon will retry on every startup.
	serviceAlias := fleet.ServiceRoomAlias()
	serviceRoomID, err := session.ResolveAlias(ctx, serviceAlias)
	if err != nil {
		logger.Warn("could not resolve service room (daemon will retry on startup)",
			"alias", serviceAlias, "error", err)
	} else {
		if _, err := session.JoinRoom(ctx, serviceRoomID); err != nil {
			logger.Warn("join service room failed (may need admin invitation)",
				"room_id", serviceRoomID, "error", err)
		} else {
			logger.Info("joined service room", "room_id", serviceRoomID)
		}
	}

	return nil
}

// derivePermanentPassword derives a password from the machine name and private
// key material. The result is deterministic: the same keypair always produces
// the same password. This is used after bootstrap to replace the one-time
// password, ensuring that only the machine itself (which holds the private key)
// can derive its own password.
func derivePermanentPassword(machineName string, keypair *sealed.Keypair) (*secret.Buffer, error) {
	hash := sha256.Sum256([]byte("bureau-machine-permanent:" + machineName + ":" + keypair.PrivateKey.String()))
	hexBytes := []byte(hex.EncodeToString(hash[:]))
	return secret.NewFromBytes(hexBytes)
}

// registerOrLogin registers a new account, or logs in if it already exists.
// Password and registrationToken are read but not closed — the caller retains ownership.
func registerOrLogin(ctx context.Context, client *messaging.Client, username string, password, registrationToken *secret.Buffer) (*messaging.DirectSession, error) {
	session, err := client.Register(ctx, messaging.RegisterRequest{
		Username:          username,
		Password:          password,
		RegistrationToken: registrationToken,
	})
	if err == nil {
		return session, nil
	}

	if messaging.IsMatrixError(err, messaging.ErrCodeUserInUse) {
		slog.Info("account already exists, logging in", "username", username)
		return client.Login(ctx, username, password)
	}

	return nil, err
}

// derivePassword deterministically derives a password from the registration
// token and machine name. The result is returned in an mmap-backed buffer;
// the caller must close it. This makes re-registration idempotent — calling
// register with the same token and machine name always produces the same
// password, so the launcher can safely re-run first-boot against an account
// that already exists.
func derivePassword(registrationToken *secret.Buffer, machineName string) (*secret.Buffer, error) {
	preimage, err := secret.Concat("bureau-machine-password:", machineName, ":", registrationToken)
	if err != nil {
		return nil, fmt.Errorf("building password preimage: %w", err)
	}
	defer preimage.Close()
	hash := sha256.Sum256(preimage.Bytes())
	hexBytes := []byte(hex.EncodeToString(hash[:]))
	return secret.NewFromBytes(hexBytes)
}
