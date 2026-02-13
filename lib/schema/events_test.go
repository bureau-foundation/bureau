// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestEventTypeConstants(t *testing.T) {
	// Verify the event type strings match the Matrix convention (m.bureau.*).
	// These are wire-format identifiers that must never change without a
	// coordinated migration.
	tests := []struct {
		name     string
		constant string
		want     string
	}{
		{"machine_key", EventTypeMachineKey, "m.bureau.machine_key"},
		{"machine_info", EventTypeMachineInfo, "m.bureau.machine_info"},
		{"machine_status", EventTypeMachineStatus, "m.bureau.machine_status"},
		{"machine_config", EventTypeMachineConfig, "m.bureau.machine_config"},
		{"credentials", EventTypeCredentials, "m.bureau.credentials"},
		{"service", EventTypeService, "m.bureau.service"},
		{"layout", EventTypeLayout, "m.bureau.layout"},
		{"template", EventTypeTemplate, "m.bureau.template"},
		{"project", EventTypeProject, "m.bureau.project"},
		{"workspace", EventTypeWorkspace, "m.bureau.workspace"},
		{"pipeline", EventTypePipeline, "m.bureau.pipeline"},
		{"pipeline_result", EventTypePipelineResult, "m.bureau.pipeline_result"},
		{"room_service", EventTypeRoomService, "m.bureau.room_service"},
		{"ticket", EventTypeTicket, "m.bureau.ticket"},
		{"ticket_config", EventTypeTicketConfig, "m.bureau.ticket_config"},
		{"artifact_scope", EventTypeArtifactScope, "m.bureau.artifact_scope"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.constant != test.want {
				t.Errorf("%s = %q, want %q", test.name, test.constant, test.want)
			}
		})
	}
}

func TestMachineKeyRoundTrip(t *testing.T) {
	original := MachineKey{
		Algorithm: "age-x25519",
		PublicKey: "age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs3290gq",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON field names match the wire format.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "algorithm", "age-x25519")
	assertField(t, raw, "public_key", original.PublicKey)

	var decoded MachineKey
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestMachineInfoRoundTrip(t *testing.T) {
	original := MachineInfo{
		Principal:     "@machine/workstation:bureau.local",
		Hostname:      "workstation",
		KernelVersion: "6.14.0-37-generic",
		BoardVendor:   "ASUS",
		BoardName:     "Pro WS WRX90E-SAGE SE",
		CPU: CPUInfo{
			Model:          "AMD Ryzen Threadripper PRO 7995WX 96-Cores",
			Sockets:        1,
			CoresPerSocket: 96,
			ThreadsPerCore: 2,
			L3CacheKB:      32768,
		},
		MemoryTotalMB: 515413,
		SwapTotalMB:   8191,
		NUMANodes:     4,
		GPUs: []GPUInfo{
			{
				Vendor:                            "AMD",
				PCIDeviceID:                       "0x744a",
				PCISlot:                           "0000:c3:00.0",
				VRAMTotalBytes:                    48301604864,
				UniqueID:                          "30437a849c458574",
				VBIOSVersion:                      "113-APM7489-DS2-100",
				VRAMVendor:                        "samsung",
				PCIeLinkWidth:                     16,
				ThermalLimitCriticalMillidegrees:  100000,
				ThermalLimitEmergencyMillidegrees: 105000,
				Driver:                            "amdgpu",
			},
			{
				Vendor:                            "AMD",
				PCIDeviceID:                       "0x744a",
				PCISlot:                           "0000:e3:00.0",
				VRAMTotalBytes:                    48301604864,
				UniqueID:                          "4939e1d93d24ff77",
				VBIOSVersion:                      "113-APM7489-DS2-100",
				VRAMVendor:                        "samsung",
				PCIeLinkWidth:                     16,
				ThermalLimitCriticalMillidegrees:  100000,
				ThermalLimitEmergencyMillidegrees: 105000,
				Driver:                            "amdgpu",
			},
			{
				Vendor:        "NVIDIA",
				ModelName:     "NVIDIA GeForce RTX 4090",
				PCIDeviceID:   "0x2684",
				PCISlot:       "0000:01:00.0",
				UniqueID:      "GPU-12345678-abcd-efgh-ijkl-123456789abc",
				VBIOSVersion:  "95.02.3c.80.b8",
				PCIeLinkWidth: 16,
				Driver:        "nvidia",
			},
		},
		DaemonVersion: "v0.1.0-dev",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "principal", "@machine/workstation:bureau.local")
	assertField(t, raw, "hostname", "workstation")
	assertField(t, raw, "kernel_version", "6.14.0-37-generic")
	assertField(t, raw, "board_vendor", "ASUS")
	assertField(t, raw, "board_name", "Pro WS WRX90E-SAGE SE")
	assertField(t, raw, "memory_total_mb", float64(515413))
	assertField(t, raw, "swap_total_mb", float64(8191))
	assertField(t, raw, "numa_nodes", float64(4))
	assertField(t, raw, "daemon_version", "v0.1.0-dev")

	// Verify CPU nested struct wire format.
	cpu, ok := raw["cpu"].(map[string]any)
	if !ok {
		t.Fatal("cpu field missing or wrong type")
	}
	assertField(t, cpu, "model", "AMD Ryzen Threadripper PRO 7995WX 96-Cores")
	assertField(t, cpu, "sockets", float64(1))
	assertField(t, cpu, "cores_per_socket", float64(96))
	assertField(t, cpu, "threads_per_core", float64(2))
	assertField(t, cpu, "l3_cache_kb", float64(32768))

	// Verify GPUs array wire format.
	gpus, ok := raw["gpus"].([]any)
	if !ok {
		t.Fatal("gpus field missing or wrong type")
	}
	if len(gpus) != 3 {
		t.Fatalf("gpus count = %d, want 3", len(gpus))
	}
	firstGPU := gpus[0].(map[string]any)
	assertField(t, firstGPU, "vendor", "AMD")
	assertField(t, firstGPU, "pci_device_id", "0x744a")
	assertField(t, firstGPU, "pci_slot", "0000:c3:00.0")
	assertField(t, firstGPU, "vram_total_bytes", float64(48301604864))
	assertField(t, firstGPU, "unique_id", "30437a849c458574")
	assertField(t, firstGPU, "vbios_version", "113-APM7489-DS2-100")
	assertField(t, firstGPU, "vram_vendor", "samsung")
	assertField(t, firstGPU, "pcie_link_width", float64(16))
	assertField(t, firstGPU, "thermal_limit_critical_millidegrees", float64(100000))
	assertField(t, firstGPU, "thermal_limit_emergency_millidegrees", float64(105000))
	assertField(t, firstGPU, "driver", "amdgpu")

	// Verify NVIDIA GPU wire format (third entry).
	nvidiaGPU := gpus[2].(map[string]any)
	assertField(t, nvidiaGPU, "vendor", "NVIDIA")
	assertField(t, nvidiaGPU, "model_name", "NVIDIA GeForce RTX 4090")
	assertField(t, nvidiaGPU, "pci_device_id", "0x2684")
	assertField(t, nvidiaGPU, "driver", "nvidia")
	assertField(t, nvidiaGPU, "unique_id", "GPU-12345678-abcd-efgh-ijkl-123456789abc")

	var decoded MachineInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestMachineInfoOmitsOptionalFields(t *testing.T) {
	// Minimal MachineInfo: no board info, no GPUs, no swap, no NUMA.
	// This is what a VM or container without DMI or GPUs would report.
	info := MachineInfo{
		Principal:     "@machine/cloud-vm:bureau.local",
		Hostname:      "cloud-vm",
		KernelVersion: "6.8.0-44-generic",
		CPU: CPUInfo{
			Model:          "Intel Xeon Platinum 8375C",
			Sockets:        1,
			CoresPerSocket: 8,
			ThreadsPerCore: 2,
		},
		MemoryTotalMB: 32768,
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

	// Optional fields should be omitted from the wire format.
	for _, field := range []string{"board_vendor", "board_name", "swap_total_mb", "numa_nodes", "gpus"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when zero/empty", field)
		}
	}

	// L3 cache should be omitted from the CPU sub-object.
	cpu := raw["cpu"].(map[string]any)
	if _, exists := cpu["l3_cache_kb"]; exists {
		t.Error("l3_cache_kb should be omitted when zero")
	}
}

func TestMachineStatusRoundTrip(t *testing.T) {
	original := MachineStatus{
		Principal:    "@machine/workstation:bureau.local",
		CPUPercent:   42,
		MemoryUsedMB: 12595,
		GPUStats: []GPUStatus{
			{
				PCISlot:                 "0000:c3:00.0",
				UtilizationPercent:      79,
				VRAMUsedBytes:           27860992,
				TemperatureMillidegrees: 55000,
				PowerDrawWatts:          74,
				GraphicsClockMHz:        879,
				MemoryClockMHz:          772,
			},
			{
				PCISlot:                 "0000:e3:00.0",
				UtilizationPercent:      0,
				VRAMUsedBytes:           27860992,
				TemperatureMillidegrees: 49000,
				PowerDrawWatts:          28,
				GraphicsClockMHz:        4,
				MemoryClockMHz:          96,
			},
		},
		Sandboxes:     SandboxCounts{Running: 5, Idle: 2},
		UptimeSeconds: 86400,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "principal", "@machine/workstation:bureau.local")
	assertField(t, raw, "cpu_percent", float64(42))
	assertField(t, raw, "memory_used_mb", float64(12595))
	assertField(t, raw, "uptime_seconds", float64(86400))

	// Verify gpu_stats array wire format.
	gpuStats, ok := raw["gpu_stats"].([]any)
	if !ok {
		t.Fatal("gpu_stats field missing or wrong type")
	}
	if len(gpuStats) != 2 {
		t.Fatalf("gpu_stats count = %d, want 2", len(gpuStats))
	}
	firstGPU := gpuStats[0].(map[string]any)
	assertField(t, firstGPU, "pci_slot", "0000:c3:00.0")
	assertField(t, firstGPU, "utilization_percent", float64(79))
	assertField(t, firstGPU, "vram_used_bytes", float64(27860992))
	assertField(t, firstGPU, "temperature_millidegrees", float64(55000))
	assertField(t, firstGPU, "power_draw_watts", float64(74))
	assertField(t, firstGPU, "graphics_clock_mhz", float64(879))
	assertField(t, firstGPU, "memory_clock_mhz", float64(772))

	var decoded MachineStatus
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestMachineStatusOmitsGPUStatsWhenEmpty(t *testing.T) {
	status := MachineStatus{
		Principal:     "@machine/pi-kitchen:bureau.local",
		CPUPercent:    15,
		MemoryUsedMB:  819,
		Sandboxes:     SandboxCounts{Running: 1, Idle: 0},
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
	if _, exists := raw["gpu_stats"]; exists {
		t.Error("gpu_stats should be omitted when nil")
	}
}

func TestMachineConfigRoundTrip(t *testing.T) {
	original := MachineConfig{
		Principals: []PrincipalAssignment{
			{
				Localpart:         "iree/amdgpu/pm",
				Template:          "llm-agent",
				AutoStart:         true,
				Labels:            map[string]string{"role": "agent", "team": "iree"},
				ServiceVisibility: []string{"service/stt/*", "service/embedding/**"},
			},
			{
				Localpart: "service/stt/whisper",
				Template:  "whisper-stt",
				AutoStart: true,
				Labels:    map[string]string{"role": "service"},
			},
			{
				Localpart: "iree/amdgpu/codegen",
				Template:  "llm-agent",
				AutoStart: false,
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
	principals, ok := raw["principals"].([]any)
	if !ok {
		t.Fatal("principals field missing or wrong type")
	}
	if len(principals) != 3 {
		t.Fatalf("principals count = %d, want 3", len(principals))
	}

	first := principals[0].(map[string]any)
	assertField(t, first, "localpart", "iree/amdgpu/pm")
	assertField(t, first, "template", "llm-agent")
	assertField(t, first, "auto_start", true)

	// Verify labels appear in the wire format.
	labels, ok := first["labels"].(map[string]any)
	if !ok {
		t.Fatal("labels field missing or wrong type")
	}
	assertField(t, labels, "role", "agent")
	assertField(t, labels, "team", "iree")

	// Verify service_visibility appears in the wire format.
	visibility, ok := first["service_visibility"].([]any)
	if !ok {
		t.Fatal("service_visibility field missing or wrong type")
	}
	if len(visibility) != 2 {
		t.Fatalf("service_visibility count = %d, want 2", len(visibility))
	}
	if visibility[0] != "service/stt/*" || visibility[1] != "service/embedding/**" {
		t.Errorf("service_visibility = %v, want [service/stt/* service/embedding/**]", visibility)
	}

	// Verify service_visibility is omitted when nil.
	second := principals[1].(map[string]any)
	if _, exists := second["service_visibility"]; exists {
		t.Error("service_visibility should be omitted when nil")
	}

	// Verify labels are omitted when nil (third principal has no labels).
	third := principals[2].(map[string]any)
	if _, exists := third["labels"]; exists {
		t.Error("labels should be omitted when nil")
	}

	var decoded MachineConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded.Principals) != len(original.Principals) {
		t.Fatalf("round-trip principal count = %d, want %d", len(decoded.Principals), len(original.Principals))
	}
	for i := range original.Principals {
		if !reflect.DeepEqual(decoded.Principals[i], original.Principals[i]) {
			t.Errorf("principal[%d]: got %+v, want %+v", i, decoded.Principals[i], original.Principals[i])
		}
	}
}

func TestObservePolicyRoundTrip(t *testing.T) {
	original := ObservePolicy{
		AllowedObservers:   []string{"bureau-admin", "iree/**"},
		ReadWriteObservers: []string{"bureau-admin"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	allowedObservers, ok := raw["allowed_observers"].([]any)
	if !ok {
		t.Fatal("allowed_observers field missing or wrong type")
	}
	if len(allowedObservers) != 2 {
		t.Fatalf("allowed_observers count = %d, want 2", len(allowedObservers))
	}
	if allowedObservers[0] != "bureau-admin" || allowedObservers[1] != "iree/**" {
		t.Errorf("allowed_observers = %v, want [bureau-admin iree/**]", allowedObservers)
	}

	readWriteObservers, ok := raw["readwrite_observers"].([]any)
	if !ok {
		t.Fatal("readwrite_observers field missing or wrong type")
	}
	if len(readWriteObservers) != 1 || readWriteObservers[0] != "bureau-admin" {
		t.Errorf("readwrite_observers = %v, want [bureau-admin]", readWriteObservers)
	}

	var decoded ObservePolicy
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded.AllowedObservers) != 2 {
		t.Fatalf("AllowedObservers count = %d, want 2", len(decoded.AllowedObservers))
	}
	if decoded.AllowedObservers[0] != "bureau-admin" || decoded.AllowedObservers[1] != "iree/**" {
		t.Errorf("AllowedObservers = %v, want [bureau-admin iree/**]", decoded.AllowedObservers)
	}
	if len(decoded.ReadWriteObservers) != 1 || decoded.ReadWriteObservers[0] != "bureau-admin" {
		t.Errorf("ReadWriteObservers = %v, want [bureau-admin]", decoded.ReadWriteObservers)
	}
}

func TestObservePolicyOmitsEmptyFields(t *testing.T) {
	policy := ObservePolicy{}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	for _, field := range []string{"allowed_observers", "readwrite_observers"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty", field)
		}
	}
}

func TestObservePolicyDefaultDeny(t *testing.T) {
	// A nil ObservePolicy and an empty ObservePolicy both mean
	// "no observation allowed". Verify the zero value has no patterns.
	var policy ObservePolicy
	if len(policy.AllowedObservers) != 0 {
		t.Errorf("zero-value AllowedObservers should be empty, got %v", policy.AllowedObservers)
	}
	if len(policy.ReadWriteObservers) != 0 {
		t.Errorf("zero-value ReadWriteObservers should be empty, got %v", policy.ReadWriteObservers)
	}
}

func TestMachineConfigWithObservePolicy(t *testing.T) {
	// A MachineConfig with both DefaultObservePolicy and per-principal
	// ObservePolicy should round-trip correctly.
	config := MachineConfig{
		Principals: []PrincipalAssignment{
			{
				Localpart: "iree/amdgpu/pm",
				Template:  "llm-agent",
				AutoStart: true,
				ObservePolicy: &ObservePolicy{
					AllowedObservers:   []string{"bureau-admin", "iree/**"},
					ReadWriteObservers: []string{"bureau-admin"},
				},
			},
			{
				Localpart: "service/stt/whisper",
				Template:  "whisper-stt",
				AutoStart: true,
				// No per-principal policy — falls back to DefaultObservePolicy.
			},
		},
		DefaultObservePolicy: &ObservePolicy{
			AllowedObservers: []string{"bureau-admin"},
		},
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded MachineConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Check DefaultObservePolicy.
	if decoded.DefaultObservePolicy == nil {
		t.Fatal("DefaultObservePolicy should not be nil after round-trip")
	}
	if len(decoded.DefaultObservePolicy.AllowedObservers) != 1 ||
		decoded.DefaultObservePolicy.AllowedObservers[0] != "bureau-admin" {
		t.Errorf("DefaultObservePolicy.AllowedObservers = %v, want [bureau-admin]",
			decoded.DefaultObservePolicy.AllowedObservers)
	}

	// Check first principal's per-principal policy.
	if decoded.Principals[0].ObservePolicy == nil {
		t.Fatal("Principals[0].ObservePolicy should not be nil after round-trip")
	}
	if len(decoded.Principals[0].ObservePolicy.AllowedObservers) != 2 {
		t.Fatalf("Principals[0].ObservePolicy.AllowedObservers count = %d, want 2",
			len(decoded.Principals[0].ObservePolicy.AllowedObservers))
	}
	if len(decoded.Principals[0].ObservePolicy.ReadWriteObservers) != 1 {
		t.Fatalf("Principals[0].ObservePolicy.ReadWriteObservers count = %d, want 1",
			len(decoded.Principals[0].ObservePolicy.ReadWriteObservers))
	}

	// Check second principal has no per-principal policy.
	if decoded.Principals[1].ObservePolicy != nil {
		t.Errorf("Principals[1].ObservePolicy should be nil, got %+v",
			decoded.Principals[1].ObservePolicy)
	}

	// Verify the wire format has the right structure.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["default_observe_policy"]; !exists {
		t.Error("default_observe_policy should be present in JSON")
	}
	principals := raw["principals"].([]any)
	firstPrincipal := principals[0].(map[string]any)
	if _, exists := firstPrincipal["observe_policy"]; !exists {
		t.Error("first principal should have observe_policy in JSON")
	}
	secondPrincipal := principals[1].(map[string]any)
	if _, exists := secondPrincipal["observe_policy"]; exists {
		t.Error("second principal should not have observe_policy in JSON")
	}
}

func TestBureauVersionOnMachineConfig(t *testing.T) {
	config := MachineConfig{
		Principals: []PrincipalAssignment{
			{
				Localpart: "service/stt/whisper",
				Template:  "bureau/template:whisper-stt",
				AutoStart: true,
			},
		},
		BureauVersion: &BureauVersion{
			DaemonStorePath:   "/nix/store/abc123-bureau-daemon/bin/bureau-daemon",
			LauncherStorePath: "/nix/store/def456-bureau-launcher/bin/bureau-launcher",
			ProxyStorePath:    "/nix/store/ghi789-bureau-proxy/bin/bureau-proxy",
		},
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	bureauVersion, ok := raw["bureau_version"].(map[string]any)
	if !ok {
		t.Fatal("bureau_version field missing or wrong type")
	}
	assertField(t, bureauVersion, "daemon_store_path", "/nix/store/abc123-bureau-daemon/bin/bureau-daemon")
	assertField(t, bureauVersion, "launcher_store_path", "/nix/store/def456-bureau-launcher/bin/bureau-launcher")
	assertField(t, bureauVersion, "proxy_store_path", "/nix/store/ghi789-bureau-proxy/bin/bureau-proxy")

	var decoded MachineConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.BureauVersion == nil {
		t.Fatal("BureauVersion should not be nil after round-trip")
	}
	if decoded.BureauVersion.DaemonStorePath != config.BureauVersion.DaemonStorePath {
		t.Errorf("DaemonStorePath: got %q, want %q",
			decoded.BureauVersion.DaemonStorePath, config.BureauVersion.DaemonStorePath)
	}
	if decoded.BureauVersion.LauncherStorePath != config.BureauVersion.LauncherStorePath {
		t.Errorf("LauncherStorePath: got %q, want %q",
			decoded.BureauVersion.LauncherStorePath, config.BureauVersion.LauncherStorePath)
	}
	if decoded.BureauVersion.ProxyStorePath != config.BureauVersion.ProxyStorePath {
		t.Errorf("ProxyStorePath: got %q, want %q",
			decoded.BureauVersion.ProxyStorePath, config.BureauVersion.ProxyStorePath)
	}
}

func TestBureauVersionOmittedWhenNil(t *testing.T) {
	config := MachineConfig{
		Principals: []PrincipalAssignment{
			{
				Localpart: "service/stt/whisper",
				Template:  "bureau/template:whisper-stt",
				AutoStart: true,
			},
		},
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["bureau_version"]; exists {
		t.Error("bureau_version should be omitted when nil")
	}
}

func TestCredentialsRoundTrip(t *testing.T) {
	original := Credentials{
		Version:   1,
		Principal: "@iree/amdgpu/pm:bureau.local",
		EncryptedFor: []string{
			"@machine/workstation:bureau.local",
			"yubikey:operator-escrow",
		},
		Keys:          []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"},
		Ciphertext:    "YWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IFgyNTUxOSA...",
		ProvisionedBy: "@bureau/operator:bureau.local",
		ProvisionedAt: "2026-02-09T18:30:00Z",
		Signature:     "base64signature==",
		ExpiresAt:     "2026-08-09T18:30:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "version", float64(1))
	assertField(t, raw, "principal", "@iree/amdgpu/pm:bureau.local")
	assertField(t, raw, "ciphertext", original.Ciphertext)
	assertField(t, raw, "provisioned_by", "@bureau/operator:bureau.local")
	assertField(t, raw, "provisioned_at", "2026-02-09T18:30:00Z")
	assertField(t, raw, "signature", "base64signature==")
	assertField(t, raw, "expires_at", "2026-08-09T18:30:00Z")

	var decoded Credentials
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Version != original.Version {
		t.Errorf("Version: got %d, want %d", decoded.Version, original.Version)
	}
	if decoded.Principal != original.Principal {
		t.Errorf("Principal: got %q, want %q", decoded.Principal, original.Principal)
	}
	if decoded.Ciphertext != original.Ciphertext {
		t.Errorf("Ciphertext: got %q, want %q", decoded.Ciphertext, original.Ciphertext)
	}
}

func TestCredentialsOmitsEmptyExpiry(t *testing.T) {
	credentials := Credentials{
		Version:       1,
		Principal:     "@iree/amdgpu/pm:bureau.local",
		EncryptedFor:  []string{"@machine/workstation:bureau.local"},
		Keys:          []string{"OPENAI_API_KEY"},
		Ciphertext:    "encrypted",
		ProvisionedBy: "@bureau/operator:bureau.local",
		ProvisionedAt: "2026-02-09T18:30:00Z",
		Signature:     "sig",
		// ExpiresAt deliberately empty.
	}

	data, err := json.Marshal(credentials)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["expires_at"]; exists {
		t.Error("expires_at should be omitted when empty")
	}
}

func TestServiceRoundTrip(t *testing.T) {
	original := Service{
		Principal:    "@service/stt/whisper:bureau.local",
		Machine:      "@machine/cloud-gpu-1:bureau.local",
		Capabilities: []string{"streaming", "speaker-diarization"},
		Protocol:     "http",
		Description:  "Whisper Large V3 streaming STT",
		Metadata: map[string]any{
			"languages":     []any{"en", "es", "ja"},
			"model_version": "large-v3",
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
	assertField(t, raw, "principal", "@service/stt/whisper:bureau.local")
	assertField(t, raw, "machine", "@machine/cloud-gpu-1:bureau.local")
	assertField(t, raw, "protocol", "http")
	assertField(t, raw, "description", "Whisper Large V3 streaming STT")

	var decoded Service
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Principal != original.Principal {
		t.Errorf("Principal: got %q, want %q", decoded.Principal, original.Principal)
	}
	if decoded.Machine != original.Machine {
		t.Errorf("Machine: got %q, want %q", decoded.Machine, original.Machine)
	}
	if decoded.Protocol != original.Protocol {
		t.Errorf("Protocol: got %q, want %q", decoded.Protocol, original.Protocol)
	}
	if len(decoded.Capabilities) != 2 {
		t.Fatalf("Capabilities count = %d, want 2", len(decoded.Capabilities))
	}
	if decoded.Capabilities[0] != "streaming" || decoded.Capabilities[1] != "speaker-diarization" {
		t.Errorf("Capabilities: got %v, want [streaming speaker-diarization]", decoded.Capabilities)
	}
}

func TestServiceOmitsOptionalFields(t *testing.T) {
	service := Service{
		Principal: "@service/tts/piper:bureau.local",
		Machine:   "@machine/workstation:bureau.local",
		Protocol:  "http",
		// Capabilities, Description, Metadata deliberately empty.
	}

	data, err := json.Marshal(service)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	for _, field := range []string{"capabilities", "description", "metadata"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty", field)
		}
	}
}

func TestRoomServiceContentRoundTrip(t *testing.T) {
	original := RoomServiceContent{
		Principal: "@service/ticket/iree:bureau.local",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "principal", "@service/ticket/iree:bureau.local")

	var decoded RoomServiceContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Principal != original.Principal {
		t.Errorf("Principal: got %q, want %q", decoded.Principal, original.Principal)
	}
}

func TestAdminProtectedEvents(t *testing.T) {
	events := AdminProtectedEvents()

	expectedEvents := []string{
		"m.room.avatar",
		"m.room.canonical_alias",
		"m.room.encryption",
		"m.room.history_visibility",
		"m.room.join_rules",
		"m.room.name",
		"m.room.power_levels",
		"m.room.server_acl",
		"m.room.tombstone",
		"m.room.topic",
		"m.space.child",
	}

	for _, eventType := range expectedEvents {
		powerLevel, ok := events[eventType]
		if !ok {
			t.Errorf("%s missing from AdminProtectedEvents", eventType)
			continue
		}
		if powerLevel != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, powerLevel)
		}
	}

	if len(events) != len(expectedEvents) {
		t.Errorf("AdminProtectedEvents has %d entries, want %d", len(events), len(expectedEvents))
	}

	// Verify the function returns a fresh map each call (callers may modify it).
	first := AdminProtectedEvents()
	first["m.room.test"] = 42
	second := AdminProtectedEvents()
	if _, ok := second["m.room.test"]; ok {
		t.Error("AdminProtectedEvents returned a shared map instead of a fresh copy")
	}
}

func TestConfigRoomPowerLevels(t *testing.T) {
	adminUserID := "@bureau-admin:bureau.local"
	machineUserID := "@machine/workstation:bureau.local"
	levels := ConfigRoomPowerLevels(adminUserID, machineUserID)

	// Admin should have power level 100.
	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	adminLevel, ok := users[adminUserID]
	if !ok {
		t.Fatalf("admin %q not in users map", adminUserID)
	}
	if adminLevel != 100 {
		t.Errorf("admin power level = %v, want 100", adminLevel)
	}

	// Machine should have power level 50 (sufficient for invite during
	// room creation, but insufficient for config or credential writes).
	machineLevel, ok := users[machineUserID]
	if !ok {
		t.Fatalf("machine %q not in users map", machineUserID)
	}
	if machineLevel != 50 {
		t.Errorf("machine power level = %v, want 50", machineLevel)
	}

	// Default user power level should be 0 (other members are read-only).
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	// Invite should require PL 50 (machine can invite admin during creation).
	if levels["invite"] != 50 {
		t.Errorf("invite = %v, want 50", levels["invite"])
	}

	// Bureau config and credentials events require power level 100.
	events, ok := levels["events"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}
	if events[EventTypeMachineConfig] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypeMachineConfig, events[EventTypeMachineConfig])
	}
	if events[EventTypeCredentials] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypeCredentials, events[EventTypeCredentials])
	}

	// Layout events are writable by the daemon (power level 0) so it can
	// publish layout state without admin privileges.
	if events[EventTypeLayout] != 0 {
		t.Errorf("%s power level = %v, want 0", EventTypeLayout, events[EventTypeLayout])
	}

	// Messages are writable by anyone in the room (power level 0) so the
	// daemon can post command results and pipeline results.
	if events["m.room.message"] != 0 {
		t.Errorf("m.room.message power level = %v, want 0", events["m.room.message"])
	}

	// Default event power level should be 100 (admin-only room).
	if levels["events_default"] != 100 {
		t.Errorf("events_default = %v, want 100", levels["events_default"])
	}

	// Room metadata events from AdminProtectedEvents should all be PL 100.
	for _, eventType := range []string{
		"m.room.encryption", "m.room.server_acl",
		"m.room.tombstone", "m.space.child",
	} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, events[eventType])
		}
	}

	// Administrative actions require power level 100.
	for _, field := range []string{"state_default", "ban", "kick", "redact"} {
		if levels[field] != 100 {
			t.Errorf("%s = %v, want 100", field, levels[field])
		}
	}
}

func TestWorkspaceRoomPowerLevels(t *testing.T) {
	adminUserID := "@bureau-admin:bureau.local"
	machineUserID := "@machine/workstation:bureau.local"
	levels := WorkspaceRoomPowerLevels(adminUserID, machineUserID)

	// Admin should have power level 100.
	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if users[adminUserID] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID])
	}

	// Machine should have power level 50 (can invite principals into
	// workspace rooms, but cannot modify power levels or project config).
	if users[machineUserID] != 50 {
		t.Errorf("machine power level = %v, want 50", users[machineUserID])
	}

	// Default user power level should be 0.
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	// Workspace rooms are collaboration spaces: events_default should be 0
	// (agents can send messages freely), unlike config rooms where
	// events_default is 100 (admin-only).
	if levels["events_default"] != 0 {
		t.Errorf("events_default = %v, want 0", levels["events_default"])
	}

	// Event-specific power levels.
	events, ok := levels["events"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}

	// Admin-only events (PL 100).
	for _, eventType := range []string{EventTypeProject} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, events[eventType])
		}
	}

	// Admin-only power level management (PL 100). Continuwuity validates
	// all fields in m.room.power_levels against sender PL (not just changed
	// ones), so only admin can modify power levels.
	if events["m.room.power_levels"] != 100 {
		t.Errorf("m.room.power_levels power level = %v, want 100", events["m.room.power_levels"])
	}

	// Default-level events (PL 0): workspace state, worktree lifecycle, and
	// layout. Room membership is the authorization boundary — the room is
	// invite-only.
	for _, eventType := range []string{EventTypeWorkspace, EventTypeWorktree, EventTypeLayout} {
		if events[eventType] != 0 {
			t.Errorf("%s power level = %v, want 0", eventType, events[eventType])
		}
	}

	// Room metadata events from AdminProtectedEvents should all be PL 100.
	for _, eventType := range []string{
		"m.room.encryption", "m.room.server_acl",
		"m.room.tombstone", "m.space.child",
	} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, events[eventType])
		}
	}

	// state_default is 0: room membership is the authorization boundary.
	// New Bureau state event types work without updating power levels.
	if levels["state_default"] != 0 {
		t.Errorf("state_default = %v, want 0", levels["state_default"])
	}

	// Moderation actions require power level 100.
	for _, field := range []string{"ban", "kick", "redact"} {
		if levels[field] != 100 {
			t.Errorf("%s = %v, want 100", field, levels[field])
		}
	}

	// Invite should require PL 50 (machine can invite principals).
	if levels["invite"] != 50 {
		t.Errorf("invite = %v, want 50", levels["invite"])
	}
}

func TestWorkspaceRoomPowerLevelsSameUser(t *testing.T) {
	// When admin and machine are the same user, the users map should
	// have exactly one entry (no duplicate).
	adminUserID := "@bureau-admin:bureau.local"
	levels := WorkspaceRoomPowerLevels(adminUserID, adminUserID)

	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if len(users) != 1 {
		t.Errorf("expected 1 user entry for same user, got %d", len(users))
	}
	if users[adminUserID] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID])
	}
}

func TestWorkspaceRoomPowerLevelsEmptyMachine(t *testing.T) {
	// When machine user ID is empty, the users map should have only the admin.
	adminUserID := "@bureau-admin:bureau.local"
	levels := WorkspaceRoomPowerLevels(adminUserID, "")

	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if len(users) != 1 {
		t.Errorf("expected 1 user entry for empty machine, got %d", len(users))
	}
}

func TestLayoutContentRoundTrip(t *testing.T) {
	// A channel layout with two windows: agents (two observe panes) and
	// tools (a command pane and an observe pane). Exercises all pane modes
	// except ObserveMembers (tested separately).
	original := LayoutContent{
		Prefix: "C-a",
		Windows: []LayoutWindow{
			{
				Name: "agents",
				Panes: []LayoutPane{
					{Observe: "iree/amdgpu/pm", Split: "horizontal", Size: 50},
					{Observe: "iree/amdgpu/codegen", Size: 50},
				},
			},
			{
				Name: "tools",
				Panes: []LayoutPane{
					{Command: "beads-tui --project iree/amdgpu", Split: "horizontal", Size: 30},
					{Observe: "iree/amdgpu/ci-runner", Size: 70},
				},
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON field names match the wire format from OBSERVATION.md.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "prefix", "C-a")
	windows, ok := raw["windows"].([]any)
	if !ok {
		t.Fatal("windows field missing or wrong type")
	}
	if len(windows) != 2 {
		t.Fatalf("windows count = %d, want 2", len(windows))
	}

	agentsWindow := windows[0].(map[string]any)
	assertField(t, agentsWindow, "name", "agents")
	agentsPanes := agentsWindow["panes"].([]any)
	if len(agentsPanes) != 2 {
		t.Fatalf("agents panes count = %d, want 2", len(agentsPanes))
	}
	firstPane := agentsPanes[0].(map[string]any)
	assertField(t, firstPane, "observe", "iree/amdgpu/pm")
	assertField(t, firstPane, "split", "horizontal")
	assertField(t, firstPane, "size", float64(50))

	toolsWindow := windows[1].(map[string]any)
	assertField(t, toolsWindow, "name", "tools")
	toolsPanes := toolsWindow["panes"].([]any)
	firstToolPane := toolsPanes[0].(map[string]any)
	assertField(t, firstToolPane, "command", "beads-tui --project iree/amdgpu")
	assertField(t, firstToolPane, "size", float64(30))

	// Round-trip back to struct.
	var decoded LayoutContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Prefix != original.Prefix {
		t.Errorf("Prefix: got %q, want %q", decoded.Prefix, original.Prefix)
	}
	if len(decoded.Windows) != len(original.Windows) {
		t.Fatalf("windows count: got %d, want %d", len(decoded.Windows), len(original.Windows))
	}
	for windowIndex, window := range original.Windows {
		decodedWindow := decoded.Windows[windowIndex]
		if decodedWindow.Name != window.Name {
			t.Errorf("window[%d].Name: got %q, want %q", windowIndex, decodedWindow.Name, window.Name)
		}
		if len(decodedWindow.Panes) != len(window.Panes) {
			t.Fatalf("window[%d] panes count: got %d, want %d", windowIndex, len(decodedWindow.Panes), len(window.Panes))
		}
		for paneIndex, pane := range window.Panes {
			decodedPane := decodedWindow.Panes[paneIndex]
			if decodedPane.Observe != pane.Observe {
				t.Errorf("window[%d].pane[%d].Observe: got %q, want %q", windowIndex, paneIndex, decodedPane.Observe, pane.Observe)
			}
			if decodedPane.Command != pane.Command {
				t.Errorf("window[%d].pane[%d].Command: got %q, want %q", windowIndex, paneIndex, decodedPane.Command, pane.Command)
			}
			if decodedPane.Split != pane.Split {
				t.Errorf("window[%d].pane[%d].Split: got %q, want %q", windowIndex, paneIndex, decodedPane.Split, pane.Split)
			}
			if decodedPane.Size != pane.Size {
				t.Errorf("window[%d].pane[%d].Size: got %d, want %d", windowIndex, paneIndex, decodedPane.Size, pane.Size)
			}
		}
	}
}

func TestLayoutContentPrincipalLayout(t *testing.T) {
	// A principal layout uses "role" instead of "observe" or "command".
	// The launcher resolves roles to concrete commands.
	original := LayoutContent{
		Windows: []LayoutWindow{
			{
				Name: "main",
				Panes: []LayoutPane{
					{Role: "agent", Split: "horizontal", Size: 65},
					{Role: "shell", Size: 35},
				},
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

	// Prefix should be omitted when empty (uses Bureau default).
	if _, exists := raw["prefix"]; exists {
		t.Error("prefix should be omitted when empty")
	}

	windows := raw["windows"].([]any)
	mainWindow := windows[0].(map[string]any)
	panes := mainWindow["panes"].([]any)
	agentPane := panes[0].(map[string]any)
	assertField(t, agentPane, "role", "agent")
	assertField(t, agentPane, "size", float64(65))

	// Observe and command should not appear in principal layouts.
	if _, exists := agentPane["observe"]; exists {
		t.Error("observe should be omitted when empty")
	}
	if _, exists := agentPane["command"]; exists {
		t.Error("command should be omitted when empty")
	}

	var decoded LayoutContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Prefix != "" {
		t.Errorf("Prefix should be empty, got %q", decoded.Prefix)
	}
	if decoded.Windows[0].Panes[0].Role != "agent" {
		t.Errorf("Role: got %q, want %q", decoded.Windows[0].Panes[0].Role, "agent")
	}
}

func TestLayoutContentObserveMembers(t *testing.T) {
	// Dynamic pane creation from room membership. The daemon expands
	// ObserveMembers into concrete observe panes at runtime.
	original := LayoutContent{
		Windows: []LayoutWindow{
			{
				Name: "team",
				Panes: []LayoutPane{
					{
						ObserveMembers: &LayoutMemberFilter{Labels: map[string]string{"role": "agent"}},
						Split:          "horizontal",
					},
				},
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

	windows := raw["windows"].([]any)
	panes := windows[0].(map[string]any)["panes"].([]any)
	pane := panes[0].(map[string]any)

	observeMembers, ok := pane["observe_members"].(map[string]any)
	if !ok {
		t.Fatal("observe_members field missing or wrong type")
	}
	labels, ok := observeMembers["labels"].(map[string]any)
	if !ok {
		t.Fatal("observe_members.labels field missing or wrong type")
	}
	assertField(t, labels, "role", "agent")

	// Other pane mode fields should be absent.
	for _, field := range []string{"observe", "command", "role"} {
		if _, exists := pane[field]; exists {
			t.Errorf("%s should be omitted when ObserveMembers is set", field)
		}
	}

	var decoded LayoutContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	decodedPane := decoded.Windows[0].Panes[0]
	if decodedPane.ObserveMembers == nil {
		t.Fatal("ObserveMembers should not be nil after round-trip")
	}
	if decodedPane.ObserveMembers.Labels["role"] != "agent" {
		t.Errorf("ObserveMembers.Labels[role]: got %q, want %q", decodedPane.ObserveMembers.Labels["role"], "agent")
	}
}

func TestLayoutContentSourceMachineRoundTrip(t *testing.T) {
	// SourceMachine and SealedMetadata are set by the daemon before
	// publishing; verify they survive JSON serialization.
	original := LayoutContent{
		SourceMachine:  "@machine/workstation:bureau.local",
		SealedMetadata: "age-encrypted-blob-base64",
		Windows: []LayoutWindow{
			{
				Name: "main",
				Panes: []LayoutPane{
					{Role: "agent"},
				},
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
	assertField(t, raw, "source_machine", "@machine/workstation:bureau.local")
	assertField(t, raw, "sealed_metadata", "age-encrypted-blob-base64")

	var decoded LayoutContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.SourceMachine != original.SourceMachine {
		t.Errorf("SourceMachine: got %q, want %q", decoded.SourceMachine, original.SourceMachine)
	}
	if decoded.SealedMetadata != original.SealedMetadata {
		t.Errorf("SealedMetadata: got %q, want %q", decoded.SealedMetadata, original.SealedMetadata)
	}
}

func TestLayoutContentOmitsEmptySourceMachine(t *testing.T) {
	// When SourceMachine and SealedMetadata are empty, they should be
	// omitted from the JSON to keep the wire format clean.
	layout := LayoutContent{
		Windows: []LayoutWindow{
			{Name: "main", Panes: []LayoutPane{{Role: "agent"}}},
		},
	}

	data, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	for _, field := range []string{"source_machine", "sealed_metadata", "prefix"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty", field)
		}
	}
}

func TestLayoutContentOmitsEmptyFields(t *testing.T) {
	// Verify that zero-value optional fields are omitted from JSON.
	layout := LayoutContent{
		Windows: []LayoutWindow{
			{
				Name: "minimal",
				Panes: []LayoutPane{
					{Observe: "test/agent"},
				},
			},
		},
	}

	data, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Top-level prefix should be omitted.
	if _, exists := raw["prefix"]; exists {
		t.Error("prefix should be omitted when empty")
	}

	panes := raw["windows"].([]any)[0].(map[string]any)["panes"].([]any)
	pane := panes[0].(map[string]any)

	for _, field := range []string{"command", "role", "observe_members", "split", "size"} {
		if _, exists := pane[field]; exists {
			t.Errorf("%s should be omitted when zero-value", field)
		}
	}
}

func TestTemplateContentRoundTrip(t *testing.T) {
	original := TemplateContent{
		Description: "GPU-accelerated LLM agent with IREE runtime",
		Inherits:    "bureau/template:base",
		Command:     []string{"/usr/local/bin/claude", "--agent", "--no-tty"},
		Environment: "/nix/store/abc123-bureau-agent-env",
		EnvironmentVariables: map[string]string{
			"PATH":           "/workspace/bin:/usr/local/bin:/usr/bin:/bin",
			"HOME":           "/workspace",
			"BUREAU_SANDBOX": "1",
		},
		Filesystem: []TemplateMount{
			{Source: "${WORKTREE}", Dest: "/workspace", Mode: "rw"},
			{Type: "tmpfs", Dest: "/tmp", Options: "size=64M"},
			{Source: "/nix", Dest: "/nix", Mode: "ro", Optional: true},
		},
		Namespaces: &TemplateNamespaces{
			PID: true,
			Net: true,
			IPC: true,
			UTS: true,
		},
		Resources: &TemplateResources{
			CPUShares:     1024,
			MemoryLimitMB: 8192,
			PidsLimit:     512,
		},
		Security: &TemplateSecurity{
			NewSession:    true,
			DieWithParent: true,
			NoNewPrivs:    true,
		},
		CreateDirs:          []string{"/tmp", "/var/tmp", "/run/bureau"},
		Roles:               map[string][]string{"agent": {"/usr/local/bin/claude", "--agent"}, "shell": {"/bin/bash"}},
		RequiredCredentials: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"},
		DefaultPayload: map[string]any{
			"model":      "claude-sonnet-4-5-20250929",
			"max_tokens": float64(4096),
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON field names match the wire format.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "description", "GPU-accelerated LLM agent with IREE runtime")
	assertField(t, raw, "inherits", "bureau/template:base")
	assertField(t, raw, "environment", "/nix/store/abc123-bureau-agent-env")

	// Verify command is an array.
	command, ok := raw["command"].([]any)
	if !ok {
		t.Fatal("command field missing or wrong type")
	}
	if len(command) != 3 || command[0] != "/usr/local/bin/claude" {
		t.Errorf("command = %v, want [/usr/local/bin/claude --agent --no-tty]", command)
	}

	// Verify filesystem is an array with correct structure.
	filesystem, ok := raw["filesystem"].([]any)
	if !ok {
		t.Fatal("filesystem field missing or wrong type")
	}
	if len(filesystem) != 3 {
		t.Fatalf("filesystem count = %d, want 3", len(filesystem))
	}
	firstMount := filesystem[0].(map[string]any)
	assertField(t, firstMount, "source", "${WORKTREE}")
	assertField(t, firstMount, "dest", "/workspace")
	assertField(t, firstMount, "mode", "rw")

	// Verify tmpfs mount has type but no source.
	tmpfsMount := filesystem[1].(map[string]any)
	assertField(t, tmpfsMount, "type", "tmpfs")
	assertField(t, tmpfsMount, "dest", "/tmp")
	if _, exists := tmpfsMount["source"]; exists {
		t.Error("tmpfs mount should not have source")
	}

	// Verify optional mount.
	nixMount := filesystem[2].(map[string]any)
	assertField(t, nixMount, "optional", true)

	// Verify namespaces.
	namespaces, ok := raw["namespaces"].(map[string]any)
	if !ok {
		t.Fatal("namespaces field missing or wrong type")
	}
	assertField(t, namespaces, "pid", true)
	assertField(t, namespaces, "net", true)

	// Verify resources.
	resources, ok := raw["resources"].(map[string]any)
	if !ok {
		t.Fatal("resources field missing or wrong type")
	}
	assertField(t, resources, "cpu_shares", float64(1024))
	assertField(t, resources, "memory_limit_mb", float64(8192))

	// Verify security.
	security, ok := raw["security"].(map[string]any)
	if !ok {
		t.Fatal("security field missing or wrong type")
	}
	assertField(t, security, "new_session", true)
	assertField(t, security, "no_new_privs", true)

	// Verify roles.
	roles, ok := raw["roles"].(map[string]any)
	if !ok {
		t.Fatal("roles field missing or wrong type")
	}
	agentRole, ok := roles["agent"].([]any)
	if !ok {
		t.Fatal("roles.agent missing or wrong type")
	}
	if len(agentRole) != 2 || agentRole[0] != "/usr/local/bin/claude" {
		t.Errorf("roles.agent = %v, want [/usr/local/bin/claude --agent]", agentRole)
	}

	// Verify required_credentials.
	requiredCredentials, ok := raw["required_credentials"].([]any)
	if !ok {
		t.Fatal("required_credentials field missing or wrong type")
	}
	if len(requiredCredentials) != 2 {
		t.Fatalf("required_credentials count = %d, want 2", len(requiredCredentials))
	}

	// Verify default_payload.
	defaultPayload, ok := raw["default_payload"].(map[string]any)
	if !ok {
		t.Fatal("default_payload field missing or wrong type")
	}
	assertField(t, defaultPayload, "model", "claude-sonnet-4-5-20250929")
	assertField(t, defaultPayload, "max_tokens", float64(4096))

	// Round-trip back to struct.
	var decoded TemplateContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Description != original.Description {
		t.Errorf("Description: got %q, want %q", decoded.Description, original.Description)
	}
	if decoded.Inherits != original.Inherits {
		t.Errorf("Inherits: got %q, want %q", decoded.Inherits, original.Inherits)
	}
	if decoded.Environment != original.Environment {
		t.Errorf("Environment: got %q, want %q", decoded.Environment, original.Environment)
	}
	if len(decoded.Command) != 3 {
		t.Fatalf("Command count = %d, want 3", len(decoded.Command))
	}
	if decoded.Command[0] != "/usr/local/bin/claude" {
		t.Errorf("Command[0]: got %q, want %q", decoded.Command[0], "/usr/local/bin/claude")
	}
	if len(decoded.Filesystem) != 3 {
		t.Fatalf("Filesystem count = %d, want 3", len(decoded.Filesystem))
	}
	if decoded.Filesystem[0].Source != "${WORKTREE}" || decoded.Filesystem[0].Dest != "/workspace" {
		t.Errorf("Filesystem[0]: got source=%q dest=%q, want source=${WORKTREE} dest=/workspace",
			decoded.Filesystem[0].Source, decoded.Filesystem[0].Dest)
	}
	if decoded.Filesystem[2].Optional != true {
		t.Error("Filesystem[2].Optional should be true")
	}
	if decoded.Namespaces == nil || !decoded.Namespaces.PID || !decoded.Namespaces.Net {
		t.Error("Namespaces should have PID and Net set")
	}
	if decoded.Resources == nil || decoded.Resources.CPUShares != 1024 {
		t.Error("Resources.CPUShares should be 1024")
	}
	if decoded.Security == nil || !decoded.Security.NoNewPrivs {
		t.Error("Security.NoNewPrivs should be true")
	}
	if len(decoded.Roles) != 2 {
		t.Fatalf("Roles count = %d, want 2", len(decoded.Roles))
	}
	if len(decoded.Roles["agent"]) != 2 || decoded.Roles["agent"][0] != "/usr/local/bin/claude" {
		t.Errorf("Roles[agent] = %v, want [/usr/local/bin/claude --agent]", decoded.Roles["agent"])
	}
}

func TestTemplateContentOmitsEmptyFields(t *testing.T) {
	// A minimal template with only required structure should omit all
	// optional fields from the JSON wire format.
	template := TemplateContent{
		Command: []string{"/bin/bash"},
	}

	data, err := json.Marshal(template)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	omittedFields := []string{
		"description", "inherits", "environment", "environment_variables",
		"filesystem", "namespaces", "resources", "security",
		"create_dirs", "roles", "required_credentials", "default_payload",
		"health_check",
	}
	for _, field := range omittedFields {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty/nil", field)
		}
	}

	// Command should be present.
	if _, exists := raw["command"]; !exists {
		t.Error("command should be present")
	}
}

func TestHealthCheckOnTemplateContent(t *testing.T) {
	original := TemplateContent{
		Command:     []string{"/usr/local/bin/gmail-watcher"},
		Environment: "/nix/store/abc123-gmail-watcher-env",
		HealthCheck: &HealthCheck{
			Endpoint:           "/health",
			IntervalSeconds:    30,
			TimeoutSeconds:     5,
			FailureThreshold:   3,
			GracePeriodSeconds: 60,
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

	healthCheck, ok := raw["health_check"].(map[string]any)
	if !ok {
		t.Fatal("health_check field missing or wrong type")
	}
	assertField(t, healthCheck, "endpoint", "/health")
	assertField(t, healthCheck, "interval_seconds", float64(30))
	assertField(t, healthCheck, "timeout_seconds", float64(5))
	assertField(t, healthCheck, "failure_threshold", float64(3))
	assertField(t, healthCheck, "grace_period_seconds", float64(60))

	var decoded TemplateContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.HealthCheck == nil {
		t.Fatal("HealthCheck should not be nil after round-trip")
	}
	if decoded.HealthCheck.Endpoint != "/health" {
		t.Errorf("Endpoint: got %q, want %q", decoded.HealthCheck.Endpoint, "/health")
	}
	if decoded.HealthCheck.IntervalSeconds != 30 {
		t.Errorf("IntervalSeconds: got %d, want 30", decoded.HealthCheck.IntervalSeconds)
	}
	if decoded.HealthCheck.TimeoutSeconds != 5 {
		t.Errorf("TimeoutSeconds: got %d, want 5", decoded.HealthCheck.TimeoutSeconds)
	}
	if decoded.HealthCheck.FailureThreshold != 3 {
		t.Errorf("FailureThreshold: got %d, want 3", decoded.HealthCheck.FailureThreshold)
	}
	if decoded.HealthCheck.GracePeriodSeconds != 60 {
		t.Errorf("GracePeriodSeconds: got %d, want 60", decoded.HealthCheck.GracePeriodSeconds)
	}
}

func TestHealthCheckOmitsZeroOptionalFields(t *testing.T) {
	// A HealthCheck with only required fields should omit the optional
	// fields (timeout, failure threshold, grace period) from the wire
	// format — they default at runtime.
	healthCheck := HealthCheck{
		Endpoint:        "/healthz",
		IntervalSeconds: 15,
	}

	data, err := json.Marshal(healthCheck)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "endpoint", "/healthz")
	assertField(t, raw, "interval_seconds", float64(15))

	for _, field := range []string{"timeout_seconds", "failure_threshold", "grace_period_seconds"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when zero (runtime default)", field)
		}
	}
}

func TestHealthCheckOmittedWhenNilOnTemplate(t *testing.T) {
	// Templates without health monitoring should not include health_check.
	template := TemplateContent{
		Command: []string{"/usr/local/bin/claude", "--agent"},
	}

	data, err := json.Marshal(template)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["health_check"]; exists {
		t.Error("health_check should be omitted when nil")
	}
}

func TestTemplateMountOmitsEmptyFields(t *testing.T) {
	// A bind mount with only dest and mode should omit type, options,
	// source (if empty), and optional.
	mount := TemplateMount{
		Dest: "/workspace",
		Mode: "rw",
	}

	data, err := json.Marshal(mount)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"source", "type", "options", "optional"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty/zero", field)
		}
	}
	assertField(t, raw, "dest", "/workspace")
	assertField(t, raw, "mode", "rw")
}

func TestPrincipalAssignmentOverrides(t *testing.T) {
	// A PrincipalAssignment with all override fields set should
	// round-trip correctly through JSON.
	original := PrincipalAssignment{
		Localpart:           "iree/amdgpu/pm",
		Template:            "iree/template:amdgpu-developer",
		AutoStart:           true,
		CommandOverride:     []string{"/usr/local/bin/custom-agent", "--mode=gpu"},
		EnvironmentOverride: "/nix/store/xyz789-custom-env",
		ExtraEnvironmentVariables: map[string]string{
			"MODEL_NAME": "claude-opus-4-6",
			"BATCH_SIZE": "32",
		},
		Payload: map[string]any{
			"project":    "iree/amdgpu",
			"max_tokens": float64(8192),
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

	// Verify override wire format field names.
	commandOverride, ok := raw["command_override"].([]any)
	if !ok {
		t.Fatal("command_override field missing or wrong type")
	}
	if len(commandOverride) != 2 || commandOverride[0] != "/usr/local/bin/custom-agent" {
		t.Errorf("command_override = %v, want [/usr/local/bin/custom-agent --mode=gpu]", commandOverride)
	}

	assertField(t, raw, "environment_override", "/nix/store/xyz789-custom-env")

	extraEnvVars, ok := raw["extra_environment_variables"].(map[string]any)
	if !ok {
		t.Fatal("extra_environment_variables field missing or wrong type")
	}
	assertField(t, extraEnvVars, "MODEL_NAME", "claude-opus-4-6")
	assertField(t, extraEnvVars, "BATCH_SIZE", "32")

	payload, ok := raw["payload"].(map[string]any)
	if !ok {
		t.Fatal("payload field missing or wrong type")
	}
	assertField(t, payload, "project", "iree/amdgpu")
	assertField(t, payload, "max_tokens", float64(8192))

	// Round-trip.
	var decoded PrincipalAssignment
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Template != "iree/template:amdgpu-developer" {
		t.Errorf("Template: got %q, want %q", decoded.Template, "iree/template:amdgpu-developer")
	}
	if len(decoded.CommandOverride) != 2 || decoded.CommandOverride[0] != "/usr/local/bin/custom-agent" {
		t.Errorf("CommandOverride: got %v, want [/usr/local/bin/custom-agent --mode=gpu]", decoded.CommandOverride)
	}
	if decoded.EnvironmentOverride != "/nix/store/xyz789-custom-env" {
		t.Errorf("EnvironmentOverride: got %q, want %q", decoded.EnvironmentOverride, "/nix/store/xyz789-custom-env")
	}
	if decoded.ExtraEnvironmentVariables["MODEL_NAME"] != "claude-opus-4-6" {
		t.Errorf("ExtraEnvironmentVariables[MODEL_NAME]: got %q, want %q",
			decoded.ExtraEnvironmentVariables["MODEL_NAME"], "claude-opus-4-6")
	}
}

func TestPrincipalAssignmentOmitsEmptyOverrides(t *testing.T) {
	// A PrincipalAssignment without override fields should not include
	// them in the wire format (backward compatibility).
	assignment := PrincipalAssignment{
		Localpart: "service/stt/whisper",
		Template:  "bureau/template:whisper-stt",
		AutoStart: true,
	}

	data, err := json.Marshal(assignment)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	overrideFields := []string{
		"command_override", "environment_override",
		"extra_environment_variables", "payload",
	}
	for _, field := range overrideFields {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty/nil", field)
		}
	}

	// Existing fields should still be present.
	assertField(t, raw, "localpart", "service/stt/whisper")
	assertField(t, raw, "template", "bureau/template:whisper-stt")
	assertField(t, raw, "auto_start", true)
}

func TestProjectConfigGitBackedRoundTrip(t *testing.T) {
	original := ProjectConfig{
		Repository:    "https://github.com/iree-org/iree.git",
		WorkspacePath: "iree",
		DefaultBranch: "main",
		Worktrees: map[string]WorktreeConfig{
			"amdgpu/inference": {Branch: "feature/amdgpu-inference", Description: "AMDGPU inference pipeline"},
			"amdgpu/pm":        {Branch: "feature/amdgpu-pm"},
			"remoting":         {Branch: "main"},
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
	assertField(t, raw, "repository", "https://github.com/iree-org/iree.git")
	assertField(t, raw, "workspace_path", "iree")
	assertField(t, raw, "default_branch", "main")

	worktrees, ok := raw["worktrees"].(map[string]any)
	if !ok {
		t.Fatal("worktrees field missing or wrong type")
	}
	if len(worktrees) != 3 {
		t.Fatalf("worktrees count = %d, want 3", len(worktrees))
	}
	inference, ok := worktrees["amdgpu/inference"].(map[string]any)
	if !ok {
		t.Fatal("worktrees[amdgpu/inference] missing or wrong type")
	}
	assertField(t, inference, "branch", "feature/amdgpu-inference")
	assertField(t, inference, "description", "AMDGPU inference pipeline")

	// Directories should be omitted for git-backed projects.
	if _, exists := raw["directories"]; exists {
		t.Error("directories should be omitted for git-backed projects")
	}

	var decoded ProjectConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Repository != original.Repository {
		t.Errorf("Repository: got %q, want %q", decoded.Repository, original.Repository)
	}
	if decoded.WorkspacePath != original.WorkspacePath {
		t.Errorf("WorkspacePath: got %q, want %q", decoded.WorkspacePath, original.WorkspacePath)
	}
	if decoded.DefaultBranch != original.DefaultBranch {
		t.Errorf("DefaultBranch: got %q, want %q", decoded.DefaultBranch, original.DefaultBranch)
	}
	if len(decoded.Worktrees) != 3 {
		t.Fatalf("Worktrees count = %d, want 3", len(decoded.Worktrees))
	}
	inferenceConfig := decoded.Worktrees["amdgpu/inference"]
	if inferenceConfig.Branch != "feature/amdgpu-inference" {
		t.Errorf("Worktrees[amdgpu/inference].Branch: got %q, want %q",
			inferenceConfig.Branch, "feature/amdgpu-inference")
	}
	if inferenceConfig.Description != "AMDGPU inference pipeline" {
		t.Errorf("Worktrees[amdgpu/inference].Description: got %q, want %q",
			inferenceConfig.Description, "AMDGPU inference pipeline")
	}
}

func TestProjectConfigNonGitRoundTrip(t *testing.T) {
	original := ProjectConfig{
		WorkspacePath: "lore",
		Directories: map[string]DirectoryConfig{
			"novel4": {Description: "Fourth novel workspace"},
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
	assertField(t, raw, "workspace_path", "lore")

	// Git-specific fields should be omitted.
	for _, field := range []string{"repository", "default_branch", "worktrees"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted for non-git projects", field)
		}
	}

	directories, ok := raw["directories"].(map[string]any)
	if !ok {
		t.Fatal("directories field missing or wrong type")
	}
	novel4, ok := directories["novel4"].(map[string]any)
	if !ok {
		t.Fatal("directories[novel4] missing or wrong type")
	}
	assertField(t, novel4, "description", "Fourth novel workspace")

	var decoded ProjectConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.WorkspacePath != "lore" {
		t.Errorf("WorkspacePath: got %q, want %q", decoded.WorkspacePath, "lore")
	}
	if len(decoded.Directories) != 1 {
		t.Fatalf("Directories count = %d, want 1", len(decoded.Directories))
	}
	if decoded.Directories["novel4"].Description != "Fourth novel workspace" {
		t.Errorf("Directories[novel4].Description: got %q, want %q",
			decoded.Directories["novel4"].Description, "Fourth novel workspace")
	}
}

func TestProjectConfigOmitsEmptyFields(t *testing.T) {
	// Minimal ProjectConfig with only required field.
	config := ProjectConfig{
		WorkspacePath: "scratch",
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"repository", "default_branch", "worktrees", "directories"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty", field)
		}
	}
	assertField(t, raw, "workspace_path", "scratch")
}

func TestWorktreeConfigOmitsEmptyDescription(t *testing.T) {
	config := WorktreeConfig{Branch: "main"}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "branch", "main")
	if _, exists := raw["description"]; exists {
		t.Error("description should be omitted when empty")
	}
}

func TestStartConditionOnPrincipalAssignment(t *testing.T) {
	original := PrincipalAssignment{
		Localpart: "iree/amdgpu/pm",
		Template:  "iree/template:llm-agent",
		AutoStart: true,
		StartCondition: &StartCondition{
			EventType:    "m.bureau.workspace",
			StateKey:     "",
			RoomAlias:    "#iree/amdgpu/inference:bureau.local",
			ContentMatch: ContentMatch{"status": Eq("active")},
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

	startCondition, ok := raw["start_condition"].(map[string]any)
	if !ok {
		t.Fatal("start_condition field missing or wrong type")
	}
	assertField(t, startCondition, "event_type", "m.bureau.workspace")
	assertField(t, startCondition, "state_key", "")
	assertField(t, startCondition, "room_alias", "#iree/amdgpu/inference:bureau.local")

	contentMatch, ok := startCondition["content_match"].(map[string]any)
	if !ok {
		t.Fatal("content_match field missing or wrong type")
	}
	assertField(t, contentMatch, "status", "active")

	var decoded PrincipalAssignment
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.StartCondition == nil {
		t.Fatal("StartCondition should not be nil after round-trip")
	}
	if decoded.StartCondition.EventType != "m.bureau.workspace" {
		t.Errorf("StartCondition.EventType: got %q, want %q",
			decoded.StartCondition.EventType, "m.bureau.workspace")
	}
	if decoded.StartCondition.StateKey != "" {
		t.Errorf("StartCondition.StateKey: got %q, want empty", decoded.StartCondition.StateKey)
	}
	if decoded.StartCondition.RoomAlias != "#iree/amdgpu/inference:bureau.local" {
		t.Errorf("StartCondition.RoomAlias: got %q, want %q",
			decoded.StartCondition.RoomAlias, "#iree/amdgpu/inference:bureau.local")
	}
	statusMatch, ok := decoded.StartCondition.ContentMatch["status"]
	if !ok {
		t.Fatal("StartCondition.ContentMatch missing \"status\" key")
	}
	if statusMatch.StringValue() != "active" {
		t.Errorf("StartCondition.ContentMatch[status]: got %q, want %q",
			statusMatch.StringValue(), "active")
	}
}

func TestStartConditionOmittedWhenNil(t *testing.T) {
	assignment := PrincipalAssignment{
		Localpart: "service/stt/whisper",
		Template:  "bureau/template:whisper-stt",
		AutoStart: true,
	}

	data, err := json.Marshal(assignment)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["start_condition"]; exists {
		t.Error("start_condition should be omitted when nil")
	}
}

func TestStartConditionOmitsEmptyRoomAlias(t *testing.T) {
	// When RoomAlias is empty (check principal's own config room),
	// it should be omitted from the wire format.
	condition := StartCondition{
		EventType: "m.bureau.workspace",
		StateKey:  "",
	}

	data, err := json.Marshal(condition)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "event_type", "m.bureau.workspace")
	assertField(t, raw, "state_key", "")
	if _, exists := raw["room_alias"]; exists {
		t.Error("room_alias should be omitted when empty")
	}
	if _, exists := raw["content_match"]; exists {
		t.Error("content_match should be omitted when nil")
	}
}

func TestStartConditionContentMatchRoundTrip(t *testing.T) {
	condition := StartCondition{
		EventType:    "m.bureau.workspace",
		StateKey:     "",
		RoomAlias:    "#iree/amdgpu/inference:bureau.local",
		ContentMatch: ContentMatch{"status": Eq("active")},
	}

	data, err := json.Marshal(condition)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded StartCondition
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.ContentMatch) != 1 {
		t.Fatalf("ContentMatch length = %d, want 1", len(decoded.ContentMatch))
	}
	statusMatch, ok := decoded.ContentMatch["status"]
	if !ok {
		t.Fatal("ContentMatch missing \"status\" key")
	}
	if statusMatch.StringValue() != "active" {
		t.Errorf("ContentMatch[status] = %q, want %q", statusMatch.StringValue(), "active")
	}
}

func TestStartConditionMultipleContentMatch(t *testing.T) {
	// ContentMatch with multiple keys — all must match.
	condition := StartCondition{
		EventType: "m.bureau.service",
		StateKey:  "stt/whisper",
		ContentMatch: ContentMatch{
			"status": Eq("healthy"),
			"stage":  Eq("canary"),
		},
	}

	data, err := json.Marshal(condition)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	contentMatch, ok := raw["content_match"].(map[string]any)
	if !ok {
		t.Fatal("content_match field missing or wrong type")
	}
	assertField(t, contentMatch, "status", "healthy")
	assertField(t, contentMatch, "stage", "canary")
}

func TestWorkspaceStateRoundTrip(t *testing.T) {
	original := WorkspaceState{
		Status:    "active",
		Project:   "iree",
		Machine:   "workstation",
		UpdatedAt: "2026-02-10T12:00:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "status", "active")
	assertField(t, raw, "project", "iree")
	assertField(t, raw, "machine", "workstation")
	assertField(t, raw, "updated_at", "2026-02-10T12:00:00Z")
	if _, exists := raw["archive_path"]; exists {
		t.Error("archive_path should be omitted when empty")
	}

	var decoded WorkspaceState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestWorkspaceStateArchived(t *testing.T) {
	original := WorkspaceState{
		Status:      "archived",
		Project:     "iree",
		Machine:     "workstation",
		UpdatedAt:   "2026-02-10T14:30:00Z",
		ArchivePath: "/workspace/.archive/iree-20260210T143000",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "status", "archived")
	assertField(t, raw, "archive_path", "/workspace/.archive/iree-20260210T143000")

	var decoded WorkspaceState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestPipelineContentRoundTrip(t *testing.T) {
	original := PipelineContent{
		Description: "Clone repository and prepare project workspace",
		Variables: map[string]PipelineVariable{
			"REPOSITORY": {
				Description: "Git clone URL",
				Required:    true,
			},
			"BRANCH": {
				Description: "Git branch to check out",
				Default:     "main",
			},
			"PROJECT": {
				Description: "Project name",
				Required:    true,
			},
		},
		Steps: []PipelineStep{
			{
				Name: "create-project-directory",
				Run:  "mkdir -p /workspace/${PROJECT}",
			},
			{
				Name:    "clone-repository",
				Run:     "git clone --bare ${REPOSITORY} /workspace/${PROJECT}/.bare",
				Check:   "test -d /workspace/${PROJECT}/.bare/objects",
				When:    "test -n '${REPOSITORY}'",
				Timeout: "10m",
			},
			{
				Name:     "run-project-init",
				Run:      "/workspace/${PROJECT}/.bureau/pipeline/init",
				Optional: true,
				Env:      map[string]string{"INIT_MODE": "full"},
			},
			{
				Name: "publish-active",
				Publish: &PipelinePublish{
					EventType: "m.bureau.workspace",
					Room:      "${WORKSPACE_ROOM_ID}",
					Content: map[string]any{
						"status":     "active",
						"project":    "${PROJECT}",
						"machine":    "${MACHINE}",
						"updated_at": "2026-02-10T12:00:00Z",
					},
				},
			},
		},
		Log: &PipelineLog{
			Room: "${WORKSPACE_ROOM_ID}",
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON field names match the wire format.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "description", "Clone repository and prepare project workspace")

	// Verify variables.
	variables, ok := raw["variables"].(map[string]any)
	if !ok {
		t.Fatal("variables field missing or wrong type")
	}
	if len(variables) != 3 {
		t.Fatalf("variables count = %d, want 3", len(variables))
	}
	repoVar, ok := variables["REPOSITORY"].(map[string]any)
	if !ok {
		t.Fatal("variables[REPOSITORY] missing or wrong type")
	}
	assertField(t, repoVar, "description", "Git clone URL")
	assertField(t, repoVar, "required", true)

	branchVar, ok := variables["BRANCH"].(map[string]any)
	if !ok {
		t.Fatal("variables[BRANCH] missing or wrong type")
	}
	assertField(t, branchVar, "default", "main")

	// Verify steps array.
	steps, ok := raw["steps"].([]any)
	if !ok {
		t.Fatal("steps field missing or wrong type")
	}
	if len(steps) != 4 {
		t.Fatalf("steps count = %d, want 4", len(steps))
	}

	// First step: simple run.
	firstStep := steps[0].(map[string]any)
	assertField(t, firstStep, "name", "create-project-directory")
	assertField(t, firstStep, "run", "mkdir -p /workspace/${PROJECT}")

	// Second step: run with check, when, timeout.
	secondStep := steps[1].(map[string]any)
	assertField(t, secondStep, "name", "clone-repository")
	assertField(t, secondStep, "check", "test -d /workspace/${PROJECT}/.bare/objects")
	assertField(t, secondStep, "when", "test -n '${REPOSITORY}'")
	assertField(t, secondStep, "timeout", "10m")

	// Third step: optional with env.
	thirdStep := steps[2].(map[string]any)
	assertField(t, thirdStep, "optional", true)
	stepEnv, ok := thirdStep["env"].(map[string]any)
	if !ok {
		t.Fatal("step env field missing or wrong type")
	}
	assertField(t, stepEnv, "INIT_MODE", "full")

	// Fourth step: publish.
	fourthStep := steps[3].(map[string]any)
	assertField(t, fourthStep, "name", "publish-active")
	publish, ok := fourthStep["publish"].(map[string]any)
	if !ok {
		t.Fatal("publish field missing or wrong type")
	}
	assertField(t, publish, "event_type", "m.bureau.workspace")
	assertField(t, publish, "room", "${WORKSPACE_ROOM_ID}")
	publishContent, ok := publish["content"].(map[string]any)
	if !ok {
		t.Fatal("publish content field missing or wrong type")
	}
	assertField(t, publishContent, "status", "active")
	assertField(t, publishContent, "project", "${PROJECT}")
	assertField(t, publishContent, "machine", "${MACHINE}")

	// Verify log.
	logField, ok := raw["log"].(map[string]any)
	if !ok {
		t.Fatal("log field missing or wrong type")
	}
	assertField(t, logField, "room", "${WORKSPACE_ROOM_ID}")

	// Round-trip back to struct.
	var decoded PipelineContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Description != original.Description {
		t.Errorf("Description: got %q, want %q", decoded.Description, original.Description)
	}
	if len(decoded.Variables) != 3 {
		t.Fatalf("Variables count = %d, want 3", len(decoded.Variables))
	}
	if !decoded.Variables["REPOSITORY"].Required {
		t.Error("Variables[REPOSITORY].Required should be true")
	}
	if decoded.Variables["BRANCH"].Default != "main" {
		t.Errorf("Variables[BRANCH].Default: got %q, want %q", decoded.Variables["BRANCH"].Default, "main")
	}
	if len(decoded.Steps) != 4 {
		t.Fatalf("Steps count = %d, want 4", len(decoded.Steps))
	}
	if decoded.Steps[0].Run != "mkdir -p /workspace/${PROJECT}" {
		t.Errorf("Steps[0].Run: got %q, want %q", decoded.Steps[0].Run, "mkdir -p /workspace/${PROJECT}")
	}
	if decoded.Steps[1].Check != "test -d /workspace/${PROJECT}/.bare/objects" {
		t.Errorf("Steps[1].Check: got %q, want %q", decoded.Steps[1].Check, "test -d /workspace/${PROJECT}/.bare/objects")
	}
	if decoded.Steps[2].Optional != true {
		t.Error("Steps[2].Optional should be true")
	}
	if decoded.Steps[3].Publish == nil {
		t.Fatal("Steps[3].Publish should not be nil after round-trip")
	}
	if decoded.Steps[3].Publish.EventType != "m.bureau.workspace" {
		t.Errorf("Steps[3].Publish.EventType: got %q, want %q",
			decoded.Steps[3].Publish.EventType, "m.bureau.workspace")
	}
	if decoded.Log == nil {
		t.Fatal("Log should not be nil after round-trip")
	}
	if decoded.Log.Room != "${WORKSPACE_ROOM_ID}" {
		t.Errorf("Log.Room: got %q, want %q", decoded.Log.Room, "${WORKSPACE_ROOM_ID}")
	}
}

func TestPipelineContentMinimal(t *testing.T) {
	// Pipeline with only the required field (steps) — all optional
	// fields should be omitted from the wire format.
	pipeline := PipelineContent{
		Steps: []PipelineStep{
			{Name: "hello", Run: "echo hello"},
		},
	}

	data, err := json.Marshal(pipeline)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"description", "variables", "log"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty/nil", field)
		}
	}

	// Steps should be present.
	steps, ok := raw["steps"].([]any)
	if !ok {
		t.Fatal("steps field missing or wrong type")
	}
	if len(steps) != 1 {
		t.Fatalf("steps count = %d, want 1", len(steps))
	}
}

func TestPipelineStepRunOnly(t *testing.T) {
	// A step with all run-related fields set; publish and interactive
	// should be omitted.
	step := PipelineStep{
		Name:     "full-run-step",
		Run:      "some-command --flag",
		Check:    "test -f /expected/file",
		When:     "test -n '${CONDITION}'",
		Optional: true,
		Timeout:  "30s",
		Env:      map[string]string{"KEY": "value"},
	}

	data, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Run-related fields should be present.
	assertField(t, raw, "name", "full-run-step")
	assertField(t, raw, "run", "some-command --flag")
	assertField(t, raw, "check", "test -f /expected/file")
	assertField(t, raw, "when", "test -n '${CONDITION}'")
	assertField(t, raw, "optional", true)
	assertField(t, raw, "timeout", "30s")

	env, ok := raw["env"].(map[string]any)
	if !ok {
		t.Fatal("env field missing or wrong type")
	}
	assertField(t, env, "KEY", "value")

	// Publish, interactive, and grace_period should be absent.
	for _, field := range []string{"publish", "interactive", "grace_period"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted on a run-only step without it set", field)
		}
	}
}

func TestPipelineStepPublishOnly(t *testing.T) {
	// A step with only publish set; run-related fields should be omitted.
	step := PipelineStep{
		Name: "publish-state",
		Publish: &PipelinePublish{
			EventType: "m.bureau.workspace",
			Room:      "!abc123:bureau.local",
			StateKey:  "custom-key",
			Content: map[string]any{
				"status": "active",
			},
		},
	}

	data, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Publish should be present with all fields.
	publish, ok := raw["publish"].(map[string]any)
	if !ok {
		t.Fatal("publish field missing or wrong type")
	}
	assertField(t, publish, "event_type", "m.bureau.workspace")
	assertField(t, publish, "room", "!abc123:bureau.local")
	assertField(t, publish, "state_key", "custom-key")
	content, ok := publish["content"].(map[string]any)
	if !ok {
		t.Fatal("publish content field missing or wrong type")
	}
	assertField(t, content, "status", "active")

	// Run-related fields should be absent.
	for _, field := range []string{"run", "check", "when", "optional", "timeout", "grace_period", "env", "interactive"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted on a publish-only step", field)
		}
	}
}

func TestPipelineStepOmitsEmptyFields(t *testing.T) {
	// Minimal step (name + run only) — all optional fields should be
	// omitted from the wire format.
	step := PipelineStep{
		Name: "simple",
		Run:  "echo done",
	}

	data, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "name", "simple")
	assertField(t, raw, "run", "echo done")

	for _, field := range []string{"check", "when", "optional", "publish", "timeout", "grace_period", "env", "interactive"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when zero-value", field)
		}
	}
}

func TestPipelineStepInteractive(t *testing.T) {
	step := PipelineStep{
		Name:        "guided-setup",
		Run:         "claude --prompt 'Set up the workspace'",
		Interactive: true,
		Timeout:     "30m",
	}

	data, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "interactive", true)
	assertField(t, raw, "timeout", "30m")

	var decoded PipelineStep
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !decoded.Interactive {
		t.Error("Interactive should be true after round-trip")
	}
}

func TestPipelineStepGracePeriod(t *testing.T) {
	step := PipelineStep{
		Name:        "db-migration",
		Run:         "python manage.py migrate",
		Timeout:     "10m",
		GracePeriod: "30s",
	}

	data, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "grace_period", "30s")
	assertField(t, raw, "timeout", "10m")

	var decoded PipelineStep
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.GracePeriod != "30s" {
		t.Errorf("GracePeriod = %q after round-trip, want %q", decoded.GracePeriod, "30s")
	}
}

func TestPipelinePublishOmitsEmptyStateKey(t *testing.T) {
	publish := PipelinePublish{
		EventType: "m.bureau.workspace",
		Room:      "${WORKSPACE_ROOM_ID}",
		Content:   map[string]any{"status": "ready"},
	}

	data, err := json.Marshal(publish)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "event_type", "m.bureau.workspace")
	assertField(t, raw, "room", "${WORKSPACE_ROOM_ID}")
	if _, exists := raw["state_key"]; exists {
		t.Error("state_key should be omitted when empty")
	}
}

func TestPipelineVariableOmitsEmptyFields(t *testing.T) {
	// A variable with only Required set should omit description and default.
	variable := PipelineVariable{
		Required: true,
	}

	data, err := json.Marshal(variable)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "required", true)
	for _, field := range []string{"description", "default"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty", field)
		}
	}
}

func TestPipelineLogOmittedWhenNil(t *testing.T) {
	pipeline := PipelineContent{
		Steps: []PipelineStep{
			{Name: "test", Run: "echo test"},
		},
	}

	data, err := json.Marshal(pipeline)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	if _, exists := raw["log"]; exists {
		t.Error("log should be omitted when nil")
	}
}

func TestPipelineRoomPowerLevels(t *testing.T) {
	adminUserID := "@bureau-admin:bureau.local"
	levels := PipelineRoomPowerLevels(adminUserID)

	// Admin should have power level 100.
	users, ok := levels["users"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'users' map")
	}
	if users[adminUserID] != 100 {
		t.Errorf("admin power level = %v, want 100", users[adminUserID])
	}
	if len(users) != 1 {
		t.Errorf("users map should have exactly 1 entry, got %d", len(users))
	}

	// Default user power level should be 0 (other members can read).
	if levels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", levels["users_default"])
	}

	// Pipeline events require power level 100 (admin-only writes).
	events, ok := levels["events"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}
	if events[EventTypePipeline] != 100 {
		t.Errorf("%s power level = %v, want 100", EventTypePipeline, events[EventTypePipeline])
	}

	// Room metadata events from AdminProtectedEvents should all be PL 100.
	for _, eventType := range []string{
		"m.room.encryption", "m.room.server_acl",
		"m.room.tombstone", "m.space.child",
	} {
		if events[eventType] != 100 {
			t.Errorf("%s power level = %v, want 100", eventType, events[eventType])
		}
	}

	// Default event power level should be 100 (admin-only room).
	if levels["events_default"] != 100 {
		t.Errorf("events_default = %v, want 100", levels["events_default"])
	}

	// Administrative actions all require power level 100.
	for _, field := range []string{"state_default", "ban", "kick", "invite", "redact"} {
		if levels[field] != 100 {
			t.Errorf("%s = %v, want 100", field, levels[field])
		}
	}

	// Invite is 100 (unlike config/workspace rooms where machine can
	// invite at PL 50 — pipeline rooms have no machine tier).
	if levels["invite"] != 100 {
		t.Errorf("invite = %v, want 100", levels["invite"])
	}
}

// --- PipelineResultContent tests ---

func validPipelineResultContent() PipelineResultContent {
	return PipelineResultContent{
		Version:     1,
		PipelineRef: "dev-workspace-init",
		Conclusion:  "success",
		StartedAt:   "2026-02-12T10:00:00Z",
		CompletedAt: "2026-02-12T10:01:30Z",
		DurationMS:  90000,
		StepCount:   3,
		StepResults: []PipelineStepResult{
			{Name: "clone-repo", Status: "ok", DurationMS: 30000},
			{Name: "install-deps", Status: "ok", DurationMS: 45000},
			{Name: "publish-ready", Status: "ok", DurationMS: 200},
		},
		LogEventID: "$abc123:bureau.local",
	}
}

func TestPipelineResultContentRoundTrip(t *testing.T) {
	original := validPipelineResultContent()
	original.Extra = map[string]json.RawMessage{
		"trigger_event": json.RawMessage(`"$trigger123:bureau.local"`),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON field names match the wire format.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}

	requiredFields := []string{
		"version", "pipeline_ref", "conclusion",
		"started_at", "completed_at", "duration_ms",
		"step_count", "step_results", "log_event_id", "extra",
	}
	for _, field := range requiredFields {
		if _, exists := raw[field]; !exists {
			t.Errorf("JSON missing field %q", field)
		}
	}

	// Verify step result wire format.
	stepResults, ok := raw["step_results"].([]any)
	if !ok {
		t.Fatal("step_results is not an array")
	}
	if len(stepResults) != 3 {
		t.Fatalf("step_results length = %d, want 3", len(stepResults))
	}
	firstStep, ok := stepResults[0].(map[string]any)
	if !ok {
		t.Fatal("step_results[0] is not an object")
	}
	for _, field := range []string{"name", "status", "duration_ms"} {
		if _, exists := firstStep[field]; !exists {
			t.Errorf("step_results[0] missing field %q", field)
		}
	}

	// Round-trip.
	var decoded PipelineResultContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !reflect.DeepEqual(original, decoded) {
		t.Errorf("round-trip mismatch:\n  original: %+v\n  decoded:  %+v", original, decoded)
	}
}

func TestPipelineResultContentOmitsEmptyOptionals(t *testing.T) {
	content := PipelineResultContent{
		Version:     1,
		PipelineRef: "dev-init",
		Conclusion:  "success",
		StartedAt:   "2026-02-12T10:00:00Z",
		CompletedAt: "2026-02-12T10:01:00Z",
		DurationMS:  60000,
		StepCount:   1,
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Optional fields with zero values should be omitted.
	for _, field := range []string{"step_results", "failed_step", "error_message", "log_event_id", "extra"} {
		if _, exists := raw[field]; exists {
			t.Errorf("expected field %q to be omitted, but it is present", field)
		}
	}
}

func TestPipelineResultContentFailure(t *testing.T) {
	content := PipelineResultContent{
		Version:     1,
		PipelineRef: "ci-pipeline",
		Conclusion:  "failure",
		StartedAt:   "2026-02-12T10:00:00Z",
		CompletedAt: "2026-02-12T10:00:45Z",
		DurationMS:  45000,
		StepCount:   3,
		StepResults: []PipelineStepResult{
			{Name: "build", Status: "ok", DurationMS: 30000},
			{Name: "test", Status: "failed", DurationMS: 15000, Error: "exit code 1"},
		},
		FailedStep:   "test",
		ErrorMessage: "exit code 1",
		LogEventID:   "$log:bureau.local",
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if raw["failed_step"] != "test" {
		t.Errorf("failed_step = %v, want %q", raw["failed_step"], "test")
	}
	if raw["error_message"] != "exit code 1" {
		t.Errorf("error_message = %v, want %q", raw["error_message"], "exit code 1")
	}

	var decoded PipelineResultContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.FailedStep != "test" {
		t.Errorf("decoded.FailedStep = %q, want %q", decoded.FailedStep, "test")
	}
}

func TestPipelineResultContentExtraRoundTrip(t *testing.T) {
	content := validPipelineResultContent()
	content.Extra = map[string]json.RawMessage{
		"custom_metric": json.RawMessage(`42`),
		"build_info":    json.RawMessage(`{"commit":"abc123"}`),
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded PipelineResultContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if string(decoded.Extra["custom_metric"]) != "42" {
		t.Errorf("Extra[custom_metric] = %s, want 42", decoded.Extra["custom_metric"])
	}
	if string(decoded.Extra["build_info"]) != `{"commit":"abc123"}` {
		t.Errorf("Extra[build_info] = %s, want {\"commit\":\"abc123\"}", decoded.Extra["build_info"])
	}
}

func TestPipelineResultContentForwardCompatibility(t *testing.T) {
	// Simulate a newer version with unknown fields. Readers should
	// successfully unmarshal and access known fields without error.
	futureJSON := `{
		"version": 2,
		"pipeline_ref": "dev-init",
		"conclusion": "success",
		"started_at": "2026-02-12T10:00:00Z",
		"completed_at": "2026-02-12T10:01:00Z",
		"duration_ms": 60000,
		"step_count": 1,
		"future_field": "something new",
		"extra": {"custom": true}
	}`

	var content PipelineResultContent
	if err := json.Unmarshal([]byte(futureJSON), &content); err != nil {
		t.Fatalf("Unmarshal future content: %v", err)
	}

	// Known fields are correctly populated.
	if content.Version != 2 {
		t.Errorf("Version = %d, want 2", content.Version)
	}
	if content.PipelineRef != "dev-init" {
		t.Errorf("PipelineRef = %q, want %q", content.PipelineRef, "dev-init")
	}
	if content.Conclusion != "success" {
		t.Errorf("Conclusion = %q, want %q", content.Conclusion, "success")
	}

	// CanModify should refuse modification (version > current).
	if err := content.CanModify(); err == nil {
		t.Error("CanModify() = nil for future version, want error")
	}
}

func TestPipelineResultContentValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*PipelineResultContent)
		wantErr string
	}{
		{
			name:    "valid",
			modify:  func(p *PipelineResultContent) {},
			wantErr: "",
		},
		{
			name:    "version_zero",
			modify:  func(p *PipelineResultContent) { p.Version = 0 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "version_negative",
			modify:  func(p *PipelineResultContent) { p.Version = -1 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "pipeline_ref_empty",
			modify:  func(p *PipelineResultContent) { p.PipelineRef = "" },
			wantErr: "pipeline_ref is required",
		},
		{
			name:    "conclusion_empty",
			modify:  func(p *PipelineResultContent) { p.Conclusion = "" },
			wantErr: "conclusion is required",
		},
		{
			name:    "conclusion_unknown",
			modify:  func(p *PipelineResultContent) { p.Conclusion = "cancelled" },
			wantErr: `unknown conclusion "cancelled"`,
		},
		{
			name:    "conclusion_success",
			modify:  func(p *PipelineResultContent) { p.Conclusion = "success" },
			wantErr: "",
		},
		{
			name:    "conclusion_failure",
			modify:  func(p *PipelineResultContent) { p.Conclusion = "failure" },
			wantErr: "",
		},
		{
			name:    "conclusion_aborted",
			modify:  func(p *PipelineResultContent) { p.Conclusion = "aborted" },
			wantErr: "",
		},
		{
			name:    "started_at_empty",
			modify:  func(p *PipelineResultContent) { p.StartedAt = "" },
			wantErr: "started_at is required",
		},
		{
			name:    "completed_at_empty",
			modify:  func(p *PipelineResultContent) { p.CompletedAt = "" },
			wantErr: "completed_at is required",
		},
		{
			name:    "step_count_zero",
			modify:  func(p *PipelineResultContent) { p.StepCount = 0 },
			wantErr: "step_count must be >= 1",
		},
		{
			name: "step_result_invalid",
			modify: func(p *PipelineResultContent) {
				p.StepResults = []PipelineStepResult{
					{Name: "", Status: "ok"},
				}
			},
			wantErr: "step_results[0]: step result: name is required",
		},
		{
			name: "step_result_invalid_status",
			modify: func(p *PipelineResultContent) {
				p.StepResults = []PipelineStepResult{
					{Name: "build", Status: "unknown"},
				}
			},
			wantErr: `step_results[0]: step result: unknown status "unknown"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := validPipelineResultContent()
			test.modify(&content)
			err := content.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestPipelineResultContentCanModify(t *testing.T) {
	tests := []struct {
		name    string
		version int
		wantErr bool
	}{
		{"current_version", PipelineResultContentVersion, false},
		{"older_version", PipelineResultContentVersion - 1, false},
		{"newer_version", PipelineResultContentVersion + 1, true},
		{"far_future_version", PipelineResultContentVersion + 100, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := validPipelineResultContent()
			content.Version = test.version
			err := content.CanModify()
			if test.wantErr {
				if err == nil {
					t.Fatal("CanModify() = nil, want error")
				}
				message := err.Error()
				if !strings.Contains(message, "upgrade") {
					t.Errorf("error should mention upgrade: %q", message)
				}
			} else {
				if err != nil {
					t.Errorf("CanModify() = %v, want nil", err)
				}
			}
		})
	}
}

func TestPipelineStepResultValidate(t *testing.T) {
	tests := []struct {
		name    string
		step    PipelineStepResult
		wantErr string
	}{
		{
			name:    "valid_ok",
			step:    PipelineStepResult{Name: "build", Status: "ok", DurationMS: 1000},
			wantErr: "",
		},
		{
			name:    "valid_failed",
			step:    PipelineStepResult{Name: "test", Status: "failed", DurationMS: 500, Error: "exit 1"},
			wantErr: "",
		},
		{
			name:    "valid_failed_optional",
			step:    PipelineStepResult{Name: "lint", Status: "failed (optional)", DurationMS: 200},
			wantErr: "",
		},
		{
			name:    "valid_skipped",
			step:    PipelineStepResult{Name: "deploy", Status: "skipped"},
			wantErr: "",
		},
		{
			name:    "valid_aborted",
			step:    PipelineStepResult{Name: "check", Status: "aborted"},
			wantErr: "",
		},
		{
			name:    "name_empty",
			step:    PipelineStepResult{Name: "", Status: "ok"},
			wantErr: "name is required",
		},
		{
			name:    "status_empty",
			step:    PipelineStepResult{Name: "build", Status: ""},
			wantErr: "status is required",
		},
		{
			name:    "status_unknown",
			step:    PipelineStepResult{Name: "build", Status: "timeout"},
			wantErr: `unknown status "timeout"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.step.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestMsgTypeConstants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		constant string
		want     string
	}{
		{"command", MsgTypeCommand, "m.bureau.command"},
		{"command_result", MsgTypeCommandResult, "m.bureau.command_result"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.constant != test.want {
				t.Errorf("%s = %q, want %q", test.name, test.constant, test.want)
			}
		})
	}
}

func TestPowerLevelConstants(t *testing.T) {
	t.Parallel()
	if PowerLevelReadOnly != 0 {
		t.Errorf("PowerLevelReadOnly = %d, want 0", PowerLevelReadOnly)
	}
	if PowerLevelOperator != 50 {
		t.Errorf("PowerLevelOperator = %d, want 50", PowerLevelOperator)
	}
	if PowerLevelAdmin != 100 {
		t.Errorf("PowerLevelAdmin = %d, want 100", PowerLevelAdmin)
	}
}

func TestCommandMessageRoundTrip(t *testing.T) {
	t.Parallel()
	original := CommandMessage{
		MsgType:       MsgTypeCommand,
		Body:          "workspace status iree/amdgpu/inference",
		Command:       "workspace.status",
		Workspace:     "iree/amdgpu/inference",
		RequestID:     "req-a7f3",
		SenderMachine: "machine/workstation",
		Parameters: map[string]any{
			"verbose": true,
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON field names match the wire format (snake_case).
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "msgtype", MsgTypeCommand)
	assertField(t, raw, "body", "workspace status iree/amdgpu/inference")
	assertField(t, raw, "command", "workspace.status")
	assertField(t, raw, "workspace", "iree/amdgpu/inference")
	assertField(t, raw, "request_id", "req-a7f3")
	assertField(t, raw, "sender_machine", "machine/workstation")

	// Verify parameters round-trip.
	params, ok := raw["parameters"].(map[string]any)
	if !ok {
		t.Fatal("parameters missing or wrong type")
	}
	assertField(t, params, "verbose", true)

	var decoded CommandMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.MsgType != original.MsgType ||
		decoded.Body != original.Body ||
		decoded.Command != original.Command ||
		decoded.Workspace != original.Workspace ||
		decoded.RequestID != original.RequestID ||
		decoded.SenderMachine != original.SenderMachine {
		t.Errorf("round-trip string field mismatch: got %+v", decoded)
	}
	if len(decoded.Parameters) != len(original.Parameters) {
		t.Errorf("parameters length = %d, want %d", len(decoded.Parameters), len(original.Parameters))
	}
}

func TestCommandMessageMinimal(t *testing.T) {
	t.Parallel()
	original := CommandMessage{
		MsgType: MsgTypeCommand,
		Body:    "workspace list",
		Command: "workspace.list",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	// Required fields present.
	assertField(t, raw, "msgtype", MsgTypeCommand)
	assertField(t, raw, "command", "workspace.list")

	// Optional fields omitted from JSON.
	for _, field := range []string{"workspace", "request_id", "sender_machine", "parameters"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted from minimal CommandMessage", field)
		}
	}

	var decoded CommandMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

// assertField checks that a JSON object has a field with the expected value.
func assertField(t *testing.T, object map[string]any, key string, want any) {
	t.Helper()
	got, ok := object[key]
	if !ok {
		t.Errorf("field %q missing from JSON", key)
		return
	}
	// JSON numbers are float64, booleans are bool, strings are string.
	if got != want {
		t.Errorf("field %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}

func TestArtifactScopeRoundTrip(t *testing.T) {
	original := ArtifactScope{
		Version:          1,
		ServicePrincipal: "@service/artifact/main:bureau.local",
		TagGlobs:         []string{"iree/resnet50/**", "shared/datasets/*"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "version", float64(1))
	assertField(t, raw, "service_principal", "@service/artifact/main:bureau.local")

	var decoded ArtifactScope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestArtifactScopeOmitsEmptyOptionals(t *testing.T) {
	scope := ArtifactScope{
		Version:          1,
		ServicePrincipal: "@service/artifact/main:bureau.local",
	}

	data, err := json.Marshal(scope)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	if _, exists := raw["tag_globs"]; exists {
		t.Error("tag_globs should be omitted when empty, but is present")
	}
}

func TestArtifactScopeValidate(t *testing.T) {
	tests := []struct {
		name    string
		scope   ArtifactScope
		wantErr string
	}{
		{
			name: "valid",
			scope: ArtifactScope{
				Version:          1,
				ServicePrincipal: "@service/artifact/main:bureau.local",
			},
			wantErr: "",
		},
		{
			name: "valid_with_globs",
			scope: ArtifactScope{
				Version:          1,
				ServicePrincipal: "@service/artifact/main:bureau.local",
				TagGlobs:         []string{"iree/**"},
			},
			wantErr: "",
		},
		{
			name: "version_zero",
			scope: ArtifactScope{
				Version:          0,
				ServicePrincipal: "@service/artifact/main:bureau.local",
			},
			wantErr: "version must be >= 1",
		},
		{
			name: "service_principal_empty",
			scope: ArtifactScope{
				Version: 1,
			},
			wantErr: "service_principal is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.scope.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestArtifactScopeCanModify(t *testing.T) {
	tests := []struct {
		name    string
		version int
		wantErr bool
	}{
		{"current_version", ArtifactScopeVersion, false},
		{"older_version", ArtifactScopeVersion - 1, false},
		{"newer_version", ArtifactScopeVersion + 1, true},
		{"far_future_version", ArtifactScopeVersion + 100, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scope := ArtifactScope{
				Version:          test.version,
				ServicePrincipal: "@service/artifact/main:bureau.local",
			}
			err := scope.CanModify()
			if test.wantErr {
				if err == nil {
					t.Fatal("CanModify() = nil, want error")
				}
				if !strings.Contains(err.Error(), "upgrade") {
					t.Errorf("error should mention upgrade: %q", err)
				}
			} else {
				if err != nil {
					t.Errorf("CanModify() = %v, want nil", err)
				}
			}
		})
	}
}

func TestArtifactScopeForwardCompatibility(t *testing.T) {
	// Simulate a v2 event with an unknown top-level field. This
	// documents the behavior that CanModify guards against: unknown
	// fields are silently dropped on unmarshal/re-marshal.
	v2JSON := `{
		"version": 2,
		"service_principal": "@service/artifact/main:bureau.local",
		"tag_globs": ["iree/**"],
		"new_v2_field": "this field does not exist in v1 ArtifactScope"
	}`

	// v1 code can unmarshal v2 events without error.
	var scope ArtifactScope
	if err := json.Unmarshal([]byte(v2JSON), &scope); err != nil {
		t.Fatalf("Unmarshal v2 event: %v", err)
	}

	if scope.Version != 2 {
		t.Errorf("Version = %d, want 2", scope.Version)
	}
	if scope.ServicePrincipal != "@service/artifact/main:bureau.local" {
		t.Errorf("ServicePrincipal mismatch")
	}

	// CanModify rejects modification of v2 events.
	if err := scope.CanModify(); err == nil {
		t.Fatal("CanModify() = nil for v2 event, want error")
	}

	// Re-marshal drops the unknown field (this is what CanModify prevents).
	remarshaled, _ := json.Marshal(scope)
	var raw map[string]any
	json.Unmarshal(remarshaled, &raw)
	if _, exists := raw["new_v2_field"]; exists {
		t.Error("new_v2_field survived round-trip through v1 struct (unexpected)")
	}
}

func TestArtifactRoomPowerLevels(t *testing.T) {
	adminUserID := "@bureau/admin:bureau.local"
	powerLevels := ArtifactRoomPowerLevels(adminUserID)

	// Admin has PL 100.
	users := powerLevels["users"].(map[string]any)
	if users[adminUserID] != 100 {
		t.Errorf("admin PL = %v, want 100", users[adminUserID])
	}

	// Default user PL is 0.
	if powerLevels["users_default"] != 0 {
		t.Errorf("users_default = %v, want 0", powerLevels["users_default"])
	}

	// No member-writable event types: all artifact metadata lives in
	// the artifact service, not Matrix state events.
	events := powerLevels["events"].(map[string]any)

	// Default event PL is 100 (unknown event types require admin).
	if powerLevels["events_default"] != 100 {
		t.Errorf("events_default = %v, want 100", powerLevels["events_default"])
	}

	// Default state PL is 100.
	if powerLevels["state_default"] != 100 {
		t.Errorf("state_default = %v, want 100", powerLevels["state_default"])
	}

	// Admin-protected Matrix events are all PL 100.
	for _, eventType := range []string{
		"m.room.power_levels", "m.room.name", "m.room.topic",
		"m.room.canonical_alias", "m.room.join_rules",
	} {
		if events[eventType] != 100 {
			t.Errorf("%s PL = %v, want 100", eventType, events[eventType])
		}
	}

	// Administrative actions require PL 100.
	for _, action := range []string{"ban", "kick", "invite", "redact"} {
		if powerLevels[action] != 100 {
			t.Errorf("%s = %v, want 100", action, powerLevels[action])
		}
	}
}

func TestVersionConstants(t *testing.T) {
	// Verify version constants are positive. A zero version
	// constant would mean CanModify can never reject anything.
	if ArtifactScopeVersion < 1 {
		t.Errorf("ArtifactScopeVersion = %d, must be >= 1", ArtifactScopeVersion)
	}
	if CredentialsVersion < 1 {
		t.Errorf("CredentialsVersion = %d, must be >= 1", CredentialsVersion)
	}
	if TicketContentVersion < 1 {
		t.Errorf("TicketContentVersion = %d, must be >= 1", TicketContentVersion)
	}
	if TicketConfigVersion < 1 {
		t.Errorf("TicketConfigVersion = %d, must be >= 1", TicketConfigVersion)
	}
}

// --- Ticket schema tests ---

// validTicketContent returns a TicketContent with all required fields
// set to valid values. Tests modify individual fields to test validation.
func validTicketContent() TicketContent {
	return TicketContent{
		Version:   1,
		Title:     "Fix authentication bug in login flow",
		Status:    "open",
		Priority:  2,
		Type:      "bug",
		CreatedBy: "@iree/amdgpu/pm:bureau.local",
		CreatedAt: "2026-02-12T10:00:00Z",
		UpdatedAt: "2026-02-12T10:00:00Z",
	}
}

func TestTicketContentRoundTrip(t *testing.T) {
	original := TicketContent{
		Version:   1,
		Title:     "Implement AMDGPU inference pipeline",
		Body:      "Set up the full inference pipeline for AMDGPU targets.",
		Status:    "in_progress",
		Priority:  1,
		Type:      "feature",
		Labels:    []string{"amdgpu", "inference", "p1"},
		Assignee:  "@iree/amdgpu/pm:bureau.local",
		Parent:    "tkt-epic1",
		BlockedBy: []string{"tkt-a3f9", "tkt-b2c4"},
		Gates: []TicketGate{
			{
				ID:          "ci-pass",
				Type:        "pipeline",
				Status:      "pending",
				Description: "CI pipeline must pass",
				PipelineRef: "ci/amdgpu-tests",
				Conclusion:  "success",
				CreatedAt:   "2026-02-12T10:00:00Z",
			},
			{
				ID:          "lead-approval",
				Type:        "human",
				Status:      "satisfied",
				Description: "Team lead approval",
				CreatedAt:   "2026-02-12T10:00:00Z",
				SatisfiedAt: "2026-02-12T11:00:00Z",
				SatisfiedBy: "@bureau/admin:bureau.local",
			},
		},
		Notes: []TicketNote{
			{
				ID:        "n-1",
				Author:    "@bureau/admin:bureau.local",
				CreatedAt: "2026-02-12T10:30:00Z",
				Body:      "Be careful about the memory alignment on MI300X.",
			},
		},
		Attachments: []TicketAttachment{
			{
				Ref:         "art-a3f9b2c1e7d4",
				Label:       "stack trace from crash",
				ContentType: "text/plain",
			},
			{
				Ref:   "art-b7e3d9f0a1c5",
				Label: "screenshot of rendering bug",
			},
		},
		CreatedBy:   "@bureau/admin:bureau.local",
		CreatedAt:   "2026-02-12T09:00:00Z",
		UpdatedAt:   "2026-02-12T11:00:00Z",
		ClosedAt:    "",
		CloseReason: "",
		Origin: &TicketOrigin{
			Source:      "github",
			ExternalRef: "GH-4201",
			SourceRoom:  "!old_room:bureau.local",
		},
		Extra: map[string]json.RawMessage{
			"experimental": json.RawMessage(`{"nested":true}`),
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify JSON field names match the wire format.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "version", float64(1))
	assertField(t, raw, "title", original.Title)
	assertField(t, raw, "body", original.Body)
	assertField(t, raw, "status", "in_progress")
	assertField(t, raw, "priority", float64(1))
	assertField(t, raw, "type", "feature")
	assertField(t, raw, "assignee", original.Assignee)
	assertField(t, raw, "parent", "tkt-epic1")
	assertField(t, raw, "created_by", "@bureau/admin:bureau.local")
	assertField(t, raw, "created_at", "2026-02-12T09:00:00Z")
	assertField(t, raw, "updated_at", "2026-02-12T11:00:00Z")

	// Labels: verify as JSON array.
	labels, ok := raw["labels"].([]any)
	if !ok {
		t.Fatalf("labels is not an array: %T", raw["labels"])
	}
	if len(labels) != 3 || labels[0] != "amdgpu" || labels[1] != "inference" || labels[2] != "p1" {
		t.Errorf("labels = %v, want [amdgpu inference p1]", labels)
	}

	// BlockedBy: verify as JSON array.
	blockedBy, ok := raw["blocked_by"].([]any)
	if !ok {
		t.Fatalf("blocked_by is not an array: %T", raw["blocked_by"])
	}
	if len(blockedBy) != 2 || blockedBy[0] != "tkt-a3f9" || blockedBy[1] != "tkt-b2c4" {
		t.Errorf("blocked_by = %v, want [tkt-a3f9 tkt-b2c4]", blockedBy)
	}

	// Gates: verify first gate wire format.
	gates, ok := raw["gates"].([]any)
	if !ok {
		t.Fatalf("gates is not an array: %T", raw["gates"])
	}
	if len(gates) != 2 {
		t.Fatalf("gates count = %d, want 2", len(gates))
	}
	firstGate := gates[0].(map[string]any)
	assertField(t, firstGate, "id", "ci-pass")
	assertField(t, firstGate, "type", "pipeline")
	assertField(t, firstGate, "status", "pending")
	assertField(t, firstGate, "description", "CI pipeline must pass")
	assertField(t, firstGate, "pipeline_ref", "ci/amdgpu-tests")
	assertField(t, firstGate, "conclusion", "success")

	// Satisfied gate: verify lifecycle metadata.
	secondGate := gates[1].(map[string]any)
	assertField(t, secondGate, "id", "lead-approval")
	assertField(t, secondGate, "type", "human")
	assertField(t, secondGate, "status", "satisfied")
	assertField(t, secondGate, "satisfied_at", "2026-02-12T11:00:00Z")
	assertField(t, secondGate, "satisfied_by", "@bureau/admin:bureau.local")

	// Notes: verify wire format.
	notes, ok := raw["notes"].([]any)
	if !ok {
		t.Fatalf("notes is not an array: %T", raw["notes"])
	}
	if len(notes) != 1 {
		t.Fatalf("notes count = %d, want 1", len(notes))
	}
	firstNote := notes[0].(map[string]any)
	assertField(t, firstNote, "id", "n-1")
	assertField(t, firstNote, "author", "@bureau/admin:bureau.local")
	assertField(t, firstNote, "body", "Be careful about the memory alignment on MI300X.")

	// Attachments: verify wire format.
	attachments, ok := raw["attachments"].([]any)
	if !ok {
		t.Fatalf("attachments is not an array: %T", raw["attachments"])
	}
	if len(attachments) != 2 {
		t.Fatalf("attachments count = %d, want 2", len(attachments))
	}
	firstAttachment := attachments[0].(map[string]any)
	assertField(t, firstAttachment, "ref", "art-a3f9b2c1e7d4")
	assertField(t, firstAttachment, "label", "stack trace from crash")
	assertField(t, firstAttachment, "content_type", "text/plain")

	// Origin: verify wire format.
	origin, ok := raw["origin"].(map[string]any)
	if !ok {
		t.Fatalf("origin is not an object: %T", raw["origin"])
	}
	assertField(t, origin, "source", "github")
	assertField(t, origin, "external_ref", "GH-4201")
	assertField(t, origin, "source_room", "!old_room:bureau.local")

	// Extra: verify nested structure.
	extra, ok := raw["extra"].(map[string]any)
	if !ok {
		t.Fatalf("extra is not an object: %T", raw["extra"])
	}
	experimental, ok := extra["experimental"].(map[string]any)
	if !ok {
		t.Fatalf("extra.experimental is not an object: %T", extra["experimental"])
	}
	if experimental["nested"] != true {
		t.Errorf("extra.experimental.nested = %v, want true", experimental["nested"])
	}

	// Round-trip: marshal → unmarshal → compare.
	var decoded TicketContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestTicketContentOmitsEmptyOptionals(t *testing.T) {
	content := validTicketContent()

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	optionalFields := []string{
		"body", "labels", "assignee", "parent", "blocked_by",
		"gates", "notes", "attachments", "closed_at", "close_reason",
		"origin", "extra",
	}
	for _, field := range optionalFields {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
	}

	// Required fields must be present.
	requiredFields := []string{
		"version", "title", "status", "priority", "type",
		"created_by", "created_at", "updated_at",
	}
	for _, field := range requiredFields {
		if _, exists := raw[field]; !exists {
			t.Errorf("required field %s is missing from JSON", field)
		}
	}
}

func TestTicketContentExtraRoundTrip(t *testing.T) {
	original := validTicketContent()
	original.Extra = map[string]json.RawMessage{
		"string_field":  json.RawMessage(`"hello"`),
		"number_field":  json.RawMessage(`42`),
		"object_field":  json.RawMessage(`{"key":"value","count":3}`),
		"array_field":   json.RawMessage(`[1,2,3]`),
		"boolean_field": json.RawMessage(`false`),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded TicketContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Extra) != 5 {
		t.Fatalf("Extra has %d entries, want 5", len(decoded.Extra))
	}

	for key, want := range original.Extra {
		got, ok := decoded.Extra[key]
		if !ok {
			t.Errorf("Extra[%q] missing after round-trip", key)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("Extra[%q] = %s, want %s", key, got, want)
		}
	}
}

func TestTicketContentForwardCompatibility(t *testing.T) {
	// Simulate a v2 event with an unknown top-level field. This
	// documents the behavior that CanModify guards against: unknown
	// fields are silently dropped on unmarshal, so a read-modify-write
	// cycle through v1 code would lose the "new_v2_field".
	v2JSON := `{
		"version": 2,
		"title": "Fix something",
		"status": "open",
		"priority": 2,
		"type": "bug",
		"created_by": "@bureau/admin:bureau.local",
		"created_at": "2026-02-12T10:00:00Z",
		"updated_at": "2026-02-12T10:00:00Z",
		"new_v2_field": "this field does not exist in v1 TicketContent"
	}`

	// v1 code can unmarshal v2 events without error.
	var content TicketContent
	if err := json.Unmarshal([]byte(v2JSON), &content); err != nil {
		t.Fatalf("Unmarshal v2 event: %v", err)
	}

	// Known fields are correctly populated.
	if content.Version != 2 {
		t.Errorf("Version = %d, want 2", content.Version)
	}
	if content.Title != "Fix something" {
		t.Errorf("Title = %q, want %q", content.Title, "Fix something")
	}

	// CanModify rejects modification of v2 events from v1 code.
	if err := content.CanModify(); err == nil {
		t.Error("CanModify() should reject v2 events from v1 code")
	}

	// Re-marshaling drops the unknown field.
	remarshaled, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if strings.Contains(string(remarshaled), "new_v2_field") {
		t.Error("unknown field survived re-marshal; expected it to be dropped")
	}
}

func TestTicketContentValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*TicketContent)
		wantErr string
	}{
		{
			name:    "valid",
			modify:  func(tc *TicketContent) {},
			wantErr: "",
		},
		{
			name:    "version_zero",
			modify:  func(tc *TicketContent) { tc.Version = 0 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "version_negative",
			modify:  func(tc *TicketContent) { tc.Version = -1 },
			wantErr: "version must be >= 1",
		},
		{
			name:    "title_empty",
			modify:  func(tc *TicketContent) { tc.Title = "" },
			wantErr: "title is required",
		},
		{
			name:    "status_empty",
			modify:  func(tc *TicketContent) { tc.Status = "" },
			wantErr: "status is required",
		},
		{
			name:    "status_invalid",
			modify:  func(tc *TicketContent) { tc.Status = "wontfix" },
			wantErr: `unknown status "wontfix"`,
		},
		{
			name:    "status_open",
			modify:  func(tc *TicketContent) { tc.Status = "open" },
			wantErr: "",
		},
		{
			name:    "status_in_progress",
			modify:  func(tc *TicketContent) { tc.Status = "in_progress" },
			wantErr: "",
		},
		{
			name:    "status_blocked",
			modify:  func(tc *TicketContent) { tc.Status = "blocked" },
			wantErr: "",
		},
		{
			name:    "status_closed",
			modify:  func(tc *TicketContent) { tc.Status = "closed" },
			wantErr: "",
		},
		{
			name:    "priority_negative",
			modify:  func(tc *TicketContent) { tc.Priority = -1 },
			wantErr: "priority must be 0-4",
		},
		{
			name:    "priority_too_high",
			modify:  func(tc *TicketContent) { tc.Priority = 5 },
			wantErr: "priority must be 0-4",
		},
		{
			name:    "priority_zero_critical",
			modify:  func(tc *TicketContent) { tc.Priority = 0 },
			wantErr: "",
		},
		{
			name:    "priority_four_backlog",
			modify:  func(tc *TicketContent) { tc.Priority = 4 },
			wantErr: "",
		},
		{
			name:    "type_empty",
			modify:  func(tc *TicketContent) { tc.Type = "" },
			wantErr: "type is required",
		},
		{
			name:    "type_invalid",
			modify:  func(tc *TicketContent) { tc.Type = "improvement" },
			wantErr: `unknown type "improvement"`,
		},
		{
			name:    "type_task",
			modify:  func(tc *TicketContent) { tc.Type = "task" },
			wantErr: "",
		},
		{
			name:    "type_bug",
			modify:  func(tc *TicketContent) { tc.Type = "bug" },
			wantErr: "",
		},
		{
			name:    "type_feature",
			modify:  func(tc *TicketContent) { tc.Type = "feature" },
			wantErr: "",
		},
		{
			name:    "type_epic",
			modify:  func(tc *TicketContent) { tc.Type = "epic" },
			wantErr: "",
		},
		{
			name:    "type_chore",
			modify:  func(tc *TicketContent) { tc.Type = "chore" },
			wantErr: "",
		},
		{
			name:    "type_docs",
			modify:  func(tc *TicketContent) { tc.Type = "docs" },
			wantErr: "",
		},
		{
			name:    "type_question",
			modify:  func(tc *TicketContent) { tc.Type = "question" },
			wantErr: "",
		},
		{
			name:    "created_by_empty",
			modify:  func(tc *TicketContent) { tc.CreatedBy = "" },
			wantErr: "created_by is required",
		},
		{
			name:    "created_at_empty",
			modify:  func(tc *TicketContent) { tc.CreatedAt = "" },
			wantErr: "created_at is required",
		},
		{
			name:    "updated_at_empty",
			modify:  func(tc *TicketContent) { tc.UpdatedAt = "" },
			wantErr: "updated_at is required",
		},
		{
			name: "invalid_gate",
			modify: func(tc *TicketContent) {
				tc.Gates = []TicketGate{{ID: "", Type: "human", Status: "pending"}}
			},
			wantErr: "gates[0]: gate: id is required",
		},
		{
			name: "invalid_note",
			modify: func(tc *TicketContent) {
				tc.Notes = []TicketNote{{ID: "n-1", Author: "", CreatedAt: "2026-02-12T10:00:00Z", Body: "test"}}
			},
			wantErr: "notes[0]: note: author is required",
		},
		{
			name: "invalid_attachment",
			modify: func(tc *TicketContent) {
				tc.Attachments = []TicketAttachment{{Ref: ""}}
			},
			wantErr: "attachments[0]: attachment: ref is required",
		},
		{
			name: "invalid_origin",
			modify: func(tc *TicketContent) {
				tc.Origin = &TicketOrigin{Source: "", ExternalRef: "GH-4201"}
			},
			wantErr: "origin: origin: source is required",
		},
		{
			name: "valid_with_all_optional",
			modify: func(tc *TicketContent) {
				tc.Body = "Full description"
				tc.Labels = []string{"important"}
				tc.Assignee = "@test:bureau.local"
				tc.Parent = "tkt-parent"
				tc.BlockedBy = []string{"tkt-dep"}
				tc.Gates = []TicketGate{{ID: "g1", Type: "human", Status: "pending"}}
				tc.Notes = []TicketNote{{ID: "n-1", Author: "@a:b.c", CreatedAt: "2026-01-01T00:00:00Z", Body: "note"}}
				tc.Attachments = []TicketAttachment{{Ref: "art-abc123"}}
				tc.Origin = &TicketOrigin{Source: "github", ExternalRef: "GH-1234"}
			},
			wantErr: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := validTicketContent()
			test.modify(&content)
			err := content.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestTicketContentCanModify(t *testing.T) {
	tests := []struct {
		name    string
		version int
		wantErr bool
	}{
		{"current_version", TicketContentVersion, false},
		{"older_version", 1, false},
		{"newer_version", TicketContentVersion + 1, true},
		{"far_future_version", TicketContentVersion + 100, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := validTicketContent()
			content.Version = test.version
			err := content.CanModify()
			if test.wantErr {
				if err == nil {
					t.Fatal("CanModify() = nil, want error")
				}
				if !strings.Contains(err.Error(), "upgrade") {
					t.Errorf("error should mention upgrade: %q", err)
				}
			} else {
				if err != nil {
					t.Errorf("CanModify() = %v, want nil", err)
				}
			}
		})
	}
}

func TestTicketGateRoundTrip(t *testing.T) {
	// Exercise each gate type to verify wire format.
	tests := []struct {
		name string
		gate TicketGate
		// Fields to check on the wire-format JSON map.
		checks map[string]any
	}{
		{
			name: "pipeline_gate",
			gate: TicketGate{
				ID:          "ci-pass",
				Type:        "pipeline",
				Status:      "pending",
				Description: "CI must pass",
				PipelineRef: "ci/build-test",
				Conclusion:  "success",
				CreatedAt:   "2026-02-12T10:00:00Z",
			},
			checks: map[string]any{
				"id":           "ci-pass",
				"type":         "pipeline",
				"status":       "pending",
				"description":  "CI must pass",
				"pipeline_ref": "ci/build-test",
				"conclusion":   "success",
				"created_at":   "2026-02-12T10:00:00Z",
			},
		},
		{
			name: "human_gate_satisfied",
			gate: TicketGate{
				ID:          "lead-approval",
				Type:        "human",
				Status:      "satisfied",
				Description: "Team lead approval",
				CreatedAt:   "2026-02-12T10:00:00Z",
				SatisfiedAt: "2026-02-12T11:00:00Z",
				SatisfiedBy: "@bureau/admin:bureau.local",
			},
			checks: map[string]any{
				"id":           "lead-approval",
				"type":         "human",
				"status":       "satisfied",
				"satisfied_at": "2026-02-12T11:00:00Z",
				"satisfied_by": "@bureau/admin:bureau.local",
			},
		},
		{
			name: "state_event_gate",
			gate: TicketGate{
				ID:        "workspace-ready",
				Type:      "state_event",
				Status:    "pending",
				EventType: "m.bureau.workspace",
				StateKey:  "",
				RoomAlias: "#iree/amdgpu/inference:bureau.local",
				ContentMatch: ContentMatch{
					"status": Eq("active"),
				},
			},
			checks: map[string]any{
				"id":         "workspace-ready",
				"type":       "state_event",
				"event_type": "m.bureau.workspace",
				"room_alias": "#iree/amdgpu/inference:bureau.local",
			},
		},
		{
			name: "ticket_gate",
			gate: TicketGate{
				ID:       "dep-closed",
				Type:     "ticket",
				Status:   "pending",
				TicketID: "tkt-a3f9",
			},
			checks: map[string]any{
				"id":        "dep-closed",
				"type":      "ticket",
				"ticket_id": "tkt-a3f9",
			},
		},
		{
			name: "timer_gate",
			gate: TicketGate{
				ID:        "soak-period",
				Type:      "timer",
				Status:    "pending",
				Duration:  "24h",
				CreatedAt: "2026-02-12T10:00:00Z",
			},
			checks: map[string]any{
				"id":       "soak-period",
				"type":     "timer",
				"duration": "24h",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(test.gate)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatalf("Unmarshal to map: %v", err)
			}

			for key, want := range test.checks {
				assertField(t, raw, key, want)
			}

			// Round-trip.
			var decoded TicketGate
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(decoded, test.gate) {
				t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, test.gate)
			}
		})
	}
}

func TestTicketGateOmitsEmptyOptionals(t *testing.T) {
	// A minimal human gate has no type-specific or lifecycle fields.
	gate := TicketGate{
		ID:     "manual",
		Type:   "human",
		Status: "pending",
	}

	data, err := json.Marshal(gate)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	optionalFields := []string{
		"description", "pipeline_ref", "conclusion", "event_type",
		"state_key", "room_alias", "content_match", "ticket_id",
		"duration", "created_at", "satisfied_at", "satisfied_by",
	}
	for _, field := range optionalFields {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
	}
}

func TestTicketGateValidate(t *testing.T) {
	tests := []struct {
		name    string
		gate    TicketGate
		wantErr string
	}{
		{
			name:    "valid_human",
			gate:    TicketGate{ID: "g1", Type: "human", Status: "pending"},
			wantErr: "",
		},
		{
			name:    "valid_pipeline",
			gate:    TicketGate{ID: "g1", Type: "pipeline", Status: "pending", PipelineRef: "ci/test"},
			wantErr: "",
		},
		{
			name:    "valid_state_event",
			gate:    TicketGate{ID: "g1", Type: "state_event", Status: "satisfied", EventType: "m.bureau.workspace"},
			wantErr: "",
		},
		{
			name:    "valid_ticket",
			gate:    TicketGate{ID: "g1", Type: "ticket", Status: "pending", TicketID: "tkt-abc"},
			wantErr: "",
		},
		{
			name:    "valid_timer",
			gate:    TicketGate{ID: "g1", Type: "timer", Status: "pending", Duration: "24h"},
			wantErr: "",
		},
		{
			name:    "id_empty",
			gate:    TicketGate{ID: "", Type: "human", Status: "pending"},
			wantErr: "id is required",
		},
		{
			name:    "type_empty",
			gate:    TicketGate{ID: "g1", Type: "", Status: "pending"},
			wantErr: "type is required",
		},
		{
			name:    "type_invalid",
			gate:    TicketGate{ID: "g1", Type: "webhook", Status: "pending"},
			wantErr: `unknown type "webhook"`,
		},
		{
			name:    "status_empty",
			gate:    TicketGate{ID: "g1", Type: "human", Status: ""},
			wantErr: "status is required",
		},
		{
			name:    "status_invalid",
			gate:    TicketGate{ID: "g1", Type: "human", Status: "failed"},
			wantErr: `unknown status "failed"`,
		},
		{
			name:    "pipeline_missing_ref",
			gate:    TicketGate{ID: "g1", Type: "pipeline", Status: "pending"},
			wantErr: "pipeline_ref is required for pipeline gates",
		},
		{
			name:    "state_event_missing_event_type",
			gate:    TicketGate{ID: "g1", Type: "state_event", Status: "pending"},
			wantErr: "event_type is required for state_event gates",
		},
		{
			name:    "ticket_missing_ticket_id",
			gate:    TicketGate{ID: "g1", Type: "ticket", Status: "pending"},
			wantErr: "ticket_id is required for ticket gates",
		},
		{
			name:    "timer_missing_duration",
			gate:    TicketGate{ID: "g1", Type: "timer", Status: "pending"},
			wantErr: "duration is required for timer gates",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.gate.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestTicketNoteRoundTrip(t *testing.T) {
	original := TicketNote{
		ID:        "n-1",
		Author:    "@bureau/admin:bureau.local",
		CreatedAt: "2026-02-12T10:30:00Z",
		Body:      "Security scanner found CVE-2026-1234 in this dependency.",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "id", "n-1")
	assertField(t, raw, "author", "@bureau/admin:bureau.local")
	assertField(t, raw, "created_at", "2026-02-12T10:30:00Z")
	assertField(t, raw, "body", original.Body)

	var decoded TicketNote
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestTicketNoteValidate(t *testing.T) {
	tests := []struct {
		name    string
		note    TicketNote
		wantErr string
	}{
		{
			name:    "valid",
			note:    TicketNote{ID: "n-1", Author: "@a:b.c", CreatedAt: "2026-01-01T00:00:00Z", Body: "note"},
			wantErr: "",
		},
		{
			name:    "id_empty",
			note:    TicketNote{ID: "", Author: "@a:b.c", CreatedAt: "2026-01-01T00:00:00Z", Body: "note"},
			wantErr: "id is required",
		},
		{
			name:    "author_empty",
			note:    TicketNote{ID: "n-1", Author: "", CreatedAt: "2026-01-01T00:00:00Z", Body: "note"},
			wantErr: "author is required",
		},
		{
			name:    "created_at_empty",
			note:    TicketNote{ID: "n-1", Author: "@a:b.c", CreatedAt: "", Body: "note"},
			wantErr: "created_at is required",
		},
		{
			name:    "body_empty",
			note:    TicketNote{ID: "n-1", Author: "@a:b.c", CreatedAt: "2026-01-01T00:00:00Z", Body: ""},
			wantErr: "body is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.note.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestTicketAttachmentRoundTrip(t *testing.T) {
	original := TicketAttachment{
		Ref:         "art-a3f9b2c1e7d4",
		Label:       "crash stack trace",
		ContentType: "text/plain",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "ref", "art-a3f9b2c1e7d4")
	assertField(t, raw, "label", "crash stack trace")
	assertField(t, raw, "content_type", "text/plain")

	var decoded TicketAttachment
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestTicketAttachmentOmitsEmptyOptionals(t *testing.T) {
	attachment := TicketAttachment{Ref: "art-abc123"}

	data, err := json.Marshal(attachment)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"label", "content_type"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
	}
}

func TestTicketAttachmentValidate(t *testing.T) {
	tests := []struct {
		name    string
		attach  TicketAttachment
		wantErr string
	}{
		{
			name:    "valid_artifact",
			attach:  TicketAttachment{Ref: "art-a3f9b2c1e7d4"},
			wantErr: "",
		},
		{
			name:    "rejected_mxc",
			attach:  TicketAttachment{Ref: "mxc://bureau.local/abc123"},
			wantErr: "mxc:// refs are not supported",
		},
		{
			name:    "ref_empty",
			attach:  TicketAttachment{Ref: ""},
			wantErr: "ref is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.attach.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestTicketOriginRoundTrip(t *testing.T) {
	original := TicketOrigin{
		Source:      "github",
		ExternalRef: "GH-4201",
		SourceRoom:  "!old_room:bureau.local",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "source", "github")
	assertField(t, raw, "external_ref", "GH-4201")
	assertField(t, raw, "source_room", "!old_room:bureau.local")

	var decoded TicketOrigin
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestTicketOriginOmitsEmptySourceRoom(t *testing.T) {
	origin := TicketOrigin{Source: "github", ExternalRef: "org/repo#42"}

	data, err := json.Marshal(origin)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	if _, exists := raw["source_room"]; exists {
		t.Error("source_room should be omitted when empty")
	}
}

func TestTicketOriginValidate(t *testing.T) {
	tests := []struct {
		name    string
		origin  TicketOrigin
		wantErr string
	}{
		{
			name:    "valid",
			origin:  TicketOrigin{Source: "github", ExternalRef: "GH-4201"},
			wantErr: "",
		},
		{
			name:    "source_empty",
			origin:  TicketOrigin{Source: "", ExternalRef: "GH-4201"},
			wantErr: "source is required",
		},
		{
			name:    "external_ref_empty",
			origin:  TicketOrigin{Source: "github", ExternalRef: ""},
			wantErr: "external_ref is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.origin.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestTicketConfigContentRoundTrip(t *testing.T) {
	original := TicketConfigContent{
		Version:       1,
		Prefix:        "iree",
		DefaultLabels: []string{"amdgpu", "inference"},
		Extra: map[string]json.RawMessage{
			"auto_triage": json.RawMessage(`true`),
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
	assertField(t, raw, "version", float64(1))
	assertField(t, raw, "prefix", "iree")

	labels, ok := raw["default_labels"].([]any)
	if !ok {
		t.Fatalf("default_labels is not an array: %T", raw["default_labels"])
	}
	if len(labels) != 2 || labels[0] != "amdgpu" || labels[1] != "inference" {
		t.Errorf("default_labels = %v, want [amdgpu inference]", labels)
	}

	var decoded TicketConfigContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestTicketConfigContentOmitsEmptyOptionals(t *testing.T) {
	config := TicketConfigContent{Version: 1}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	for _, field := range []string{"prefix", "default_labels", "extra"} {
		if _, exists := raw[field]; exists {
			t.Errorf("%s should be omitted when empty, but is present", field)
		}
	}
}

func TestTicketConfigContentValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  TicketConfigContent
		wantErr string
	}{
		{
			name:    "valid_minimal",
			config:  TicketConfigContent{Version: 1},
			wantErr: "",
		},
		{
			name:    "valid_with_prefix",
			config:  TicketConfigContent{Version: 1, Prefix: "iree"},
			wantErr: "",
		},
		{
			name:    "valid_with_labels",
			config:  TicketConfigContent{Version: 1, DefaultLabels: []string{"amdgpu"}},
			wantErr: "",
		},
		{
			name:    "version_zero",
			config:  TicketConfigContent{Version: 0},
			wantErr: "version must be >= 1",
		},
		{
			name:    "version_negative",
			config:  TicketConfigContent{Version: -1},
			wantErr: "version must be >= 1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("Validate() = %q, want error containing %q", err, test.wantErr)
				}
			}
		})
	}
}

func TestTicketConfigContentCanModify(t *testing.T) {
	tests := []struct {
		name    string
		version int
		wantErr bool
	}{
		{"current_version", TicketConfigVersion, false},
		{"older_version", 1, false},
		{"newer_version", TicketConfigVersion + 1, true},
		{"far_future_version", TicketConfigVersion + 100, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := TicketConfigContent{Version: test.version}
			err := config.CanModify()
			if test.wantErr {
				if err == nil {
					t.Fatal("CanModify() = nil, want error")
				}
				if !strings.Contains(err.Error(), "upgrade") {
					t.Errorf("error should mention upgrade: %q", err)
				}
			} else {
				if err != nil {
					t.Errorf("CanModify() = %v, want nil", err)
				}
			}
		})
	}
}

func TestTicketConfigContentForwardCompatibility(t *testing.T) {
	v2JSON := `{
		"version": 2,
		"prefix": "tkt",
		"new_v2_field": "unknown to v1"
	}`

	var config TicketConfigContent
	if err := json.Unmarshal([]byte(v2JSON), &config); err != nil {
		t.Fatalf("Unmarshal v2 event: %v", err)
	}

	if config.Version != 2 {
		t.Errorf("Version = %d, want 2", config.Version)
	}
	if config.Prefix != "tkt" {
		t.Errorf("Prefix = %q, want %q", config.Prefix, "tkt")
	}

	if err := config.CanModify(); err == nil {
		t.Error("CanModify() should reject v2 events from v1 code")
	}

	remarshaled, err2 := json.Marshal(config)
	if err2 != nil {
		t.Fatalf("re-Marshal: %v", err2)
	}
	if strings.Contains(string(remarshaled), "new_v2_field") {
		t.Error("unknown field survived re-marshal; expected it to be dropped")
	}
}

func TestWorktreeStateRoundTrip(t *testing.T) {
	original := WorktreeState{
		Status:       "active",
		Project:      "iree",
		WorktreePath: "feature/amdgpu",
		Branch:       "feature/amdgpu-inference",
		Machine:      "workstation",
		UpdatedAt:    "2026-02-12T00:00:00Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	assertField(t, raw, "status", "active")
	assertField(t, raw, "project", "iree")
	assertField(t, raw, "worktree_path", "feature/amdgpu")
	assertField(t, raw, "branch", "feature/amdgpu-inference")
	assertField(t, raw, "machine", "workstation")
	assertField(t, raw, "updated_at", "2026-02-12T00:00:00Z")

	var decoded WorktreeState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

func TestWorktreeStateOmitsEmptyBranch(t *testing.T) {
	state := WorktreeState{
		Status:       "creating",
		Project:      "iree",
		WorktreePath: "detached-work",
		Machine:      "workstation",
		UpdatedAt:    "2026-02-12T00:00:00Z",
	}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}

	if _, exists := raw["branch"]; exists {
		t.Error("branch should be omitted when empty")
	}
	assertField(t, raw, "status", "creating")
	assertField(t, raw, "worktree_path", "detached-work")
}

func TestPipelineAssertStateInStep(t *testing.T) {
	content := PipelineContent{
		Steps: []PipelineStep{
			{
				Name: "verify-teardown",
				AssertState: &PipelineAssertState{
					Room:       "!room:bureau.local",
					EventType:  "m.bureau.workspace",
					Field:      "status",
					Equals:     "teardown",
					OnMismatch: "abort",
					Message:    "workspace is no longer in teardown",
				},
			},
			{
				Name: "assert-not-removing",
				AssertState: &PipelineAssertState{
					Room:      "!room:bureau.local",
					EventType: "m.bureau.worktree",
					StateKey:  "feature/amdgpu",
					Field:     "status",
					NotIn:     []string{"removing", "archived", "removed"},
				},
			},
		},
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded PipelineContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Steps) != 2 {
		t.Fatalf("Steps count = %d, want 2", len(decoded.Steps))
	}

	step0 := decoded.Steps[0]
	if step0.AssertState == nil {
		t.Fatal("Steps[0].AssertState is nil")
	}
	if step0.AssertState.Equals != "teardown" {
		t.Errorf("AssertState.Equals = %q, want %q", step0.AssertState.Equals, "teardown")
	}
	if step0.AssertState.OnMismatch != "abort" {
		t.Errorf("AssertState.OnMismatch = %q, want %q", step0.AssertState.OnMismatch, "abort")
	}

	step1 := decoded.Steps[1]
	if step1.AssertState == nil {
		t.Fatal("Steps[1].AssertState is nil")
	}
	if step1.AssertState.StateKey != "feature/amdgpu" {
		t.Errorf("AssertState.StateKey = %q, want %q", step1.AssertState.StateKey, "feature/amdgpu")
	}
	if len(step1.AssertState.NotIn) != 3 {
		t.Fatalf("AssertState.NotIn length = %d, want 3", len(step1.AssertState.NotIn))
	}
}

func TestPipelineOnFailure(t *testing.T) {
	content := PipelineContent{
		Steps: []PipelineStep{
			{Name: "do-work", Run: "echo hello"},
		},
		OnFailure: []PipelineStep{
			{
				Name: "publish-failed",
				Publish: &PipelinePublish{
					EventType: "m.bureau.worktree",
					Room:      "!room:bureau.local",
					StateKey:  "feature/amdgpu",
					Content:   map[string]any{"status": "failed"},
				},
			},
		},
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded PipelineContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.OnFailure) != 1 {
		t.Fatalf("OnFailure count = %d, want 1", len(decoded.OnFailure))
	}
	if decoded.OnFailure[0].Name != "publish-failed" {
		t.Errorf("OnFailure[0].Name = %q, want %q", decoded.OnFailure[0].Name, "publish-failed")
	}
	if decoded.OnFailure[0].Publish == nil {
		t.Fatal("OnFailure[0].Publish is nil")
	}
	if decoded.OnFailure[0].Publish.EventType != "m.bureau.worktree" {
		t.Errorf("OnFailure[0].Publish.EventType = %q, want %q",
			decoded.OnFailure[0].Publish.EventType, "m.bureau.worktree")
	}
}
