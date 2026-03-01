// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// Package sqlitepool provides a Bureau-standard SQLite connection pool.
//
// Every Bureau service that needs local structured storage uses this
// package. It wraps zombiezen.com/go/sqlite with production-ready
// defaults: WAL journal mode, NORMAL synchronous for process-crash
// durability without fsync-per-commit overhead, memory-mapped I/O for
// read performance, and busy timeout to handle write contention
// gracefully.
//
// The pool is built on zombiezen's sqlitex.Pool, which manages a
// fixed-size set of connections. Callers [Pool.Take] a connection,
// perform work, and [Pool.Put] it back. Connections are NOT safe for
// concurrent use — each goroutine must hold its own connection for the
// duration of its work.
//
// # Pragmas
//
// Every connection in the pool is initialized with these pragmas:
//
//   - journal_mode=WAL: write-ahead logging for concurrent readers and
//     a single writer. Reads never block writes; writes never block
//     reads.
//   - synchronous=NORMAL: transactions survive process crashes. Not
//     durable across OS crashes or power failure — acceptable for
//     Bureau's telemetry, caching, and index use cases where the source
//     of truth is Matrix events or the CBOR ingest stream.
//   - busy_timeout=5000: wait up to 5 seconds for a write lock instead
//     of returning SQLITE_BUSY immediately.
//   - foreign_keys=OFF: Bureau services manage referential integrity
//     explicitly. FK cascades in materialized views are a footgun.
//   - cache_size=-8192: 8 MB page cache per connection.
//   - mmap_size=268435456: 256 MB memory-mapped I/O for reads. On Linux
//     this avoids read(2) syscall overhead by letting the OS page cache
//     serve reads directly.
//   - temp_store=MEMORY: temporary tables and indexes in memory.
//
// # Usage
//
//	pool, err := sqlitepool.Open(sqlitepool.Config{
//	    Path:     "/var/bureau/telemetry/telemetry.db",
//	    PoolSize: 8,
//	    Logger:   logger,
//	    OnConnect: func(conn *sqlite.Conn) error {
//	        // Create tables, register functions, etc.
//	        return sqlitex.ExecuteScript(conn, schema, nil)
//	    },
//	})
//	if err != nil {
//	    return err
//	}
//	defer pool.Close()
//
//	conn, err := pool.Take(ctx)
//	if err != nil {
//	    return err
//	}
//	defer pool.Put(conn)
//
// # Design
//
// This package is intentionally thin: it applies standard pragmas and
// exposes the underlying zombiezen types directly. There is no attempt
// to abstract away SQLite's connection model or invent a query builder.
// Services write SQL, use sqlitex.Execute for cached statements, and
// manage transactions with sqlitex.ImmediateTransaction. The goal is a
// shared foundation (one dependency, one set of pragmas, one pool
// pattern) without an abstraction layer that fights SQLite's strengths.
package sqlitepool
