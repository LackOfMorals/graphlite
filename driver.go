// Package graphlite is a zero-infrastructure embedded property graph database
// for Go.
//
// graphlite stores a labelled property graph in a local SQLite file and
// accepts queries written in openCypher. There is no external process to run,
// no driver dependency, and no network — just open a file and query.
//
// # Quick start
//
//	db, err := graphlite.Open(":memory:")
//	result, err := db.RunQuery(ctx, `MATCH (n:Person) RETURN n.name AS name`, nil)
//	for result.Next(ctx) {
//	    fmt.Println(result.Record().Values()[0])
//	}
//
// For explicit transaction control:
//
//	tx, err := db.BeginTx(ctx)
//	result, err := tx.Run(ctx, `CREATE (n:Person {name: $name})`, map[string]any{"name": "Alice"})
//	err = tx.Commit()
//
// In tests, use [NewTestDB] to open an in-memory database that is closed
// automatically when the test ends:
//
//	db := graphlite.NewTestDB(t)
//
// # Options
//
// Pass functional options to [Open] to tune behaviour:
//
//	db, err := graphlite.Open("graph.db",
//	    graphlite.WithBusyTimeout(5*time.Second),
//	    graphlite.WithReadOnly(),
//	)
package graphlite

import (
	"context"
	stdsql "database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/LackOfMorals/graphlite/cypher"
	glsql "github.com/LackOfMorals/graphlite/sql"
	"github.com/LackOfMorals/graphlite/store"
)

// DB is an open graphlite database. All methods are safe for concurrent use
// from multiple goroutines.
type DB struct {
	st          store.Store
	readOnly    bool
	maxPathHops int
	cache       *planCache // bounded LRU cache for parse→plan→translate results
}

// Open opens (or creates) a graphlite database at path and returns a *DB.
//
// Use ":memory:" for a transient in-memory database. A file path (absolute or
// relative) opens (or creates) a persistent SQLite file. Pass Option values to
// customise behaviour — see WithBusyTimeout and WithReadOnly.
//
// Open applies the schema DDL and enables WAL journal mode before returning
// (skipped when WithReadOnly is set — the schema must already exist).
//
// Path traversal protection: if path is not ":memory:", Open rejects paths
// whose resolved form contains ".." components and resolves symlinks in the
// parent directory to prevent directory traversal via both ".." sequences and
// symlinks (e.g. "../../etc/passwd" and symlinks pointing outside the working
// tree are both rejected).
func Open(path string, opts ...Option) (*DB, error) {
	cfg := &dbConfig{}
	for _, o := range opts {
		o(cfg)
	}

	if path != ":memory:" {
		cleaned := filepath.Clean(path)
		// Resolve symlinks in the parent directory to catch escapes via
		// symbolic links before applying the ".." component check.
		if dir, err := filepath.EvalSymlinks(filepath.Dir(cleaned)); err == nil {
			cleaned = filepath.Join(dir, filepath.Base(cleaned))
		}
		if slices.Contains(strings.Split(cleaned, string(filepath.Separator)), "..") {
			return nil, fmt.Errorf("graphlite: Open: path traversal not allowed: %q", path)
		}
	}

	st, err := store.Open(path, store.Config{
		BusyTimeout: cfg.busyTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("graphlite: open %q: %w", path, err)
	}
	return &DB{
		st:          st,
		readOnly:    cfg.readOnly,
		maxPathHops: cfg.maxPathHops,
		cache:       newPlanCache(planCacheMaxSize),
	}, nil
}

// Snapshot writes an atomic, consistent copy of the database to path.
// path must not already exist. The resulting file is a valid SQLite database
// that can be opened with [Open]. Works on both file-backed and in-memory
// databases — snapshotting an in-memory database is useful to persist its
// state before it is discarded.
//
// Returns an error if the backend does not support snapshots.
func (d *DB) Snapshot(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("graphlite: snapshot: %q already exists", path)
	}
	sn, ok := d.st.(store.Snapshotter)
	if !ok {
		return fmt.Errorf("graphlite: snapshot: not supported by this backend")
	}
	return sn.Snapshot(path)
}

// Close releases all resources held by the database. Subsequent calls on a
// closed DB return errors.
func (d *DB) Close(_ context.Context) error {
	if err := d.st.Close(); err != nil {
		return fmt.Errorf("graphlite: close: %w", err)
	}
	return nil
}

// RunQuery executes cypherStr in auto-commit mode and returns a lazy
// Result cursor. The caller must consume or exhaust the result to release
// underlying resources.
//
// params may be nil if the query has no parameters.
// Returns ErrReadOnly if the database was opened with WithReadOnly and the
// query contains write statements.
func (d *DB) RunQuery(ctx context.Context, cypherStr string, params map[string]any) (*Result, error) {
	if d.readOnly {
		sqlResult, err := buildSQLResult(cypherStr, params, d.maxPathHops, d.cache)
		if err != nil {
			return nil, err
		}
		for _, s := range sqlResult.Statements {
			if s.Kind != glsql.KindSelect {
				return nil, ErrReadOnly
			}
		}
		return executeStatements(ctx, d.st.Exec(), sqlResult, nil)
	}
	return runQuery(ctx, d.st.Exec(), cypherStr, params, d.st.BeginExecTx, d.maxPathHops, d.cache)
}

// BeginTx starts an explicit transaction and returns a *Tx.
//
// Returns [ErrReadOnly] if the database was opened with [WithReadOnly]; use
// [DB.RunQuery] for read-only access.
func (d *DB) BeginTx(ctx context.Context) (*Tx, error) {
	if d.readOnly {
		return nil, ErrReadOnly
	}
	txEx, err := d.st.BeginExecTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("graphlite: begin transaction: %w", err)
	}
	return &Tx{rawTx: txEx, maxPathHops: d.maxPathHops, cache: d.cache}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Core execution pipeline (shared by auto-commit and transactional paths)
// ─────────────────────────────────────────────────────────────────────────────

// execer is an alias for store.Execer used throughout the execution helpers.
// Any *sql.DB, *sql.Tx, or store.TxExecer satisfies this interface.
type execer = store.Execer

// runQuery is the execution pipeline for auto-commit mode.
// beginTxFn is used to start an implicit transaction for atomic MERGE operations.
func runQuery(ctx context.Context, ex execer, cypherStr string, params map[string]any, beginTxFn func(context.Context) (store.TxExecer, error), maxPathHops int, cache *planCache) (*Result, error) {
	sqlResult, err := buildSQLResult(cypherStr, params, maxPathHops, cache)
	if err != nil {
		return nil, err
	}
	return executeStatements(ctx, ex, sqlResult, beginTxFn)
}

// runQueryTx is the execution pipeline for transactional mode. beginTxFn is
// nil because the caller already holds an open transaction.
func runQueryTx(ctx context.Context, ex execer, cypherStr string, params map[string]any, maxPathHops int, cache *planCache) (*Result, error) {
	sqlResult, err := buildSQLResult(cypherStr, params, maxPathHops, cache)
	if err != nil {
		return nil, err
	}
	return executeStatements(ctx, ex, sqlResult, nil)
}

// buildSQLResult runs parse → plan → translate → bind-params, returning the
// bound Result ready for execution.
//
// When cache is non-nil, the parse→plan→translate result (unbound, containing
// paramSentinel values) is cached keyed on cypherStr. On a cache hit the three
// expensive steps are skipped and only BindParams is called. The cached Result
// is read-only after insertion and is never mutated, so sharing it across
// goroutines is safe.
func buildSQLResult(cypherStr string, params map[string]any, maxPathHops int, cache *planCache) (glsql.Result, error) {
	var (
		unbound   glsql.Result
		cacheHit  bool
	)

	if cache != nil {
		unbound, cacheHit = cache.get(cypherStr)
	}

	if !cacheHit {
		// Cache miss (or no cache): run the full pipeline.

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
		translator := glsql.NewTranslator(glsql.SQLiteDialect{}, glsql.WithMaxPathHops(maxPathHops))
		unbound, err = translator.Translate(plan, scope)
		if err != nil {
			return glsql.Result{}, fmt.Errorf("graphlite: translate: %w", err)
		}

		// Store the unbound result in the cache for future hits.
		if cache != nil {
			cache.put(cypherStr, unbound)
		}
	}

	// Step 4: bind named parameters (always executed — params differ per call).
	if params == nil {
		params = map[string]any{}
	}
	sqlResult, err := glsql.BindParams(unbound, params)
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

// executeStatements runs all Statements in sqlResult against ex. For a single
// KindSelect it returns a lazy Result; for write statements it executes
// each in order and returns a Result with counters.
//
// beginTxFn, when non-nil, is called to start an implicit transaction for
// atomic MERGE operations. Pass nil when ex is already transaction-scoped.
func executeStatements(ctx context.Context, ex execer, sqlResult glsql.Result, beginTxFn func(context.Context) (store.TxExecer, error)) (*Result, error) {
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
		qr, err := newResultFromRows(rows)
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

	// If the statement list contains a MERGE check and we have a beginTxFn,
	// wrap the execution in an implicit transaction for atomicity.
	hasMerge := false
	for _, s := range stmts {
		if s.Kind == glsql.KindMergeCheck {
			hasMerge = true
			break
		}
	}
	if hasMerge && beginTxFn != nil {
		txEx, err := beginTxFn(ctx)
		if err != nil {
			return nil, fmt.Errorf("graphlite: MERGE begin tx: %w", err)
		}
		qr, err := execWriteStatements(ctx, txEx, stmts)
		if err != nil {
			_ = txEx.Rollback()
			return nil, err
		}
		if err := txEx.Commit(); err != nil {
			return nil, fmt.Errorf("graphlite: MERGE commit: %w", err)
		}
		return qr, nil
		// beginTxFn == nil: already in a transaction, fall through to execWriteStatements.
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
func execWriteThenSelect(ctx context.Context, ex execer, stmts []glsql.Statement) (*Result, error) {
	selectStmt := stmts[len(stmts)-1]
	writeStmts := stmts[:len(stmts)-1]
	var ctr queryCounters

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

		// For OPTIONAL MATCH with no rows found, produce one null row by
		// synthesizing a null record from the final SELECT's column structure.
		if len(matchedRows) == 0 && matchStmt.Optional {
			nullRec, keys, err := buildOptionalNullRow(ctx, ex, selectStmt)
			if err != nil {
				return nil, err
			}
			qr := newInMemoryResult(keys, []*Record{nullRec})
			qr.setCounters(ctr)
			return qr, nil
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
			rowQR, err := newResultFromRows(rows)
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

		qr := newInMemoryResult(resultKeys, allRecords)
		qr.setCounters(ctr)
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
	qr, err := newResultFromRows(rows)
	if err != nil {
		_ = rows.Close()
		return nil, err
	}
	qr.setCounters(ctr)
	return qr, nil
}

// execWriteStatements runs write statements, resolving idSentinels between steps.
//
// The statement sequence may begin with a KindMatchForWrite SELECT that returns
// matched variable IDs row by row. When encountered, we execute all subsequent
// write statements once per matched row, accumulating counters across all rows.
func execWriteStatements(ctx context.Context, ex execer, stmts []glsql.Statement) (*Result, error) {
	// idMap: Cypher variable name → int64 row ID from INSERT or MATCH SELECT.
	idMap := make(map[string]int64)
	var ctr queryCounters

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

	qr := &Result{consumed: true}
	qr.setCounters(ctr)
	return qr, nil
}

// buildOptionalNullRow derives the column names from a KindSelectAfterWrite
// statement by executing it with a WHERE-never-true rewrite, then returns a
// single *Record with all-nil values (representing the OPTIONAL MATCH null row).
func buildOptionalNullRow(ctx context.Context, ex execer, stmt glsql.Statement) (*Record, []string, error) {
	// Rewrite the SQL: replace the last WHERE clause with "WHERE 0=1" to get
	// the column structure without running the real query (avoids resolving
	// idSentinels which would be absent for the unmatched optional row).
	sql := stmt.SQL
	upperSQL := strings.ToUpper(sql)
	if idx := strings.LastIndex(upperSQL, " WHERE "); idx >= 0 {
		sql = sql[:idx] + " WHERE 0=1"
	} else {
		sql += " WHERE 0=1"
	}
	rows, err := ex.QueryContext(ctx, sql)
	if err != nil {
		return nil, nil, fmt.Errorf("graphlite: optional null row: %w", err)
	}
	cols, err := rows.Columns()
	_ = rows.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("graphlite: optional null row columns: %w", err)
	}
	vals := make([]any, len(cols))
	return newRecord(cols, vals), cols, nil
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
		_ = rows.Close()
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
			_ = rows.Close()
			return nil, nil, fmt.Errorf("graphlite: match-for-write scan: %w", err)
		}
		row := make([]any, len(cols))
		copy(row, vals)
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, fmt.Errorf("graphlite: match-for-write iterate: %w", err)
	}
	_ = rows.Close()
	return result, cols, nil
}

// execWriteBatch runs one "batch" of write statements with the provided idMap.
// idMap is updated in place as INSERT statements produce new row IDs.
//
// MERGE statements (KindMergeCheck + KindMergeInsert + tagged KindUpdate) are
// handled by execMergeBatch when a KindMergeCheck statement is encountered.
func execWriteBatch(ctx context.Context, ex execer, stmts []glsql.Statement, idMap map[string]int64, ctr *queryCounters) error {
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
			ctr.nodesCreated++
			ctr.propertiesSet += s.NumProps

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
			ctr.relationshipsCreated++
			ctr.propertiesSet += s.NumProps

		case glsql.KindUpdate:
			if _, err := ex.ExecContext(ctx, s.SQL, s.Args...); err != nil {
				return fmt.Errorf("graphlite: update: %w", err)
			}
			ctr.propertiesSet++

		case glsql.KindDeleteEdges:
			res, err := ex.ExecContext(ctx, s.SQL, s.Args...)
			if err != nil {
				return fmt.Errorf("graphlite: delete edges: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("graphlite: delete edges rows affected: %w", err)
			}
			ctr.relationshipsDeleted += int(n)

		case glsql.KindDeleteNodes:
			res, err := ex.ExecContext(ctx, s.SQL, s.Args...)
			if err != nil {
				return fmt.Errorf("graphlite: delete node: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("graphlite: delete nodes rows affected: %w", err)
			}
			ctr.nodesDeleted += int(n)

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
// All actions run within the caller's execer scope (either a TxExecer already
// in a transaction, or a plain Execer when beginTxFn wraps the MERGE).
func execMergeBatch(ctx context.Context, ex execer, stmts []glsql.Statement, idMap map[string]int64, ctr *queryCounters) (consumed int, err error) {
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
			ctr.propertiesSet++
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
		ctr.nodesCreated++
		ctr.propertiesSet += insertStmt.NumProps

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
			ctr.propertiesSet++
		}
	}

	return consumed, nil
}
