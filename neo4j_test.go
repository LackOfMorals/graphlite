package graphlite_test

import (
	"context"
	"testing"

	graphlite "github.com/LackOfMorals/graphlite"
)

// TestDBImplementsDriver verifies the compile-time assertion in interfaces.go.
// If *DB drifts from the Driver interface this test will fail to compile.
func TestDBImplementsDriver(t *testing.T) {
	var _ graphlite.Driver = (*graphlite.DB)(nil)
}

// TestNewDriverMemory checks that NewDriver(":memory:", NoAuth()) returns a
// value assignable to graphlite.Driver without error.
func TestNewDriverMemory(t *testing.T) {
	d, err := graphlite.NewDriver(":memory:", graphlite.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver(:memory:, NoAuth): unexpected error: %v", err)
	}
	defer d.Close(context.Background())

	var _ graphlite.Driver = d
}

// TestNewDriverNoAuth checks that NoAuth() is accepted without error.
func TestNewDriverNoAuth(t *testing.T) {
	d, err := graphlite.NewDriver(":memory:", graphlite.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver with NoAuth: unexpected error: %v", err)
	}
	defer d.Close(context.Background())
}

// TestNewDriverZeroAuth checks that a zero-value AuthToken is accepted.
func TestNewDriverZeroAuth(t *testing.T) {
	d, err := graphlite.NewDriver(":memory:", graphlite.AuthToken{})
	if err != nil {
		t.Fatalf("NewDriver with zero AuthToken: unexpected error: %v", err)
	}
	defer d.Close(context.Background())
}

// TestNewSessionAccepted checks that NewSession returns a non-nil Session.
func TestNewSessionAccepted(t *testing.T) {
	ctx := context.Background()
	d, err := graphlite.NewDriver(":memory:", graphlite.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(ctx)

	sess := d.NewSession(ctx)
	if sess == nil {
		t.Fatal("NewSession returned nil")
	}
	defer sess.Close(ctx)
}

// TestDriverCloseReleasesResources checks that Close releases resources cleanly.
func TestDriverCloseReleasesResources(t *testing.T) {
	ctx := context.Background()
	d, err := graphlite.NewDriver(":memory:", graphlite.NoAuth())
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
	d, err := graphlite.NewDriver(":memory:", graphlite.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(ctx)
	if err := d.VerifyConnectivity(ctx); err != nil {
		t.Fatalf("VerifyConnectivity: unexpected error: %v", err)
	}
}

// TestSessionRun checks that session.Run executes Cypher and returns a Result.
func TestSessionRun(t *testing.T) {
	ctx := context.Background()
	d, err := graphlite.NewDriver(":memory:", graphlite.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(ctx)

	sess := d.NewSession(ctx)
	defer sess.Close(ctx)

	res, err := sess.Run(ctx, "CREATE (n:Person {name: 'Alice'})", nil)
	if err != nil {
		t.Fatalf("session.Run CREATE: %v", err)
	}
	if _, err := res.Consume(ctx); err != nil {
		t.Fatalf("Consume: %v", err)
	}

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

// TestExplicitTransaction checks that BeginTransaction returns a Transaction
// that correctly Commits and Rollbacks.
func TestExplicitTransaction(t *testing.T) {
	ctx := context.Background()
	d, err := graphlite.NewDriver(":memory:", graphlite.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(ctx)

	sess := d.NewSession(ctx)
	defer sess.Close(ctx)

	tx, err := sess.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}

	if _, err = tx.Run(ctx, "CREATE (n:Robot {id: 1})", nil); err != nil {
		t.Fatalf("tx.Run: %v", err)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("tx.Rollback: %v", err)
	}

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
	d, err := graphlite.NewDriver(":memory:", graphlite.NoAuth())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close(ctx)

	sess := d.NewSession(ctx)
	defer sess.Close(ctx)

	_, err = sess.ExecuteWrite(ctx, func(tx graphlite.ManagedTransaction) (any, error) {
		_, err := tx.Run(ctx, "CREATE (n:Bot {id: 42})", nil)
		return nil, err
	})
	if err != nil {
		t.Fatalf("ExecuteWrite: %v", err)
	}

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
