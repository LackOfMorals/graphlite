// Package store defines the storage interface and its SQLite implementation.
// The store layer works only with raw IDs, label strings, JSON blobs, and SQL
// result rows — it never imports Cypher types.
package store

import (
	"context"
	"database/sql"
)

// Execer is the SQL execution surface required by the query translation layer.
// It is satisfied by *database/sql.DB, *database/sql.Tx, and by any adapter
// that wraps another database/sql-compatible driver (DuckDB, PostgreSQL, etc.).
type Execer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// TxExecer is an Execer scoped to a single database transaction.
// Commit persists the transaction; Rollback discards it.
type TxExecer interface {
	Execer
	Commit() error
	Rollback() error
}

// NodeRow is the raw node record as stored in SQLite.
// Props is a JSON object encoded as a string.
// Labels is a comma-separated list of label names (e.g. "Person,Employee").
type NodeRow struct {
	ID     int64
	Labels string // comma-separated label names
	Props  string // JSON object
}

// EdgeRow is the raw edge record as stored in SQLite.
// Props is a JSON object encoded as a string.
type EdgeRow struct {
	ID      int64
	Type    string
	StartID int64
	EndID   int64
	Props   string // JSON object
}

// Store is the abstraction boundary between the Cypher query layer and SQLite
// persistence. All methods operate on raw IDs, label strings, JSON blobs, and
// SQL primitives — no Cypher types cross this boundary.
//
// The Store is safe for concurrent reads when opened with WAL mode (which the
// SQLiteStore implementation enables automatically).
type Store interface {
	// --- Node operations ---

	// InsertNode inserts a new node with the given comma-separated labels and
	// JSON-encoded properties. It returns the new node's integer ID.
	InsertNode(ctx context.Context, labels string, propsJSON string) (int64, error)

	// GetNode returns the node with the given ID, or sql.ErrNoRows if not found.
	GetNode(ctx context.Context, id int64) (*NodeRow, error)

	// DeleteNode removes the node with the given ID.
	// It does NOT remove associated edges; callers are responsible for edge cleanup.
	DeleteNode(ctx context.Context, id int64) error

	// ListNodes returns all nodes. An empty result set is not an error.
	ListNodes(ctx context.Context) ([]*NodeRow, error)

	// ListNodesByLabel returns nodes whose labels column contains labelName.
	ListNodesByLabel(ctx context.Context, labelName string) ([]*NodeRow, error)

	// UpdateNodeProps replaces the props JSON for the node with the given ID.
	UpdateNodeProps(ctx context.Context, id int64, propsJSON string) error

	// --- Edge operations ---

	// InsertEdge inserts a new edge with the given type, start/end node IDs,
	// and JSON-encoded properties. It returns the new edge's integer ID.
	InsertEdge(ctx context.Context, edgeType string, startID, endID int64, propsJSON string) (int64, error)

	// GetEdge returns the edge with the given ID, or sql.ErrNoRows if not found.
	GetEdge(ctx context.Context, id int64) (*EdgeRow, error)

	// DeleteEdge removes the edge with the given ID.
	DeleteEdge(ctx context.Context, id int64) error

	// ListEdges returns all edges. An empty result set is not an error.
	ListEdges(ctx context.Context) ([]*EdgeRow, error)

	// ListEdgesByType returns edges with the given relationship type.
	ListEdgesByType(ctx context.Context, edgeType string) ([]*EdgeRow, error)

	// ListEdgesByStartNode returns all edges whose start_id equals startID.
	ListEdgesByStartNode(ctx context.Context, startID int64) ([]*EdgeRow, error)

	// ListEdgesByEndNode returns all edges whose end_id equals endID.
	ListEdgesByEndNode(ctx context.Context, endID int64) ([]*EdgeRow, error)

	// EdgeExistsForNode returns true if any edge references the given node ID
	// as either start_id or end_id.
	EdgeExistsForNode(ctx context.Context, nodeID int64) (bool, error)

	// UpdateEdgeProps replaces the props JSON for the edge with the given ID.
	UpdateEdgeProps(ctx context.Context, id int64, propsJSON string) error

	// --- Transaction management ---

	// Begin starts a new database transaction and returns a Store that operates
	// within that transaction. Call Commit or Rollback on the returned Tx.
	Begin(ctx context.Context) (Tx, error)

	// --- Lifecycle ---

	// Close releases all resources held by the store.
	Close() error

	// Exec returns the SQL execution surface for running generated queries.
	// The returned Execer shares the store's connection and transaction state.
	Exec() Execer

	// BeginExecTx starts a database transaction and returns a TxExecer that
	// runs all subsequent SQL within that transaction. Call Commit or Rollback
	// on the returned TxExecer to finish the transaction.
	//
	// Returns an error if called on a Store that is already a transaction scope
	// (i.e. nested transactions are not supported).
	BeginExecTx(ctx context.Context) (TxExecer, error)
}

// Snapshotter is implemented by stores that support atomic file snapshots.
// SQLiteStore satisfies this via VACUUM INTO.
type Snapshotter interface {
	// Snapshot writes a consistent copy of the database to path.
	// path must not already exist; SQLite refuses to overwrite an existing file.
	Snapshot(path string) error
}

// Tx is a Store scoped to a single database transaction. It embeds Store so
// all read and write methods are available within the transaction context.
type Tx interface {
	Store

	// Commit commits the transaction. Subsequent calls return an error.
	Commit() error

	// Rollback aborts the transaction. Subsequent calls return an error.
	// Rollback is a no-op if the transaction has already been committed.
	Rollback() error
}
