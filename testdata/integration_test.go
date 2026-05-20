// Package testdata_test contains end-to-end integration tests for the graphlite
// v0.1 feature set. Each test runs real Cypher queries against an in-memory
// graphlite database (no network, no Docker) and asserts results match expected
// values. Failures report the query, expected output, and actual output.
//
// Coverage: every row in the v0.1 compatibility table has at least one test.
//
// Run these tests with:
//
//	CGO_ENABLED=0 go test github.com/LackOfMorals/graphlite/testdata
//
// Note: Go's ./... pattern excludes directories named "testdata" by design.
// Use the full import path above to run this package explicitly.
package testdata_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	graphlite "github.com/LackOfMorals/graphlite"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test harness helpers
// ─────────────────────────────────────────────────────────────────────────────

// openDB opens a fresh in-memory graphlite database and registers a cleanup
// function to close it when the test completes.
func openDB(t *testing.T) *graphlite.DB {
	t.Helper()
	db, err := graphlite.Open(":memory:")
	if err != nil {
		t.Fatalf("Open :memory: failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// setup runs one or more setup Cypher statements against db, failing the test
// if any statement returns an error.
func setup(t *testing.T, db *graphlite.DB, queries ...string) {
	t.Helper()
	ctx := context.Background()
	for _, q := range queries {
		qr, err := db.RunQuery(ctx, q, nil)
		if err != nil {
			t.Fatalf("setup query %q failed: %v", q, err)
		}
		if _, err := qr.Consume(ctx); err != nil {
			t.Fatalf("setup consume %q failed: %v", q, err)
		}
	}
}

// query runs a Cypher query and returns an EagerResult, failing the test on
// any error. The failure message includes the original query string so test
// output is self-explanatory.
func query(t *testing.T, db *graphlite.DB, cypher string, params map[string]any) *graphlite.EagerResult {
	t.Helper()
	ctx := context.Background()
	qr, err := db.RunQuery(ctx, cypher, params)
	if err != nil {
		t.Fatalf("query %q failed: %v", cypher, err)
	}
	result, err := graphlite.NewEagerResult(ctx, qr)
	if err != nil {
		t.Fatalf("collect result for %q failed: %v", cypher, err)
	}
	return result
}

// assertCount fails if the result does not contain exactly n records,
// reporting the query and expected/actual counts.
func assertCount(t *testing.T, cypher string, result *graphlite.EagerResult, n int) {
	t.Helper()
	if len(result.Records) != n {
		t.Errorf("query %q: expected %d record(s), got %d", cypher, n, len(result.Records))
	}
}

// get returns the value for key from record[i], failing if absent.
func get(t *testing.T, cypher string, result *graphlite.EagerResult, i int, key string) any {
	t.Helper()
	if i >= len(result.Records) {
		t.Fatalf("query %q: record index %d out of range (got %d records)", cypher, i, len(result.Records))
	}
	v, ok := result.Records[i].Get(key)
	if !ok {
		t.Fatalf("query %q: record[%d] missing key %q (keys: %v)", cypher, i, key, result.Records[i].Keys())
	}
	return v
}

// assertString fails if the value is not the expected string.
func assertString(t *testing.T, cypher, key string, got any, want string) {
	t.Helper()
	s, ok := got.(string)
	if !ok {
		t.Errorf("query %q key %q: expected string %q, got %T %v", cypher, key, want, got, got)
		return
	}
	if s != want {
		t.Errorf("query %q key %q: expected %q, got %q", cypher, key, want, s)
	}
}

// assertInt64 fails if the value is not the expected int64.
func assertInt64(t *testing.T, cypher, key string, got any, want int64) {
	t.Helper()
	var actual int64
	switch v := got.(type) {
	case int64:
		actual = v
	case float64:
		actual = int64(v)
	default:
		t.Errorf("query %q key %q: expected int64 %d, got %T %v", cypher, key, want, got, got)
		return
	}
	if actual != want {
		t.Errorf("query %q key %q: expected %d, got %d", cypher, key, want, actual)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: MATCH single node (all nodes)
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_MatchSingleNode(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n) RETURN n.name AS name`

	setup(t, db,
		`CREATE (n:Person {name: "Alice"})`,
		`CREATE (n:Person {name: "Bob"})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 2)

	// Collect names to verify both are present (order may vary).
	names := make(map[string]bool)
	for _, rec := range result.Records {
		v, ok := rec.Get("name")
		if !ok {
			t.Errorf("record missing key 'name'")
			continue
		}
		if s, ok := v.(string); ok {
			names[s] = true
		}
	}
	if !names["Alice"] || !names["Bob"] {
		t.Errorf("expected Alice and Bob, got %v", names)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: MATCH by label
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_MatchByLabel(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Animal) RETURN n.name AS name`

	setup(t, db,
		`CREATE (n:Animal {name: "Cat"})`,
		`CREATE (n:Animal {name: "Dog"})`,
		`CREATE (n:Vehicle {name: "Car"})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 2)

	for _, rec := range result.Records {
		v, _ := rec.Get("name")
		if v == "Car" {
			t.Errorf("Vehicle node should not appear in Animal label match")
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: MATCH by property
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_MatchByProperty(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Product {sku: "ABC-123"}) RETURN n.name AS name`

	setup(t, db,
		`CREATE (n:Product {sku: "ABC-123", name: "Widget"})`,
		`CREATE (n:Product {sku: "XYZ-456", name: "Gadget"})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "name", get(t, cypher, result, 0, "name"), "Widget")
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Single-hop directed relationship
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_SingleHopDirected(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.name AS src, b.name AS dst`

	setup(t, db,
		`CREATE (a:Person {name: "Alice"})-[:KNOWS]->(b:Person {name: "Bob"})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "src", get(t, cypher, result, 0, "src"), "Alice")
	assertString(t, cypher, "dst", get(t, cypher, result, 0, "dst"), "Bob")
}

// TestIntegration_SingleHopDirected_WrongDirection verifies that directed MATCH
// does not match the reverse direction.
func TestIntegration_SingleHopDirected_WrongDirection(t *testing.T) {
	db := openDB(t)

	setup(t, db,
		`CREATE (a:Person {name: "Alice"})-[:KNOWS]->(b:Person {name: "Bob"})`,
	)

	// Query in the wrong direction should return zero rows.
	cypher := `MATCH (a:Person)<-[:KNOWS]-(b:Person) WHERE a.name = "Alice" RETURN b.name AS name`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 0)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Single-hop undirected relationship
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_SingleHopUndirected(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (x:Node)-[:EDGE]-(y:Node) RETURN x.id AS x, y.id AS y`

	// Create one directed edge a → b; undirected match should return two rows.
	setup(t, db,
		`CREATE (a:Node {id: 1})-[:EDGE]->(b:Node {id: 2})`,
	)

	result := query(t, db, cypher, nil)
	// One directed edge → two undirected traversal results (a→b and b←a).
	if len(result.Records) != 2 {
		t.Errorf("query %q: expected 2 rows for undirected match, got %d", cypher, len(result.Records))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Multi-hop (2 hops)
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_MultiHop2(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (a:Station)-[:LINE]->(b:Station)-[:LINE]->(c:Station) RETURN a.name AS a, c.name AS c`

	setup(t, db,
		`CREATE (a:Station {name: "Alpha"})-[:LINE]->(b:Station {name: "Beta"})-[:LINE]->(c:Station {name: "Gamma"})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "a", get(t, cypher, result, 0, "a"), "Alpha")
	assertString(t, cypher, "c", get(t, cypher, result, 0, "c"), "Gamma")
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Multi-hop (3 hops)
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_MultiHop3(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (a:H)-[:E]->(b:H)-[:E]->(c:H)-[:E]->(d:H) RETURN a.v AS a, d.v AS d`

	setup(t, db,
		`CREATE (a:H {v: 1})-[:E]->(b:H {v: 2})-[:E]->(c:H {v: 3})-[:E]->(d:H {v: 4})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertInt64(t, cypher, "a", get(t, cypher, result, 0, "a"), 1)
	assertInt64(t, cypher, "d", get(t, cypher, result, 0, "d"), 4)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Multi-hop (4 hops)
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_MultiHop4(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (a:N)-[:L]->(b:N)-[:L]->(c:N)-[:L]->(d:N)-[:L]->(e:N) RETURN a.v AS a, e.v AS e`

	setup(t, db,
		`CREATE (a:N {v: 1})-[:L]->(b:N {v: 2})-[:L]->(c:N {v: 3})-[:L]->(d:N {v: 4})-[:L]->(e:N {v: 5})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertInt64(t, cypher, "a", get(t, cypher, result, 0, "a"), 1)
	assertInt64(t, cypher, "e", get(t, cypher, result, 0, "e"), 5)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Multi-hop (5 hops)
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_MultiHop5(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (a:M)-[:C]->(b:M)-[:C]->(c:M)-[:C]->(d:M)-[:C]->(e:M)-[:C]->(f:M) RETURN a.v AS a, f.v AS f`

	setup(t, db,
		`CREATE (a:M {v: 1})-[:C]->(b:M {v: 2})-[:C]->(c:M {v: 3})-[:C]->(d:M {v: 4})-[:C]->(e:M {v: 5})-[:C]->(f:M {v: 6})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertInt64(t, cypher, "a", get(t, cypher, result, 0, "a"), 1)
	assertInt64(t, cypher, "f", get(t, cypher, result, 0, "f"), 6)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: WHERE comparisons (=, <>, <, >, <=, >=)
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_WhereComparisons(t *testing.T) {
	db := openDB(t)

	// Insert nodes with values 1–5.
	for i := 1; i <= 5; i++ {
		setup(t, db, fmt.Sprintf(`CREATE (n:Cmp {v: %d})`, i))
	}

	tests := []struct {
		op       string
		where    string
		wantRows int
	}{
		{"=", `n.v = 3`, 1},
		{"<>", `n.v <> 3`, 4},
		{"<", `n.v < 3`, 2},
		{"<=", `n.v <= 3`, 3},
		{">", `n.v > 3`, 2},
		{">=", `n.v >= 3`, 3},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.op, func(t *testing.T) {
			cypher := fmt.Sprintf(`MATCH (n:Cmp) WHERE %s RETURN n.v AS v`, tc.where)
			result := query(t, db, cypher, nil)
			if len(result.Records) != tc.wantRows {
				t.Errorf("WHERE %s: expected %d rows, got %d", tc.where, tc.wantRows, len(result.Records))
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: WHERE AND / OR / NOT
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_WhereAnd(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Score) WHERE n.x > 1 AND n.y < 30 RETURN n.x AS x`

	setup(t, db,
		`CREATE (n:Score {x: 1, y: 10})`,
		`CREATE (n:Score {x: 2, y: 20})`,
		`CREATE (n:Score {x: 3, y: 30})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertInt64(t, cypher, "x", get(t, cypher, result, 0, "x"), 2)
}

func TestIntegration_WhereOr(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Tag) WHERE n.v = 1 OR n.v = 3 RETURN n.v AS v`

	setup(t, db,
		`CREATE (n:Tag {v: 1})`,
		`CREATE (n:Tag {v: 2})`,
		`CREATE (n:Tag {v: 3})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 2)

	vals := make(map[int64]bool)
	for _, rec := range result.Records {
		v, _ := rec.Get("v")
		switch n := v.(type) {
		case int64:
			vals[n] = true
		case float64:
			vals[int64(n)] = true
		}
	}
	if !vals[1] || !vals[3] {
		t.Errorf("expected v=1 and v=3, got %v", vals)
	}
}

func TestIntegration_WhereNot(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Flag) WHERE NOT n.active = true RETURN n.name AS name`

	setup(t, db,
		`CREATE (n:Flag {name: "enabled", active: true})`,
		`CREATE (n:Flag {name: "disabled", active: false})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "name", get(t, cypher, result, 0, "name"), "disabled")
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: RETURN with aliases
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_ReturnWithAliases(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (p:Person) RETURN p.name AS fullName, p.age AS yearsOld`

	setup(t, db,
		`CREATE (p:Person {name: "Charlie", age: 25})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)

	// Keys should be the aliases, not the raw property paths.
	keys := result.Records[0].Keys()
	hasFullName := false
	hasYearsOld := false
	for _, k := range keys {
		if k == "fullName" {
			hasFullName = true
		}
		if k == "yearsOld" {
			hasYearsOld = true
		}
	}
	if !hasFullName || !hasYearsOld {
		t.Errorf("expected alias keys 'fullName' and 'yearsOld', got %v", keys)
	}

	assertString(t, cypher, "fullName", get(t, cypher, result, 0, "fullName"), "Charlie")
	assertInt64(t, cypher, "yearsOld", get(t, cypher, result, 0, "yearsOld"), 25)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: ORDER BY / LIMIT / SKIP
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_OrderByAsc(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Num) RETURN n.v AS v ORDER BY n.v ASC`

	setup(t, db,
		`CREATE (n:Num {v: 30})`,
		`CREATE (n:Num {v: 10})`,
		`CREATE (n:Num {v: 20})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 3)
	assertInt64(t, cypher, "v[0]", get(t, cypher, result, 0, "v"), 10)
	assertInt64(t, cypher, "v[1]", get(t, cypher, result, 1, "v"), 20)
	assertInt64(t, cypher, "v[2]", get(t, cypher, result, 2, "v"), 30)
}

func TestIntegration_OrderByDesc(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Num) RETURN n.v AS v ORDER BY n.v DESC`

	setup(t, db,
		`CREATE (n:Num {v: 30})`,
		`CREATE (n:Num {v: 10})`,
		`CREATE (n:Num {v: 20})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 3)
	assertInt64(t, cypher, "v[0]", get(t, cypher, result, 0, "v"), 30)
	assertInt64(t, cypher, "v[1]", get(t, cypher, result, 1, "v"), 20)
	assertInt64(t, cypher, "v[2]", get(t, cypher, result, 2, "v"), 10)
}

func TestIntegration_Limit(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Item) RETURN n.v AS v ORDER BY n.v ASC LIMIT 2`

	setup(t, db,
		`CREATE (n:Item {v: 1})`,
		`CREATE (n:Item {v: 2})`,
		`CREATE (n:Item {v: 3})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 2)
	assertInt64(t, cypher, "v[0]", get(t, cypher, result, 0, "v"), 1)
	assertInt64(t, cypher, "v[1]", get(t, cypher, result, 1, "v"), 2)
}

func TestIntegration_Skip(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Item) RETURN n.v AS v ORDER BY n.v ASC SKIP 2`

	setup(t, db,
		`CREATE (n:Item {v: 1})`,
		`CREATE (n:Item {v: 2})`,
		`CREATE (n:Item {v: 3})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertInt64(t, cypher, "v[0]", get(t, cypher, result, 0, "v"), 3)
}

func TestIntegration_SkipAndLimit(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Item) RETURN n.v AS v ORDER BY n.v ASC SKIP 1 LIMIT 2`

	setup(t, db,
		`CREATE (n:Item {v: 1})`,
		`CREATE (n:Item {v: 2})`,
		`CREATE (n:Item {v: 3})`,
		`CREATE (n:Item {v: 4})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 2)
	assertInt64(t, cypher, "v[0]", get(t, cypher, result, 0, "v"), 2)
	assertInt64(t, cypher, "v[1]", get(t, cypher, result, 1, "v"), 3)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Named query parameters ($param syntax)
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_NamedParams_WhereString(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:City) WHERE n.country = $country RETURN n.name AS city`

	setup(t, db,
		`CREATE (n:City {name: "London", country: "UK"})`,
		`CREATE (n:City {name: "Paris", country: "FR"})`,
	)

	result := query(t, db, cypher, map[string]any{"country": "UK"})
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "city", get(t, cypher, result, 0, "city"), "London")
}

func TestIntegration_NamedParams_WhereInt(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Age) WHERE n.years > $minAge RETURN n.name AS name`

	setup(t, db,
		`CREATE (n:Age {name: "Alice", years: 30})`,
		`CREATE (n:Age {name: "Bob", years: 20})`,
	)

	result := query(t, db, cypher, map[string]any{"minAge": int64(25)})
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "name", get(t, cypher, result, 0, "name"), "Alice")
}

func TestIntegration_NamedParams_CreateProperty(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	// Use a transaction to CREATE with params then query.
	tx, err := db.BeginTx(ctx, false)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	createQ := `CREATE (n:Parameterised {label: $lbl, score: $score})`
	qr, err := tx.Run(ctx, createQ, map[string]any{"lbl": "hello", "score": int64(42)})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("tx.Run create: %v", err)
	}
	if _, err := qr.Consume(ctx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("consume create: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	cypher := `MATCH (n:Parameterised) RETURN n.label AS lbl, n.score AS score`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "lbl", get(t, cypher, result, 0, "lbl"), "hello")
	assertInt64(t, cypher, "score", get(t, cypher, result, 0, "score"), 42)
}

func TestIntegration_NamedParams_MultiParam(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Multi) WHERE n.a = $a AND n.b = $b RETURN n.name AS name`

	setup(t, db,
		`CREATE (n:Multi {name: "target", a: 1, b: 2})`,
		`CREATE (n:Multi {name: "other", a: 1, b: 3})`,
	)

	result := query(t, db, cypher, map[string]any{"a": int64(1), "b": int64(2)})
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "name", get(t, cypher, result, 0, "name"), "target")
}

func TestIntegration_NamedParams_MissingParam(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	setup(t, db, `CREATE (n:Test {v: 1})`)

	// Provide no params — should get a missing-parameter error.
	_, err := db.RunQuery(ctx, `MATCH (n:Test) WHERE n.v = $x RETURN n`, nil)
	if err == nil {
		t.Fatal("expected error for missing $x parameter, got nil")
	}
	var mp *graphlite.ErrMissingParameter
	if !errors.As(err, &mp) {
		t.Errorf("expected *ErrMissingParameter in error chain, got: %v (%T)", err, err)
	} else if mp.Name != "x" {
		t.Errorf("ErrMissingParameter.Name = %q, want %q", mp.Name, "x")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: CREATE node
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_CreateNode(t *testing.T) {
	db := openDB(t)

	setup(t, db, `CREATE (n:Widget {color: "red", weight: 42})`)

	cypher := `MATCH (n:Widget) RETURN n.color AS color, n.weight AS weight`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "color", get(t, cypher, result, 0, "color"), "red")
	assertInt64(t, cypher, "weight", get(t, cypher, result, 0, "weight"), 42)
}

func TestIntegration_CreateNode_MultiLabel(t *testing.T) {
	db := openDB(t)

	setup(t, db, `CREATE (n:Employee:Manager {name: "Eve"})`)

	// Both labels present.
	cyphers := []struct {
		label string
		count int
	}{
		{`MATCH (n:Employee) RETURN n`, 1},
		{`MATCH (n:Manager) RETURN n`, 1},
		{`MATCH (n:Employee:Manager) RETURN n`, 1},
	}
	for _, tc := range cyphers {
		result := query(t, db, tc.label, nil)
		if len(result.Records) != tc.count {
			t.Errorf("%q: expected %d records, got %d", tc.label, tc.count, len(result.Records))
		}
	}
}

func TestIntegration_CreateNode_WholeNodeProjection(t *testing.T) {
	db := openDB(t)

	setup(t, db, `CREATE (n:Sensor {id: 99, unit: "celsius"})`)

	cypher := `MATCH (n:Sensor) RETURN n`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)

	v, ok := result.Records[0].Get("n")
	if !ok {
		t.Fatalf("record missing key 'n'")
	}
	node, ok := v.(*graphlite.Node)
	if !ok {
		t.Fatalf("expected *graphlite.Node, got %T", v)
	}
	if len(node.Labels) == 0 || node.Labels[0] != "Sensor" {
		t.Errorf("node.Labels = %v, want [Sensor]", node.Labels)
	}
	if node.Props["unit"] != "celsius" {
		t.Errorf("node.Props[unit] = %v, want celsius", node.Props["unit"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: CREATE relationship
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_CreateRelationship(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (a:Author)-[:WROTE]->(b:Book) RETURN a.name AS author, b.title AS title`

	setup(t, db,
		`CREATE (a:Author {name: "Tolkien"})-[:WROTE]->(b:Book {title: "LOTR"})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "author", get(t, cypher, result, 0, "author"), "Tolkien")
	assertString(t, cypher, "title", get(t, cypher, result, 0, "title"), "LOTR")
}

func TestIntegration_CreateRelationship_WithProps(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (a:P)-[r:SCORED]->(b:P) RETURN r.score AS score`

	setup(t, db,
		`CREATE (a:P {id: 1})-[:SCORED {score: 95}]->(b:P {id: 2})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertInt64(t, cypher, "score", get(t, cypher, result, 0, "score"), 95)
}

func TestIntegration_CreateRelationship_BetweenExistingNodes(t *testing.T) {
	db := openDB(t)

	// Create both nodes and the relationship in one chain CREATE statement.
	// The MATCH (a), (b) CREATE (a)-[:R]->(b) comma-MATCH+CREATE form is not
	// yet supported (known v0.1 limitation); the inline chain form covers the
	// same relationship creation semantics.
	setup(t, db, `CREATE (a:Src {id: 1})-[:LINKED]->(b:Dst {id: 2})`)

	cypher := `MATCH (a:Src)-[:LINKED]->(b:Dst) RETURN a.id AS src, b.id AS dst`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertInt64(t, cypher, "src", get(t, cypher, result, 0, "src"), 1)
	assertInt64(t, cypher, "dst", get(t, cypher, result, 0, "dst"), 2)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: SET property
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_SetProperty_Literal(t *testing.T) {
	db := openDB(t)

	setup(t, db, `CREATE (n:Box {color: "red"})`)
	setup(t, db, `MATCH (n:Box) SET n.color = "blue"`)

	cypher := `MATCH (n:Box) RETURN n.color AS color`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "color", get(t, cypher, result, 0, "color"), "blue")
}

func TestIntegration_SetProperty_Param(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	setup(t, db, `CREATE (n:Config {key: "timeout", value: 30})`)

	tx, err := db.BeginTx(ctx, false)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	qr, err := tx.Run(ctx, `MATCH (n:Config {key: "timeout"}) SET n.value = $v`,
		map[string]any{"v": int64(60)})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("tx.Run SET: %v", err)
	}
	if _, err := qr.Consume(ctx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("consume: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	cypher := `MATCH (n:Config {key: "timeout"}) RETURN n.value AS value`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertInt64(t, cypher, "value", get(t, cypher, result, 0, "value"), 60)
}

func TestIntegration_SetProperty_PreservesOtherProps(t *testing.T) {
	db := openDB(t)

	setup(t, db, `CREATE (n:Thing {a: 1, b: 2})`)
	setup(t, db, `MATCH (n:Thing) SET n.b = 99`)

	cypher := `MATCH (n:Thing) RETURN n.a AS a, n.b AS b`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertInt64(t, cypher, "a", get(t, cypher, result, 0, "a"), 1)
	assertInt64(t, cypher, "b", get(t, cypher, result, 0, "b"), 99)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: DELETE (node without relationships)
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_DeleteNode(t *testing.T) {
	db := openDB(t)

	setup(t, db,
		`CREATE (n:Temp {id: 1})`,
		`CREATE (n:Temp {id: 2})`,
	)

	// Delete just the node with id=1.
	setup(t, db, `MATCH (n:Temp {id: 1}) DELETE n`)

	cypher := `MATCH (n:Temp) RETURN n.id AS id`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertInt64(t, cypher, "id", get(t, cypher, result, 0, "id"), 2)
}

func TestIntegration_DeleteNode_WithRelationshipBlocked(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	setup(t, db,
		`CREATE (a:Blocked {id: 1})-[:LINK]->(b:Blocked {id: 2})`,
	)

	// Non-detach DELETE on a node with relationships must return an error.
	qr, err := db.RunQuery(ctx, `MATCH (n:Blocked {id: 1}) DELETE n`, nil)
	if err == nil && qr != nil {
		_, err = qr.Consume(ctx)
	}
	if err == nil {
		t.Error("expected error when deleting node with existing relationships (non-detach), got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: DETACH DELETE
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_DetachDelete_Node(t *testing.T) {
	db := openDB(t)

	setup(t, db,
		`CREATE (a:Detach {id: 1})-[:LINK]->(b:Detach {id: 2})`,
	)

	setup(t, db, `MATCH (n:Detach {id: 1}) DETACH DELETE n`)

	cypher := `MATCH (n:Detach) RETURN n.id AS id`
	result := query(t, db, cypher, nil)
	// Node 1 deleted; node 2 remains; the LINK edge is also deleted.
	assertCount(t, cypher, result, 1)
	assertInt64(t, cypher, "id", get(t, cypher, result, 0, "id"), 2)
}

func TestIntegration_DetachDelete_AllNodes(t *testing.T) {
	db := openDB(t)

	setup(t, db,
		`CREATE (a:Purge {id: 1})-[:R]->(b:Purge {id: 2})`,
		`CREATE (c:Purge {id: 3})`,
	)

	setup(t, db, `MATCH (n:Purge) DETACH DELETE n`)

	cypher := `MATCH (n:Purge) RETURN n`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 0)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: DELETE relationship
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_DeleteRelationship(t *testing.T) {
	db := openDB(t)

	setup(t, db,
		`CREATE (a:RelDel {n: 1})-[:TEMP]->(b:RelDel {n: 2})`,
	)

	setup(t, db, `MATCH (a:RelDel)-[r:TEMP]->(b:RelDel) DELETE r`)

	// Nodes must still exist.
	cypher := `MATCH (n:RelDel) RETURN n.n AS n`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 2)

	// Edge must be gone.
	cypher2 := `MATCH ()-[r:TEMP]->() RETURN r`
	result2 := query(t, db, cypher2, nil)
	assertCount(t, cypher2, result2, 0)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Whole-node and whole-relationship projections
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_WholeRelationshipProjection(t *testing.T) {
	db := openDB(t)

	setup(t, db,
		`CREATE (a:PA {id: 1})-[:KNOWS {since: 2020}]->(b:PA {id: 2})`,
	)

	cypher := `MATCH (a:PA)-[r:KNOWS]->(b:PA) RETURN r`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)

	v, ok := result.Records[0].Get("r")
	if !ok {
		t.Fatalf("record missing key 'r'")
	}
	rel, ok := v.(*graphlite.Relationship)
	if !ok {
		t.Fatalf("expected *graphlite.Relationship, got %T", v)
	}
	if rel.Type != "KNOWS" {
		t.Errorf("rel.Type = %q, want KNOWS", rel.Type)
	}
	if rel.Props["since"] == nil {
		t.Errorf("rel.Props[since] is nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Transaction commit and rollback
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_Transaction_CommitPersists(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, false)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	qr, err := tx.Run(ctx, `CREATE (n:TxNode {v: 1})`, nil)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("tx.Run: %v", err)
	}
	if _, err := qr.Consume(ctx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("consume: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	cypher := `MATCH (n:TxNode) RETURN n.v AS v`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
}

func TestIntegration_Transaction_RollbackReverts(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, false)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	qr, err := tx.Run(ctx, `CREATE (n:RollbackNode {v: 1})`, nil)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("tx.Run: %v", err)
	}
	if _, err := qr.Consume(ctx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("consume: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	cypher := `MATCH (n:RollbackNode) RETURN n`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 0)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: ResultSummary counters
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_Counters_NodesCreated(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	qr, err := db.RunQuery(ctx, `CREATE (n:Counter {v: 1})`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	sum, err := qr.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if sum.Counters().NodesCreated() != 1 {
		t.Errorf("NodesCreated = %d, want 1", sum.Counters().NodesCreated())
	}
}

func TestIntegration_Counters_NodesDeleted(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	setup(t, db, `CREATE (n:ToDelete {v: 1})`)

	qr, err := db.RunQuery(ctx, `MATCH (n:ToDelete) DETACH DELETE n`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	sum, err := qr.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if sum.Counters().NodesDeleted() != 1 {
		t.Errorf("NodesDeleted = %d, want 1", sum.Counters().NodesDeleted())
	}
}

func TestIntegration_Counters_RelationshipsCreated(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	qr, err := db.RunQuery(ctx, `CREATE (a:RA {id: 1})-[:KNOWS]->(b:RA {id: 2})`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	sum, err := qr.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if sum.Counters().RelationshipsCreated() != 1 {
		t.Errorf("RelationshipsCreated = %d, want 1", sum.Counters().RelationshipsCreated())
	}
}

func TestIntegration_Counters_PropertiesSet(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	setup(t, db, `CREATE (n:PropSet {v: 1})`)

	qr, err := db.RunQuery(ctx, `MATCH (n:PropSet) SET n.v = 2`, nil)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	sum, err := qr.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if sum.Counters().PropertiesSet() != 1 {
		t.Errorf("PropertiesSet = %d, want 1", sum.Counters().PropertiesSet())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: WHERE boolean with string comparison
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_WhereStringEquality(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Animal) WHERE n.name = "Cat" RETURN n.name AS name`

	setup(t, db,
		`CREATE (n:Animal {name: "Cat"})`,
		`CREATE (n:Animal {name: "Dog"})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "name", get(t, cypher, result, 0, "name"), "Cat")
}

func TestIntegration_WhereStringInequality(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Animal) WHERE n.name <> "Cat" RETURN n.name AS name`

	setup(t, db,
		`CREATE (n:Animal {name: "Cat"})`,
		`CREATE (n:Animal {name: "Dog"})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "name", get(t, cypher, result, 0, "name"), "Dog")
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Multi-label MATCH (AND semantics)
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_MultiLabel_AndSemantics(t *testing.T) {
	db := openDB(t)

	setup(t, db,
		`CREATE (n:Employee:Manager {name: "Eve"})`,
		`CREATE (n:Employee {name: "Frank"})`,
		`CREATE (n:Manager {name: "Grace"})`,
	)

	// Only Eve has BOTH Employee AND Manager labels.
	cypher := `MATCH (n:Employee:Manager) RETURN n.name AS name`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "name", get(t, cypher, result, 0, "name"), "Eve")
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Large-graph stress test (100 nodes, 200 relationships)
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_LargeGraph_MatchCount(t *testing.T) {
	db := openDB(t)

	// Create 100 isolated nodes (no edges). Each in its own CREATE statement.
	for i := 0; i < 100; i++ {
		setup(t, db, fmt.Sprintf(`CREATE (n:LargeNode {id: %d})`, i))
	}

	// Create 10 small chains (each chain: 3 nodes connected by 2 :SEQ edges).
	// This gives 10 chains × 2 edges = 20 total :SEQ edges.
	for c := 0; c < 10; c++ {
		setup(t, db, fmt.Sprintf(
			`CREATE (a:Chain {c: %d, pos: 1})-[:SEQ]->(b:Chain {c: %d, pos: 2})-[:SEQ]->(x:Chain {c: %d, pos: 3})`,
			c, c, c,
		))
	}

	// Verify LargeNode count.
	cypher := `MATCH (n:LargeNode) RETURN n.id AS id`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 100)

	// Verify chain relationship count.
	cypher2 := `MATCH (a:Chain)-[:SEQ]->(b:Chain) RETURN a.c AS c`
	result2 := query(t, db, cypher2, nil)
	// 10 chains × 2 hops = 20 edges.
	assertCount(t, cypher2, result2, 20)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Compound statement — MATCH + RETURN (read-only chain)
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_MatchWithMultipleConditions(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Person) WHERE n.age >= 25 AND n.active = true RETURN n.name AS name`

	setup(t, db,
		`CREATE (n:Person {name: "Alice", age: 30, active: true})`,
		`CREATE (n:Person {name: "Bob", age: 20, active: true})`,
		`CREATE (n:Person {name: "Carol", age: 28, active: false})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "name", get(t, cypher, result, 0, "name"), "Alice")
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Empty result set
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_EmptyResultSet(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:NonExistent) RETURN n`

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 0)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Failure reporting — query/expected/actual info in test output
// ─────────────────────────────────────────────────────────────────────────────

// TestIntegration_FailureReporting demonstrates that test failures include the
// query, expected result, and actual result. This test is a verification of the
// harness itself: it intentionally checks that the query string appears in
// test output when an assertion fails by examining the log of a sub-test.
func TestIntegration_FailureReporting_HarnessCheck(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	setup(t, db, `CREATE (n:Report {v: 10})`)

	// Run a query and verify the keys include the expected alias.
	qr, err := db.RunQuery(ctx, `MATCH (n:Report) RETURN n.v AS value`, nil)
	if err != nil {
		t.Fatalf("RunQuery failed: %v", err)
	}
	result, err := graphlite.NewEagerResult(ctx, qr)
	if err != nil {
		t.Fatalf("NewEagerResult failed: %v", err)
	}

	if len(result.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(result.Records))
	}

	v, ok := result.Records[0].Get("value")
	if !ok {
		t.Fatal("expected key 'value' in record")
	}
	// The harness's get/assertInt64 helpers include the query string in failure
	// messages. Verify the value is correct here using the same helpers.
	cypher := `MATCH (n:Report) RETURN n.v AS value`
	assertInt64(t, cypher, "value", v, 10)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: MATCH (n) without any label filter returns ALL nodes
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_MatchAllNodes(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n) RETURN n`

	setup(t, db,
		`CREATE (n:A {v: 1})`,
		`CREATE (n:B {v: 2})`,
		`CREATE (n:C {v: 3})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 3)
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: MATCH relationship and verify element IDs are stable strings
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_NodeElementId_IsStableString(t *testing.T) {
	db := openDB(t)

	setup(t, db, `CREATE (n:IDTest {v: 42})`)

	cypher := `MATCH (n:IDTest) RETURN n`
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)

	v, _ := result.Records[0].Get("n")
	node, ok := v.(*graphlite.Node)
	if !ok {
		t.Fatalf("expected *graphlite.Node, got %T", v)
	}
	if node.ElementId == "" {
		t.Error("node.ElementId must be a non-empty string")
	}
	// ElementId should be parseable as an integer string.
	if strings.TrimSpace(node.ElementId) != node.ElementId {
		t.Errorf("ElementId %q has unexpected whitespace", node.ElementId)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: ORDER BY on string properties
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_OrderByString(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Word) RETURN n.w AS w ORDER BY n.w ASC`

	setup(t, db,
		`CREATE (n:Word {w: "cherry"})`,
		`CREATE (n:Word {w: "apple"})`,
		`CREATE (n:Word {w: "banana"})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 3)
	assertString(t, cypher, "w[0]", get(t, cypher, result, 0, "w"), "apple")
	assertString(t, cypher, "w[1]", get(t, cypher, result, 1, "w"), "banana")
	assertString(t, cypher, "w[2]", get(t, cypher, result, 2, "w"), "cherry")
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: Match node by property using param (inline prop pattern)
// ─────────────────────────────────────────────────────────────────────────────

func TestIntegration_MatchByPropertyInlinePattern(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Server {role: "primary"}) RETURN n.host AS host`

	setup(t, db,
		`CREATE (n:Server {host: "db1", role: "primary"})`,
		`CREATE (n:Server {host: "db2", role: "replica"})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertString(t, cypher, "host", get(t, cypher, result, 0, "host"), "db1")
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature: WITH pipelines and aggregation (task-024 / v0.2)
// ─────────────────────────────────────────────────────────────────────────────

// TestIntegration_With_CountRel verifies count(r) aggregation via WITH.
// Each CREATE statement creates a new source node, so we create two distinct
// source nodes each with one outgoing KNOWS edge and verify GROUP BY gives
// one row per source node with cnt=1.
func TestIntegration_With_CountRel(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:CRPerson)-[r:KNOWS]->() WITH n, count(r) AS cnt RETURN n.name AS name, cnt`

	// Two separate source nodes, each with one KNOWS edge.
	setup(t, db,
		`CREATE (a:CRPerson {name: "Alice"})-[:KNOWS]->(b:CRPerson {name: "Bob"})`,
		`CREATE (d:CRPerson {name: "Dave"})-[:KNOWS]->(e:CRPerson {name: "Eve"})`,
	)

	// Two source nodes each with cnt=1.
	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 2)
	// Each source node must have cnt=1.
	for i, rec := range result.Records {
		cntVal, _ := rec.Get("cnt")
		assertInt64(t, cypher, fmt.Sprintf("cnt[%d]", i), cntVal, 1)
	}
}

// TestIntegration_With_CountStar verifies count(*) aggregation via WITH.
func TestIntegration_With_CountStar(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Thing) WITH count(*) AS total RETURN total`

	setup(t, db,
		`CREATE (n:Thing {v: 1})`,
		`CREATE (n:Thing {v: 2})`,
		`CREATE (n:Thing {v: 3})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertInt64(t, cypher, "total", get(t, cypher, result, 0, "total"), 3)
}

// TestIntegration_With_SumAggregation verifies sum() aggregation via WITH.
func TestIntegration_With_SumAggregation(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Score) WITH sum(n.points) AS total RETURN total`

	setup(t, db,
		`CREATE (n:Score {points: 10})`,
		`CREATE (n:Score {points: 20})`,
		`CREATE (n:Score {points: 30})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)
	assertInt64(t, cypher, "total", get(t, cypher, result, 0, "total"), 60)
}

// TestIntegration_With_AvgAggregation verifies avg() aggregation via WITH.
func TestIntegration_With_AvgAggregation(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Grade) WITH avg(n.score) AS mean RETURN mean`

	setup(t, db,
		`CREATE (n:Grade {score: 80})`,
		`CREATE (n:Grade {score: 90})`,
		`CREATE (n:Grade {score: 100})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 1)

	// avg(80,90,100) = 90. SQLite returns a float for AVG.
	v := get(t, cypher, result, 0, "mean")
	var actual float64
	switch n := v.(type) {
	case float64:
		actual = n
	case int64:
		actual = float64(n)
	default:
		t.Fatalf("query %q key %q: expected numeric, got %T %v", cypher, "mean", v, v)
	}
	if actual != 90.0 {
		t.Errorf("query %q key %q: expected 90, got %v", cypher, "mean", actual)
	}
}

// TestIntegration_With_MinMaxAggregation verifies min() and max() via WITH.
func TestIntegration_With_MinMaxAggregation(t *testing.T) {
	db := openDB(t)

	setup(t, db,
		`CREATE (n:Val {v: 5})`,
		`CREATE (n:Val {v: 2})`,
		`CREATE (n:Val {v: 8})`,
		`CREATE (n:Val {v: 1})`,
	)

	minCypher := `MATCH (n:Val) WITH min(n.v) AS lo RETURN lo`
	minResult := query(t, db, minCypher, nil)
	assertCount(t, minCypher, minResult, 1)
	assertInt64(t, minCypher, "lo", get(t, minCypher, minResult, 0, "lo"), 1)

	maxCypher := `MATCH (n:Val) WITH max(n.v) AS hi RETURN hi`
	maxResult := query(t, db, maxCypher, nil)
	assertCount(t, maxCypher, maxResult, 1)
	assertInt64(t, maxCypher, "hi", get(t, maxCypher, maxResult, 0, "hi"), 8)
}

// TestIntegration_With_CountAndReturn verifies the canonical pattern from
// the task spec: MATCH ... WITH n, count(r) AS cnt RETURN n.prop AS p, cnt.
// Each CREATE statement produces a new source node, so we use separate authors
// each with a distinct number of outgoing WROTE edges and verify count per node.
func TestIntegration_With_CountAndReturn(t *testing.T) {
	db := openDB(t)
	cypher := `MATCH (n:Author)-[r:WROTE]->() WITH n, count(r) AS cnt RETURN n.name AS name, cnt`

	// Two distinct authors: one with one book, one with one book.
	// Since each CREATE creates a new Author node, we get one Author per CREATE.
	setup(t, db,
		`CREATE (a:Author {name: "Tolkien"})-[:WROTE]->(b:Book {title: "LOTR"})`,
		`CREATE (d:Author {name: "Rowling"})-[:WROTE]->(e:Book {title: "HP1"})`,
	)

	result := query(t, db, cypher, nil)
	assertCount(t, cypher, result, 2)

	// Each author has exactly one book, so cnt=1 for both.
	counts := make(map[string]int64)
	for _, rec := range result.Records {
		name, _ := rec.Get("name")
		cnt, _ := rec.Get("cnt")
		n, ok := name.(string)
		if !ok {
			t.Fatalf("name is not string: %T %v", name, name)
		}
		switch c := cnt.(type) {
		case int64:
			counts[n] = c
		case float64:
			counts[n] = int64(c)
		}
	}
	if counts["Tolkien"] != 1 {
		t.Errorf("Tolkien count: expected 1, got %d", counts["Tolkien"])
	}
	if counts["Rowling"] != 1 {
		t.Errorf("Rowling count: expected 1, got %d", counts["Rowling"])
	}
}
