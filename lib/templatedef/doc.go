// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package templatedef provides template resolution for Bureau sandbox
// templates. Templates are stored as m.bureau.template state events in
// Matrix rooms and support multi-level inheritance. This package
// fetches templates from Matrix and walks the inheritance chain to
// produce a fully-merged [schema.TemplateContent].
//
// [Resolve] is the primary entry point: given a template reference
// string (e.g., "bureau/template:base"), it follows the Inherits
// chain to the root and merges from base to leaf. Cycle detection
// prevents infinite loops.
//
// [Fetch] resolves a single template without walking inheritance.
//
// [Merge] applies child over parent with these rules:
//
//   - Scalars (Description, Command, Environment): child replaces
//   - Maps (EnvironmentVariables, Roles, DefaultPayload): merged,
//     child wins on key conflict
//   - Slices (Filesystem, CreateDirs, RequiredCredentials,
//     RequiredServices): child appended, deduplicated
//   - Pointers (Namespaces, Resources, Security): child replaces
//   - Inherits is cleared after resolution
//
// The daemon uses Resolve to produce [schema.SandboxSpec] for sandbox
// creation. The CLI uses it for template inspection.
//
// Depends on lib/schema and messaging.
package templatedef
