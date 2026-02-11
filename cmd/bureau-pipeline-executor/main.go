// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/pipeline"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/version"
)

const (
	defaultProxySocket = "/run/bureau/proxy.sock"
	defaultPayloadPath = "/run/bureau/payload.json"
	eventTypePipeline  = "m.bureau.pipeline"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[pipeline] fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Parse arguments. Usage:
	//   bureau-pipeline-executor [--version] [pipeline-path-or-ref]
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println(version.Info())
		return nil
	}

	// Refuse to run outside a sandbox. The default paths
	// (/run/bureau/proxy.sock, /run/bureau/payload.json) are bind-mount
	// destinations inside a bwrap namespace — outside a sandbox they could
	// point to another instance's socket or not exist at all.
	if os.Getenv("BUREAU_SANDBOX") != "1" {
		return fmt.Errorf("bureau-pipeline-executor must run inside a Bureau sandbox (BUREAU_SANDBOX=1 not set)")
	}

	var argument string
	if len(args) > 0 {
		argument = args[0]
	}

	ctx := context.Background()

	// Create proxy client.
	socketPath := os.Getenv("BUREAU_PROXY_SOCKET")
	if socketPath == "" {
		socketPath = defaultProxySocket
	}
	proxy := newProxyClient(socketPath)

	// Whoami — establishes the server name needed for alias construction.
	if err := proxy.whoami(ctx); err != nil {
		return fmt.Errorf("proxy connection failed (is the proxy running at %s?): %w", socketPath, err)
	}

	// Resolve pipeline definition.
	payloadPath := os.Getenv("BUREAU_PAYLOAD_PATH")
	if payloadPath == "" {
		payloadPath = defaultPayloadPath
	}

	name, content, err := resolvePipeline(ctx, argument, payloadPath, proxy)
	if err != nil {
		return fmt.Errorf("resolving pipeline: %w", err)
	}

	// Validate.
	issues := pipeline.Validate(content)
	if len(issues) > 0 {
		return fmt.Errorf("pipeline %q has validation errors:\n  %s", name, strings.Join(issues, "\n  "))
	}

	// Load payload variables and resolve.
	payloadVariables, err := loadPayloadVariables(payloadPath)
	if err != nil {
		return fmt.Errorf("loading payload variables: %w", err)
	}

	variables, err := pipeline.ResolveVariables(content.Variables, payloadVariables, os.Getenv)
	if err != nil {
		return err
	}

	// Set up thread logging if configured.
	var logger *threadLogger
	if content.Log != nil {
		logRoom, err := pipeline.Expand(content.Log.Room, variables)
		if err != nil {
			return fmt.Errorf("expanding log.room: %w", err)
		}
		logger, err = newThreadLogger(ctx, proxy, logRoom, name, len(content.Steps))
		if err != nil {
			return fmt.Errorf("creating pipeline log thread: %w", err)
		}
	}

	// Execute steps.
	fmt.Printf("[pipeline] %s: starting (%d steps)\n", name, len(content.Steps))
	pipelineStart := time.Now()

	for index, step := range content.Steps {
		expandedStep, err := pipeline.ExpandStep(step, variables)
		if err != nil {
			logger.logFailed(ctx, name, step.Name, err)
			return fmt.Errorf("expanding step %q: %w", step.Name, err)
		}

		result := executeStep(ctx, expandedStep, index, len(content.Steps), proxy, logger)

		if result.status == "failed" {
			if expandedStep.Optional {
				fmt.Printf("[pipeline] step %d/%d: %s... failed (optional, continuing): %v\n",
					index+1, len(content.Steps), expandedStep.Name, result.err)
				logger.logStep(ctx, index, len(content.Steps), expandedStep.Name,
					"failed (optional)", result.duration)
			} else {
				fmt.Printf("[pipeline] step %d/%d: %s... failed: %v\n",
					index+1, len(content.Steps), expandedStep.Name, result.err)
				logger.logFailed(ctx, name, expandedStep.Name, result.err)
				return fmt.Errorf("step %q failed: %w", expandedStep.Name, result.err)
			}
		}
	}

	totalDuration := time.Since(pipelineStart)
	fmt.Printf("[pipeline] %s: complete (%s)\n", name, formatDuration(totalDuration))
	logger.logComplete(ctx, name, totalDuration)
	return nil
}

// resolvePipeline determines the pipeline definition from the available
// sources, in priority order:
//
//  1. CLI argument — file path (contains "/" or ends with .jsonc/.json)
//     or pipeline ref (contains ":")
//  2. Payload pipeline_ref — ref resolved via proxy
//  3. Payload pipeline_inline — inline PipelineContent JSON
//
// Returns the pipeline name and parsed content. Returns an error if no
// source provides a pipeline definition.
func resolvePipeline(ctx context.Context, argument string, payloadPath string, proxy *proxyClient) (string, *schema.PipelineContent, error) {
	// Tier 1: CLI argument.
	if argument != "" {
		if isFilePath(argument) {
			content, err := pipeline.ReadFile(argument)
			if err != nil {
				return "", nil, err
			}
			return pipeline.NameFromPath(argument), content, nil
		}
		// Pipeline ref.
		return resolvePipelineRef(ctx, argument, proxy)
	}

	// Tier 2 & 3: Payload keys.
	payloadData, err := os.ReadFile(payloadPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, fmt.Errorf("no pipeline specified: no CLI argument and no payload file at %s", payloadPath)
		}
		return "", nil, fmt.Errorf("reading payload: %w", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadData, &payload); err != nil {
		return "", nil, fmt.Errorf("parsing payload: %w", err)
	}

	// Tier 2: pipeline_ref in payload.
	if ref, ok := payload["pipeline_ref"].(string); ok && ref != "" {
		return resolvePipelineRef(ctx, ref, proxy)
	}

	// Tier 3: pipeline_inline in payload.
	if inline, ok := payload["pipeline_inline"]; ok {
		inlineJSON, err := json.Marshal(inline)
		if err != nil {
			return "", nil, fmt.Errorf("marshaling pipeline_inline: %w", err)
		}
		content, err := pipeline.Parse(inlineJSON)
		if err != nil {
			return "", nil, fmt.Errorf("parsing pipeline_inline: %w", err)
		}
		return "inline", content, nil
	}

	return "", nil, fmt.Errorf("no pipeline specified: no CLI argument, no pipeline_ref or pipeline_inline in payload")
}

// resolvePipelineRef parses a pipeline ref string, constructs the room
// alias, resolves it via the proxy, and reads the pipeline state event.
func resolvePipelineRef(ctx context.Context, ref string, proxy *proxyClient) (string, *schema.PipelineContent, error) {
	templateRef, err := schema.ParseTemplateRef(ref)
	if err != nil {
		return "", nil, fmt.Errorf("parsing pipeline ref %q: %w", ref, err)
	}

	alias := templateRef.RoomAlias(proxy.serverName)
	roomID, err := proxy.resolveAlias(ctx, alias)
	if err != nil {
		return "", nil, fmt.Errorf("resolving pipeline room %q: %w", alias, err)
	}

	stateContent, err := proxy.getState(ctx, roomID, eventTypePipeline, templateRef.Template)
	if err != nil {
		return "", nil, fmt.Errorf("reading pipeline %q from room %s: %w", templateRef.Template, roomID, err)
	}

	content, err := pipeline.Parse(stateContent)
	if err != nil {
		return "", nil, fmt.Errorf("parsing pipeline %q: %w", templateRef.Template, err)
	}

	return templateRef.Template, content, nil
}

// isFilePath returns true if argument looks like a file path rather than a
// pipeline ref. Disambiguation rules, in priority order:
//
//   - Absolute or explicit relative paths (/, ./, ../) are always file paths.
//   - Known file extensions (.jsonc, .json) are always file paths.
//   - Arguments containing ":" that parse as a valid TemplateRef are pipeline
//     refs, not file paths. This handles refs like "bureau/pipeline:init"
//     whose room localpart contains slashes.
//   - Bare relative paths with slashes but no colon are file paths.
func isFilePath(argument string) bool {
	if strings.HasPrefix(argument, "/") || strings.HasPrefix(argument, "./") || strings.HasPrefix(argument, "../") {
		return true
	}
	if strings.HasSuffix(argument, ".jsonc") || strings.HasSuffix(argument, ".json") {
		return true
	}
	// Pipeline refs use colons as separators (room:template or
	// room@server:port:template). If the argument has a colon and
	// parses as a valid template ref, it's a ref, not a file path.
	if strings.Contains(argument, ":") {
		_, err := schema.ParseTemplateRef(argument)
		if err == nil {
			return false
		}
	}
	if strings.Contains(argument, "/") {
		return true
	}
	return false
}

// loadPayloadVariables reads the payload JSON file and converts its values
// to a string map suitable for pipeline variable resolution. Keys
// "pipeline_ref" and "pipeline_inline" are excluded (consumed by pipeline
// resolution, not pipeline variables).
//
// Value conversion:
//   - string → pass through
//   - number → format without trailing zeros
//   - bool → "true" or "false"
//   - object/array → JSON-stringify
//
// Returns an empty map (not an error) if the payload file does not exist.
func loadPayloadVariables(payloadPath string) (map[string]string, error) {
	data, err := os.ReadFile(payloadPath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", payloadPath, err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", payloadPath, err)
	}

	variables := make(map[string]string, len(raw))
	for key, value := range raw {
		// Skip keys consumed by pipeline resolution.
		if key == "pipeline_ref" || key == "pipeline_inline" {
			continue
		}

		switch typed := value.(type) {
		case string:
			variables[key] = typed
		case float64:
			variables[key] = strconv.FormatFloat(typed, 'f', -1, 64)
		case bool:
			variables[key] = strconv.FormatBool(typed)
		case nil:
			// Skip null values.
		default:
			// Objects and arrays: JSON-stringify.
			encoded, err := json.Marshal(typed)
			if err != nil {
				return nil, fmt.Errorf("payload key %q: %w", key, err)
			}
			variables[key] = string(encoded)
		}
	}

	return variables, nil
}
