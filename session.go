package graphlite

import (
	"context"
	"fmt"

	"github.com/LackOfMorals/graphlite/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// session — implements Session
// ─────────────────────────────────────────────────────────────────────────────

// session is the concrete Session returned by DB.NewSession. It holds a
// reference to the parent *DB and manages transaction lifecycles.
type session struct {
	db     *DB
	closed bool
}

var _ Session = (*session)(nil)

// ExecuteRead runs work inside an auto-commit read transaction. graphlite does
// not distinguish read from write at the SQLite level; both paths use the same
// transaction machinery.
func (s *session) ExecuteRead(ctx context.Context, work ManagedTransactionWork) (any, error) {
	return s.runManaged(ctx, work)
}

// ExecuteWrite runs work inside an auto-commit write transaction.
func (s *session) ExecuteWrite(ctx context.Context, work ManagedTransactionWork) (any, error) {
	return s.runManaged(ctx, work)
}

// runManaged begins a transaction, calls work, and commits on success or rolls
// back on error.
func (s *session) runManaged(ctx context.Context, work ManagedTransactionWork) (any, error) {
	if s.closed {
		return nil, fmt.Errorf("graphlite: session is closed")
	}
	tx, err := s.db.BeginTx(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("graphlite: begin managed transaction: %w", err)
	}
	result, err := work(&managedTx{tx: tx})
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("graphlite: commit managed transaction: %w", err)
	}
	return result, nil
}

// BeginTransaction starts an explicit transaction.
func (s *session) BeginTransaction(ctx context.Context) (Transaction, error) {
	if s.closed {
		return nil, fmt.Errorf("graphlite: session is closed")
	}
	tx, err := s.db.BeginTx(ctx, false)
	if err != nil {
		return nil, err
	}
	return tx, nil
}

// Run executes an auto-commit statement on this session.
func (s *session) Run(ctx context.Context, cypher string, params map[string]any) (Result, error) {
	if s.closed {
		return nil, fmt.Errorf("graphlite: session is closed")
	}
	return s.db.RunQuery(ctx, cypher, params)
}

// Close marks this session as closed.
func (s *session) Close(_ context.Context) error {
	s.closed = true
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// managedTx — implements ManagedTransaction
// ─────────────────────────────────────────────────────────────────────────────

type managedTx struct{ tx *Tx }

var _ ManagedTransaction = (*managedTx)(nil)

func (m *managedTx) Run(ctx context.Context, cypher string, params map[string]any) (Result, error) {
	return m.tx.Run(ctx, cypher, params)
}

// ─────────────────────────────────────────────────────────────────────────────
// Tx — explicit transaction, implements Transaction
// ─────────────────────────────────────────────────────────────────────────────

// Tx is an explicit graphlite transaction. All queries run via Run share the
// same underlying database transaction. Call Commit or Rollback to finish.
//
// A Tx must not be used after Commit or Rollback returns.
type Tx struct {
	rawTx store.TxExecer
	done  bool
}

var _ Transaction = (*Tx)(nil)

// Run executes cypherStr within the transaction and returns a lazy Result.
// The result must be consumed before running another query on the same
// transaction — SQLite does not support multiple concurrent cursors on one
// connection.
//
// params may be nil if the query has no parameters.
func (t *Tx) Run(ctx context.Context, cypherStr string, params map[string]any) (Result, error) {
	if t.done {
		return nil, fmt.Errorf("graphlite: transaction already closed")
	}
	return runQueryTx(ctx, t.rawTx, cypherStr, params)
}

// Commit commits the transaction.
func (t *Tx) Commit(_ context.Context) error {
	if t.done {
		return fmt.Errorf("graphlite: transaction already closed")
	}
	t.done = true
	if err := t.rawTx.Commit(); err != nil {
		return fmt.Errorf("graphlite: commit: %w", err)
	}
	return nil
}

// Rollback aborts the transaction.
func (t *Tx) Rollback(_ context.Context) error {
	if t.done {
		return fmt.Errorf("graphlite: transaction already closed")
	}
	t.done = true
	if err := t.rawTx.Rollback(); err != nil {
		return fmt.Errorf("graphlite: rollback: %w", err)
	}
	return nil
}

// Close rolls back the transaction if it has not already been committed or
// rolled back. Returns nil if the transaction is already done.
func (t *Tx) Close(ctx context.Context) error {
	if t.done {
		return nil
	}
	return t.Rollback(ctx)
}
