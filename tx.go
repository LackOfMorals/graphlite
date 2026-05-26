package graphlite

import (
	"context"
	"fmt"

	"github.com/LackOfMorals/graphlite/store"
)

// Tx is an explicit graphlite transaction. All queries run via Run share the
// same underlying database transaction. Call Commit or Rollback to finish.
//
// A Tx must not be used after Commit or Rollback returns.
type Tx struct {
	rawTx store.TxExecer
	done  bool
}

// Run executes cypherStr within the transaction and returns a lazy *Result.
// The result must be consumed before running another query on the same
// transaction — SQLite does not support multiple concurrent cursors on one
// connection.
//
// params may be nil if the query has no parameters.
func (t *Tx) Run(ctx context.Context, cypherStr string, params map[string]any) (*Result, error) {
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
