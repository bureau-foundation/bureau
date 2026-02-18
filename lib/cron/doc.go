// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package cron parses standard 5-field cron expressions and computes
// the next occurrence after a given time.
//
// Supported syntax:
//
//	┌───────────── minute (0-59)
//	│ ┌───────────── hour (0-23)
//	│ │ ┌───────────── day of month (1-31)
//	│ │ │ ┌───────────── month (1-12)
//	│ │ │ │ ┌───────────── day of week (0-6, 0=Sunday)
//	│ │ │ │ │
//	* * * * *
//
// Each field supports:
//   - Single values: 5
//   - Ranges: 1-5
//   - Lists: 1,3,5
//   - Steps: */15, 1-30/5
//   - Wildcard: *
//
// All times are UTC. No @yearly/@monthly shortcuts, no seconds field,
// no named days/months. This is intentionally minimal — Bureau
// schedules use UTC wall-clock time exclusively.
package cron
