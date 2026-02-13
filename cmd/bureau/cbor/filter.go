// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cbor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// filterCBOR decodes CBOR data, converts to JSON, and pipes it through
// jq with the given filter expression and extra arguments. The jqArgs
// slice should contain the filter as the first element, followed by any
// additional jq flags (already extracted by the caller). Output from jq
// goes directly to stdout/stderr.
func filterCBOR(data []byte, jqArgs []string) error {
	if len(data) == 0 {
		return fmt.Errorf("empty input: expected CBOR data")
	}

	var value any
	if err := toolDecMode.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode CBOR: %w", err)
	}

	jsonData, err := json.Marshal(normalizeValue(value))
	if err != nil {
		return fmt.Errorf("encode JSON for jq: %w", err)
	}

	return runJQ(jsonData, jqArgs)
}

// runJQ executes jq with the given arguments, feeding jsonData to its
// stdin. jq's stdout and stderr are connected directly to the process
// stdout and stderr.
func runJQ(jsonData []byte, jqArgs []string) error {
	jqPath, err := exec.LookPath("jq")
	if err != nil {
		return fmt.Errorf("jq not found in PATH; install jq or use \"bureau cbor decode\" for raw JSON output")
	}

	cmd := exec.Command(jqPath, jqArgs...)
	cmd.Stdin = bytes.NewReader(jsonData)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			// Propagate jq's exit code so piped commands behave
			// correctly (e.g., jq -e returns 1 for false/null).
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("run jq: %w", err)
	}
	return nil
}
