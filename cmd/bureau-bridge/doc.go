// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Bureau-bridge bridges TCP connections to a unix socket, enabling HTTP
// clients inside network-isolated sandboxes to reach the credential
// proxy via localhost. Supports standalone mode (listen and forward) and
// exec mode (start bridge then exec a child command).
package main
