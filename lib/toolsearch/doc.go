// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package toolsearch provides relevance-ranked search over tool
// definitions. It maps tool-specific fields (name, description,
// argument names and descriptions) to weighted BM25 documents using
// the generic [bm25] package, applying field-specific weights so
// that tool names carry more influence than argument descriptions.
//
// Three consumers share this package:
//   - The MCP server's progressive disclosure meta-tools
//     (bureau_tools_list with a query parameter)
//   - The native agent loop's tool search strategy
//     (selecting which tools to include in LLM requests)
//   - The CLI's `bureau suggest` command and unknown-command
//     fallback (natural language command discovery)
package toolsearch
