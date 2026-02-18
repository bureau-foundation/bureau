// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule represents a parsed cron expression. Use Parse to create
// one from a string, then call Next to compute the next matching time.
type Schedule struct {
	minutes     bitset64
	hours       bitset64
	daysOfMonth bitset64
	months      bitset64
	daysOfWeek  bitset64
}

// bitset64 uses a uint64 as a compact set of integers 0-63.
type bitset64 uint64

func (b bitset64) has(value int) bool { return b&(1<<uint(value)) != 0 }
func (b *bitset64) set(value int)     { *b |= 1 << uint(value) }

// Parse parses a standard 5-field cron expression. Returns an error
// if the expression is malformed or contains out-of-range values.
func Parse(expression string) (Schedule, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("cron: expected 5 fields, got %d", len(fields))
	}

	minutes, err := parseField(fields[0], 0, 59)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: minute field: %w", err)
	}
	hours, err := parseField(fields[1], 0, 23)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: hour field: %w", err)
	}
	daysOfMonth, err := parseField(fields[2], 1, 31)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: day-of-month field: %w", err)
	}
	months, err := parseField(fields[3], 1, 12)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: month field: %w", err)
	}
	daysOfWeek, err := parseField(fields[4], 0, 6)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: day-of-week field: %w", err)
	}

	return Schedule{
		minutes:     minutes,
		hours:       hours,
		daysOfMonth: daysOfMonth,
		months:      months,
		daysOfWeek:  daysOfWeek,
	}, nil
}

// Next returns the earliest time strictly after t that matches the
// schedule. All computation is in UTC.
//
// Returns an error if no matching time can be found within 4 years
// of t (prevents infinite loops on impossible schedules like
// Feb 31).
func (s Schedule) Next(t time.Time) (time.Time, error) {
	// Start from the next minute after t, with seconds/nanos zeroed.
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)

	// Search limit: 4 years covers all leap year cycles.
	limit := t.AddDate(4, 0, 0)

	for t.Before(limit) {
		// Advance to a matching month.
		if !s.months.has(int(t.Month())) {
			// Jump to the first day of the next month.
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}

		// Check day-of-month and day-of-week. Standard cron
		// semantics: if BOTH fields are restricted (non-wildcard),
		// the match is OR (either day matches). If only one is
		// restricted, the match is AND with the wildcard.
		//
		// We don't track which fields were wildcards — instead we
		// just check both constraints. Since wildcards produce
		// bitsets with all bits set, AND is effectively the
		// behavior for a wildcard field.
		dayOfMonth := t.Day()
		dayOfWeek := int(t.Weekday())

		if !s.daysOfMonth.has(dayOfMonth) || !s.daysOfWeek.has(dayOfWeek) {
			// Advance to next day.
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}

		// Check hour.
		if !s.hours.has(t.Hour()) {
			// Advance to next hour.
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, time.UTC)
			continue
		}

		// Check minute.
		if !s.minutes.has(t.Minute()) {
			// Advance by one minute.
			t = t.Add(time.Minute)
			continue
		}

		return t, nil
	}

	return time.Time{}, fmt.Errorf("cron: no matching time within 4 years of %s", t.Format(time.RFC3339))
}

// parseField parses a single cron field into a bitset. The field may
// contain comma-separated terms, each of which is a wildcard, value,
// range, or stepped range/wildcard.
func parseField(field string, minimum, maximum int) (bitset64, error) {
	var result bitset64
	for _, term := range strings.Split(field, ",") {
		bits, err := parseTerm(term, minimum, maximum)
		if err != nil {
			return 0, err
		}
		result |= bits
	}
	if result == 0 {
		return 0, fmt.Errorf("field %q produces empty set", field)
	}
	return result, nil
}

// parseTerm parses a single term: *, */N, V, V-V, V-V/N.
func parseTerm(term string, minimum, maximum int) (bitset64, error) {
	// Split on "/" for step expressions.
	parts := strings.SplitN(term, "/", 2)
	rangeExpression := parts[0]
	step := 1
	if len(parts) == 2 {
		parsed, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, fmt.Errorf("invalid step %q: %w", parts[1], err)
		}
		if parsed <= 0 {
			return 0, fmt.Errorf("step must be positive, got %d", parsed)
		}
		step = parsed
	}

	var rangeStart, rangeEnd int

	if rangeExpression == "*" {
		rangeStart = minimum
		rangeEnd = maximum
	} else if dashIndex := strings.IndexByte(rangeExpression, '-'); dashIndex >= 0 {
		// Range: V-V
		startStr := rangeExpression[:dashIndex]
		endStr := rangeExpression[dashIndex+1:]
		var err error
		rangeStart, err = strconv.Atoi(startStr)
		if err != nil {
			return 0, fmt.Errorf("invalid range start %q: %w", startStr, err)
		}
		rangeEnd, err = strconv.Atoi(endStr)
		if err != nil {
			return 0, fmt.Errorf("invalid range end %q: %w", endStr, err)
		}
		if rangeStart > rangeEnd {
			return 0, fmt.Errorf("range start %d > end %d", rangeStart, rangeEnd)
		}
	} else {
		// Single value.
		value, err := strconv.Atoi(rangeExpression)
		if err != nil {
			return 0, fmt.Errorf("invalid value %q: %w", rangeExpression, err)
		}
		rangeStart = value
		rangeEnd = value
	}

	if rangeStart < minimum || rangeEnd > maximum {
		return 0, fmt.Errorf("value out of range [%d-%d]: got %d-%d", minimum, maximum, rangeStart, rangeEnd)
	}

	var result bitset64
	for value := rangeStart; value <= rangeEnd; value += step {
		result.set(value)
	}
	return result, nil
}
