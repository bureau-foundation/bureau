// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestFleetServiceContentRoundTrip(t *testing.T) {
	original := FleetServiceContent{
		Template: "bureau/template:whisper-stt",
		Replicas: ReplicaSpec{Min: 1, Max: 3},
		Resources: ResourceRequirements{
			MemoryMB:      8192,
			CPUMillicores: 2000,
			GPU:           true,
			GPUMemoryMB:   24576,
		},
		Placement: PlacementConstraints{
			Requires:          []string{"gpu", "persistent"},
			PreferredMachines: []string{"machine/workstation", "machine/spare-workstation"},
			AntiAffinity:      []string{"service/stt/whisper"},
			CoLocateWith:      []string{"service/rag/embeddings"},
		},
		Failover:     "migrate",
		ServiceRooms: []string{"#iree/**"},
		Priority:     10,
		Fleet:        "service/fleet/prod",
		ManagedBy:    "service/fleet/prod",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "template", "bureau/template:whisper-stt")
	assertField(t, raw, "failover", "migrate")
	assertField(t, raw, "priority", float64(10))
	assertField(t, raw, "fleet", "service/fleet/prod")
	assertField(t, raw, "managed_by", "service/fleet/prod")

	// Verify replicas nested struct.
	replicas, ok := raw["replicas"].(map[string]any)
	if !ok {
		t.Fatal("replicas field missing or wrong type")
	}
	assertField(t, replicas, "min", float64(1))
	assertField(t, replicas, "max", float64(3))

	// Verify resources nested struct.
	resources, ok := raw["resources"].(map[string]any)
	if !ok {
		t.Fatal("resources field missing or wrong type")
	}
	assertField(t, resources, "memory_mb", float64(8192))
	assertField(t, resources, "cpu_millicores", float64(2000))
	assertField(t, resources, "gpu", true)
	assertField(t, resources, "gpu_memory_mb", float64(24576))

	// Verify placement nested struct.
	placement, ok := raw["placement"].(map[string]any)
	if !ok {
		t.Fatal("placement field missing or wrong type")
	}
	requires, ok := placement["requires"].([]any)
	if !ok || len(requires) != 2 {
		t.Fatalf("placement.requires: got %v, want 2 elements", requires)
	}
	if requires[0] != "gpu" || requires[1] != "persistent" {
		t.Errorf("placement.requires = %v, want [gpu persistent]", requires)
	}

	// Verify service_rooms array.
	serviceRooms, ok := raw["service_rooms"].([]any)
	if !ok || len(serviceRooms) != 1 {
		t.Fatalf("service_rooms: got %v, want 1 element", serviceRooms)
	}
	if serviceRooms[0] != "#iree/**" {
		t.Errorf("service_rooms[0] = %v, want #iree/**", serviceRooms[0])
	}

	var decoded FleetServiceContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Template != original.Template {
		t.Errorf("Template: got %q, want %q", decoded.Template, original.Template)
	}
	if decoded.Replicas != original.Replicas {
		t.Errorf("Replicas: got %+v, want %+v", decoded.Replicas, original.Replicas)
	}
	if !reflect.DeepEqual(decoded.Resources, original.Resources) {
		t.Errorf("Resources: got %+v, want %+v", decoded.Resources, original.Resources)
	}
	if !reflect.DeepEqual(decoded.Placement, original.Placement) {
		t.Errorf("Placement: got %+v, want %+v", decoded.Placement, original.Placement)
	}
	if decoded.Failover != original.Failover {
		t.Errorf("Failover: got %q, want %q", decoded.Failover, original.Failover)
	}
	if decoded.Priority != original.Priority {
		t.Errorf("Priority: got %d, want %d", decoded.Priority, original.Priority)
	}
}

func TestFleetServiceContentOmitsOptionalFields(t *testing.T) {
	// Minimal fleet service: required fields only.
	service := FleetServiceContent{
		Template:  "bureau/template:minimal",
		Replicas:  ReplicaSpec{Min: 1},
		Placement: PlacementConstraints{},
		Failover:  "none",
	}

	data, err := json.Marshal(service)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{
		"ha_class", "service_rooms", "scheduling", "fleet", "payload", "managed_by",
	} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty", field)
		}
	}

	// Replicas.max should be omitted when 0.
	replicas := raw["replicas"].(map[string]any)
	if _, exists := replicas["max"]; exists {
		t.Error("replicas.max should be omitted when zero")
	}
}

func TestFleetServiceContentWithScheduling(t *testing.T) {
	service := FleetServiceContent{
		Template:  "bureau/template:ml-training",
		Replicas:  ReplicaSpec{Min: 1},
		Placement: PlacementConstraints{Requires: []string{"gpu"}},
		Failover:  "none",
		Priority:  100,
		Scheduling: &SchedulingSpec{
			Class:       "batch",
			Preemptible: true,
			PreferredWindows: []TimeWindow{
				{Days: []string{"mon", "tue", "wed", "thu", "fri"}, StartHour: 22, EndHour: 6},
				{StartHour: 0, EndHour: 24},
			},
			CheckpointSignal:  "SIGUSR1",
			DrainGraceSeconds: 300,
		},
	}

	data, err := json.Marshal(service)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	scheduling, ok := raw["scheduling"].(map[string]any)
	if !ok {
		t.Fatal("scheduling field missing or wrong type")
	}
	assertField(t, scheduling, "class", "batch")
	assertField(t, scheduling, "preemptible", true)
	assertField(t, scheduling, "checkpoint_signal", "SIGUSR1")
	assertField(t, scheduling, "drain_grace_seconds", float64(300))

	windows, ok := scheduling["preferred_windows"].([]any)
	if !ok || len(windows) != 2 {
		t.Fatalf("preferred_windows: got %v, want 2 elements", windows)
	}

	firstWindow := windows[0].(map[string]any)
	assertField(t, firstWindow, "start_hour", float64(22))
	assertField(t, firstWindow, "end_hour", float64(6))
	days, ok := firstWindow["days"].([]any)
	if !ok || len(days) != 5 {
		t.Fatalf("first window days: got %v, want 5 elements", days)
	}

	// Second window: all days (days omitted).
	secondWindow := windows[1].(map[string]any)
	if _, exists := secondWindow["days"]; exists {
		t.Error("second window should omit days when empty (applies to all days)")
	}

	var decoded FleetServiceContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Scheduling == nil {
		t.Fatal("Scheduling should not be nil after round-trip")
	}
	if decoded.Scheduling.Class != "batch" {
		t.Errorf("Scheduling.Class: got %q, want %q", decoded.Scheduling.Class, "batch")
	}
	if !decoded.Scheduling.Preemptible {
		t.Error("Scheduling.Preemptible should be true")
	}
	if len(decoded.Scheduling.PreferredWindows) != 2 {
		t.Errorf("PreferredWindows count: got %d, want 2", len(decoded.Scheduling.PreferredWindows))
	}
}

func TestFleetServiceContentWithPayload(t *testing.T) {
	service := FleetServiceContent{
		Template:  "bureau/template:agent",
		Replicas:  ReplicaSpec{Min: 1},
		Placement: PlacementConstraints{},
		Failover:  "migrate",
		Payload:   json.RawMessage(`{"model":"claude-3.5-sonnet","temperature":0}`),
	}

	data, err := json.Marshal(service)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	payload, ok := raw["payload"].(map[string]any)
	if !ok {
		t.Fatal("payload should be a JSON object")
	}
	assertField(t, payload, "model", "claude-3.5-sonnet")

	var decoded FleetServiceContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Payload == nil {
		t.Fatal("Payload should not be nil after round-trip")
	}
}

func TestFleetServiceContentWithAuthorization(t *testing.T) {
	service := FleetServiceContent{
		Template:  "bureau/template:ticket-service",
		Replicas:  ReplicaSpec{Min: 1},
		Placement: PlacementConstraints{},
		Failover:  "migrate",
		MatrixPolicy: &MatrixPolicy{
			AllowJoin:       true,
			AllowInvite:     true,
			AllowRoomCreate: false,
		},
		ServiceVisibility: []string{"service/**"},
		Authorization: &AuthorizationPolicy{
			Grants: []Grant{
				{Actions: []string{"ticket/create", "ticket/update"}},
			},
		},
	}

	data, err := json.Marshal(service)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	if _, exists := raw["matrix_policy"]; !exists {
		t.Error("matrix_policy should be present in JSON")
	}
	visibility, ok := raw["service_visibility"].([]any)
	if !ok || len(visibility) != 1 {
		t.Errorf("service_visibility = %v, want [service/**]", raw["service_visibility"])
	}
	if _, exists := raw["authorization"]; !exists {
		t.Error("authorization should be present in JSON")
	}

	var decoded FleetServiceContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.MatrixPolicy == nil {
		t.Fatal("MatrixPolicy should not be nil after round-trip")
	}
	if !decoded.MatrixPolicy.AllowJoin {
		t.Error("MatrixPolicy.AllowJoin should be true after round-trip")
	}
	if !decoded.MatrixPolicy.AllowInvite {
		t.Error("MatrixPolicy.AllowInvite should be true after round-trip")
	}
	if len(decoded.ServiceVisibility) != 1 || decoded.ServiceVisibility[0] != "service/**" {
		t.Errorf("ServiceVisibility = %v, want [service/**]", decoded.ServiceVisibility)
	}
	if decoded.Authorization == nil {
		t.Fatal("Authorization should not be nil after round-trip")
	}
	if len(decoded.Authorization.Grants) != 1 {
		t.Fatalf("Grants count = %d, want 1", len(decoded.Authorization.Grants))
	}
}

func TestFleetServiceContentOmitsNilAuthorization(t *testing.T) {
	service := FleetServiceContent{
		Template:  "bureau/template:worker",
		Replicas:  ReplicaSpec{Min: 1},
		Placement: PlacementConstraints{},
		Failover:  "none",
	}

	data, err := json.Marshal(service)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	if _, exists := raw["matrix_policy"]; exists {
		t.Error("matrix_policy should be omitted when nil")
	}
	if _, exists := raw["service_visibility"]; exists {
		t.Error("service_visibility should be omitted when nil")
	}
	if _, exists := raw["authorization"]; exists {
		t.Error("authorization should be omitted when nil")
	}
}

func TestMachineDefinitionContentRoundTrip(t *testing.T) {
	original := MachineDefinitionContent{
		Provider: "local",
		Labels:   map[string]string{"persistent": "true", "gpu": "rtx3080"},
		Resources: MachineResources{
			CPUCores:    12,
			MemoryMB:    32768,
			GPU:         "rtx3080",
			GPUCount:    1,
			GPUMemoryMB: 10240,
		},
		Provisioning: ProvisioningConfig{
			MACAddress:          "aa:bb:cc:dd:ee:ff",
			IPAddress:           "192.168.1.50",
			CostPerHourMilliUSD: 0,
		},
		Scaling: ScalingConfig{
			MinInstances: 0,
			MaxInstances: 1,
		},
		Lifecycle: MachineLifecycleConfig{
			ProvisionOn:        "demand",
			DeprovisionOn:      "idle",
			IdleTimeoutSeconds: 3600,
			WakeMethod:         "wol",
			SuspendMethod:      "ssh_command",
			SuspendCommand:     "systemctl suspend",
			WakeLatencySeconds: 30,
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "provider", "local")

	// Verify labels.
	labels, ok := raw["labels"].(map[string]any)
	if !ok {
		t.Fatal("labels field missing or wrong type")
	}
	assertField(t, labels, "persistent", "true")
	assertField(t, labels, "gpu", "rtx3080")

	// Verify resources.
	resources, ok := raw["resources"].(map[string]any)
	if !ok {
		t.Fatal("resources field missing or wrong type")
	}
	assertField(t, resources, "cpu_cores", float64(12))
	assertField(t, resources, "memory_mb", float64(32768))
	assertField(t, resources, "gpu", "rtx3080")
	assertField(t, resources, "gpu_count", float64(1))
	assertField(t, resources, "gpu_memory_mb", float64(10240))

	// Verify provisioning.
	provisioning, ok := raw["provisioning"].(map[string]any)
	if !ok {
		t.Fatal("provisioning field missing or wrong type")
	}
	assertField(t, provisioning, "mac_address", "aa:bb:cc:dd:ee:ff")
	assertField(t, provisioning, "ip_address", "192.168.1.50")

	// Verify lifecycle.
	lifecycle, ok := raw["lifecycle"].(map[string]any)
	if !ok {
		t.Fatal("lifecycle field missing or wrong type")
	}
	assertField(t, lifecycle, "provision_on", "demand")
	assertField(t, lifecycle, "deprovision_on", "idle")
	assertField(t, lifecycle, "idle_timeout_seconds", float64(3600))
	assertField(t, lifecycle, "wake_method", "wol")
	assertField(t, lifecycle, "suspend_method", "ssh_command")
	assertField(t, lifecycle, "suspend_command", "systemctl suspend")
	assertField(t, lifecycle, "wake_latency_seconds", float64(30))

	var decoded MachineDefinitionContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", decoded, original)
	}
}

func TestMachineDefinitionCloudProvider(t *testing.T) {
	definition := MachineDefinitionContent{
		Provider: "gcloud",
		Labels:   map[string]string{"ephemeral": "true", "gpu": "h100"},
		Resources: MachineResources{
			CPUCores:    96,
			MemoryMB:    348160,
			GPU:         "h100",
			GPUCount:    1,
			GPUMemoryMB: 81920,
		},
		Provisioning: ProvisioningConfig{
			InstanceType:        "a3-highgpu-1g",
			Region:              "us-central1",
			BootImage:           "bureau-runner-v1",
			CredentialName:      "gcloud-compute",
			CostPerHourMilliUSD: 3500,
		},
		Scaling: ScalingConfig{
			MinInstances: 0,
			MaxInstances: 2,
		},
		Lifecycle: MachineLifecycleConfig{
			ProvisionOn:   "demand",
			DeprovisionOn: "idle",
			WakeMethod:    "api",
			SuspendMethod: "api",
		},
	}

	data, err := json.Marshal(definition)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	provisioning := raw["provisioning"].(map[string]any)
	assertField(t, provisioning, "instance_type", "a3-highgpu-1g")
	assertField(t, provisioning, "region", "us-central1")
	assertField(t, provisioning, "boot_image", "bureau-runner-v1")
	assertField(t, provisioning, "credential_name", "gcloud-compute")
	assertField(t, provisioning, "cost_per_hour_milliusd", float64(3500))

	var decoded MachineDefinitionContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Provisioning.CostPerHourMilliUSD != 3500 {
		t.Errorf("CostPerHourMilliUSD: got %d, want 3500", decoded.Provisioning.CostPerHourMilliUSD)
	}
}

func TestFleetConfigContentRoundTrip(t *testing.T) {
	original := FleetConfigContent{
		RebalancePolicy:          "alert",
		PressureThresholdCPU:     85,
		PressureThresholdMemory:  90,
		PressureThresholdGPU:     95,
		PressureSustainedSeconds: 300,
		RebalanceCooldownSeconds: 600,
		HeartbeatIntervalSeconds: 30,
		PreemptibleBy:            []string{"service/fleet/prod"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "rebalance_policy", "alert")
	assertField(t, raw, "pressure_threshold_cpu", float64(85))
	assertField(t, raw, "pressure_threshold_memory", float64(90))
	assertField(t, raw, "pressure_threshold_gpu", float64(95))
	assertField(t, raw, "pressure_sustained_seconds", float64(300))
	assertField(t, raw, "rebalance_cooldown_seconds", float64(600))
	assertField(t, raw, "heartbeat_interval_seconds", float64(30))

	preemptibleBy, ok := raw["preemptible_by"].([]any)
	if !ok || len(preemptibleBy) != 1 {
		t.Fatalf("preemptible_by: got %v, want 1 element", preemptibleBy)
	}
	if preemptibleBy[0] != "service/fleet/prod" {
		t.Errorf("preemptible_by[0] = %v, want service/fleet/prod", preemptibleBy[0])
	}

	var decoded FleetConfigContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", decoded, original)
	}
}

func TestFleetConfigContentOmitsDefaults(t *testing.T) {
	// Minimal config: only rebalance_policy is semantically required.
	config := FleetConfigContent{
		RebalancePolicy: "auto",
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{
		"pressure_threshold_cpu", "pressure_threshold_memory", "pressure_threshold_gpu",
		"pressure_sustained_seconds", "rebalance_cooldown_seconds",
		"heartbeat_interval_seconds", "preemptible_by",
	} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when zero/empty", field)
		}
	}
}

func TestHALeaseContentRoundTrip(t *testing.T) {
	original := HALeaseContent{
		Holder:     "machine/workstation",
		ExpiresAt:  "2026-02-14T12:00:30Z",
		Service:    "service/fleet/prod",
		AcquiredAt: "2026-02-14T12:00:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "holder", "machine/workstation")
	assertField(t, raw, "expires_at", "2026-02-14T12:00:30Z")
	assertField(t, raw, "service", "service/fleet/prod")
	assertField(t, raw, "acquired_at", "2026-02-14T12:00:00Z")

	var decoded HALeaseContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestServiceStatusContentRoundTrip(t *testing.T) {
	original := ServiceStatusContent{
		Machine:           "machine/workstation",
		QueueDepth:        34,
		AvgLatencyMS:      250,
		RequestsPerMinute: 120,
		ErrorRatePercent:  2,
		Healthy:           true,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "machine", "machine/workstation")
	assertField(t, raw, "queue_depth", float64(34))
	assertField(t, raw, "avg_latency_ms", float64(250))
	assertField(t, raw, "requests_per_minute", float64(120))
	assertField(t, raw, "error_rate_percent", float64(2))
	assertField(t, raw, "healthy", true)

	var decoded ServiceStatusContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestServiceStatusContentOmitsOptionalMetrics(t *testing.T) {
	// Service that only reports health, no detailed metrics.
	status := ServiceStatusContent{
		Machine: "machine/pi-kitchen",
		Healthy: true,
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"queue_depth", "avg_latency_ms", "requests_per_minute", "error_rate_percent"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when zero", field)
		}
	}
}

func TestPrincipalResourceUsageRoundTrip(t *testing.T) {
	original := PrincipalResourceUsage{
		CPUPercent:  42,
		MemoryMB:    2048,
		GPUPercent:  79,
		GPUMemoryMB: 12288,
		Status:      "running",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "cpu_percent", float64(42))
	assertField(t, raw, "memory_mb", float64(2048))
	assertField(t, raw, "gpu_percent", float64(79))
	assertField(t, raw, "gpu_memory_mb", float64(12288))
	assertField(t, raw, "status", "running")

	var decoded PrincipalResourceUsage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestPrincipalResourceUsageOmitsGPU(t *testing.T) {
	// Non-GPU sandbox: GPU fields should be omitted.
	usage := PrincipalResourceUsage{
		CPUPercent: 15,
		MemoryMB:   512,
		Status:     "idle",
	}

	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"gpu_percent", "gpu_memory_mb"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when zero", field)
		}
	}
}

func TestMachineStatusWithPrincipals(t *testing.T) {
	// Enriched heartbeat with per-principal resource usage.
	status := MachineStatus{
		Principal:     "@machine/workstation:bureau.local",
		CPUPercent:    65,
		MemoryUsedMB:  24576,
		Sandboxes:     SandboxCounts{Running: 3},
		UptimeSeconds: 86400,
		Principals: map[string]PrincipalResourceUsage{
			"service/stt/whisper": {
				CPUPercent:  30,
				MemoryMB:    8192,
				GPUPercent:  79,
				GPUMemoryMB: 12288,
				Status:      "running",
			},
			"iree/amdgpu/pm": {
				CPUPercent: 25,
				MemoryMB:   4096,
				Status:     "running",
			},
			"service/ticket/iree": {
				CPUPercent: 2,
				MemoryMB:   256,
				Status:     "idle",
			},
		},
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Verify principals map is present.
	principals, ok := raw["principals"].(map[string]any)
	if !ok {
		t.Fatal("principals field missing or wrong type")
	}
	if len(principals) != 3 {
		t.Fatalf("principals count = %d, want 3", len(principals))
	}

	// Verify a specific principal.
	whisper, ok := principals["service/stt/whisper"].(map[string]any)
	if !ok {
		t.Fatal("service/stt/whisper missing from principals")
	}
	assertField(t, whisper, "cpu_percent", float64(30))
	assertField(t, whisper, "memory_mb", float64(8192))
	assertField(t, whisper, "gpu_percent", float64(79))
	assertField(t, whisper, "status", "running")

	var decoded MachineStatus
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded.Principals) != 3 {
		t.Fatalf("round-trip principals count = %d, want 3", len(decoded.Principals))
	}
	decodedWhisper := decoded.Principals["service/stt/whisper"]
	if decodedWhisper.CPUPercent != 30 || decodedWhisper.GPUPercent != 79 {
		t.Errorf("round-trip whisper: got %+v", decodedWhisper)
	}
}

func TestMachineStatusWithoutPrincipals(t *testing.T) {
	// Old-style heartbeat without per-principal data.
	status := MachineStatus{
		Principal:     "@machine/pi-kitchen:bureau.local",
		CPUPercent:    15,
		MemoryUsedMB:  819,
		Sandboxes:     SandboxCounts{Running: 1},
		UptimeSeconds: 3600,
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Principals should be omitted from old-style heartbeats.
	if _, exists := raw["principals"]; exists {
		t.Error("principals should be omitted when nil")
	}

	// Verify backward compat: old heartbeat still round-trips cleanly.
	var decoded MachineStatus
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Principals != nil {
		t.Error("Principals should be nil when absent from JSON")
	}
	if decoded.CPUPercent != 15 {
		t.Errorf("CPUPercent: got %d, want 15", decoded.CPUPercent)
	}
}

func TestMachineInfoWithLabels(t *testing.T) {
	info := MachineInfo{
		Principal:     "@machine/workstation:bureau.local",
		Hostname:      "workstation",
		KernelVersion: "6.14.0-37-generic",
		CPU: CPUInfo{
			Model:          "AMD Ryzen Threadripper PRO 7995WX 96-Cores",
			Sockets:        1,
			CoresPerSocket: 96,
			ThreadsPerCore: 2,
		},
		MemoryTotalMB: 515413,
		Labels: map[string]string{
			"persistent": "true",
			"gpu":        "rtx4090",
			"tier":       "production",
		},
		DaemonVersion: "v0.1.0-dev",
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	labels, ok := raw["labels"].(map[string]any)
	if !ok {
		t.Fatal("labels field missing or wrong type")
	}
	assertField(t, labels, "persistent", "true")
	assertField(t, labels, "gpu", "rtx4090")
	assertField(t, labels, "tier", "production")

	var decoded MachineInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded.Labels, info.Labels) {
		t.Errorf("Labels: got %v, want %v", decoded.Labels, info.Labels)
	}
}

func TestMachineInfoLabelsOmittedWhenEmpty(t *testing.T) {
	info := MachineInfo{
		Principal:     "@machine/pi-kitchen:bureau.local",
		Hostname:      "pi-kitchen",
		KernelVersion: "6.8.0-44-generic",
		CPU: CPUInfo{
			Model:          "BCM2712",
			Sockets:        1,
			CoresPerSocket: 4,
			ThreadsPerCore: 1,
		},
		MemoryTotalMB: 8192,
		DaemonVersion: "v0.1.0-dev",
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	if _, exists := raw["labels"]; exists {
		t.Error("labels should be omitted when nil")
	}
}

func TestFleetAlertContentRoundTrip(t *testing.T) {
	original := FleetAlertContent{
		AlertType: "failover",
		Fleet:     "service/fleet/prod",
		Service:   "service/stt/whisper",
		Machine:   "machine/cloud-gpu-1",
		Message:   "Machine offline for 90 seconds. Service requires re-placement.",
		ProposedActions: []ProposedAction{
			{
				Action:      "move",
				Service:     "service/stt/whisper",
				FromMachine: "machine/cloud-gpu-1",
				ToMachine:   "machine/workstation",
				Score:       85,
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "alert_type", "failover")
	assertField(t, raw, "fleet", "service/fleet/prod")
	assertField(t, raw, "service", "service/stt/whisper")
	assertField(t, raw, "machine", "machine/cloud-gpu-1")
	assertField(t, raw, "message", "Machine offline for 90 seconds. Service requires re-placement.")

	actions, ok := raw["proposed_actions"].([]any)
	if !ok || len(actions) != 1 {
		t.Fatalf("proposed_actions: got %v, want 1 element", actions)
	}
	action := actions[0].(map[string]any)
	assertField(t, action, "action", "move")
	assertField(t, action, "service", "service/stt/whisper")
	assertField(t, action, "from_machine", "machine/cloud-gpu-1")
	assertField(t, action, "to_machine", "machine/workstation")
	assertField(t, action, "score", float64(85))

	var decoded FleetAlertContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.AlertType != original.AlertType {
		t.Errorf("AlertType: got %q, want %q", decoded.AlertType, original.AlertType)
	}
	if len(decoded.ProposedActions) != 1 {
		t.Fatalf("ProposedActions count: got %d, want 1", len(decoded.ProposedActions))
	}
	if decoded.ProposedActions[0].Score != 85 {
		t.Errorf("ProposedActions[0].Score: got %d, want 85", decoded.ProposedActions[0].Score)
	}
}

func TestFleetAlertContentOmitsOptionalFields(t *testing.T) {
	// Alert without service/machine context (e.g., capacity request).
	alert := FleetAlertContent{
		AlertType: "capacity_request",
		Fleet:     "service/fleet/prod",
		Message:   "No machine satisfies placement constraints for service/ml/training.",
	}

	data, err := json.Marshal(alert)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"service", "machine", "proposed_actions"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty", field)
		}
	}
}
