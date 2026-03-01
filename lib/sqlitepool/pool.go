// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sqlitepool

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Config holds the parameters for opening a SQLite connection pool.
// Path is required; all other fields have sensible defaults.
type Config struct {
	// Path is the filesystem path to the SQLite database file. The
	// parent directory must exist. The file is created if it does not
	// exist. Use ":memory:" for an in-memory database (useful in
	// tests, but the pool size must be 1 since each in-memory
	// connection is independent).
	Path string

	// PoolSize is the number of connections in the pool. If zero or
	// negative, defaults to max(runtime.NumCPU(), 4). For write-heavy
	// workloads, 4-8 is usually sufficient because SQLite serializes
	// writes regardless of pool size. Extra connections help when
	// multiple goroutines need concurrent reads.
	PoolSize int

	// Logger receives operational messages (pool open/close, pragma
	// errors). If nil, a no-op logger is used.
	Logger *slog.Logger

	// OnConnect is called once per connection after standard pragmas
	// are applied. Use this for service-specific setup: schema
	// creation, custom function registration, additional pragmas.
	// If OnConnect returns an error, the connection is discarded and
	// the error is returned to the caller of Take.
	OnConnect func(conn *sqlite.Conn) error
}

// Pool is a fixed-size pool of SQLite connections with Bureau-standard
// pragmas. It wraps sqlitex.Pool and exposes the same Take/Put API.
//
// Pool is safe for concurrent use. Individual connections are not —
// each goroutine must Take its own connection and Put it back when done.
type Pool struct {
	inner  *sqlitex.Pool
	logger *slog.Logger
	path   string
}

// Open creates a new connection pool and applies Bureau-standard
// pragmas to every connection. The database file is created if it does
// not exist. All connections are initialized lazily on first Take.
//
// Open validates the configuration and returns an error if Path is
// empty or the database cannot be opened. The caller must call Close
// when the pool is no longer needed.
func Open(cfg Config) (*Pool, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("sqlitepool: Path is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	poolSize := cfg.PoolSize
	if poolSize <= 0 {
		poolSize = runtime.NumCPU()
		if poolSize < 4 {
			poolSize = 4
		}
	}

	inner, err := sqlitex.NewPool(cfg.Path, sqlitex.PoolOptions{
		PoolSize: poolSize,
		PrepareConn: func(conn *sqlite.Conn) error {
			return prepareConnection(conn, cfg.OnConnect)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sqlitepool: opening %s: %w", cfg.Path, err)
	}

	logger.Info("sqlite pool opened",
		"path", cfg.Path,
		"pool_size", poolSize,
	)

	return &Pool{
		inner:  inner,
		logger: logger,
		path:   cfg.Path,
	}, nil
}

// Take borrows a connection from the pool. Blocks until a connection
// is available or ctx is cancelled. The caller MUST call Put when done
// with the connection, typically via defer:
//
//	conn, err := pool.Take(ctx)
//	if err != nil {
//	    return err
//	}
//	defer pool.Put(conn)
func (p *Pool) Take(ctx context.Context) (*sqlite.Conn, error) {
	conn, err := p.inner.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlitepool: take: %w", err)
	}
	return conn, nil
}

// Put returns a connection to the pool. Safe to call with nil (no-op).
// After Put, the caller must not use the connection.
func (p *Pool) Put(conn *sqlite.Conn) {
	p.inner.Put(conn)
}

// Close closes all connections in the pool. Blocks until all borrowed
// connections are returned. After Close, Take returns an error.
func (p *Pool) Close() error {
	err := p.inner.Close()
	if err != nil {
		p.logger.Error("sqlite pool close error",
			"path", p.path,
			"error", err,
		)
		return fmt.Errorf("sqlitepool: closing %s: %w", p.path, err)
	}
	p.logger.Info("sqlite pool closed", "path", p.path)
	return nil
}

// prepareConnection applies Bureau-standard pragmas and then calls the
// optional OnConnect callback. This runs once per connection in the
// pool, on first use.
func prepareConnection(conn *sqlite.Conn, onConnect func(*sqlite.Conn) error) error {
	// WAL mode: concurrent readers, single writer, no reader blocking.
	// This is set via OpenFlags by default in zombiezen, but we set it
	// explicitly via PRAGMA for clarity and to handle databases that
	// were created without the WAL flag.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=OFF",
		"PRAGMA cache_size=-8192",
		"PRAGMA mmap_size=268435456",
		"PRAGMA temp_store=MEMORY",
	}

	for _, pragma := range pragmas {
		if err := sqlitex.ExecuteTransient(conn, pragma, nil); err != nil {
			return fmt.Errorf("sqlitepool: %s: %w", pragma, err)
		}
	}

	if onConnect != nil {
		if err := onConnect(conn); err != nil {
			return fmt.Errorf("sqlitepool: OnConnect: %w", err)
		}
	}

	return nil
}
