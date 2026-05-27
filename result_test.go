package graphlite_test

import (
	"context"
	"errors"
	"testing"

	"github.com/LackOfMorals/graphlite"
)

// openDB opens an in-memory graphlite database for use in result-layer tests.
func openDB(t *testing.T) *graphlite.DB {
	t.Helper()
	db, err := graphlite.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return db
}

// ─────────────────────────────────────────────────────────────────────────────
// Result cursor tests (via db.RunQuery)
// ─────────────────────────────────────────────────────────────────────────────

func TestResult_Empty(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	qr, err := db.RunQuery(ctx, `MATCH (n:Person) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	if qr.Next(ctx) {
		t.Error("expected Next to return false on empty result set")
	}
	if qr.Err() != nil {
		t.Errorf("unexpected error: %v", qr.Err())
	}
	if qr.Record() != nil {
		t.Error("Record should be nil before any Next call")
	}
}

func TestResult_ScalarColumns(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:Person {name: "Alice", age: 30})`, nil); err != nil {
		t.Fatalf("create Alice: %v", err)
	}
	if _, err := db.RunQuery(ctx, `CREATE (:Person {name: "Bob", age: 25})`, nil); err != nil {
		t.Fatalf("create Bob: %v", err)
	}

	qr, err := db.RunQuery(ctx, `MATCH (n:Person) RETURN n.name AS name, n.age AS age ORDER BY n.name`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}

	if !qr.Next(ctx) {
		t.Fatalf("expected first record, got none; err=%v", qr.Err())
	}
	rec := qr.Record()
	if rec == nil {
		t.Fatal("Record() returned nil")
	}
	name, ok := rec.Get("name")
	if !ok || name != "Alice" {
		t.Errorf("expected name=Alice got %v (ok=%v)", name, ok)
	}
	age, ok := rec.Get("age")
	if !ok {
		t.Error("expected age key in record")
	}
	switch a := age.(type) {
	case float64:
		if a != 30 {
			t.Errorf("expected age=30 got %v", a)
		}
	case int64:
		if a != 30 {
			t.Errorf("expected age=30 got %v", a)
		}
	default:
		t.Errorf("unexpected age type %T value %v", age, age)
	}

	if !qr.Next(ctx) {
		t.Fatalf("expected second record; err=%v", qr.Err())
	}
	rec2 := qr.Record()
	name2, _ := rec2.Get("name")
	if name2 != "Bob" {
		t.Errorf("expected name=Bob got %v", name2)
	}

	if qr.Next(ctx) {
		t.Error("expected no more records")
	}
}

func TestResult_AsMap(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:Person {name: "Carol"})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	qr, err := db.RunQuery(ctx, `MATCH (n:Person) RETURN n.name AS name LIMIT 1`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	if !qr.Next(ctx) {
		t.Fatal("expected one record")
	}
	m := qr.Record().AsMap()
	if m["name"] != "Carol" {
		t.Errorf("expected name=Carol in AsMap, got %v", m["name"])
	}
}

func TestResult_Collect(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	for _, v := range []int{1, 2, 3} {
		if _, err := db.RunQuery(ctx, `CREATE (:X {v: $v})`, map[string]any{"v": v}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	qr, err := db.RunQuery(ctx, `MATCH (n:X) RETURN n.v AS v ORDER BY n.v`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	recs, err := qr.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(recs) != 3 {
		t.Errorf("expected 3 records, got %d", len(recs))
	}
}

func TestResult_Keys(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:Z {x: 1})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	qr, err := db.RunQuery(ctx, `MATCH (n:Z) RETURN n.x AS x, n AS z LIMIT 1`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	keys := qr.Keys()
	if len(keys) != 2 || keys[0] != "x" || keys[1] != "z" {
		t.Errorf("unexpected keys %v", keys)
	}
	// Drain to avoid resource leak.
	_, _ = qr.Consume(ctx)
}

func TestResult_Consume(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:A)`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	qr, err := db.RunQuery(ctx, `MATCH (n:A) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	sum, err := qr.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if sum == nil {
		t.Error("Consume returned nil summary")
	}
	// Counters on a read query are all zero.
	c := sum.Counters()
	if c.NodesCreated() != 0 || c.NodesDeleted() != 0 {
		t.Errorf("expected zero counters, got created=%d deleted=%d",
			c.NodesCreated(), c.NodesDeleted())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Node / Relationship mapping tests (via db.RunQuery)
// ─────────────────────────────────────────────────────────────────────────────

func TestResult_NodeProjection(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:Person:Employee {name: "Dave", age: 40})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	qr, err := db.RunQuery(ctx, `MATCH (n:Person) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	if !qr.Next(ctx) {
		t.Fatalf("expected one record; err=%v", qr.Err())
	}
	val, ok := qr.Record().Get("n")
	if !ok {
		t.Fatal("expected key 'n' in record")
	}
	node, ok := val.(*graphlite.Node)
	if !ok {
		t.Fatalf("expected *Node got %T: %v", val, val)
	}
	// Labels
	wantLabels := []string{"Person", "Employee"}
	if len(node.Labels) != len(wantLabels) {
		t.Errorf("Labels length: want %d got %d: %v", len(wantLabels), len(node.Labels), node.Labels)
	}
	for i, l := range wantLabels {
		if i < len(node.Labels) && node.Labels[i] != l {
			t.Errorf("Labels[%d]: want %q got %q", i, l, node.Labels[i])
		}
	}
	// Props
	if node.Props["name"] != "Dave" {
		t.Errorf("Props[name]: want Dave got %v", node.Props["name"])
	}
}

func TestResult_RelationshipProjection(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:Person {name:"Alice"})-[:KNOWS {since: 2020}]->(:Person {name:"Bob"})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	qr, err := db.RunQuery(ctx, `MATCH (:Person)-[r:KNOWS]->(:Person) RETURN r`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	if !qr.Next(ctx) {
		t.Fatalf("expected one record; err=%v", qr.Err())
	}
	val, ok := qr.Record().Get("r")
	if !ok {
		t.Fatal("expected key 'r' in record")
	}
	rel, ok := val.(*graphlite.Relationship)
	if !ok {
		t.Fatalf("expected *Relationship got %T: %v", val, val)
	}
	if rel.Type != "KNOWS" {
		t.Errorf("Type: want KNOWS got %s", rel.Type)
	}
	if rel.Props["since"] == nil {
		t.Error("expected props[since] to be non-nil")
	}
}

func TestResult_NoLabels(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (n)`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	qr, err := db.RunQuery(ctx, `MATCH (n) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	if !qr.Next(ctx) {
		t.Fatalf("expected one record; err=%v", qr.Err())
	}
	val, _ := qr.Record().Get("n")
	node, ok := val.(*graphlite.Node)
	if !ok {
		t.Fatalf("expected *Node got %T", val)
	}
	if len(node.Labels) != 0 {
		t.Errorf("expected empty labels, got %v", node.Labels)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Counters tests (via write queries)
// ─────────────────────────────────────────────────────────────────────────────

func TestCounters_WriteOperation(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	qr, err := db.RunQuery(ctx, `CREATE (:X), (:X)`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	sum, err := qr.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	c := sum.Counters()
	if c.NodesCreated() != 2 {
		t.Errorf("NodesCreated: want 2 got %d", c.NodesCreated())
	}
	if !c.ContainsUpdates() {
		t.Error("ContainsUpdates should be true")
	}
	if c.NodesDeleted() != 0 {
		t.Errorf("NodesDeleted: want 0 got %d", c.NodesDeleted())
	}
}

func TestCounters_CreateRelationship(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	qr, err := db.RunQuery(ctx, `CREATE (:A)-[:LINK]->(:B)`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	sum, err := qr.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	c := sum.Counters()
	if c.RelationshipsCreated() != 1 {
		t.Errorf("RelationshipsCreated: want 1 got %d", c.RelationshipsCreated())
	}
}

func TestCounters_Zero(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	qr, err := db.RunQuery(ctx, `MATCH (n:NoSuchLabel) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	sum, _ := qr.Consume(ctx)
	c := sum.Counters()
	if c.ContainsUpdates() {
		t.Error("ContainsUpdates should be false for zero counters")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Single / ErrNoRecords / ErrMultipleRecords tests
// ─────────────────────────────────────────────────────────────────────────────

func TestSingle_ExactlyOne(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:Unique {id: 1})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	qr, err := db.RunQuery(ctx, `MATCH (n:Unique) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	rec, err := qr.Single(ctx)
	if err != nil {
		t.Fatalf("Single: unexpected error: %v", err)
	}
	if rec == nil {
		t.Fatal("Single: expected record, got nil")
	}
}

func TestSingle_ErrNoRecords(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	qr, err := db.RunQuery(ctx, `MATCH (n:Empty) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	_, err = qr.Single(ctx)
	if !errors.Is(err, graphlite.ErrNoRecords) {
		t.Errorf("expected ErrNoRecords, got %v", err)
	}
}

func TestSingle_ErrMultipleRecords(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:Multi), (:Multi)`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	qr, err := db.RunQuery(ctx, `MATCH (n:Multi) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	_, err = qr.Single(ctx)
	if !errors.Is(err, graphlite.ErrMultipleRecords) {
		t.Errorf("expected ErrMultipleRecords, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Record helper tests
// ─────────────────────────────────────────────────────────────────────────────

// Ensure Record.Get returns false for missing keys (via a query-created record).
func TestRecord_MissingKey(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:T {a: 1})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	qr, err := db.RunQuery(ctx, `MATCH (n:T) RETURN n.a AS a`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	if !qr.Next(ctx) {
		t.Fatal("expected one record")
	}
	rec := qr.Record()
	_, ok := rec.Get("missing")
	if ok {
		t.Error("expected false for missing key")
	}
	_, _ = qr.Consume(ctx)
}
