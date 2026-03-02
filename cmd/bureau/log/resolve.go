// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package log

import (
	"context"
	"fmt"
	"io"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/codec"
	logschema "github.com/bureau-foundation/bureau/lib/schema/log"
)

// resolveLogContent fetches the LogContent metadata for a given source
// and optional session ID. If sessionID is empty, the latest session
// is selected (the last tag alphabetically, which for UUID session IDs
// corresponds to the most recently created session).
//
// Returns the resolved LogContent and the tag name used.
func resolveLogContent(ctx context.Context, client *artifactstore.Client, source string, sessionID string) (*logschema.LogContent, string, error) {
	if sessionID != "" {
		// Exact session requested — fetch directly by tag name.
		tagName := "log/" + source + "/" + sessionID
		content, err := fetchLogMetadata(ctx, client, tagName)
		if err != nil {
			return nil, "", fmt.Errorf("fetching log metadata for %s: %w", tagName, err)
		}
		return content, tagName, nil
	}

	// No session specified — list all sessions for this source and
	// pick the latest.
	prefix := "log/" + source + "/"
	tagsResponse, err := client.Tags(ctx, prefix)
	if err != nil {
		return nil, "", fmt.Errorf("listing log sessions for %s: %w", source, err)
	}
	if len(tagsResponse.Tags) == 0 {
		return nil, "", fmt.Errorf("no output sessions found for source %q", source)
	}

	// Pick the last tag (latest session by alphabetical ordering).
	// If finer selection is needed (e.g., by timestamp), we'd fetch
	// each metadata artifact and compare. For now, alphabetical
	// ordering on session IDs is sufficient since UUIDs sort
	// chronologically.
	latestTag := tagsResponse.Tags[len(tagsResponse.Tags)-1]
	content, err := fetchLogMetadata(ctx, client, latestTag.Name)
	if err != nil {
		return nil, "", fmt.Errorf("fetching log metadata for %s: %w", latestTag.Name, err)
	}
	return content, latestTag.Name, nil
}

// fetchLogMetadata fetches and decodes a LogContent metadata artifact
// from the given tag name.
func fetchLogMetadata(ctx context.Context, client *artifactstore.Client, tagName string) (*logschema.LogContent, error) {
	result, err := client.Fetch(ctx, tagName)
	if err != nil {
		return nil, err
	}
	defer result.Content.Close()

	data, err := io.ReadAll(result.Content)
	if err != nil {
		return nil, fmt.Errorf("reading metadata: %w", err)
	}

	var content logschema.LogContent
	if err := codec.Unmarshal(data, &content); err != nil {
		return nil, fmt.Errorf("decoding metadata: %w", err)
	}

	return &content, nil
}

// writeChunks fetches each chunk's content from the artifact service
// and writes the raw bytes sequentially to the provided writer. This
// reconstructs the original terminal output stream.
func writeChunks(ctx context.Context, client *artifactstore.Client, chunks []logschema.LogChunk, output io.Writer) error {
	for index, chunk := range chunks {
		result, err := client.Fetch(ctx, chunk.Ref)
		if err != nil {
			return fmt.Errorf("fetching chunk %d (ref %s): %w", index, chunk.Ref, err)
		}

		_, err = io.Copy(output, result.Content)
		result.Content.Close()
		if err != nil {
			return fmt.Errorf("writing chunk %d: %w", index, err)
		}
	}
	return nil
}
