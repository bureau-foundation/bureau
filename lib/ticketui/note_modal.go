// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import "github.com/bureau-foundation/bureau/lib/tui"

// NoteModal is a modal overlay for adding multi-line text input.
type NoteModal = tui.NoteModal

// NewNoteModal creates a NoteModal for adding a note in the given
// context. The textarea starts empty and focused.
var NewNoteModal = tui.NewNoteModal
