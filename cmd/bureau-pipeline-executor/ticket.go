// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/schema/ticket"
	"github.com/bureau-foundation/bureau/lib/service"
)

const (
	// defaultTicketSocket is the sandbox path where the ticket service
	// socket is bind-mounted by the daemon. Matches the standard
	// RequiredServices mounting convention.
	defaultTicketSocket = "/run/bureau/service/ticket.sock"

	// defaultTicketTokenPath is the sandbox path for the ticket service
	// token, written by the daemon at sandbox creation time.
	defaultTicketTokenPath = "/run/bureau/service/token/ticket.token"

	// cancellationPollInterval is how often the cancellation watcher
	// polls the ticket service for status changes. The ticket service
	// serves "show" from its in-memory index in microseconds over a
	// local Unix socket, so 5-second polling has negligible overhead.
	cancellationPollInterval = 5 * time.Second

	// closeRetryCount is the maximum number of attempts for closing a
	// ticket. Close is a critical operation — the ticket must reach
	// "closed" status to reflect the pipeline's terminal outcome.
	closeRetryCount = 3

	// closeRetryBaseDelay is the initial delay between close retry
	// attempts. Each subsequent attempt doubles the delay.
	closeRetryBaseDelay = 500 * time.Millisecond
)

// claimTicket calls the "update" action to atomically transition a
// ticket from "open" to "in_progress" with the given assignee. If the
// ticket is already in_progress (contention), returns an error that
// isContention recognizes. The caller should exit cleanly on
// contention — another executor already claimed this ticket.
func claimTicket(ctx context.Context, client *service.ServiceClient, ticketID, roomID, assignee string) error {
	fields := map[string]any{
		"ticket":   ticketID,
		"room":     roomID,
		"status":   string(ticket.StatusInProgress),
		"assignee": assignee,
	}
	return client.Call(ctx, "update", fields, nil)
}

// isContention reports whether an error from claimTicket indicates that
// the ticket is already in_progress (another executor claimed it). The
// ticket service rejects open→in_progress transitions when the ticket
// is already in_progress, returning a ServiceError with a message
// containing "already in_progress".
func isContention(err error) bool {
	var serviceError *service.ServiceError
	if errors.As(err, &serviceError) {
		// The ticket service error message format for contention is:
		// "ticket <id> is already in_progress (assigned to <user>)"
		return serviceError.Action == "update" &&
			containsSubstring(serviceError.Message, "already in_progress")
	}
	return false
}

// containsSubstring checks if s contains substr. Extracted to avoid
// importing strings for a single call.
func containsSubstring(s, substr string) bool {
	return len(substr) <= len(s) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// updateTicketProgress calls the "update" action to set pipeline
// execution progress on a ticket. The entire PipelineExecutionContent
// is replaced. Best-effort: the caller should log warnings on failure
// but continue pipeline execution.
func updateTicketProgress(ctx context.Context, client *service.ServiceClient, ticketID, roomID string, pipelineContent ticket.PipelineExecutionContent) error {
	fields := map[string]any{
		"ticket":   ticketID,
		"room":     roomID,
		"pipeline": &pipelineContent,
	}
	return client.Call(ctx, "update", fields, nil)
}

// addTicketStepNote calls the "add-note" action to record a step
// outcome on the ticket. Best-effort: the caller should log warnings
// on failure but continue pipeline execution.
func addTicketStepNote(ctx context.Context, client *service.ServiceClient, ticketID, roomID, body string) error {
	fields := map[string]any{
		"ticket": ticketID,
		"room":   roomID,
		"body":   body,
	}
	return client.Call(ctx, "add-note", fields, nil)
}

// closeTicket calls the "close" action with retry logic. Close is a
// critical operation — the ticket must reflect the pipeline's terminal
// outcome. Retries up to closeRetryCount times with exponential
// backoff (500ms, 1s, 2s).
func closeTicket(ctx context.Context, clk clock.Clock, client *service.ServiceClient, ticketID, roomID, reason string, logger *slog.Logger) error {
	fields := map[string]any{
		"ticket": ticketID,
		"room":   roomID,
		"reason": reason,
	}

	var lastError error
	delay := closeRetryBaseDelay
	for attempt := 0; attempt < closeRetryCount; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("close ticket: context cancelled after %d attempts: %w", attempt, lastError)
			case <-clk.After(delay):
			}
			delay *= 2
		}
		lastError = client.Call(ctx, "close", fields, nil)
		if lastError == nil {
			return nil
		}
		logger.Warn("close ticket attempt failed", "attempt", attempt+1, "max_attempts", closeRetryCount, "error", lastError)
	}
	return fmt.Errorf("close ticket failed after %d attempts: %w", closeRetryCount, lastError)
}

// ticketShowResult is the minimal response structure for the "show"
// action, containing only the fields the cancellation watcher needs.
// The ticket service encodes showResponse with Go field names (since
// showResponse uses json tags, and the CBOR encoder falls back to Go
// field names when no cbor tags are present).
type ticketShowResult struct {
	Content ticketShowContent
}

type ticketShowContent struct {
	Status ticket.TicketStatus
}

// showTicketStatus calls the "show" action and returns the ticket's
// current status. Used by the cancellation watcher to detect external
// cancellation (ticket closed while the pipeline is running).
func showTicketStatus(ctx context.Context, client *service.ServiceClient, ticketID, roomID string) (ticket.TicketStatus, error) {
	fields := map[string]any{
		"ticket": ticketID,
		"room":   roomID,
	}
	var result ticketShowResult
	if err := client.Call(ctx, "show", fields, &result); err != nil {
		return "", err
	}
	return result.Content.Status, nil
}

// watchForCancellation polls the ticket service at the given interval
// and cancels the step context when the ticket's status changes to
// "closed". This enables external cancellation: an operator or system
// can close the pip- ticket to abort a running pipeline.
//
// The goroutine exits when stepContext is done (steps finished or
// cancellation triggered). Transient errors from the ticket service
// are logged and retried — a brief socket unavailability does not
// kill the pipeline.
func watchForCancellation(stepContext context.Context, clk clock.Clock, client *service.ServiceClient, ticketID, roomID string, cancelSteps context.CancelFunc, cancelled *atomic.Bool, pollInterval time.Duration, logger *slog.Logger) {
	ticker := clk.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stepContext.Done():
			return
		case <-ticker.C:
			status, err := showTicketStatus(stepContext, client, ticketID, roomID)
			if err != nil {
				// Transient failure: log and retry next tick.
				logger.Warn("cancellation watcher poll failed", "error", err)
				continue
			}
			if status == ticket.StatusClosed {
				logger.Info("ticket closed externally, cancelling execution")
				cancelled.Store(true)
				cancelSteps()
				return
			}
		}
	}
}
