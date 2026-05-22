//go:build !unit

// Package bench_test contains standard Go benchmarks for graphlite.
//
// Run all benchmarks:
//
//	go test -bench=. -benchtime=10s ./bench/... | tee bench/results/latest.txt
//
// Run a single benchmark:
//
//	go test -bench=BenchmarkMatchNodeByID ./bench/...
//
// Enable the 1M-node benchmark (disabled by default to avoid CI timeouts):
//
//	go test -bench=BenchmarkSingleHopTraversal_1M -bench-1m ./bench/...
package bench_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"sync"
	"testing"

	graphlite "github.com/LackOfMorals/graphlite"
)

// bench1M is a flag that enables the 1M-node benchmark. Off by default because
// setup takes ~30s and would exceed typical CI job time limits.
var bench1M = flag.Bool("bench-1m", false, "enable the 1M-node single-hop benchmark")

// ─────────────────────────────────────────────────────────────────────────────
// Fixtures: lazily initialised, shared across all benchmarks in the process.
// ─────────────────────────────────────────────────────────────────────────────

// smallDB is a 1K-node in-memory database used for targeted micro-benchmarks.
var (
	smallOnce sync.Once
	smallDB   *graphlite.DB
	smallErr  error
)

func getSmallDB(b *testing.B) *graphlite.DB {
	b.Helper()
	smallOnce.Do(func() {
		db, err := graphlite.Open(":memory:")
		if err != nil {
			smallErr = fmt.Errorf("open small db: %w", err)
			return
		}
		if err := seedNodes(db, 1_000); err != nil {
			_ = db.Close(context.Background())
			smallErr = fmt.Errorf("seed small db: %w", err)
			return
		}
		smallDB = db
	})
	if smallErr != nil {
		b.Fatalf("fixture setup: %v", smallErr)
	}
	return smallDB
}

// medium100KDB is a 100K-node, 100K-edge in-memory database.
var (
	medium100KOnce sync.Once
	medium100KDB   *graphlite.DB
	medium100KErr  error
)

func get100KDB(b *testing.B) *graphlite.DB {
	b.Helper()
	medium100KOnce.Do(func() {
		db, err := graphlite.Open(":memory:")
		if err != nil {
			medium100KErr = fmt.Errorf("open 100K db: %w", err)
			return
		}
		if err := seedGraph(db, 100_000, 100_000); err != nil {
			_ = db.Close(context.Background())
			medium100KErr = fmt.Errorf("seed 100K db: %w", err)
			return
		}
		medium100KDB = db
	})
	if medium100KErr != nil {
		b.Fatalf("fixture setup: %v", medium100KErr)
	}
	return medium100KDB
}

// large1MDB is a 1M-node, 500K-edge in-memory database.
var (
	large1MOnce sync.Once
	large1MDB   *graphlite.DB
	large1MErr  error
)

func get1MDB(b *testing.B) *graphlite.DB {
	b.Helper()
	large1MOnce.Do(func() {
		db, err := graphlite.Open(":memory:")
		if err != nil {
			large1MErr = fmt.Errorf("open 1M db: %w", err)
			return
		}
		if err := seedGraph(db, 1_000_000, 500_000); err != nil {
			_ = db.Close(context.Background())
			large1MErr = fmt.Errorf("seed 1M db: %w", err)
			return
		}
		large1MDB = db
	})
	if large1MErr != nil {
		b.Fatalf("fixture setup: %v", large1MErr)
	}
	return large1MDB
}

// ─────────────────────────────────────────────────────────────────────────────
// Seed helpers
// ─────────────────────────────────────────────────────────────────────────────

// importDoc is the shape used with graphlite.FormatJSON.
type importDoc struct {
	Nodes []importNode `json:"nodes"`
	Edges []importEdge `json:"edges"`
}

type importNode struct {
	ID     string         `json:"id"`
	Labels []string       `json:"labels"`
	Props  map[string]any `json:"props"`
}

type importEdge struct {
	Type    string         `json:"type"`
	StartID string         `json:"startId"`
	EndID   string         `json:"endId"`
	Props   map[string]any `json:"props"`
}

// seedNodes imports n Person nodes (no edges) using JSON bulk import.
func seedNodes(db *graphlite.DB, n int) error {
	doc := importDoc{Nodes: make([]importNode, n)}
	for i := range doc.Nodes {
		doc.Nodes[i] = importNode{
			ID:     fmt.Sprintf("n%d", i),
			Labels: []string{"Person"},
			Props:  map[string]any{"name": fmt.Sprintf("Person%d", i), "age": float64(20 + i%60)},
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return db.Import(context.Background(), bytes.NewReader(raw), graphlite.FormatJSON)
}

// seedGraph imports nodeCount Person nodes and edgeCount KNOWS edges (as a ring
// so every node participates) using JSON bulk import.
func seedGraph(db *graphlite.DB, nodeCount, edgeCount int) error {
	nodes := make([]importNode, nodeCount)
	for i := range nodes {
		nodes[i] = importNode{
			ID:     fmt.Sprintf("n%d", i),
			Labels: []string{"Person"},
			Props:  map[string]any{"name": fmt.Sprintf("Person%d", i), "age": float64(20 + i%60)},
		}
	}
	edges := make([]importEdge, edgeCount)
	for i := range edges {
		edges[i] = importEdge{
			Type:    "KNOWS",
			StartID: fmt.Sprintf("n%d", i%nodeCount),
			EndID:   fmt.Sprintf("n%d", (i+1)%nodeCount),
		}
	}
	doc := importDoc{Nodes: nodes, Edges: edges}
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return db.Import(context.Background(), bytes.NewReader(raw), graphlite.FormatJSON)
}

// ─────────────────────────────────────────────────────────────────────────────
// Benchmarks
// ─────────────────────────────────────────────────────────────────────────────

// BenchmarkMatchNodeByID measures the cost of a MATCH by a specific property
// value (name) on a 1K-node graph. Because graphlite does not expose raw
// integer IDs in the public API, we use a unique name property as the
// functional equivalent of "match by ID".
func BenchmarkMatchNodeByID(b *testing.B) {
	db := getSmallDB(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := range b.N {
		name := fmt.Sprintf("Person%d", i%1_000)
		res, err := db.RunQuery(ctx, `MATCH (n:Person {name: $name}) RETURN n.name AS name`, map[string]any{"name": name})
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		if _, err := res.Collect(ctx); err != nil {
			b.Fatalf("collect: %v", err)
		}
	}
}

// BenchmarkMatchNodeByProperty_100K measures MATCH (n:Person {name: ?}) on a
// 100K-node graph — a property-equality scan backed by the label index.
func BenchmarkMatchNodeByProperty_100K(b *testing.B) {
	db := get100KDB(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := range b.N {
		name := fmt.Sprintf("Person%d", i%100_000)
		res, err := db.RunQuery(ctx, `MATCH (n:Person {name: $name}) RETURN n.name AS name`, map[string]any{"name": name})
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		if _, err := res.Collect(ctx); err != nil {
			b.Fatalf("collect: %v", err)
		}
	}
}

// BenchmarkSingleHopTraversal_100K measures MATCH (a)-[r:KNOWS]->(b) on a
// 100K-node, 100K-edge graph. Each iteration starts from a different node so
// the plan cache and SQLite page cache are exercised uniformly.
func BenchmarkSingleHopTraversal_100K(b *testing.B) {
	db := get100KDB(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := range b.N {
		name := fmt.Sprintf("Person%d", i%100_000)
		res, err := db.RunQuery(ctx,
			`MATCH (a:Person {name: $name})-[r:KNOWS]->(b:Person) RETURN b.name AS name`,
			map[string]any{"name": name},
		)
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		if _, err := res.Collect(ctx); err != nil {
			b.Fatalf("collect: %v", err)
		}
	}
}

// BenchmarkSingleHopTraversal_1M measures MATCH (a)-[r:KNOWS]->(b) on a
// 1M-node, 500K-edge graph. This benchmark is disabled by default because
// setup allocates several hundred MB of in-memory SQLite data (~30s on a
// typical laptop). Enable it with: go test -bench=BenchmarkSingleHopTraversal_1M -bench-1m ./bench/...
func BenchmarkSingleHopTraversal_1M(b *testing.B) {
	if !*bench1M {
		b.Skip("skipped: requires -bench-1m flag (setup takes ~30s, ~500MB RAM)")
	}
	db := get1MDB(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := range b.N {
		name := fmt.Sprintf("Person%d", i%1_000_000)
		res, err := db.RunQuery(ctx,
			`MATCH (a:Person {name: $name})-[r:KNOWS]->(b:Person) RETURN b.name AS name`,
			map[string]any{"name": name},
		)
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		if _, err := res.Collect(ctx); err != nil {
			b.Fatalf("collect: %v", err)
		}
	}
}

// BenchmarkCreateSingle measures the throughput of creating a single node per
// iteration in a fresh in-memory database. Each benchmark iteration gets a
// unique name to avoid property uniqueness concerns.
func BenchmarkCreateSingle(b *testing.B) {
	db, err := graphlite.Open(":memory:")
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { _ = db.Close(context.Background()) })
	ctx := context.Background()
	b.ResetTimer()
	for i := range b.N {
		res, err := db.RunQuery(ctx,
			`CREATE (n:Person {name: $name, age: $age})`,
			map[string]any{"name": fmt.Sprintf("Node%d", i), "age": int64(i % 100)},
		)
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		if _, err := res.Collect(ctx); err != nil {
			b.Fatalf("collect: %v", err)
		}
	}
}

// BenchmarkCreateBatch_1000 measures the throughput of creating 1000 nodes in
// a single JSON bulk import call (one transaction). Each iteration uses a
// freshly opened in-memory database.
func BenchmarkCreateBatch_1000(b *testing.B) {
	ctx := context.Background()
	// Pre-build the import payload (identical for every iteration — we measure
	// import throughput, not JSON serialisation overhead).
	nodes := make([]importNode, 1_000)
	for i := range nodes {
		nodes[i] = importNode{
			ID:     fmt.Sprintf("n%d", i),
			Labels: []string{"Person"},
			Props:  map[string]any{"name": fmt.Sprintf("BatchNode%d", i), "age": float64(i % 100)},
		}
	}
	doc := importDoc{Nodes: nodes}
	payload, err := json.Marshal(doc)
	if err != nil {
		b.Fatalf("marshal: %v", err)
	}

	b.ResetTimer()
	for range b.N {
		db, err := graphlite.Open(":memory:")
		if err != nil {
			b.Fatalf("open: %v", err)
		}
		if err := db.Import(ctx, bytes.NewReader(payload), graphlite.FormatJSON); err != nil {
			_ = db.Close(context.Background())
			b.Fatalf("import: %v", err)
		}
		_ = db.Close(context.Background())
	}
}

// BenchmarkAggregationPipeline measures the cost of a
// MATCH ... WITH n RETURN count(n) aggregation pipeline on a 100K-node graph.
func BenchmarkAggregationPipeline(b *testing.B) {
	db := get100KDB(b)
	ctx := context.Background()
	b.ResetTimer()
	for range b.N {
		res, err := db.RunQuery(ctx,
			`MATCH (n:Person) WITH n RETURN count(n) AS cnt`,
			nil,
		)
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		if _, err := res.Collect(ctx); err != nil {
			b.Fatalf("collect: %v", err)
		}
	}
}
