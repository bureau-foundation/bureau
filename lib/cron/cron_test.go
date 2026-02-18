// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cron

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expression string) Schedule {
	t.Helper()
	schedule, err := Parse(expression)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expression, err)
	}
	return schedule
}

func utc(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

func TestParseValid(t *testing.T) {
	expressions := []string{
		"* * * * *",
		"0 7 * * *",
		"*/15 0-6 1,15 * 1-5",
		"30 3 * * 0",
		"0 0 1 1 *",
		"5,10,15 * * * *",
		"0-30/5 * * * *",
	}
	for _, expression := range expressions {
		t.Run(expression, func(t *testing.T) {
			if _, err := Parse(expression); err != nil {
				t.Errorf("Parse(%q) = %v, want nil", expression, err)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		wantErr    string
	}{
		{"too_few_fields", "* * * *", "expected 5 fields"},
		{"too_many_fields", "* * * * * *", "expected 5 fields"},
		{"empty", "", "expected 5 fields"},
		{"minute_out_of_range", "60 * * * *", "out of range"},
		{"hour_out_of_range", "* 24 * * *", "out of range"},
		{"day_zero", "* * 0 * *", "out of range"},
		{"day_out_of_range", "* * 32 * *", "out of range"},
		{"month_zero", "* * * 0 *", "out of range"},
		{"month_out_of_range", "* * * 13 *", "out of range"},
		{"dow_out_of_range", "* * * * 7", "out of range"},
		{"negative_step", "*/0 * * * *", "step must be positive"},
		{"bad_range", "5-3 * * * *", "range start 5 > end 3"},
		{"non_numeric", "abc * * * *", "invalid value"},
		{"bad_step_value", "*/x * * * *", "invalid step"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(test.expression)
			if err == nil {
				t.Fatalf("Parse(%q) = nil, want error containing %q", test.expression, test.wantErr)
			}
			if !contains(err.Error(), test.wantErr) {
				t.Errorf("Parse(%q) = %q, want error containing %q", test.expression, err, test.wantErr)
			}
		})
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && searchSubstring(haystack, needle)
}

func searchSubstring(haystack, needle string) bool {
	for i := 0; i <= len(haystack)-len(needle); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestNextEveryMinute(t *testing.T) {
	schedule := mustParse(t, "* * * * *")
	from := utc(2026, 2, 18, 10, 30)
	next, err := schedule.Next(from)
	if err != nil {
		t.Fatal(err)
	}
	want := utc(2026, 2, 18, 10, 31)
	if !next.Equal(want) {
		t.Errorf("Next = %v, want %v", next, want)
	}
}

func TestNextDailyAt7AM(t *testing.T) {
	schedule := mustParse(t, "0 7 * * *")

	// Before 7am → same day.
	next, err := schedule.Next(utc(2026, 2, 18, 5, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2026, 2, 18, 7, 0); !next.Equal(want) {
		t.Errorf("before 7am: Next = %v, want %v", next, want)
	}

	// After 7am → next day.
	next, err = schedule.Next(utc(2026, 2, 18, 8, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2026, 2, 19, 7, 0); !next.Equal(want) {
		t.Errorf("after 7am: Next = %v, want %v", next, want)
	}

	// Exactly at 7:00 → next day (strictly after).
	next, err = schedule.Next(utc(2026, 2, 18, 7, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2026, 2, 19, 7, 0); !next.Equal(want) {
		t.Errorf("at 7am: Next = %v, want %v", next, want)
	}
}

func TestNextEvery15Minutes(t *testing.T) {
	schedule := mustParse(t, "*/15 * * * *")

	tests := []struct {
		from time.Time
		want time.Time
	}{
		{utc(2026, 2, 18, 10, 0), utc(2026, 2, 18, 10, 15)},
		{utc(2026, 2, 18, 10, 14), utc(2026, 2, 18, 10, 15)},
		{utc(2026, 2, 18, 10, 15), utc(2026, 2, 18, 10, 30)},
		{utc(2026, 2, 18, 10, 46), utc(2026, 2, 18, 11, 0)},
		{utc(2026, 2, 18, 23, 50), utc(2026, 2, 19, 0, 0)},
	}

	for _, test := range tests {
		next, err := schedule.Next(test.from)
		if err != nil {
			t.Fatalf("Next(%v): %v", test.from, err)
		}
		if !next.Equal(test.want) {
			t.Errorf("Next(%v) = %v, want %v", test.from, next, test.want)
		}
	}
}

func TestNextWeekdaysOnly(t *testing.T) {
	// Monday through Friday at 9am.
	schedule := mustParse(t, "0 9 * * 1-5")

	// Tuesday → same day.
	next, err := schedule.Next(utc(2026, 2, 17, 8, 0)) // Feb 17 2026 is Tuesday
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2026, 2, 17, 9, 0); !next.Equal(want) {
		t.Errorf("Tuesday before 9am: Next = %v (weekday=%v), want %v", next, next.Weekday(), want)
	}

	// Friday after 9am → next Monday.
	next, err = schedule.Next(utc(2026, 2, 20, 10, 0)) // Feb 20 2026 is Friday
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2026, 2, 23, 9, 0); !next.Equal(want) {
		t.Errorf("Friday after 9am: Next = %v (weekday=%v), want %v (Monday)", next, next.Weekday(), want)
	}
}

func TestNextSpecificDayOfMonth(t *testing.T) {
	// 1st and 15th of every month at midnight.
	schedule := mustParse(t, "0 0 1,15 * *")

	next, err := schedule.Next(utc(2026, 2, 2, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2026, 2, 15, 0, 0); !next.Equal(want) {
		t.Errorf("Feb 2: Next = %v, want %v", next, want)
	}

	next, err = schedule.Next(utc(2026, 2, 16, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2026, 3, 1, 0, 0); !next.Equal(want) {
		t.Errorf("Feb 16: Next = %v, want %v", next, want)
	}
}

func TestNextJanuary1(t *testing.T) {
	// Only Jan 1 at midnight.
	schedule := mustParse(t, "0 0 1 1 *")

	next, err := schedule.Next(utc(2026, 3, 15, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2027, 1, 1, 0, 0); !next.Equal(want) {
		t.Errorf("Next = %v, want %v", next, want)
	}
}

func TestNextMonthBoundary(t *testing.T) {
	// Midnight on the 31st. Months without a 31st are skipped.
	schedule := mustParse(t, "0 0 31 * *")

	// January 31 exists.
	next, err := schedule.Next(utc(2026, 1, 1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2026, 1, 31, 0, 0); !next.Equal(want) {
		t.Errorf("Jan: Next = %v, want %v", next, want)
	}

	// February has no 31st, skip to March 31.
	next, err = schedule.Next(utc(2026, 2, 1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2026, 3, 31, 0, 0); !next.Equal(want) {
		t.Errorf("Feb: Next = %v, want %v", next, want)
	}
}

func TestNextYearRollover(t *testing.T) {
	schedule := mustParse(t, "0 7 * * *")

	next, err := schedule.Next(utc(2026, 12, 31, 8, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2027, 1, 1, 7, 0); !next.Equal(want) {
		t.Errorf("Dec 31 after 7am: Next = %v, want %v", next, want)
	}
}

func TestNextLeapYear(t *testing.T) {
	// Feb 29 only exists in leap years. 2028 is a leap year.
	schedule := mustParse(t, "0 0 29 2 *")

	next, err := schedule.Next(utc(2026, 1, 1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2028, 2, 29, 0, 0); !next.Equal(want) {
		t.Errorf("Next Feb 29: Next = %v, want %v", next, want)
	}
}

func TestNextStrictlyAfter(t *testing.T) {
	// Verify that Next returns a time strictly after t, even when
	// t is exactly on a match.
	schedule := mustParse(t, "30 10 * * *")

	next, err := schedule.Next(utc(2026, 2, 18, 10, 30))
	if err != nil {
		t.Fatal(err)
	}
	if !next.After(utc(2026, 2, 18, 10, 30)) {
		t.Errorf("Next should be strictly after input, got %v", next)
	}
	if want := utc(2026, 2, 19, 10, 30); !next.Equal(want) {
		t.Errorf("Next = %v, want %v", next, want)
	}
}

func TestNextWithSubMinutePrecision(t *testing.T) {
	// When t has seconds or nanoseconds, Next should still return
	// the correct next minute boundary.
	schedule := mustParse(t, "0 * * * *")

	from := utc(2026, 2, 18, 10, 59).Add(30 * time.Second)
	next, err := schedule.Next(from)
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2026, 2, 18, 11, 0); !next.Equal(want) {
		t.Errorf("Next = %v, want %v", next, want)
	}
}

func TestNextSundayCron(t *testing.T) {
	// Every Sunday at 3am.
	schedule := mustParse(t, "0 3 * * 0")

	// Feb 18 2026 is Wednesday.
	next, err := schedule.Next(utc(2026, 2, 18, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	// Next Sunday is Feb 22.
	if want := utc(2026, 2, 22, 3, 0); !next.Equal(want) {
		t.Errorf("Next Sunday = %v (weekday=%v), want %v", next, next.Weekday(), want)
	}
}

func TestNextRangeWithStep(t *testing.T) {
	// Every 5 minutes in the first half hour.
	schedule := mustParse(t, "0-30/5 * * * *")

	next, err := schedule.Next(utc(2026, 2, 18, 10, 7))
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2026, 2, 18, 10, 10); !next.Equal(want) {
		t.Errorf("Next = %v, want %v", next, want)
	}

	// After minute 30, wraps to next hour.
	next, err = schedule.Next(utc(2026, 2, 18, 10, 31))
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(2026, 2, 18, 11, 0); !next.Equal(want) {
		t.Errorf("After :30 = %v, want %v", next, want)
	}
}

func TestMultipleConsecutiveNext(t *testing.T) {
	// Verify that calling Next repeatedly produces a correct
	// sequence of times.
	schedule := mustParse(t, "0 */6 * * *")

	cursor := utc(2026, 2, 18, 0, 0)
	expected := []time.Time{
		utc(2026, 2, 18, 6, 0),
		utc(2026, 2, 18, 12, 0),
		utc(2026, 2, 18, 18, 0),
		utc(2026, 2, 19, 0, 0),
		utc(2026, 2, 19, 6, 0),
	}

	for i, want := range expected {
		next, err := schedule.Next(cursor)
		if err != nil {
			t.Fatalf("Next #%d from %v: %v", i, cursor, err)
		}
		if !next.Equal(want) {
			t.Errorf("Next #%d = %v, want %v", i, next, want)
		}
		cursor = next
	}
}

func TestParseFieldEdgeCases(t *testing.T) {
	// Verify specific field parsing behaviors.
	tests := []struct {
		name  string
		field string
		min   int
		max   int
		want  []int
	}{
		{"single", "5", 0, 59, []int{5}},
		{"range", "1-3", 0, 59, []int{1, 2, 3}},
		{"list", "1,3,5", 0, 59, []int{1, 3, 5}},
		{"star", "*", 0, 5, []int{0, 1, 2, 3, 4, 5}},
		{"star_step", "*/2", 0, 5, []int{0, 2, 4}},
		{"range_step", "1-10/3", 0, 59, []int{1, 4, 7, 10}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bits, err := parseField(test.field, test.min, test.max)
			if err != nil {
				t.Fatalf("parseField(%q, %d, %d) = %v", test.field, test.min, test.max, err)
			}
			for _, value := range test.want {
				if !bits.has(value) {
					t.Errorf("parseField(%q): missing value %d", test.field, value)
				}
			}
			// Verify no extra values are set.
			count := 0
			for value := test.min; value <= test.max; value++ {
				if bits.has(value) {
					count++
				}
			}
			if count != len(test.want) {
				t.Errorf("parseField(%q): got %d values, want %d", test.field, count, len(test.want))
			}
		})
	}
}
