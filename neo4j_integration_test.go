// Integration tests for the graphlite all-three-tier transaction API.
// All tests exercise every v0.1 Cypher feature through at least one of the
// three transaction tiers:
//
//   Tier 1 — graphlite.ExecuteQuery with EagerResultTransformer
//   Tier 2 — session.ExecuteRead / session.ExecuteWrite with ManagedTransaction
//   Tier 3 — session.BeginTransaction with explicit Commit / Rollback
package graphlite_test

import (
	"context"
	"fmt"
	"testing"

	graphlite "github.com/LackOfMorals/graphlite"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// newDriver creates an in-memory *DB and registers cleanup.
func newDriver(t *testing.T) *graphlite.DB {
	t.Helper()
	d, err := graphlite.NewDriver(":memory:", graphlite.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d
}

// newSession opens a session on d and registers cleanup.
func newSession(t *testing.T, d graphlite.Driver) graphlite.Session {
	t.Helper()
	ctx := context.Background()
	sess := d.NewSession(ctx)
	t.Cleanup(func() { _ = sess.Close(ctx) })
	return sess
}

// executeQueryEager is a convenience wrapper for the common Tier-1 pattern:
//
//	graphlite.ExecuteQuery[*graphlite.EagerResult](ctx, driver, cypher, params, graphlite.EagerResultTransformer)
func executeQueryEager(t *testing.T, ctx context.Context, d graphlite.Driver, cypher string, params map[string]any) *graphlite.EagerResult {
	t.Helper()
	result, err := graphlite.ExecuteQuery[*graphlite.EagerResult](ctx, d, cypher, params, graphlite.EagerResultTransformer)
	if err != nil {
		t.Fatalf("ExecuteQuery %q: %v", cypher, err)
	}
	return result
}

// requireRecordCount fails if result does not contain exactly n records.
func requireRecordCount(t *testing.T, result *graphlite.EagerResult, n int) {
	t.Helper()
	if len(result.Records) != n {
		t.Fatalf("expected %d records, got %d", n, len(result.Records))
	}
}

// requireGet returns the value for key from record, failing if absent.
func requireGet(t *testing.T, rec *graphlite.Record, key string) any {
	t.Helper()
	v, ok := rec.Get(key)
	if !ok {
		t.Fatalf("record missing key %q", key)
	}
	return v
}

// ─────────────────────────────────────────────────────────────────────────────
// Tier 1 — graphlite.ExecuteQuery tests
// ─────────────────────────────────────────────────────────────────────────────

// TestTier1_ExecuteQuery_CreateAndMatch exercises the most common Tier-1 pattern:
// write with ExecuteQuery, then read back.
func TestTier1_ExecuteQuery_CreateAndMatch(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	// Write: CREATE a node.
	executeQueryEager(t, ctx, d, `CREATE (n:Person {name: "Alice", age: 30})`, nil)

	// Read: MATCH and verify.
	res := executeQueryEager(t, ctx, d, `MATCH (n:Person) RETURN n.name AS name, n.age AS age`, nil)
	requireRecordCount(t, res, 1)

	name := requireGet(t, res.Records[0], "name")
	if name != "Alice" {
		t.Errorf("name = %v, want Alice", name)
	}
}

// TestTier1_ExecuteQuery_WithParams exercises $param binding through Tier-1.
func TestTier1_ExecuteQuery_WithParams(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	executeQueryEager(t, ctx, d,
		`CREATE (n:City {name: "London", country: "UK"})`, nil)

	res := executeQueryEager(t, ctx, d,
		`MATCH (n:City) WHERE n.country = $c RETURN n.name AS city`,
		map[string]any{"c": "UK"},
	)
	requireRecordCount(t, res, 1)
	if requireGet(t, res.Records[0], "city") != "London" {
		t.Errorf("expected London")
	}
}

// TestTier1_ExecuteQuery_ReadersRouting verifies that ExecuteQuery works for
// reads. graphlite has no reader/writer routing distinction; all paths use the
// same transaction machinery.
func TestTier1_ExecuteQuery_ReadersRouting(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	// Seed data.
	executeQueryEager(t, ctx, d, `CREATE (n:Book {title: "Go Programming"})`, nil)

	res := executeQueryEager(t, ctx, d, `MATCH (n:Book) RETURN n.title AS title`, nil)
	requireRecordCount(t, res, 1)
}

// TestTier1_ExecuteQuery_MultipleNodes verifies CREATE of multiple nodes and
// MATCH with RETURN of multiple records.
func TestTier1_ExecuteQuery_MultipleNodes(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	for _, name := range []string{"Alpha", "Beta", "Gamma"} {
		executeQueryEager(t, ctx, d,
			fmt.Sprintf(`CREATE (n:Tag {name: "%s"})`, name), nil)
	}

	res := executeQueryEager(t, ctx, d,
		`MATCH (n:Tag) RETURN n.name AS name`, nil)
	requireRecordCount(t, res, 3)
}

// TestTier1_ExecuteQuery_Relationship verifies single-hop MATCH via Tier-1.
func TestTier1_ExecuteQuery_Relationship(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	executeQueryEager(t, ctx, d,
		`CREATE (a:Actor {name: "Bob"})-[:STARRED_IN]->(m:Movie {title: "Opus"})`, nil)

	res := executeQueryEager(t, ctx, d,
		`MATCH (a:Actor)-[:STARRED_IN]->(m:Movie) RETURN a.name AS actor, m.title AS movie`, nil)
	requireRecordCount(t, res, 1)
	if requireGet(t, res.Records[0], "actor") != "Bob" {
		t.Errorf("actor mismatch")
	}
	if requireGet(t, res.Records[0], "movie") != "Opus" {
		t.Errorf("movie mismatch")
	}
}

// TestTier1_ExecuteQuery_WhereComparisons exercises all six comparison
// operators through Tier-1.
func TestTier1_ExecuteQuery_WhereComparisons(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	for i := 1; i <= 5; i++ {
		executeQueryEager(t, ctx, d,
			fmt.Sprintf(`CREATE (n:Num {val: %d})`, i), nil)
	}

	tests := []struct {
		where string
		count int
	}{
		{`n.val = 3`, 1},
		{`n.val <> 3`, 4},
		{`n.val > 3`, 2},
		{`n.val >= 3`, 3},
		{`n.val < 3`, 2},
		{`n.val <= 3`, 3},
	}
	for _, tc := range tests {
		res := executeQueryEager(t, ctx, d,
			fmt.Sprintf(`MATCH (n:Num) WHERE %s RETURN n`, tc.where), nil)
		if len(res.Records) != tc.count {
			t.Errorf("WHERE %s: got %d records, want %d", tc.where, len(res.Records), tc.count)
		}
	}
}

// TestTier1_ExecuteQuery_WhereAndOrNot exercises AND, OR, NOT combinators.
func TestTier1_ExecuteQuery_WhereAndOrNot(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	for _, pair := range [][2]int{{1, 10}, {2, 20}, {3, 30}} {
		executeQueryEager(t, ctx, d,
			fmt.Sprintf(`CREATE (n:Val {a: %d, b: %d})`, pair[0], pair[1]), nil)
	}

	// AND
	res := executeQueryEager(t, ctx, d,
		`MATCH (n:Val) WHERE n.a > 1 AND n.b < 30 RETURN n`, nil)
	if len(res.Records) != 1 {
		t.Errorf("AND: got %d records, want 1", len(res.Records))
	}

	// OR
	res = executeQueryEager(t, ctx, d,
		`MATCH (n:Val) WHERE n.a = 1 OR n.a = 3 RETURN n`, nil)
	if len(res.Records) != 2 {
		t.Errorf("OR: got %d records, want 2", len(res.Records))
	}

	// NOT
	res = executeQueryEager(t, ctx, d,
		`MATCH (n:Val) WHERE NOT n.a = 2 RETURN n`, nil)
	if len(res.Records) != 2 {
		t.Errorf("NOT: got %d records, want 2", len(res.Records))
	}
}

// TestTier1_ExecuteQuery_OrderByLimitSkip exercises ORDER BY, LIMIT, SKIP.
func TestTier1_ExecuteQuery_OrderByLimitSkip(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	for _, v := range []int{30, 10, 20} {
		executeQueryEager(t, ctx, d,
			fmt.Sprintf(`CREATE (n:Score {v: %d})`, v), nil)
	}

	// ORDER BY ASC + LIMIT 2.
	res := executeQueryEager(t, ctx, d,
		`MATCH (n:Score) RETURN n.v AS v ORDER BY n.v ASC LIMIT 2`, nil)
	requireRecordCount(t, res, 2)
	vals := []any{requireGet(t, res.Records[0], "v"), requireGet(t, res.Records[1], "v")}
	if vals[0] != int64(10) || vals[1] != int64(20) {
		t.Errorf("ORDER BY+LIMIT: got %v, want [10 20]", vals)
	}

	// ORDER BY DESC + SKIP 1.
	res = executeQueryEager(t, ctx, d,
		`MATCH (n:Score) RETURN n.v AS v ORDER BY n.v DESC SKIP 1`, nil)
	requireRecordCount(t, res, 2)
	if requireGet(t, res.Records[0], "v") != int64(20) {
		t.Errorf("ORDER BY DESC + SKIP 1: first record should be 20, got %v",
			requireGet(t, res.Records[0], "v"))
	}
}

// TestTier1_ExecuteQuery_SetProperty exercises MATCH + SET via Tier-1.
func TestTier1_ExecuteQuery_SetProperty(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	executeQueryEager(t, ctx, d, `CREATE (n:Widget {color: "red"})`, nil)
	executeQueryEager(t, ctx, d, `MATCH (n:Widget) SET n.color = "blue"`, nil)

	res := executeQueryEager(t, ctx, d,
		`MATCH (n:Widget) RETURN n.color AS color`, nil)
	requireRecordCount(t, res, 1)
	if requireGet(t, res.Records[0], "color") != "blue" {
		t.Errorf("SET property: expected blue, got %v", requireGet(t, res.Records[0], "color"))
	}
}

// TestTier1_ExecuteQuery_DetachDelete exercises MATCH + DETACH DELETE via Tier-1.
func TestTier1_ExecuteQuery_DetachDelete(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	executeQueryEager(t, ctx, d,
		`CREATE (a:X {id: 1})-[:LINK]->(b:X {id: 2})`, nil)

	executeQueryEager(t, ctx, d, `MATCH (n:X) DETACH DELETE n`, nil)

	res := executeQueryEager(t, ctx, d, `MATCH (n:X) RETURN n`, nil)
	requireRecordCount(t, res, 0)
}

// TestTier1_ExecuteQuery_MultiLabel verifies multi-label MATCH semantics (AND).
func TestTier1_ExecuteQuery_MultiLabel(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	executeQueryEager(t, ctx, d, `CREATE (n:Employee:Manager {name: "Eve"})`, nil)
	executeQueryEager(t, ctx, d, `CREATE (n:Employee {name: "Frank"})`, nil)

	// Only Eve has both labels.
	res := executeQueryEager(t, ctx, d,
		`MATCH (n:Employee:Manager) RETURN n.name AS name`, nil)
	requireRecordCount(t, res, 1)
	if requireGet(t, res.Records[0], "name") != "Eve" {
		t.Errorf("multi-label: expected Eve, got %v", requireGet(t, res.Records[0], "name"))
	}
}

// TestTier1_ExecuteQuery_MultiHop verifies a 2-hop MATCH via Tier-1.
func TestTier1_ExecuteQuery_MultiHop(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	executeQueryEager(t, ctx, d,
		`CREATE (a:Hub {id: 1})-[:CONNECTS]->(b:Hub {id: 2})-[:CONNECTS]->(c:Hub {id: 3})`,
		nil)

	res := executeQueryEager(t, ctx, d,
		`MATCH (a:Hub)-[:CONNECTS]->(b:Hub)-[:CONNECTS]->(c:Hub) RETURN a.id AS a, c.id AS c`,
		nil)
	requireRecordCount(t, res, 1)
	if requireGet(t, res.Records[0], "a") != int64(1) || requireGet(t, res.Records[0], "c") != int64(3) {
		t.Errorf("multi-hop: unexpected values")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tier 2 — session.ExecuteRead / session.ExecuteWrite tests
// ─────────────────────────────────────────────────────────────────────────────

// TestTier2_ExecuteWrite_CreateAndCommit verifies that ExecuteWrite commits on
// success and that the data is visible outside the callback.
func TestTier2_ExecuteWrite_CreateAndCommit(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	sess := newSession(t, d)

	_, err := sess.ExecuteWrite(ctx, func(tx graphlite.ManagedTransaction) (any, error) {
		_, err := tx.Run(ctx, `CREATE (n:Tier2 {val: "written"})`, nil)
		return nil, err
	})
	if err != nil {
		t.Fatalf("ExecuteWrite: %v", err)
	}

	// Verify data is visible via a new session.
	sess2 := newSession(t, d)
	res, err := sess2.Run(ctx, `MATCH (n:Tier2) RETURN n.val AS val`, nil)
	if err != nil {
		t.Fatalf("Run MATCH: %v", err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	val, _ := recs[0].Get("val")
	if val != "written" {
		t.Errorf("val = %v, want written", val)
	}
}

// TestTier2_ExecuteWrite_RollbackOnError verifies that a callback returning
// an error causes ExecuteWrite to roll back (no data visible afterwards).
func TestTier2_ExecuteWrite_RollbackOnError(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	sess := newSession(t, d)

	_, err := sess.ExecuteWrite(ctx, func(tx graphlite.ManagedTransaction) (any, error) {
		_, runErr := tx.Run(ctx, `CREATE (n:EphemeralTier2 {x: 1})`, nil)
		if runErr != nil {
			return nil, runErr
		}
		return nil, fmt.Errorf("intentional error to trigger rollback")
	})
	if err == nil {
		t.Fatal("expected error from ExecuteWrite, got nil")
	}

	// Data must NOT be visible.
	sess2 := newSession(t, d)
	res, err := sess2.Run(ctx, `MATCH (n:EphemeralTier2) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Run MATCH: %v", err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("expected 0 records after rollback, got %d", len(recs))
	}
}

// TestTier2_ExecuteRead_ReturnsCallbackResult verifies that ExecuteRead
// passes data from the callback result back to the caller.
func TestTier2_ExecuteRead_ReturnsCallbackResult(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	sess := newSession(t, d)

	// Seed data.
	_, err := sess.ExecuteWrite(ctx, func(tx graphlite.ManagedTransaction) (any, error) {
		_, e := tx.Run(ctx, `CREATE (n:ReadTest {score: 99})`, nil)
		return nil, e
	})
	if err != nil {
		t.Fatalf("seed ExecuteWrite: %v", err)
	}

	// Read via ExecuteRead — return the score from inside the callback.
	raw, err := sess.ExecuteRead(ctx, func(tx graphlite.ManagedTransaction) (any, error) {
		res, e := tx.Run(ctx, `MATCH (n:ReadTest) RETURN n.score AS score`, nil)
		if e != nil {
			return nil, e
		}
		recs, e := res.Collect(ctx)
		if e != nil {
			return nil, e
		}
		if len(recs) == 0 {
			return nil, fmt.Errorf("no records found")
		}
		return recs[0].Values()[0], nil
	})
	if err != nil {
		t.Fatalf("ExecuteRead: %v", err)
	}
	if raw != int64(99) {
		t.Errorf("score = %v (%T), want 99", raw, raw)
	}
}

// TestTier2_ExecuteWrite_WithParams verifies $param binding inside managed tx.
func TestTier2_ExecuteWrite_WithParams(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	sess := newSession(t, d)

	_, err := sess.ExecuteWrite(ctx, func(tx graphlite.ManagedTransaction) (any, error) {
		_, e := tx.Run(ctx,
			`CREATE (n:Parameterised {key: $k, val: $v})`,
			map[string]any{"k": "hello", "v": "world"},
		)
		return nil, e
	})
	if err != nil {
		t.Fatalf("ExecuteWrite with params: %v", err)
	}

	res, err := sess.Run(ctx,
		`MATCH (n:Parameterised) WHERE n.key = $k RETURN n.val AS val`,
		map[string]any{"k": "hello"},
	)
	if err != nil {
		t.Fatalf("Run MATCH: %v", err)
	}
	recs, _ := res.Collect(ctx)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	val, _ := recs[0].Get("val")
	if val != "world" {
		t.Errorf("val = %v, want world", val)
	}
}

// TestTier2_ExecuteWrite_DeleteRelationship verifies DELETE r in a managed tx.
func TestTier2_ExecuteWrite_DeleteRelationship(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	sess := newSession(t, d)

	// Create a two-node relationship.
	_, err := sess.ExecuteWrite(ctx, func(tx graphlite.ManagedTransaction) (any, error) {
		_, e := tx.Run(ctx,
			`CREATE (a:RelDelTest {id: 1})-[:TEMP]->(b:RelDelTest {id: 2})`, nil)
		return nil, e
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Delete just the relationship.
	_, err = sess.ExecuteWrite(ctx, func(tx graphlite.ManagedTransaction) (any, error) {
		_, e := tx.Run(ctx,
			`MATCH (a:RelDelTest)-[r:TEMP]->(b:RelDelTest) DELETE r`, nil)
		return nil, e
	})
	if err != nil {
		t.Fatalf("DELETE r: %v", err)
	}

	// Nodes must still exist but no edges.
	res, _ := sess.Run(ctx, `MATCH (n:RelDelTest) RETURN n`, nil)
	recs, _ := res.Collect(ctx)
	if len(recs) != 2 {
		t.Errorf("expected 2 nodes remaining, got %d", len(recs))
	}
	res2, _ := sess.Run(ctx, `MATCH ()-[r:TEMP]->() RETURN r`, nil)
	recs2, _ := res2.Collect(ctx)
	if len(recs2) != 0 {
		t.Errorf("expected 0 TEMP relationships, got %d", len(recs2))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tier 3 — session.BeginTransaction (explicit) tests
// ─────────────────────────────────────────────────────────────────────────────

// TestTier3_ExplicitTx_CommitPersists verifies Commit makes data visible.
func TestTier3_ExplicitTx_CommitPersists(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	sess := newSession(t, d)

	tx, err := sess.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}

	_, err = tx.Run(ctx, `CREATE (n:Explicit {label: "committed"})`, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("tx.Run: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, _ := sess.Run(ctx, `MATCH (n:Explicit) RETURN n.label AS label`, nil)
	recs, _ := res.Collect(ctx)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record after commit, got %d", len(recs))
	}
	lbl, _ := recs[0].Get("label")
	if lbl != "committed" {
		t.Errorf("label = %v, want committed", lbl)
	}
}

// TestTier3_ExplicitTx_RollbackReverts verifies Rollback removes data.
func TestTier3_ExplicitTx_RollbackReverts(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	sess := newSession(t, d)

	tx, err := sess.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	_, err = tx.Run(ctx, `CREATE (n:RolledBack {x: 1})`, nil)
	if err != nil {
		t.Fatalf("tx.Run: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	res, _ := sess.Run(ctx, `MATCH (n:RolledBack) RETURN n`, nil)
	recs, _ := res.Collect(ctx)
	if len(recs) != 0 {
		t.Errorf("expected 0 records after rollback, got %d", len(recs))
	}
}

// TestTier3_ExplicitTx_CloseWithoutCommitRollsBack verifies that Close on an
// uncommitted transaction rolls back.
func TestTier3_ExplicitTx_CloseWithoutCommitRollsBack(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	sess := newSession(t, d)

	tx, err := sess.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	_, err = tx.Run(ctx, `CREATE (n:CloseTest {x: 1})`, nil)
	if err != nil {
		t.Fatalf("tx.Run: %v", err)
	}
	// Close without commit — should roll back.
	if err := tx.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res, _ := sess.Run(ctx, `MATCH (n:CloseTest) RETURN n`, nil)
	recs, _ := res.Collect(ctx)
	if len(recs) != 0 {
		t.Errorf("expected 0 records after Close (implicit rollback), got %d", len(recs))
	}
}

// TestTier3_ExplicitTx_MultipleOperations verifies a multi-operation explicit tx.
func TestTier3_ExplicitTx_MultipleOperations(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	sess := newSession(t, d)

	tx, err := sess.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}

	// Create two nodes in the same explicit tx.
	for _, name := range []string{"TxNode1", "TxNode2"} {
		_, err = tx.Run(ctx, fmt.Sprintf(`CREATE (n:MultiOp {name: "%s"})`, name), nil)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("tx.Run %s: %v", name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, _ := sess.Run(ctx, `MATCH (n:MultiOp) RETURN n.name AS name`, nil)
	recs, _ := res.Collect(ctx)
	if len(recs) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(recs))
	}
}

// TestTier3_ExplicitTx_WithSetAndDelete verifies SET + DELETE inside explicit tx.
func TestTier3_ExplicitTx_WithSetAndDelete(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	sess := newSession(t, d)

	// Seed: two nodes.
	_, err := sess.ExecuteWrite(ctx, func(tx graphlite.ManagedTransaction) (any, error) {
		_, e := tx.Run(ctx, `CREATE (a:SetDelTest {k: "keep"}), (b:SetDelTest {k: "drop"})`, nil)
		return nil, e
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Explicit tx: SET the keep node, DETACH DELETE the drop node.
	tx, err := sess.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	_, err = tx.Run(ctx, `MATCH (n:SetDelTest {k: "keep"}) SET n.updated = true`, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("SET: %v", err)
	}
	_, err = tx.Run(ctx, `MATCH (n:SetDelTest {k: "drop"}) DETACH DELETE n`, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("DELETE: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, _ := sess.Run(ctx, `MATCH (n:SetDelTest) RETURN n.k AS k, n.updated AS upd`, nil)
	recs, _ := res.Collect(ctx)
	if len(recs) != 1 {
		t.Fatalf("expected 1 node after delete, got %d", len(recs))
	}
	k, _ := recs[0].Get("k")
	if k != "keep" {
		t.Errorf("expected keep node, got k=%v", k)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Cross-tier integration tests
// ─────────────────────────────────────────────────────────────────────────────

// TestAllTiers_WriteReadCycle exercises all three tiers in a single test:
// write via Tier-1, verify via Tier-2, update via Tier-3.
func TestAllTiers_WriteReadCycle(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	// Tier 1: write.
	executeQueryEager(t, ctx, d,
		`CREATE (n:CrossTier {status: "initial"})`, nil)

	// Tier 2: read — verify initial state.
	sess := newSession(t, d)
	val, err := sess.ExecuteRead(ctx, func(tx graphlite.ManagedTransaction) (any, error) {
		res, e := tx.Run(ctx, `MATCH (n:CrossTier) RETURN n.status AS status`, nil)
		if e != nil {
			return nil, e
		}
		recs, e := res.Collect(ctx)
		if e != nil || len(recs) == 0 {
			return nil, fmt.Errorf("no records")
		}
		v, _ := recs[0].Get("status")
		return v, nil
	})
	if err != nil {
		t.Fatalf("Tier-2 read: %v", err)
	}
	if val != "initial" {
		t.Errorf("Tier-2 read: status = %v, want initial", val)
	}

	// Tier 3: update.
	tx, err := sess.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("Tier-3 BeginTransaction: %v", err)
	}
	_, err = tx.Run(ctx, `MATCH (n:CrossTier) SET n.status = "updated"`, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Tier-3 SET: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Tier-3 Commit: %v", err)
	}

	// Tier 1: re-read to confirm update.
	res := executeQueryEager(t, ctx, d,
		`MATCH (n:CrossTier) RETURN n.status AS status`, nil)
	requireRecordCount(t, res, 1)
	if requireGet(t, res.Records[0], "status") != "updated" {
		t.Errorf("after Tier-3 update: status = %v, want updated",
			requireGet(t, res.Records[0], "status"))
	}
}

// TestAllTiers_NodeProjection verifies that whole-node projections return
// proper *graphlite.Node values with labels and properties populated.
func TestAllTiers_NodeProjection(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	executeQueryEager(t, ctx, d,
		`CREATE (n:Projected {x: "y"})`, nil)

	res := executeQueryEager(t, ctx, d,
		`MATCH (n:Projected) RETURN n`, nil)
	requireRecordCount(t, res, 1)

	raw := requireGet(t, res.Records[0], "n")
	node, ok := raw.(*graphlite.Node)
	if !ok {
		t.Fatalf("expected *graphlite.Node, got %T", raw)
	}
	if len(node.Labels) == 0 || node.Labels[0] != "Projected" {
		t.Errorf("node.Labels = %v, want [Projected]", node.Labels)
	}
	if node.Props["x"] != "y" {
		t.Errorf("node.Props[x] = %v, want y", node.Props["x"])
	}
}

// TestAllTiers_UndirectedRelationship verifies that undirected MATCH works.
func TestAllTiers_UndirectedRelationship(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	// Create directed: a → b.
	executeQueryEager(t, ctx, d,
		`CREATE (a:UDir {n: 1})-[:LINK]->(b:UDir {n: 2})`, nil)

	// Undirected match should find both orderings from the same edge.
	res := executeQueryEager(t, ctx, d,
		`MATCH (x:UDir)-[:LINK]-(y:UDir) RETURN x.n AS x, y.n AS y`, nil)
	// One directed edge yields 2 undirected rows (a→b and b←a both match).
	if len(res.Records) != 2 {
		t.Errorf("undirected MATCH: expected 2 rows, got %d", len(res.Records))
	}
}
