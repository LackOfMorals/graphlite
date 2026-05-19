package graphlite_test

import (
	"context"
	"testing"

	graphlite "github.com/LackOfMorals/graphlite"
	neo4j "github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// TestDriverCompatInterfaceAssertion verifies the compile-time interface
// assertion var _ neo4j.Driver = (*DriverCompat)(nil) in neo4j.go.
// If DriverCompat drifts from neo4j.Driver this test will fail to compile.
func TestDriverCompatInterfaceAssertion(t *testing.T) {
	var _ neo4j.Driver = (*graphlite.DriverCompat)(nil)
}

// TestNewDriverMemory checks that NewDriver(":memory:", nil) returns a value
// assignable to neo4j.Driver without error.
func TestNewDriverMemory(t *testing.T) {
	d, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver(:memory:, NoAuth): unexpected error: %v", err)
	}
	defer d.Close(context.Background())

	// Must be assignable to neo4j.Driver.
	var _ neo4j.Driver = d
}

// TestNewDriverBasicAuth checks that neo4j.BasicAuth is accepted without error.
func TestNewDriverBasicAuth(t *testing.T) {
	auth := neo4j.BasicAuth("user", "pass", "")
	d, err := graphlite.NewDriver(":memory:", auth)
	if err != nil {
		t.Fatalf("NewDriver with BasicAuth: unexpected error: %v", err)
	}
	defer d.Close(context.Background())
}

// TestNewDriverNoAuth checks that neo4j.NoAuth() is accepted without error.
func TestNewDriverNoAuth(t *testing.T) {
	d, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver with NoAuth: unexpected error: %v", err)
	}
	defer d.Close(context.Background())
}

// TestNewDriverNilAuth checks that nil auth is accepted without error.
func TestNewDriverNilAuth(t *testing.T) {
	d, err := graphlite.NewDriver(":memory:", neo4j.AuthToken{})
	if err != nil {
		t.Fatalf("NewDriver with nil-equivalent auth: unexpected error: %v", err)
	}
	defer d.Close(context.Background())
}

// TestSessionConfigDatabaseName checks that SessionConfig.DatabaseName is
// accepted and ignored without error.
func TestSessionConfigDatabaseName(t *testing.T) {
	ctx := context.Background()
	d, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(ctx)

	sess := d.NewSession(ctx, neo4j.SessionConfig{
		DatabaseName: "my-db",
	})
	if sess == nil {
		t.Fatal("NewSession returned nil")
	}
	defer sess.Close(ctx)
}

// TestDriverCloseReleasesResources checks that Close releases all resources
// cleanly and can be called without error.
func TestDriverCloseReleasesResources(t *testing.T) {
	ctx := context.Background()
	d, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if err := d.Close(ctx); err != nil {
		t.Fatalf("Close: unexpected error: %v", err)
	}
}

// TestDriverVerifyConnectivity checks that VerifyConnectivity returns nil.
func TestDriverVerifyConnectivity(t *testing.T) {
	ctx := context.Background()
	d, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(ctx)
	if err := d.VerifyConnectivity(ctx); err != nil {
		t.Fatalf("VerifyConnectivity: unexpected error: %v", err)
	}
}

// TestDriverVerifyAuthentication checks that VerifyAuthentication returns nil.
func TestDriverVerifyAuthentication(t *testing.T) {
	ctx := context.Background()
	d, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(ctx)
	auth := neo4j.BasicAuth("user", "pass", "")
	if err := d.VerifyAuthentication(ctx, &auth); err != nil {
		t.Fatalf("VerifyAuthentication: unexpected error: %v", err)
	}
}

// TestDriverGetServerInfo checks that GetServerInfo returns a valid ServerInfo.
func TestDriverGetServerInfo(t *testing.T) {
	ctx := context.Background()
	d, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(ctx)
	info, err := d.GetServerInfo(ctx)
	if err != nil {
		t.Fatalf("GetServerInfo: unexpected error: %v", err)
	}
	if info == nil {
		t.Fatal("GetServerInfo returned nil ServerInfo")
	}
}

// TestDriverIsEncrypted checks that IsEncrypted returns false.
func TestDriverIsEncrypted(t *testing.T) {
	d, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(context.Background())
	if d.IsEncrypted() {
		t.Fatal("IsEncrypted should return false for graphlite")
	}
}

// TestDriverExecuteQueryBookmarkManager checks that ExecuteQueryBookmarkManager
// returns a non-nil BookmarkManager.
func TestDriverExecuteQueryBookmarkManager(t *testing.T) {
	d, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(context.Background())
	bkMgr := d.ExecuteQueryBookmarkManager()
	if bkMgr == nil {
		t.Fatal("ExecuteQueryBookmarkManager returned nil")
	}
}

// TestSessionRun checks that session.Run executes a Cypher query and returns
// a valid neo4j.Result.
func TestSessionRun(t *testing.T) {
	ctx := context.Background()
	d, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(ctx)

	sess := d.NewSession(ctx, neo4j.SessionConfig{})
	defer sess.Close(ctx)

	// CREATE a node.
	res, err := sess.Run(ctx, "CREATE (n:Person {name: 'Alice'})", nil)
	if err != nil {
		t.Fatalf("session.Run CREATE: %v", err)
	}
	sum, err := res.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if sum == nil {
		t.Fatal("Consume returned nil summary")
	}

	// MATCH and verify.
	res2, err := sess.Run(ctx, "MATCH (n:Person) RETURN n.name AS name", nil)
	if err != nil {
		t.Fatalf("session.Run MATCH: %v", err)
	}
	recs, err := res2.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	name, ok := recs[0].Get("name")
	if !ok {
		t.Fatal("record missing 'name' key")
	}
	if name != "Alice" {
		t.Fatalf("expected 'Alice', got %v", name)
	}
}

// TestExplicitTransaction checks that BeginTransaction returns a valid
// neo4j.ExplicitTransaction that can Commit and Rollback.
func TestExplicitTransaction(t *testing.T) {
	ctx := context.Background()
	d, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(ctx)

	sess := d.NewSession(ctx, neo4j.SessionConfig{})
	defer sess.Close(ctx)

	tx, err := sess.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}

	_, err = tx.Run(ctx, "CREATE (n:Robot {id: 1})", nil)
	if err != nil {
		t.Fatalf("tx.Run: %v", err)
	}

	// Rollback — node should not persist.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("tx.Rollback: %v", err)
	}

	// Verify node is not in DB.
	res, err := sess.Run(ctx, "MATCH (n:Robot) RETURN n", nil)
	if err != nil {
		t.Fatalf("post-rollback MATCH: %v", err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		t.Fatalf("post-rollback Collect: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected 0 records after rollback, got %d", len(recs))
	}
}

// TestManagedTransaction checks that ExecuteWrite runs work in a committed tx.
func TestManagedTransaction(t *testing.T) {
	ctx := context.Background()
	d, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(ctx)

	sess := d.NewSession(ctx, neo4j.SessionConfig{})
	defer sess.Close(ctx)

	_, err = sess.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(ctx, "CREATE (n:Bot {id: 42})", nil)
		return nil, err
	})
	if err != nil {
		t.Fatalf("ExecuteWrite: %v", err)
	}

	// Verify node is in DB.
	res, err := sess.Run(ctx, "MATCH (n:Bot) RETURN n.id AS id", nil)
	if err != nil {
		t.Fatalf("post-write MATCH: %v", err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		t.Fatalf("post-write Collect: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
}
