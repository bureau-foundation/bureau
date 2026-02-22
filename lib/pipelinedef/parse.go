// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package pipelinedef provides parsing, validation, and variable expansion
// for Bureau pipeline definitions. Pipelines are structured sequences of
// steps (shell commands, Matrix state event publications, interactive
// sessions) that run inside Bureau sandboxes.
//
// Pipeline definitions are stored as m.bureau.pipeline state events in
// Matrix (JSON), and authored on disk as JSONC files (JSON extended with
// comments and trailing commas). This package handles both formats.
//
// The typical flow:
//
//  1. ReadFile or Parse: JSONC bytes → pipeline.PipelineContent
//  2. Validate: structural checks (Run XOR Publish, required fields, etc.)
//  3. ResolveVariables: merge declarations + payload + environment → variable map
//  4. ExpandStep: substitute ${NAME} references in each step before execution
package pipelinedef

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/jsonc"

	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
)

// Parse strips JSONC comments and trailing commas from data, then
// unmarshals the result into a PipelineContent. The input format is
// the same JSON stored in Matrix state events, extended with //
// line comments, /* block comments */, and trailing commas.
func Parse(data []byte) (*pipeline.PipelineContent, error) {
	stripped := jsonc.ToJSON(data)

	var content pipeline.PipelineContent
	if err := json.Unmarshal(stripped, &content); err != nil {
		return nil, fmt.Errorf("parsing pipeline: %w", err)
	}

	return &content, nil
}

// ReadFile reads a JSONC pipeline file from disk and parses it into a
// PipelineContent. Returns a descriptive error if the file cannot be
// read or the JSON is malformed.
func ReadFile(path string) (*pipeline.PipelineContent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	content, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return content, nil
}

// NameFromPath extracts a pipeline name from a file path by stripping
// the directory prefix and the file extension. For example,
// "deploy/content/pipeline/dev-workspace-init.jsonc" returns
// "dev-workspace-init".
func NameFromPath(path string) string {
	base := filepath.Base(path)
	extension := filepath.Ext(base)
	return strings.TrimSuffix(base, extension)
}
