package graphlite_test

import (
	"context"
	"errors"
	"testing"

	"github.com/LackOfMorals/graphlite/v2"
)

// ─────────────────────────────────────────────────────────────────────────────
// GetProperty tests
// ─────────────────────────────────────────────────────────────────────────────

func TestGetProperty_NodeString(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:Person {name: "Alice"})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	qr, err := db.RunQuery(ctx, `MATCH (n:Person) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	rec, err := qr.Single(ctx)
	if err != nil {
		t.Fatalf("Single: %v", err)
	}
	val, ok := rec.Get("n")
	if !ok {
		t.Fatal("expected key 'n'")
	}
	node := val.(*graphlite.Node)

	name, err := graphlite.GetProperty[string](node, "name")
	if err != nil {
		t.Fatalf("GetProperty: %v", err)
	}
	if name != "Alice" {
		t.Errorf("expected Alice, got %q", name)
	}
}

func TestGetProperty_NodeNumeric(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:P {score: 42})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	qr, err := db.RunQuery(ctx, `MATCH (n:P) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	rec, err := qr.Single(ctx)
	if err != nil {
		t.Fatalf("Single: %v", err)
	}
	node := rec.Values()[0].(*graphlite.Node)

	score, err := graphlite.GetProperty[int64](node, "score")
	if err != nil {
		t.Fatalf("GetProperty[int64]: %v", err)
	}
	if score != 42 {
		t.Errorf("expected 42, got %d", score)
	}
}

func TestGetProperty_MissingKey(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:Q {x: 1})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	qr, err := db.RunQuery(ctx, `MATCH (n:Q) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	rec, err := qr.Single(ctx)
	if err != nil {
		t.Fatalf("Single: %v", err)
	}
	node := rec.Values()[0].(*graphlite.Node)

	_, err = graphlite.GetProperty[string](node, "missing")
	if err == nil {
		t.Error("expected error for missing key, got nil")
	}
}

func TestGetProperty_RelationshipProp(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:A)-[:LINK {weight: 3}]->(:B)`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	qr, err := db.RunQuery(ctx, `MATCH ()-[r:LINK]->() RETURN r`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	rec, err := qr.Single(ctx)
	if err != nil {
		t.Fatalf("Single: %v", err)
	}
	rel := rec.Values()[0].(*graphlite.Relationship)

	weight, err := graphlite.GetProperty[float64](rel, "weight")
	if err != nil {
		t.Fatalf("GetProperty[float64]: %v", err)
	}
	if weight != 3 {
		t.Errorf("expected 3, got %v", weight)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetRecordValue tests
// ─────────────────────────────────────────────────────────────────────────────

func TestGetRecordValue_Scalar(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:R {val: "hello"})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	qr, err := db.RunQuery(ctx, `MATCH (n:R) RETURN n.val AS v`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	rec, err := qr.Single(ctx)
	if err != nil {
		t.Fatalf("Single: %v", err)
	}

	v, isNil, err := graphlite.GetRecordValue[string](rec, "v")
	if err != nil {
		t.Fatalf("GetRecordValue: %v", err)
	}
	if isNil {
		t.Error("expected isNil=false")
	}
	if v != "hello" {
		t.Errorf("expected hello, got %q", v)
	}
}

func TestGetRecordValue_Node(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:NV {x: 7})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	qr, err := db.RunQuery(ctx, `MATCH (n:NV) RETURN n`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	rec, err := qr.Single(ctx)
	if err != nil {
		t.Fatalf("Single: %v", err)
	}

	node, isNil, err := graphlite.GetRecordValue[*graphlite.Node](rec, "n")
	if err != nil {
		t.Fatalf("GetRecordValue[*Node]: %v", err)
	}
	if isNil {
		t.Error("expected isNil=false")
	}
	if node == nil {
		t.Fatal("expected non-nil node")
	}
	if node.Props["x"] == nil {
		t.Error("expected x property")
	}
}

func TestGetRecordValue_MissingKey(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:MK {a: 1})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	qr, err := db.RunQuery(ctx, `MATCH (n:MK) RETURN n.a AS a`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	rec, err := qr.Single(ctx)
	if err != nil {
		t.Fatalf("Single: %v", err)
	}

	_, _, err = graphlite.GetRecordValue[string](rec, "missing")
	if err == nil {
		t.Error("expected error for missing key")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CollectT tests
// ─────────────────────────────────────────────────────────────────────────────

func TestCollectT_Names(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	for _, name := range []string{"Alice", "Bob", "Carol"} {
		if _, err := db.RunQuery(ctx, `CREATE (:CT {name: $n})`, map[string]any{"n": name}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	qr, err := db.RunQuery(ctx, `MATCH (n:CT) RETURN n.name AS name ORDER BY n.name`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}

	names, err := graphlite.CollectT(ctx, qr, func(rec *graphlite.Record) (string, error) {
		v, _, err := graphlite.GetRecordValue[string](rec, "name")
		return v, err
	})
	if err != nil {
		t.Fatalf("CollectT: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %d", len(names))
	}
	want := []string{"Alice", "Bob", "Carol"}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("names[%d]: want %q got %q", i, n, names[i])
		}
	}
}

func TestCollectT_MapperError(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:CE {v: 1})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	qr, err := db.RunQuery(ctx, `MATCH (n:CE) RETURN n.v AS v`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}

	sentinel := errors.New("mapper failed")
	_, err = graphlite.CollectT(ctx, qr, func(_ *graphlite.Record) (int, error) {
		return 0, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// SingleT tests
// ─────────────────────────────────────────────────────────────────────────────

func TestSingleT_ExactlyOne(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:ST {name: "Solo"})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	qr, err := db.RunQuery(ctx, `MATCH (n:ST) RETURN n.name AS name`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}

	name, err := graphlite.SingleT(ctx, qr, func(rec *graphlite.Record) (string, error) {
		v, _, err := graphlite.GetRecordValue[string](rec, "name")
		return v, err
	})
	if err != nil {
		t.Fatalf("SingleT: %v", err)
	}
	if name != "Solo" {
		t.Errorf("expected Solo, got %q", name)
	}
}

func TestSingleT_ErrNoRecords(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	qr, err := db.RunQuery(ctx, `MATCH (n:STEmpty) RETURN n.name AS name`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}

	_, err = graphlite.SingleT(ctx, qr, func(rec *graphlite.Record) (string, error) {
		v, _, e := graphlite.GetRecordValue[string](rec, "name")
		return v, e
	})
	if !errors.Is(err, graphlite.ErrNoRecords) {
		t.Errorf("expected ErrNoRecords, got %v", err)
	}
}

func TestSingleT_ErrMultipleRecords(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.RunQuery(ctx, `CREATE (:STM {v:1}), (:STM {v:2})`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	qr, err := db.RunQuery(ctx, `MATCH (n:STM) RETURN n.v AS v`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}

	_, err = graphlite.SingleT(ctx, qr, func(rec *graphlite.Record) (int64, error) {
		v, _, e := graphlite.GetRecordValue[int64](rec, "v")
		return v, e
	})
	if !errors.Is(err, graphlite.ErrMultipleRecords) {
		t.Errorf("expected ErrMultipleRecords, got %v", err)
	}
}
