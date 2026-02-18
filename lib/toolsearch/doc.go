// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package toolsearch provides relevance-ranked search over tool
// definitions using BM25 (Okapi variant). The index accepts tool
// documents with name, description, and argument metadata, then
// scores natural language queries against them using term-frequency
// and inverse-document-frequency weighting.
//
// Three consumers share this package:
//   - The MCP server's progressive disclosure meta-tools
//     (bureau_tools_list with a query parameter)
//   - The native agent loop's tool search strategy
//     (selecting which tools to include in LLM requests)
//   - The CLI's `bureau suggest` command and unknown-command
//     fallback (natural language command discovery)
package toolsearch
