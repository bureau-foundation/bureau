// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestAuthorizeObserveDefaultDeny(t *testing.T) {
	t.Parallel()

	daemon := &Daemon{}
	// No lastConfig — everything should be denied.
	authz := daemon.authorizeObserve("@ben:bureau.local", "iree/amdgpu/pm", "readwrite")
	if authz.Allowed {
		t.Error("expected default deny when lastConfig is nil")
	}
}

func TestAuthorizeObserveNoPolicyDeny(t *testing.T) {
	t.Parallel()

	daemon := &Daemon{
		lastConfig: &schema.MachineConfig{
			Principals: []schema.PrincipalAssignment{
				{Localpart: "iree/amdgpu/pm", AutoStart: true},
			},
			// No DefaultObservePolicy and no per-principal ObservePolicy.
		},
	}
	authz := daemon.authorizeObserve("@ben:bureau.local", "iree/amdgpu/pm", "readwrite")
	if authz.Allowed {
		t.Error("expected deny when no ObservePolicy exists at any level")
	}
}

func TestAuthorizeObserveDefaultPolicyAllows(t *testing.T) {
	t.Parallel()

	daemon := &Daemon{
		lastConfig: &schema.MachineConfig{
			DefaultObservePolicy: &schema.ObservePolicy{
				AllowedObservers: []string{"**"},
			},
			Principals: []schema.PrincipalAssignment{
				{Localpart: "iree/amdgpu/pm", AutoStart: true},
			},
		},
	}
	authz := daemon.authorizeObserve("@ben:bureau.local", "iree/amdgpu/pm", "readonly")
	if !authz.Allowed {
		t.Error("expected allow with ** default policy")
	}
	if authz.GrantedMode != "readonly" {
		t.Errorf("GrantedMode = %q, want readonly", authz.GrantedMode)
	}
}

func TestAuthorizeObservePrincipalPolicyOverridesDefault(t *testing.T) {
	t.Parallel()

	daemon := &Daemon{
		lastConfig: &schema.MachineConfig{
			DefaultObservePolicy: &schema.ObservePolicy{
				AllowedObservers: []string{"**"},
			},
			Principals: []schema.PrincipalAssignment{
				{
					Localpart: "iree/amdgpu/pm",
					AutoStart: true,
					ObservePolicy: &schema.ObservePolicy{
						// Principal-level policy only allows specific observer.
						AllowedObservers: []string{"ops/alice"},
					},
				},
			},
		},
	}

	// ben is not in the principal's policy, even though default allows **.
	authz := daemon.authorizeObserve("@ben:bureau.local", "iree/amdgpu/pm", "readonly")
	if authz.Allowed {
		t.Error("expected deny: principal policy overrides default, ben not in AllowedObservers")
	}

	// alice matches the principal's policy.
	authz = daemon.authorizeObserve("@ops/alice:bureau.local", "iree/amdgpu/pm", "readonly")
	if !authz.Allowed {
		t.Error("expected allow: alice matches principal policy")
	}
}

func TestAuthorizeObserveModeDowngrade(t *testing.T) {
	t.Parallel()

	daemon := &Daemon{
		lastConfig: &schema.MachineConfig{
			DefaultObservePolicy: &schema.ObservePolicy{
				AllowedObservers:   []string{"**"},
				ReadWriteObservers: []string{"ops/**"},
			},
			Principals: []schema.PrincipalAssignment{
				{Localpart: "iree/amdgpu/pm", AutoStart: true},
			},
		},
	}

	// ben requests readwrite but is not in ReadWriteObservers — should
	// be downgraded to readonly.
	authz := daemon.authorizeObserve("@ben:bureau.local", "iree/amdgpu/pm", "readwrite")
	if !authz.Allowed {
		t.Fatal("expected allow")
	}
	if authz.GrantedMode != "readonly" {
		t.Errorf("GrantedMode = %q, want readonly (downgraded)", authz.GrantedMode)
	}

	// ops/alice requests readwrite and is in ReadWriteObservers — should get it.
	authz = daemon.authorizeObserve("@ops/alice:bureau.local", "iree/amdgpu/pm", "readwrite")
	if !authz.Allowed {
		t.Fatal("expected allow")
	}
	if authz.GrantedMode != "readwrite" {
		t.Errorf("GrantedMode = %q, want readwrite", authz.GrantedMode)
	}
}

func TestAuthorizeObserveReadonlyNotUpgraded(t *testing.T) {
	t.Parallel()

	daemon := &Daemon{
		lastConfig: &schema.MachineConfig{
			DefaultObservePolicy: &schema.ObservePolicy{
				AllowedObservers:   []string{"**"},
				ReadWriteObservers: []string{"**"},
			},
			Principals: []schema.PrincipalAssignment{
				{Localpart: "iree/amdgpu/pm", AutoStart: true},
			},
		},
	}

	// Even though observer has readwrite permission, requesting readonly
	// should stay readonly.
	authz := daemon.authorizeObserve("@ben:bureau.local", "iree/amdgpu/pm", "readonly")
	if !authz.Allowed {
		t.Fatal("expected allow")
	}
	if authz.GrantedMode != "readonly" {
		t.Errorf("GrantedMode = %q, want readonly (not upgraded)", authz.GrantedMode)
	}
}

func TestAuthorizeObserveGlobPatterns(t *testing.T) {
	t.Parallel()

	daemon := &Daemon{
		lastConfig: &schema.MachineConfig{
			DefaultObservePolicy: &schema.ObservePolicy{
				AllowedObservers: []string{"ops/*"},
			},
			Principals: []schema.PrincipalAssignment{
				{Localpart: "iree/amdgpu/pm", AutoStart: true},
			},
		},
	}

	// ops/alice matches ops/*
	authz := daemon.authorizeObserve("@ops/alice:bureau.local", "iree/amdgpu/pm", "readonly")
	if !authz.Allowed {
		t.Error("expected allow: ops/alice matches ops/*")
	}

	// ops/team/lead does NOT match ops/* (single segment only).
	authz = daemon.authorizeObserve("@ops/team/lead:bureau.local", "iree/amdgpu/pm", "readonly")
	if authz.Allowed {
		t.Error("expected deny: ops/team/lead should not match ops/* (single segment)")
	}

	// iree/builder does NOT match ops/*.
	authz = daemon.authorizeObserve("@iree/builder:bureau.local", "iree/amdgpu/pm", "readonly")
	if authz.Allowed {
		t.Error("expected deny: iree/builder does not match ops/*")
	}
}

func TestAuthorizeObserveUnknownPrincipal(t *testing.T) {
	t.Parallel()

	daemon := &Daemon{
		lastConfig: &schema.MachineConfig{
			DefaultObservePolicy: &schema.ObservePolicy{
				AllowedObservers: []string{"**"},
			},
			// No principals listed — an unknown principal still uses
			// the default policy.
		},
	}

	authz := daemon.authorizeObserve("@ben:bureau.local", "unknown/principal", "readonly")
	if !authz.Allowed {
		t.Error("expected allow: unknown principal should fall back to default policy")
	}
}

func TestAuthorizeListFiltering(t *testing.T) {
	t.Parallel()

	daemon := &Daemon{
		lastConfig: &schema.MachineConfig{
			Principals: []schema.PrincipalAssignment{
				{
					Localpart: "iree/amdgpu/pm",
					AutoStart: true,
					ObservePolicy: &schema.ObservePolicy{
						AllowedObservers: []string{"ops/**"},
					},
				},
				{
					Localpart: "iree/amdgpu/builder",
					AutoStart: true,
					ObservePolicy: &schema.ObservePolicy{
						AllowedObservers: []string{"**"},
					},
				},
			},
		},
	}

	// ben can only see builder, not pm.
	if daemon.authorizeList("@ben:bureau.local", "iree/amdgpu/pm") {
		t.Error("ben should not be authorized to list iree/amdgpu/pm")
	}
	if !daemon.authorizeList("@ben:bureau.local", "iree/amdgpu/builder") {
		t.Error("ben should be authorized to list iree/amdgpu/builder")
	}

	// ops/alice can see both.
	if !daemon.authorizeList("@ops/alice:bureau.local", "iree/amdgpu/pm") {
		t.Error("ops/alice should be authorized to list iree/amdgpu/pm")
	}
	if !daemon.authorizeList("@ops/alice:bureau.local", "iree/amdgpu/builder") {
		t.Error("ops/alice should be authorized to list iree/amdgpu/builder")
	}
}

func TestFindObservePolicyFallback(t *testing.T) {
	t.Parallel()

	defaultPolicy := &schema.ObservePolicy{
		AllowedObservers: []string{"**"},
	}
	principalPolicy := &schema.ObservePolicy{
		AllowedObservers: []string{"ops/**"},
	}

	daemon := &Daemon{
		lastConfig: &schema.MachineConfig{
			DefaultObservePolicy: defaultPolicy,
			Principals: []schema.PrincipalAssignment{
				{
					Localpart:     "with-policy",
					ObservePolicy: principalPolicy,
				},
				{
					Localpart: "without-policy",
				},
			},
		},
	}

	// Principal with its own policy should use it.
	result := daemon.findObservePolicy("with-policy")
	if result != principalPolicy {
		t.Error("expected principal-level policy for with-policy")
	}

	// Principal without its own policy should fall back to default.
	result = daemon.findObservePolicy("without-policy")
	if result != defaultPolicy {
		t.Error("expected default policy for without-policy")
	}

	// Unknown principal should also fall back to default.
	result = daemon.findObservePolicy("unknown")
	if result != defaultPolicy {
		t.Error("expected default policy for unknown principal")
	}
}
