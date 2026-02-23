// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"strings"
	"testing"

	pipelineschema "github.com/bureau-foundation/bureau/lib/schema/pipeline"
)

func TestListPipelines(t *testing.T) {

	state := newPipelineTestState()
	state.addPipelineRoom("#bureau/pipeline:test.local", "!pipeline:test", map[string]pipelineschema.PipelineContent{
		"dev-workspace-init": {
			Description: "Initialize a development workspace",
			Steps: []pipelineschema.PipelineStep{
				{Name: "clone", Run: "git clone ${REPO}"},
				{Name: "setup", Run: "make setup"},
			},
		},
		"deploy": {
			Description: "Deploy to production",
			Steps: []pipelineschema.PipelineStep{
				{Name: "build", Run: "make build"},
				{Name: "push", Run: "make push"},
				{Name: "rollout", Run: "make rollout"},
			},
		},
	})
	startTestServer(t, state)

	cmd := listCommand()
	// The command writes to stdout; we verify it doesn't error.
	// The --server-name flag must match the mock alias.
	if err := cmd.FlagSet().Parse([]string{"--server-name", "test.local"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	if err := cmd.Run(context.Background(), []string{"bureau/pipeline"}, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
}

func TestListPipelinesJSON(t *testing.T) {

	state := newPipelineTestState()
	state.addPipelineRoom("#bureau/pipeline:test.local", "!pipeline:test", map[string]pipelineschema.PipelineContent{
		"init": {
			Description: "Initialize",
			Steps: []pipelineschema.PipelineStep{
				{Name: "step1", Run: "echo hello"},
			},
		},
	})
	startTestServer(t, state)

	cmd := listCommand()
	if err := cmd.FlagSet().Parse([]string{"--server-name", "test.local", "--json"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	if err := cmd.Run(context.Background(), []string{"bureau/pipeline"}, nil); err != nil {
		t.Fatalf("list --json: %v", err)
	}
}

func TestListPipelinesEmptyRoom(t *testing.T) {

	state := newPipelineTestState()
	// Room exists but has no pipeline events.
	state.addPipelineRoom("#bureau/pipeline:test.local", "!pipeline:test", nil)
	startTestServer(t, state)

	cmd := listCommand()
	if err := cmd.FlagSet().Parse([]string{"--server-name", "test.local"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	// No pipelines — command should still succeed (prints "no pipelines found").
	if err := cmd.Run(context.Background(), []string{"bureau/pipeline"}, nil); err != nil {
		t.Fatalf("list empty room: %v", err)
	}
}

func TestListPipelinesRoomNotFound(t *testing.T) {

	state := newPipelineTestState()
	startTestServer(t, state)

	cmd := listCommand()
	if err := cmd.FlagSet().Parse([]string{"--server-name", "test.local"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	err := cmd.Run(context.Background(), []string{"nonexistent/room"}, nil)
	if err == nil {
		t.Fatal("expected error for nonexistent room")
	}
	if !strings.Contains(err.Error(), "resolving room alias") {
		t.Errorf("error %q should mention alias resolution", err.Error())
	}
}

func TestListPipelinesNoArgs(t *testing.T) {
	t.Parallel()

	cmd := listCommand()
	err := cmd.Run(context.Background(), []string{}, nil)
	if err == nil {
		t.Fatal("expected error for no args")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q should contain usage hint", err.Error())
	}
}
