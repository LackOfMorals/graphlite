package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // CGO-free SQLite driver
)

// SQLiteStore is the SQLite-backed implementation of Store.
// It uses modernc.org/sqlite so no CGO is required.
type SQLiteStore struct {
	db *sql.DB
}

// Open opens (or creates) a SQLite database at the given URI and returns a
// Store. The URI may be:
//   - ":memory:" for an in-memory database
//   - A file path (absolute or relative) for a persistent database
//   - Any valid modernc.org/sqlite DSN
//
// Open applies the schema DDL and enables WAL journal mode before returning.
func Open(uri string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", uri, err)
	}

	// SQLite supports only one writer at a time and ":memory:" databases are
	// per-connection. A pool size of 1 ensures all operations share a single
	// connection, which is correct for both file-based and in-memory databases.
	db.SetMaxOpenConns(1)

	// Enable WAL mode for better concurrent-reader performance.
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable WAL mode: %w", err)
	}

	// Apply schema DDL (idempotent via IF NOT EXISTS guards).
	if _, err := db.Exec(schemaDDL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

// DB returns the underlying *sql.DB.
func (s *SQLiteStore) DB() *sql.DB { return s.db }

// Close releases all resources held by the store.
func (s *SQLiteStore) Close() error { return s.db.Close() }

// Begin starts a new database transaction and returns a Tx.
func (s *SQLiteStore) Begin(ctx context.Context) (Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin transaction: %w", err)
	}
	return &sqliteTx{SQLiteStore: &SQLiteStore{db: s.db}, tx: tx}, nil
}

// --- Node operations ---

// InsertNode inserts a new node and returns its integer ID.
func (s *SQLiteStore) InsertNode(ctx context.Context, labels string, propsJSON string) (int64, error) {
	return insertNode(ctx, s.db, labels, propsJSON)
}

// GetNode returns the node with the given ID, or sql.ErrNoRows if not found.
func (s *SQLiteStore) GetNode(ctx context.Context, id int64) (*NodeRow, error) {
	return getNode(ctx, s.db, id)
}

// DeleteNode removes the node with the given ID.
func (s *SQLiteStore) DeleteNode(ctx context.Context, id int64) error {
	return deleteNode(ctx, s.db, id)
}

// ListNodes returns all nodes.
func (s *SQLiteStore) ListNodes(ctx context.Context) ([]*NodeRow, error) {
	return listNodes(ctx, s.db)
}

// ListNodesByLabel returns nodes whose labels column contains labelName.
func (s *SQLiteStore) ListNodesByLabel(ctx context.Context, labelName string) ([]*NodeRow, error) {
	return listNodesByLabel(ctx, s.db, labelName)
}

// UpdateNodeProps replaces the props JSON for the node with the given ID.
func (s *SQLiteStore) UpdateNodeProps(ctx context.Context, id int64, propsJSON string) error {
	return updateNodeProps(ctx, s.db, id, propsJSON)
}

// --- Edge operations ---

// InsertEdge inserts a new edge and returns its integer ID.
func (s *SQLiteStore) InsertEdge(ctx context.Context, edgeType string, startID, endID int64, propsJSON string) (int64, error) {
	return insertEdge(ctx, s.db, edgeType, startID, endID, propsJSON)
}

// GetEdge returns the edge with the given ID, or sql.ErrNoRows if not found.
func (s *SQLiteStore) GetEdge(ctx context.Context, id int64) (*EdgeRow, error) {
	return getEdge(ctx, s.db, id)
}

// DeleteEdge removes the edge with the given ID.
func (s *SQLiteStore) DeleteEdge(ctx context.Context, id int64) error {
	return deleteEdge(ctx, s.db, id)
}

// ListEdges returns all edges.
func (s *SQLiteStore) ListEdges(ctx context.Context) ([]*EdgeRow, error) {
	return listEdges(ctx, s.db)
}

// ListEdgesByType returns edges with the given relationship type.
func (s *SQLiteStore) ListEdgesByType(ctx context.Context, edgeType string) ([]*EdgeRow, error) {
	return listEdgesByType(ctx, s.db, edgeType)
}

// ListEdgesByStartNode returns all edges whose start_id equals startID.
func (s *SQLiteStore) ListEdgesByStartNode(ctx context.Context, startID int64) ([]*EdgeRow, error) {
	return listEdgesByStartNode(ctx, s.db, startID)
}

// ListEdgesByEndNode returns all edges whose end_id equals endID.
func (s *SQLiteStore) ListEdgesByEndNode(ctx context.Context, endID int64) ([]*EdgeRow, error) {
	return listEdgesByEndNode(ctx, s.db, endID)
}

// EdgeExistsForNode returns true if any edge references the given node ID.
func (s *SQLiteStore) EdgeExistsForNode(ctx context.Context, nodeID int64) (bool, error) {
	return edgeExistsForNode(ctx, s.db, nodeID)
}

// UpdateEdgeProps replaces the props JSON for the edge with the given ID.
func (s *SQLiteStore) UpdateEdgeProps(ctx context.Context, id int64, propsJSON string) error {
	return updateEdgeProps(ctx, s.db, id, propsJSON)
}

// ============================================================================
// sqliteTx — transaction-scoped Store
// ============================================================================

// sqliteTx is a Store that executes all operations within a single SQL
// transaction. It embeds SQLiteStore to inherit all method implementations,
// but overrides the querier with a *sql.Tx.
type sqliteTx struct {
	*SQLiteStore
	tx *sql.Tx
}

// Compile-time assertion: sqliteTx must satisfy Tx.
var _ Tx = (*sqliteTx)(nil)

// Commit commits the transaction.
func (t *sqliteTx) Commit() error { return t.tx.Commit() }

// Rollback aborts the transaction.
func (t *sqliteTx) Rollback() error { return t.tx.Rollback() }

// Begin is not supported on a Tx; callers should use the parent Store.
func (t *sqliteTx) Begin(_ context.Context) (Tx, error) {
	return nil, fmt.Errorf("store: cannot nest transactions")
}

// DB returns the underlying *sql.DB (not the transaction's connection).
func (t *sqliteTx) DB() *sql.DB { return t.SQLiteStore.db }

// Override all operations to use the transaction's executor.

func (t *sqliteTx) InsertNode(ctx context.Context, labels string, propsJSON string) (int64, error) {
	return insertNode(ctx, t.tx, labels, propsJSON)
}

func (t *sqliteTx) GetNode(ctx context.Context, id int64) (*NodeRow, error) {
	return getNode(ctx, t.tx, id)
}

func (t *sqliteTx) DeleteNode(ctx context.Context, id int64) error {
	return deleteNode(ctx, t.tx, id)
}

func (t *sqliteTx) ListNodes(ctx context.Context) ([]*NodeRow, error) {
	return listNodes(ctx, t.tx)
}

func (t *sqliteTx) ListNodesByLabel(ctx context.Context, labelName string) ([]*NodeRow, error) {
	return listNodesByLabel(ctx, t.tx, labelName)
}

func (t *sqliteTx) UpdateNodeProps(ctx context.Context, id int64, propsJSON string) error {
	return updateNodeProps(ctx, t.tx, id, propsJSON)
}

func (t *sqliteTx) InsertEdge(ctx context.Context, edgeType string, startID, endID int64, propsJSON string) (int64, error) {
	return insertEdge(ctx, t.tx, edgeType, startID, endID, propsJSON)
}

func (t *sqliteTx) GetEdge(ctx context.Context, id int64) (*EdgeRow, error) {
	return getEdge(ctx, t.tx, id)
}

func (t *sqliteTx) DeleteEdge(ctx context.Context, id int64) error {
	return deleteEdge(ctx, t.tx, id)
}

func (t *sqliteTx) ListEdges(ctx context.Context) ([]*EdgeRow, error) {
	return listEdges(ctx, t.tx)
}

func (t *sqliteTx) ListEdgesByType(ctx context.Context, edgeType string) ([]*EdgeRow, error) {
	return listEdgesByType(ctx, t.tx, edgeType)
}

func (t *sqliteTx) ListEdgesByStartNode(ctx context.Context, startID int64) ([]*EdgeRow, error) {
	return listEdgesByStartNode(ctx, t.tx, startID)
}

func (t *sqliteTx) ListEdgesByEndNode(ctx context.Context, endID int64) ([]*EdgeRow, error) {
	return listEdgesByEndNode(ctx, t.tx, endID)
}

func (t *sqliteTx) EdgeExistsForNode(ctx context.Context, nodeID int64) (bool, error) {
	return edgeExistsForNode(ctx, t.tx, nodeID)
}

func (t *sqliteTx) UpdateEdgeProps(ctx context.Context, id int64, propsJSON string) error {
	return updateEdgeProps(ctx, t.tx, id, propsJSON)
}

// ============================================================================
// querier — shared interface for *sql.DB and *sql.Tx
// ============================================================================

// querier is satisfied by both *sql.DB and *sql.Tx, allowing helper functions
// to be reused for both non-transactional and transactional contexts.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ============================================================================
// Helper functions (shared between SQLiteStore and sqliteTx)
// ============================================================================

func insertNode(ctx context.Context, q querier, labels string, propsJSON string) (int64, error) {
	res, err := q.ExecContext(ctx,
		`INSERT INTO nodes (labels, props) VALUES (?, json(?))`,
		labels, propsJSON,
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert node: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: insert node last-id: %w", err)
	}
	return id, nil
}

func getNode(ctx context.Context, q querier, id int64) (*NodeRow, error) {
	row := q.QueryRowContext(ctx,
		`SELECT id, labels, props FROM nodes WHERE id = ?`, id,
	)
	var n NodeRow
	if err := row.Scan(&n.ID, &n.Labels, &n.Props); err != nil {
		return nil, err // preserve sql.ErrNoRows
	}
	return &n, nil
}

func deleteNode(ctx context.Context, q querier, id int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete node %d: %w", id, err)
	}
	return nil
}

func listNodes(ctx context.Context, q querier) ([]*NodeRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, labels, props FROM nodes`)
	if err != nil {
		return nil, fmt.Errorf("store: list nodes: %w", err)
	}
	defer rows.Close()
	return scanNodeRows(rows)
}

func listNodesByLabel(ctx context.Context, q querier, labelName string) ([]*NodeRow, error) {
	// Use the idx_nodes_labels index with an exact-match or instr-based check.
	// The label is stored in a comma-separated list, so we need to find it as
	// a whole word. We use instr to check for the label with comma delimiters,
	// covering the cases: label at start, middle, or end of the list.
	rows, err := q.QueryContext(ctx,
		`SELECT id, labels, props FROM nodes
		 WHERE labels = ?
		    OR labels LIKE ? || ',%'
		    OR labels LIKE '%,' || ?
		    OR labels LIKE '%,' || ? || ',%'`,
		labelName, labelName, labelName, labelName,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list nodes by label %q: %w", labelName, err)
	}
	defer rows.Close()
	return scanNodeRows(rows)
}

func updateNodeProps(ctx context.Context, q querier, id int64, propsJSON string) error {
	_, err := q.ExecContext(ctx,
		`UPDATE nodes SET props = json(?) WHERE id = ?`,
		propsJSON, id,
	)
	if err != nil {
		return fmt.Errorf("store: update node props %d: %w", id, err)
	}
	return nil
}

func insertEdge(ctx context.Context, q querier, edgeType string, startID, endID int64, propsJSON string) (int64, error) {
	res, err := q.ExecContext(ctx,
		`INSERT INTO edges (type, start_id, end_id, props) VALUES (?, ?, ?, json(?))`,
		edgeType, startID, endID, propsJSON,
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert edge: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: insert edge last-id: %w", err)
	}
	return id, nil
}

func getEdge(ctx context.Context, q querier, id int64) (*EdgeRow, error) {
	row := q.QueryRowContext(ctx,
		`SELECT id, type, start_id, end_id, props FROM edges WHERE id = ?`, id,
	)
	var e EdgeRow
	if err := row.Scan(&e.ID, &e.Type, &e.StartID, &e.EndID, &e.Props); err != nil {
		return nil, err // preserve sql.ErrNoRows
	}
	return &e, nil
}

func deleteEdge(ctx context.Context, q querier, id int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM edges WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete edge %d: %w", id, err)
	}
	return nil
}

func listEdges(ctx context.Context, q querier) ([]*EdgeRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, type, start_id, end_id, props FROM edges`)
	if err != nil {
		return nil, fmt.Errorf("store: list edges: %w", err)
	}
	defer rows.Close()
	return scanEdgeRows(rows)
}

func listEdgesByType(ctx context.Context, q querier, edgeType string) ([]*EdgeRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, type, start_id, end_id, props FROM edges WHERE type = ?`, edgeType,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list edges by type %q: %w", edgeType, err)
	}
	defer rows.Close()
	return scanEdgeRows(rows)
}

func listEdgesByStartNode(ctx context.Context, q querier, startID int64) ([]*EdgeRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, type, start_id, end_id, props FROM edges WHERE start_id = ?`, startID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list edges by start_id %d: %w", startID, err)
	}
	defer rows.Close()
	return scanEdgeRows(rows)
}

func listEdgesByEndNode(ctx context.Context, q querier, endID int64) ([]*EdgeRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, type, start_id, end_id, props FROM edges WHERE end_id = ?`, endID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list edges by end_id %d: %w", endID, err)
	}
	defer rows.Close()
	return scanEdgeRows(rows)
}

func edgeExistsForNode(ctx context.Context, q querier, nodeID int64) (bool, error) {
	row := q.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM edges WHERE start_id = ? OR end_id = ? LIMIT 1)`,
		nodeID, nodeID,
	)
	var exists bool
	if err := row.Scan(&exists); err != nil {
		return false, fmt.Errorf("store: edge exists for node %d: %w", nodeID, err)
	}
	return exists, nil
}

func updateEdgeProps(ctx context.Context, q querier, id int64, propsJSON string) error {
	_, err := q.ExecContext(ctx,
		`UPDATE edges SET props = json(?) WHERE id = ?`,
		propsJSON, id,
	)
	if err != nil {
		return fmt.Errorf("store: update edge props %d: %w", id, err)
	}
	return nil
}

// ============================================================================
// Row scanners
// ============================================================================

func scanNodeRows(rows *sql.Rows) ([]*NodeRow, error) {
	var result []*NodeRow
	for rows.Next() {
		var n NodeRow
		if err := rows.Scan(&n.ID, &n.Labels, &n.Props); err != nil {
			return nil, fmt.Errorf("store: scan node row: %w", err)
		}
		result = append(result, &n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate node rows: %w", err)
	}
	return result, nil
}

func scanEdgeRows(rows *sql.Rows) ([]*EdgeRow, error) {
	var result []*EdgeRow
	for rows.Next() {
		var e EdgeRow
		if err := rows.Scan(&e.ID, &e.Type, &e.StartID, &e.EndID, &e.Props); err != nil {
			return nil, fmt.Errorf("store: scan edge row: %w", err)
		}
		result = append(result, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate edge rows: %w", err)
	}
	return result, nil
}
