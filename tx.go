package graphlite

import (
	"context"
	"fmt"

	"github.com/LackOfMorals/graphlite/v2/store"
)

// Tx is an explicit graphlite transaction. All queries run via Run share the
// same underlying database transaction. Call Commit or Rollback to finish.
//
// Run returns an error if called after Commit or Rollback. However, calling
// Rollback after a successful Commit is explicitly safe and returns nil —
// this makes the standard defer-rollback guard pattern work without
// special-casing the success path:
//
//	tx, _ := db.BeginTx(ctx)
//	defer tx.Rollback() // no-op if tx.Commit() already succeeded
//	if err := doWork(tx); err != nil { return err }
//	return tx.Commit()
type Tx struct {
	rawTx       store.TxExecer
	done        bool
	maxPathHops int
	cache       *planCache // shared plan cache from the parent DB
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
	return runQueryTx(ctx, t.rawTx, cypherStr, params, t.maxPathHops, t.cache)
}

// Commit commits the transaction.
func (t *Tx) Commit() error {
	if t.done {
		return fmt.Errorf("graphlite: transaction already closed")
	}
	if err := t.rawTx.Commit(); err != nil {
		return fmt.Errorf("graphlite: commit: %w", err)
	}
	t.done = true
	return nil
}

// Rollback aborts the transaction. If the transaction has already been committed
// or rolled back, Rollback returns nil without doing anything. This follows the
// database/sql.Tx convention, which makes it safe to use in a deferred rollback
// guard alongside an explicit Commit call:
//
//	tx, _ := db.BeginTx(ctx)
//	defer tx.Rollback() // no-op if tx.Commit() already succeeded
//	if err := doWork(tx); err != nil { return err }
//	return tx.Commit()
func (t *Tx) Rollback() error {
	if t.done {
		// Already committed or rolled back — this is a deliberate no-op so that
		// the defer-rollback pattern works without special-casing the success path.
		return nil
	}
	t.done = true
	if err := t.rawTx.Rollback(); err != nil {
		return fmt.Errorf("graphlite: rollback: %w", err)
	}
	return nil
}

// Close rolls back the transaction if it has not already been committed or
// rolled back. Returns nil if the transaction is already done.
func (t *Tx) Close() error {
	if t.done {
		return nil
	}
	return t.Rollback()
}
