// Package store_test contains comprehensive unit tests for the store CRUD
// operations and transaction primitives required by task-004.
package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/LackOfMorals/graphlite/store"
)

// helper opens an in-memory store and registers cleanup.
func openMemory(t *testing.T) *store.SQLiteStore {
	t.Helper()
	s, err := store.Open(":memory:", store.Config{})
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ============================================================
// Node CRUD
// ============================================================

// TestInsertNode verifies that InsertNode returns a positive, unique ID.
func TestInsertNode(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	id1, err := s.InsertNode(ctx, "Person", `{"name":"Alice","age":30}`)
	if err != nil {
		t.Fatalf("InsertNode: %v", err)
	}
	if id1 <= 0 {
		t.Fatalf("expected positive ID, got %d", id1)
	}

	id2, err := s.InsertNode(ctx, "Person", `{"name":"Bob","age":25}`)
	if err != nil {
		t.Fatalf("InsertNode second: %v", err)
	}
	if id2 <= 0 {
		t.Fatalf("expected positive ID, got %d", id2)
	}
	if id2 == id1 {
		t.Fatalf("expected unique IDs, both are %d", id1)
	}
}

// TestInsertNodeStableID verifies that a retrieved node has the same ID that
// was returned at insert time (stable ElementId contract).
func TestInsertNodeStableID(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	insertedID, err := s.InsertNode(ctx, "City", `{"name":"London"}`)
	if err != nil {
		t.Fatalf("InsertNode: %v", err)
	}

	n, err := s.GetNode(ctx, insertedID)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if n.ID != insertedID {
		t.Errorf("stable ID: want %d, got %d", insertedID, n.ID)
	}
}

// TestGetNode verifies that GetNode returns the correct labels and props.
func TestGetNode(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	id, err := s.InsertNode(ctx, "Animal,Pet", `{"name":"Fido","legs":4}`)
	if err != nil {
		t.Fatalf("InsertNode: %v", err)
	}

	n, err := s.GetNode(ctx, id)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if n.Labels != "Animal,Pet" {
		t.Errorf("Labels: want %q, got %q", "Animal,Pet", n.Labels)
	}
	if !strings.Contains(n.Props, "Fido") {
		t.Errorf("Props should contain 'Fido', got %q", n.Props)
	}
}

// TestGetNodeNotFound verifies that GetNode returns sql.ErrNoRows for a
// non-existent ID.
func TestGetNodeNotFound(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	_, err := s.GetNode(ctx, 9999999)
	if err == nil {
		t.Fatal("expected error for non-existent node, got nil")
	}
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}

// TestDeleteNode verifies that a deleted node is no longer retrievable.
func TestDeleteNode(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	id, err := s.InsertNode(ctx, "Temp", `{}`)
	if err != nil {
		t.Fatalf("InsertNode: %v", err)
	}

	if err := s.DeleteNode(ctx, id); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}

	_, err = s.GetNode(ctx, id)
	if err == nil {
		t.Fatal("expected error after delete, got nil")
	}
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows after delete, got %v", err)
	}
}

// TestListNodes verifies that ListNodes returns all inserted nodes.
func TestListNodes(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	labels := []string{"A", "B", "C"}
	insertedIDs := make(map[int64]bool)
	for _, lbl := range labels {
		id, err := s.InsertNode(ctx, lbl, `{}`)
		if err != nil {
			t.Fatalf("InsertNode(%q): %v", lbl, err)
		}
		insertedIDs[id] = true
	}

	nodes, err := s.ListNodes(ctx)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != len(labels) {
		t.Fatalf("expected %d nodes, got %d", len(labels), len(nodes))
	}
	for _, n := range nodes {
		if !insertedIDs[n.ID] {
			t.Errorf("unexpected node ID %d in ListNodes result", n.ID)
		}
	}
}

// TestListNodesEmpty verifies that ListNodes returns an empty slice (not nil
// error) when the store is empty.
func TestListNodesEmpty(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	nodes, err := s.ListNodes(ctx)
	if err != nil {
		t.Fatalf("ListNodes on empty store: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("expected 0 nodes, got %d", len(nodes))
	}
}

// TestListNodesByLabel verifies that ListNodesByLabel correctly filters by label,
// using the idx_nodes_labels index (exact, prefix, suffix, and middle positions).
func TestListNodesByLabel(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	// Insert nodes with various label combinations.
	ids := map[string]int64{}
	for lbl, props := range map[string]string{
		"Person":          `{"name":"Alice"}`,
		"Person,Employee": `{"name":"Bob"}`,
		"Employee,Person": `{"name":"Carol"}`,
		"Employee":        `{"name":"Dave"}`,
		"Admin,Person,Manager": `{"name":"Eve"}`,
	} {
		id, err := s.InsertNode(ctx, lbl, props)
		if err != nil {
			t.Fatalf("InsertNode(%q): %v", lbl, err)
		}
		ids[lbl] = id
	}

	// All nodes with label "Person" should be found.
	results, err := s.ListNodesByLabel(ctx, "Person")
	if err != nil {
		t.Fatalf("ListNodesByLabel: %v", err)
	}
	if len(results) != 4 {
		t.Errorf("expected 4 nodes with label Person, got %d", len(results))
	}

	// Only nodes with label "Employee" (not just any that contain "Employee").
	employeeResults, err := s.ListNodesByLabel(ctx, "Employee")
	if err != nil {
		t.Fatalf("ListNodesByLabel(Employee): %v", err)
	}
	// All 3 should be found: "Person,Employee", "Employee,Person", "Employee"
	if len(employeeResults) != 3 {
		t.Errorf("expected 3 nodes with label Employee, got %d", len(employeeResults))
	}

	// "Admin" should match only one node.
	adminResults, err := s.ListNodesByLabel(ctx, "Admin")
	if err != nil {
		t.Fatalf("ListNodesByLabel(Admin): %v", err)
	}
	if len(adminResults) != 1 {
		t.Errorf("expected 1 node with label Admin, got %d", len(adminResults))
	}
}

// TestListNodesByLabelIndexHint verifies that idx_nodes_labels is used for
// label lookup via EXPLAIN QUERY PLAN.
func TestListNodesByLabelIndexHint(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	// Insert a few nodes so the index is non-trivially used.
	for range 5 {
		if _, err := s.InsertNode(ctx, "Person", `{}`); err != nil {
			t.Fatalf("InsertNode: %v", err)
		}
	}

	// EXPLAIN QUERY PLAN for the first OR branch (exact match), which uses the index.
	rows, err := s.DB().QueryContext(ctx,
		`EXPLAIN QUERY PLAN SELECT id, labels, props FROM nodes WHERE labels = ?`,
		"Person",
	)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("EXPLAIN scan: %v", err)
		}
		if strings.Contains(detail, "idx_nodes_labels") {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	if !found {
		t.Error("expected EXPLAIN QUERY PLAN to use idx_nodes_labels, but it did not")
	}
}

// TestUpdateNodeProps verifies that UpdateNodeProps replaces the props JSON.
func TestUpdateNodeProps(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	id, err := s.InsertNode(ctx, "Widget", `{"color":"red"}`)
	if err != nil {
		t.Fatalf("InsertNode: %v", err)
	}

	if err := s.UpdateNodeProps(ctx, id, `{"color":"blue","size":42}`); err != nil {
		t.Fatalf("UpdateNodeProps: %v", err)
	}

	n, err := s.GetNode(ctx, id)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if strings.Contains(n.Props, "red") {
		t.Errorf("old prop value 'red' should not be present after update, got %q", n.Props)
	}
	if !strings.Contains(n.Props, "blue") {
		t.Errorf("new prop value 'blue' should be present, got %q", n.Props)
	}
}

// ============================================================
// Edge CRUD
// ============================================================

// insertTestNodes is a helper that creates two nodes for edge tests.
func insertTestNodes(t *testing.T, s *store.SQLiteStore) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	id1, err := s.InsertNode(ctx, "Node", `{"n":1}`)
	if err != nil {
		t.Fatalf("InsertNode 1: %v", err)
	}
	id2, err := s.InsertNode(ctx, "Node", `{"n":2}`)
	if err != nil {
		t.Fatalf("InsertNode 2: %v", err)
	}
	return id1, id2
}

// TestInsertEdge verifies that InsertEdge returns a positive, unique ID.
func TestInsertEdge(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()
	n1, n2 := insertTestNodes(t, s)

	id1, err := s.InsertEdge(ctx, "KNOWS", n1, n2, `{"since":2020}`)
	if err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}
	if id1 <= 0 {
		t.Fatalf("expected positive edge ID, got %d", id1)
	}

	id2, err := s.InsertEdge(ctx, "LIKES", n1, n2, `{}`)
	if err != nil {
		t.Fatalf("InsertEdge second: %v", err)
	}
	if id2 == id1 {
		t.Fatalf("expected unique edge IDs, both are %d", id1)
	}
}

// TestInsertEdgeStableID verifies the stable ElementId contract for edges.
func TestInsertEdgeStableID(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()
	n1, n2 := insertTestNodes(t, s)

	insertedID, err := s.InsertEdge(ctx, "FRIENDS", n1, n2, `{}`)
	if err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}

	e, err := s.GetEdge(ctx, insertedID)
	if err != nil {
		t.Fatalf("GetEdge: %v", err)
	}
	if e.ID != insertedID {
		t.Errorf("stable ID: want %d, got %d", insertedID, e.ID)
	}
}

// TestGetEdge verifies that GetEdge returns correct type, start/end IDs, and props.
func TestGetEdge(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()
	n1, n2 := insertTestNodes(t, s)

	id, err := s.InsertEdge(ctx, "WORKS_AT", n1, n2, `{"role":"engineer"}`)
	if err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}

	e, err := s.GetEdge(ctx, id)
	if err != nil {
		t.Fatalf("GetEdge: %v", err)
	}
	if e.Type != "WORKS_AT" {
		t.Errorf("Type: want %q, got %q", "WORKS_AT", e.Type)
	}
	if e.StartID != n1 {
		t.Errorf("StartID: want %d, got %d", n1, e.StartID)
	}
	if e.EndID != n2 {
		t.Errorf("EndID: want %d, got %d", n2, e.EndID)
	}
	if !strings.Contains(e.Props, "engineer") {
		t.Errorf("Props should contain 'engineer', got %q", e.Props)
	}
}

// TestGetEdgeNotFound verifies that GetEdge returns sql.ErrNoRows for a
// non-existent ID.
func TestGetEdgeNotFound(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	_, err := s.GetEdge(ctx, 9999999)
	if err == nil {
		t.Fatal("expected error for non-existent edge, got nil")
	}
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}

// TestDeleteEdge verifies that a deleted edge is no longer retrievable.
func TestDeleteEdge(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()
	n1, n2 := insertTestNodes(t, s)

	id, err := s.InsertEdge(ctx, "TEMP", n1, n2, `{}`)
	if err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}

	if err := s.DeleteEdge(ctx, id); err != nil {
		t.Fatalf("DeleteEdge: %v", err)
	}

	_, err = s.GetEdge(ctx, id)
	if err == nil {
		t.Fatal("expected error after delete, got nil")
	}
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows after delete, got %v", err)
	}
}

// TestListEdges verifies that ListEdges returns all inserted edges.
func TestListEdges(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()
	n1, n2 := insertTestNodes(t, s)

	types := []string{"KNOWS", "LIKES", "FOLLOWS"}
	insertedIDs := make(map[int64]bool)
	for _, typ := range types {
		id, err := s.InsertEdge(ctx, typ, n1, n2, `{}`)
		if err != nil {
			t.Fatalf("InsertEdge(%q): %v", typ, err)
		}
		insertedIDs[id] = true
	}

	edges, err := s.ListEdges(ctx)
	if err != nil {
		t.Fatalf("ListEdges: %v", err)
	}
	if len(edges) != len(types) {
		t.Fatalf("expected %d edges, got %d", len(types), len(edges))
	}
	for _, e := range edges {
		if !insertedIDs[e.ID] {
			t.Errorf("unexpected edge ID %d in ListEdges result", e.ID)
		}
	}
}

// TestListEdgesEmpty verifies that ListEdges returns an empty slice on an
// empty store.
func TestListEdgesEmpty(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	edges, err := s.ListEdges(ctx)
	if err != nil {
		t.Fatalf("ListEdges on empty store: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges, got %d", len(edges))
	}
}

// TestListEdgesByType verifies that ListEdgesByType filters correctly.
func TestListEdgesByType(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()
	n1, n2 := insertTestNodes(t, s)

	// Insert 2 KNOWS and 1 LIKES edge.
	for range 2 {
		if _, err := s.InsertEdge(ctx, "KNOWS", n1, n2, `{}`); err != nil {
			t.Fatalf("InsertEdge KNOWS: %v", err)
		}
	}
	if _, err := s.InsertEdge(ctx, "LIKES", n1, n2, `{}`); err != nil {
		t.Fatalf("InsertEdge LIKES: %v", err)
	}

	knows, err := s.ListEdgesByType(ctx, "KNOWS")
	if err != nil {
		t.Fatalf("ListEdgesByType(KNOWS): %v", err)
	}
	if len(knows) != 2 {
		t.Errorf("expected 2 KNOWS edges, got %d", len(knows))
	}

	likes, err := s.ListEdgesByType(ctx, "LIKES")
	if err != nil {
		t.Fatalf("ListEdgesByType(LIKES): %v", err)
	}
	if len(likes) != 1 {
		t.Errorf("expected 1 LIKES edge, got %d", len(likes))
	}
}

// TestListEdgesByStartNode verifies that edges are filterable by start node.
func TestListEdgesByStartNode(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()
	n1, n2 := insertTestNodes(t, s)
	n3, err := s.InsertNode(ctx, "Node", `{"n":3}`)
	if err != nil {
		t.Fatalf("InsertNode 3: %v", err)
	}

	// n1 -> n2, n1 -> n3, n2 -> n3
	if _, err := s.InsertEdge(ctx, "A", n1, n2, `{}`); err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}
	if _, err := s.InsertEdge(ctx, "B", n1, n3, `{}`); err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}
	if _, err := s.InsertEdge(ctx, "C", n2, n3, `{}`); err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}

	fromN1, err := s.ListEdgesByStartNode(ctx, n1)
	if err != nil {
		t.Fatalf("ListEdgesByStartNode: %v", err)
	}
	if len(fromN1) != 2 {
		t.Errorf("expected 2 edges from n1, got %d", len(fromN1))
	}

	fromN2, err := s.ListEdgesByStartNode(ctx, n2)
	if err != nil {
		t.Fatalf("ListEdgesByStartNode: %v", err)
	}
	if len(fromN2) != 1 {
		t.Errorf("expected 1 edge from n2, got %d", len(fromN2))
	}
}

// TestListEdgesByEndNode verifies that edges are filterable by end node.
func TestListEdgesByEndNode(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()
	n1, n2 := insertTestNodes(t, s)
	n3, err := s.InsertNode(ctx, "Node", `{"n":3}`)
	if err != nil {
		t.Fatalf("InsertNode 3: %v", err)
	}

	// n1 -> n3, n2 -> n3
	if _, err := s.InsertEdge(ctx, "X", n1, n3, `{}`); err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}
	if _, err := s.InsertEdge(ctx, "Y", n2, n3, `{}`); err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}

	toN3, err := s.ListEdgesByEndNode(ctx, n3)
	if err != nil {
		t.Fatalf("ListEdgesByEndNode: %v", err)
	}
	if len(toN3) != 2 {
		t.Errorf("expected 2 edges to n3, got %d", len(toN3))
	}
}

// TestEdgeExistsForNode verifies that EdgeExistsForNode detects both start_id
// and end_id references.
func TestEdgeExistsForNode(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()
	n1, n2 := insertTestNodes(t, s)

	// Before any edges, neither node has edges.
	for _, id := range []int64{n1, n2} {
		exists, err := s.EdgeExistsForNode(ctx, id)
		if err != nil {
			t.Fatalf("EdgeExistsForNode(%d): %v", id, err)
		}
		if exists {
			t.Errorf("expected no edges for node %d, but found some", id)
		}
	}

	// Insert edge: n1 -> n2.
	if _, err := s.InsertEdge(ctx, "REL", n1, n2, `{}`); err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}

	// Now both n1 (start) and n2 (end) should report edge existence.
	for _, id := range []int64{n1, n2} {
		exists, err := s.EdgeExistsForNode(ctx, id)
		if err != nil {
			t.Fatalf("EdgeExistsForNode(%d): %v", id, err)
		}
		if !exists {
			t.Errorf("expected edge for node %d, found none", id)
		}
	}
}

// TestUpdateEdgeProps verifies that UpdateEdgeProps replaces the props JSON.
func TestUpdateEdgeProps(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()
	n1, n2 := insertTestNodes(t, s)

	id, err := s.InsertEdge(ctx, "REL", n1, n2, `{"weight":1}`)
	if err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}

	if err := s.UpdateEdgeProps(ctx, id, `{"weight":99,"note":"updated"}`); err != nil {
		t.Fatalf("UpdateEdgeProps: %v", err)
	}

	e, err := s.GetEdge(ctx, id)
	if err != nil {
		t.Fatalf("GetEdge: %v", err)
	}
	if strings.Contains(e.Props, `"weight":1`) && !strings.Contains(e.Props, `"weight":99`) {
		t.Errorf("old prop value not replaced, got %q", e.Props)
	}
	if !strings.Contains(e.Props, "updated") {
		t.Errorf("new prop value 'updated' not found, got %q", e.Props)
	}
}

// ============================================================
// Transaction tests
// ============================================================

// TestTxNodeRollback verifies that a rolled-back transaction does not persist
// node inserts.
func TestTxNodeRollback(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	id, err := tx.InsertNode(ctx, "RollMe", `{}`)
	if err != nil {
		t.Fatalf("tx.InsertNode: %v", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Node must not exist outside the rolled-back transaction.
	_, err = s.GetNode(ctx, id)
	if err == nil {
		t.Fatal("expected error after rollback, got nil")
	}
}

// TestTxEdgeRollback verifies that a rolled-back transaction does not persist
// edge inserts.
func TestTxEdgeRollback(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	// Create nodes outside any transaction so they persist.
	n1, n2 := insertTestNodes(t, s)

	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	edgeID, err := tx.InsertEdge(ctx, "EPHEMERAL", n1, n2, `{}`)
	if err != nil {
		t.Fatalf("tx.InsertEdge: %v", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Edge must not exist outside the rolled-back transaction.
	_, err = s.GetEdge(ctx, edgeID)
	if err == nil {
		t.Fatal("expected error after edge rollback, got nil")
	}
}

// TestTxAtomicCommit verifies that a committed transaction persists both node
// and edge inserts together.
func TestTxAtomicCommit(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	n1, err := tx.InsertNode(ctx, "TxA", `{}`)
	if err != nil {
		t.Fatalf("tx.InsertNode A: %v", err)
	}
	n2, err := tx.InsertNode(ctx, "TxB", `{}`)
	if err != nil {
		t.Fatalf("tx.InsertNode B: %v", err)
	}
	edgeID, err := tx.InsertEdge(ctx, "TX_REL", n1, n2, `{"key":"val"}`)
	if err != nil {
		t.Fatalf("tx.InsertEdge: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// All three rows must be visible after commit.
	if _, err := s.GetNode(ctx, n1); err != nil {
		t.Errorf("GetNode n1 after commit: %v", err)
	}
	if _, err := s.GetNode(ctx, n2); err != nil {
		t.Errorf("GetNode n2 after commit: %v", err)
	}
	if _, err := s.GetEdge(ctx, edgeID); err != nil {
		t.Errorf("GetEdge after commit: %v", err)
	}
}

// TestTxMultipleMutationsRollback verifies that rolling back a transaction that
// has performed multiple insert/delete mutations reverts ALL of them.
func TestTxMultipleMutationsRollback(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	// Insert a node that will be deleted inside the rolled-back tx.
	persistedID, err := s.InsertNode(ctx, "Persist", `{}`)
	if err != nil {
		t.Fatalf("InsertNode: %v", err)
	}

	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// Insert several new nodes inside the tx.
	var newIDs []int64
	for range 3 {
		id, err := tx.InsertNode(ctx, "Tx", `{}`)
		if err != nil {
			t.Fatalf("tx.InsertNode: %v", err)
		}
		newIDs = append(newIDs, id)
	}

	// Delete the pre-existing node inside the tx.
	if err := tx.DeleteNode(ctx, persistedID); err != nil {
		t.Fatalf("tx.DeleteNode: %v", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// New nodes must NOT exist.
	for _, id := range newIDs {
		if _, err := s.GetNode(ctx, id); err == nil {
			t.Errorf("expected GetNode(%d) to fail after rollback, got nil", id)
		}
	}

	// Pre-existing node MUST still exist (delete was rolled back).
	if _, err := s.GetNode(ctx, persistedID); err != nil {
		t.Errorf("GetNode(%d) after rollback of delete should succeed: %v", persistedID, err)
	}
}

// TestNestedTransactionError verifies that Begin on a Tx returns an error.
func TestNestedTransactionError(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	_, err = tx.Begin(ctx)
	if err == nil {
		t.Fatal("expected error from nested Begin, got nil")
	}
}

// ============================================================
// Transaction delegate method coverage (task-021)
// The sqliteTx type delegates each operation to the shared helpers via the
// embedded *sql.Tx executor. These tests ensure the delegate methods are
// exercised (they appear as 0% uncovered in the coverage report even though
// the helpers themselves are tested above via the direct store path).
// ============================================================

// openTx opens an in-memory store and begins a transaction.
// The transaction is automatically rolled back when the test ends unless the
// caller commits it.
func openTx(t *testing.T) (*store.SQLiteStore, store.Tx) {
	t.Helper()
	s := openMemory(t)
	tx, err := s.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return s, tx
}

// TestTxDB verifies that DB() on a Tx returns the same *sql.DB as the store.
func TestTxDB(t *testing.T) {
	s, tx := openTx(t)
	if tx.DB() != s.DB() {
		t.Error("tx.DB() should return the same *sql.DB as the parent store")
	}
}

// TestTxGetNode verifies that GetNode works within a transaction.
func TestTxGetNode(t *testing.T) {
	_, tx := openTx(t)
	ctx := context.Background()

	id, err := tx.InsertNode(ctx, "TxGet", `{"x":1}`)
	if err != nil {
		t.Fatalf("tx.InsertNode: %v", err)
	}
	n, err := tx.GetNode(ctx, id)
	if err != nil {
		t.Fatalf("tx.GetNode: %v", err)
	}
	if n.ID != id {
		t.Errorf("tx.GetNode: expected ID %d, got %d", id, n.ID)
	}
	if n.Labels != "TxGet" {
		t.Errorf("tx.GetNode: expected labels %q, got %q", "TxGet", n.Labels)
	}
}

// TestTxListNodes verifies that ListNodes works within a transaction.
func TestTxListNodes(t *testing.T) {
	_, tx := openTx(t)
	ctx := context.Background()

	for range 3 {
		if _, err := tx.InsertNode(ctx, "TxList", `{}`); err != nil {
			t.Fatalf("tx.InsertNode: %v", err)
		}
	}
	nodes, err := tx.ListNodes(ctx)
	if err != nil {
		t.Fatalf("tx.ListNodes: %v", err)
	}
	if len(nodes) != 3 {
		t.Errorf("tx.ListNodes: expected 3 nodes, got %d", len(nodes))
	}
}

// TestTxListNodesByLabel verifies that ListNodesByLabel works within a transaction.
func TestTxListNodesByLabel(t *testing.T) {
	_, tx := openTx(t)
	ctx := context.Background()

	if _, err := tx.InsertNode(ctx, "Alpha", `{}`); err != nil {
		t.Fatalf("tx.InsertNode Alpha: %v", err)
	}
	if _, err := tx.InsertNode(ctx, "Beta", `{}`); err != nil {
		t.Fatalf("tx.InsertNode Beta: %v", err)
	}

	alphas, err := tx.ListNodesByLabel(ctx, "Alpha")
	if err != nil {
		t.Fatalf("tx.ListNodesByLabel(Alpha): %v", err)
	}
	if len(alphas) != 1 {
		t.Errorf("tx.ListNodesByLabel: expected 1 Alpha node, got %d", len(alphas))
	}
}

// TestTxUpdateNodeProps verifies that UpdateNodeProps works within a transaction.
func TestTxUpdateNodeProps(t *testing.T) {
	_, tx := openTx(t)
	ctx := context.Background()

	id, err := tx.InsertNode(ctx, "Upd", `{"v":1}`)
	if err != nil {
		t.Fatalf("tx.InsertNode: %v", err)
	}
	if err := tx.UpdateNodeProps(ctx, id, `{"v":99}`); err != nil {
		t.Fatalf("tx.UpdateNodeProps: %v", err)
	}
	n, err := tx.GetNode(ctx, id)
	if err != nil {
		t.Fatalf("tx.GetNode after update: %v", err)
	}
	if !strings.Contains(n.Props, "99") {
		t.Errorf("tx.UpdateNodeProps: expected updated props to contain '99', got %q", n.Props)
	}
}

// insertTxNodes inserts two nodes within tx and returns their IDs.
// All work stays on the transaction's connection to avoid the deadlock caused
// by calling s.InsertNode() (which needs the shared single connection) while
// tx already holds it.
func insertTxNodes(t *testing.T, tx store.Tx) (n1, n2 int64) {
	t.Helper()
	ctx := context.Background()
	var err error
	n1, err = tx.InsertNode(ctx, "TxNodeA", `{}`)
	if err != nil {
		t.Fatalf("insertTxNodes: InsertNode A: %v", err)
	}
	n2, err = tx.InsertNode(ctx, "TxNodeB", `{}`)
	if err != nil {
		t.Fatalf("insertTxNodes: InsertNode B: %v", err)
	}
	return n1, n2
}

// TestTxGetEdge verifies that GetEdge works within a transaction.
func TestTxGetEdge(t *testing.T) {
	_, tx := openTx(t)
	ctx := context.Background()

	n1, n2 := insertTxNodes(t, tx)

	edgeID, err := tx.InsertEdge(ctx, "REL", n1, n2, `{"w":1}`)
	if err != nil {
		t.Fatalf("tx.InsertEdge: %v", err)
	}
	e, err := tx.GetEdge(ctx, edgeID)
	if err != nil {
		t.Fatalf("tx.GetEdge: %v", err)
	}
	if e.ID != edgeID {
		t.Errorf("tx.GetEdge: expected ID %d, got %d", edgeID, e.ID)
	}
	if e.Type != "REL" {
		t.Errorf("tx.GetEdge: expected type %q, got %q", "REL", e.Type)
	}
}

// TestTxDeleteEdge verifies that DeleteEdge works within a transaction.
func TestTxDeleteEdge(t *testing.T) {
	_, tx := openTx(t)
	ctx := context.Background()

	n1, n2 := insertTxNodes(t, tx)

	edgeID, err := tx.InsertEdge(ctx, "DEL_REL", n1, n2, `{}`)
	if err != nil {
		t.Fatalf("tx.InsertEdge: %v", err)
	}
	if err := tx.DeleteEdge(ctx, edgeID); err != nil {
		t.Fatalf("tx.DeleteEdge: %v", err)
	}
	_, err = tx.GetEdge(ctx, edgeID)
	if err == nil {
		t.Fatal("expected error after tx.DeleteEdge, got nil")
	}
}

// TestTxListEdges verifies that ListEdges works within a transaction.
func TestTxListEdges(t *testing.T) {
	_, tx := openTx(t)
	ctx := context.Background()

	n1, n2 := insertTxNodes(t, tx)

	for range 2 {
		if _, err := tx.InsertEdge(ctx, "TX_E", n1, n2, `{}`); err != nil {
			t.Fatalf("tx.InsertEdge: %v", err)
		}
	}
	edges, err := tx.ListEdges(ctx)
	if err != nil {
		t.Fatalf("tx.ListEdges: %v", err)
	}
	if len(edges) != 2 {
		t.Errorf("tx.ListEdges: expected 2 edges, got %d", len(edges))
	}
}

// TestTxListEdgesByType verifies that ListEdgesByType works within a transaction.
func TestTxListEdgesByType(t *testing.T) {
	_, tx := openTx(t)
	ctx := context.Background()

	n1, n2 := insertTxNodes(t, tx)

	if _, err := tx.InsertEdge(ctx, "TYPE_A", n1, n2, `{}`); err != nil {
		t.Fatalf("tx.InsertEdge TYPE_A: %v", err)
	}
	if _, err := tx.InsertEdge(ctx, "TYPE_B", n1, n2, `{}`); err != nil {
		t.Fatalf("tx.InsertEdge TYPE_B: %v", err)
	}

	results, err := tx.ListEdgesByType(ctx, "TYPE_A")
	if err != nil {
		t.Fatalf("tx.ListEdgesByType(TYPE_A): %v", err)
	}
	if len(results) != 1 {
		t.Errorf("tx.ListEdgesByType: expected 1 TYPE_A edge, got %d", len(results))
	}
}

// TestTxListEdgesByStartNode verifies that ListEdgesByStartNode works within a transaction.
func TestTxListEdgesByStartNode(t *testing.T) {
	_, tx := openTx(t)
	ctx := context.Background()

	n1, n2 := insertTxNodes(t, tx)

	if _, err := tx.InsertEdge(ctx, "FROM_N1", n1, n2, `{}`); err != nil {
		t.Fatalf("tx.InsertEdge: %v", err)
	}
	results, err := tx.ListEdgesByStartNode(ctx, n1)
	if err != nil {
		t.Fatalf("tx.ListEdgesByStartNode: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("tx.ListEdgesByStartNode: expected 1 edge from n1, got %d", len(results))
	}
	if results[0].StartID != n1 {
		t.Errorf("tx.ListEdgesByStartNode: expected StartID=%d, got %d", n1, results[0].StartID)
	}
}

// TestTxListEdgesByEndNode verifies that ListEdgesByEndNode works within a transaction.
func TestTxListEdgesByEndNode(t *testing.T) {
	_, tx := openTx(t)
	ctx := context.Background()

	n1, n2 := insertTxNodes(t, tx)

	if _, err := tx.InsertEdge(ctx, "TO_N2", n1, n2, `{}`); err != nil {
		t.Fatalf("tx.InsertEdge: %v", err)
	}
	results, err := tx.ListEdgesByEndNode(ctx, n2)
	if err != nil {
		t.Fatalf("tx.ListEdgesByEndNode: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("tx.ListEdgesByEndNode: expected 1 edge to n2, got %d", len(results))
	}
	if results[0].EndID != n2 {
		t.Errorf("tx.ListEdgesByEndNode: expected EndID=%d, got %d", n2, results[0].EndID)
	}
}

// TestTxEdgeExistsForNode verifies that EdgeExistsForNode works within a transaction.
func TestTxEdgeExistsForNode(t *testing.T) {
	_, tx := openTx(t)
	ctx := context.Background()

	n1, n2 := insertTxNodes(t, tx)

	// Before inserting any edge, EdgeExistsForNode should be false.
	exists, err := tx.EdgeExistsForNode(ctx, n1)
	if err != nil {
		t.Fatalf("tx.EdgeExistsForNode before insert: %v", err)
	}
	if exists {
		t.Errorf("tx.EdgeExistsForNode: expected false before any edge, got true")
	}

	if _, err := tx.InsertEdge(ctx, "EDGE", n1, n2, `{}`); err != nil {
		t.Fatalf("tx.InsertEdge: %v", err)
	}

	exists, err = tx.EdgeExistsForNode(ctx, n1)
	if err != nil {
		t.Fatalf("tx.EdgeExistsForNode after insert: %v", err)
	}
	if !exists {
		t.Errorf("tx.EdgeExistsForNode: expected true after edge insert, got false")
	}
}

// TestTxUpdateEdgeProps verifies that UpdateEdgeProps works within a transaction.
func TestTxUpdateEdgeProps(t *testing.T) {
	_, tx := openTx(t)
	ctx := context.Background()

	n1, n2 := insertTxNodes(t, tx)

	edgeID, err := tx.InsertEdge(ctx, "UPD_REL", n1, n2, `{"before":1}`)
	if err != nil {
		t.Fatalf("tx.InsertEdge: %v", err)
	}
	if err := tx.UpdateEdgeProps(ctx, edgeID, `{"after":2}`); err != nil {
		t.Fatalf("tx.UpdateEdgeProps: %v", err)
	}
	e, err := tx.GetEdge(ctx, edgeID)
	if err != nil {
		t.Fatalf("tx.GetEdge after UpdateEdgeProps: %v", err)
	}
	if !strings.Contains(e.Props, "after") {
		t.Errorf("tx.UpdateEdgeProps: expected updated props to contain 'after', got %q", e.Props)
	}
}
