// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"strings"
	"testing"

	pipelineschema "github.com/bureau-foundation/bureau/lib/schema/pipeline"
)

func TestShowPipeline(t *testing.T) {

	state := newPipelineTestState()
	state.addPipelineRoom("#bureau/pipeline:test.local", "!pipeline:test", map[string]pipelineschema.PipelineContent{
		"dev-workspace-init": {
			Description: "Initialize a development workspace",
			Variables: map[string]pipelineschema.PipelineVariable{
				"PROJECT": {Description: "project name", Required: true},
			},
			Steps: []pipelineschema.PipelineStep{
				{Name: "clone", Run: "git clone ${REPO}"},
				{Name: "setup", Run: "make setup", Timeout: "10m"},
			},
			Log: &pipelineschema.PipelineLog{Room: "#iree/general:bureau.local"},
		},
	})
	startTestServer(t, state)

	cmd := showCommand()
	if err := cmd.FlagSet().Parse([]string{"--server-name", "test.local"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	// The command prints JSON to stdout; we verify it doesn't error.
	if err := cmd.Run(context.Background(), []string{"bureau/pipeline:dev-workspace-init"}, testLogger()); err != nil {
		t.Fatalf("show: %v", err)
	}
}

func TestShowPipelineNotFound(t *testing.T) {

	state := newPipelineTestState()
	state.addPipelineRoom("#bureau/pipeline:test.local", "!pipeline:test", map[string]pipelineschema.PipelineContent{
		"exists": {
			Steps: []pipelineschema.PipelineStep{{Name: "x", Run: "echo"}},
		},
	})
	startTestServer(t, state)

	cmd := showCommand()
	if err := cmd.FlagSet().Parse([]string{"--server-name", "test.local"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{"bureau/pipeline:nonexistent"}, testLogger())
	if err == nil {
		t.Fatal("expected error for nonexistent pipeline")
	}
	if !strings.Contains(err.Error(), "fetching pipeline") {
		t.Errorf("error %q should mention fetching", err.Error())
	}
}

func TestShowPipelineBadRef(t *testing.T) {
	t.Parallel()

	cmd := showCommand()
	err := cmd.Run(context.Background(), []string{"no-colon-here"}, testLogger())
	if err == nil {
		t.Fatal("expected error for bad pipeline reference")
	}
	if !strings.Contains(err.Error(), "parsing pipeline reference") {
		t.Errorf("error %q should mention parsing", err.Error())
	}
}

func TestShowPipelineNoArgs(t *testing.T) {
	t.Parallel()

	cmd := showCommand()
	err := cmd.Run(context.Background(), []string{}, testLogger())
	if err == nil {
		t.Fatal("expected error for no args")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q should contain usage hint", err.Error())
	}
}
