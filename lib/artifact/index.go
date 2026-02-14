// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"regexp"
	"sort"
	"strings"
)

// DefaultListLimit is the maximum number of results returned by
// ArtifactIndex.List when no limit is specified.
const DefaultListLimit = 100

// MaxListLimit is the maximum allowed value for ArtifactFilter.Limit.
const MaxListLimit = 1000

// ArtifactFilter controls which artifacts ArtifactIndex.List returns.
// Zero-value fields mean "no filter" for that dimension. All non-zero
// fields must match (AND semantics).
type ArtifactFilter struct {
	// ContentType matches artifacts with this exact MIME type.
	ContentType string

	// Label matches artifacts whose Labels slice contains this string.
	Label string

	// CachePolicy matches artifacts with this exact cache policy.
	CachePolicy string

	// Visibility matches artifacts with this exact visibility.
	Visibility string

	// MinSize matches artifacts with Size >= this value. Zero means
	// no minimum.
	MinSize int64

	// MaxSize matches artifacts with Size <= this value. Zero means
	// no maximum.
	MaxSize int64

	// Limit is the maximum number of results to return. Zero means
	// DefaultListLimit. Values above MaxListLimit are clamped.
	Limit int

	// Offset skips this many results before collecting. Used with
	// Limit for pagination.
	Offset int
}

// ArtifactEntry pairs a file hash with its metadata. Returned by
// query methods.
type ArtifactEntry struct {
	FileHash Hash
	Metadata ArtifactMetadata
}

// ArtifactStats holds aggregate counts across all artifacts in the index.
type ArtifactStats struct {
	Total         int
	TotalSize     int64
	ByContentType map[string]int
	ByCachePolicy map[string]int
}

// ArtifactIndex is an in-memory index of artifact metadata with
// secondary indexes for filtered queries. It follows the same pattern
// as the ticket.Index: secondary indexes map dimension values to sets
// of file hashes for fast intersection.
//
// ArtifactIndex is not safe for concurrent use. The artifact service
// serializes all writes through its write mutex; reads and writes
// must not overlap.
type ArtifactIndex struct {
	artifacts map[Hash]ArtifactMetadata

	// Secondary indexes: dimension value → set of file hashes.
	byContentType map[string]map[Hash]struct{}
	byLabel       map[string]map[Hash]struct{}
	byCachePolicy map[string]map[Hash]struct{}
	byVisibility  map[string]map[Hash]struct{}
}

// NewArtifactIndex returns an empty index ready for use.
func NewArtifactIndex() *ArtifactIndex {
	return &ArtifactIndex{
		artifacts:     make(map[Hash]ArtifactMetadata),
		byContentType: make(map[string]map[Hash]struct{}),
		byLabel:       make(map[string]map[Hash]struct{}),
		byCachePolicy: make(map[string]map[Hash]struct{}),
		byVisibility:  make(map[string]map[Hash]struct{}),
	}
}

// Build populates the index from a slice of metadata records. Called
// once at startup with the output of MetadataStore.ScanAll.
func (idx *ArtifactIndex) Build(records []ArtifactMetadata) {
	for _, meta := range records {
		idx.addToIndexes(meta)
	}
}

// Put adds or updates an artifact in the index. If an artifact with
// the same file hash already exists, it is replaced and all secondary
// indexes are updated.
func (idx *ArtifactIndex) Put(meta ArtifactMetadata) {
	if existing, exists := idx.artifacts[meta.FileHash]; exists {
		idx.removeFromIndexes(existing)
	}
	idx.addToIndexes(meta)
}

// Remove deletes an artifact from all indexes.
func (idx *ArtifactIndex) Remove(fileHash Hash) {
	existing, exists := idx.artifacts[fileHash]
	if !exists {
		return
	}
	idx.removeFromIndexes(existing)
}

// Get returns the metadata for a single artifact by hash.
func (idx *ArtifactIndex) Get(fileHash Hash) (ArtifactMetadata, bool) {
	meta, exists := idx.artifacts[fileHash]
	return meta, exists
}

// List returns artifacts matching all filter dimensions. Results are
// sorted by StoredAt descending (newest first). Limit and offset
// control pagination.
func (idx *ArtifactIndex) List(filter ArtifactFilter) ([]ArtifactEntry, int) {
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	// Start with the smallest candidate set.
	candidates := idx.candidateSet(filter)

	// Apply all filter predicates.
	var matching []ArtifactEntry
	for hash := range candidates {
		meta := idx.artifacts[hash]
		if idx.matchesFilter(meta, filter) {
			matching = append(matching, ArtifactEntry{
				FileHash: hash,
				Metadata: meta,
			})
		}
	}

	// Sort by StoredAt descending.
	sort.Slice(matching, func(i, j int) bool {
		return matching[i].Metadata.StoredAt.After(matching[j].Metadata.StoredAt)
	})

	total := len(matching)

	// Apply offset.
	if filter.Offset > 0 {
		if filter.Offset >= len(matching) {
			return nil, total
		}
		matching = matching[filter.Offset:]
	}

	// Apply limit.
	if len(matching) > limit {
		matching = matching[:limit]
	}

	return matching, total
}

// Grep searches artifact descriptions and filenames for the given
// regex pattern. Returns matching entries.
func (idx *ArtifactIndex) Grep(pattern string) ([]ArtifactEntry, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	var results []ArtifactEntry
	for hash, meta := range idx.artifacts {
		if re.MatchString(meta.Description) || re.MatchString(meta.Filename) {
			results = append(results, ArtifactEntry{
				FileHash: hash,
				Metadata: meta,
			})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Metadata.StoredAt.After(results[j].Metadata.StoredAt)
	})

	return results, nil
}

// Len returns the number of artifacts in the index.
func (idx *ArtifactIndex) Len() int {
	return len(idx.artifacts)
}

// Stats returns aggregate counts across all indexed artifacts.
func (idx *ArtifactIndex) Stats() ArtifactStats {
	stats := ArtifactStats{
		Total:         len(idx.artifacts),
		ByContentType: make(map[string]int),
		ByCachePolicy: make(map[string]int),
	}
	for _, meta := range idx.artifacts {
		stats.TotalSize += meta.Size
		stats.ByContentType[meta.ContentType]++
		if meta.CachePolicy != "" {
			stats.ByCachePolicy[meta.CachePolicy]++
		}
	}
	return stats
}

// candidateSet returns the initial set of hashes to filter. When a
// filter dimension has a secondary index, use the smallest matching
// set. When no indexed filter is active, return all artifacts.
func (idx *ArtifactIndex) candidateSet(filter ArtifactFilter) map[Hash]struct{} {
	type indexedSet struct {
		set  map[Hash]struct{}
		size int
	}

	var candidates []indexedSet

	if filter.ContentType != "" {
		if set, ok := idx.byContentType[filter.ContentType]; ok {
			candidates = append(candidates, indexedSet{set, len(set)})
		} else {
			// No artifacts match this content type.
			return nil
		}
	}
	if filter.Label != "" {
		if set, ok := idx.byLabel[filter.Label]; ok {
			candidates = append(candidates, indexedSet{set, len(set)})
		} else {
			return nil
		}
	}
	if filter.CachePolicy != "" {
		if set, ok := idx.byCachePolicy[filter.CachePolicy]; ok {
			candidates = append(candidates, indexedSet{set, len(set)})
		} else {
			return nil
		}
	}
	if filter.Visibility != "" {
		if set, ok := idx.byVisibility[filter.Visibility]; ok {
			candidates = append(candidates, indexedSet{set, len(set)})
		} else {
			return nil
		}
	}

	if len(candidates) == 0 {
		// No indexed filter — return all artifacts.
		all := make(map[Hash]struct{}, len(idx.artifacts))
		for hash := range idx.artifacts {
			all[hash] = struct{}{}
		}
		return all
	}

	// Start from the smallest index and intersect with others.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].size < candidates[j].size
	})

	result := make(map[Hash]struct{}, candidates[0].size)
	for hash := range candidates[0].set {
		result[hash] = struct{}{}
	}

	for _, other := range candidates[1:] {
		for hash := range result {
			if _, ok := other.set[hash]; !ok {
				delete(result, hash)
			}
		}
	}

	return result
}

// matchesFilter checks whether a single metadata record satisfies all
// filter predicates. Indexed dimensions (content type, label, cache
// policy, visibility) are already handled by candidateSet; this method
// handles the remaining non-indexed dimensions (size range).
func (idx *ArtifactIndex) matchesFilter(meta ArtifactMetadata, filter ArtifactFilter) bool {
	if filter.MinSize > 0 && meta.Size < filter.MinSize {
		return false
	}
	if filter.MaxSize > 0 && meta.Size > filter.MaxSize {
		return false
	}
	return true
}

// addToIndexes adds a metadata record to the primary map and all
// secondary indexes.
func (idx *ArtifactIndex) addToIndexes(meta ArtifactMetadata) {
	idx.artifacts[meta.FileHash] = meta

	addToSet(idx.byContentType, meta.ContentType, meta.FileHash)
	for _, label := range meta.Labels {
		addToSet(idx.byLabel, label, meta.FileHash)
	}
	if meta.CachePolicy != "" {
		addToSet(idx.byCachePolicy, meta.CachePolicy, meta.FileHash)
	}
	if meta.Visibility != "" {
		addToSet(idx.byVisibility, meta.Visibility, meta.FileHash)
	}
}

// removeFromIndexes removes a metadata record from all secondary
// indexes and the primary map.
func (idx *ArtifactIndex) removeFromIndexes(meta ArtifactMetadata) {
	delete(idx.artifacts, meta.FileHash)

	removeFromSet(idx.byContentType, meta.ContentType, meta.FileHash)
	for _, label := range meta.Labels {
		removeFromSet(idx.byLabel, label, meta.FileHash)
	}
	if meta.CachePolicy != "" {
		removeFromSet(idx.byCachePolicy, meta.CachePolicy, meta.FileHash)
	}
	if meta.Visibility != "" {
		removeFromSet(idx.byVisibility, meta.Visibility, meta.FileHash)
	}
}

// addToSet adds a hash to a secondary index set, creating the set if
// it doesn't exist.
func addToSet(index map[string]map[Hash]struct{}, key string, hash Hash) {
	if key == "" {
		return
	}
	set, ok := index[key]
	if !ok {
		set = make(map[Hash]struct{})
		index[key] = set
	}
	set[hash] = struct{}{}
}

// removeFromSet removes a hash from a secondary index set, deleting
// the set entry if it becomes empty.
func removeFromSet(index map[string]map[Hash]struct{}, key string, hash Hash) {
	if key == "" {
		return
	}
	set, ok := index[key]
	if !ok {
		return
	}
	delete(set, hash)
	if len(set) == 0 {
		delete(index, key)
	}
}

// matchesLabel checks whether the labels slice contains the target label.
// Not currently used by matchesFilter (handled by candidateSet) but
// available for direct use.
func matchesLabel(labels []string, target string) bool {
	for _, label := range labels {
		if strings.EqualFold(label, target) {
			return true
		}
	}
	return false
}
