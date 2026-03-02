// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package modelregistry

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bureau-foundation/bureau/lib/clock"
	"github.com/bureau-foundation/bureau/lib/schema/model"
)

// baseTime is 2026-03-02T14:30:00Z — a Monday afternoon in March.
var baseTime = time.Date(2026, 3, 2, 14, 30, 0, 0, time.UTC)

func TestQuotaCheck_NilQuota(t *testing.T) {
	fakeClock := clock.Fake(baseTime)
	tracker := NewQuotaTracker(fakeClock)

	// nil quota means unlimited — should always pass.
	if err := tracker.Check("account", nil); err != nil {
		t.Errorf("Check with nil quota: %v", err)
	}
}

func TestQuotaCheck_ZeroLimits(t *testing.T) {
	fakeClock := clock.Fake(baseTime)
	tracker := NewQuotaTracker(fakeClock)

	// Zero limits in a non-nil Quota mean no enforcement.
	quota := &model.Quota{DailyMicrodollars: 0, MonthlyMicrodollars: 0}
	tracker.Record("account", 999999999)

	if err := tracker.Check("account", quota); err != nil {
		t.Errorf("Check with zero limits: %v", err)
	}
}

func TestQuotaRecord_IncrementsDailyAndMonthly(t *testing.T) {
	fakeClock := clock.Fake(baseTime)
	tracker := NewQuotaTracker(fakeClock)

	tracker.Record("account", 1000)
	tracker.Record("account", 2000)

	if got := tracker.DailySpend("account"); got != 3000 {
		t.Errorf("DailySpend = %d, want 3000", got)
	}
	if got := tracker.MonthlySpend("account"); got != 3000 {
		t.Errorf("MonthlySpend = %d, want 3000", got)
	}
}

func TestQuotaRecord_ZeroCostIgnored(t *testing.T) {
	fakeClock := clock.Fake(baseTime)
	tracker := NewQuotaTracker(fakeClock)

	tracker.Record("account", 0)
	tracker.Record("account", -100)

	if got := tracker.DailySpend("account"); got != 0 {
		t.Errorf("DailySpend = %d, want 0 (zero/negative cost ignored)", got)
	}
}

func TestQuotaRecord_IsolatesAccounts(t *testing.T) {
	fakeClock := clock.Fake(baseTime)
	tracker := NewQuotaTracker(fakeClock)

	tracker.Record("alice", 5000)
	tracker.Record("shared", 3000)

	if got := tracker.DailySpend("alice"); got != 5000 {
		t.Errorf("alice DailySpend = %d, want 5000", got)
	}
	if got := tracker.DailySpend("shared"); got != 3000 {
		t.Errorf("shared DailySpend = %d, want 3000", got)
	}
}

func TestQuotaCheck_DailyExceeded(t *testing.T) {
	fakeClock := clock.Fake(baseTime)
	tracker := NewQuotaTracker(fakeClock)

	quota := &model.Quota{DailyMicrodollars: 10000}
	tracker.Record("account", 10000)

	err := tracker.Check("account", quota)
	if err == nil {
		t.Fatal("expected QuotaExceededError")
	}

	var quotaError *QuotaExceededError
	if !errors.As(err, &quotaError) {
		t.Fatalf("expected *QuotaExceededError, got %T: %v", err, err)
	}
	if quotaError.AccountName != "account" {
		t.Errorf("AccountName = %q, want %q", quotaError.AccountName, "account")
	}
	if quotaError.Window != "daily" {
		t.Errorf("Window = %q, want %q", quotaError.Window, "daily")
	}
	if quotaError.Limit != 10000 {
		t.Errorf("Limit = %d, want 10000", quotaError.Limit)
	}
	if quotaError.Current != 10000 {
		t.Errorf("Current = %d, want 10000", quotaError.Current)
	}

	// Reset time should be midnight UTC next day: 2026-03-03T00:00:00Z.
	expectedReset := time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)
	if !quotaError.ResetsAt.Equal(expectedReset) {
		t.Errorf("ResetsAt = %s, want %s", quotaError.ResetsAt, expectedReset)
	}
}

func TestQuotaCheck_MonthlyExceeded(t *testing.T) {
	fakeClock := clock.Fake(baseTime)
	tracker := NewQuotaTracker(fakeClock)

	quota := &model.Quota{
		DailyMicrodollars:   999999999, // high enough to not trigger
		MonthlyMicrodollars: 50000,
	}
	tracker.Record("account", 50000)

	err := tracker.Check("account", quota)
	if err == nil {
		t.Fatal("expected QuotaExceededError")
	}

	var quotaError *QuotaExceededError
	if !errors.As(err, &quotaError) {
		t.Fatalf("expected *QuotaExceededError, got %T: %v", err, err)
	}
	if quotaError.Window != "monthly" {
		t.Errorf("Window = %q, want %q", quotaError.Window, "monthly")
	}

	// Reset time should be first of next month: 2026-04-01T00:00:00Z.
	expectedReset := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if !quotaError.ResetsAt.Equal(expectedReset) {
		t.Errorf("ResetsAt = %s, want %s", quotaError.ResetsAt, expectedReset)
	}
}

func TestQuotaCheck_DailyCheckedBeforeMonthly(t *testing.T) {
	fakeClock := clock.Fake(baseTime)
	tracker := NewQuotaTracker(fakeClock)

	// Both limits exceeded. Daily should be reported first since it's
	// checked first.
	quota := &model.Quota{
		DailyMicrodollars:   100,
		MonthlyMicrodollars: 100,
	}
	tracker.Record("account", 200)

	err := tracker.Check("account", quota)
	var quotaError *QuotaExceededError
	if !errors.As(err, &quotaError) {
		t.Fatalf("expected *QuotaExceededError, got %T: %v", err, err)
	}
	if quotaError.Window != "daily" {
		t.Errorf("Window = %q, want %q (daily checked before monthly)", quotaError.Window, "daily")
	}
}

func TestQuotaCheck_UnderLimitPasses(t *testing.T) {
	fakeClock := clock.Fake(baseTime)
	tracker := NewQuotaTracker(fakeClock)

	quota := &model.Quota{
		DailyMicrodollars:   10000,
		MonthlyMicrodollars: 100000,
	}
	tracker.Record("account", 9999)

	if err := tracker.Check("account", quota); err != nil {
		t.Errorf("Check should pass when under limit: %v", err)
	}
}

func TestQuota_DailyWindowResets(t *testing.T) {
	fakeClock := clock.Fake(baseTime)
	tracker := NewQuotaTracker(fakeClock)

	quota := &model.Quota{DailyMicrodollars: 10000}
	tracker.Record("account", 10000)

	// Exceeded at 14:30 on March 2nd.
	if err := tracker.Check("account", quota); err == nil {
		t.Fatal("expected quota exceeded on March 2nd")
	}

	// Advance to March 3rd — new daily window, should pass.
	fakeClock.Advance(10 * time.Hour) // now 2026-03-03T00:30:00Z
	if err := tracker.Check("account", quota); err != nil {
		t.Errorf("Check should pass after daily window reset: %v", err)
	}

	// Old spend should not count in the new day.
	if got := tracker.DailySpend("account"); got != 0 {
		t.Errorf("DailySpend after window reset = %d, want 0", got)
	}

	// But monthly spend still includes the old day.
	if got := tracker.MonthlySpend("account"); got != 10000 {
		t.Errorf("MonthlySpend = %d, want 10000 (previous day counted)", got)
	}
}

func TestQuota_MonthlyWindowResets(t *testing.T) {
	// Start at end of March.
	endOfMarch := time.Date(2026, 3, 31, 23, 0, 0, 0, time.UTC)
	fakeClock := clock.Fake(endOfMarch)
	tracker := NewQuotaTracker(fakeClock)

	quota := &model.Quota{MonthlyMicrodollars: 50000}
	tracker.Record("account", 50000)

	// Exceeded on March 31st.
	if err := tracker.Check("account", quota); err == nil {
		t.Fatal("expected quota exceeded on March 31st")
	}

	// Advance to April 1st — new monthly window, should pass.
	fakeClock.Advance(2 * time.Hour) // now 2026-04-01T01:00:00Z
	if err := tracker.Check("account", quota); err != nil {
		t.Errorf("Check should pass after monthly window reset: %v", err)
	}

	if got := tracker.MonthlySpend("account"); got != 0 {
		t.Errorf("MonthlySpend after month boundary = %d, want 0", got)
	}
}

func TestQuota_Cleanup(t *testing.T) {
	fakeClock := clock.Fake(baseTime)
	tracker := NewQuotaTracker(fakeClock)

	// Record spend on March 2nd.
	tracker.Record("account", 5000)

	// Advance to March 4th (skip a day).
	fakeClock.Advance(48 * time.Hour)
	tracker.Record("account", 3000) // spend on March 4th

	// Before cleanup: entries for March 2nd (daily + monthly) and
	// March 4th (daily + monthly). The monthly window is the same
	// ("2026-03") so: 3 distinct keys.
	if got := tracker.SpendEntryCount(); got != 3 {
		t.Errorf("SpendEntryCount before cleanup = %d, want 3", got)
	}

	tracker.Cleanup()

	// After cleanup: only current daily (March 4th) and current
	// monthly (March) survive. The March 2nd daily entry is removed.
	if got := tracker.SpendEntryCount(); got != 2 {
		t.Errorf("SpendEntryCount after cleanup = %d, want 2", got)
	}

	// Current day's spend is unaffected.
	if got := tracker.DailySpend("account"); got != 3000 {
		t.Errorf("DailySpend after cleanup = %d, want 3000", got)
	}

	// Monthly spend includes March 2nd (5000) + March 4th (3000).
	if got := tracker.MonthlySpend("account"); got != 8000 {
		t.Errorf("MonthlySpend after cleanup = %d, want 8000", got)
	}
}

func TestQuota_UnknownAccountReturnsZero(t *testing.T) {
	fakeClock := clock.Fake(baseTime)
	tracker := NewQuotaTracker(fakeClock)

	if got := tracker.DailySpend("nonexistent"); got != 0 {
		t.Errorf("DailySpend(nonexistent) = %d, want 0", got)
	}
	if got := tracker.MonthlySpend("nonexistent"); got != 0 {
		t.Errorf("MonthlySpend(nonexistent) = %d, want 0", got)
	}
}

func TestQuotaExceededError_Format(t *testing.T) {
	err := &QuotaExceededError{
		AccountName: "alice",
		Window:      "daily",
		Limit:       50000000,
		Current:     50000001,
		ResetsAt:    time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC),
	}

	message := err.Error()
	if message == "" {
		t.Fatal("Error() returned empty string")
	}

	// Verify the message contains key information.
	for _, expected := range []string{"alice", "daily", "50000000", "50000001", "2026-03-03"} {
		if !strings.Contains(message, expected) {
			t.Errorf("Error() = %q, expected to contain %q", message, expected)
		}
	}
}
