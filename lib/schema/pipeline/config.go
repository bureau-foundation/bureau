// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// EventTypePipelineConfig enables and configures pipeline execution
// for a room. Rooms without this event are not eligible for pipeline
// execution — the daemon skips pip- tickets in unconfigured rooms.
// Published by the admin (via "bureau pipeline enable") to each room
// where pipeline execution should be available.
//
// State key: "" (singleton per room)
// Room: any room that wants pipeline execution
const EventTypePipelineConfig ref.EventType = "m.bureau.pipeline_config"

// PipelineConfigVersion is the current schema version for
// PipelineConfigContent events.
const PipelineConfigVersion = 1

// PipelineConfigContent is the content of m.bureau.pipeline_config
// state events. The presence of this event in a room enables pipeline
// execution for that room — the daemon checks for it before processing
// pip- tickets. Published by the admin (via "bureau pipeline enable")
// alongside power level configuration.
//
// The Version field future-proofs the schema. Additional fields
// (allowed pipelines, execution constraints, default variables) can be
// added in later versions as we discover what per-room pipeline
// configuration is actually needed.
type PipelineConfigContent struct {
	// Version is the schema version (see PipelineConfigVersion).
	Version int `json:"version"`
}

// Validate checks that the pipeline config is well-formed.
func (c PipelineConfigContent) Validate() error {
	if c.Version < 1 {
		return fmt.Errorf("pipeline config version must be >= 1, got %d", c.Version)
	}
	return nil
}

// CanModify returns true if the caller's code understands this version.
// Callers performing read-modify-write should check CanModify before
// writing back to avoid silently dropping fields added in newer versions.
func (c PipelineConfigContent) CanModify() bool {
	return c.Version <= PipelineConfigVersion
}
