package graphlite_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LackOfMorals/graphlite/v2"
)

// ─────────────────────────────────────────────────────────────────────────────
// Node
// ─────────────────────────────────────────────────────────────────────────────

func TestNode_Fields(t *testing.T) {
	n := graphlite.Node{
		ElementId: "1",
		Labels:    []string{"Person", "Employee"},
		Props:     map[string]any{"name": "Alice", "age": int64(30)},
	}

	if n.ElementId != "1" {
		t.Errorf("ElementId: got %q, want %q", n.ElementId, "1")
	}
	if len(n.Labels) != 2 || n.Labels[0] != "Person" || n.Labels[1] != "Employee" {
		t.Errorf("Labels: got %v", n.Labels)
	}
	if n.Props["name"] != "Alice" {
		t.Errorf("Props[name]: got %v", n.Props["name"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Relationship
// ─────────────────────────────────────────────────────────────────────────────

func TestRelationship_Fields(t *testing.T) {
	r := graphlite.Relationship{
		ElementId:      "10",
		Type:           "KNOWS",
		StartElementId: "1",
		EndElementId:   "2",
		Props:          map[string]any{"since": int64(2020)},
	}

	if r.ElementId != "10" {
		t.Errorf("ElementId: got %q, want %q", r.ElementId, "10")
	}
	if r.Type != "KNOWS" {
		t.Errorf("Type: got %q, want %q", r.Type, "KNOWS")
	}
	if r.StartElementId != "1" {
		t.Errorf("StartElementId: got %q, want %q", r.StartElementId, "1")
	}
	if r.EndElementId != "2" {
		t.Errorf("EndElementId: got %q, want %q", r.EndElementId, "2")
	}
	if r.Props["since"] != int64(2020) {
		t.Errorf("Props[since]: got %v", r.Props["since"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Record — tested through the public query API
// ─────────────────────────────────────────────────────────────────────────────

// fetchRecord runs a MATCH query and returns the first record, failing the
// test if the query or iteration fails.
func fetchRecord(t *testing.T, db *graphlite.DB, query string, params map[string]any) *graphlite.Record {
	t.Helper()
	ctx := context.Background()
	qr, err := db.RunQuery(ctx, query, params)
	if err != nil {
		t.Fatalf("RunQuery %q: %v", query, err)
	}
	if !qr.Next(ctx) {
		t.Fatalf("RunQuery %q: expected at least one record", query)
	}
	rec := qr.Record()
	_, _ = qr.Consume(ctx)
	return rec
}

func TestRecord_Get_Hit(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	if _, err := db.RunQuery(ctx, `CREATE (:P {name: "Alice", age: 30})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := fetchRecord(t, db, `MATCH (n:P) RETURN n.name AS name, n.age AS age`, nil)

	v, ok := rec.Get("name")
	if !ok {
		t.Fatal("Get(name): expected ok=true, got false")
	}
	if v != "Alice" {
		t.Errorf("Get(name): got %v, want Alice", v)
	}
}

func TestRecord_Get_Miss(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	if _, err := db.RunQuery(ctx, `CREATE (:P {name: "Alice"})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := fetchRecord(t, db, `MATCH (n:P) RETURN n.name AS name`, nil)

	v, ok := rec.Get("unknown")
	if ok {
		t.Errorf("Get(unknown): expected ok=false, got true with value %v", v)
	}
	if v != nil {
		t.Errorf("Get(unknown): expected nil value, got %v", v)
	}
}

func TestRecord_AsMap(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	if _, err := db.RunQuery(ctx, `CREATE (:P {name: "Bob", age: 25})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := fetchRecord(t, db, `MATCH (n:P) RETURN n.name AS name, n.age AS age`, nil)

	m := rec.AsMap()
	if m["name"] != "Bob" {
		t.Errorf("AsMap[name]: got %v, want Bob", m["name"])
	}
	if len(m) != 2 {
		t.Errorf("AsMap len: got %d, want 2", len(m))
	}
}

func TestRecord_AsMap_IsCopy(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	if _, err := db.RunQuery(ctx, `CREATE (:P {x: 1})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := fetchRecord(t, db, `MATCH (n:P) RETURN n.x AS x`, nil)

	m := rec.AsMap()
	m["x"] = 99 // mutate the copy

	// The original record should be unaffected.
	v, ok := rec.Get("x")
	if !ok {
		t.Fatal("Get(x) after AsMap mutation: key missing")
	}
	// Value should be unchanged (int64 or float64 1, not 99).
	switch vt := v.(type) {
	case int64:
		if vt == 99 {
			t.Error("mutation of AsMap copy affected original record")
		}
	case float64:
		if vt == 99 {
			t.Error("mutation of AsMap copy affected original record")
		}
	}
}

func TestRecord_Keys(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	if _, err := db.RunQuery(ctx, `CREATE (:P {x: 1, y: 2})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := fetchRecord(t, db, `MATCH (n:P) RETURN n.x AS x, n.y AS y`, nil)

	keys := rec.Keys()
	if len(keys) != 2 || keys[0] != "x" || keys[1] != "y" {
		t.Errorf("Keys: got %v", keys)
	}
	// Mutating the returned slice must not affect the record.
	keys[0] = "z"
	ks := rec.Keys()
	if ks[0] != "x" {
		t.Errorf("Keys() not a copy: mutation affected original")
	}
}

func TestRecord_Values(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	if _, err := db.RunQuery(ctx, `CREATE (:P {x: 42})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := fetchRecord(t, db, `MATCH (n:P) RETURN n.x AS x`, nil)

	vals := rec.Values()
	if len(vals) != 1 {
		t.Fatalf("Values: got %d elements, want 1", len(vals))
	}
	switch v := vals[0].(type) {
	case int64:
		if v != 42 {
			t.Errorf("Values[0]: got %v, want 42", v)
		}
	case float64:
		if v != 42 {
			t.Errorf("Values[0]: got %v, want 42", v)
		}
	default:
		t.Errorf("Values[0]: unexpected type %T value %v", vals[0], vals[0])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ErrUnsupportedCypher
// ─────────────────────────────────────────────────────────────────────────────

func TestErrUnsupportedCypher_ErrorsAs(t *testing.T) {
	err := &graphlite.ErrUnsupportedCypher{
		Clause:   "UNION",
		Position: 15,
		Detail:   "UNION is not supported in v0.1",
	}

	var target *graphlite.ErrUnsupportedCypher
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed for ErrUnsupportedCypher")
	}
	if target.Clause != "UNION" {
		t.Errorf("Clause: got %q, want UNION", target.Clause)
	}
	if target.Position != 15 {
		t.Errorf("Position: got %d, want 15", target.Position)
	}
}

func TestErrUnsupportedCypher_ErrorString_WithPosition(t *testing.T) {
	err := &graphlite.ErrUnsupportedCypher{Clause: "CALL", Position: 5, Detail: "procedures not supported"}
	s := err.Error()
	if s == "" {
		t.Error("Error() returned empty string")
	}
	if !strings.Contains(s, "CALL") {
		t.Errorf("Error() does not mention clause name: %q", s)
	}
}

func TestErrUnsupportedCypher_ErrorString_NoPosition(t *testing.T) {
	err := &graphlite.ErrUnsupportedCypher{Clause: "UNION"}
	s := err.Error()
	if !strings.Contains(s, "UNION") {
		t.Errorf("Error() does not mention clause name: %q", s)
	}
}

func TestErrUnsupportedCypher_WrappedErrorsAs(t *testing.T) {
	inner := &graphlite.ErrUnsupportedCypher{Clause: "MERGE", Position: 0}
	wrapped := fmt.Errorf("planner: %w", inner)

	var target *graphlite.ErrUnsupportedCypher
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed for wrapped ErrUnsupportedCypher")
	}
	if target.Clause != "MERGE" {
		t.Errorf("Clause: got %q, want MERGE", target.Clause)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ErrMissingParameter
// ─────────────────────────────────────────────────────────────────────────────

func TestErrMissingParameter_ErrorsAs(t *testing.T) {
	err := &graphlite.ErrMissingParameter{Name: "minAge"}
	var target *graphlite.ErrMissingParameter
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed for ErrMissingParameter")
	}
	if target.Name != "minAge" {
		t.Errorf("Name: got %q, want minAge", target.Name)
	}
	if !strings.Contains(err.Error(), "minAge") {
		t.Errorf("Error() does not mention parameter name: %q", err.Error())
	}
}
