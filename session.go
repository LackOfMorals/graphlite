// This file contains graphlite's explicit transaction type.
package graphlite

import (
	"context"
	"fmt"

	"github.com/LackOfMorals/graphlite/store"
)

// Tx is an explicit graphlite transaction. All queries run via Run share the
// same underlying database transaction. Call Commit to persist changes or
// Rollback to discard them.
//
// A Tx must not be used after Commit or Rollback returns.
type Tx struct {
	rawTx store.TxExecer
	done  bool
}

// Run executes cypherStr within the transaction and returns a lazy QueryResult.
// The result must be consumed (or iterated to exhaustion) before running another
// query on the same transaction — SQLite does not support multiple concurrent
// cursors on a single connection.
//
// params may be nil if the query has no parameters.
func (t *Tx) Run(ctx context.Context, cypherStr string, params map[string]any) (*QueryResult, error) {
	if t.done {
		return nil, fmt.Errorf("graphlite: transaction already closed")
	}
	return runQueryTx(ctx, t.rawTx, cypherStr, params)
}

// Commit commits the transaction. Returns an error if the transaction has
// already been committed or rolled back.
func (t *Tx) Commit() error {
	if t.done {
		return fmt.Errorf("graphlite: transaction already closed")
	}
	t.done = true
	if err := t.rawTx.Commit(); err != nil {
		return fmt.Errorf("graphlite: commit: %w", err)
	}
	return nil
}

// Rollback aborts the transaction. Returns an error if the transaction has
// already been committed or rolled back.
func (t *Tx) Rollback() error {
	if t.done {
		return fmt.Errorf("graphlite: transaction already closed")
	}
	t.done = true
	if err := t.rawTx.Rollback(); err != nil {
		return fmt.Errorf("graphlite: rollback: %w", err)
	}
	return nil
}
