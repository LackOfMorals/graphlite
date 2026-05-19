// Package graphlite provides the public native API: Open, DB, and query
// execution. The execution pipeline is:
//
//  1. cypher.Parse  → *Query
//  2. cypher.Plan   → LogicalPlan
//  3. sql.Translate → Result (with paramSentinels and idSentinels in Args)
//  4. sql.BindParams → Result (paramSentinels replaced by caller values)
//  5. executeStatements → run each Statement; KindMatchForWrite populates idMap;
//     KindInsertNode captures last-insert rowid → idMap; idSentinels resolved
//     from idMap before each write statement.
package graphlite

import (
	"context"
	stdsql "database/sql"
	"errors"
	"fmt"

	"github.com/LackOfMorals/graphlite/cypher"
	glsql "github.com/LackOfMorals/graphlite/sql"
	"github.com/LackOfMorals/graphlite/store"
)

// DB is an open graphlite database. All methods are safe for concurrent use
// from multiple goroutines.
type DB struct {
	st store.Store
}

// Open opens (or creates) a graphlite database at path and returns a *DB.
//
// Use ":memory:" for a transient in-memory database. A file path (absolute or
// relative) opens (or creates) a persistent SQLite file.
//
// Open applies the schema DDL and enables WAL journal mode before returning.
func Open(path string) (*DB, error) {
	st, err := store.Open(path)
	if err != nil {
		return nil, fmt.Errorf("graphlite: open %q: %w", path, err)
	}
	return &DB{st: st}, nil
}

// Close releases all resources held by the database. Subsequent calls on a
// closed DB return errors.
func (d *DB) Close() error {
	if err := d.st.Close(); err != nil {
		return fmt.Errorf("graphlite: close: %w", err)
	}
	return nil
}

// RunQuery executes cypherStr in auto-commit mode and returns a lazy
// QueryResult cursor. The caller must consume or exhaust the result to release
// underlying resources.
//
// params may be nil if the query has no parameters.
func (d *DB) RunQuery(ctx context.Context, cypherStr string, params map[string]any) (*QueryResult, error) {
	return runQuery(ctx, d.st.DB(), cypherStr, params)
}

// BeginTx starts an explicit transaction and returns a *Tx.
//
// readOnly is accepted but ignored — SQLite does not distinguish read-only
// transactions at the Go driver level.
func (d *DB) BeginTx(ctx context.Context, _ bool) (*Tx, error) {
	rawTx, err := d.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("graphlite: begin transaction: %w", err)
	}
	return &Tx{rawTx: rawTx}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Core execution pipeline (shared by auto-commit and transactional paths)
// ─────────────────────────────────────────────────────────────────────────────

// execer abstracts *stdsql.DB and *stdsql.Tx for the execution helpers.
type execer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*stdsql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *stdsql.Row
	ExecContext(ctx context.Context, query string, args ...any) (stdsql.Result, error)
}

// runQuery is the execution pipeline against *stdsql.DB (auto-commit).
func runQuery(ctx context.Context, db *stdsql.DB, cypherStr string, params map[string]any) (*QueryResult, error) {
	sqlResult, err := buildSQLResult(cypherStr, params)
	if err != nil {
		return nil, err
	}
	return executeStatements(ctx, db, sqlResult)
}

// runQueryTx is the execution pipeline against *stdsql.Tx (transactional).
func runQueryTx(ctx context.Context, tx *stdsql.Tx, cypherStr string, params map[string]any) (*QueryResult, error) {
	sqlResult, err := buildSQLResult(cypherStr, params)
	if err != nil {
		return nil, err
	}
	return executeStatements(ctx, tx, sqlResult)
}

// buildSQLResult runs parse → plan → translate → bind-params, returning the
// bound Result ready for execution.
func buildSQLResult(cypherStr string, params map[string]any) (glsql.Result, error) {
	// Step 1: parse.
	q, err := cypher.Parse(cypherStr)
	if err != nil {
		return glsql.Result{}, fmt.Errorf("graphlite: parse: %w", err)
	}

	// Step 2: plan.
	scope := cypher.NewScope()
	plan, err := cypher.Plan(q, scope)
	if err != nil {
		return glsql.Result{}, fmt.Errorf("graphlite: plan: %w", err)
	}

	// Step 3: translate.
	translator := glsql.NewTranslator(glsql.SQLiteDialect{})
	sqlResult, err := translator.Translate(plan, scope)
	if err != nil {
		return glsql.Result{}, fmt.Errorf("graphlite: translate: %w", err)
	}

	// Step 4: bind named parameters.
	if params == nil {
		params = map[string]any{}
	}
	sqlResult, err = glsql.BindParams(sqlResult, params)
	if err != nil {
		var mp *glsql.ErrMissingParam
		if errors.As(err, &mp) {
			return glsql.Result{}, &ErrMissingParameter{Name: mp.Name}
		}
		return glsql.Result{}, fmt.Errorf("graphlite: bind params: %w", err)
	}

	return sqlResult, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Statement executor
// ─────────────────────────────────────────────────────────────────────────────

// executeStatements runs all Statements in sqlResult against ex (a *stdsql.DB
// or *stdsql.Tx). For a single KindSelect it returns a lazy QueryResult; for
// write statements it executes each in order and returns a QueryResult with
// counters.
func executeStatements(ctx context.Context, ex execer, sqlResult glsql.Result) (*QueryResult, error) {
	stmts := sqlResult.Statements
	if len(stmts) == 0 {
		return nil, fmt.Errorf("graphlite: no SQL statements to execute")
	}

	// A single SELECT statement: return a lazy cursor.
	if len(stmts) == 1 && stmts[0].Kind == glsql.KindSelect {
		rows, err := ex.QueryContext(ctx, stmts[0].SQL, stmts[0].Args...)
		if err != nil {
			return nil, fmt.Errorf("graphlite: query: %w", err)
		}
		qr, err := NewQueryResultFromRows(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		return qr, nil
	}

	// Write (or guard+write) statements: execute in order.
	return execWriteStatements(ctx, ex, stmts)
}

// execWriteStatements runs write statements, resolving idSentinels between steps.
//
// The statement sequence may begin with a KindMatchForWrite SELECT that returns
// matched variable IDs row by row. When encountered, we execute all subsequent
// write statements once per matched row, accumulating counters across all rows.
func execWriteStatements(ctx context.Context, ex execer, stmts []glsql.Statement) (*QueryResult, error) {
	// idMap: Cypher variable name → int64 row ID from INSERT or MATCH SELECT.
	idMap := make(map[string]int64)
	var ctr QueryCounters

	// Separate the leading KindMatchForWrite statement (if any) from the rest.
	var matchStmt *glsql.Statement
	writeStart := 0
	if len(stmts) > 0 && stmts[0].Kind == glsql.KindMatchForWrite {
		matchStmt = &stmts[0]
		writeStart = 1
	}

	writeStmts := stmts[writeStart:]

	if matchStmt != nil {
		// MATCH+write: execute the match SELECT to get matched IDs, collect all
		// rows into memory first (closing the cursor), then run write statements.
		// This is required because SQLite only allows one active statement per
		// connection; the cursor must be closed before any write statement can run.
		matchedRows, cols, err := collectMatchRows(ctx, ex, matchStmt)
		if err != nil {
			return nil, err
		}
		// Run write statements once per matched row.
		for _, rowVals := range matchedRows {
			for i, col := range cols {
				switch v := rowVals[i].(type) {
				case int64:
					idMap[col] = v
				case float64:
					idMap[col] = int64(v)
				}
			}
			if err := execWriteBatch(ctx, ex, writeStmts, idMap, &ctr); err != nil {
				return nil, err
			}
		}
	} else {
		// Pure write (no MATCH): run all statements in order once.
		if err := execWriteBatch(ctx, ex, writeStmts, idMap, &ctr); err != nil {
			return nil, err
		}
	}

	qr := &QueryResult{consumed: true}
	qr.SetCounters(ctr)
	return qr, nil
}

// collectMatchRows executes a KindMatchForWrite SELECT, drains all rows into
// memory, closes the cursor, and returns (rows, columnNames, error).
// The cursor is always closed before returning — callers may safely issue write
// statements immediately afterwards on the same single-connection DB.
func collectMatchRows(ctx context.Context, ex execer, stmt *glsql.Statement) ([][]any, []string, error) {
	rows, err := ex.QueryContext(ctx, stmt.SQL, stmt.Args...)
	if err != nil {
		return nil, nil, fmt.Errorf("graphlite: match-for-write select: %w", err)
	}
	cols, err := rows.Columns()
	if err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("graphlite: match-for-write columns: %w", err)
	}
	var result [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("graphlite: match-for-write scan: %w", err)
		}
		row := make([]any, len(cols))
		copy(row, vals)
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("graphlite: match-for-write iterate: %w", err)
	}
	rows.Close()
	return result, cols, nil
}

// execWriteBatch runs one "batch" of write statements with the provided idMap.
// idMap is updated in place as INSERT statements produce new row IDs.
func execWriteBatch(ctx context.Context, ex execer, stmts []glsql.Statement, idMap map[string]int64, ctr *QueryCounters) error {
	for _, stmt := range stmts {
		// Resolve idSentinels from idMap.
		resolved, err := glsql.ResolveIDs(glsql.Result{Statements: []glsql.Statement{stmt}}, idMap)
		if err != nil {
			return fmt.Errorf("graphlite: resolve IDs: %w", err)
		}
		s := resolved.Statements[0]

		switch s.Kind {
		case glsql.KindDeleteGuard:
			var count int64
			if err := ex.QueryRowContext(ctx, s.SQL, s.Args...).Scan(&count); err != nil {
				return fmt.Errorf("graphlite: guard check: %w", err)
			}
			if count > 0 {
				return fmt.Errorf("graphlite: cannot delete node with existing relationships (use DETACH DELETE)")
			}

		case glsql.KindInsertNode:
			res, err := ex.ExecContext(ctx, s.SQL, s.Args...)
			if err != nil {
				return fmt.Errorf("graphlite: insert node: %w", err)
			}
			lastID, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("graphlite: insert node last-id: %w", err)
			}
			// Bind the new ID to the Cypher variable that this INSERT created.
			if stmt.CreatedVar != "" {
				idMap[stmt.CreatedVar] = lastID
			}
			ctr.NodesCreated++

		case glsql.KindInsertEdge:
			if _, err := ex.ExecContext(ctx, s.SQL, s.Args...); err != nil {
				return fmt.Errorf("graphlite: insert edge: %w", err)
			}
			ctr.RelationshipsCreated++

		case glsql.KindUpdate:
			if _, err := ex.ExecContext(ctx, s.SQL, s.Args...); err != nil {
				return fmt.Errorf("graphlite: update: %w", err)
			}
			ctr.PropertiesSet++

		case glsql.KindDeleteEdges:
			res, err := ex.ExecContext(ctx, s.SQL, s.Args...)
			if err != nil {
				return fmt.Errorf("graphlite: delete edges: %w", err)
			}
			n, _ := res.RowsAffected()
			ctr.RelationshipsDeleted += int(n)

		case glsql.KindDeleteNodes:
			res, err := ex.ExecContext(ctx, s.SQL, s.Args...)
			if err != nil {
				return fmt.Errorf("graphlite: delete node: %w", err)
			}
			n, _ := res.RowsAffected()
			ctr.NodesDeleted += int(n)

		default:
			if _, err := ex.ExecContext(ctx, s.SQL, s.Args...); err != nil {
				return fmt.Errorf("graphlite: exec: %w", err)
			}
		}
	}
	return nil
}
