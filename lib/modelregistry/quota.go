// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modelregistry

import (
	"fmt"
	"sync"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/schema/model"
)

// QuotaExceededError reports that an account has exceeded its spending
// limit. The model service returns this to callers with enough detail
// to construct a meaningful error response (which limit was hit, how
// much was spent, when the window resets).
type QuotaExceededError struct {
	// AccountName is the account whose quota was exceeded.
	AccountName string

	// Window is "daily" or "monthly".
	Window string

	// Limit is the maximum allowed spend in microdollars for this window.
	Limit int64

	// Current is the cumulative spend in microdollars so far in
	// this window.
	Current int64

	// ResetsAt is the UTC time when this window resets.
	ResetsAt time.Time
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf(
		"modelregistry: account %q exceeded %s quota (limit=%d, current=%d, resets=%s)",
		e.AccountName, e.Window, e.Limit, e.Current,
		e.ResetsAt.Format(time.RFC3339))
}

// QuotaTracker tracks cumulative spend per account per time window
// (daily and monthly). The model service checks quota before
// forwarding a request and records cost after the request completes.
//
// QuotaTracker is thread-safe. The model service calls Check from
// request handlers concurrently, and Record from the response path.
//
// Time windows are UTC-aligned:
//   - Daily: midnight-to-midnight UTC
//   - Monthly: first-of-month to first-of-next-month UTC
//
// Old window entries are cleaned up lazily: each Check/Record call
// operates on the current window, and Cleanup removes entries from
// expired windows.
type QuotaTracker struct {
	clock clock.Clock

	mu    sync.Mutex
	spend map[spendKey]int64
}

// spendKey identifies a cumulative spend bucket: one account in one
// time window.
type spendKey struct {
	// Account is the account name (state key).
	Account string

	// Window is the window identifier: "2026-03-02" for daily,
	// "2026-03" for monthly.
	Window string
}

// NewQuotaTracker creates a tracker using the given clock for time
// determination. Production code passes clock.Real(); tests pass
// clock.Fake() for deterministic window boundaries.
func NewQuotaTracker(clock clock.Clock) *QuotaTracker {
	return &QuotaTracker{
		clock: clock,
		spend: make(map[spendKey]int64),
	}
}

// Check verifies that the account is within its quota limits. Returns
// nil if the account has no quota or is within limits. Returns a
// *QuotaExceededError if either the daily or monthly limit has been
// reached.
//
// Check is called before forwarding a request. It does not reserve
// capacity — two concurrent requests that both pass Check may
// together exceed the limit. This is acceptable: quotas are guardrails
// for cost management, not hard security boundaries. The slight
// overshoot from concurrent requests is bounded by a single request's
// cost.
func (tracker *QuotaTracker) Check(accountName string, quota *model.Quota) error {
	if quota == nil {
		return nil
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	now := tracker.clock.Now().UTC()

	if quota.DailyMicrodollars > 0 {
		dailyKey := spendKey{Account: accountName, Window: dailyWindow(now)}
		current := tracker.spend[dailyKey]
		if current >= quota.DailyMicrodollars {
			return &QuotaExceededError{
				AccountName: accountName,
				Window:      "daily",
				Limit:       quota.DailyMicrodollars,
				Current:     current,
				ResetsAt:    nextDayUTC(now),
			}
		}
	}

	if quota.MonthlyMicrodollars > 0 {
		monthlyKey := spendKey{Account: accountName, Window: monthlyWindow(now)}
		current := tracker.spend[monthlyKey]
		if current >= quota.MonthlyMicrodollars {
			return &QuotaExceededError{
				AccountName: accountName,
				Window:      "monthly",
				Limit:       quota.MonthlyMicrodollars,
				Current:     current,
				ResetsAt:    nextMonthUTC(now),
			}
		}
	}

	return nil
}

// Record adds cost to the account's current daily and monthly windows.
// Called after a model request completes, when the actual token usage
// and cost are known.
func (tracker *QuotaTracker) Record(accountName string, costMicrodollars int64) {
	if costMicrodollars <= 0 {
		return
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	now := tracker.clock.Now().UTC()

	dailyKey := spendKey{Account: accountName, Window: dailyWindow(now)}
	tracker.spend[dailyKey] += costMicrodollars

	monthlyKey := spendKey{Account: accountName, Window: monthlyWindow(now)}
	tracker.spend[monthlyKey] += costMicrodollars
}

// DailySpend returns the cumulative spend for an account in the
// current UTC day. Returns 0 if no spend has been recorded.
func (tracker *QuotaTracker) DailySpend(accountName string) int64 {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	now := tracker.clock.Now().UTC()
	return tracker.spend[spendKey{Account: accountName, Window: dailyWindow(now)}]
}

// MonthlySpend returns the cumulative spend for an account in the
// current UTC month. Returns 0 if no spend has been recorded.
func (tracker *QuotaTracker) MonthlySpend(accountName string) int64 {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	now := tracker.clock.Now().UTC()
	return tracker.spend[spendKey{Account: accountName, Window: monthlyWindow(now)}]
}

// Cleanup removes spend entries from expired time windows. Call
// periodically (e.g., once per hour) to prevent unbounded growth
// of the spend map. Not required for correctness — only for memory.
func (tracker *QuotaTracker) Cleanup() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	now := tracker.clock.Now().UTC()
	currentDaily := dailyWindow(now)
	currentMonthly := monthlyWindow(now)

	for key := range tracker.spend {
		if key.Window != currentDaily && key.Window != currentMonthly {
			delete(tracker.spend, key)
		}
	}
}

// SpendEntryCount returns the number of tracked spend entries. Used
// for testing and monitoring.
func (tracker *QuotaTracker) SpendEntryCount() int {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return len(tracker.spend)
}

// --- Time window helpers ---

// dailyWindow returns the window key for UTC daily tracking.
// Format: "2026-03-02".
func dailyWindow(now time.Time) string {
	return now.Format("2006-01-02")
}

// monthlyWindow returns the window key for UTC monthly tracking.
// Format: "2026-03".
func monthlyWindow(now time.Time) string {
	return now.Format("2006-01")
}

// nextDayUTC returns midnight UTC of the next day.
func nextDayUTC(now time.Time) time.Time {
	year, month, day := now.Date()
	return time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC)
}

// nextMonthUTC returns midnight UTC of the first of the next month.
func nextMonthUTC(now time.Time) time.Time {
	year, month, _ := now.Date()
	return time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)
}
