// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/pipelinedef"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/lib/templatedef"
	"github.com/bureau-foundation/bureau/messaging"
)

// validatedTemplate holds a template that has been parsed, hashed,
// validated, and ref-constructed — ready to push.
type validatedTemplate struct {
	name    string
	ref     schema.TemplateRef
	content schema.TemplateContent
}

// validatedPipeline holds a pipeline that has been parsed, hashed,
// validated, and ref-constructed — ready to push.
type validatedPipeline struct {
	name    string
	ref     schema.PipelineRef
	content pipeline.PipelineContent
}

// ValidateFlakeTemplates loads and validates all templates from a flake's
// bureauTemplates.<system> output without publishing any. Returns the
// validated entries ready for publishing, or an error if any entry fails
// parsing, hashing, or validation. Returns nil, nil when the flake does
// not declare any templates.
//
// This separation from publishing ensures atomicity: compose can validate
// all templates AND pipelines before publishing either.
func ValidateFlakeTemplates(ctx context.Context, flakeRef, system string, namespace ref.Namespace, logger *slog.Logger) ([]validatedTemplate, error) {
	batch, err := cli.LoadBatchFromFlake(ctx, flakeRef, "bureauTemplates."+system, logger)
	if err != nil {
		return nil, fmt.Errorf("loading templates from flake: %w", err)
	}
	if batch == nil {
		return nil, nil
	}

	// Sort keys for deterministic validation order and log output.
	names := make([]string, 0, len(batch))
	for name := range batch {
		names = append(names, name)
	}
	sort.Strings(names)

	var validated []validatedTemplate

	for _, name := range names {
		source := batch[name]

		var content schema.TemplateContent
		if err := json.Unmarshal(source.Data, &content); err != nil {
			return nil, cli.Internal("parsing template %q from flake: %w", name, err).
				WithHint("Template content must use snake_case field names matching the TemplateContent JSON wire format.")
		}

		// Compute content hash and set origin for update tracking.
		contentCopy := content
		contentCopy.Origin = nil
		hash, err := cli.ComputeContentHash(contentCopy)
		if err != nil {
			return nil, cli.Internal("computing content hash for template %q: %w", name, err)
		}
		source.Origin.ContentHash = hash
		content.Origin = source.Origin

		// Validate — invalid content is a hard error.
		issues := templatedef.Validate(&content)
		if len(issues) > 0 {
			return nil, cli.Validation("template %q from flake has %d validation issue(s):\n- %s",
				name, len(issues), strings.Join(issues, "\n- "))
		}

		// Construct the template ref: namespace/template:name.
		templateRefString := namespace.TemplateRoomAliasLocalpart() + ":" + name
		templateRef, err := schema.ParseTemplateRef(templateRefString)
		if err != nil {
			return nil, cli.Internal("derived template ref %q is invalid: %w", templateRefString, err)
		}

		validated = append(validated, validatedTemplate{
			name:    name,
			ref:     templateRef,
			content: content,
		})
	}

	return validated, nil
}

// ValidateFlakePipelines loads and validates all pipelines from a flake's
// bureauPipelines output without publishing any. Returns the validated
// entries ready for publishing, or an error if any entry fails parsing,
// hashing, or validation. Returns nil, nil when the flake does not
// declare any pipelines.
func ValidateFlakePipelines(ctx context.Context, flakeRef string, namespace ref.Namespace, logger *slog.Logger) ([]validatedPipeline, error) {
	batch, err := cli.LoadBatchFromFlake(ctx, flakeRef, "bureauPipelines", logger)
	if err != nil {
		return nil, fmt.Errorf("loading pipelines from flake: %w", err)
	}
	if batch == nil {
		return nil, nil
	}

	// Sort keys for deterministic validation order and log output.
	names := make([]string, 0, len(batch))
	for name := range batch {
		names = append(names, name)
	}
	sort.Strings(names)

	var validated []validatedPipeline

	for _, name := range names {
		source := batch[name]

		var content pipeline.PipelineContent
		if err := json.Unmarshal(source.Data, &content); err != nil {
			return nil, cli.Internal("parsing pipeline %q from flake: %w", name, err).
				WithHint("Pipeline content must use snake_case field names matching the PipelineContent JSON wire format.")
		}

		// Compute content hash and set origin for update tracking.
		contentCopy := content
		contentCopy.Origin = nil
		hash, err := cli.ComputeContentHash(contentCopy)
		if err != nil {
			return nil, cli.Internal("computing content hash for pipeline %q: %w", name, err)
		}
		source.Origin.ContentHash = hash
		content.Origin = source.Origin

		// Validate — invalid content is a hard error.
		issues := pipelinedef.Validate(&content)
		if len(issues) > 0 {
			return nil, cli.Validation("pipeline %q from flake has %d validation issue(s):\n- %s",
				name, len(issues), strings.Join(issues, "\n- "))
		}

		// Construct the pipeline ref: namespace/pipeline:name.
		pipelineRefString := namespace.PipelineRoomAliasLocalpart() + ":" + name
		pipelineRef, err := schema.ParsePipelineRef(pipelineRefString)
		if err != nil {
			return nil, cli.Internal("derived pipeline ref %q is invalid: %w", pipelineRefString, err)
		}

		validated = append(validated, validatedPipeline{
			name:    name,
			ref:     pipelineRef,
			content: content,
		})
	}

	return validated, nil
}

// PublishFlakeTemplates validates and publishes templates from a flake's
// bureauTemplates.<system> output. All templates are validated before any
// are published, so a validation failure never leaves partial state.
// Returns the number of templates published.
func PublishFlakeTemplates(ctx context.Context, session messaging.Session, flakeRef, system string, namespace ref.Namespace, logger *slog.Logger) (int, error) {
	templates, err := ValidateFlakeTemplates(ctx, flakeRef, system, namespace, logger)
	if err != nil {
		return 0, err
	}

	return publishTemplates(ctx, session, namespace.Server(), templates, logger)
}

// PublishFlakePipelines validates and publishes pipelines from a flake's
// bureauPipelines output. All pipelines are validated before any are
// published. Returns the number of pipelines published.
func PublishFlakePipelines(ctx context.Context, session messaging.Session, flakeRef string, namespace ref.Namespace, logger *slog.Logger) (int, error) {
	pipelines, err := ValidateFlakePipelines(ctx, flakeRef, namespace, logger)
	if err != nil {
		return 0, err
	}

	return publishPipelines(ctx, session, namespace.Server(), pipelines, logger)
}

// publishTemplates pushes pre-validated templates to Matrix.
func publishTemplates(ctx context.Context, session messaging.Session, serverName ref.ServerName, templates []validatedTemplate, logger *slog.Logger) (int, error) {
	for index, entry := range templates {
		result, err := templatedef.Push(ctx, session, entry.ref, entry.content, serverName)
		if err != nil {
			return index, cli.Internal("publishing template %q: %w", entry.name, err)
		}

		logger.Info("published template from flake",
			"template", entry.name,
			"room", result.RoomAlias,
			"event_id", result.EventID,
		)
	}

	return len(templates), nil
}

// publishPipelines pushes pre-validated pipelines to Matrix.
func publishPipelines(ctx context.Context, session messaging.Session, serverName ref.ServerName, pipelines []validatedPipeline, logger *slog.Logger) (int, error) {
	for index, entry := range pipelines {
		result, err := pipelinedef.Push(ctx, session, entry.ref, entry.content, serverName)
		if err != nil {
			return index, cli.Internal("publishing pipeline %q: %w", entry.name, err)
		}

		logger.Info("published pipeline from flake",
			"pipeline", entry.name,
			"room", result.RoomAlias,
			"event_id", result.EventID,
		)
	}

	return len(pipelines), nil
}
