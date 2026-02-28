// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/bureau-foundation/bureau/lib/artifactstore"
	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/agent"
)

// maxChainDepth is the maximum number of commits walkChain will
// traverse before returning an error. This prevents unbounded walks
// from corrupt chains that form cycles. Real chains are unlikely to
// exceed a few hundred commits before compaction.
const maxChainDepth = 10000

// Stop strategy constants for materialize-context requests.
const (
	// stopStrategyCompaction stops at the nearest compaction commit
	// (including it). This is the default: materialization produces
	// compaction summary + post-compaction deltas.
	stopStrategyCompaction = "compaction"

	// stopStrategyRoot walks the entire chain to the root commit.
	// Produces the full conversation, ignoring compaction boundaries.
	stopStrategyRoot = "root"
)

// chainEntry pairs a commit ID with its deserialized content.
type chainEntry struct {
	ID      string
	Content agent.ContextCommitContent
}

// walkChain reads the context commit chain from tip to the stop point
// determined by stopStrategy, returning commits in root-to-tip order
// (ready for concatenation).
//
// Stop strategies:
//   - "compaction": stop after including the nearest compaction commit.
//   - "root": walk until Parent is empty.
//   - A specific "ctx-*" ID: stop after including that commit.
//
// The returned slice is never empty on success — it always contains at
// least the tip commit.
func (agentService *AgentService) walkChain(
	ctx context.Context,
	tipCommitID string,
	stopStrategy string,
) ([]chainEntry, error) {
	var chain []chainEntry
	currentID := tipCommitID

	// isSpecificID is true when the stop strategy is a specific
	// commit ID rather than "compaction" or "root".
	isSpecificID := stopStrategy != stopStrategyCompaction && stopStrategy != stopStrategyRoot
	foundStopTarget := false

	for {
		if len(chain) >= maxChainDepth {
			return nil, fmt.Errorf(
				"chain depth exceeded %d commits from tip %q: possible cycle or corrupt chain",
				maxChainDepth, tipCommitID,
			)
		}

		content, err := agentService.getCommit(ctx, currentID)
		if err != nil {
			return nil, err
		}

		chain = append(chain, chainEntry{ID: currentID, Content: *content})

		// Decide whether to stop walking.
		switch stopStrategy {
		case stopStrategyCompaction:
			// Stop after including a compaction commit — it acts
			// as the conversation prefix.
			if content.CommitType == agent.CommitTypeCompaction {
				goto done
			}
			// Also stop if a snapshot is encountered — it already
			// contains the full materialized context up to this point.
			if content.CommitType == agent.CommitTypeSnapshot {
				goto done
			}
		case stopStrategyRoot:
			// Walk all the way to root — only stop when Parent is empty.
		default:
			// Specific ctx-* ID: stop after including that commit.
			if currentID == stopStrategy {
				foundStopTarget = true
				goto done
			}
		}

		if content.Parent == "" {
			// Reached the root of the chain.
			break
		}

		currentID = content.Parent
	}

	// If the caller asked to stop at a specific commit ID and we
	// walked the entire chain without finding it, that's an error —
	// either the ID is wrong or it's not an ancestor of the tip.
	if isSpecificID && !foundStopTarget {
		return nil, fmt.Errorf(
			"stop target %q not found in chain from tip %q (walked %d commits)",
			stopStrategy, tipCommitID, len(chain),
		)
	}

done:
	// Reverse: chain was built tip-to-root, callers need root-to-tip.
	slices.Reverse(chain)
	return chain, nil
}

// concatenateDeltas fetches each commit's artifact from the CAS and
// concatenates them using format-specific rules. Returns the combined
// content bytes and the appropriate content type string.
//
// All commits in the chain must have the same Format. The caller
// validates this before calling concatenateDeltas.
func (agentService *AgentService) concatenateDeltas(
	ctx context.Context,
	chain []chainEntry,
) ([]byte, string, error) {
	if len(chain) == 0 {
		return nil, "", fmt.Errorf("empty chain: nothing to concatenate")
	}

	format := chain[0].Content.Format

	switch format {
	case "claude-code-v1":
		return agentService.concatenateJSONL(ctx, chain)
	case "bureau-agent-v1", "events-v1":
		return agentService.concatenateCBORArrays(ctx, chain)
	default:
		return nil, "", fmt.Errorf("unsupported context format %q: cannot concatenate", format)
	}
}

// concatenateJSONL fetches and byte-appends JSONL deltas. Each delta
// is a sequence of complete JSONL lines; concatenation is byte append.
func (agentService *AgentService) concatenateJSONL(
	ctx context.Context,
	chain []chainEntry,
) ([]byte, string, error) {
	var buffer bytes.Buffer

	for _, entry := range chain {
		data, err := agentService.fetchArtifactContent(ctx, entry.Content.ArtifactRef)
		if err != nil {
			return nil, "", fmt.Errorf("fetching artifact for commit %s: %w", entry.ID, err)
		}
		buffer.Write(data)
		// Ensure each delta ends with a newline so concatenation
		// doesn't merge the last line of one delta with the first
		// line of the next.
		if len(data) > 0 && data[len(data)-1] != '\n' {
			buffer.WriteByte('\n')
		}
	}

	return buffer.Bytes(), "application/jsonl", nil
}

// concatenateCBORArrays fetches CBOR array deltas, merges them into a
// single array, and re-encodes. Each delta is a CBOR-encoded array of
// message structs; concatenation decodes each and appends all items.
func (agentService *AgentService) concatenateCBORArrays(
	ctx context.Context,
	chain []chainEntry,
) ([]byte, string, error) {
	var merged []codec.RawMessage

	for _, entry := range chain {
		data, err := agentService.fetchArtifactContent(ctx, entry.Content.ArtifactRef)
		if err != nil {
			return nil, "", fmt.Errorf("fetching artifact for commit %s: %w", entry.ID, err)
		}

		var items []codec.RawMessage
		if err := codec.Unmarshal(data, &items); err != nil {
			return nil, "", fmt.Errorf("decoding CBOR array from commit %s (artifact %s): %w",
				entry.ID, entry.Content.ArtifactRef, err)
		}
		merged = append(merged, items...)
	}

	result, err := codec.Marshal(merged)
	if err != nil {
		return nil, "", fmt.Errorf("encoding merged CBOR array: %w", err)
	}

	return result, "application/cbor", nil
}

// fetchArtifactContent downloads an artifact's full content into a byte
// slice. The caller is responsible for not calling this on extremely
// large artifacts (context deltas are bounded by LLM context windows,
// so individual deltas are at most a few MB).
func (agentService *AgentService) fetchArtifactContent(ctx context.Context, artifactRef string) ([]byte, error) {
	result, err := agentService.artifactClient.Fetch(ctx, artifactRef)
	if err != nil {
		return nil, fmt.Errorf("fetching artifact %s: %w", artifactRef, err)
	}
	defer result.Content.Close()

	data, err := io.ReadAll(result.Content)
	if err != nil {
		return nil, fmt.Errorf("reading artifact %s content: %w", artifactRef, err)
	}
	return data, nil
}

// storeArtifact stores content as a new artifact and returns the
// artifact ref.
func (agentService *AgentService) storeArtifact(
	ctx context.Context,
	content []byte,
	contentType string,
	labels []string,
) (string, error) {
	header := &artifactstore.StoreHeader{
		Action:      "store",
		ContentType: contentType,
		Size:        int64(len(content)),
		Data:        content,
		Labels:      labels,
	}

	response, err := agentService.artifactClient.Store(ctx, header, nil)
	if err != nil {
		return "", fmt.Errorf("storing artifact: %w", err)
	}

	return response.Ref, nil
}
