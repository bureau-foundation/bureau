// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package agentdriver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/bureau-foundation/bureau/lib/proxyclient"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema/agent"
	"github.com/bureau-foundation/bureau/messaging"
)

// RunConfig holds the configuration for the agent lifecycle.
type RunConfig struct {
	// ProxySocketPath is the path to the Bureau proxy Unix socket.
	ProxySocketPath string

	// MachineName is the fleet-scoped machine localpart
	// (e.g., "bureau/fleet/prod/machine/workstation").
	MachineName string

	// ServerName is the Matrix server name (e.g., "bureau.local").
	ServerName string

	// SessionLogPath is the file path for the JSONL session log.
	// If empty, session logging is disabled.
	SessionLogPath string

	// WorkingDirectory is the directory the agent process starts in.
	// If empty, defaults to the current working directory.
	WorkingDirectory string

	// AgentServiceSocketPath is the path to the agent service socket.
	// If empty, agent service integration is disabled (session
	// tracking and metrics are skipped).
	AgentServiceSocketPath string

	// AgentServiceTokenPath is the path to the agent service token.
	// Required when AgentServiceSocketPath is set.
	AgentServiceTokenPath string

	// CheckpointFormat enables event-level checkpointing in the
	// event pump. When non-empty, the event pump checkpoints
	// []Event as CBOR deltas using this format identifier (e.g.,
	// "events-v1"). AgentServiceSocketPath must be set — checkpoint
	// data flows through the agent service, which stores artifacts
	// internally. When empty, no event checkpointing occurs.
	CheckpointFormat string

	// AgentTemplate is the agent template identifier included in
	// checkpoint metadata. Read from BUREAU_AGENT_TEMPLATE.
	AgentTemplate string

	// SelfHealStaleSessions controls whether Run() automatically
	// ends a stale active session (e.g., from a crashed sandbox) at
	// startup. When true, a stale session is ended with zero metrics
	// and a warning is logged. When false, Run() returns an error
	// explaining the stale session. Defaults to true (zero value of
	// bool is false, so callers must set this explicitly or use
	// RunConfigFromEnvironment which sets it to true).
	SelfHealStaleSessions bool

	// Logger is the structured logger for wrapper-level events.
	// If nil, a default stderr logger is used.
	Logger *slog.Logger
}

// RunConfigFromEnvironment reads RunConfig from standard Bureau environment
// variables. These are set by the template's EnvironmentVariables section
// and expanded by the launcher at sandbox creation time.
//
// Agent service integration is enabled automatically when the agent
// service socket exists at the default sandbox path. This happens when
// the template declares RequiredServices: ["agent"].
func RunConfigFromEnvironment() RunConfig {
	config := RunConfig{
		ProxySocketPath:       os.Getenv("BUREAU_PROXY_SOCKET"),
		MachineName:           os.Getenv("BUREAU_MACHINE_NAME"),
		ServerName:            os.Getenv("BUREAU_SERVER_NAME"),
		SelfHealStaleSessions: true,
	}

	// Auto-detect agent service when running inside a sandbox with
	// the agent service socket bind-mounted at the standard path.
	if _, err := os.Stat(DefaultAgentServiceSocketPath); err == nil {
		config.AgentServiceSocketPath = DefaultAgentServiceSocketPath
		config.AgentServiceTokenPath = DefaultAgentServiceTokenPath
	}

	config.AgentTemplate = os.Getenv("BUREAU_AGENT_TEMPLATE")

	return config
}

// Run is the main agent lifecycle function. It:
//  1. Creates a proxy client and verifies the proxy is reachable (Identity call).
//  2. Builds the agent context (identity, grants, services, payload, config room).
//  3. If agent service is configured: checks for stale sessions,
//     starts a new session in the agent service.
//  4. Writes the Bureau system prompt to a temp file.
//  5. Opens the session log writer.
//  6. Spawns the agent process via the Driver.
//  7. Pumps structured events from the driver into the session log.
//  8. Starts a message pump: long-polls Matrix /sync for incoming messages
//     in the config room and writes them to the agent's stdin.
//  9. Waits for the pump to capture its initial stream position.
//  10. Sends a "ready" message to the config room — this guarantees
//     the pump is listening and any message sent after "agent-ready"
//     will be delivered to the agent.
//  11. Handles SIGINT/SIGTERM for graceful shutdown.
//  12. Waits for the agent process to exit.
//  13. Posts a completion summary to the config room.
//  14. If agent service is configured: ends the session with metrics.
//  15. Closes the session log.
func Run(ctx context.Context, driver Driver, config RunConfig) error {
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	// Validate required config.
	if config.ProxySocketPath == "" {
		return fmt.Errorf("BUREAU_PROXY_SOCKET is required")
	}
	if config.MachineName == "" {
		return fmt.Errorf("BUREAU_MACHINE_NAME is required")
	}
	if config.ServerName == "" {
		return fmt.Errorf("BUREAU_SERVER_NAME is required")
	}

	// Validate event checkpoint infrastructure. Checkpoint data flows
	// through the agent service (which stores artifacts internally),
	// so the agent service socket is required when checkpointing is
	// enabled. Fail at startup, not at runtime.
	if config.CheckpointFormat != "" {
		if config.AgentServiceSocketPath == "" {
			return fmt.Errorf(
				"CheckpointFormat %q requires agent service socket "+
					"(AgentServiceSocketPath is empty)", config.CheckpointFormat)
		}
	}

	serverName, err := ref.ParseServerName(config.ServerName)
	if err != nil {
		return fmt.Errorf("invalid server name %q: %w", config.ServerName, err)
	}

	machine, err := ref.ParseMachine(config.MachineName, serverName)
	if err != nil {
		return fmt.Errorf("invalid machine name %q: %w", config.MachineName, err)
	}

	// Create proxy client and verify connectivity.
	proxy := proxyclient.New(config.ProxySocketPath, serverName)

	logger.Info("building agent context")
	agentContext, err := BuildContext(ctx, proxy, machine)
	if err != nil {
		return fmt.Errorf("building agent context: %w", err)
	}
	logger.Info("agent identity", "user_id", agentContext.Identity.UserID, "config_room", agentContext.ConfigRoomID)

	// Create a MatrixSession for operations that only need Matrix
	// access (message pump, text messages). BuildContext above used
	// the raw proxy client for proxy-specific operations (Identity,
	// Grants, Services).
	identityUserID, err := ref.ParseUserID(agentContext.Identity.UserID)
	if err != nil {
		return fmt.Errorf("parse identity user ID: %w", err)
	}
	session := proxyclient.NewProxySession(proxy, identityUserID)

	// Write system prompt to temp file.
	systemPromptFile, err := agentContext.WriteSystemPromptFile()
	if err != nil {
		return fmt.Errorf("writing system prompt: %w", err)
	}
	defer os.Remove(systemPromptFile)

	// Open session log.
	var sessionLog *SessionLogWriter
	if config.SessionLogPath != "" {
		sessionLog, err = NewSessionLogWriter(config.SessionLogPath)
		if err != nil {
			return fmt.Errorf("creating session log: %w", err)
		}
		defer sessionLog.Close()
		logger.Info("session log opened", "path", config.SessionLogPath)
	}

	// Generate session ID.
	sessionID := fmt.Sprintf("%s-%d", agentContext.Identity.UserID, time.Now().UnixMilli())

	// Agent service integration: session tracking and metrics.
	var agentServiceClient *AgentServiceClient
	if config.AgentServiceSocketPath != "" {
		if config.AgentServiceTokenPath == "" {
			return fmt.Errorf("AgentServiceTokenPath is required when AgentServiceSocketPath is set")
		}

		client, clientError := NewAgentServiceClient(
			config.AgentServiceSocketPath, config.AgentServiceTokenPath,
		)
		if clientError != nil {
			return fmt.Errorf("creating agent service client: %w", clientError)
		}
		agentServiceClient = client

		// Check for stale active sessions (e.g., from a crashed sandbox).
		currentSession, getError := agentServiceClient.GetSession(ctx, "")
		if getError != nil {
			return fmt.Errorf("checking current session state: %w", getError)
		}

		if currentSession != nil && currentSession.ActiveSessionID != "" {
			if !config.SelfHealStaleSessions {
				return fmt.Errorf(
					"stale active session %q exists (started at %s); "+
						"end it manually or set SelfHealStaleSessions=true",
					currentSession.ActiveSessionID,
					currentSession.ActiveSessionStartedAt,
				)
			}
			logger.Warn("clearing stale active session",
				"stale_session_id", currentSession.ActiveSessionID,
				"started_at", currentSession.ActiveSessionStartedAt,
			)
			healError := agentServiceClient.EndSession(ctx, EndSessionRequest{
				SessionID: currentSession.ActiveSessionID,
			})
			if healError != nil {
				return fmt.Errorf("clearing stale session %q: %w",
					currentSession.ActiveSessionID, healError)
			}
		}

		// Start the new session.
		if startError := agentServiceClient.StartSession(ctx, sessionID); startError != nil {
			return fmt.Errorf("starting agent service session: %w", startError)
		}
		logger.Info("agent service session started", "session_id", sessionID)
	}

	// Create event checkpoint tracker when checkpointing is enabled.
	// The tracker sends CBOR event deltas to the agent service, which
	// stores them as artifacts and records commit metadata. Created
	// after session start so the sessionID is available.
	var tracker *eventCheckpointTracker
	if config.CheckpointFormat != "" {
		tracker = newEventCheckpointTracker(
			agentServiceClient,
			config.CheckpointFormat,
			sessionID,
			config.AgentTemplate,
			logger,
		)
		logger.Info("event checkpoint tracker initialized",
			"format", config.CheckpointFormat,
			"template", config.AgentTemplate,
		)
	}

	// Determine initial prompt. If no prompt is configured in the
	// payload, pass empty to the driver — the agent loop waits for
	// a Matrix message instead of making a wasted LLM call.
	prompt := agentContext.TaskPrompt()

	// Log the initial prompt (skip if no prompt was configured).
	if sessionLog != nil && prompt != "" {
		sessionLog.Write(Event{
			Timestamp: time.Now(),
			Type:      EventTypePrompt,
			Prompt: &PromptEvent{
				Content: prompt,
				Source:  "initial",
			},
		})
	}

	// Determine working directory.
	workingDirectory := config.WorkingDirectory
	if workingDirectory == "" {
		workingDirectory, _ = os.Getwd()
	}

	// Start the agent process.
	logger.Info("starting agent process")
	driverConfig := DriverConfig{
		Prompt:           prompt,
		SystemPromptFile: systemPromptFile,
		SessionID:        sessionID,
		WorkingDirectory: workingDirectory,
	}
	process, stdout, err := driver.Start(ctx, driverConfig)
	if err != nil {
		return fmt.Errorf("starting agent process: %w", err)
	}
	defer stdout.Close()

	// Event pump: driver.ParseOutput → session log writer.
	eventsDone := make(chan struct{})
	events := make(chan Event, 64)

	// Consumer goroutine: drains the events channel into the session
	// log and, when enabled, into the event checkpoint tracker.
	go func() {
		defer close(eventsDone)
		for event := range events {
			if sessionLog != nil {
				if writeError := sessionLog.Write(event); writeError != nil {
					logger.Warn("writing session log event", "error", writeError)
				}
			}
			if tracker != nil {
				tracker.appendEvent(event)
				switch {
				case event.Type == EventTypeResponse:
					tracker.checkpointDelta(ctx, agent.CheckpointTurnBoundary)
				case event.Type == EventTypeSystem &&
					event.System != nil &&
					event.System.Subtype == "compact_boundary":
					tracker.checkpointDelta(ctx, agent.CheckpointCompaction)
				case event.Type == EventTypeMetric:
					tracker.checkpointDelta(ctx, agent.CheckpointSessionEnd)
				}
			}
		}
	}()

	// Producer goroutine: ParseOutput reads stdout and emits events.
	go func() {
		if parseError := driver.ParseOutput(ctx, stdout, events); parseError != nil {
			logger.Warn("parsing agent output", "error", parseError)
		}
		close(events)
	}()

	// Message pump: poll config room for incoming messages, write to stdin.
	// The pump must be started and listening BEFORE we send "agent-ready",
	// so that any message sent in response to agent-ready is captured.
	messagePumpCtx, cancelMessagePump := context.WithCancel(ctx)
	defer cancelMessagePump()

	pumpReady := make(chan struct{})
	go runMessagePump(messagePumpCtx, session, agentContext.ConfigRoomID, identityUserID, machine.UserID(), process.Stdin(), logger, pumpReady)

	// Wait for the pump to complete its initial /sync before announcing
	// readiness. This guarantees "agent-ready" means the pump is listening.
	select {
	case <-pumpReady:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Send ready message.
	logger.Info("sending ready message to config room")
	if _, err := session.SendMessage(ctx, agentContext.ConfigRoomID, messaging.NewTextMessage("agent-ready")); err != nil {
		logger.Warn("failed to send ready message", "error", err)
	}

	// Signal handling.
	signalChannel := make(chan os.Signal, 2)
	signal.Notify(signalChannel, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		count := 0
		for range signalChannel {
			count++
			if count == 1 {
				logger.Info("received signal, interrupting agent")
				if interruptError := driver.Interrupt(process); interruptError != nil {
					logger.Warn("interrupting agent", "error", interruptError)
				}
			} else {
				logger.Info("received second signal, killing agent")
				process.Signal(syscall.SIGKILL)
			}
		}
	}()

	// Wait for the agent process to exit.
	processError := process.Wait()
	cancelMessagePump()
	signal.Stop(signalChannel)
	close(signalChannel)

	// Wait for event pump to drain.
	<-eventsDone

	// Final safety checkpoint: catch any events appended after the
	// last trigger-driven checkpoint (e.g., the metric event itself
	// is checkpointed inline, but this catches edge cases where the
	// pump drains without a final trigger). Uses context.Background()
	// because the main context may be cancelled during shutdown.
	if tracker != nil {
		tracker.checkpointDelta(context.Background(), agent.CheckpointSessionEnd)
	}

	// Compute session summary. The summary is used both for the
	// completion message and for reporting metrics to the agent service.
	var summary SessionSummary
	if sessionLog != nil {
		summary = sessionLog.Summary()
	}

	// End the agent service session before posting the completion
	// summary. This ensures that by the time "agent-complete" is
	// visible to observers via /sync, the session and metrics state
	// events have already been written by the agent service. Runs
	// regardless of whether the agent process succeeded or failed —
	// the session is over and its metrics should be recorded. Errors
	// are logged but do not mask the agent's own exit status.
	if agentServiceClient != nil {
		endRequest := EndSessionRequestFromSummary(sessionID, summary, "")
		if endError := agentServiceClient.EndSession(ctx, endRequest); endError != nil {
			logger.Error("ending agent service session", "error", endError)
		} else {
			logger.Info("agent service session ended", "session_id", sessionID)
		}
	}

	// Post completion summary to config room. This is the last action
	// the agent takes — observers use "agent-complete" as a signal
	// that all cleanup (including agent service session end) is done.
	if sessionLog != nil {
		summaryMessage := formatSummary(summary, processError)
		logger.Info("posting completion summary",
			"events", summary.EventCount,
			"input_tokens", summary.InputTokens,
			"output_tokens", summary.OutputTokens,
			"tool_calls", summary.ToolCallCount,
			"cost_usd", summary.CostUSD,
			"duration", summary.Duration,
		)
		if _, sendError := session.SendMessage(ctx, agentContext.ConfigRoomID, messaging.NewTextMessage(summaryMessage)); sendError != nil {
			logger.Warn("failed to send completion summary", "error", sendError)
		}
	}

	if processError != nil {
		return fmt.Errorf("agent process exited: %w", processError)
	}
	return nil
}

// formatSummary formats a SessionSummary as a human-readable message for
// posting to the config room.
func formatSummary(summary SessionSummary, processError error) string {
	var builder strings.Builder
	builder.WriteString("agent-complete")
	if processError != nil {
		builder.WriteString(fmt.Sprintf(" (exit error: %v)", processError))
	}
	builder.WriteString(fmt.Sprintf("\n- Duration: %s", summary.Duration.Truncate(time.Second)))
	builder.WriteString(fmt.Sprintf("\n- Events: %d", summary.EventCount))
	if summary.InputTokens > 0 || summary.OutputTokens > 0 {
		builder.WriteString(fmt.Sprintf("\n- Tokens: %d in / %d out", summary.InputTokens, summary.OutputTokens))
	}
	if summary.CacheReadTokens > 0 || summary.CacheWriteTokens > 0 {
		builder.WriteString(fmt.Sprintf("\n- Cache: %d read / %d write", summary.CacheReadTokens, summary.CacheWriteTokens))
	}
	if summary.ToolCallCount > 0 {
		builder.WriteString(fmt.Sprintf("\n- Tool calls: %d", summary.ToolCallCount))
	}
	if summary.CostUSD > 0 {
		builder.WriteString(fmt.Sprintf("\n- Cost: $%.4f", summary.CostUSD))
	}
	if summary.TurnCount > 0 {
		builder.WriteString(fmt.Sprintf("\n- Turns: %d", summary.TurnCount))
	}
	if summary.ErrorCount > 0 {
		builder.WriteString(fmt.Sprintf("\n- Errors: %d", summary.ErrorCount))
	}
	return builder.String()
}

// runMessagePump uses Matrix /sync long-polling to receive incoming
// messages from the config room and write them to the agent's stdin.
// The homeserver holds each /sync request for up to 30 seconds,
// returning immediately when new events arrive — no client-side
// polling interval, no wasted cycles.
//
// Messages from the agent itself and from the machine daemon are
// skipped (the daemon posts operational messages like service directory
// updates that are not intended for the agent).
func runMessagePump(ctx context.Context, session messaging.Session, roomID ref.RoomID, ownUserID, machineUserID ref.UserID, stdin io.Writer, logger *slog.Logger, ready chan<- struct{}) {
	filter := buildMessageSyncFilter(roomID.String())

	// Initial /sync with timeout=0 to capture the stream position. Events
	// from the initial sync are discarded — only messages arriving AFTER
	// the agent starts should be injected into the agent's stdin.
	response, err := session.Sync(ctx, messaging.SyncOptions{
		Timeout:    0,
		SetTimeout: true,
		Filter:     filter,
	})
	if err != nil {
		logger.Error("message pump: initial sync failed", "error", err)
		close(ready)
		return
	}
	sinceToken := response.NextBatch

	// Signal that the pump has captured the stream position and is
	// ready to receive messages. Callers wait on this before announcing
	// "agent-ready" to ensure no messages are missed.
	close(ready)

	// Long-poll loop with exponential backoff on transient errors.
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		response, err := session.Sync(ctx, messaging.SyncOptions{
			Since:      sinceToken,
			Timeout:    30000, // 30s server-side hold
			SetTimeout: true,
			Filter:     filter,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Debug("message pump sync error, retrying", "error", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		backoff = time.Second
		sinceToken = response.NextBatch

		joined, ok := response.Rooms.Join[roomID]
		if !ok {
			continue
		}

		for _, event := range joined.Timeline.Events {
			if event.Type != "m.room.message" {
				continue
			}
			if event.Sender == ownUserID || event.Sender == machineUserID {
				continue
			}
			msgtype, _ := event.Content["msgtype"].(string)
			if msgtype != "m.text" {
				continue
			}
			body, _ := event.Content["body"].(string)
			if body == "" {
				continue
			}

			// Route messages using the chat model:
			// - Machine-originated messages set "target" to a specific agent's user ID.
			// - Human-originated messages use @mentions (structured m.mentions
			//   or body @-patterns) to address specific agents.
			// - Messages with no target and no mentions are broadcast to all agents.
			target, _ := event.Content["target"].(string)
			if target != "" {
				if target != ownUserID.String() {
					logger.Debug("skipping targeted message for other agent",
						"target", target, "sender", event.Sender)
					continue
				}
			} else {
				mentions := extractEventMentions(event.Content, body)
				if len(mentions) > 0 && !slices.Contains(mentions, ownUserID.String()) {
					logger.Debug("skipping mentioned message not addressed to this agent",
						"mentions", mentions, "sender", event.Sender)
					continue
				}
			}

			logger.Info("injecting message from Matrix", "sender", event.Sender)
			fmt.Fprintf(stdin, "%s\n", body)
		}
	}
}

// buildMessageSyncFilter builds an inline JSON filter for /sync that
// restricts the response to m.room.message events in the given room.
// This minimizes data transfer and server-side work — the agent only
// needs text messages from its config room.
func buildMessageSyncFilter(roomID string) string {
	filter := map[string]any{
		"room": map[string]any{
			"rooms": []string{roomID},
			"timeline": map[string]any{
				"types": []string{"m.room.message"},
				"limit": 50,
			},
			"state":        map[string]any{"types": []string{}},
			"ephemeral":    map[string]any{"types": []string{}},
			"account_data": map[string]any{"types": []string{}},
		},
		"presence":     map[string]any{"types": []string{}},
		"account_data": map[string]any{"types": []string{}},
	}
	data, err := json.Marshal(filter)
	if err != nil {
		// The filter is statically constructed — marshaling cannot fail.
		panic("building message sync filter: " + err.Error())
	}
	return string(data)
}
