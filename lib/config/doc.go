// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package config provides YAML configuration loading for Bureau components.
//
// Configuration is loaded from a single file specified by either the
// BUREAU_CONFIG environment variable (via [Load]) or a --config flag
// (via [LoadFile]). There are no fallbacks, no ~/.config discovery,
// and no automatic file search. This ensures deterministic, auditable
// configuration with no hidden overrides.
//
// The configuration file supports environment-specific sections
// (development, staging, production) that override base values when
// [Config].Environment matches. Production defaults are stricter:
// AutoStart is disabled and missing user namespaces cause errors
// rather than being silently skipped.
//
// Variable expansion is performed on path fields after loading:
// ${HOME}, ${BUREAU_ROOT}, and ${VAR:-default} patterns are expanded.
// No other environment variables override config values.
//
// Key exports:
//
//   - [Config] -- master struct with Paths, Proxy, Sandbox, Launcher
//   - [Default] -- returns a Config with development defaults
//   - [Load] and [LoadFile] -- the two entry points for loading
//
// This package depends on no other Bureau packages.
package config
