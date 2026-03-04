// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package content provides embedded Bureau content definitions (pipelines
// and templates). Content files are JSONC (JSON with comments and trailing
// commas) — the same wire format as Matrix state events plus comments for
// human documentation.
//
// Files are embedded at compile time via go:embed. The primary consumer is
// "bureau matrix setup" (publishes content to Matrix rooms during homeserver
// bootstrap). The pipeline executor also reads embedded pipelines as a Nix
// fallback when Matrix is unreachable.
package content

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tidwall/jsonc"

	"github.com/bureau-foundation/bureau/lib/pipelinedef"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
)

//go:embed pipeline/*.jsonc
var pipelineFiles embed.FS

// Pipeline is an embedded pipeline definition with its name (derived
// from the filename) and parsed content.
type Pipeline struct {
	// Name is the pipeline name, used as the Matrix state key when
	// publishing. Derived from the filename without extension (e.g.,
	// "dev-workspace-init" from "dev-workspace-init.jsonc").
	Name string

	// Content is the parsed pipeline definition.
	Content pipeline.PipelineContent

	// SourceHash is the SHA-256 hex digest of the raw JSONC source
	// file. Used by "bureau matrix doctor" to detect whether a
	// Matrix state event has been modified relative to the embedded
	// version. Comparing the hash avoids false positives from JSON
	// serialization differences (field ordering, whitespace).
	SourceHash string
}

// Pipelines returns all embedded pipeline definitions, parsed and
// validated. Returns an error if any embedded file fails to parse or
// validate — this indicates a bug in the embedded content, not a
// runtime condition.
func Pipelines() ([]Pipeline, error) {
	entries, err := pipelineFiles.ReadDir("pipeline")
	if err != nil {
		return nil, fmt.Errorf("reading embedded pipeline directory: %w", err)
	}

	var pipelines []Pipeline
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) != ".jsonc" {
			continue
		}

		path := "pipeline/" + entry.Name()
		data, err := pipelineFiles.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading embedded pipeline %s: %w", path, err)
		}

		content, err := pipelinedef.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("parsing embedded pipeline %s: %w", path, err)
		}

		issues := pipelinedef.Validate(content)
		if len(issues) > 0 {
			return nil, fmt.Errorf("validating embedded pipeline %s: %s", path, strings.Join(issues, "; "))
		}

		hash := sha256.Sum256(data)

		pipelines = append(pipelines, Pipeline{
			Name:       pipelinedef.NameFromPath(entry.Name()),
			Content:    *content,
			SourceHash: hex.EncodeToString(hash[:]),
		})
	}

	return pipelines, nil
}

//go:embed template/*.jsonc
var templateFiles embed.FS

// Template is an embedded sandbox template definition with its name
// (derived from the filename) and parsed content.
type Template struct {
	// Name is the template name, used as the Matrix state key when
	// publishing. Derived from the filename without extension (e.g.,
	// "base-networked" from "base-networked.jsonc").
	Name string

	// Content is the parsed template definition with variable
	// substitution applied (${TEMPLATE_ROOM} replaced with the
	// actual template room localpart).
	Content schema.TemplateContent

	// SourceHash is the SHA-256 hex digest of the raw JSONC source
	// file before variable substitution. Computed on the original
	// embedded bytes for reproducibility — the hash is independent
	// of the template room localpart used at publish time.
	SourceHash string
}

// Templates returns all embedded template definitions, parsed and
// validated. The templateRoomLocalpart parameter is substituted for
// ${TEMPLATE_ROOM} in inherits references (e.g., "bureau/template"
// produces inherits values like "bureau/template:base").
//
// Returns an error if any embedded file fails to parse — this
// indicates a bug in the embedded content, not a runtime condition.
func Templates(templateRoomLocalpart string) ([]Template, error) {
	entries, err := templateFiles.ReadDir("template")
	if err != nil {
		return nil, fmt.Errorf("reading embedded template directory: %w", err)
	}

	var templates []Template
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) != ".jsonc" {
			continue
		}

		path := "template/" + entry.Name()
		data, err := templateFiles.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading embedded template %s: %w", path, err)
		}

		// Hash the raw source before any substitution for reproducibility.
		hash := sha256.Sum256(data)

		// Strip JSONC comments and trailing commas, then substitute the
		// template room localpart into inherits references.
		stripped := jsonc.ToJSON(data)
		substituted := strings.ReplaceAll(string(stripped), "${TEMPLATE_ROOM}", templateRoomLocalpart)

		var content schema.TemplateContent
		if err := json.Unmarshal([]byte(substituted), &content); err != nil {
			return nil, fmt.Errorf("parsing embedded template %s: %w", path, err)
		}

		if content.Description == "" {
			return nil, fmt.Errorf("embedded template %s: description is required", path)
		}

		name := strings.TrimSuffix(entry.Name(), ".jsonc")

		templates = append(templates, Template{
			Name:       name,
			Content:    content,
			SourceHash: hex.EncodeToString(hash[:]),
		})
	}

	return templates, nil
}
