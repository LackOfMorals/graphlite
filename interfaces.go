package graphlite

import "context"

// Driver is the top-level handle for a graphlite database. It mirrors the
// public API of the Neo4j Go driver's Driver interface so that application
// code written against either can use the other with a thin adapter.
type Driver interface {
	// NewSession creates a new session for running queries.
	NewSession(ctx context.Context) Session
	// VerifyConnectivity checks that the database is reachable. For graphlite
	// this is always nil — the database is in-process.
	VerifyConnectivity(ctx context.Context) error
	// Close releases all resources held by the driver.
	Close(ctx context.Context) error
}

// Session represents a logical unit of work against the database.
type Session interface {
	// ExecuteRead runs work inside a read transaction with automatic
	// commit on success and rollback on error.
	ExecuteRead(ctx context.Context, work ManagedTransactionWork) (any, error)
	// ExecuteWrite runs work inside a write transaction with automatic
	// commit on success and rollback on error.
	ExecuteWrite(ctx context.Context, work ManagedTransactionWork) (any, error)
	// BeginTransaction starts an explicit transaction that the caller
	// manages with Commit, Rollback, or Close.
	BeginTransaction(ctx context.Context) (Transaction, error)
	// Run executes an auto-commit statement and returns a lazy result cursor.
	Run(ctx context.Context, cypher string, params map[string]any) (Result, error)
	// Close releases session resources.
	Close(ctx context.Context) error
}

// ManagedTransactionWork is the callback passed to ExecuteRead / ExecuteWrite.
type ManagedTransactionWork func(tx ManagedTransaction) (any, error)

// ManagedTransaction is the handle available inside ExecuteRead / ExecuteWrite.
type ManagedTransaction interface {
	// Run executes a Cypher statement within the managed transaction.
	Run(ctx context.Context, cypher string, params map[string]any) (Result, error)
}

// Transaction is an explicit transaction started with Session.BeginTransaction.
type Transaction interface {
	// Run executes a Cypher statement within this transaction.
	Run(ctx context.Context, cypher string, params map[string]any) (Result, error)
	// Commit persists all changes made in this transaction.
	Commit(ctx context.Context) error
	// Rollback discards all changes made in this transaction.
	Rollback(ctx context.Context) error
	// Close rolls back the transaction if it has not already been committed
	// or rolled back, then releases resources.
	Close(ctx context.Context) error
}

// Result is a lazy streaming cursor over query result records.
type Result interface {
	// Keys returns the projection column names for this result set.
	Keys() []string
	// Next advances the cursor. Returns true while records are available.
	Next(ctx context.Context) bool
	// Record returns the current record. Call after a successful Next.
	Record() *Record
	// Collect drains all remaining records and returns them as a slice.
	Collect(ctx context.Context) ([]*Record, error)
	// Err returns the first error encountered during iteration.
	Err() error
	// Consume drains any remaining records and returns the ResultSummary.
	Consume(ctx context.Context) (ResultSummary, error)
}

// ResultTransformer accumulates records from a query into a result of type T.
// It mirrors the neo4j.ResultTransformer pattern so that code using
// graphlite.ExecuteQuery looks identical to code using neo4j.ExecuteQuery.
type ResultTransformer[T any] interface {
	// Accept is called for each record returned by the query.
	Accept(*Record) error
	// Complete is called when all records have been consumed without error.
	Complete(keys []string, summary ResultSummary) (T, error)
}

// EagerResultTransformer returns a ResultTransformer that collects all records
// into an *EagerResult. Pass it to ExecuteQuery the same way you would pass
// neo4j.EagerResultTransformer to neo4j.ExecuteQuery.
func EagerResultTransformer() ResultTransformer[*EagerResult] {
	return &eagerResultTransformer{}
}

type eagerResultTransformer struct {
	records []*Record
}

func (e *eagerResultTransformer) Accept(rec *Record) error {
	e.records = append(e.records, rec)
	return nil
}

func (e *eagerResultTransformer) Complete(keys []string, summary ResultSummary) (*EagerResult, error) {
	return &EagerResult{Keys: keys, Records: e.records, Summary: summary}, nil
}

// ExecuteQuery runs query against driver in a single managed write transaction
// and transforms the result with newTransformer. It mirrors neo4j.ExecuteQuery
// so that call sites need only change the import, not the call shape.
func ExecuteQuery[T any](
	ctx context.Context,
	driver Driver,
	query string,
	params map[string]any,
	newTransformer func() ResultTransformer[T],
) (T, error) {
	sess := driver.NewSession(ctx)
	defer sess.Close(ctx) //nolint:errcheck

	result, err := sess.ExecuteWrite(ctx, func(tx ManagedTransaction) (any, error) {
		transformer := newTransformer()
		cursor, err := tx.Run(ctx, query, params)
		if err != nil {
			return nil, err
		}
		for cursor.Next(ctx) {
			if err := transformer.Accept(cursor.Record()); err != nil {
				return nil, err
			}
		}
		if err := cursor.Err(); err != nil {
			return nil, err
		}
		summary, err := cursor.Consume(ctx)
		if err != nil {
			return nil, err
		}
		return transformer.Complete(cursor.Keys(), summary)
	})
	if err != nil {
		return *new(T), err
	}
	return result.(T), nil
}

// Compile-time interface assertions.
var (
	_ Driver          = (*DB)(nil)
	_ Result          = (*QueryResult)(nil)
)
