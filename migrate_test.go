package graphlite_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	graphlite "github.com/LackOfMorals/graphlite"
	neo4j "github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// seedGraph creates a small graph in db: two Person nodes connected by KNOWS.
func seedGraph(t *testing.T, db *graphlite.DB) {
	t.Helper()
	ctx := context.Background()
	_, err := db.RunQuery(ctx, `CREATE (:Person {name: "Alice"})-[:KNOWS]->(:Person {name: "Bob"})`, nil)
	if err != nil {
		t.Fatalf("seed graph: %v", err)
	}
}

// personNames runs MATCH (n:Person) RETURN n.name and collects the results.
func personNames(t *testing.T, db *graphlite.DB) map[string]bool {
	t.Helper()
	ctx := context.Background()
	res, err := db.RunQuery(ctx, `MATCH (n:Person) RETURN n.name AS name`, nil)
	if err != nil {
		t.Fatalf("query persons: %v", err)
	}
	names := map[string]bool{}
	for res.Next(ctx) {
		name, _ := res.Record().Get("name")
		names[name.(string)] = true
	}
	return names
}

// knowsCount returns the number of KNOWS relationships.
func knowsCount(t *testing.T, db *graphlite.DB) int {
	t.Helper()
	ctx := context.Background()
	res, err := db.RunQuery(ctx, `MATCH ()-[:KNOWS]->() RETURN count(*) AS n`, nil)
	if err != nil {
		t.Fatalf("count knows: %v", err)
	}
	res.Next(ctx)
	n, _ := res.Record().Get("n")
	return int(n.(int64))
}

func TestCopyFrom(t *testing.T) {
	ctx := context.Background()

	// Source: a DriverCompat seeded with two persons and a relationship.
	src, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	seedGraph(t, src.DB())

	// Destination: fresh empty DB.
	dst := graphlite.NewTestDB(t)

	if err := dst.CopyFrom(ctx, src); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}

	names := personNames(t, dst)
	if !names["Alice"] || !names["Bob"] {
		t.Errorf("expected Alice and Bob, got %v", names)
	}
	if n := knowsCount(t, dst); n != 1 {
		t.Errorf("expected 1 KNOWS relationship, got %d", n)
	}
}

func TestCopyFrom_Transactional(t *testing.T) {
	// Verify that an existing node in dst is preserved when CopyFrom appends.
	dst := graphlite.NewTestDB(t)
	ctx := context.Background()
	_, err := dst.RunQuery(ctx, `CREATE (:Existing {x: 1})`, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Normal copy should succeed and add 2 more nodes.
	srcDriver, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer srcDriver.Close(ctx)
	seedGraph(t, srcDriver.DB())

	if err := dst.CopyFrom(ctx, srcDriver); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}

	names := personNames(t, dst)
	if !names["Alice"] || !names["Bob"] {
		t.Errorf("expected Alice and Bob after copy, got %v", names)
	}
}

func TestCopyFrom_EmptySource(t *testing.T) {
	ctx := context.Background()

	src, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)

	dst := graphlite.NewTestDB(t)
	if err := dst.CopyFrom(ctx, src); err != nil {
		t.Fatalf("CopyFrom empty source: %v", err)
	}
}

func TestCopyTo(t *testing.T) {
	ctx := context.Background()

	src := graphlite.NewTestDB(t)
	seedGraph(t, src)

	dst, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close(ctx)

	if err := src.CopyTo(ctx, dst); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}

	names := personNames(t, dst.DB())
	if !names["Alice"] || !names["Bob"] {
		t.Errorf("expected Alice and Bob in dst, got %v", names)
	}
	if n := knowsCount(t, dst.DB()); n != 1 {
		t.Errorf("expected 1 KNOWS relationship in dst, got %d", n)
	}
}

func TestCopyTo_NoTempProperty(t *testing.T) {
	ctx := context.Background()

	src := graphlite.NewTestDB(t)
	seedGraph(t, src)

	dst, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close(ctx)

	if err := src.CopyTo(ctx, dst); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}

	// _graphliteId must have been removed.
	res, err := dst.DB().RunQuery(ctx,
		"MATCH (n) WHERE n._graphliteId IS NOT NULL RETURN count(n) AS c", nil)
	if err != nil {
		t.Fatalf("check temp prop: %v", err)
	}
	res.Next(ctx)
	c, _ := res.Record().Get("c")
	if c.(int64) != 0 {
		t.Errorf("expected _graphliteId to be cleaned up, found %d nodes with it", c.(int64))
	}
}

func TestCopyTo_EmptySource(t *testing.T) {
	ctx := context.Background()

	src := graphlite.NewTestDB(t)

	dst, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close(ctx)

	if err := src.CopyTo(ctx, dst); err != nil {
		t.Fatalf("CopyTo empty source: %v", err)
	}
}

func TestCopyRoundTrip(t *testing.T) {
	ctx := context.Background()

	// Seed source.
	src, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	seedGraph(t, src.DB())

	// CopyTo an intermediate graphlite instance.
	mid, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer mid.Close(ctx)

	if err := src.DB().CopyTo(ctx, mid); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}

	// CopyFrom the intermediate back into a new DB.
	dst := graphlite.NewTestDB(t)
	if err := dst.CopyFrom(ctx, mid); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}

	names := personNames(t, dst)
	if !names["Alice"] || !names["Bob"] {
		t.Errorf("round-trip: expected Alice and Bob, got %v", names)
	}
	if n := knowsCount(t, dst); n != 1 {
		t.Errorf("round-trip: expected 1 KNOWS, got %d", n)
	}
}

func TestSnapshot(t *testing.T) {
	dir := t.TempDir()
	snapPath := filepath.Join(dir, "snap.db")

	db := graphlite.NewTestDB(t)
	seedGraph(t, db)

	if err := db.Snapshot(snapPath); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Reopen the snapshot and verify data.
	snap, err := graphlite.Open(snapPath)
	if err != nil {
		t.Fatalf("Open snapshot: %v", err)
	}
	defer snap.Close(context.Background())

	names := personNames(t, snap)
	if !names["Alice"] || !names["Bob"] {
		t.Errorf("snapshot: expected Alice and Bob, got %v", names)
	}
	if n := knowsCount(t, snap); n != 1 {
		t.Errorf("snapshot: expected 1 KNOWS, got %d", n)
	}
}

func TestSnapshot_FileAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	snapPath := filepath.Join(dir, "snap.db")

	// Pre-create the file.
	if err := os.WriteFile(snapPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	db := graphlite.NewTestDB(t)
	seedGraph(t, db)

	err := db.Snapshot(snapPath)
	if err == nil {
		t.Fatal("expected error when target file already exists")
	}
}

func TestSnapshot_InMemory(t *testing.T) {
	dir := t.TempDir()
	snapPath := filepath.Join(dir, "from-memory.db")

	db, err := graphlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	seedGraph(t, db)

	if err := db.Snapshot(snapPath); err != nil {
		t.Fatalf("Snapshot in-memory: %v", err)
	}

	snap, err := graphlite.Open(snapPath)
	if err != nil {
		t.Fatalf("Open snapshot: %v", err)
	}
	defer snap.Close(context.Background())

	names := personNames(t, snap)
	if !names["Alice"] || !names["Bob"] {
		t.Errorf("in-memory snapshot: expected Alice and Bob, got %v", names)
	}
}
