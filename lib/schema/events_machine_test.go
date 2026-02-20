// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/bureau-foundation/bureau/lib/ref"
)

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
				Principal:         testEntity(t, "@bureau/fleet/test/agent/pm:bureau.local"),
				Template:          "llm-agent",
				AutoStart:         true,
				Labels:            map[string]string{"role": "agent", "team": "iree"},
				ServiceVisibility: []string{"service/stt/*", "service/embedding/**"},
			},
			{
				Principal: testEntity(t, "@bureau/fleet/test/service/stt/whisper:bureau.local"),
				Template:  "whisper-stt",
				AutoStart: true,
				Labels:    map[string]string{"role": "service"},
			},
			{
				Principal: testEntity(t, "@bureau/fleet/test/agent/codegen:bureau.local"),
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
	assertField(t, first, "principal", "@bureau/fleet/test/agent/pm:bureau.local")
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

func TestBureauVersionOnMachineConfig(t *testing.T) {
	config := MachineConfig{
		Principals: []PrincipalAssignment{
			{
				Principal: testEntity(t, "@bureau/fleet/test/service/stt/whisper:bureau.local"),
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
				Principal: testEntity(t, "@bureau/fleet/test/service/stt/whisper:bureau.local"),
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
	principalUserID := "@bureau/fleet/prod/service/stt/whisper:bureau.local"
	machineUserID := "@bureau/fleet/prod/machine/cloud-gpu-1:bureau.local"

	principal := mustParseEntity(t, principalUserID)
	machine := mustParseMachine(t, machineUserID)

	original := Service{
		Principal:    principal,
		Machine:      machine,
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
	assertField(t, raw, "principal", principalUserID)
	assertField(t, raw, "machine", machineUserID)
	assertField(t, raw, "protocol", "http")
	assertField(t, raw, "description", "Whisper Large V3 streaming STT")

	var decoded Service
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Principal != original.Principal {
		t.Errorf("Principal: got %v, want %v", decoded.Principal, original.Principal)
	}
	if decoded.Machine != original.Machine {
		t.Errorf("Machine: got %v, want %v", decoded.Machine, original.Machine)
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
		Principal: mustParseEntity(t, "@bureau/fleet/prod/service/tts/piper:bureau.local"),
		Machine:   mustParseMachine(t, "@bureau/fleet/prod/machine/workstation:bureau.local"),
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
	principalUserID := "@bureau/fleet/prod/service/ticket/iree:bureau.local"
	original := RoomServiceContent{
		Principal: mustParseEntity(t, principalUserID),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	assertField(t, raw, "principal", principalUserID)

	var decoded RoomServiceContent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Principal != original.Principal {
		t.Errorf("Principal: got %v, want %v", decoded.Principal, original.Principal)
	}
}

func mustParseEntity(t *testing.T, userID string) ref.Entity {
	t.Helper()
	entity, err := ref.ParseEntityUserID(userID)
	if err != nil {
		t.Fatalf("parse entity %q: %v", userID, err)
	}
	return entity
}

func mustParseMachine(t *testing.T, userID string) ref.Machine {
	t.Helper()
	machine, err := ref.ParseMachineUserID(userID)
	if err != nil {
		t.Fatalf("parse machine %q: %v", userID, err)
	}
	return machine
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

	// Machine should have power level 50 (operational writes: MachineConfig
	// for HA hosting, layout publishes, invites). PL 50 cannot modify
	// credentials, power levels, or room metadata (all PL 100).
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

	// MachineConfig requires PL 50 (machine and fleet controllers can write placements).
	// Credentials require PL 100 (admin only).
	events, ok := levels["events"].(map[string]any)
	if !ok {
		t.Fatal("power levels missing 'events' map")
	}
	if events[EventTypeMachineConfig] != 50 {
		t.Errorf("%s power level = %v, want 50", EventTypeMachineConfig, events[EventTypeMachineConfig])
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

	// State default is 0: room members (services, agents) can write
	// arbitrary state event types. Room membership is the authorization
	// boundary, not per-event-type power levels.
	if levels["state_default"] != 0 {
		t.Errorf("state_default = %v, want 0", levels["state_default"])
	}

	// Administrative actions require power level 100.
	for _, field := range []string{"ban", "kick", "redact"} {
		if levels[field] != 100 {
			t.Errorf("%s = %v, want 100", field, levels[field])
		}
	}
}
