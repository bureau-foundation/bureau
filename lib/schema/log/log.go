// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package log

import (
	"fmt"

	"github.com/bureau-foundation/bureau/lib/ref"
)

// LogContentVersion is the current schema version for LogContent
// events. Increment when adding fields that existing code must not
// silently drop during read-modify-write.
const LogContentVersion = 1

// LogStatus describes the lifecycle state of a log entity.
type LogStatus string

const (
	// LogStatusActive means the process is running and output is
	// being captured. New chunks are being appended.
	LogStatusActive LogStatus = "active"

	// LogStatusComplete means the process has exited. No more chunks
	// will be appended. The chunk list is the final, complete record.
	LogStatusComplete LogStatus = "complete"

	// LogStatusRotating means the process is long-lived and the
	// telemetry service is actively evicting old chunks to stay
	// within retention or size limits. New chunks are appended at
	// the end while old chunks are removed from the front.
	LogStatusRotating LogStatus = "rotating"
)

// LogFormat describes the encoding of the captured output.
type LogFormat string

const (
	// LogFormatRaw is unprocessed terminal output bytes: ANSI
	// escape sequences, binary protocol frames, partial multi-byte
	// characters. This is what the PTY produces.
	LogFormatRaw LogFormat = "raw"
)

// LogChunk references a single CAS artifact containing a contiguous
// segment of captured output. Chunks are ordered by Sequence within
// the parent LogContent's Chunks slice.
type LogChunk struct {
	// Ref is the CAS artifact reference (BLAKE3 hex hash) pointing
	// to the raw output bytes for this chunk.
	Ref string `json:"ref"`

	// Sequence is the OutputDelta sequence number of the first delta
	// included in this chunk. Combined with the next chunk's Sequence
	// (or the session's current sequence for the last chunk), this
	// defines the exact delta range the chunk covers. Used for gap
	// detection and tail-from-offset.
	Sequence uint64 `json:"sequence"`

	// Size is the uncompressed size of the chunk in bytes. The CAS
	// artifact may be compressed; this is the logical size.
	Size int64 `json:"size"`

	// Timestamp is the capture time of the first OutputDelta in this
	// chunk, as Unix nanoseconds. Used for time-based seeking and
	// display.
	Timestamp int64 `json:"timestamp"`
}

// Validate checks that all required fields of a LogChunk are present
// and well-formed.
func (chunk *LogChunk) Validate(index int) error {
	if chunk.Ref == "" {
		return fmt.Errorf("log chunk %d: ref is required", index)
	}
	if chunk.Size <= 0 {
		return fmt.Errorf("log chunk %d: size must be > 0, got %d", index, chunk.Size)
	}
	return nil
}

// LogContent is the content of an EventTypeLog state event. Tracks
// the chunk index for one process invocation's output capture. The
// telemetry service creates this event when the first output delta
// arrives for a session and updates it as chunks are stored.
//
// The chunk list is flat and append-only during active capture (no
// parent chains, no branching). The only mutation after initial
// creation is appending chunks, updating TotalBytes, and transitioning
// Status. During rotation, old chunks are removed from the front.
type LogContent struct {
	// Version is the schema version (see LogContentVersion). Code
	// that modifies this event must call CanModify() first; if
	// Version exceeds LogContentVersion, the modification is refused
	// to prevent silent field loss. Readers may process any version
	// (unknown fields are harmlessly ignored by Go's JSON
	// unmarshaler).
	Version int `json:"version"`

	// SessionID identifies the process invocation this log belongs
	// to. A new session starts each time the sandbox is created;
	// the ID distinguishes output from different incarnations of
	// the same principal.
	SessionID string `json:"session_id"`

	// Source identifies the principal producing output. This is the
	// sandboxed process whose stdout/stderr is being captured.
	Source ref.Entity `json:"source"`

	// Format describes the encoding of captured output. Currently
	// always LogFormatRaw ("raw"). When structured log parsing is
	// added, this field distinguishes raw terminal bytes from
	// pre-parsed log records (requiring a version bump).
	Format LogFormat `json:"format"`

	// Status is the lifecycle state of this log entity. Transitions:
	// active → complete (process exits), active → rotating (eviction
	// starts on a long-lived process).
	Status LogStatus `json:"status"`

	// TotalBytes is the sum of all chunk sizes. Updated with each
	// new chunk append and each eviction removal. Represents the
	// current window size, not lifetime total.
	TotalBytes int64 `json:"total_bytes"`

	// Chunks is the ordered list of CAS artifact references that
	// make up this log. Append-only during active capture; the
	// eviction loop removes entries from the front during rotation.
	Chunks []LogChunk `json:"chunks,omitempty"`
}

// Validate checks that all required fields are present and
// well-formed.
func (content *LogContent) Validate() error {
	if content.Version < 1 {
		return fmt.Errorf("log: version must be >= 1, got %d", content.Version)
	}
	if content.SessionID == "" {
		return fmt.Errorf("log: session_id is required")
	}
	if content.Source.IsZero() {
		return fmt.Errorf("log: source is required")
	}
	switch content.Format {
	case LogFormatRaw:
		// Valid.
	case "":
		return fmt.Errorf("log: format is required")
	default:
		return fmt.Errorf("log: unsupported format %q", content.Format)
	}
	switch content.Status {
	case LogStatusActive, LogStatusComplete, LogStatusRotating:
		// Valid.
	case "":
		return fmt.Errorf("log: status is required")
	default:
		return fmt.Errorf("log: unsupported status %q", content.Status)
	}
	if content.TotalBytes < 0 {
		return fmt.Errorf("log: total_bytes must be >= 0, got %d", content.TotalBytes)
	}
	for index := range content.Chunks {
		if err := content.Chunks[index].Validate(index); err != nil {
			return err
		}
	}
	return nil
}

// CanModify checks whether this code version can safely perform a
// read-modify-write cycle on this event. Returns nil if safe, or an
// error explaining why modification would risk data loss.
func (content *LogContent) CanModify() error {
	if content.Version > LogContentVersion {
		return fmt.Errorf(
			"log version %d exceeds supported version %d: "+
				"modification would lose fields added in newer versions; "+
				"upgrade before modifying this event",
			content.Version, LogContentVersion,
		)
	}
	return nil
}
