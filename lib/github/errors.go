// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"errors"
	"fmt"
	"strings"
)

// APIError represents a non-2xx response from the GitHub REST API.
// GitHub returns structured JSON error bodies with a message, optional
// documentation URL, and optional field-level validation errors.
type APIError struct {
	// StatusCode is the HTTP response status code.
	StatusCode int

	// Message is the top-level error description from GitHub.
	Message string

	// DocumentationURL points to the relevant API documentation.
	DocumentationURL string

	// Errors contains field-level validation failures. Present only
	// on 422 Unprocessable Entity responses.
	Errors []ValidationError
}

// ValidationError describes a specific validation failure on a resource
// field. Returned by GitHub on 422 responses.
type ValidationError struct {
	Resource string `json:"resource"`
	Code     string `json:"code"`
	Field    string `json:"field"`
	Message  string `json:"message"`
}

func (err *APIError) Error() string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "github: HTTP %d: %s", err.StatusCode, err.Message)
	for _, validationError := range err.Errors {
		if validationError.Message != "" {
			fmt.Fprintf(&builder, "; %s.%s: %s", validationError.Resource, validationError.Field, validationError.Message)
		} else {
			fmt.Fprintf(&builder, "; %s.%s: %s", validationError.Resource, validationError.Field, validationError.Code)
		}
	}
	return builder.String()
}

// IsNotFound reports whether err is a GitHub API 404 Not Found response.
func IsNotFound(err error) bool {
	var apiError *APIError
	return errors.As(err, &apiError) && apiError.StatusCode == 404
}

// IsRateLimited reports whether err is a GitHub API rate limit response.
// GitHub returns 403 when the primary rate limit is exceeded and 429
// for secondary (abuse) rate limits.
func IsRateLimited(err error) bool {
	var apiError *APIError
	if !errors.As(err, &apiError) {
		return false
	}
	return apiError.StatusCode == 429 || (apiError.StatusCode == 403 && isRateLimitMessage(apiError.Message))
}

// IsValidationFailed reports whether err is a GitHub API 422 response
// with field-level validation errors.
func IsValidationFailed(err error) bool {
	var apiError *APIError
	return errors.As(err, &apiError) && apiError.StatusCode == 422
}

// IsConflict reports whether err is a GitHub API 409 Conflict response.
func IsConflict(err error) bool {
	var apiError *APIError
	return errors.As(err, &apiError) && apiError.StatusCode == 409
}

// isRateLimitMessage checks whether a 403 error message indicates a
// rate limit rather than a permission issue. GitHub's rate limit 403
// responses contain recognizable phrases.
func isRateLimitMessage(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "abuse detection")
}
