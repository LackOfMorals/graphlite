package graphlite_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	. "github.com/LackOfMorals/graphlite"
	_ "modernc.org/sqlite"
)

// openTestDB opens an in-memory SQLite database with the graphlite schema for
// use in result-layer tests. It applies the minimal schema needed to run
// parameterised SELECTs against known data.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("openTestDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	schema := `
CREATE TABLE nodes (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    labels TEXT    NOT NULL DEFAULT '',
    props  JSON    NOT NULL DEFAULT '{}'
);
CREATE TABLE edges (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    type     TEXT    NOT NULL,
    start_id INTEGER NOT NULL REFERENCES nodes(id),
    end_id   INTEGER NOT NULL REFERENCES nodes(id),
    props    JSON    NOT NULL DEFAULT '{}'
);
`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("openTestDB schema: %v", err)
	}
	return db
}

// insertNode inserts a node and returns its ID.
func insertNode(t *testing.T, db *sql.DB, labels string, props map[string]any) int64 {
	t.Helper()
	p, _ := json.Marshal(props)
	res, err := db.Exec(
		`INSERT INTO nodes (labels, props) VALUES (?, json(?))`,
		labels, string(p),
	)
	if err != nil {
		t.Fatalf("insertNode: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// insertEdge inserts an edge and returns its ID.
func insertEdge(t *testing.T, db *sql.DB, typ string, startID, endID int64, props map[string]any) int64 {
	t.Helper()
	p, _ := json.Marshal(props)
	res, err := db.Exec(
		`INSERT INTO edges (type, start_id, end_id, props) VALUES (?, ?, ?, json(?))`,
		typ, startID, endID, string(p),
	)
	if err != nil {
		t.Fatalf("insertEdge: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// ─────────────────────────────────────────────────────────────────────────────
// QueryResult tests
// ─────────────────────────────────────────────────────────────────────────────

func TestQueryResult_Empty(t *testing.T) {
	db := openTestDB(t)
	rows, err := db.Query("SELECT id, labels, props FROM nodes")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	qr, err := NewResultFromRows(rows)
	if err != nil {
		t.Fatalf("NewResultFromRows: %v", err)
	}

	ctx := context.Background()
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

func TestQueryResult_ScalarColumns(t *testing.T) {
	db := openTestDB(t)
	insertNode(t, db, "Person", map[string]any{"name": "Alice", "age": 30})
	insertNode(t, db, "Person", map[string]any{"name": "Bob", "age": 25})

	// Scalar projection: json_extract for name and age.
	rows, err := db.Query(
		`SELECT json_extract(props, '$.name') AS name,
		        json_extract(props, '$.age')  AS age
		 FROM nodes
		 WHERE labels = 'Person'
		 ORDER BY json_extract(props, '$.name')`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	qr, err := NewResultFromRows(rows)
	if err != nil {
		t.Fatalf("NewResultFromRows: %v", err)
	}

	ctx := context.Background()

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
	// SQLite returns JSON numbers as float64 or int64 depending on query.
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

func TestQueryResult_AsMap(t *testing.T) {
	db := openTestDB(t)
	insertNode(t, db, "Person", map[string]any{"name": "Carol"})

	rows, err := db.Query(
		`SELECT json_extract(props, '$.name') AS name FROM nodes LIMIT 1`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	qr, err := NewResultFromRows(rows)
	if err != nil {
		t.Fatalf("NewResultFromRows: %v", err)
	}

	ctx := context.Background()
	if !qr.Next(ctx) {
		t.Fatal("expected one record")
	}
	m := qr.Record().AsMap()
	if m["name"] != "Carol" {
		t.Errorf("expected name=Carol in AsMap, got %v", m["name"])
	}
}

func TestQueryResult_Collect(t *testing.T) {
	db := openTestDB(t)
	insertNode(t, db, "X", map[string]any{"v": 1})
	insertNode(t, db, "X", map[string]any{"v": 2})
	insertNode(t, db, "X", map[string]any{"v": 3})

	rows, err := db.Query(
		`SELECT json_extract(props, '$.v') AS v FROM nodes ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	qr, err := NewResultFromRows(rows)
	if err != nil {
		t.Fatalf("NewResultFromRows: %v", err)
	}

	recs, err := qr.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(recs) != 3 {
		t.Errorf("expected 3 records, got %d", len(recs))
	}
}

func TestQueryResult_Keys(t *testing.T) {
	db := openTestDB(t)
	insertNode(t, db, "Z", map[string]any{"x": 1})

	rows, err := db.Query(
		`SELECT json_extract(props, '$.x') AS x, id FROM nodes LIMIT 1`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	qr, err := NewResultFromRows(rows)
	if err != nil {
		t.Fatalf("NewResultFromRows: %v", err)
	}
	keys := qr.Keys()
	if len(keys) != 2 || keys[0] != "x" || keys[1] != "id" {
		t.Errorf("unexpected keys %v", keys)
	}
}

func TestQueryResult_Consume(t *testing.T) {
	db := openTestDB(t)
	insertNode(t, db, "A", map[string]any{})

	rows, err := db.Query(`SELECT id FROM nodes`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	qr, err := NewResultFromRows(rows)
	if err != nil {
		t.Fatalf("NewResultFromRows: %v", err)
	}
	sum, err := qr.Consume(context.Background())
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
// Node / Relationship mapping tests
// ─────────────────────────────────────────────────────────────────────────────

func TestQueryResult_NodeProjection(t *testing.T) {
	db := openTestDB(t)
	id := insertNode(t, db, "Person,Employee", map[string]any{"name": "Dave", "age": 40})

	// Simulate the translator's whole-node VarExpr projection.
	nodeSQL := fmt.Sprintf(
		`SELECT json_object('id', n.id, 'labels', n.labels, 'props', json(n.props)) AS n
		 FROM nodes n WHERE n.id = %d`, id)
	rows, err := db.Query(nodeSQL)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	qr, err := NewResultFromRows(rows)
	if err != nil {
		t.Fatalf("NewResultFromRows: %v", err)
	}

	ctx := context.Background()
	if !qr.Next(ctx) {
		t.Fatalf("expected one record; err=%v", qr.Err())
	}
	val, ok := qr.Record().Get("n")
	if !ok {
		t.Fatal("expected key 'n' in record")
	}
	node, ok := val.(*Node)
	if !ok {
		t.Fatalf("expected *Node got %T: %v", val, val)
	}
	if node.ElementId != fmt.Sprintf("%d", id) {
		t.Errorf("ElementId: want %d got %s", id, node.ElementId)
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

func TestQueryResult_RelationshipProjection(t *testing.T) {
	db := openTestDB(t)
	idA := insertNode(t, db, "Person", map[string]any{"name": "Alice"})
	idB := insertNode(t, db, "Person", map[string]any{"name": "Bob"})
	relID := insertEdge(t, db, "KNOWS", idA, idB, map[string]any{"since": 2020})

	// Simulate the translator's whole-rel VarExpr projection.
	relSQL := fmt.Sprintf(
		`SELECT json_object('id', r.id, 'type', r.type, 'start_id', r.start_id, 'end_id', r.end_id, 'props', json(r.props)) AS r
		 FROM edges r WHERE r.id = %d`, relID)
	rows, err := db.Query(relSQL)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	qr, err := NewResultFromRows(rows)
	if err != nil {
		t.Fatalf("NewResultFromRows: %v", err)
	}

	ctx := context.Background()
	if !qr.Next(ctx) {
		t.Fatalf("expected one record; err=%v", qr.Err())
	}
	val, ok := qr.Record().Get("r")
	if !ok {
		t.Fatal("expected key 'r' in record")
	}
	rel, ok := val.(*Relationship)
	if !ok {
		t.Fatalf("expected *Relationship got %T: %v", val, val)
	}
	if rel.ElementId != fmt.Sprintf("%d", relID) {
		t.Errorf("ElementId: want %d got %s", relID, rel.ElementId)
	}
	if rel.Type != "KNOWS" {
		t.Errorf("Type: want KNOWS got %s", rel.Type)
	}
	if rel.StartElementId != fmt.Sprintf("%d", idA) {
		t.Errorf("StartElementId: want %d got %s", idA, rel.StartElementId)
	}
	if rel.EndElementId != fmt.Sprintf("%d", idB) {
		t.Errorf("EndElementId: want %d got %s", idB, rel.EndElementId)
	}
	if rel.Props["since"] == nil {
		t.Error("expected props[since] to be non-nil")
	}
}

func TestQueryResult_NoLabels(t *testing.T) {
	db := openTestDB(t)
	id := insertNode(t, db, "", map[string]any{})

	nodeSQL := fmt.Sprintf(
		`SELECT json_object('id', n.id, 'labels', n.labels, 'props', json(n.props)) AS n
		 FROM nodes n WHERE n.id = %d`, id)
	rows, err := db.Query(nodeSQL)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	qr, err := NewResultFromRows(rows)
	if err != nil {
		t.Fatalf("NewResultFromRows: %v", err)
	}

	ctx := context.Background()
	if !qr.Next(ctx) {
		t.Fatalf("expected one record; err=%v", qr.Err())
	}
	val, _ := qr.Record().Get("n")
	node, ok := val.(*Node)
	if !ok {
		t.Fatalf("expected *Node got %T", val)
	}
	if len(node.Labels) != 0 {
		t.Errorf("expected empty labels, got %v", node.Labels)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Counters tests
// ─────────────────────────────────────────────────────────────────────────────

func TestCounters_WriteOperation(t *testing.T) {
	db := openTestDB(t)

	rows, err := db.Query(`SELECT id FROM nodes WHERE 1=0`) // empty result
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	qr, err := NewResultFromRows(rows)
	if err != nil {
		t.Fatalf("NewResultFromRows: %v", err)
	}
	// Simulate a write operation that created 2 nodes and 1 relationship.
	qr.SetCounters(QueryCounters{
		NodesCreated:         2,
		RelationshipsCreated: 1,
	})

	sum, err := qr.Consume(context.Background())
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	c := sum.Counters()
	if c.NodesCreated() != 2 {
		t.Errorf("NodesCreated: want 2 got %d", c.NodesCreated())
	}
	if c.RelationshipsCreated() != 1 {
		t.Errorf("RelationshipsCreated: want 1 got %d", c.RelationshipsCreated())
	}
	if !c.ContainsUpdates() {
		t.Error("ContainsUpdates should be true")
	}
	if c.NodesDeleted() != 0 {
		t.Errorf("NodesDeleted: want 0 got %d", c.NodesDeleted())
	}
}

func TestCounters_Zero(t *testing.T) {
	db := openTestDB(t)
	rows, err := db.Query(`SELECT id FROM nodes WHERE 1=0`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	qr, err := NewResultFromRows(rows)
	if err != nil {
		t.Fatalf("NewResultFromRows: %v", err)
	}
	sum, _ := qr.Consume(context.Background())
	c := sum.Counters()
	if c.ContainsUpdates() {
		t.Error("ContainsUpdates should be false for zero counters")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Column mapper unit tests
// ─────────────────────────────────────────────────────────────────────────────

func TestMapColumnValue_NonJSON(t *testing.T) {
	// Non-JSON strings are returned unchanged.
	got := MapColumnValue("hello")
	if got != "hello" {
		t.Errorf("expected hello got %v", got)
	}
}

func TestMapColumnValue_NodeJSON(t *testing.T) {
	// Props is a nested JSON object, as SQLite returns it via json_object(..., json(props)).
	j := `{"id":1,"labels":"Person","props":{"name":"Alice"}}`
	got := MapColumnValue(j)
	if _, ok := got.(*Node); !ok {
		t.Errorf("expected *Node got %T: %v", got, got)
	}
}

func TestMapColumnValue_RelJSON(t *testing.T) {
	// Props is a nested JSON object.
	j := `{"id":5,"type":"KNOWS","start_id":1,"end_id":2,"props":{}}`
	got := MapColumnValue(j)
	if _, ok := got.(*Relationship); !ok {
		t.Errorf("expected *Relationship got %T: %v", got, got)
	}
}

func TestMapColumnValue_RegularJSONObject(t *testing.T) {
	// A JSON object that is neither a node nor a rel shape should be returned
	// as the original string (no panic).
	j := `{"foo":"bar"}`
	got := MapColumnValue(j)
	if _, ok := got.(*Node); ok {
		t.Error("should not be decoded as Node")
	}
	if _, ok := got.(*Relationship); ok {
		t.Error("should not be decoded as Relationship")
	}
	// The original string should be returned.
	if got != j {
		t.Errorf("expected original string back, got %v", got)
	}
}

func TestMapColumnValue_ByteSlice(t *testing.T) {
	// []byte input (as returned by some SQLite drivers for JSON columns).
	// Props is a nested JSON object, not a string.
	j := `{"id":3,"labels":"X","props":{}}`
	got := MapColumnValue([]byte(j))
	if _, ok := got.(*Node); !ok {
		t.Errorf("expected *Node got %T", got)
	}
}

func TestSplitLabels(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"Person", []string{"Person"}},
		{"Person,Employee", []string{"Person", "Employee"}},
		{"A,B,C", []string{"A", "B", "C"}},
	}
	for _, tc := range cases {
		got := SplitLabels(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("SplitLabels(%q): len want %d got %d: %v", tc.in, len(tc.want), len(got), got)
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("SplitLabels(%q)[%d]: want %q got %q", tc.in, i, tc.want[i], got[i])
			}
		}
	}
}

// Ensure Record.Get returns false for missing keys.
func TestRecord_MissingKey(t *testing.T) {
	rec := NewRecord([]string{"a", "b"}, []any{1, 2})
	_, ok := rec.Get("missing")
	if ok {
		t.Error("expected false for missing key")
	}
}

