package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // CGO-free SQLite driver
)

// Config holds optional SQLite configuration applied at Open time.
// Zero value is valid and uses SQLite's defaults.
type Config struct {
	// BusyTimeout sets PRAGMA busy_timeout (milliseconds). When non-zero,
	// SQLite will retry locked operations for up to this duration before
	// returning SQLITE_BUSY. Useful under concurrent write contention.
	BusyTimeout time.Duration
}

// SQLiteStore is the SQLite-backed implementation of Store.
// It uses modernc.org/sqlite so no CGO is required.
//
// The q field is the active SQL executor: it is set to db (as a querier) for
// non-transactional operation and to a *sql.Tx for transactional operation.
// The db field is always the underlying connection pool; it is used for
// lifecycle operations (Close, Snapshot, BeginExecTx) regardless of
// transaction state. The tx field is non-nil only within a transaction and
// provides Commit/Rollback; it is always equal to q when non-nil.
type SQLiteStore struct {
	db *sql.DB
	q  querier    // *sql.DB (no tx) or *sql.Tx (within a tx)
	tx *sql.Tx    // non-nil only when this store is a transaction scope
}

// Compile-time assertion: *SQLiteStore must satisfy both Store and Tx.
var _ Store = (*SQLiteStore)(nil)
var _ Tx = (*SQLiteStore)(nil)

// Open opens (or creates) a SQLite database at the given URI and returns a
// Store. The URI may be:
//   - ":memory:" for an in-memory database
//   - A file path (absolute or relative) for a persistent database
//   - Any valid modernc.org/sqlite DSN
//
// Open applies the schema DDL and enables WAL journal mode before returning.
func Open(uri string, cfg Config) (*SQLiteStore, error) {
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

	// Enforce foreign-key constraints. SQLite disables them by default;
	// enabling here ensures that edge inserts referencing non-existent node IDs
	// are rejected at the database level.
	if _, err := db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable foreign keys: %w", err)
	}

	if cfg.BusyTimeout > 0 {
		ms := cfg.BusyTimeout.Milliseconds()
		if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d;", ms)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: set busy_timeout: %w", err)
		}
	}

	// Apply schema DDL (idempotent via IF NOT EXISTS guards).
	if _, err := db.Exec(schemaDDL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}

	return &SQLiteStore{db: db, q: db}, nil
}

// DB returns the underlying *sql.DB. This method is on the concrete type only
// and is not part of the Store interface — use Exec() in interface-typed code.
func (s *SQLiteStore) DB() *sql.DB { return s.db }

// Exec returns the active SQL executor as a store.Execer. When called on a
// transaction-scoped store, the returned Execer runs within that transaction.
func (s *SQLiteStore) Exec() Execer { return s.q }

// BeginExecTx starts a new transaction and returns a TxExecer.
// Returns an error if called on an already-transactional store.
func (s *SQLiteStore) BeginExecTx(ctx context.Context) (TxExecer, error) {
	if s.tx != nil {
		return nil, fmt.Errorf("store: cannot nest transactions")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin exec tx: %w", err)
	}
	return tx, nil
}

// Close releases all resources held by the store.
func (s *SQLiteStore) Close() error { return s.db.Close() }

// Snapshot writes an atomic, consistent copy of the database to path using
// VACUUM INTO. path must not already exist.
//
// Path-traversal protection: the path is cleaned via filepath.Clean and any
// remaining ".." components are rejected. Symlinks in the parent directory
// are resolved so that ".." after symlink expansion is also caught. Note that
// absolute paths with no ".." components are accepted; callers that need to
// restrict snapshot destinations to a specific directory must enforce that
// constraint themselves.
func (s *SQLiteStore) Snapshot(path string) error {
	// Apply the same path-traversal protection used in driver.Open: clean the
	// path, resolve parent-directory symlinks, then reject any ".." component.
	cleaned := filepath.Clean(path)
	if dir, err := filepath.EvalSymlinks(filepath.Dir(cleaned)); err == nil {
		cleaned = filepath.Join(dir, filepath.Base(cleaned))
	}
	if slices.Contains(strings.Split(cleaned, string(filepath.Separator)), "..") {
		return fmt.Errorf("store: snapshot: path traversal not allowed: %q", path)
	}

	if _, err := s.db.Exec("VACUUM INTO ?", cleaned); err != nil {
		return fmt.Errorf("store: snapshot: %w", err)
	}
	return nil
}

// Begin starts a new database transaction and returns a Tx. The returned Tx
// is a *SQLiteStore whose querier is set to the underlying *sql.Tx so all
// CRUD methods execute within the transaction without per-method overrides.
// Returns an error if called on an already-transactional store.
func (s *SQLiteStore) Begin(ctx context.Context) (Tx, error) {
	if s.tx != nil {
		return nil, fmt.Errorf("store: cannot nest transactions")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin transaction: %w", err)
	}
	return &SQLiteStore{db: s.db, q: tx, tx: tx}, nil
}

// Commit commits the transaction. Returns an error if the store is not in a
// transaction scope.
func (s *SQLiteStore) Commit() error {
	if s.tx == nil {
		return fmt.Errorf("store: commit called on non-transactional store")
	}
	return s.tx.Commit()
}

// Rollback aborts the transaction. Returns an error if the store is not in a
// transaction scope.
func (s *SQLiteStore) Rollback() error {
	if s.tx == nil {
		return fmt.Errorf("store: rollback called on non-transactional store")
	}
	return s.tx.Rollback()
}

// --- Node operations ---

// InsertNode inserts a new node and returns its integer ID.
func (s *SQLiteStore) InsertNode(ctx context.Context, labels Labels, propsJSON string) (int64, error) {
	return insertNode(ctx, s.q, labels, propsJSON)
}

// GetNode returns the node with the given ID, or sql.ErrNoRows if not found.
func (s *SQLiteStore) GetNode(ctx context.Context, id int64) (*NodeRow, error) {
	return getNode(ctx, s.q, id)
}

// DeleteNode removes the node with the given ID.
func (s *SQLiteStore) DeleteNode(ctx context.Context, id int64) error {
	return deleteNode(ctx, s.q, id)
}

// ListNodes returns all nodes.
func (s *SQLiteStore) ListNodes(ctx context.Context) ([]*NodeRow, error) {
	return listNodes(ctx, s.q)
}

// ListNodesByLabel returns nodes whose labels column contains labelName.
func (s *SQLiteStore) ListNodesByLabel(ctx context.Context, labelName string) ([]*NodeRow, error) {
	return listNodesByLabel(ctx, s.q, labelName)
}

// UpdateNodeProps replaces the props JSON for the node with the given ID.
func (s *SQLiteStore) UpdateNodeProps(ctx context.Context, id int64, propsJSON string) error {
	return updateNodeProps(ctx, s.q, id, propsJSON)
}

// --- Edge operations ---

// InsertEdge inserts a new edge and returns its integer ID.
func (s *SQLiteStore) InsertEdge(ctx context.Context, edgeType string, startID, endID int64, propsJSON string) (int64, error) {
	return insertEdge(ctx, s.q, edgeType, startID, endID, propsJSON)
}

// GetEdge returns the edge with the given ID, or sql.ErrNoRows if not found.
func (s *SQLiteStore) GetEdge(ctx context.Context, id int64) (*EdgeRow, error) {
	return getEdge(ctx, s.q, id)
}

// DeleteEdge removes the edge with the given ID.
func (s *SQLiteStore) DeleteEdge(ctx context.Context, id int64) error {
	return deleteEdge(ctx, s.q, id)
}

// ListEdges returns all edges.
func (s *SQLiteStore) ListEdges(ctx context.Context) ([]*EdgeRow, error) {
	return listEdges(ctx, s.q)
}

// ListEdgesByType returns edges with the given relationship type.
func (s *SQLiteStore) ListEdgesByType(ctx context.Context, edgeType string) ([]*EdgeRow, error) {
	return listEdgesByType(ctx, s.q, edgeType)
}

// ListEdgesByStartNode returns all edges whose start_id equals startID.
func (s *SQLiteStore) ListEdgesByStartNode(ctx context.Context, startID int64) ([]*EdgeRow, error) {
	return listEdgesByStartNode(ctx, s.q, startID)
}

// ListEdgesByEndNode returns all edges whose end_id equals endID.
func (s *SQLiteStore) ListEdgesByEndNode(ctx context.Context, endID int64) ([]*EdgeRow, error) {
	return listEdgesByEndNode(ctx, s.q, endID)
}

// EdgeExistsForNode returns true if any edge references the given node ID.
func (s *SQLiteStore) EdgeExistsForNode(ctx context.Context, nodeID int64) (bool, error) {
	return edgeExistsForNode(ctx, s.q, nodeID)
}

// UpdateEdgeProps replaces the props JSON for the edge with the given ID.
func (s *SQLiteStore) UpdateEdgeProps(ctx context.Context, id int64, propsJSON string) error {
	return updateEdgeProps(ctx, s.q, id, propsJSON)
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
// Helper functions (shared for both non-transactional and transactional use)
// ============================================================================

func insertNode(ctx context.Context, q querier, labels Labels, propsJSON string) (int64, error) {
	res, err := q.ExecContext(ctx,
		`INSERT INTO nodes (labels, props) VALUES (?, json(?))`,
		labels.Encode(), propsJSON,
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
	defer func() { _ = rows.Close() }()
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
	defer func() { _ = rows.Close() }()
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
	defer func() { _ = rows.Close() }()
	return scanEdgeRows(rows)
}

func listEdgesByType(ctx context.Context, q querier, edgeType string) ([]*EdgeRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, type, start_id, end_id, props FROM edges WHERE type = ?`, edgeType,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list edges by type %q: %w", edgeType, err)
	}
	defer func() { _ = rows.Close() }()
	return scanEdgeRows(rows)
}

func listEdgesByStartNode(ctx context.Context, q querier, startID int64) ([]*EdgeRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, type, start_id, end_id, props FROM edges WHERE start_id = ?`, startID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list edges by start_id %d: %w", startID, err)
	}
	defer func() { _ = rows.Close() }()
	return scanEdgeRows(rows)
}

func listEdgesByEndNode(ctx context.Context, q querier, endID int64) ([]*EdgeRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, type, start_id, end_id, props FROM edges WHERE end_id = ?`, endID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list edges by end_id %d: %w", endID, err)
	}
	defer func() { _ = rows.Close() }()
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
