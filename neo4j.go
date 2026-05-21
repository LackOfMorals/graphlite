// This file contains the DriverCompat adapter that satisfies the neo4j.Driver
// interface from github.com/neo4j/neo4j-go-driver/v6/neo4j, enabling code
// written against the Neo4j Go driver v6 to run against an embedded
// SQLite-backed graphlite instance with no network or Docker container.
//
// Usage:
//
//	driver, err := graphlite.NewDriver(":memory:", nil)
//	// use driver with neo4j.ExecuteQuery, session.ExecuteRead/Write, etc.
package graphlite

import (
	"context"
	"fmt"
	"net/url"
	"time"

	neo4j "github.com/neo4j/neo4j-go-driver/v6/neo4j"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j/db"
)

// Compile-time assertion: DriverCompat must satisfy neo4j.Driver.
var _ neo4j.Driver = (*DriverCompat)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// DriverCompat — top-level driver adapter
// ─────────────────────────────────────────────────────────────────────────────

// DriverCompat wraps a graphlite *DB and satisfies the neo4j.Driver interface.
//
// Auth tokens passed to NewDriver are accepted and silently ignored. DatabaseName
// in SessionConfig is accepted and ignored (graphlite is single-database). The URI
// ":memory:" opens an in-memory SQLite store; any other value is treated as a
// file path and opens (or creates) a persistent SQLite file.
type DriverCompat struct {
	db      *DB
	target  url.URL
	bkMgr   neo4j.BookmarkManager
}

// NewDriver creates a DriverCompat that satisfies neo4j.Driver.
//
// uri must be ":memory:" (in-memory) or a file-system path to a SQLite file.
// auth is accepted and silently ignored (graphlite has no authentication layer).
func NewDriver(uri string, _ neo4j.AuthToken) (*DriverCompat, error) {
	gdb, err := Open(uri)
	if err != nil {
		return nil, fmt.Errorf("graphlite: NewDriver: %w", err)
	}
	parsed, _ := url.Parse("bolt+ssc://localhost:7687")
	if parsed == nil {
		parsed = &url.URL{Scheme: "bolt", Host: "localhost:7687"}
	}
	return &DriverCompat{
		db:     gdb,
		target: *parsed,
		bkMgr:  neo4j.NewBookmarkManager(neo4j.BookmarkManagerConfig{}),
	}, nil
}

// Target returns a placeholder bolt URI (graphlite uses no network transport).
func (d *DriverCompat) Target() url.URL { return d.target }

// NewSession creates a new neo4j.Session backed by this DriverCompat.
// SessionConfig.DatabaseName is accepted and silently ignored.
func (d *DriverCompat) NewSession(_ context.Context, _ neo4j.SessionConfig) neo4j.Session {
	s := &compatSession{db: d.db}
	// Wire the EmbeddableSession's unexported-method callbacks to our
	// runManagedTx implementation. neo4j.ExecuteQuery calls
	// session.executeQueryWrite (or executeQueryRead) via selectTxFunctionApi;
	// those methods are satisfied by EmbeddableSession and delegate here.
	s.EmbeddableSession = &neo4j.EmbeddableSession{
		ExecQueryReadFn: func(ctx context.Context, work neo4j.ManagedTransactionWork, _ ...func(*neo4j.TransactionConfig)) (any, error) {
			return s.runManagedTx(ctx, work)
		},
		ExecQueryWriteFn: func(ctx context.Context, work neo4j.ManagedTransactionWork, _ ...func(*neo4j.TransactionConfig)) (any, error) {
			return s.runManagedTx(ctx, work)
		},
	}
	return s
}

// VerifyConnectivity reports nil — graphlite is always reachable.
func (d *DriverCompat) VerifyConnectivity(_ context.Context) error { return nil }

// VerifyAuthentication reports nil — graphlite has no authentication layer.
func (d *DriverCompat) VerifyAuthentication(_ context.Context, _ *neo4j.AuthToken) error {
	return nil
}

// GetServerInfo returns a minimal ServerInfo describing this embedded instance.
func (d *DriverCompat) GetServerInfo(_ context.Context) (neo4j.ServerInfo, error) {
	return &compatServerInfo{}, nil
}

// IsEncrypted returns false — graphlite has no network transport.
func (d *DriverCompat) IsEncrypted() bool { return false }

// ExecuteQueryBookmarkManager returns the bookmark manager used by ExecuteQuery.
// Bookmarks are accepted and tracked but have no semantic effect on SQLite queries.
func (d *DriverCompat) ExecuteQueryBookmarkManager() neo4j.BookmarkManager { return d.bkMgr }

// Close closes all resources held by the DriverCompat.
func (d *DriverCompat) Close(_ context.Context) error { return d.db.Close() }

// ─────────────────────────────────────────────────────────────────────────────
// compatSession — implements neo4j.Session
// ─────────────────────────────────────────────────────────────────────────────

// compatSession satisfies neo4j.Session by embedding *neo4j.EmbeddableSession
// (a concrete type injected into the vendored neo4j package by graphlite) instead
// of embedding a nil neo4j.Session interface. This avoids the nil-pointer panic
// that would otherwise occur when neo4j.ExecuteQuery calls the unexported
// session.executeQueryWrite / session.executeQueryRead methods.
//
// The embedded EmbeddableSession holds ExecQueryReadFn / ExecQueryWriteFn
// callbacks that delegate to this session's runManagedTx. All public Session
// methods (BeginTransaction, ExecuteRead, ExecuteWrite, Run, Close) are
// overridden directly on compatSession.
type compatSession struct {
	*neo4j.EmbeddableSession // concrete embed; satisfies unexported Session methods
	db                       *DB
	closed                   bool
}

// LastBookmarks returns an empty bookmark slice (graphlite does not use bookmarks).
func (s *compatSession) LastBookmarks() neo4j.Bookmarks { return neo4j.Bookmarks{} }

// BeginTransaction starts an explicit graphlite transaction wrapped in
// neo4j.ExplicitTransaction.
func (s *compatSession) BeginTransaction(ctx context.Context, _ ...func(*neo4j.TransactionConfig)) (neo4j.ExplicitTransaction, error) {
	if s.closed {
		return nil, fmt.Errorf("graphlite: session is closed")
	}
	tx, err := s.db.BeginTx(ctx, false)
	if err != nil {
		return nil, err
	}
	return &compatExplicitTx{tx: tx}, nil
}

// ExecuteRead executes work in a read-capable transaction.
// graphlite does not distinguish read-only from read-write at the SQLite level.
func (s *compatSession) ExecuteRead(ctx context.Context, work neo4j.ManagedTransactionWork, _ ...func(*neo4j.TransactionConfig)) (any, error) {
	return s.runManagedTx(ctx, work)
}

// ExecuteWrite executes work in a write-capable transaction.
func (s *compatSession) ExecuteWrite(ctx context.Context, work neo4j.ManagedTransactionWork, _ ...func(*neo4j.TransactionConfig)) (any, error) {
	return s.runManagedTx(ctx, work)
}

// runManagedTx begins a transaction, calls work, and commits on success or
// rolls back on failure.
func (s *compatSession) runManagedTx(ctx context.Context, work neo4j.ManagedTransactionWork) (any, error) {
	if s.closed {
		return nil, fmt.Errorf("graphlite: session is closed")
	}
	tx, err := s.db.BeginTx(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("graphlite: begin managed transaction: %w", err)
	}
	mtx := &compatManagedTx{tx: tx}
	result, err := work(mtx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return nil, fmt.Errorf("graphlite: commit managed transaction: %w", commitErr)
	}
	return result, nil
}

// Run executes an auto-commit statement on this session.
func (s *compatSession) Run(ctx context.Context, cypher string, params map[string]any, _ ...func(*neo4j.TransactionConfig)) (neo4j.Result, error) {
	if s.closed {
		return nil, fmt.Errorf("graphlite: session is closed")
	}
	qr, err := s.db.RunQuery(ctx, cypher, params)
	if err != nil {
		return nil, err
	}
	return newCompatResult(qr), nil
}

// Close marks this session as closed.
func (s *compatSession) Close(_ context.Context) error {
	s.closed = true
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// compatManagedTx — implements neo4j.ManagedTransaction
// ─────────────────────────────────────────────────────────────────────────────

// compatManagedTx wraps a graphlite *Tx and satisfies neo4j.ManagedTransaction.
type compatManagedTx struct {
	tx *Tx
}

// Run executes a Cypher statement on this managed transaction.
func (m *compatManagedTx) Run(ctx context.Context, cypher string, params map[string]any) (neo4j.Result, error) {
	qr, err := m.tx.Run(ctx, cypher, params)
	if err != nil {
		return nil, err
	}
	return newCompatResult(qr), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// compatExplicitTx — implements neo4j.ExplicitTransaction
// ─────────────────────────────────────────────────────────────────────────────

// compatExplicitTx wraps a graphlite *Tx and satisfies neo4j.ExplicitTransaction.
type compatExplicitTx struct {
	tx *Tx
}

// Run executes a Cypher statement on this explicit transaction.
func (e *compatExplicitTx) Run(ctx context.Context, cypher string, params map[string]any) (neo4j.Result, error) {
	qr, err := e.tx.Run(ctx, cypher, params)
	if err != nil {
		return nil, err
	}
	return newCompatResult(qr), nil
}

// Commit commits the explicit transaction.
func (e *compatExplicitTx) Commit(_ context.Context) error { return e.tx.Commit() }

// Rollback rolls back the explicit transaction.
func (e *compatExplicitTx) Rollback(_ context.Context) error { return e.tx.Rollback() }

// Close rolls back the transaction if it has not already been committed or
// rolled back. Returns nil if the transaction is already done.
func (e *compatExplicitTx) Close(ctx context.Context) error {
	if e.tx.done {
		return nil
	}
	return e.Rollback(ctx)
}

// ─────────────────────────────────────────────────────────────────────────────
// compatResult — implements neo4j.Result
// ─────────────────────────────────────────────────────────────────────────────

// compatResult wraps a graphlite *QueryResult and satisfies neo4j.Result by
// embedding neo4j.Result (to inherit the unexported buffer/errorHandler methods)
// and overriding all public methods.
//
// The embedded interface value is nil. The unexported methods buffer() and
// errorHandler() that neo4j.Result requires are only called by the driver's
// own transaction implementation (transaction.go:82,151) on results it creates
// internally — never on results returned by ManagedTransaction.Run(). Since
// compatSession replaces the driver's transaction layer entirely, these paths
// are never reached and the nil embed is safe. If a future driver version calls
// these methods on user-provided results this would panic; review the vendored
// transaction.go if upgrading the neo4j driver.
type compatResult struct {
	neo4j.Result // nil — satisfies unexported interface methods; see above
	qr           *QueryResult
}

// newCompatResult wraps a *QueryResult in a neo4j.Result adapter.
func newCompatResult(qr *QueryResult) neo4j.Result {
	return &compatResult{qr: qr}
}

// Keys returns the column names for this result set.
func (r *compatResult) Keys() ([]string, error) { return r.qr.Keys(), nil }

// Next advances the cursor to the next record.
func (r *compatResult) Next(ctx context.Context) bool { return r.qr.Next(ctx) }

// NextRecord advances and sets *record to the current record if one is available.
func (r *compatResult) NextRecord(ctx context.Context, record **neo4j.Record) bool {
	if !r.qr.Next(ctx) {
		return false
	}
	if record != nil {
		*record = toNeo4jRecord(r.qr.Record())
	}
	return true
}

// PeekRecord returns true if there is a record after the current one without advancing.
// graphlite's QueryResult does not support peek; this always returns false.
func (r *compatResult) PeekRecord(_ context.Context, _ **neo4j.Record) bool { return false }

// Peek returns true only if there is a record after the current one without advancing.
// graphlite's QueryResult does not support peek; this always returns false.
func (r *compatResult) Peek(_ context.Context) bool { return false }

// Err returns the latest iteration error.
func (r *compatResult) Err() error { return r.qr.Err() }

// Record returns the current record as a *neo4j.Record.
func (r *compatResult) Record() *neo4j.Record { return toNeo4jRecord(r.qr.Record()) }

// Collect fetches all remaining records and returns them as []*neo4j.Record.
func (r *compatResult) Collect(ctx context.Context) ([]*neo4j.Record, error) {
	recs, err := r.qr.Collect(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*neo4j.Record, len(recs))
	for i, rec := range recs {
		out[i] = toNeo4jRecord(rec)
	}
	return out, nil
}

// Records returns a single-use iterator over the records in this result.
func (r *compatResult) Records(ctx context.Context) func(yield func(*neo4j.Record, error) bool) {
	return func(yield func(*neo4j.Record, error) bool) {
		for r.qr.Next(ctx) {
			if !yield(toNeo4jRecord(r.qr.Record()), nil) {
				return
			}
		}
		if err := r.qr.Err(); err != nil {
			yield(nil, err)
		}
	}
}

// Single returns the only remaining record, or an error if 0 or 2+ remain.
func (r *compatResult) Single(ctx context.Context) (*neo4j.Record, error) {
	if !r.qr.Next(ctx) {
		if err := r.qr.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("graphlite: result contains no records")
	}
	rec := toNeo4jRecord(r.qr.Record())
	if r.qr.Next(ctx) {
		// Drain remaining rows.
		for r.qr.Next(ctx) {
		}
		return nil, fmt.Errorf("graphlite: result contains more than one record")
	}
	return rec, r.qr.Err()
}

// Consume discards remaining records and returns a ResultSummary.
func (r *compatResult) Consume(ctx context.Context) (neo4j.ResultSummary, error) {
	sum, err := r.qr.Consume(ctx)
	if err != nil {
		return nil, err
	}
	return newCompatResultSummary(sum), nil
}

// IsOpen returns true if the result cursor is still open.
func (r *compatResult) IsOpen() bool { return !r.qr.consumed }

// ─────────────────────────────────────────────────────────────────────────────
// Record conversion helpers
// ─────────────────────────────────────────────────────────────────────────────

// toNeo4jRecord converts a graphlite *Record to a *neo4j.Record (db.Record).
// Returns nil if rec is nil.
func toNeo4jRecord(rec *Record) *neo4j.Record {
	if rec == nil {
		return nil
	}
	keys := rec.Keys()
	vals := make([]any, len(keys))
	for i, k := range keys {
		v, _ := rec.Get(k)
		vals[i] = toNeo4jValue(v)
	}
	return &db.Record{Keys: keys, Values: vals}
}

// toNeo4jValue converts graphlite-internal value types to neo4j driver types.
// *Node and *Relationship are converted to neo4j dbtype.Node and dbtype.Relationship.
// All other values are returned unchanged.
func toNeo4jValue(v any) any {
	switch val := v.(type) {
	case *Node:
		if val == nil {
			return nil
		}
		return neo4j.Node{
			ElementId: val.ElementId,
			Labels:    val.Labels,
			Props:     val.Props,
		}
	case *Relationship:
		if val == nil {
			return nil
		}
		return neo4j.Relationship{
			ElementId:      val.ElementId,
			Type:           val.Type,
			StartElementId: val.StartElementId,
			EndElementId:   val.EndElementId,
			Props:          val.Props,
		}
	default:
		return v
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// compatResultSummary — implements neo4j.ResultSummary
// ─────────────────────────────────────────────────────────────────────────────

// compatResultSummary wraps a graphlite ResultSummary and satisfies neo4j.ResultSummary.
type compatResultSummary struct {
	inner ResultSummary
}

func newCompatResultSummary(inner ResultSummary) neo4j.ResultSummary {
	return &compatResultSummary{inner: inner}
}

func (s *compatResultSummary) Server() neo4j.ServerInfo    { return &compatServerInfo{} }
func (s *compatResultSummary) Query() neo4j.Query          { return &compatQuery{} }
func (s *compatResultSummary) StatementType() neo4j.StatementType {
	return neo4j.QueryTypeUnknown
}
func (s *compatResultSummary) QueryType() neo4j.QueryType { return neo4j.QueryTypeUnknown }
func (s *compatResultSummary) Counters() neo4j.Counters {
	return newCompatCounters(s.inner.Counters())
}
func (s *compatResultSummary) Plan() neo4j.Plan           { return nil }
func (s *compatResultSummary) Profile() neo4j.ProfiledPlan { return nil }
func (s *compatResultSummary) Notifications() []neo4j.Notification { return nil }
func (s *compatResultSummary) GqlStatusObjects() []neo4j.GqlStatusObject { return nil }
func (s *compatResultSummary) ResultAvailableAfter() time.Duration { return -1 }
func (s *compatResultSummary) ResultConsumedAfter() time.Duration  { return -1 }
func (s *compatResultSummary) Database() neo4j.DatabaseInfo { return &compatDatabaseInfo{} }

// ─────────────────────────────────────────────────────────────────────────────
// compatCounters — implements neo4j.Counters
// ─────────────────────────────────────────────────────────────────────────────

type compatCounters struct {
	inner Counters
}

func newCompatCounters(inner Counters) neo4j.Counters {
	return &compatCounters{inner: inner}
}

func (c *compatCounters) ContainsUpdates() bool      { return c.inner.ContainsUpdates() }
func (c *compatCounters) NodesCreated() int           { return c.inner.NodesCreated() }
func (c *compatCounters) NodesDeleted() int           { return c.inner.NodesDeleted() }
func (c *compatCounters) RelationshipsCreated() int   { return c.inner.RelationshipsCreated() }
func (c *compatCounters) RelationshipsDeleted() int   { return c.inner.RelationshipsDeleted() }
func (c *compatCounters) PropertiesSet() int          { return c.inner.PropertiesSet() }
func (c *compatCounters) LabelsAdded() int            { return 0 }
func (c *compatCounters) LabelsRemoved() int          { return 0 }
func (c *compatCounters) IndexesAdded() int           { return 0 }
func (c *compatCounters) IndexesRemoved() int         { return 0 }
func (c *compatCounters) ConstraintsAdded() int       { return 0 }
func (c *compatCounters) ConstraintsRemoved() int     { return 0 }
func (c *compatCounters) SystemUpdates() int          { return 0 }
func (c *compatCounters) ContainsSystemUpdates() bool { return false }

// ─────────────────────────────────────────────────────────────────────────────
// Minimal stub types for ServerInfo, Query, DatabaseInfo
// ─────────────────────────────────────────────────────────────────────────────

type compatServerInfo struct{}

func (i *compatServerInfo) Address() string                    { return "localhost:0" }
func (i *compatServerInfo) Agent() string                      { return "graphlite/1.0" }
func (i *compatServerInfo) ProtocolVersion() db.ProtocolVersion { return db.ProtocolVersion{} }

type compatQuery struct{}

func (q *compatQuery) Text() string               { return "" }
func (q *compatQuery) Parameters() map[string]any { return nil }

type compatDatabaseInfo struct{}

func (d *compatDatabaseInfo) Name() string { return "graphlite" }

