// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package fleet

import "testing"

func TestValidateCacheURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{
			name: "https URL",
			url:  "https://cache.infra.bureau.foundation",
		},
		{
			name: "http URL with port and path",
			url:  "http://attic.internal:5580/main",
		},
		{
			name: "http localhost",
			url:  "http://localhost:5580/bureau",
		},
		{
			name:    "no scheme",
			url:     "cache.infra.bureau.foundation",
			wantErr: true,
		},
		{
			name:    "empty string",
			url:     "",
			wantErr: true,
		},
		{
			name:    "file scheme",
			url:     "file:///nix/store",
			wantErr: true,
		},
		{
			name:    "s3 scheme",
			url:     "s3://my-bucket/cache",
			wantErr: true,
		},
		{
			name:    "no host",
			url:     "https://",
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCacheURL(test.url)
			if test.wantErr {
				if err == nil {
					t.Errorf("validateCacheURL(%q) succeeded, want error", test.url)
				}
				return
			}
			if err != nil {
				t.Errorf("validateCacheURL(%q) failed: %v", test.url, err)
			}
		})
	}
}

func TestValidatePublicKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{
			name: "standard Nix format",
			key:  "cache.infra.bureau.foundation-1:AbCdEfGhIjKlMnOpQrStUvWxYz0123456789+/==",
		},
		{
			name: "short name",
			key:  "bureau-prod:base64key",
		},
		{
			name:    "no colon",
			key:     "just-a-string-without-colon",
			wantErr: true,
		},
		{
			name:    "empty name",
			key:     ":base64key",
			wantErr: true,
		},
		{
			name:    "empty key material",
			key:     "name:",
			wantErr: true,
		},
		{
			name:    "empty string",
			key:     "",
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validatePublicKey(test.key)
			if test.wantErr {
				if err == nil {
					t.Errorf("validatePublicKey(%q) succeeded, want error", test.key)
				}
				return
			}
			if err != nil {
				t.Errorf("validatePublicKey(%q) failed: %v", test.key, err)
			}
		})
	}
}

func TestHasCacheUpdates(t *testing.T) {
	tests := []struct {
		name   string
		params cacheParams
		want   bool
	}{
		{
			name:   "no flags",
			params: cacheParams{},
			want:   false,
		},
		{
			name:   "url only",
			params: cacheParams{URL: "https://example.com"},
			want:   true,
		},
		{
			name:   "name only",
			params: cacheParams{Name: "main"},
			want:   true,
		},
		{
			name:   "public key only",
			params: cacheParams{PublicKey: []string{"bureau:key"}},
			want:   true,
		},
		{
			name: "all flags",
			params: cacheParams{
				URL:       "https://example.com",
				Name:      "main",
				PublicKey: []string{"bureau:key"},
			},
			want: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := hasCacheUpdates(&test.params)
			if got != test.want {
				t.Errorf("hasCacheUpdates() = %v, want %v", got, test.want)
			}
		})
	}
}
