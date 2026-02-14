// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import "fmt"

// ExitError signals a non-zero exit code without printing an extra
// error message. When a command handler returns an ExitError, the CLI
// framework exits with the specified code without printing the error
// string — the command is expected to have already written its own
// output.
//
// This is useful for commands where a non-zero exit is a valid
// outcome (e.g., "artifact exists" returning 1 for not-found, or
// "matrix doctor" returning 1 for failed checks) rather than an
// unexpected error.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exit code %d", e.Code)
}

// ExitCode returns the exit code. The CLI framework's main function
// checks for this interface on returned errors to distinguish
// "handled non-zero exit" from "unexpected error to display".
func (e *ExitError) ExitCode() int {
	return e.Code
}
