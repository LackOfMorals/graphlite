package graphlite

import (
	"testing"
	"time"
)

// Option is a functional option for configuring a graphlite database.
// Pass one or more Options to Open or NewDriver to customise behaviour.
type Option func(*dbConfig)

type dbConfig struct {
	busyTimeout time.Duration
	readOnly    bool
}

// WithBusyTimeout sets the SQLite busy_timeout pragma. When a write operation
// encounters a locked database, SQLite will retry for up to d before returning
// an error. The default (zero) uses SQLite's built-in behaviour (no retry).
//
// Useful when multiple goroutines or processes share the same database file.
func WithBusyTimeout(d time.Duration) Option {
	return func(c *dbConfig) { c.busyTimeout = d }
}

// WithReadOnly opens the database in read-only mode by setting
// PRAGMA query_only=ON. All INSERT, UPDATE, and DELETE statements are rejected
// by SQLite with a permission error. The database file must already exist and
// contain the graphlite schema.
//
// Read-only mode is enforced at the SQLite level, not in application code.
func WithReadOnly() Option {
	return func(c *dbConfig) { c.readOnly = true }
}

// NewTestDB opens an in-memory graphlite database, registers db.Close with
// t.Cleanup, and returns a ready-to-use *DB. t.Fatal is called on any error.
//
// This is the recommended way to create a graphlite database in tests:
//
//	func TestMyFeature(t *testing.T) {
//	    db := graphlite.NewTestDB(t)
//	    // db is closed automatically when the test ends
//	}
func NewTestDB(t testing.TB, opts ...Option) *DB {
	t.Helper()
	db, err := Open(":memory:", opts...)
	if err != nil {
		t.Fatalf("graphlite.NewTestDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
