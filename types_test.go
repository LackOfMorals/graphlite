package graphlite_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LackOfMorals/graphlite"
)

// ----------------------------------------------------------------------------
// Node
// ----------------------------------------------------------------------------

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

// ----------------------------------------------------------------------------
// Relationship
// ----------------------------------------------------------------------------

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

// ----------------------------------------------------------------------------
// Record
// ----------------------------------------------------------------------------

func TestRecord_Get_Hit(t *testing.T) {
	rec := graphlite.NewRecord(
		[]string{"name", "age"},
		[]any{"Alice", int64(30)},
	)

	v, ok := rec.Get("name")
	if !ok {
		t.Fatal("Get(name): expected ok=true, got false")
	}
	if v != "Alice" {
		t.Errorf("Get(name): got %v, want Alice", v)
	}
}

func TestRecord_Get_Miss(t *testing.T) {
	rec := graphlite.NewRecord([]string{"name"}, []any{"Alice"})

	v, ok := rec.Get("unknown")
	if ok {
		t.Errorf("Get(unknown): expected ok=false, got true with value %v", v)
	}
	if v != nil {
		t.Errorf("Get(unknown): expected nil value, got %v", v)
	}
}

func TestRecord_AsMap(t *testing.T) {
	rec := graphlite.NewRecord(
		[]string{"name", "age"},
		[]any{"Bob", int64(25)},
	)

	m := rec.AsMap()
	if m["name"] != "Bob" {
		t.Errorf("AsMap[name]: got %v, want Bob", m["name"])
	}
	if m["age"] != int64(25) {
		t.Errorf("AsMap[age]: got %v, want 25", m["age"])
	}
	if len(m) != 2 {
		t.Errorf("AsMap len: got %d, want 2", len(m))
	}
}

func TestRecord_AsMap_IsCopy(t *testing.T) {
	rec := graphlite.NewRecord([]string{"x"}, []any{1})
	m := rec.AsMap()
	m["x"] = 99 // mutate the copy

	// The original record should be unaffected
	v, ok := rec.Get("x")
	if !ok || v != 1 {
		t.Errorf("mutation of AsMap copy affected original record: Get(x)=%v, ok=%v", v, ok)
	}
}

func TestRecord_NewRecord_PanicOnLengthMismatch(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on keys/values length mismatch, got none")
		}
	}()
	graphlite.NewRecord([]string{"a", "b"}, []any{1})
}

func TestRecord_Keys(t *testing.T) {
	rec := graphlite.NewRecord([]string{"x", "y"}, []any{1, 2})
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
	rec := graphlite.NewRecord([]string{"x"}, []any{42})
	vals := rec.Values()
	if len(vals) != 1 || vals[0] != 42 {
		t.Errorf("Values: got %v", vals)
	}
}

// ----------------------------------------------------------------------------
// ErrUnsupportedCypher
// ----------------------------------------------------------------------------

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
	// Should mention the clause name.
	if !contains(s, "CALL") {
		t.Errorf("Error() does not mention clause name: %q", s)
	}
}

func TestErrUnsupportedCypher_ErrorString_NoPosition(t *testing.T) {
	err := &graphlite.ErrUnsupportedCypher{Clause: "UNION"}
	s := err.Error()
	if !contains(s, "UNION") {
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

// ----------------------------------------------------------------------------
// ErrMissingParameter
// ----------------------------------------------------------------------------

func TestErrMissingParameter_ErrorsAs(t *testing.T) {
	err := &graphlite.ErrMissingParameter{Name: "minAge"}
	var target *graphlite.ErrMissingParameter
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed for ErrMissingParameter")
	}
	if target.Name != "minAge" {
		t.Errorf("Name: got %q, want minAge", target.Name)
	}
	if !contains(err.Error(), "minAge") {
		t.Errorf("Error() does not mention parameter name: %q", err.Error())
	}
}

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
