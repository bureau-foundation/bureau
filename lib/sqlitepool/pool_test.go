// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package sqlitepool_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"

	"github.com/bureau-foundation/bureau/lib/sqlitepool"
)

func TestOpenAndClose(t *testing.T) {
	pool := openTestPool(t, nil)

	conn, err := pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	defer pool.Put(conn)

	// Verify WAL mode is active.
	var journalMode string
	err = sqlitex.Execute(conn, "PRAGMA journal_mode", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			journalMode = stmt.ColumnText(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want %q", journalMode, "wal")
	}

	// Verify synchronous is NORMAL (1).
	var synchronous int
	err = sqlitex.Execute(conn, "PRAGMA synchronous", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			synchronous = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("PRAGMA synchronous: %v", err)
	}
	if synchronous != 1 {
		t.Errorf("synchronous = %d, want 1 (NORMAL)", synchronous)
	}
}

func TestOnConnect(t *testing.T) {
	var called bool
	pool := openTestPool(t, func(conn *sqlite.Conn) error {
		called = true
		return sqlitex.ExecuteScript(conn, `
			CREATE TABLE IF NOT EXISTS test_table (
				id INTEGER PRIMARY KEY,
				value TEXT NOT NULL
			);
		`, nil)
	})

	conn, err := pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	defer pool.Put(conn)

	if !called {
		t.Error("OnConnect was not called")
	}

	// Verify the table exists by inserting a row.
	err = sqlitex.Execute(conn, "INSERT INTO test_table (value) VALUES (?)", &sqlitex.ExecOptions{
		Args: []any{"hello"},
	})
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}
}

func TestConcurrentReads(t *testing.T) {
	pool := openTestPool(t, func(conn *sqlite.Conn) error {
		return sqlitex.ExecuteScript(conn, `
			CREATE TABLE IF NOT EXISTS numbers (value INTEGER NOT NULL);
		`, nil)
	})

	// Insert test data once via a single connection.
	conn, err := pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Take for setup: %v", err)
	}
	err = sqlitex.ExecuteScript(conn, `
		INSERT INTO numbers (value) VALUES (1), (2), (3), (4), (5);
	`, nil)
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	pool.Put(conn)

	// Read from multiple goroutines simultaneously.
	const goroutineCount = 8
	var waitGroup sync.WaitGroup
	errors := make(chan error, goroutineCount)

	for range goroutineCount {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()

			conn, err := pool.Take(context.Background())
			if err != nil {
				errors <- err
				return
			}
			defer pool.Put(conn)

			var sum int64
			err = sqlitex.Execute(conn, "SELECT value FROM numbers", &sqlitex.ExecOptions{
				ResultFunc: func(stmt *sqlite.Stmt) error {
					sum += stmt.ColumnInt64(0)
					return nil
				},
			})
			if err != nil {
				errors <- err
				return
			}
			if sum != 15 {
				errors <- fmt.Errorf("sum = %d, want 15", sum)
			}
		}()
	}

	waitGroup.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}
}

func TestEmptyPathRejected(t *testing.T) {
	_, err := sqlitepool.Open(sqlitepool.Config{})
	if err == nil {
		t.Fatal("expected error for empty Path")
	}
}

func TestContextCancellation(t *testing.T) {
	pool, err := sqlitepool.Open(sqlitepool.Config{
		Path:     filepath.Join(t.TempDir(), "cancel.db"),
		PoolSize: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	conn, err := pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}

	// Try to take a second connection with a cancelled context.
	// The pool has size 1, so this should block then fail.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = pool.Take(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}

	pool.Put(conn)
}

// openTestPool creates a pool backed by a temporary database file.
// The pool is closed automatically when the test completes.
func openTestPool(t *testing.T, onConnect func(*sqlite.Conn) error) *sqlitepool.Pool {
	t.Helper()

	pool, err := sqlitepool.Open(sqlitepool.Config{
		Path:      filepath.Join(t.TempDir(), "test.db"),
		PoolSize:  4,
		OnConnect: onConnect,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return pool
}
