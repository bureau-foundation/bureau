// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	telemetryschema "github.com/bureau-foundation/bureau/lib/schema/telemetry"
)

// parseTimeFlag parses a time specification from a CLI flag value.
// Accepts three formats:
//   - Go duration strings: "1h", "30m", "2h30m" — resolved relative to now
//   - Day suffixes: "7d", "30d" — shorthand for multiples of 24h
//   - Timestamps: RFC3339 ("2026-03-01T12:00:00Z") or date-only ("2026-03-01")
//
// Returns Unix nanoseconds. Duration-based values are subtracted from
// the current time (i.e., "1h" means "1 hour ago").
func parseTimeFlag(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}

	// Try day suffix first (not handled by time.ParseDuration).
	if strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err == nil && days > 0 {
			return time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixNano(), nil
		}
	}

	// Try Go duration.
	duration, err := time.ParseDuration(value)
	if err == nil {
		return time.Now().Add(-duration).UnixNano(), nil
	}

	// Try RFC3339 timestamp.
	timestamp, err := time.Parse(time.RFC3339, value)
	if err == nil {
		return timestamp.UnixNano(), nil
	}

	// Try date-only (YYYY-MM-DD), interpreted as midnight UTC.
	timestamp, err = time.Parse("2006-01-02", value)
	if err == nil {
		return timestamp.UnixNano(), nil
	}

	return 0, fmt.Errorf("invalid time %q: expected duration (1h, 7d), RFC3339 timestamp, or date (2006-01-02)", value)
}

// parseDurationFlag parses a duration flag value and returns
// nanoseconds. Accepts Go duration strings and day suffixes.
func parseDurationFlag(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}

	if strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err == nil && days > 0 {
			return int64(time.Duration(days) * 24 * time.Hour), nil
		}
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: expected Go duration (1h, 30m) or day suffix (7d)", value)
	}
	return int64(duration), nil
}

// formatDuration formats nanoseconds as a human-readable duration.
// Uses the largest appropriate unit: ns, µs, ms, s, or compound
// minutes+seconds / hours+minutes for longer durations.
func formatDuration(nanoseconds int64) string {
	if nanoseconds < 0 {
		return fmt.Sprintf("-%s", formatDuration(-nanoseconds))
	}
	duration := time.Duration(nanoseconds)
	switch {
	case duration < time.Microsecond:
		return fmt.Sprintf("%dns", nanoseconds)
	case duration < time.Millisecond:
		return fmt.Sprintf("%.1fµs", float64(nanoseconds)/float64(time.Microsecond))
	case duration < time.Second:
		return fmt.Sprintf("%.1fms", float64(nanoseconds)/float64(time.Millisecond))
	case duration < time.Minute:
		return fmt.Sprintf("%.2fs", float64(nanoseconds)/float64(time.Second))
	case duration < time.Hour:
		minutes := int(duration / time.Minute)
		seconds := int((duration % time.Minute) / time.Second)
		if seconds == 0 {
			return fmt.Sprintf("%dm", minutes)
		}
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	default:
		hours := int(duration / time.Hour)
		minutes := int((duration % time.Hour) / time.Minute)
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
}

// formatTimestamp formats Unix nanoseconds as a local-time string.
// Uses RFC3339 with second precision for absolute display.
func formatTimestamp(nanoseconds int64) string {
	if nanoseconds == 0 {
		return "-"
	}
	return time.Unix(0, nanoseconds).Local().Format("2006-01-02T15:04:05")
}

// formatUptime formats seconds as a human-readable uptime string.
func formatUptime(seconds float64) string {
	duration := time.Duration(seconds * float64(time.Second))
	hours := int(duration / time.Hour)
	minutes := int((duration % time.Hour) / time.Minute)
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

// formatBytes formats a byte count as a human-readable string.
func formatBytes(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// severityName returns a human-readable name for an OpenTelemetry
// severity number. Maps to the standard range names.
func severityName(severity uint8) string {
	switch {
	case severity >= telemetryschema.SeverityFatal:
		return "FATAL"
	case severity >= telemetryschema.SeverityError:
		return "ERROR"
	case severity >= telemetryschema.SeverityWarn:
		return "WARN"
	case severity >= telemetryschema.SeverityInfo:
		return "INFO"
	case severity >= telemetryschema.SeverityDebug:
		return "DEBUG"
	case severity >= telemetryschema.SeverityTrace:
		return "TRACE"
	default:
		return "UNSET"
	}
}

// parseSeverityFlag parses a severity name to its OpenTelemetry
// severity number. Accepts: trace, debug, info, warn, error, fatal.
func parseSeverityFlag(name string) (uint8, error) {
	switch strings.ToLower(name) {
	case "trace":
		return telemetryschema.SeverityTrace, nil
	case "debug":
		return telemetryschema.SeverityDebug, nil
	case "info":
		return telemetryschema.SeverityInfo, nil
	case "warn", "warning":
		return telemetryschema.SeverityWarn, nil
	case "error":
		return telemetryschema.SeverityError, nil
	case "fatal":
		return telemetryschema.SeverityFatal, nil
	default:
		return 0, fmt.Errorf("invalid severity %q: expected trace, debug, info, warn, error, or fatal", name)
	}
}

// spanStatusName returns a human-readable name for a SpanStatus value.
func spanStatusName(status telemetryschema.SpanStatus) string {
	switch status {
	case telemetryschema.SpanStatusOK:
		return "ok"
	case telemetryschema.SpanStatusError:
		return "error"
	default:
		return "unset"
	}
}

// parseSpanStatusFlag parses a span status flag value to a SpanStatus.
// Accepts: ok, error, unset.
func parseSpanStatusFlag(name string) (telemetryschema.SpanStatus, error) {
	switch strings.ToLower(name) {
	case "ok":
		return telemetryschema.SpanStatusOK, nil
	case "error":
		return telemetryschema.SpanStatusError, nil
	case "unset":
		return telemetryschema.SpanStatusUnset, nil
	default:
		return 0, fmt.Errorf("invalid span status %q: expected ok, error, or unset", name)
	}
}

// metricKindName returns a human-readable name for a MetricKind value.
func metricKindName(kind telemetryschema.MetricKind) string {
	switch kind {
	case telemetryschema.MetricKindGauge:
		return "gauge"
	case telemetryschema.MetricKindCounter:
		return "counter"
	case telemetryschema.MetricKindHistogram:
		return "histogram"
	default:
		return fmt.Sprintf("kind(%d)", kind)
	}
}

// truncate shortens a string to maxLength, appending "..." if truncated.
func truncate(value string, maxLength int) string {
	if len(value) <= maxLength {
		return value
	}
	if maxLength <= 3 {
		return value[:maxLength]
	}
	return value[:maxLength-3] + "..."
}
