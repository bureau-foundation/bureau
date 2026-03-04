// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"testing"

	"github.com/bureau-foundation/bureau/lib/schema"
)

func TestParseMountSpec(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    schema.TemplateMount
		wantErr bool
	}{
		{
			name: "bind mount read-only implicit",
			spec: "/host/data:/sandbox/data",
			want: schema.TemplateMount{
				Source: "/host/data",
				Dest:   "/sandbox/data",
			},
		},
		{
			name: "bind mount read-only explicit",
			spec: "/host/data:/sandbox/data:ro",
			want: schema.TemplateMount{
				Source: "/host/data",
				Dest:   "/sandbox/data",
				Mode:   schema.MountModeRO,
			},
		},
		{
			name: "bind mount read-write",
			spec: "/host/data:/sandbox/data:rw",
			want: schema.TemplateMount{
				Source: "/host/data",
				Dest:   "/sandbox/data",
				Mode:   schema.MountModeRW,
			},
		},
		{
			name: "tmpfs mount",
			spec: "tmpfs:/tmp",
			want: schema.TemplateMount{
				Dest: "/tmp",
				Type: "tmpfs",
			},
		},
		{
			name:    "empty destination",
			spec:    "/host:",
			wantErr: true,
		},
		{
			name:    "empty source for bind mount",
			spec:    ":/sandbox/data",
			wantErr: true,
		},
		{
			name:    "unknown mode",
			spec:    "/host:/sandbox:wx",
			wantErr: true,
		},
		{
			name:    "tmpfs with mode",
			spec:    "tmpfs:/tmp:rw",
			wantErr: true,
		},
		{
			name:    "no colon separator",
			spec:    "/just/a/path",
			wantErr: true,
		},
		{
			name:    "empty spec",
			spec:    "",
			wantErr: true,
		},
		{
			name:    "empty source with mode",
			spec:    ":/sandbox:ro",
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseMountSpec(test.spec)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseMountSpec(%q) succeeded, want error", test.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMountSpec(%q) failed: %v", test.spec, err)
			}
			if got.Source != test.want.Source {
				t.Errorf("source = %q, want %q", got.Source, test.want.Source)
			}
			if got.Dest != test.want.Dest {
				t.Errorf("dest = %q, want %q", got.Dest, test.want.Dest)
			}
			if got.Type != test.want.Type {
				t.Errorf("type = %q, want %q", got.Type, test.want.Type)
			}
			if got.Mode != test.want.Mode {
				t.Errorf("mode = %q, want %q", got.Mode, test.want.Mode)
			}
		})
	}
}
