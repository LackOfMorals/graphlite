package graphlite

import (
	"context"
	"testing"
	"time"
)

// Option is a functional option for configuring a graphlite database.
// Pass one or more Options to [Open] to customise behaviour.
type Option func(*dbConfig)

type dbConfig struct {
	busyTimeout  time.Duration
	readOnly     bool
	maxPathHops  int
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

// WithMaxPathHops sets the maximum number of hops allowed in variable-length
// Cypher path patterns such as MATCH (a)-[*1..n]->(b). By default the cap is
// 15 hops. Explicit bounds in the Cypher query that exceed the cap return an
// error rather than executing a potentially unbounded recursive CTE.
//
// Passing n <= 0 is a no-op; the built-in default of 15 remains in effect.
// To lower the cap below 15, pass a positive n smaller than 15.
//
// WARNING: raising this cap above the default of 15 can cause exponential
// query execution time on dense graphs. Only increase it when the graph
// topology is known to be sparse and the traversal depth is bounded by other
// constraints (e.g. relationship type filters or node label filters).
func WithMaxPathHops(n int) Option {
	return func(c *dbConfig) {
		if n > 0 {
			c.maxPathHops = n
		}
	}
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
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return db
}
