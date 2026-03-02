// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// PageIterator lazily fetches pages of results from a paginated GitHub
// API endpoint. Each call to Next fetches the next page and returns the
// items. Returns nil, nil when all pages have been consumed.
//
// The iterator is not safe for concurrent use.
type PageIterator[T any] struct {
	client  *Client
	nextURL string
	done    bool
}

// Next fetches the next page of results. Returns the items from that page.
// Returns nil, nil when no more pages are available. Each page fetch is
// subject to rate limiting and authentication, same as any other API call.
func (iterator *PageIterator[T]) Next(ctx context.Context) ([]T, error) {
	if iterator.done || iterator.nextURL == "" {
		return nil, nil
	}

	response, err := iterator.client.doRaw(ctx, http.MethodGet, iterator.nextURL, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, parseAPIError(response)
	}

	var items []T
	if err := json.NewDecoder(response.Body).Decode(&items); err != nil {
		return nil, err
	}

	// Parse the Link header to find the next page URL.
	iterator.nextURL = parseLinkNext(response.Header.Get("Link"))
	if iterator.nextURL == "" {
		iterator.done = true
	}

	return items, nil
}

// Collect fetches all remaining pages and returns all items concatenated.
// Convenience method for callers that need all results at once.
func (iterator *PageIterator[T]) Collect(ctx context.Context) ([]T, error) {
	var all []T
	for {
		items, err := iterator.Next(ctx)
		if err != nil {
			return all, err
		}
		if items == nil {
			return all, nil
		}
		all = append(all, items...)
	}
}

// parseLinkNext extracts the URL with rel="next" from an RFC 5988 Link
// header. Returns empty string if no next link is present.
//
// Format: <https://api.github.com/...?page=2>; rel="next", <...>; rel="last"
func parseLinkNext(header string) string {
	if header == "" {
		return ""
	}

	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)

		// Each part is: <url>; rel="type"
		segments := strings.SplitN(part, ";", 2)
		if len(segments) != 2 {
			continue
		}

		urlPart := strings.TrimSpace(segments[0])
		relPart := strings.TrimSpace(segments[1])

		if !strings.Contains(relPart, `rel="next"`) {
			continue
		}

		// Extract URL from angle brackets.
		if strings.HasPrefix(urlPart, "<") && strings.HasSuffix(urlPart, ">") {
			return urlPart[1 : len(urlPart)-1]
		}
	}

	return ""
}
