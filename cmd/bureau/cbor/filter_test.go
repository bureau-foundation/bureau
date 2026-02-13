// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"bytes"
	"os/exec"
	"testing"

	"github.com/bureau-foundation/bureau/lib/codec"
)

func TestFilterCBOR(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not in PATH, skipping filter tests")
	}

	input := map[string]any{
		"action":    "status",
		"principal": "iree/amdgpu/pm",
		"count":     int64(42),
	}
	cborData, err := codec.Marshal(input)
	if err != nil {
		t.Fatalf("marshal CBOR: %v", err)
	}

	tests := []struct {
		name   string
		args   []string
		want   string // expected stdout (trimmed)
		verify func(t *testing.T, output string)
	}{
		{
			name: "extract string field",
			args: []string{".action"},
			want: `"status"`,
		},
		{
			name: "extract number field",
			args: []string{".count"},
			want: "42",
		},
		{
			name: "raw output",
			args: []string{"-r", ".principal"},
			want: "iree/amdgpu/pm",
		},
		{
			name: "compact output",
			args: []string{"-c", "{action, count}"},
			want: `{"action":"status","count":42}`,
		},
		{
			name: "pipe expression",
			args: []string{".action | length"},
			want: "6",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// filterCBOR writes directly to os.Stdout, which makes
			// it hard to capture in tests. Instead, test the
			// underlying pieces: decode to JSON, then verify jq
			// would receive correct input.
			//
			// We test the decode→JSON part and the jq invocation
			// separately. The decode part is covered by decode
			// tests; here we verify that jq receives valid JSON.
			var jsonOutput bytes.Buffer
			if err := decodeCBOR(cborData, &jsonOutput, true, false); err != nil {
				t.Fatalf("decode for filter: %v", err)
			}

			// Run jq directly on the decoded JSON to verify the
			// filter produces expected output.
			cmd := exec.Command("jq", tt.args...)
			cmd.Stdin = bytes.NewReader(jsonOutput.Bytes())
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("jq %v: %v", tt.args, err)
			}

			got := bytes.TrimSpace(output)
			if string(got) != tt.want {
				t.Errorf("jq %v = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
