// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// FetchBundle fetches a Sigstore provenance bundle from a Nix binary
// cache's attestation directory. The bundle is expected at:
//
//	<cacheURL>/attestation/<storePathBasename>.bundle.json
//
// The storePathBasename is the filename portion of the Nix store path
// (everything after "/nix/store/"), e.g., "xyz-bureau-sysadmin-runner-env".
//
// Returns the raw bundle JSON bytes, or an error if the fetch fails
// or the server returns a non-200 status. A 404 response returns an
// error that can be distinguished with errors.Is(err, ErrNoBundleFound).
// validStorePathBasename matches Nix store path basenames:
// 32-char hash prefix (base32 a-z0-9) followed by a hyphen and the
// name portion which may contain [a-zA-Z0-9+._?=-]. No slashes,
// no path traversal.
var validStorePathBasename = regexp.MustCompile(`^[a-z0-9]{32}-[a-zA-Z0-9+._?=-]+$`)

func FetchBundle(client *http.Client, cacheURL, storePathBasename string) ([]byte, error) {
	if !validStorePathBasename.MatchString(storePathBasename) {
		return nil, fmt.Errorf("invalid store path basename %q: must match Nix store path format", storePathBasename)
	}
	url := strings.TrimRight(cacheURL, "/") + "/attestation/" + storePathBasename + ".bundle.json"

	response, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch provenance bundle from %s: %w", url, err)
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("fetch provenance bundle from %s: %w", url, ErrNoBundleFound)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch provenance bundle from %s: HTTP %d", url, response.StatusCode)
	}

	// Bundles are small (typically 5-20KB). Read the whole body.
	const maxBundleSize = 1 << 20 // 1MB — well above any realistic bundle
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBundleSize))
	if err != nil {
		return nil, fmt.Errorf("read provenance bundle from %s: %w", url, err)
	}

	return body, nil
}

// ErrNoBundleFound indicates that no provenance bundle exists for the
// requested artifact. This is distinct from a verification failure —
// it means the artifact was published without attestation. The caller
// should treat this as StatusUnverified and consult the enforcement
// level to decide whether to accept, warn, or reject.
var ErrNoBundleFound = fmt.Errorf("no provenance bundle found")
