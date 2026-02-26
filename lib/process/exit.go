// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package process

import (
	"fmt"
	"os"
)

// Fatal writes "error: err" to stderr and exits with code 1. This is
// the standard Bureau binary entrypoint error handler. Use it in main()
// for errors from run() where the structured logger may not be
// initialized.
func Fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
