// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau's unified model inference gateway. Routes completion and
// embedding requests to external providers (OpenRouter, OpenAI) and
// local inference engines (llama.cpp, vLLM) with per-project
// accounting, quota enforcement, and stable model aliasing.
//
// The model service exposes a native CBOR API on a Unix service socket.
// Agents send typed Request messages (keyasint-encoded CBOR) and
// receive streaming Response messages for completions or single
// EmbedResponse messages for embeddings.
//
// Configuration (providers, aliases, accounts) arrives via Matrix state
// events and incremental /sync. Credentials for external providers are
// delivered by the launcher's credential pipeline as files in a
// credentials directory.
//
// model-service.md defines the full design. lib/schema/model defines
// the wire protocol and configuration types. lib/modelregistry provides
// the in-memory index. lib/modelprovider implements the HTTP transport.
package main
