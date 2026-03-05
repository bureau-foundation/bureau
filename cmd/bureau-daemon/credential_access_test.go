// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/messaging"
)

// --- checkAllowedPipelines tests ---

func TestCheckAllowedPipelines_NilAllowsAll(t *testing.T) {
	t.Parallel()
	if err := checkAllowedPipelines(nil, "any/pipeline:step"); err != nil {
		t.Errorf("nil AllowedPipelines should allow all, got: %v", err)
	}
}

func TestCheckAllowedPipelines_EmptyDeniesAll(t *testing.T) {
	t.Parallel()
	empty := []string{}
	if err := checkAllowedPipelines(&empty, "any/pipeline:step"); err == nil {
		t.Error("empty AllowedPipelines should deny all")
	}
}

func TestCheckAllowedPipelines_MatchAllows(t *testing.T) {
	t.Parallel()
	list := []string{"allowed/pipeline:build", "allowed/pipeline:test"}
	if err := checkAllowedPipelines(&list, "allowed/pipeline:build"); err != nil {
		t.Errorf("matching pipeline should be allowed, got: %v", err)
	}
}

func TestCheckAllowedPipelines_NoMatchDenies(t *testing.T) {
	t.Parallel()
	list := []string{"allowed/pipeline:build"}
	if err := checkAllowedPipelines(&list, "other/pipeline:deploy"); err == nil {
		t.Error("non-matching pipeline should be denied")
	}
}

func TestCheckAllowedPipelines_NameOnlyMatchesFullRef(t *testing.T) {
	t.Parallel()
	// Template allowed_pipelines lists just the pipeline name, but the
	// command-driven path provides the full qualified ref. The check
	// should extract the name portion and match.
	list := []string{"environment-compose"}
	if err := checkAllowedPipelines(&list, "bureau/pipeline:environment-compose"); err != nil {
		t.Errorf("name-only entry should match full ref, got: %v", err)
	}
}

func TestCheckAllowedPipelines_NameOnlyRejectsWrongPipeline(t *testing.T) {
	t.Parallel()
	list := []string{"environment-compose"}
	if err := checkAllowedPipelines(&list, "bureau/pipeline:workspace-setup"); err == nil {
		t.Error("name-only entry should not match a different pipeline name")
	}
}

func TestCheckAllowedPipelines_FullRefStillMatches(t *testing.T) {
	t.Parallel()
	// Full refs in the allowed list should still match full refs.
	list := []string{"bureau/pipeline:environment-compose"}
	if err := checkAllowedPipelines(&list, "bureau/pipeline:environment-compose"); err != nil {
		t.Errorf("full ref in allowed list should match full ref, got: %v", err)
	}
}

// --- substituteCredentialRefVariables tests ---

func TestSubstituteCredentialRefVariables_FleetRoom(t *testing.T) {
	t.Parallel()

	daemon, _ := credentialAccessTestSetup(t)
	// daemon.fleet is set by testMachineSetup — verify the substitution.

	result := daemon.substituteCredentialRefVariables("${FLEET_ROOM}:nix-builder")
	expected := daemon.fleet.Localpart() + ":nix-builder"
	if result != expected {
		t.Errorf("substituteCredentialRefVariables = %q, want %q", result, expected)
	}
}

func TestSubstituteCredentialRefVariables_NoVariable(t *testing.T) {
	t.Parallel()

	daemon, _ := credentialAccessTestSetup(t)

	// A literal credential ref should pass through unchanged.
	result := daemon.substituteCredentialRefVariables("bureau/fleet/prod:nix-builder")
	if result != "bureau/fleet/prod:nix-builder" {
		t.Errorf("literal ref should be unchanged, got %q", result)
	}
}

// --- hasCredentialAccess tests ---

// credentialAccessTestSetup creates a daemon with a mock Matrix server
// configured for credential access tests. The credential room has power
// levels and the daemon has a session connected to the mock server.
func credentialAccessTestSetup(t *testing.T) (*Daemon, *mockMatrixState) {
	t.Helper()

	machine, fleet := testMachineSetup(t, "testmachine", "bureau.local")
	matrixState := newMockMatrixState()
	matrixServer := httptest.NewServer(matrixState.handler())
	t.Cleanup(matrixServer.Close)

	matrixClient, err := messaging.NewClient(messaging.ClientConfig{
		HomeserverURL: matrixServer.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session, err := matrixClient.SessionFromToken(machine.UserID(), "syt_test_token")
	if err != nil {
		t.Fatalf("SessionFromToken: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	daemon, _ := newTestDaemon(t)
	daemon.machine = machine
	daemon.fleet = fleet
	daemon.session = session

	return daemon, matrixState
}

const testCredentialRoomID = "!cred-room:bureau.local"

func TestHasCredentialAccess_AdminPowerLevel(t *testing.T) {
	t.Parallel()

	daemon, matrixState := credentialAccessTestSetup(t)
	credRoomID := mustRoomID(testCredentialRoomID)
	actor := ref.MustParseUserID("@bureau/test/agent/admin:bureau.local")

	// Set actor to PL 100 in the credential room.
	matrixState.setStateEvent(testCredentialRoomID, schema.MatrixEventTypePowerLevels, "",
		schema.PowerLevels{
			Users: map[string]int{
				actor.String(): 100,
			},
		})

	credRef := schema.CredentialRef{Room: "bureau/creds", StateKey: "nix-builder"}
	action := schema.ActionCredentialUsePrefix + credRef.String()

	result := daemon.hasCredentialAccess(context.Background(), credRoomID, actor, credRef, action)
	if !result.Allowed {
		t.Fatal("admin PL should grant access")
	}
	if result.Reason != "admin power level in credential room" {
		t.Errorf("unexpected reason: %s", result.Reason)
	}
}

func TestHasCredentialAccess_BelowAdminPowerLevel(t *testing.T) {
	t.Parallel()

	daemon, matrixState := credentialAccessTestSetup(t)
	credRoomID := mustRoomID(testCredentialRoomID)
	actor := ref.MustParseUserID("@bureau/test/agent/regular:bureau.local")

	// Set actor to PL 50 (below admin).
	matrixState.setStateEvent(testCredentialRoomID, schema.MatrixEventTypePowerLevels, "",
		schema.PowerLevels{
			Users: map[string]int{
				actor.String(): 50,
			},
		})

	credRef := schema.CredentialRef{Room: "bureau/creds", StateKey: "nix-builder"}
	action := schema.ActionCredentialUsePrefix + credRef.String()

	result := daemon.hasCredentialAccess(context.Background(), credRoomID, actor, credRef, action)
	if result.Allowed {
		t.Fatal("PL 50 should not grant access")
	}
}

func TestHasCredentialAccess_GrantWithoutDenial(t *testing.T) {
	t.Parallel()

	daemon, matrixState := credentialAccessTestSetup(t)
	credRoomID := mustRoomID(testCredentialRoomID)
	actor := ref.MustParseUserID("@bureau/test/agent/builder:bureau.local")

	// No admin PL — set low.
	matrixState.setStateEvent(testCredentialRoomID, schema.MatrixEventTypePowerLevels, "",
		schema.PowerLevels{})

	// Add a credential/use grant for this actor. Uses ** to match across
	// the "/" segments in credential references (e.g., "bureau/creds:nix-builder").
	daemon.authorizationIndex.SetPrincipal(actor, schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"credential/use/**"}},
		},
	})

	credRef := schema.CredentialRef{Room: "bureau/creds", StateKey: "nix-builder"}
	action := schema.ActionCredentialUsePrefix + credRef.String()

	result := daemon.hasCredentialAccess(context.Background(), credRoomID, actor, credRef, action)
	if !result.Allowed {
		t.Fatal("credential/use grant should allow access")
	}
	if result.Reason != "credential/use grant" {
		t.Errorf("unexpected reason: %s", result.Reason)
	}
}

func TestHasCredentialAccess_GrantOverriddenByDenial(t *testing.T) {
	t.Parallel()

	daemon, matrixState := credentialAccessTestSetup(t)
	credRoomID := mustRoomID(testCredentialRoomID)
	actor := ref.MustParseUserID("@bureau/test/agent/builder:bureau.local")

	matrixState.setStateEvent(testCredentialRoomID, schema.MatrixEventTypePowerLevels, "",
		schema.PowerLevels{})

	// Grant and denial for the same action — denial should win.
	daemon.authorizationIndex.SetPrincipal(actor, schema.AuthorizationPolicy{
		Grants: []schema.Grant{
			{Actions: []string{"credential/use/**"}},
		},
		Denials: []schema.Denial{
			{Actions: []string{"credential/use/**"}},
		},
	})

	credRef := schema.CredentialRef{Room: "bureau/creds", StateKey: "nix-builder"}
	action := schema.ActionCredentialUsePrefix + credRef.String()

	result := daemon.hasCredentialAccess(context.Background(), credRoomID, actor, credRef, action)
	if result.Allowed {
		t.Fatal("denial should override grant")
	}
}

func TestHasCredentialAccess_IdentityMatch(t *testing.T) {
	t.Parallel()

	daemon, matrixState := credentialAccessTestSetup(t)
	credRoomID := mustRoomID(testCredentialRoomID)

	// Actor's localpart matches the credential state key.
	actor := ref.MustParseUserID("@nix-builder:bureau.local")

	matrixState.setStateEvent(testCredentialRoomID, schema.MatrixEventTypePowerLevels, "",
		schema.PowerLevels{})

	credRef := schema.CredentialRef{Room: "bureau/creds", StateKey: "nix-builder"}
	action := schema.ActionCredentialUsePrefix + credRef.String()

	result := daemon.hasCredentialAccess(context.Background(), credRoomID, actor, credRef, action)
	if !result.Allowed {
		t.Fatal("actor matching credential identity should have access")
	}
	if result.Reason != "actor is credential identity" {
		t.Errorf("unexpected reason: %s", result.Reason)
	}
}

func TestHasCredentialAccess_IdentityMismatch(t *testing.T) {
	t.Parallel()

	daemon, matrixState := credentialAccessTestSetup(t)
	credRoomID := mustRoomID(testCredentialRoomID)
	actor := ref.MustParseUserID("@other-principal:bureau.local")

	matrixState.setStateEvent(testCredentialRoomID, schema.MatrixEventTypePowerLevels, "",
		schema.PowerLevels{})

	credRef := schema.CredentialRef{Room: "bureau/creds", StateKey: "nix-builder"}
	action := schema.ActionCredentialUsePrefix + credRef.String()

	result := daemon.hasCredentialAccess(context.Background(), credRoomID, actor, credRef, action)
	if result.Allowed {
		t.Fatal("non-matching identity should be denied")
	}
}

// --- checkCredentialAccess tests ---

func TestCheckCredentialAccess_RequesterAllowed(t *testing.T) {
	t.Parallel()

	daemon, matrixState := credentialAccessTestSetup(t)
	credRoomID := mustRoomID(testCredentialRoomID)
	requester := ref.MustParseUserID("@bureau/test/agent/admin:bureau.local")
	templateAuthor := ref.MustParseUserID("@bureau/test/agent/author:bureau.local")

	// Requester is admin in credential room.
	matrixState.setStateEvent(testCredentialRoomID, schema.MatrixEventTypePowerLevels, "",
		schema.PowerLevels{
			Users: map[string]int{
				requester.String(): 100,
			},
		})

	credRef := schema.CredentialRef{Room: "bureau/creds", StateKey: "nix-builder"}

	result := daemon.checkCredentialAccess(
		context.Background(), credRef, credRoomID, requester, templateAuthor,
	)
	if !result.Allowed {
		t.Fatal("requester with admin PL should be allowed")
	}
	// Reason should NOT mention "template author" since requester was checked.
	if result.Reason == "" {
		t.Error("reason should not be empty")
	}
}

func TestCheckCredentialAccess_TemplateAuthorFallback(t *testing.T) {
	t.Parallel()

	daemon, matrixState := credentialAccessTestSetup(t)
	credRoomID := mustRoomID(testCredentialRoomID)
	requester := ref.MustParseUserID("@bureau/test/agent/unprivileged:bureau.local")
	templateAuthor := ref.MustParseUserID("@bureau/test/agent/author:bureau.local")

	// Only template author has admin PL.
	matrixState.setStateEvent(testCredentialRoomID, schema.MatrixEventTypePowerLevels, "",
		schema.PowerLevels{
			Users: map[string]int{
				templateAuthor.String(): 100,
			},
		})

	credRef := schema.CredentialRef{Room: "bureau/creds", StateKey: "nix-builder"}

	result := daemon.checkCredentialAccess(
		context.Background(), credRef, credRoomID, requester, templateAuthor,
	)
	if !result.Allowed {
		t.Fatal("template author fallback should grant access")
	}
	if result.Reason != "template author: admin power level in credential room" {
		t.Errorf("unexpected reason: %s", result.Reason)
	}
}

func TestCheckCredentialAccess_BothDenied(t *testing.T) {
	t.Parallel()

	daemon, matrixState := credentialAccessTestSetup(t)
	credRoomID := mustRoomID(testCredentialRoomID)
	requester := ref.MustParseUserID("@bureau/test/agent/nobody:bureau.local")
	templateAuthor := ref.MustParseUserID("@bureau/test/agent/alsonobody:bureau.local")

	// Neither has any access.
	matrixState.setStateEvent(testCredentialRoomID, schema.MatrixEventTypePowerLevels, "",
		schema.PowerLevels{})

	credRef := schema.CredentialRef{Room: "bureau/creds", StateKey: "nix-builder"}

	result := daemon.checkCredentialAccess(
		context.Background(), credRef, credRoomID, requester, templateAuthor,
	)
	if result.Allowed {
		t.Fatal("neither requester nor author should have access")
	}
}

func TestCheckCredentialAccess_ZeroTemplateAuthorSkipped(t *testing.T) {
	t.Parallel()

	daemon, matrixState := credentialAccessTestSetup(t)
	credRoomID := mustRoomID(testCredentialRoomID)
	requester := ref.MustParseUserID("@bureau/test/agent/nobody:bureau.local")
	var zeroAuthor ref.UserID // zero value

	matrixState.setStateEvent(testCredentialRoomID, schema.MatrixEventTypePowerLevels, "",
		schema.PowerLevels{})

	credRef := schema.CredentialRef{Room: "bureau/creds", StateKey: "nix-builder"}

	result := daemon.checkCredentialAccess(
		context.Background(), credRef, credRoomID, requester, zeroAuthor,
	)
	// Should deny (requester has no access, zero author is skipped).
	if result.Allowed {
		t.Fatal("zero template author should be skipped, access should be denied")
	}
}

func TestCheckCredentialAccess_SameRequesterAndAuthor(t *testing.T) {
	t.Parallel()

	daemon, matrixState := credentialAccessTestSetup(t)
	credRoomID := mustRoomID(testCredentialRoomID)
	user := ref.MustParseUserID("@bureau/test/agent/admin:bureau.local")

	// User is admin.
	matrixState.setStateEvent(testCredentialRoomID, schema.MatrixEventTypePowerLevels, "",
		schema.PowerLevels{
			Users: map[string]int{
				user.String(): 100,
			},
		})

	credRef := schema.CredentialRef{Room: "bureau/creds", StateKey: "nix-builder"}

	// Same user as both requester and author — should not check twice.
	result := daemon.checkCredentialAccess(
		context.Background(), credRef, credRoomID, user, user,
	)
	if !result.Allowed {
		t.Fatal("same user as requester and author should be allowed")
	}
	// Reason should be direct (not "template author:" prefixed) since
	// the template author path is skipped when author == requester.
	if result.Reason != "admin power level in credential room" {
		t.Errorf("unexpected reason: %s", result.Reason)
	}
}

// --- readCredentialsByRef tests ---

func TestReadCredentialsByRef_Success(t *testing.T) {
	t.Parallel()

	daemon, matrixState := credentialAccessTestSetup(t)

	credRef := schema.CredentialRef{Room: "bureau/creds", StateKey: "nix-builder"}
	credRoomAlias := "#bureau/creds:bureau.local"

	// Register the room alias and the credential state event.
	matrixState.setRoomAlias(ref.MustParseRoomAlias(credRoomAlias), testCredentialRoomID)
	matrixState.setStateEvent(testCredentialRoomID, schema.EventTypeCredentials, "nix-builder",
		schema.Credentials{
			Version:   1,
			Principal: ref.MustParseUserID("@nix-builder:bureau.local"),
			Keys:      []string{"CACHIX_AUTH_TOKEN"},
		})

	credentials, roomID, err := daemon.readCredentialsByRef(context.Background(), credRef)
	if err != nil {
		t.Fatalf("readCredentialsByRef: %v", err)
	}
	if roomID.String() != testCredentialRoomID {
		t.Errorf("room ID = %s, want %s", roomID, testCredentialRoomID)
	}
	if credentials.Version != 1 {
		t.Errorf("version = %d, want 1", credentials.Version)
	}
	if len(credentials.Keys) != 1 || credentials.Keys[0] != "CACHIX_AUTH_TOKEN" {
		t.Errorf("keys = %v, want [CACHIX_AUTH_TOKEN]", credentials.Keys)
	}
}

func TestReadCredentialsByRef_AliasNotFound(t *testing.T) {
	t.Parallel()

	daemon, _ := credentialAccessTestSetup(t)

	credRef := schema.CredentialRef{Room: "nonexistent/room", StateKey: "key"}
	_, _, err := daemon.readCredentialsByRef(context.Background(), credRef)
	if err == nil {
		t.Fatal("expected error for unresolvable alias")
	}
}

func TestReadCredentialsByRef_CredentialsNotFound(t *testing.T) {
	t.Parallel()

	daemon, matrixState := credentialAccessTestSetup(t)

	credRef := schema.CredentialRef{Room: "bureau/creds", StateKey: "missing-key"}
	credRoomAlias := "#bureau/creds:bureau.local"

	// Alias resolves, but the state event doesn't exist.
	matrixState.setRoomAlias(ref.MustParseRoomAlias(credRoomAlias), testCredentialRoomID)

	_, _, err := daemon.readCredentialsByRef(context.Background(), credRef)
	if err == nil {
		t.Fatal("expected error for missing credential state event")
	}
}

// --- isSensitiveAction test for credential/use prefix ---

func TestIsSensitiveAction_CredentialUse(t *testing.T) {
	t.Parallel()

	action := schema.ActionCredentialUsePrefix + "bureau/creds:nix-builder"
	if !isSensitiveAction(action) {
		t.Errorf("credential/use action should be sensitive")
	}
}
