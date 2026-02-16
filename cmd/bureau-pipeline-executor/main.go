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
	"github.com/bureau-foundation/bureau/lib/proxyclient"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/version"
)

const (
	defaultProxySocket = "/run/bureau/proxy.sock"
	defaultPayloadPath = "/run/bureau/payload.json"
	defaultTriggerPath = "/run/bureau/trigger.json"
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
	proxy := proxyclient.New(socketPath, "")

	// Discover server name — needed for constructing room aliases.
	if _, err := proxy.DiscoverServerName(ctx); err != nil {
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

	// Load trigger variables (from the event that satisfied the principal's
	// StartCondition). These are prefixed with EVENT_ to avoid collisions
	// with payload variables. Trigger variables have lower priority than
	// payload variables: payload is explicit per-principal config, trigger
	// is ambient context from the launching event.
	triggerPath := os.Getenv("BUREAU_TRIGGER_PATH")
	if triggerPath == "" {
		triggerPath = defaultTriggerPath
	}
	triggerVariables, err := loadTriggerVariables(triggerPath)
	if err != nil {
		return fmt.Errorf("loading trigger variables: %w", err)
	}

	// Load payload variables.
	payloadVariables, err := loadPayloadVariables(payloadPath)
	if err != nil {
		return fmt.Errorf("loading payload variables: %w", err)
	}

	// Merge: trigger first (lower priority), then payload on top.
	// ResolveVariables applies: declarations < mergedVariables < environment.
	mergedVariables := make(map[string]string, len(triggerVariables)+len(payloadVariables))
	for key, value := range triggerVariables {
		mergedVariables[key] = value
	}
	for key, value := range payloadVariables {
		mergedVariables[key] = value
	}

	variables, err := pipeline.ResolveVariables(content.Variables, mergedVariables, os.Getenv)
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

	// Set up JSONL result log. The daemon reads this for structured
	// pipeline outcomes (step-level detail, duration, log thread link).
	// Controlled by BUREAU_RESULT_PATH — when not set, result logging
	// is disabled. The launcher sets this when spawning executor sandboxes.
	var results *resultLog
	if resultPath := os.Getenv("BUREAU_RESULT_PATH"); resultPath != "" {
		results, err = newResultLog(resultPath)
		if err != nil {
			return fmt.Errorf("creating result log: %w", err)
		}
		defer results.Close()
	}

	// Execute steps.
	fmt.Printf("[pipeline] %s: starting (%d steps)\n", name, len(content.Steps))
	pipelineStart := time.Now()
	results.writeStart(name, len(content.Steps))

	// Accumulate step outcomes for the pipeline result state event.
	var stepResults []schema.PipelineStepResult

	for index, step := range content.Steps {
		expandedStep, err := pipeline.ExpandStep(step, variables)
		if err != nil {
			totalDuration := time.Since(pipelineStart)
			results.writeFailed(step.Name, err.Error(),
				totalDuration.Milliseconds(), logger.logEventID())
			logger.logFailed(ctx, name, step.Name, err)
			publishPipelineResult(ctx, proxy, logger, name, len(content.Steps),
				"failure", pipelineStart, totalDuration, stepResults,
				step.Name, err.Error())
			return fmt.Errorf("expanding step %q: %w", step.Name, err)
		}

		result := executeStep(ctx, expandedStep, index, len(content.Steps), proxy, logger)

		switch result.status {
		case "aborted":
			// Clean exit — a precondition check determined this pipeline's
			// work is no longer needed. Not an error: the pipeline exits
			// with success and on_failure steps are NOT run (nothing failed).
			fmt.Printf("[pipeline] %s: aborted at step %q: %v\n", name, expandedStep.Name, result.err)
			logger.logAborted(ctx, name, expandedStep.Name, result.err)
			results.writeStep(index, expandedStep.Name, "aborted",
				result.duration.Milliseconds(), result.err.Error())
			results.writeAborted(expandedStep.Name, result.err.Error(),
				time.Since(pipelineStart).Milliseconds(), logger.logEventID())
			stepResults = append(stepResults, schema.PipelineStepResult{
				Name:       expandedStep.Name,
				Status:     "aborted",
				DurationMS: result.duration.Milliseconds(),
				Error:      result.err.Error(),
			})
			totalDuration := time.Since(pipelineStart)
			publishPipelineResult(ctx, proxy, logger, name, len(content.Steps),
				"aborted", pipelineStart, totalDuration, stepResults,
				expandedStep.Name, result.err.Error())
			return nil

		case "failed":
			if expandedStep.Optional {
				fmt.Printf("[pipeline] step %d/%d: %s... failed (optional, continuing): %v\n",
					index+1, len(content.Steps), expandedStep.Name, result.err)
				logger.logStep(ctx, index, len(content.Steps), expandedStep.Name,
					"failed (optional)", result.duration)
				results.writeStep(index, expandedStep.Name, "failed (optional)",
					result.duration.Milliseconds(), result.err.Error())
				stepResults = append(stepResults, schema.PipelineStepResult{
					Name:       expandedStep.Name,
					Status:     "failed (optional)",
					DurationMS: result.duration.Milliseconds(),
					Error:      result.err.Error(),
				})
			} else {
				fmt.Printf("[pipeline] step %d/%d: %s... failed: %v\n",
					index+1, len(content.Steps), expandedStep.Name, result.err)
				logger.logFailed(ctx, name, expandedStep.Name, result.err)
				results.writeStep(index, expandedStep.Name, "failed",
					result.duration.Milliseconds(), result.err.Error())
				stepResults = append(stepResults, schema.PipelineStepResult{
					Name:       expandedStep.Name,
					Status:     "failed",
					DurationMS: result.duration.Milliseconds(),
					Error:      result.err.Error(),
				})

				// Run on_failure steps before recording the terminal failure.
				// These are best-effort: their failures are logged but do not
				// change the pipeline's overall outcome.
				runOnFailureSteps(ctx, content.OnFailure, variables,
					expandedStep.Name, result.err, proxy, logger, results)

				totalDuration := time.Since(pipelineStart)
				results.writeFailed(expandedStep.Name, result.err.Error(),
					totalDuration.Milliseconds(), logger.logEventID())
				publishPipelineResult(ctx, proxy, logger, name, len(content.Steps),
					"failure", pipelineStart, totalDuration, stepResults,
					expandedStep.Name, result.err.Error())
				return fmt.Errorf("step %q failed: %w", expandedStep.Name, result.err)
			}

		default:
			results.writeStep(index, expandedStep.Name, result.status,
				result.duration.Milliseconds(), "")
			stepResults = append(stepResults, schema.PipelineStepResult{
				Name:       expandedStep.Name,
				Status:     result.status,
				DurationMS: result.duration.Milliseconds(),
			})
		}
	}

	totalDuration := time.Since(pipelineStart)
	fmt.Printf("[pipeline] %s: complete (%s)\n", name, formatDuration(totalDuration))
	logger.logComplete(ctx, name, totalDuration)
	results.writeComplete(totalDuration.Milliseconds(), logger.logEventID())
	publishPipelineResult(ctx, proxy, logger, name, len(content.Steps),
		"success", pipelineStart, totalDuration, stepResults,
		"", "")
	return nil
}

// publishPipelineResult publishes a PipelineResultContent state event to the
// pipeline's log room. This is the Matrix-native record of pipeline execution
// that the ticket service watches to evaluate pipeline gates.
//
// Publishing is best-effort: if the log room is not configured (logger is nil)
// or the putState call fails, a warning is printed but the pipeline's own
// exit status is not affected. The JSONL result log is the primary structured
// output for the daemon; the state event is for cross-service coordination.
func publishPipelineResult(
	ctx context.Context,
	proxy *proxyclient.Client,
	logger *threadLogger,
	pipelineName string,
	stepCount int,
	conclusion string,
	startedAt time.Time,
	totalDuration time.Duration,
	stepResults []schema.PipelineStepResult,
	failedStep string,
	errorMessage string,
) {
	roomID := logger.logRoomID()
	if roomID == "" {
		return
	}

	result := schema.PipelineResultContent{
		Version:      schema.PipelineResultContentVersion,
		PipelineRef:  pipelineName,
		Conclusion:   conclusion,
		StartedAt:    startedAt.UTC().Format(time.RFC3339),
		CompletedAt:  time.Now().UTC().Format(time.RFC3339),
		DurationMS:   totalDuration.Milliseconds(),
		StepCount:    stepCount,
		StepResults:  stepResults,
		FailedStep:   failedStep,
		ErrorMessage: errorMessage,
		LogEventID:   logger.logEventID(),
	}

	if _, err := proxy.PutState(ctx, proxyclient.PutStateRequest{
		Room:      roomID,
		EventType: schema.EventTypePipelineResult,
		StateKey:  pipelineName,
		Content:   result,
	}); err != nil {
		fmt.Printf("[pipeline] warning: failed to publish pipeline result event: %v\n", err)
	}
}

// runOnFailureSteps executes the on_failure steps after a pipeline failure.
// These steps run with the same variables as the main pipeline, plus two
// additional variables:
//
//   - FAILED_STEP: the name of the step that failed
//   - FAILED_ERROR: the error message from the failed step
//
// All on_failure steps are best-effort: if one fails, the error is logged
// and execution continues with the remaining steps. This prevents cascading
// failures from hiding the original error.
//
// on_failure steps are NOT run on abort (precondition mismatch) — abort means
// nothing went wrong, the work was simply no longer needed.
func runOnFailureSteps(
	ctx context.Context,
	steps []schema.PipelineStep,
	variables map[string]string,
	failedStepName string,
	failedError error,
	proxy *proxyclient.Client,
	logger *threadLogger,
	results *resultLog,
) {
	if len(steps) == 0 {
		return
	}

	// Build a copy of the variables with failure context added.
	failureVariables := make(map[string]string, len(variables)+2)
	for key, value := range variables {
		failureVariables[key] = value
	}
	failureVariables["FAILED_STEP"] = failedStepName
	failureVariables["FAILED_ERROR"] = failedError.Error()

	fmt.Printf("[pipeline] running %d on_failure steps\n", len(steps))

	for index, step := range steps {
		expandedStep, err := pipeline.ExpandStep(step, failureVariables)
		if err != nil {
			fmt.Printf("[pipeline] on_failure[%d] %q: expansion failed: %v\n", index, step.Name, err)
			continue
		}

		result := executeStep(ctx, expandedStep, index, len(steps), proxy, logger)

		switch result.status {
		case "ok", "skipped":
			fmt.Printf("[pipeline] on_failure[%d] %q: %s\n", index, expandedStep.Name, result.status)
		case "failed":
			fmt.Printf("[pipeline] on_failure[%d] %q: failed (continuing): %v\n", index, expandedStep.Name, result.err)
		case "aborted":
			fmt.Printf("[pipeline] on_failure[%d] %q: aborted (continuing): %v\n", index, expandedStep.Name, result.err)
		}

		results.writeStep(index, "on_failure:"+expandedStep.Name, result.status,
			result.duration.Milliseconds(), "")
	}
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
func resolvePipeline(ctx context.Context, argument string, payloadPath string, proxy *proxyclient.Client) (string, *schema.PipelineContent, error) {
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
func resolvePipelineRef(ctx context.Context, ref string, proxy *proxyclient.Client) (string, *schema.PipelineContent, error) {
	templateRef, err := schema.ParseTemplateRef(ref)
	if err != nil {
		return "", nil, fmt.Errorf("parsing pipeline ref %q: %w", ref, err)
	}

	alias := templateRef.RoomAlias(proxy.ServerName())
	roomID, err := proxy.ResolveAlias(ctx, alias)
	if err != nil {
		return "", nil, fmt.Errorf("resolving pipeline room %q: %w", alias, err)
	}

	stateContent, err := proxy.GetState(ctx, roomID, schema.EventTypePipeline, templateRef.Template)
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

// loadTriggerVariables reads the trigger JSON file (the content of the state
// event that satisfied the principal's StartCondition) and converts its
// top-level values to a string map with keys prefixed by "EVENT_". This
// enables pipelines to reference trigger event fields via ${EVENT_status},
// ${EVENT_teardown_mode}, ${EVENT_machine}, etc.
//
// Value conversion follows the same rules as loadPayloadVariables:
//   - string → pass through
//   - number → format without trailing zeros
//   - bool → "true" or "false"
//   - object/array → JSON-stringify
//   - null → skip
//
// Returns an empty map (not an error) if the trigger file does not exist.
// Principals without a StartCondition have no trigger.json.
func loadTriggerVariables(triggerPath string) (map[string]string, error) {
	data, err := os.ReadFile(triggerPath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", triggerPath, err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", triggerPath, err)
	}

	variables := make(map[string]string, len(raw))
	for key, value := range raw {
		variableName := "EVENT_" + key

		switch typed := value.(type) {
		case string:
			variables[variableName] = typed
		case float64:
			variables[variableName] = strconv.FormatFloat(typed, 'f', -1, 64)
		case bool:
			variables[variableName] = strconv.FormatBool(typed)
		case nil:
			// Skip null values.
		default:
			// Objects and arrays: JSON-stringify.
			encoded, err := json.Marshal(typed)
			if err != nil {
				return nil, fmt.Errorf("trigger key %q: %w", key, err)
			}
			variables[variableName] = string(encoded)
		}
	}

	return variables, nil
}
