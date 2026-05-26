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
	defer func() { _ = db.Close(ctx) }()

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

// ExampleWithBusyTimeout shows how to configure SQLite busy-wait behaviour.
func ExampleWithBusyTimeout() {
	db, err := graphlite.Open(":memory:", graphlite.WithBusyTimeout(5*time.Second))
	if err != nil {
		panic(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
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
	defer func() { _ = ro.Close(ctx) }()

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
	defer func() { _ = db.Close(ctx) }()

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

// ExampleDB_Snapshot demonstrates saving a consistent copy of an in-memory
// database to a file so it can be reopened later.
func ExampleDB_Snapshot() {
	ctx := context.Background()

	db, err := graphlite.Open(":memory:")
	if err != nil {
		panic(err)
	}
	defer func() { _ = db.Close(ctx) }()

	_, err = db.RunQuery(ctx, `CREATE (:Event {name: "Launch"})`, nil)
	if err != nil {
		panic(err)
	}

	dir, err := os.MkdirTemp("", "graphlite-snap-*")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	snapPath := filepath.Join(dir, "snap.db")
	if err := db.Snapshot(snapPath); err != nil {
		panic(err)
	}

	// Reopen the snapshot as a normal database.
	snap, err := graphlite.Open(snapPath)
	if err != nil {
		panic(err)
	}
	defer func() { _ = snap.Close(ctx) }()

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
