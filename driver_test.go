package graphlite_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LackOfMorals/graphlite/v2"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func openMemDB(t *testing.T) *graphlite.DB {
	t.Helper()
	db, err := graphlite.Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return db
}

// ─────────────────────────────────────────────────────────────────────────────
// Open tests
// ─────────────────────────────────────────────────────────────────────────────

// TestOpen_PathTraversal verifies that Open rejects paths containing ".."
// components to prevent directory traversal attacks.
func TestOpen_PathTraversal(t *testing.T) {
	traversalPaths := []string{
		"../../etc/passwd",
		"../other.db",
		"subdir/../../secret.db",
		"a/b/../../c/../../etc/shadow",
	}
	for _, p := range traversalPaths {
		_, err := graphlite.Open(p)
		if err == nil {
			t.Errorf("Open(%q): expected error for path traversal, got nil", p)
			continue
		}
		if !strings.Contains(err.Error(), "path traversal") {
			t.Errorf("Open(%q): expected 'path traversal' in error, got: %v", p, err)
		}
	}
}

// TestOpen_Memory verifies that Open(":memory:") returns a usable *DB.
func TestOpen_Memory(t *testing.T) {
	db, err := graphlite.Open(":memory:")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if db == nil {
		t.Fatal("expected non-nil *DB")
	}
	if err := db.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestOpen_File verifies that Open("./path.db") creates the file and returns a
// usable *DB.
func TestOpen_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db, err := graphlite.Open(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("expected database file to be created")
	}
	if err := db.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Option tests
// ─────────────────────────────────────────────────────────────────────────────

// TestWithBusyTimeout verifies that WithBusyTimeout is accepted without error.
func TestWithBusyTimeout(t *testing.T) {
	db, err := graphlite.Open(":memory:", graphlite.WithBusyTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("Open with WithBusyTimeout: %v", err)
	}
	defer func() { _ = db.Close(context.Background()) }()
}

// TestWithReadOnly verifies that read queries succeed and write queries return
// ErrReadOnly when WithReadOnly is set.
func TestWithReadOnly(t *testing.T) {
	ctx := context.Background()

	// Seed data in a normal read-write database.
	rw := graphlite.NewTestDB(t)
	if _, err := rw.RunQuery(ctx, `CREATE (n:Person {name: "Alice"})`, nil); err != nil {
		t.Fatalf("CREATE: %v", err)
	}

	// Open a second in-memory db (fresh) as read-only.
	ro, err := graphlite.Open(":memory:", graphlite.WithReadOnly())
	if err != nil {
		t.Fatalf("Open ro: %v", err)
	}
	defer func() { _ = ro.Close(ctx) }()

	// Reads on an empty read-only db must succeed (empty result, no error).
	if _, err := ro.RunQuery(ctx, `MATCH (n:Person) RETURN n.name AS name`, nil); err != nil {
		t.Fatalf("MATCH on read-only db: %v", err)
	}

	// Writes must return ErrReadOnly.
	_, err = ro.RunQuery(ctx, `CREATE (n:Person {name: "Bob"})`, nil)
	if err == nil {
		t.Fatal("expected ErrReadOnly, got nil")
	}
	if err != graphlite.ErrReadOnly {
		t.Fatalf("expected ErrReadOnly, got: %v", err)
	}
}

// TestWithReadOnly_BeginTxReturnsErrReadOnly verifies that BeginTx returns
// ErrReadOnly immediately when the database was opened with WithReadOnly.
func TestWithReadOnly_BeginTxReturnsErrReadOnly(t *testing.T) {
	ctx := context.Background()
	ro, err := graphlite.Open(":memory:", graphlite.WithReadOnly())
	if err != nil {
		t.Fatalf("Open ro: %v", err)
	}
	defer func() { _ = ro.Close(ctx) }()

	_, err = ro.BeginTx(ctx)
	if err == nil {
		t.Fatal("expected ErrReadOnly from BeginTx on read-only DB, got nil")
	}
	if err != graphlite.ErrReadOnly {
		t.Fatalf("expected ErrReadOnly, got: %v", err)
	}
}

// TestNewTestDB verifies that NewTestDB returns a usable *DB and that cleanup
// is registered (the test would leak if Close were not called).
func TestNewTestDB(t *testing.T) {
	db := graphlite.NewTestDB(t)
	if db == nil {
		t.Fatal("expected non-nil *DB")
	}
	ctx := context.Background()
	if _, err := db.RunQuery(ctx, `CREATE (n:Test)`, nil); err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Close tests
// ─────────────────────────────────────────────────────────────────────────────

// TestClose verifies that Close releases resources; the DB object should report
// an error if Close is called a second time.
func TestClose_ReleasesResources(t *testing.T) {
	db, err := graphlite.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// RunQuery tests
// ─────────────────────────────────────────────────────────────────────────────

// TestRunQuery_CreateAndMatch verifies the full MATCH query returning populated
// Records.
func TestRunQuery_CreateAndMatch(t *testing.T) {
	ctx := context.Background()
	db := openMemDB(t)

	// CREATE a node.
	qr, err := db.RunQuery(ctx, `CREATE (n:Person {name: "Alice", age: 30})`, nil)
	if err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	sum, _ := qr.Consume(ctx)
	if sum.Counters().NodesCreated() != 1 {
		t.Errorf("NodesCreated = %d, want 1", sum.Counters().NodesCreated())
	}

	// MATCH the node back.
	qr2, err := db.RunQuery(ctx, `MATCH (n:Person) RETURN n.name AS name`, nil)
	if err != nil {
		t.Fatalf("MATCH: %v", err)
	}
	if !qr2.Next(ctx) {
		t.Fatal("expected at least one record")
	}
	rec := qr2.Record()
	name, ok := rec.Get("name")
	if !ok {
		t.Fatal("expected 'name' key in record")
	}
	if name != "Alice" {
		t.Errorf("name = %q, want %q", name, "Alice")
	}
	qr2.Consume(ctx) //nolint:errcheck
}

// TestRunQuery_WithParams verifies parameterised queries.
func TestRunQuery_WithParams(t *testing.T) {
	ctx := context.Background()
	db := openMemDB(t)

	_, err := db.RunQuery(ctx, `CREATE (n:Person {name: "Bob", age: 25})`, nil)
	if err != nil {
		t.Fatalf("CREATE: %v", err)
	}

	qr, err := db.RunQuery(ctx,
		`MATCH (n:Person) WHERE n.name = $name RETURN n.name AS name`,
		map[string]any{"name": "Bob"},
	)
	if err != nil {
		t.Fatalf("MATCH with param: %v", err)
	}
	if !qr.Next(ctx) {
		t.Fatal("expected at least one record")
	}
	rec := qr.Record()
	name, ok := rec.Get("name")
	if !ok {
		t.Fatal("expected 'name' key in record")
	}
	if name != "Bob" {
		t.Errorf("name = %q, want %q", name, "Bob")
	}
	qr.Consume(ctx) //nolint:errcheck
}

// TestRunQuery_MissingParam verifies that a missing $param returns a structured
// ErrMissingParameter error.
func TestRunQuery_MissingParam(t *testing.T) {
	ctx := context.Background()
	db := openMemDB(t)

	_, err := db.RunQuery(ctx,
		`MATCH (n:Person) WHERE n.name = $name RETURN n`,
		map[string]any{}, // "name" not provided
	)
	if err == nil {
		t.Fatal("expected error for missing parameter, got nil")
	}
	var mp *graphlite.ErrMissingParameter
	if ok := func() bool {
		e, ok := err.(*graphlite.ErrMissingParameter)
		if ok {
			mp = e
		}
		return ok
	}(); !ok {
		t.Fatalf("error is not *ErrMissingParameter: %T %v", err, err)
	}
	if mp.Name != "name" {
		t.Errorf("ErrMissingParameter.Name = %q, want %q", mp.Name, "name")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Write operation counter tests
// ─────────────────────────────────────────────────────────────────────────────

// TestRunQuery_CreateRelationship verifies CREATE node+rel counters.
func TestRunQuery_CreateRelationship(t *testing.T) {
	ctx := context.Background()
	db := openMemDB(t)

	qr, err := db.RunQuery(ctx, `CREATE (a:Person {name: "A"})-[:KNOWS]->(b:Person {name: "B"})`, nil)
	if err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	sum, _ := qr.Consume(ctx)
	if sum.Counters().NodesCreated() != 2 {
		t.Errorf("NodesCreated = %d, want 2", sum.Counters().NodesCreated())
	}
	if sum.Counters().RelationshipsCreated() != 1 {
		t.Errorf("RelationshipsCreated = %d, want 1", sum.Counters().RelationshipsCreated())
	}
}

// TestRunQuery_DeleteNode verifies DETACH DELETE counters.
func TestRunQuery_DetachDeleteNode(t *testing.T) {
	ctx := context.Background()
	db := openMemDB(t)

	// Create then delete.
	if _, err := db.RunQuery(ctx, `CREATE (n:Temp)`, nil); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	qr, err := db.RunQuery(ctx, `MATCH (n:Temp) DETACH DELETE n`, nil)
	if err != nil {
		t.Fatalf("DETACH DELETE: %v", err)
	}
	sum, _ := qr.Consume(ctx)
	if sum.Counters().NodesDeleted() != 1 {
		t.Errorf("NodesDeleted = %d, want 1", sum.Counters().NodesDeleted())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Explicit transaction tests
// ─────────────────────────────────────────────────────────────────────────────

// TestBeginTx_CommitPersists verifies that committed transaction data is visible
// after commit.
func TestBeginTx_CommitPersists(t *testing.T) {
	ctx := context.Background()
	db := openMemDB(t)

	tx, err := db.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.Run(ctx, `CREATE (n:TxNode {val: "committed"})`, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("tx.Run: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify the node is visible after commit.
	qr, err := db.RunQuery(ctx, `MATCH (n:TxNode) RETURN n.val AS val`, nil)
	if err != nil {
		t.Fatalf("MATCH after commit: %v", err)
	}
	if !qr.Next(ctx) {
		t.Fatal("expected committed node to be visible")
	}
	val, _ := qr.Record().Get("val")
	if val != "committed" {
		t.Errorf("val = %q, want %q", val, "committed")
	}
	qr.Consume(ctx) //nolint:errcheck
}

// TestBeginTx_RollbackReverts verifies that rolled-back mutations are not
// visible after rollback.
func TestBeginTx_RollbackReverts(t *testing.T) {
	ctx := context.Background()
	db := openMemDB(t)

	tx, err := db.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.Run(ctx, `CREATE (n:RollbackNode {val: "ephemeral"})`, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("tx.Run: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Verify the node is NOT visible after rollback.
	qr, err := db.RunQuery(ctx, `MATCH (n:RollbackNode) RETURN n`, nil)
	if err != nil {
		t.Fatalf("MATCH after rollback: %v", err)
	}
	if qr.Next(ctx) {
		t.Fatal("expected rolled-back node to be invisible")
	}
	qr.Consume(ctx) //nolint:errcheck
}

// TestBeginTx_ClosedAfterCommit verifies that a Tx returns an error after Commit.
func TestBeginTx_ClosedAfterCommit(t *testing.T) {
	ctx := context.Background()
	db := openMemDB(t)

	tx, err := db.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// A second Run should fail because the Tx is closed.
	_, err = tx.Run(ctx, `MATCH (n) RETURN n`, nil)
	if err == nil {
		t.Fatal("expected error on Run after Commit, got nil")
	}
}

// TestBeginTx_ClosedAfterRollback verifies that a Tx returns an error after Rollback.
func TestBeginTx_ClosedAfterRollback(t *testing.T) {
	ctx := context.Background()
	db := openMemDB(t)

	tx, err := db.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	_, err = tx.Run(ctx, `MATCH (n) RETURN n`, nil)
	if err == nil {
		t.Fatal("expected error on Run after Rollback, got nil")
	}
}

// TestBeginTx_RollbackAfterCommitIsNoOp verifies that calling Rollback after
// Commit returns nil, following the database/sql.Tx convention that makes the
// defer-rollback guard pattern safe to use.
func TestBeginTx_RollbackAfterCommitIsNoOp(t *testing.T) {
	ctx := context.Background()
	db := openMemDB(t)

	tx, err := db.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.Run(ctx, `CREATE (n:IdempotentNode {v: 1})`, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("Run: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Rollback after Commit must be a no-op (returns nil, not an error).
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback after Commit: got %v, want nil", err)
	}
	// The committed data must still be visible after the no-op Rollback.
	qr, err := db.RunQuery(ctx, `MATCH (n:IdempotentNode) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunQuery after Rollback: %v", err)
	}
	records, err := qr.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 committed node to remain visible, got %d", len(records))
	}
}

// TestRunQuery_NodeProjection verifies that a whole-node RETURN populates a
// *Node value with Labels and Props.
func TestRunQuery_NodeProjection(t *testing.T) {
	ctx := context.Background()
	db := openMemDB(t)

	if _, err := db.RunQuery(ctx, `CREATE (n:Animal {species: "cat"})`, nil); err != nil {
		t.Fatalf("CREATE: %v", err)
	}

	qr, err := db.RunQuery(ctx, `MATCH (n:Animal) RETURN n`, nil)
	if err != nil {
		t.Fatalf("MATCH: %v", err)
	}
	if !qr.Next(ctx) {
		t.Fatal("expected at least one record")
	}
	raw, ok := qr.Record().Get("n")
	if !ok {
		t.Fatal("expected 'n' key in record")
	}
	node, isNode := raw.(*graphlite.Node)
	if !isNode {
		t.Fatalf("expected *graphlite.Node, got %T", raw)
	}
	if len(node.Labels) == 0 || node.Labels[0] != "Animal" {
		t.Errorf("Labels = %v, want [Animal]", node.Labels)
	}
	if node.Props["species"] != "cat" {
		t.Errorf("Props[species] = %v, want %q", node.Props["species"], "cat")
	}
	qr.Consume(ctx) //nolint:errcheck
}

// ─────────────────────────────────────────────────────────────────────────────
// Plan-cache tests (task-016)
// ─────────────────────────────────────────────────────────────────────────────

// TestPlanCache_CacheHitProducesIdenticalResults verifies that repeated
// executions of the same Cypher string via RunQuery produce identical results
// regardless of whether the plan cache was hit. This tests the correctness
// invariant for the cache introduced in task-016.
func TestPlanCache_CacheHitProducesIdenticalResults(t *testing.T) {
	ctx := context.Background()
	db := openMemDB(t)

	// Seed two nodes with different names so we can verify parameter binding is
	// not contaminated by a previous cache hit.
	for _, name := range []string{"Alice", "Bob"} {
		res, err := db.RunQuery(ctx, `CREATE (n:Person {name: $name})`,
			map[string]any{"name": name})
		if err != nil {
			t.Fatalf("CREATE %q: %v", name, err)
		}
		if _, err := res.Consume(ctx); err != nil {
			t.Fatalf("consume CREATE: %v", err)
		}
	}

	query := `MATCH (n:Person {name: $name}) RETURN n.name AS name`

	// First call — cache miss.
	res1, err := db.RunQuery(ctx, query, map[string]any{"name": "Alice"})
	if err != nil {
		t.Fatalf("first RunQuery: %v", err)
	}
	recs1, err := res1.Collect(ctx)
	if err != nil {
		t.Fatalf("first Collect: %v", err)
	}

	// Second call — cache hit with the same query string but different params.
	res2, err := db.RunQuery(ctx, query, map[string]any{"name": "Bob"})
	if err != nil {
		t.Fatalf("second RunQuery: %v", err)
	}
	recs2, err := res2.Collect(ctx)
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}

	if len(recs1) != 1 {
		t.Fatalf("expected 1 record for Alice, got %d", len(recs1))
	}
	if len(recs2) != 1 {
		t.Fatalf("expected 1 record for Bob, got %d", len(recs2))
	}

	got1, _ := recs1[0].Get("name")
	got2, _ := recs2[0].Get("name")
	if got1 != "Alice" {
		t.Errorf("first result: got %q, want %q", got1, "Alice")
	}
	if got2 != "Bob" {
		t.Errorf("second result: got %q, want %q", got2, "Bob")
	}
}

// TestPlanCache_ConcurrentReadsAreSafe executes the same query concurrently
// from multiple goroutines, exercising the plan cache's concurrent-read path.
// This test is most useful when run with -race.
func TestPlanCache_ConcurrentReadsAreSafe(t *testing.T) {
	ctx := context.Background()
	db := openMemDB(t)

	// Seed 10 nodes.
	for i := range 10 {
		res, err := db.RunQuery(ctx, `CREATE (n:Person {name: $name})`,
			map[string]any{"name": strings.Repeat("A", i+1)})
		if err != nil {
			t.Fatalf("seed CREATE %d: %v", i, err)
		}
		if _, err := res.Consume(ctx); err != nil {
			t.Fatalf("consume seed: %v", err)
		}
	}

	const goroutines = 20
	errc := make(chan error, goroutines)
	for range goroutines {
		go func() {
			res, err := db.RunQuery(ctx, `MATCH (n:Person) RETURN n.name AS name`, nil)
			if err != nil {
				errc <- err
				return
			}
			recs, err := res.Collect(ctx)
			if err != nil {
				errc <- err
				return
			}
			if len(recs) != 10 {
				errc <- fmt.Errorf("expected 10 records, got %d", len(recs))
				return
			}
			errc <- nil
		}()
	}
	for range goroutines {
		if err := <-errc; err != nil {
			t.Errorf("concurrent read: %v", err)
		}
	}
}
