package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LackOfMorals/graphlite/v2/store"
)

// TestOpenMemory verifies that Open(":memory:") returns a usable store.
func TestOpenMemory(t *testing.T) {
	s, err := store.Open(":memory:", store.Config{})
	if err != nil {
		t.Fatalf("Open(:memory:) error: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Verify the store is functional by inserting a node.
	ctx := context.Background()
	id, err := s.InsertNode(ctx, store.DecodeLabels("Test"), `{"x":1}`)
	if err != nil {
		t.Fatalf("InsertNode: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive node ID, got %d", id)
	}
}

// TestOpenFilePath verifies that Open with a file path creates the database.
func TestOpenFilePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	s, err := store.Open(path, store.Config{})
	if err != nil {
		t.Fatalf("Open(%q) error: %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Verify the file was created.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatalf("expected database file %q to exist after Open", path)
	}

	// Verify the store is functional.
	ctx := context.Background()
	id, err := s.InsertNode(ctx, store.DecodeLabels("FileTest"), `{}`)
	if err != nil {
		t.Fatalf("InsertNode: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive node ID, got %d", id)
	}
}

// TestWALMode verifies that WAL journal mode is enabled after Open.
func TestWALMode(t *testing.T) {
	s, err := store.Open(":memory:", store.Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var mode string
	row := s.DB().QueryRow("PRAGMA journal_mode;")
	if err := row.Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	// SQLite in-memory databases report "memory" for journal mode even with WAL
	// requested, because WAL requires a persistent file. For file-based stores
	// the mode would be "wal". We accept both here; the important thing is that
	// the PRAGMA call did not error.
	if mode != "wal" && mode != "memory" {
		t.Errorf("unexpected journal_mode %q (want wal or memory)", mode)
	}
}

// TestWALModeFile verifies that WAL journal mode is enabled for file stores.
func TestWALModeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.db")

	s, err := store.Open(path, store.Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var mode string
	row := s.DB().QueryRow("PRAGMA journal_mode;")
	if err := row.Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("expected journal_mode=wal, got %q", mode)
	}
}

// TestSchemaTablesAndIndexes verifies that all required tables and indexes exist.
func TestSchemaTablesAndIndexes(t *testing.T) {
	s, err := store.Open(":memory:", store.Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	db := s.DB()

	// Check tables exist — including the node_labels junction table.
	for _, table := range []string{"nodes", "edges", "node_labels"} {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}

	// Check all required indexes exist.
	for _, idx := range []string{
		"idx_nodes_labels",
		"idx_edges_start",
		"idx_edges_end",
		"idx_edges_type",
		"idx_node_labels_label",
	} {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx,
		).Scan(&name)
		if err != nil {
			t.Errorf("index %q not found: %v", idx, err)
		}
	}
}

// TestStoreInterfaceCompliance checks that *SQLiteStore satisfies Store at compile time.
// This is a compile-time check only; the test body is empty.
func TestStoreInterfaceCompliance(t *testing.T) {
	var _ store.Store = (*store.SQLiteStore)(nil)
}

// TestTransactionBeginCommit verifies basic transaction begin/commit behaviour.
func TestTransactionBeginCommit(t *testing.T) {
	s, err := store.Open(":memory:", store.Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()

	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	id, err := tx.InsertNode(ctx, store.DecodeLabels("TxNode"), `{"key":"value"}`)
	if err != nil {
		t.Fatalf("tx.InsertNode: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify the node persisted after commit.
	n, err := s.GetNode(ctx, id)
	if err != nil {
		t.Fatalf("GetNode after commit: %v", err)
	}
	if n.ID != id {
		t.Errorf("expected node ID %d, got %d", id, n.ID)
	}
}

// TestTransactionRollback verifies that rollback reverts inserts.
func TestTransactionRollback(t *testing.T) {
	s, err := store.Open(":memory:", store.Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()

	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	id, err := tx.InsertNode(ctx, store.DecodeLabels("Ephemeral"), `{}`)
	if err != nil {
		t.Fatalf("tx.InsertNode: %v", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Verify the node was not persisted after rollback.
	_, err = s.GetNode(ctx, id)
	if err == nil {
		t.Fatalf("expected GetNode to return error after rollback, got nil")
	}
}
