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
	"path/filepath"
	"strings"

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
//
// Path traversal protection: if path is not ":memory:", Open rejects any path
// whose filepath.Clean form contains ".." components to prevent directory
// traversal attacks (e.g. "../../etc/passwd" is rejected).
func Open(path string) (*DB, error) {
	if path != ":memory:" {
		cleaned := filepath.Clean(path)
		// filepath.Clean resolves ".." components; if the result still contains
		// a ".." element the original path attempted to escape the working tree.
		for _, part := range strings.Split(cleaned, string(filepath.Separator)) {
			if part == ".." {
				return nil, fmt.Errorf("graphlite: Open: path traversal not allowed: %q", path)
			}
		}
	}
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
//
// When the statement list contains a KindMergeCheck and ex is a *stdsql.DB
// (auto-commit), the execution is wrapped in an implicit transaction so the
// check+insert is atomic. If ex is already a *stdsql.Tx the caller's
// transaction is used directly.
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

	// Write-then-read: statement list ends with a KindSelectAfterWrite.
	// Execute all write statements first, then run the final SELECT using the
	// resolved IDs from the write batch's idMap.
	if len(stmts) >= 1 && stmts[len(stmts)-1].Kind == glsql.KindSelectAfterWrite {
		return execWriteThenSelect(ctx, ex, stmts)
	}

	// If the statement list contains a MERGE check and ex is a plain *stdsql.DB,
	// wrap the execution in an implicit transaction for atomicity.
	hasMerge := false
	for _, s := range stmts {
		if s.Kind == glsql.KindMergeCheck {
			hasMerge = true
			break
		}
	}
	if hasMerge {
		if db, ok := ex.(*stdsql.DB); ok {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return nil, fmt.Errorf("graphlite: MERGE begin tx: %w", err)
			}
			qr, err := execWriteStatements(ctx, tx, stmts)
			if err != nil {
				_ = tx.Rollback()
				return nil, err
			}
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("graphlite: MERGE commit: %w", err)
			}
			return qr, nil
		}
		// Already a *stdsql.Tx — use it directly (the caller manages the transaction).
	}

	// Write (or guard+write) statements: execute in order.
	return execWriteStatements(ctx, ex, stmts)
}

// execWriteThenSelect executes a write-then-read statement sequence produced by
// CREATE … RETURN … and MATCH … SET/DELETE … RETURN … queries.
//
// The statement list is: [optional KindMatchForWrite, write stmts..., KindSelectAfterWrite].
//
// For pure CREATE+RETURN (no KindMatchForWrite):
//   - Runs all write statements once, collecting idMap from INSERT results.
//   - Runs the final SELECT once with idSentinels resolved from idMap.
//
// For MATCH+write+RETURN (starts with KindMatchForWrite):
//   - Runs the match SELECT to get matched IDs row by row.
//   - For each matched row: runs write statements, then runs the final SELECT.
//   - Collects all result rows across all matched rows.
func execWriteThenSelect(ctx context.Context, ex execer, stmts []glsql.Statement) (*QueryResult, error) {
	selectStmt := stmts[len(stmts)-1]
	writeStmts := stmts[:len(stmts)-1]
	var ctr QueryCounters

	// Check if the write batch starts with a KindMatchForWrite.
	var matchStmt *glsql.Statement
	writeBatch := writeStmts
	if len(writeStmts) > 0 && writeStmts[0].Kind == glsql.KindMatchForWrite {
		matchStmt = &writeStmts[0]
		writeBatch = writeStmts[1:]
	}

	if matchStmt != nil {
		// MATCH + write + RETURN: iterate over matched rows.
		matchedRows, cols, err := collectMatchRows(ctx, ex, matchStmt)
		if err != nil {
			return nil, err
		}

		// Collect all result rows from the SELECT across all matched rows.
		var allRecords []*Record
		var resultKeys []string

		for _, rowVals := range matchedRows {
			idMap := make(map[string]int64)
			for i, col := range cols {
				switch v := rowVals[i].(type) {
				case int64:
					idMap[col] = v
				case float64:
					idMap[col] = int64(v)
				}
			}

			if err := execWriteBatch(ctx, ex, writeBatch, idMap, &ctr); err != nil {
				return nil, err
			}

			// Run the final SELECT for this matched row.
			resolved, err := glsql.ResolveIDs(glsql.Result{Statements: []glsql.Statement{selectStmt}}, idMap)
			if err != nil {
				return nil, fmt.Errorf("graphlite: write-then-select resolve IDs: %w", err)
			}
			s := resolved.Statements[0]

			rows, err := ex.QueryContext(ctx, s.SQL, s.Args...)
			if err != nil {
				return nil, fmt.Errorf("graphlite: write-then-select query: %w", err)
			}
			rowQR, err := NewQueryResultFromRows(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			recs, err := rowQR.Collect(ctx)
			if err != nil {
				return nil, fmt.Errorf("graphlite: write-then-select collect: %w", err)
			}
			if len(resultKeys) == 0 {
				resultKeys = rowQR.Keys()
			}
			allRecords = append(allRecords, recs...)
		}

		qr := newInMemoryQueryResult(resultKeys, allRecords)
		qr.SetCounters(ctr)
		return qr, nil
	}

	// Pure write (no MATCH): run all write statements once.
	idMap := make(map[string]int64)
	if err := execWriteBatch(ctx, ex, writeBatch, idMap, &ctr); err != nil {
		return nil, err
	}

	// Resolve idSentinels in the SELECT statement using the populated idMap.
	resolved, err := glsql.ResolveIDs(glsql.Result{Statements: []glsql.Statement{selectStmt}}, idMap)
	if err != nil {
		return nil, fmt.Errorf("graphlite: write-then-select resolve IDs: %w", err)
	}
	s := resolved.Statements[0]

	rows, err := ex.QueryContext(ctx, s.SQL, s.Args...)
	if err != nil {
		return nil, fmt.Errorf("graphlite: write-then-select query: %w", err)
	}
	qr, err := NewQueryResultFromRows(rows)
	if err != nil {
		_ = rows.Close()
		return nil, err
	}
	qr.SetCounters(ctr)
	return qr, nil
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
//
// MERGE statements (KindMergeCheck + KindMergeInsert + tagged KindUpdate) are
// handled by execMergeBatch when a KindMergeCheck statement is encountered.
func execWriteBatch(ctx context.Context, ex execer, stmts []glsql.Statement, idMap map[string]int64, ctr *QueryCounters) error {
	i := 0
	for i < len(stmts) {
		stmt := stmts[i]

		// Detect the start of a MERGE block: KindMergeCheck is always followed by
		// KindMergeInsert and then zero or more tagged KindUpdate stmts.
		if stmt.Kind == glsql.KindMergeCheck {
			consumed, err := execMergeBatch(ctx, ex, stmts[i:], idMap, ctr)
			if err != nil {
				return err
			}
			i += consumed
			continue
		}

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
			ctr.PropertiesSet += s.NumProps

		case glsql.KindInsertEdge:
			res, err := ex.ExecContext(ctx, s.SQL, s.Args...)
			if err != nil {
				return fmt.Errorf("graphlite: insert edge: %w", err)
			}
			// Capture the new edge ID in idMap when the relationship has a variable
			// name (needed for CREATE … RETURN r … queries).
			if stmt.CreatedVar != "" {
				lastID, err := res.LastInsertId()
				if err != nil {
					return fmt.Errorf("graphlite: insert edge last-id: %w", err)
				}
				idMap[stmt.CreatedVar] = lastID
			}
			ctr.RelationshipsCreated++
			ctr.PropertiesSet += s.NumProps

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
		i++
	}
	return nil
}

// execMergeBatch handles a MERGE block starting at stmts[0] (KindMergeCheck).
// It returns the number of statements consumed from the slice.
//
// Statement layout produced by translateMerge:
//
//	[0] KindMergeCheck  — SELECT id … LIMIT 1 (existence check)
//	[1] KindMergeInsert — INSERT INTO nodes … (create branch)
//	[2..n] KindUpdate with CreatedVar="oncreate:…" — ON CREATE SET stmts
//	[n+1..] KindUpdate with CreatedVar="onmatch:…"  — ON MATCH SET stmts
//
// If the check finds a row → run ON MATCH SETs (skip insert and ON CREATE SETs).
// If the check finds no row → run INSERT + ON CREATE SETs (skip ON MATCH SETs).
// All actions run within the caller's transaction (ex is already a *stdsql.Tx or
// *stdsql.DB held by the execer; the BeginTx wrapping is the caller's responsibility).
func execMergeBatch(ctx context.Context, ex execer, stmts []glsql.Statement, idMap map[string]int64, ctr *QueryCounters) (consumed int, err error) {
	if len(stmts) == 0 || stmts[0].Kind != glsql.KindMergeCheck {
		return 0, fmt.Errorf("graphlite: execMergeBatch called on non-MergeCheck statement")
	}

	checkStmt := stmts[0]
	consumed = 1 // always consume the check statement

	// Collect the insert statement and the tagged SET statements.
	var insertStmt *glsql.Statement
	var onCreateStmts []glsql.Statement
	var onMatchStmts []glsql.Statement

	for j := 1; j < len(stmts); j++ {
		s := stmts[j]
		if s.Kind == glsql.KindMergeInsert {
			insertStmt = &stmts[j]
			consumed++
		} else if s.Kind == glsql.KindUpdate && strings.HasPrefix(s.CreatedVar, "oncreate:") {
			onCreateStmts = append(onCreateStmts, s)
			consumed++
		} else if s.Kind == glsql.KindUpdate && strings.HasPrefix(s.CreatedVar, "onmatch:") {
			onMatchStmts = append(onMatchStmts, s)
			consumed++
		} else {
			// Not part of this MERGE block.
			break
		}
	}

	// ── Run the existence check ───────────────────────────────────────────────
	// Resolve any idSentinels in the check statement's args before executing.
	// This handles MERGE checks that JOIN against externally-matched nodes
	// (e.g. MATCH (person) MERGE (city {name: person.bornIn}) — the JOIN ON
	// node.id = ? uses person's id from idMap).
	resolvedCheck, err := glsql.ResolveIDs(glsql.Result{Statements: []glsql.Statement{checkStmt}}, idMap)
	if err != nil {
		return consumed, fmt.Errorf("graphlite: MERGE check resolve IDs: %w", err)
	}
	checkStmt = resolvedCheck.Statements[0]
	var existingID int64
	row := ex.QueryRowContext(ctx, checkStmt.SQL, checkStmt.Args...)
	scanErr := row.Scan(&existingID)

	nodeExists := scanErr == nil
	if scanErr != nil && !errors.Is(scanErr, stdsql.ErrNoRows) {
		// A real scan error (not "no rows").
		return consumed, fmt.Errorf("graphlite: MERGE check: %w", scanErr)
	}

	varName := checkStmt.CreatedVar // the Cypher variable for the merged node

	if nodeExists {
		// ── ON MATCH branch ───────────────────────────────────────────────────
		// The node already exists; record its ID and run ON MATCH SET stmts.
		if varName != "" {
			idMap[varName] = existingID
		}
		for _, s := range onMatchStmts {
			// Strip the "onmatch:" tag prefix to get the real variable name for
			// ID resolution, then resolve and execute.
			realVar := strings.TrimPrefix(s.CreatedVar, "onmatch:")
			s.CreatedVar = realVar
			resolved, err := glsql.ResolveIDs(glsql.Result{Statements: []glsql.Statement{s}}, idMap)
			if err != nil {
				return consumed, fmt.Errorf("graphlite: MERGE ON MATCH resolve IDs: %w", err)
			}
			if _, err := ex.ExecContext(ctx, resolved.Statements[0].SQL, resolved.Statements[0].Args...); err != nil {
				return consumed, fmt.Errorf("graphlite: MERGE ON MATCH SET: %w", err)
			}
			ctr.PropertiesSet++
		}
	} else {
		// ── ON CREATE branch ──────────────────────────────────────────────────
		// Node does not exist; insert it and run ON CREATE SET stmts.
		if insertStmt == nil {
			return consumed, fmt.Errorf("graphlite: MERGE has no INSERT statement")
		}
		// Resolve any idSentinels in the INSERT args (e.g. from MERGE props that
		// reference external matched nodes via INSERT … SELECT … JOIN).
		resolvedInsert, err := glsql.ResolveIDs(glsql.Result{Statements: []glsql.Statement{*insertStmt}}, idMap)
		if err != nil {
			return consumed, fmt.Errorf("graphlite: MERGE insert resolve IDs: %w", err)
		}
		res, err := ex.ExecContext(ctx, resolvedInsert.Statements[0].SQL, resolvedInsert.Statements[0].Args...)
		if err != nil {
			return consumed, fmt.Errorf("graphlite: MERGE insert: %w", err)
		}
		lastID, err := res.LastInsertId()
		if err != nil {
			return consumed, fmt.Errorf("graphlite: MERGE insert last-id: %w", err)
		}
		if varName != "" {
			idMap[varName] = lastID
		}
		ctr.NodesCreated++
		ctr.PropertiesSet += insertStmt.NumProps

		for _, s := range onCreateStmts {
			realVar := strings.TrimPrefix(s.CreatedVar, "oncreate:")
			s.CreatedVar = realVar
			resolved, err := glsql.ResolveIDs(glsql.Result{Statements: []glsql.Statement{s}}, idMap)
			if err != nil {
				return consumed, fmt.Errorf("graphlite: MERGE ON CREATE resolve IDs: %w", err)
			}
			if _, err := ex.ExecContext(ctx, resolved.Statements[0].SQL, resolved.Statements[0].Args...); err != nil {
				return consumed, fmt.Errorf("graphlite: MERGE ON CREATE SET: %w", err)
			}
			ctr.PropertiesSet++
		}
	}

	return consumed, nil
}
