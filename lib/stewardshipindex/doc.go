// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package stewardshipindex provides an in-memory index of stewardship
// declarations for resource-to-declaration resolution. The ticket service
// maintains this index from m.bureau.stewardship state events received
// via /sync and queries it when processing tickets with Affects entries
// to determine which stewardship declarations apply.
package stewardshipindex
