// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"errors"
	"fmt"
)

// MatrixError represents a structured error response from the Matrix homeserver.
// Callers can use errors.As to extract the structured information:
//
//	var matrixErr *MatrixError
//	if errors.As(err, &matrixErr) {
//	    if matrixErr.Code == ErrCodeNotFound { ... }
//	}
type MatrixError struct {
	// Code is the Matrix error code (e.g., "M_FORBIDDEN", "M_UNKNOWN_TOKEN").
	Code string `json:"errcode"`
	// Message is the human-readable error description from the server.
	Message string `json:"error"`
	// StatusCode is the HTTP status code of the response.
	StatusCode int `json:"-"`
}

func (e *MatrixError) Error() string {
	return fmt.Sprintf("matrix: %s (%d): %s", e.Code, e.StatusCode, e.Message)
}

// Standard Matrix error codes.
const (
	ErrCodeForbidden     = "M_FORBIDDEN"
	ErrCodeUnknownToken  = "M_UNKNOWN_TOKEN"
	ErrCodeNotFound      = "M_NOT_FOUND"
	ErrCodeUserInUse     = "M_USER_IN_USE"
	ErrCodeLimitExceeded = "M_LIMIT_EXCEEDED"
	ErrCodeUnrecognized  = "M_UNRECOGNIZED"
	ErrCodeUnknown       = "M_UNKNOWN"
	ErrCodeInvalidParam  = "M_INVALID_PARAM"
	ErrCodeMissingParam  = "M_MISSING_PARAM"
	ErrCodeExclusive     = "M_EXCLUSIVE"
	ErrCodeRoomInUse     = "M_ROOM_IN_USE"
)

// IsMatrixError checks whether err is a *MatrixError with the given error code.
func IsMatrixError(err error, code string) bool {
	var matrixErr *MatrixError
	if errors.As(err, &matrixErr) {
		return matrixErr.Code == code
	}
	return false
}
