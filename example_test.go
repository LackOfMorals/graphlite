package graphlite_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	graphlite "github.com/LackOfMorals/graphlite"
)

// ExampleOpen demonstrates opening an in-memory database and running a query.
func ExampleOpen() {
	ctx := context.Background()

	db, err := graphlite.Open(":memory:")
	if err != nil {
		panic(err)
	}
	defer db.Close(ctx)

	if _, err := db.RunQuery(ctx, `CREATE (n:Person {name: "Alice"})`, nil); err != nil {
		panic(err)
	}

	result, err := db.RunQuery(ctx, `MATCH (n:Person) RETURN n.name AS name`, nil)
	if err != nil {
		panic(err)
	}
	for result.Next(ctx) {
		fmt.Println(result.Record().Values()[0])
	}

	// Output:
	// Alice
}

// ExampleNewDriver demonstrates using the graphlite.Driver-compatible API.
// This is the recommended entry point for code that also runs against Neo4j.
func ExampleNewDriver() {
	ctx := context.Background()

	driver, err := graphlite.NewDriver(":memory:", graphlite.NoAuth())
	if err != nil {
		panic(err)
	}
	defer driver.Close(ctx)

	// ExecuteQuery is the simplest API tier.
	_, err = graphlite.ExecuteQuery[*graphlite.EagerResult](ctx, driver,
		`CREATE (:Person {name: "Bob"})-[:KNOWS]->(:Person {name: "Carol"})`,
		nil, graphlite.EagerResultTransformer,
	)
	if err != nil {
		panic(err)
	}

	result, err := graphlite.ExecuteQuery[*graphlite.EagerResult](ctx, driver,
		`MATCH (:Person {name: "Bob"})-[:KNOWS]->(f:Person) RETURN f.name AS name`,
		nil, graphlite.EagerResultTransformer,
	)
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Records[0].AsMap()["name"])

	// Output:
	// Carol
}

// ExampleWithBusyTimeout shows how to configure SQLite busy-wait behaviour.
func ExampleWithBusyTimeout() {
	db, err := graphlite.Open(":memory:", graphlite.WithBusyTimeout(5*time.Second))
	if err != nil {
		panic(err)
	}
	defer db.Close(context.Background())
	fmt.Println("option accepted")

	// Output:
	// option accepted
}

// ExampleWithReadOnly demonstrates opening a database in read-only mode.
func ExampleWithReadOnly() {
	ctx := context.Background()

	// Seed data in a normal read-write database.
	rw, err := graphlite.Open(":memory:")
	if err != nil {
		panic(err)
	}
	if _, err := rw.RunQuery(ctx, `CREATE (n:Config {key: "version", value: "1"})`, nil); err != nil {
		panic(err)
	}
	// In a real scenario you would close rw and reopen the file as read-only.
	// Here we demonstrate WithReadOnly rejecting writes.
	_ = rw.Close(ctx)

	ro, err := graphlite.Open(":memory:", graphlite.WithReadOnly())
	if err != nil {
		panic(err)
	}
	defer ro.Close(ctx)

	_, err = ro.RunQuery(ctx, `CREATE (n:Config)`, nil)
	fmt.Println(err == graphlite.ErrReadOnly)

	// Output:
	// true
}

// ExampleDB_Import demonstrates seeding a graph from JSON.
func ExampleDB_Import() {
	ctx := context.Background()

	db, err := graphlite.Open(":memory:")
	if err != nil {
		panic(err)
	}
	defer db.Close(ctx)

	const data = `{
		"nodes": [
			{"id": "1", "labels": ["Person"], "props": {"name": "Diana"}},
			{"id": "2", "labels": ["Person"], "props": {"name": "Eve"}}
		],
		"edges": [
			{"type": "KNOWS", "startId": "1", "endId": "2", "props": {}}
		]
	}`

	if err := db.Import(ctx, strings.NewReader(data), graphlite.FormatJSON); err != nil {
		panic(err)
	}

	result, err := db.RunQuery(ctx,
		`MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.name AS src, b.name AS dst`,
		nil,
	)
	if err != nil {
		panic(err)
	}
	for result.Next(ctx) {
		rec := result.Record().AsMap()
		fmt.Printf("%s -> %s\n", rec["src"], rec["dst"])
	}

	// Output:
	// Diana -> Eve
}

// ExampleDB_CopyFrom demonstrates seeding a graphlite database from another
// graphlite.Driver — useful for loading a subset of a real Neo4j graph into a
// local in-memory instance for testing.
func ExampleDB_CopyFrom() {
	ctx := context.Background()

	// Source: any graphlite.Driver. Here we use another graphlite instance.
	src, err := graphlite.NewDriver(":memory:", graphlite.NoAuth())
	if err != nil {
		panic(err)
	}
	defer src.Close(ctx)

	_, err = graphlite.ExecuteQuery[*graphlite.EagerResult](ctx, src,
		`CREATE (:City {name: "London"})-[:CONNECTED_TO]->(:City {name: "Paris"})`,
		nil, graphlite.EagerResultTransformer)
	if err != nil {
		panic(err)
	}

	// Destination: a fresh local database.
	dst, err := graphlite.Open(":memory:")
	if err != nil {
		panic(err)
	}
	defer dst.Close(ctx)

	if err := dst.CopyFrom(ctx, src); err != nil {
		panic(err)
	}

	result, err := dst.RunQuery(ctx,
		`MATCH (a:City)-[:CONNECTED_TO]->(b:City) RETURN a.name AS src, b.name AS dst`,
		nil)
	if err != nil {
		panic(err)
	}
	for result.Next(ctx) {
		rec := result.Record().AsMap()
		fmt.Printf("%s -> %s\n", rec["src"], rec["dst"])
	}

	// Output:
	// London -> Paris
}

// ExampleDB_CopyTo demonstrates promoting a graphlite database into another
// graphlite.Driver — useful for pushing locally built graph data to Neo4j.
func ExampleDB_CopyTo() {
	ctx := context.Background()

	// Source: a graphlite database with some graph data.
	src, err := graphlite.Open(":memory:")
	if err != nil {
		panic(err)
	}
	defer src.Close(ctx)

	_, err = src.RunQuery(ctx,
		`CREATE (:Product {name: "Widget"})-[:SHIPS_TO]->(:Region {name: "EU"})`,
		nil)
	if err != nil {
		panic(err)
	}

	// Destination: any graphlite.Driver. Here we use another graphlite instance.
	dst, err := graphlite.NewDriver(":memory:", graphlite.NoAuth())
	if err != nil {
		panic(err)
	}
	defer dst.Close(ctx)

	if err := src.CopyTo(ctx, dst); err != nil {
		panic(err)
	}

	result, err := graphlite.ExecuteQuery[*graphlite.EagerResult](ctx, dst,
		`MATCH (p:Product)-[:SHIPS_TO]->(r:Region) RETURN p.name AS product, r.name AS region`,
		nil, graphlite.EagerResultTransformer)
	if err != nil {
		panic(err)
	}
	rec := result.Records[0].AsMap()
	fmt.Printf("%s ships to %s\n", rec["product"], rec["region"])

	// Output:
	// Widget ships to EU
}

// ExampleDB_Snapshot demonstrates saving a consistent copy of an in-memory
// database to a file so it can be reopened later.
func ExampleDB_Snapshot() {
	ctx := context.Background()

	db, err := graphlite.Open(":memory:")
	if err != nil {
		panic(err)
	}
	defer db.Close(ctx)

	_, err = db.RunQuery(ctx, `CREATE (:Event {name: "Launch"})`, nil)
	if err != nil {
		panic(err)
	}

	dir, err := os.MkdirTemp("", "graphlite-snap-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	snapPath := filepath.Join(dir, "snap.db")
	if err := db.Snapshot(snapPath); err != nil {
		panic(err)
	}

	// Reopen the snapshot as a normal database.
	snap, err := graphlite.Open(snapPath)
	if err != nil {
		panic(err)
	}
	defer snap.Close(ctx)

	result, err := snap.RunQuery(ctx, `MATCH (e:Event) RETURN e.name AS name`, nil)
	if err != nil {
		panic(err)
	}
	for result.Next(ctx) {
		fmt.Println(result.Record().Values()[0])
	}

	// Output:
	// Launch
}
