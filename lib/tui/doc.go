// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package tui provides shared terminal user interface components for
// Bureau's interactive viewers. Built on bubbletea (Elm architecture),
// these components handle common patterns like dropdown overlays,
// search/filter input, text editing modals, change animation, and
// ANSI-aware text manipulation.
//
// Domain-specific viewers (tickets, Matrix rooms, fleet) import this
// package for consistent look and behavior: same theme, same keyboard
// conventions, same overlay mechanics. Each viewer owns its own data
// source, layout, and domain-specific rendering.
package tui
