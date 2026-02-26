// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/pipelinedef"
	"github.com/bureau-foundation/bureau/lib/process"
	"github.com/bureau-foundation/bureau/lib/proxyclient"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/bureau-foundation/bureau/lib/schema/pipeline"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/messaging"
)

const (
	defaultProxySocket = "/run/bureau/proxy.sock"
	defaultTriggerPath = "/run/bureau/trigger.json"
)

func main() {
	if err := run(); err != nil {
		process.Fatal(err)
	}
}

func run() error {
	// Handle --version before any other checks.
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--version" {
		version.Print("bureau-pipeline-executor")
		return nil
	}

	// Refuse to run outside a sandbox. The default paths
	// (/run/bureau/proxy.sock, /run/bureau/service/ticket.sock) are
	// bind-mount destinations inside a bwrap namespace — outside a
	// sandbox they could point to another instance's socket or not
	// exist at all.
	if os.Getenv("BUREAU_SANDBOX") != "1" {
		return fmt.Errorf("bureau-pipeline-executor must run inside a Bureau sandbox (BUREAU_SANDBOX=1 not set)")
	}

	// Read ticket context from environment. The daemon sets these when
	// launching the executor sandbox after creating the pip- ticket.
	ticketID := os.Getenv("BUREAU_TICKET_ID")
	if ticketID == "" {
		return fmt.Errorf("BUREAU_TICKET_ID not set: executor requires ticket context")
	}
	ticketRoom := os.Getenv("BUREAU_TICKET_ROOM")
	if ticketRoom == "" {
		return fmt.Errorf("BUREAU_TICKET_ROOM not set: executor requires ticket context")
	}

	ctx := context.Background()

	// Create proxy client — still needed for pipeline ref resolution
	// (room alias → room ID → state event) and Matrix operations
	// (thread logging, state event publishing).
	proxySocketPath := os.Getenv("BUREAU_PROXY_SOCKET")
	if proxySocketPath == "" {
		proxySocketPath = defaultProxySocket
	}
	proxy := proxyclient.New(proxySocketPath, ref.ServerName{})

	if _, err := proxy.DiscoverServerName(ctx); err != nil {
		return fmt.Errorf("proxy connection failed (is the proxy running at %s?): %w", proxySocketPath, err)
	}

	identity, err := proxy.Identity(ctx)
	if err != nil {
		return fmt.Errorf("proxy identity: %w", err)
	}
	identityUserID, err := ref.ParseUserID(identity.UserID)
	if err != nil {
		return fmt.Errorf("parse identity user ID: %w", err)
	}
	session := proxyclient.NewProxySession(proxy, identityUserID)

	// Create ticket service client. The daemon bind-mounts the ticket
	// socket and token into the sandbox alongside the proxy socket.
	ticketSocketPath := os.Getenv("BUREAU_TICKET_SOCKET")
	if ticketSocketPath == "" {
		ticketSocketPath = defaultTicketSocket
	}
	ticketTokenPath := os.Getenv("BUREAU_TICKET_TOKEN")
	if ticketTokenPath == "" {
		ticketTokenPath = defaultTicketTokenPath
	}
	ticketClient, err := service.NewServiceClient(ticketSocketPath, ticketTokenPath)
	if err != nil {
		return fmt.Errorf("creating ticket service client: %w", err)
	}

	// Read trigger.json as TicketContent. The daemon writes the full
	// ticket content (the m.bureau.ticket state event content) as
	// trigger.json so the executor can extract the pipeline definition
	// reference and execution variables.
	triggerPath := os.Getenv("BUREAU_TRIGGER_PATH")
	if triggerPath == "" {
		triggerPath = defaultTriggerPath
	}
	triggerData, err := os.ReadFile(triggerPath)
	if err != nil {
		return fmt.Errorf("reading trigger file %s: %w", triggerPath, err)
	}

	var ticketContent ticket.TicketContent
	if err := json.Unmarshal(triggerData, &ticketContent); err != nil {
		return fmt.Errorf("parsing trigger file as TicketContent: %w", err)
	}
	if ticketContent.Type != ticket.TypePipeline {
		return fmt.Errorf("trigger ticket type is %q, expected %q", ticketContent.Type, ticket.TypePipeline)
	}
	if ticketContent.Pipeline == nil {
		return fmt.Errorf("trigger ticket has type \"pipeline\" but no pipeline content")
	}
	pipelineRef := ticketContent.Pipeline.PipelineRef
	if pipelineRef == "" {
		return fmt.Errorf("trigger ticket pipeline content has empty pipeline_ref")
	}

	// The pipeline executor runs exclusively inside bwrap sandboxes
	// where stderr is captured by the daemon — always use JSONHandler,
	// never interactive TextHandler.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With(
		"ticket_id", ticketID,
		"ticket_room", ticketRoom,
		"pipeline_ref", pipelineRef,
	)
	slog.SetDefault(logger)

	// Claim the ticket — atomically transition from "open" to
	// "in_progress" with ourselves as the assignee. On contention
	// (ticket already in_progress), exit cleanly: another executor
	// already claimed this ticket.
	if err := claimTicket(ctx, ticketClient, ticketID, ticketRoom, identity.UserID); err != nil {
		if isContention(err) {
			logger.Info("ticket already claimed, exiting cleanly")
			return nil
		}
		return fmt.Errorf("claiming ticket %s: %w", ticketID, err)
	}
	logger.Info("claimed ticket")

	// Resolve pipeline definition from the ticket's pipeline ref.
	name, content, err := resolvePipelineRef(ctx, pipelineRef, proxy)
	if err != nil {
		return fmt.Errorf("resolving pipeline: %w", err)
	}
	logger = logger.With("pipeline", name)

	// Validate.
	issues := pipelinedef.Validate(content)
	if len(issues) > 0 {
		return fmt.Errorf("pipeline %q has validation errors:\n  %s", name, strings.Join(issues, "\n  "))
	}

	// Update ticket with total_steps now that we know the pipeline
	// definition. Best-effort: log warning on failure, continue.
	pipelineExecution := ticket.PipelineExecutionContent{
		PipelineRef: pipelineRef,
		Variables:   ticketContent.Pipeline.Variables,
		TotalSteps:  len(content.Steps),
	}
	if updateError := updateTicketProgress(ctx, ticketClient, ticketID, ticketRoom, pipelineExecution); updateError != nil {
		logger.Warn("failed to update ticket with total_steps", "error", updateError)
	}

	// Load trigger variables — the top-level TicketContent fields
	// become EVENT_-prefixed variables (EVENT_status, EVENT_title,
	// EVENT_type, etc.) for use in pipeline variable expressions.
	triggerVariables, err := loadTriggerVariables(triggerPath)
	if err != nil {
		return fmt.Errorf("loading trigger variables: %w", err)
	}

	// Merge: trigger variables (EVENT_ prefix, lower priority) first,
	// then ticket pipeline variables (explicit per-execution config)
	// on top. ResolveVariables applies the final precedence:
	// declarations < mergedVariables < environment.
	ticketVariables := ticketContent.Pipeline.Variables
	mergedVariables := make(map[string]string, len(triggerVariables)+len(ticketVariables))
	for key, value := range triggerVariables {
		mergedVariables[key] = value
	}
	for key, value := range ticketVariables {
		mergedVariables[key] = value
	}

	variables, err := pipelinedef.ResolveVariables(content.Variables, mergedVariables, os.Getenv)
	if err != nil {
		return err
	}

	// Set up thread logging if configured.
	var threadLog *threadLogger
	if content.Log != nil {
		logRoom, logRoomError := pipelinedef.Expand(content.Log.Room, variables)
		if logRoomError != nil {
			return fmt.Errorf("expanding log.room: %w", logRoomError)
		}
		threadLog, err = newThreadLogger(ctx, session, logRoom, name, len(content.Steps), logger)
		if err != nil {
			return fmt.Errorf("creating pipeline log thread: %w", err)
		}
	}

	// Set up JSONL result log.
	var results *resultLog
	if resultPath := os.Getenv("BUREAU_RESULT_PATH"); resultPath != "" {
		results, err = newResultLog(resultPath, logger)
		if err != nil {
			return fmt.Errorf("creating result log: %w", err)
		}
		defer results.Close()
	}

	// Create artifact client if the artifact service socket is available.
	var artifacts *artifactstore.Client
	if artifactSocket := os.Getenv("BUREAU_ARTIFACT_SOCKET"); artifactSocket != "" {
		artifactToken := os.Getenv("BUREAU_ARTIFACT_TOKEN")
		if artifactToken == "" {
			return fmt.Errorf("BUREAU_ARTIFACT_SOCKET is set but BUREAU_ARTIFACT_TOKEN is not")
		}
		artifacts, err = artifactstore.NewClient(artifactSocket, artifactToken)
		if err != nil {
			return fmt.Errorf("creating artifact client: %w", err)
		}
	}

	// Create clock for time-dependent operations (cancellation
	// watcher polling, close retry backoff).
	clk := clock.Real()

	// Start cancellation watcher. A background goroutine polls the
	// ticket status every 5 seconds. When the ticket is closed
	// externally (by an operator or system), the goroutine cancels
	// the step context, which triggers SIGKILL on the running process
	// group via cmd.Cancel in runShellCommand.
	stepContext, cancelSteps := context.WithCancel(ctx)
	defer cancelSteps()
	var cancelled atomic.Bool
	go watchForCancellation(stepContext, clk, ticketClient, ticketID, ticketRoom, cancelSteps, &cancelled, cancellationPollInterval, logger)

	// Execute steps.
	logger.Info("pipeline starting", "steps", len(content.Steps))
	pipelineStart := time.Now()
	results.writeStart(name, len(content.Steps))

	var stepResults []pipeline.PipelineStepResult

	for index, step := range content.Steps {
		// Check for external cancellation between steps.
		if cancelled.Load() {
			break
		}

		// Update ticket progress before each step. Best-effort.
		pipelineExecution.CurrentStep = index + 1
		pipelineExecution.CurrentStepName = step.Name
		if updateError := updateTicketProgress(ctx, ticketClient, ticketID, ticketRoom, pipelineExecution); updateError != nil {
			logger.Warn("failed to update ticket progress", "error", updateError)
		}

		expandedStep, err := pipelinedef.ExpandStep(step, variables)
		if err != nil {
			totalDuration := time.Since(pipelineStart)
			results.writeFailed(step.Name, err.Error(),
				totalDuration.Milliseconds(), threadLog.logEventID())
			threadLog.logFailed(ctx, name, step.Name, err)

			pipelineExecution.Conclusion = pipeline.ConclusionFailure
			updateTicketConclusion(ctx, ticketClient, ticketID, ticketRoom, pipelineExecution, logger)
			closeTicketBestEffort(ctx, clk, ticketClient, ticketID, ticketRoom,
				fmt.Sprintf("Step %q failed: variable expansion: %v", step.Name, err), logger)

			publishPipelineResult(ctx, session, threadLog, name, ticketID, len(content.Steps),
				pipeline.ConclusionFailure, pipelineStart, totalDuration, stepResults, nil,
				step.Name, err.Error(), logger)
			return fmt.Errorf("expanding step %q: %w", step.Name, err)
		}

		result := executeStep(stepContext, expandedStep, index, len(content.Steps), session, artifacts, threadLog, logger)

		// Post step completion note to the ticket. Best-effort.
		noteBody := formatStepNote(index, len(content.Steps), expandedStep.Name, result)
		if noteError := addTicketStepNote(ctx, ticketClient, ticketID, ticketRoom, noteBody); noteError != nil {
			logger.Warn("failed to add step note", "error", noteError)
		}

		switch result.status {
		case pipeline.StepAborted:
			logger.Info("pipeline aborted", "step", expandedStep.Name, "error", result.err)
			threadLog.logAborted(ctx, name, expandedStep.Name, result.err)
			results.writeStep(index, expandedStep.Name, pipeline.StepAborted,
				result.duration.Milliseconds(), result.err.Error(), nil)
			results.writeAborted(expandedStep.Name, result.err.Error(),
				time.Since(pipelineStart).Milliseconds(), threadLog.logEventID())
			stepResults = append(stepResults, pipeline.PipelineStepResult{
				Name:       expandedStep.Name,
				Status:     pipeline.StepAborted,
				DurationMS: result.duration.Milliseconds(),
				Error:      result.err.Error(),
			})
			totalDuration := time.Since(pipelineStart)

			pipelineExecution.Conclusion = pipeline.ConclusionAborted
			updateTicketConclusion(ctx, ticketClient, ticketID, ticketRoom, pipelineExecution, logger)
			closeTicketBestEffort(ctx, clk, ticketClient, ticketID, ticketRoom,
				fmt.Sprintf("Pipeline aborted at step %q: %v", expandedStep.Name, result.err), logger)

			publishPipelineResult(ctx, session, threadLog, name, ticketID, len(content.Steps),
				pipeline.ConclusionAborted, pipelineStart, totalDuration, stepResults, nil,
				expandedStep.Name, result.err.Error(), logger)
			return nil

		case pipeline.StepFailed:
			if expandedStep.Optional {
				logger.Warn("optional step failed, continuing",
					"step", index+1, "total", len(content.Steps),
					"name", expandedStep.Name, "error", result.err)
				threadLog.logStep(ctx, index, len(content.Steps), expandedStep.Name,
					pipeline.StepFailedOptional, result.duration)
				results.writeStep(index, expandedStep.Name, pipeline.StepFailedOptional,
					result.duration.Milliseconds(), result.err.Error(), nil)
				stepResults = append(stepResults, pipeline.PipelineStepResult{
					Name:       expandedStep.Name,
					Status:     pipeline.StepFailedOptional,
					DurationMS: result.duration.Milliseconds(),
					Error:      result.err.Error(),
				})
			} else {
				logger.Error("step failed",
					"step", index+1, "total", len(content.Steps),
					"name", expandedStep.Name, "error", result.err)
				threadLog.logFailed(ctx, name, expandedStep.Name, result.err)
				results.writeStep(index, expandedStep.Name, pipeline.StepFailed,
					result.duration.Milliseconds(), result.err.Error(), nil)
				stepResults = append(stepResults, pipeline.PipelineStepResult{
					Name:       expandedStep.Name,
					Status:     pipeline.StepFailed,
					DurationMS: result.duration.Milliseconds(),
					Error:      result.err.Error(),
				})

				runOnFailureSteps(ctx, content.OnFailure, variables,
					expandedStep.Name, result.err, session, artifacts, threadLog, results, logger)

				totalDuration := time.Since(pipelineStart)
				results.writeFailed(expandedStep.Name, result.err.Error(),
					totalDuration.Milliseconds(), threadLog.logEventID())

				pipelineExecution.Conclusion = pipeline.ConclusionFailure
				updateTicketConclusion(ctx, ticketClient, ticketID, ticketRoom, pipelineExecution, logger)
				closeTicketBestEffort(ctx, clk, ticketClient, ticketID, ticketRoom,
					fmt.Sprintf("Step %q failed: %v", expandedStep.Name, result.err), logger)

				publishPipelineResult(ctx, session, threadLog, name, ticketID, len(content.Steps),
					pipeline.ConclusionFailure, pipelineStart, totalDuration, stepResults, nil,
					expandedStep.Name, result.err.Error(), logger)
				return fmt.Errorf("step %q failed: %w", expandedStep.Name, result.err)
			}

		default:
			results.writeStep(index, expandedStep.Name, result.status,
				result.duration.Milliseconds(), "", result.outputs)
			stepResults = append(stepResults, pipeline.PipelineStepResult{
				Name:       expandedStep.Name,
				Status:     result.status,
				DurationMS: result.duration.Milliseconds(),
				Outputs:    result.outputs,
			})

			if len(result.outputs) > 0 {
				stepPrefix := "OUTPUT_" + strings.ReplaceAll(expandedStep.Name, "-", "_")
				for outputName, outputValue := range result.outputs {
					variableName := stepPrefix + "_" + outputName
					variables[variableName] = outputValue
				}

				// Attach artifact outputs to the pipeline ticket.
				parsedOutputs, parseError := pipeline.ParseStepOutputs(expandedStep.Outputs)
				if parseError != nil {
					logger.Warn("failed to parse step outputs for attachment",
						"step", expandedStep.Name, "error", parseError)
				} else {
					for outputName, outputValue := range result.outputs {
						outputDeclaration, declared := parsedOutputs[outputName]
						if !declared || !outputDeclaration.Artifact {
							continue
						}
						if !strings.HasPrefix(outputValue, "art-") {
							continue
						}
						attachLabel := expandedStep.Name + "/" + outputName
						if attachError := addTicketAttachment(ctx, ticketClient,
							ticketID, ticketRoom, outputValue,
							attachLabel, outputDeclaration.ContentType); attachError != nil {
							logger.Warn("failed to attach artifact output to ticket",
								"step", expandedStep.Name,
								"output", outputName,
								"ref", outputValue,
								"error", attachError)
						}
					}
				}
			}
		}
	}

	// Handle external cancellation detected between or during steps.
	if cancelled.Load() {
		totalDuration := time.Since(pipelineStart)
		logger.Info("pipeline cancelled externally", "duration", formatDuration(totalDuration))
		threadLog.logAborted(ctx, name, "", fmt.Errorf("ticket closed externally"))
		results.writeAborted("", "ticket closed externally",
			totalDuration.Milliseconds(), threadLog.logEventID())

		pipelineExecution.Conclusion = pipeline.ConclusionCancelled
		updateTicketConclusion(ctx, ticketClient, ticketID, ticketRoom, pipelineExecution, logger)
		// Ticket is already closed (that's how we detected cancellation).

		publishPipelineResult(ctx, session, threadLog, name, ticketID, len(content.Steps),
			pipeline.ConclusionCancelled, pipelineStart, totalDuration, stepResults, nil,
			"", "ticket closed externally", logger)
		return nil
	}

	// Resolve pipeline-level output declarations.
	var pipelineOutputs map[string]string
	if len(content.Outputs) > 0 {
		pipelineOutputs = make(map[string]string, len(content.Outputs))
		for outputName, declaration := range content.Outputs {
			value, expandError := pipelinedef.Expand(declaration.Value, variables)
			if expandError != nil {
				totalDuration := time.Since(pipelineStart)
				results.writeFailed("", fmt.Sprintf("resolving pipeline output %q: %v", outputName, expandError),
					totalDuration.Milliseconds(), threadLog.logEventID())

				pipelineExecution.Conclusion = pipeline.ConclusionFailure
				updateTicketConclusion(ctx, ticketClient, ticketID, ticketRoom, pipelineExecution, logger)
				closeTicketBestEffort(ctx, clk, ticketClient, ticketID, ticketRoom,
					fmt.Sprintf("Failed to resolve pipeline output %q: %v", outputName, expandError), logger)

				publishPipelineResult(ctx, session, threadLog, name, ticketID, len(content.Steps),
					pipeline.ConclusionFailure, pipelineStart, totalDuration, stepResults, nil,
					"", fmt.Sprintf("resolving pipeline output %q: %v", outputName, expandError), logger)
				return fmt.Errorf("resolving pipeline output %q: %w", outputName, expandError)
			}
			pipelineOutputs[outputName] = value
		}
	}

	totalDuration := time.Since(pipelineStart)
	logger.Info("pipeline complete", "duration", formatDuration(totalDuration))
	threadLog.logComplete(ctx, name, totalDuration)
	results.writeComplete(totalDuration.Milliseconds(), threadLog.logEventID(), pipelineOutputs)

	pipelineExecution.Conclusion = pipeline.ConclusionSuccess
	updateTicketConclusion(ctx, ticketClient, ticketID, ticketRoom, pipelineExecution, logger)
	closeTicketBestEffort(ctx, clk, ticketClient, ticketID, ticketRoom,
		fmt.Sprintf("Pipeline completed successfully in %s", formatDuration(totalDuration)), logger)

	publishPipelineResult(ctx, session, threadLog, name, ticketID, len(content.Steps),
		pipeline.ConclusionSuccess, pipelineStart, totalDuration, stepResults, pipelineOutputs,
		"", "", logger)
	return nil
}

// updateTicketConclusion updates the ticket's pipeline execution content
// with the terminal conclusion. Important (one retry): the conclusion
// is the pipeline's most valuable status signal. If it fails, the close
// reason carries the conclusion as fallback text.
func updateTicketConclusion(ctx context.Context, client *service.ServiceClient, ticketID, roomID string, pipelineContent ticket.PipelineExecutionContent, logger *slog.Logger) {
	if err := updateTicketProgress(ctx, client, ticketID, roomID, pipelineContent); err != nil {
		logger.Warn("failed to update conclusion", "attempt", 1, "error", err)
		// One retry for conclusion updates.
		if retryError := updateTicketProgress(ctx, client, ticketID, roomID, pipelineContent); retryError != nil {
			logger.Warn("failed to update conclusion", "attempt", 2, "error", retryError)
		}
	}
}

// closeTicketBestEffort closes the ticket with retry logic, logging
// warnings on failure. Close is critical — the ticket must reflect
// the pipeline's terminal state.
func closeTicketBestEffort(ctx context.Context, clk clock.Clock, client *service.ServiceClient, ticketID, roomID, reason string, logger *slog.Logger) {
	if err := closeTicket(ctx, clk, client, ticketID, roomID, reason, logger); err != nil {
		logger.Warn("failed to close ticket", "error", err)
	}
}

// formatStepNote formats a step outcome as a human-readable note body
// for the ticket. The format matches the thread log step format.
func formatStepNote(index, total int, name string, result stepResult) string {
	switch result.status {
	case pipeline.StepOK:
		return fmt.Sprintf("step %d/%d: %s... ok (%s)", index+1, total, name, formatDuration(result.duration))
	case pipeline.StepSkipped:
		return fmt.Sprintf("step %d/%d: %s... skipped", index+1, total, name)
	case pipeline.StepFailed:
		return fmt.Sprintf("step %d/%d: %s... failed (%s): %v", index+1, total, name, formatDuration(result.duration), result.err)
	case pipeline.StepAborted:
		return fmt.Sprintf("step %d/%d: %s... aborted: %v", index+1, total, name, result.err)
	default:
		return fmt.Sprintf("step %d/%d: %s... %s (%s)", index+1, total, name, result.status, formatDuration(result.duration))
	}
}

// publishPipelineResult publishes a PipelineResultContent state event to
// the pipeline's log room. This is the Matrix-native record of pipeline
// execution that the ticket service watches to evaluate pipeline gates.
//
// Publishing is best-effort: if the log room is not configured (logger
// is nil) or the putState call fails, a warning is printed but the
// pipeline's own exit status is not affected.
func publishPipelineResult(
	ctx context.Context,
	session messaging.Session,
	threadLog *threadLogger,
	pipelineName string,
	ticketID string,
	stepCount int,
	conclusion pipeline.PipelineConclusion,
	startedAt time.Time,
	totalDuration time.Duration,
	stepResults []pipeline.PipelineStepResult,
	outputs map[string]string,
	failedStep string,
	errorMessage string,
	logger *slog.Logger,
) {
	roomID := threadLog.logRoomID()
	if roomID.IsZero() {
		return
	}

	result := pipeline.PipelineResultContent{
		Version:      pipeline.PipelineResultContentVersion,
		PipelineRef:  pipelineName,
		TicketID:     ticketID,
		Conclusion:   conclusion,
		StartedAt:    startedAt.UTC().Format(time.RFC3339),
		CompletedAt:  time.Now().UTC().Format(time.RFC3339),
		DurationMS:   totalDuration.Milliseconds(),
		StepCount:    stepCount,
		StepResults:  stepResults,
		Outputs:      outputs,
		FailedStep:   failedStep,
		ErrorMessage: errorMessage,
		LogEventID:   threadLog.logEventID(),
	}

	if _, err := session.SendStateEvent(ctx, roomID, schema.EventTypePipelineResult, pipelineName, result); err != nil {
		logger.Warn("failed to publish pipeline result event", "error", err)
	}
}

// runOnFailureSteps executes the on_failure steps after a pipeline
// failure. These steps run with the same variables as the main
// pipeline, plus two additional variables:
//
//   - FAILED_STEP: the name of the step that failed
//   - FAILED_ERROR: the error message from the failed step
//
// All on_failure steps are best-effort: if one fails, the error is
// logged and execution continues with the remaining steps.
//
// on_failure steps are NOT run on abort — abort means nothing went
// wrong, the work was simply no longer needed.
func runOnFailureSteps(
	ctx context.Context,
	steps []pipeline.PipelineStep,
	variables map[string]string,
	failedStepName string,
	failedError error,
	session messaging.Session,
	artifacts *artifactstore.Client,
	threadLog *threadLogger,
	results *resultLog,
	logger *slog.Logger,
) {
	if len(steps) == 0 {
		return
	}

	failureVariables := make(map[string]string, len(variables)+2)
	for key, value := range variables {
		failureVariables[key] = value
	}
	failureVariables["FAILED_STEP"] = failedStepName
	failureVariables["FAILED_ERROR"] = failedError.Error()

	logger.Info("running on_failure steps", "count", len(steps))

	for index, step := range steps {
		expandedStep, err := pipelinedef.ExpandStep(step, failureVariables)
		if err != nil {
			logger.Warn("on_failure step expansion failed", "index", index, "name", step.Name, "error", err)
			continue
		}

		result := executeStep(ctx, expandedStep, index, len(steps), session, artifacts, threadLog, logger)

		switch result.status {
		case pipeline.StepOK, pipeline.StepSkipped:
			logger.Info("on_failure step completed", "index", index, "name", expandedStep.Name, "status", result.status)
		case pipeline.StepFailed:
			logger.Warn("on_failure step failed, continuing", "index", index, "name", expandedStep.Name, "error", result.err)
		case pipeline.StepAborted:
			logger.Warn("on_failure step aborted, continuing", "index", index, "name", expandedStep.Name, "error", result.err)
		}

		results.writeStep(index, "on_failure:"+expandedStep.Name, result.status,
			result.duration.Milliseconds(), "", nil)
	}
}

// resolvePipelineRef parses a pipeline ref string, constructs the room
// alias, resolves it via the proxy, and reads the pipeline state event.
func resolvePipelineRef(ctx context.Context, pipelineRefString string, proxy *proxyclient.Client) (string, *pipeline.PipelineContent, error) {
	templateRef, err := schema.ParseTemplateRef(pipelineRefString)
	if err != nil {
		return "", nil, fmt.Errorf("parsing pipeline ref %q: %w", pipelineRefString, err)
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

	content, err := pipelinedef.Parse(stateContent)
	if err != nil {
		return "", nil, fmt.Errorf("parsing pipeline %q: %w", templateRef.Template, err)
	}

	return templateRef.Template, content, nil
}

// loadTriggerVariables reads the trigger JSON file and converts its
// top-level values to a string map with keys prefixed by "EVENT_".
// This enables pipelines to reference trigger event fields via
// ${EVENT_status}, ${EVENT_title}, ${EVENT_type}, etc.
//
// Value conversion:
//   - string → pass through
//   - number → format without trailing zeros
//   - bool → "true" or "false"
//   - object/array → JSON-stringify
//   - null → skip
//
// Returns an empty map (not an error) if the trigger file does not
// exist.
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
			encoded, err := json.Marshal(typed)
			if err != nil {
				return nil, fmt.Errorf("trigger key %q: %w", key, err)
			}
			variables[variableName] = string(encoded)
		}
	}

	return variables, nil
}
