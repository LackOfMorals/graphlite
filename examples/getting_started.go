//go:build ignore

// Getting-started example for graphlite.
//
// Run with:
//
//	go run github.com/LackOfMorals/graphlite/examples/getting_started.go
//
// or from the repo root:
//
//	go run examples/getting_started.go
package main

import (
	"context"
	"fmt"
	"strings"

	graphlite "github.com/LackOfMorals/graphlite"
	neo4j "github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func main() {
	ctx := context.Background()

	fmt.Println("=== 1. Native API ===")
	nativeAPIExample(ctx)

	fmt.Println("\n=== 2. JSON bulk import ===")
	importExample(ctx)

	fmt.Println("\n=== 3. Neo4j-compatible API (all three transaction tiers) ===")
	neo4jCompatExample(ctx)
}

// nativeAPIExample shows the lightweight native graphlite API.
// Use this when you don't need the neo4j.Driver interface.
func nativeAPIExample(ctx context.Context) {
	db, err := graphlite.Open(":memory:")
	must(err)
	defer db.Close()

	// Write: CREATE returns counters, not rows.
	qr, err := db.RunQuery(ctx,
		`CREATE (a:Person {name: $name, age: $age})-[:LIVES_IN]->(c:City {name: $city})`,
		map[string]any{"name": "Alice", "age": 30, "city": "London"},
	)
	must(err)
	result, err := qr.Consume(ctx)
	must(err)
	fmt.Printf("created %d node(s), %d relationship(s)\n",
		result.Counters().NodesCreated(),
		result.Counters().RelationshipsCreated(),
	)

	// Read: iterate the lazy cursor row by row.
	qr, err = db.RunQuery(ctx,
		`MATCH (p:Person)-[:LIVES_IN]->(c:City) RETURN p.name AS name, c.name AS city`,
		nil,
	)
	must(err)
	for qr.Next(ctx) {
		rec := qr.Record()
		name, _ := rec.Get("name")
		city, _ := rec.Get("city")
		fmt.Printf("  %s lives in %s\n", name, city)
	}
	must(qr.Err())

	// Parameterised query.
	qr, err = db.RunQuery(ctx,
		`MATCH (p:Person {name: $name}) RETURN p.age AS age`,
		map[string]any{"name": "Alice"},
	)
	must(err)
	eager, err := graphlite.NewEagerResult(ctx, qr)
	must(err)
	age, _ := eager.Records[0].Get("age")
	fmt.Printf("  Alice's age: %v\n", age)
}

// importExample seeds a database from a JSON payload in one atomic transaction.
func importExample(ctx context.Context) {
	db, err := graphlite.Open(":memory:")
	must(err)
	defer db.Close()

	payload := `{
		"nodes": [
			{"id": "alice", "labels": ["Person"], "props": {"name": "Alice"}},
			{"id": "bob",   "labels": ["Person"], "props": {"name": "Bob"}},
			{"id": "graph", "labels": ["Topic"],  "props": {"name": "Graph Databases"}}
		],
		"edges": [
			{"type": "KNOWS",           "startId": "alice", "endId": "bob",   "props": {}},
			{"type": "INTERESTED_IN",   "startId": "alice", "endId": "graph", "props": {}},
			{"type": "INTERESTED_IN",   "startId": "bob",   "endId": "graph", "props": {}}
		]
	}`

	err = db.Import(ctx, strings.NewReader(payload), graphlite.FormatJSON)
	must(err)

	qr, err := db.RunQuery(ctx,
		`MATCH (p:Person)-[:INTERESTED_IN]->(t:Topic) RETURN p.name AS person, t.name AS topic`,
		nil,
	)
	must(err)
	for qr.Next(ctx) {
		rec := qr.Record()
		person, _ := rec.Get("person")
		topic, _ := rec.Get("topic")
		fmt.Printf("  %s is interested in %s\n", person, topic)
	}
	must(qr.Err())
}

// neo4jCompatExample demonstrates graphlite as a drop-in neo4j.Driver substitute.
//
// In production code, replace the first line with:
//
//	driver, err := neo4j.NewDriverWithContext("bolt://localhost:7687", neo4j.BasicAuth(...))
//
// In tests, use graphlite.NewDriver(":memory:", nil) — no other changes needed.
func neo4jCompatExample(ctx context.Context) {
	// One line change to go from Neo4j Aura → graphlite:
	driver, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	must(err)
	defer driver.Close(ctx)

	// ── Tier 1: neo4j.ExecuteQuery ────────────────────────────────────────────
	// The simplest API: auto-managed session + transaction + eager result.
	_, err = neo4j.ExecuteQuery[*neo4j.EagerResult](ctx, driver,
		`CREATE (:Developer {name: "Carol", lang: "Go"})`,
		nil,
		neo4j.EagerResultTransformer,
	)
	must(err)
	fmt.Println("  Tier 1: node created via neo4j.ExecuteQuery")

	result, err := neo4j.ExecuteQuery[*neo4j.EagerResult](ctx, driver,
		`MATCH (d:Developer) RETURN d.name AS name, d.lang AS lang`,
		nil,
		neo4j.EagerResultTransformer,
	)
	must(err)
	for _, rec := range result.Records {
		name, _ := rec.Get("name")
		lang, _ := rec.Get("lang")
		fmt.Printf("  Tier 1: %s writes %s\n", name, lang)
	}

	// ── Tier 2: session.ExecuteWrite / ExecuteRead ────────────────────────────
	// Use when you need access to the ManagedTransaction to batch multiple
	// queries inside a single auto-retried transaction.
	sess := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer sess.Close(ctx)

	_, err = sess.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(ctx,
			`CREATE (:Developer {name: "Dave", lang: "Rust"})`,
			nil,
		)
		return nil, err
	})
	must(err)
	fmt.Println("  Tier 2: node created via session.ExecuteWrite")

	names, err := sess.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			`MATCH (d:Developer) RETURN d.name AS name ORDER BY d.name`,
			nil,
		)
		if err != nil {
			return nil, err
		}
		var names []string
		for result.Next(ctx) {
			name, _ := result.Record().Get("name")
			names = append(names, fmt.Sprintf("%v", name))
		}
		return names, result.Err()
	})
	must(err)
	fmt.Printf("  Tier 2: all developers: %v\n", names)

	// ── Tier 3: explicit BeginTransaction ─────────────────────────────────────
	// Use when you need manual control over commit / rollback — e.g. to roll
	// back on a business-logic error without returning an error from the work fn.
	tx, err := sess.BeginTransaction(ctx)
	must(err)

	_, err = tx.Run(ctx,
		`CREATE (:Developer {name: "Eve", lang: "Python"})`,
		nil,
	)
	must(err)

	// Intentional rollback: Eve never makes it into the database.
	must(tx.Rollback(ctx))
	fmt.Println("  Tier 3: transaction rolled back — Eve not persisted")

	tx, err = sess.BeginTransaction(ctx)
	must(err)
	_, err = tx.Run(ctx,
		`CREATE (:Developer {name: "Frank", lang: "TypeScript"})`,
		nil,
	)
	must(err)
	must(tx.Commit(ctx))
	fmt.Println("  Tier 3: transaction committed — Frank persisted")

	final, err := neo4j.ExecuteQuery[*neo4j.EagerResult](ctx, driver,
		`MATCH (d:Developer) RETURN d.name AS name ORDER BY d.name`,
		nil,
		neo4j.EagerResultTransformer,
	)
	must(err)
	var allNames []string
	for _, rec := range final.Records {
		name, _ := rec.Get("name")
		allNames = append(allNames, fmt.Sprintf("%v", name))
	}
	fmt.Printf("  Final developer roster: %v\n", allNames)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
