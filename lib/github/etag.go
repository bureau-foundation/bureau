// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import "sync"

// etagEntry holds a cached response for a URL.
type etagEntry struct {
	etag string
	body []byte
}

// etagCache stores ETag → response body mappings for conditional GET
// requests. When a GET response includes an ETag header, the response
// body is cached. On subsequent GETs to the same URL, the
// If-None-Match header is sent. If GitHub returns 304 Not Modified,
// the cached body is used instead of consuming rate limit quota.
//
// The cache has no eviction policy — it lives for the duration of the
// Client and is bounded by the number of distinct URLs queried.
type etagCache struct {
	mu      sync.Mutex
	entries map[string]etagEntry
}

func newETagCache() *etagCache {
	return &etagCache{entries: make(map[string]etagEntry)}
}

// get returns the cached ETag for a URL, or empty string if not cached.
func (cache *etagCache) get(url string) string {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[url]
	if !ok {
		return ""
	}
	return entry.etag
}

// body returns the cached response body for a URL, or nil if not cached.
func (cache *etagCache) body(url string) []byte {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[url]
	if !ok {
		return nil
	}
	return entry.body
}

// put stores an ETag and response body for a URL.
func (cache *etagCache) put(url string, etag string, body []byte) {
	if etag == "" {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.entries[url] = etagEntry{etag: etag, body: body}
}
