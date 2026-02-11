// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package content provides embedded Bureau content definitions (pipelines,
// and eventually templates). Content files are JSONC (JSON with comments
// and trailing commas) — the same wire format as Matrix state events plus
// comments for human documentation.
//
// Files are embedded at compile time via go:embed. The primary consumers
// are "bureau matrix setup" (publishes content to Matrix rooms during
// homeserver bootstrap) and the pipeline executor (Nix fallback when
// Matrix is unreachable).
package content

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bureau-foundation/bureau/lib/pipeline"
	"github.com/bureau-foundation/bureau/lib/schema"
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
	Content schema.PipelineContent

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

		content, err := pipeline.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("parsing embedded pipeline %s: %w", path, err)
		}

		issues := pipeline.Validate(content)
		if len(issues) > 0 {
			return nil, fmt.Errorf("validating embedded pipeline %s: %s", path, strings.Join(issues, "; "))
		}

		hash := sha256.Sum256(data)

		pipelines = append(pipelines, Pipeline{
			Name:       pipeline.NameFromPath(entry.Name()),
			Content:    *content,
			SourceHash: hex.EncodeToString(hash[:]),
		})
	}

	return pipelines, nil
}
