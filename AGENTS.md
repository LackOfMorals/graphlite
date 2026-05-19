# AGENTS.md — graphlite

## Project Overview

graphlite is an embedded property graph database for Go, backed by SQLite and queryable via a subset of openCypher. The primary goal is to be a drop-in local substitute for Neo4j Aura in tests.

- Module path: `github.com/LackOfMorals/graphlite`
- Go minimum version: 1.21
- SQLite driver: `modernc.org/sqlite` (CGO-free, no mattn/go-sqlite3)
- Neo4j driver: `github.com/neo4j/neo4j-go-driver/v6/neo4j`

## Feedback Commands

### Build
```bash
CGO_ENABLED=0 go build ./...
```

### Test
```bash
CGO_ENABLED=0 go test -count=1 ./...
```

### Vet
```bash
go vet ./...
```

### Build for all target platforms (cross-compile check)
```bash
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build ./...
GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

## Package Layout

```
graphlite/
├── types.go        ← Node, Relationship, Record, error types
├── driver.go       ← graphlite.Open, native DB API
├── session.go      ← transaction primitives
├── neo4j.go        ← DriverCompat (satisfies neo4j.Driver v6)
├── importer.go     ← Import / Export
├── cypher/         ← parser, plan types, planner, BindingScope
├── sql/            ← translator + Dialect interface
├── store/          ← Store interface + SQLite implementation + DDL
├── compat/         ← TCK harness (opt-in: -tags=tck)
└── testdata/       ← .cypher fixture files
```

## Key Architectural Constraints

- The `store/` package must NEVER import Cypher types — it works with raw IDs, labels, JSON blobs only.
- The `cypher/` package must NEVER import `store/` or `sql/`.
- The `sql/` package translates `cypher.LogicalPlan` → SQL; it may import `cypher/` but not `store/`.
- All SQL must use parameterised queries — never `fmt.Sprintf` user input into SQL strings.
- CGO must remain disabled: always use `modernc.org/sqlite`, never `mattn/go-sqlite3`.

## Storage Schema

```sql
CREATE TABLE nodes (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    labels TEXT    NOT NULL DEFAULT '',
    props  JSON    NOT NULL DEFAULT '{}'
);
CREATE TABLE edges (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    type     TEXT    NOT NULL,
    start_id INTEGER NOT NULL REFERENCES nodes(id),
    end_id   INTEGER NOT NULL REFERENCES nodes(id),
    props    JSON    NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_nodes_labels ON nodes(labels);
CREATE INDEX idx_edges_start  ON edges(start_id);
CREATE INDEX idx_edges_end    ON edges(end_id);
CREATE INDEX idx_edges_type   ON edges(type);
```

WAL mode is enabled via `PRAGMA journal_mode=WAL` on every open.

## Neo4j Driver Compatibility

- Target: `github.com/neo4j/neo4j-go-driver/v6/neo4j`
- v6 dropped the `WithContext` suffix — use `neo4j.Driver`, NOT `neo4j.DriverWithContext`
- Auth is accepted and silently ignored
- `DatabaseName` in `SessionConfig` is accepted and ignored (single-database)
- All three transaction tiers must work: `ExecuteQuery`, managed (`ExecuteRead`/`ExecuteWrite`), explicit (`BeginTransaction`)
- Compile-time interface assertion: `var _ neo4j.Driver = (*DriverCompat)(nil)`

## Gotchas and Learnings

- The `compat/tck_test.go` file is a test file in a non-`_test` package; it uses build tag `tck` to opt-in.
- `modernc.org/sqlite` requires no build tags — it is CGO-free by default.
- Labels stored as comma-separated text in the `labels` column; multi-label MATCH requires ALL labels present (AND semantics, not OR).
- Use `json_extract(props, '$.key')` for property access in SQLite queries.
- The `BindingScope` in `cypher/scope.go` is the most critical data structure — bugs here cause incorrect SQL for any multi-clause query.
- `Record` uses unexported fields with defensive copies — callers cannot mutate internal state. `NewRecord` panics on key/value length mismatch (programmer error).
- All error types use pointer receivers (`*ErrFoo`) so `errors.As` works correctly when errors are wrapped with `fmt.Errorf("...: %w", err)`.
- `modernc.org/sqlite v1.50.1` requires Go 1.25.0; use `v1.35.0` to stay on Go 1.23.0. Running `go get modernc.org/sqlite@latest` will silently bump the `go` directive — pin the version explicitly.
- In-memory SQLite DBs report `journal_mode = "memory"` even after `PRAGMA journal_mode=WAL` — WAL requires a file. File-based DBs correctly report `"wal"`.
- The `store` package uses a `querier` interface (ExecContext/QueryContext/QueryRowContext) to share CRUD helpers between `*sql.DB` and `*sql.Tx` — avoids duplicating all methods on `sqliteTx`.
- `modernc.org/sqlite` is a direct import in `store/sqlite.go`; declare it as a direct (non-indirect) dependency in `go.mod`.
- Use `EXPLAIN QUERY PLAN SELECT ... WHERE labels = ?` to assert that `idx_nodes_labels` is used; scan the `detail` column (4th column) for the index name.
- Assigning to a map inside a `range` loop (e.g. `m[k] = v`) is considered a "use" by Go; the compiler will not flag the variable as unused even if values are never read back.
- `store_test.go` covers schema/WAL/lifecycle; `crud_test.go` covers all CRUD and transaction tests — split for readability.
- `cloudprivacylabs/opencypher` is an evaluator library, not a pure AST library — its higher-level types (`singlePartQuery`, `nodePattern`, etc.) have unexported fields and are opaque externally. Walk the ANTLR CST directly via `opencypher.GetParser()` and the `parser.*Context` types instead.
- In `cloudprivacylabs/opencypher` grammar, SKIP must precede LIMIT in RETURN clauses (`RETURN ... SKIP 5 LIMIT 10`), unlike some other Cypher dialects. Standard openCypher allows LIMIT first — our translator must not assume ordering.
- `MATCH (a), (b)` is one `MatchClause` with two `PatternPart` entries, not two separate `MatchClause` nodes. The comma-separated patterns are parsed inside a single `OC_Pattern`.
- `OC_PatternElement` can nest inside itself for parenthesized patterns; loop `for elemCtx.OC_PatternElement() != nil { elemCtx = ... }` to unwrap.
- The `"$"` sentinel key in `NodePattern.Props` encodes a whole-properties parameter reference (`{$param}`). Cypher identifiers cannot start with `$` so this key never collides with a real property name.
- `BindingScope.Bind` takes a `Binding` struct (not separate alias/column args) — the struct carries IsNode, IsRel, IsNullable flags needed by the translator and optional-match planner.
- `LogicalPlan` and `Expr` both use sealed interfaces (unexported `planNode()`/`exprNode()` methods) — type switches are the dispatch mechanism; never use reflect.
- `MatchRelPlan.EndNode` is an embedded `MatchNodePlan` (not just `EndVar string`) so the translator can apply destination node label/property constraints without rescanning the scope.
- WITH pipeline scoping: use a fresh `NewScope()` for the next stage (not `Child()`) — only projected variables are re-bound. `Child()` inherits everything from the parent; WITH explicitly limits visibility.
- `RawExpr{Text}` is a fallback for unsupported sub-expressions; task-008 replaced raw WHERE strings with typed `ComparisonExpr`/`BoolExpr`/`NotExpr` trees built directly from the ANTLR CST in the parser.
- `MatchClause.Where` is a typed `Expr` (not a raw string); the parser builds the tree via `buildExprFromCST` walking `OC_Expression → OC_OrExpression → ... → OC_Atom`.
- Operator extraction from `OC_PartialComparisonExpressionContext` uses `strings.HasPrefix(ctx.GetText(), ...)` — `GetText()` strips whitespace, so `<>` must be tested before `<` to avoid false matches.
- Package-level unused consts/functions are not flagged by `go vet`; review for dead code before finalizing.
- Only one file in a package should have a package-level doc comment (the `package foo` comment with a `//` block above it); duplicating it in other files causes `go doc` to show it twice.
- `Plan(q *Query, scope *BindingScope)` is the planner entry point in `cypher/planner.go`; the scope is mutated in place and populated with all named variables.
- `aliasCounter` in the planner hands out `n0/n1/r0/r1` SQL table aliases; the translator resolves variable→alias via BindingScope, not by inspecting plan node order.
- Multi-hop MATCH patterns produce a `SequencePlan{Steps: []MatchRelPlan{...}}` — one `MatchRelPlan` per hop; the translator emits one SQL JOIN per step.
- `planNodePatternNewAlias` is a package-level `var` aliasing `planNodePattern` — the two functions were identical (only the name differed for call-site clarity).
- `parseExprText` in `planner.go` converts raw expression strings to typed `Expr` nodes; unrecognised forms fall back to `RawExpr`. It does NOT handle negative literals (e.g. `-42` → `RawExpr`).
- `planCreateClause` in `planner.go` was implemented in task-007 alongside the MATCH planner; task-010 only added unit tests. For a 3-node chain `CREATE (a)-[:E1]->(b)-[:E2]->(c)` the step order is: node(a), node(b), rel(a→b), node(c), rel(b→c) — end node is emitted before its outgoing relationship at each hop.
- `planSetClause` and `planDeleteClause` were also implemented in task-007; task-011 only added unit tests. `planDeleteClause` dispatches on scope binding: `IsRel=true` → `DeleteRelPlan`; otherwise `DeleteNodePlan{Detach: dc.Detach}`.
- In compound MATCH+WHERE+DELETE plans the SequencePlan layout is `[FilterPlan{Source: MatchNodePlan}, ..., DeleteNodePlan]`. Test against `seq.Steps[0]` being a `*FilterPlan` and `seq.Steps[len-1]` being a `*DeleteNodePlan`.
- When writing MATCH+CREATE tests, search for plan nodes by type in the SequencePlan steps (loop + type-assert) rather than hard-coding step indices — the exact sequence shape can be flat or nested depending on how many MATCH clauses precede the CREATE.
- `ErrUnsupportedCypher` lives in the root package; the `cypher/` package cannot import it (circular dep) — use `fmt.Errorf` for unsupported-construct errors in `cypher/`.
- `sql.Dialect` is the extension point for future backends; `SQLiteDialect` is the only implementation at v1.0. It is stateless (value receiver on `struct{}`) and zero-value constructible.
- `sql.Dialect` includes `JSONSet` and `JSONRemove` beyond the task-012 acceptance criteria — they are needed by task-014 for `SET n.prop = val` and future `REMOVE n.prop` SQL emission.
- `sql.SQLiteDialect.LabelContains` emits four OR LIKE branches — the same pattern as `store.listNodesByLabel`. Returns `[]any` with four copies of the label value; the caller appends them to the SQL args slice in order.
- Test files in the `sql/` package use `package sql_test` (black-box) with import alias `sqldialect` to avoid collision with the stdlib `"database/sql"` package name.
- Use `strings.Builder` (not string concatenation `+=`) when building SQL fragments character-by-character — avoids O(n²) allocations.
- `MatchRelPlan.StartNode` (added task-013) mirrors `EndNode` — both are embedded `MatchNodePlan` values so the translator applies label/prop constraints for both start and end nodes without re-inspecting the AST.
- `paramSentinel{Name string}` (unexported in `sql/`) is stored in `Result.Args` for `$param` references; task-015 replaces these with actual values. Tests verify sentinel presence by checking the arg is not a `string`.
- Map iteration over `Props` (map[string]Expr) is non-deterministic — WHERE fragment order for multi-prop inline constraints is not guaranteed. Write tests with at most one prop per map, or sort keys before iterating if deterministic output is required.
- `MatchRelPlan.StartNode.Labels`/`Props` will be empty when the start variable was already in scope before the hop (MATCH+MATCH re-use); start-node constraints are silently dropped in that case. Fine for v0.1 read queries, but latent issue for multi-clause MATCH patterns.
- `translateStandaloneFilter` uses `append(append([]string(nil), fc.whereFragments...), predSQL)` to avoid aliasing the source slice's backing array.
- `Result.Statements []Statement` (added task-014) carries all SQL statements in execution order. Read queries always produce `len(Statements)==1`; write queries may produce 2+ (e.g. DETACH DELETE = edges delete + node delete). The top-level `Result.SQL`/`Result.Args` mirror `Statements[0]` for backward compatibility.
- `idSentinel{VarName, Alias}` (unexported in `sql/`) occupies Args positions where an int64 node/rel ID is needed. The execution layer (task-016) resolves each sentinel to an actual ID. Test files detect sentinels by type-asserting the arg is not a plain `string`.
- `buildPropsJSON` in `sql/translator.go` sorts property keys before generating the SQL fragment — required for deterministic output since `map[string]Expr` iteration order is undefined.
- `translateCreateRel` validates that start/end scope bindings are nodes (`IsNode=true`), not relationships — catches planner bugs early at translation time.
- Non-detach `DELETE n` produces two Statements: `[0]` guard `SELECT COUNT(*) FROM edges WHERE start_id=? OR end_id=?`, `[1]` `DELETE FROM nodes WHERE id=?`. The execution layer must abort and return an error if statement[0] returns count > 0.
- Write-plan translation (`translateWritePlan`) is attempted before read-plan translation in `Translate()`. `SequencePlan` containing only read steps (MATCH, FILTER) falls through to the read path; sequences containing any write step are handled as write sequences.
- `sql.BindParams(result Result, params map[string]any) (Result, error)` resolves all `paramSentinel` values in `Result.Statements` to concrete values. It never mutates the original `Result` — fresh slices are always allocated. Call it after `Translate()` and before executing statements.
- `sql.ErrMissingParam{Name string}` is returned by `BindParams` when a required `$paramName` is absent. The task-016 execution layer should wrap it as `*graphlite.ErrMissingParameter` when returning to public API callers.
- `idSentinel` values in write-operation `Args` are intentionally preserved by `BindParams` — they are resolved by the execution layer (task-016) using last-insert-rowid or scope lookup, not via the params map.
- nil params map is safe in `BindParams`: Go map lookup on a nil map returns `(nil, false)`, so any `paramSentinel` triggers `ErrMissingParam` rather than a panic.
- `QueryResult` wraps `*sql.Rows` directly for lazy streaming; `Next(ctx)` scans one row per call via `rows.Scan`.
- `mapColumnValue` detects whole-node vs whole-relationship JSON by checking for the presence/absence of specific keys (`"type"` distinguishes rels from nodes). No type sentinel column is needed.
- SQLite's `json()` function returns nested JSON as an embedded object, NOT as a double-encoded string. `"props":{"name":"Alice"}` is correct; `"props":"{\"name\":\"Alice\"}"` is wrong. Tests must use the nested-object form.
- `QueryCounters` (exported) is the public API for setting write-operation counters; `queryCounters` (unexported) is the internal storage. `SetCounters(QueryCounters)` bridges them.
- `go.mod` `go` directive was bumped to `1.24` when the neo4j driver was added via `go get`; modernc.org/sqlite v1.35.0 still builds at go 1.24. The directive is a minimum, not a maximum.
- `NewQueryResultFromRows`, `NewEagerResult`, `MapColumnValue`, `SplitLabels` are exported for black-box tests and for use by task-016/018/019. Internal versions (`newQueryResult`, `newEagerResult`, `mapColumnValue`, `splitLabels`) remain unexported.
- `store.Open` calls `db.SetMaxOpenConns(1)` — CRITICAL for correctness. Without it, `:memory:` SQLite gives each pool connection a separate database; subsequent queries on the same `*DB` see empty tables.
- `sql.Statement` carries a `Kind StatementKind` field — use this in the execution layer instead of string-prefix matching SQL. `BindParams` and `ResolveIDs` MUST copy `Kind`, `CreatedVar`, and `MatchedVars` when creating new `Statement` values, or Kind defaults to 0 (`KindSelect`) and CREATEs silently return lazy cursors.
- `KindMatchForWrite` SELECT is emitted as the first statement in a MATCH+write sequence. The executor must drain it into memory (`collectMatchRows`) BEFORE running any write statement — SQLite's single connection cannot hold an open `*sql.Rows` cursor and a concurrent write.
- `Tx` in `session.go` wraps `*sql.Tx` directly (obtained via `d.st.DB().BeginTx()`), not `store.Tx`. This avoids an adapter and gives the execution layer direct access to the transaction.
- `Consume` and `Collect` on `QueryResult` must guard against `r.rows == nil` — write results have `rows: nil` and `consumed: true` at construction; calling `rows.Close()` on nil panics.
- `neo4j.Session` and `neo4j.Result` interfaces have **unexported methods** (`executeQueryRead`, `executeQueryWrite`, `getServerInfo`, `verifyAuthentication`, `buffer`, `errorHandler`). They CANNOT be implemented from outside the `neo4j` package directly. The only workaround is to embed the interface itself (`neo4j.Session` or `neo4j.Result`) in your struct — the nil embedded value satisfies the unexported method requirements at compile time (panics if called at runtime via the nil path, but the public override methods prevent that for normal usage paths).
- `neo4j.Driver` CAN be satisfied from outside the package (all its methods are public). Only `Session` and `Result` require the embedding trick.
- `neo4j.ExplicitTransaction` has only public methods (`Run`, `Commit`, `Rollback`, `Close`) and can be implemented directly without embedding.
- `compatExplicitTx.Close` must check `tx.done` before rolling back — calling Rollback on an already-done Tx returns an error. Guard: `if e.tx.done { return nil }`.
- `neo4j.Node` and `neo4j.Relationship` are type aliases for `dbtype.Node`/`dbtype.Relationship`. Their properties field is `Props map[string]any` (NOT `Properties`). Use `neo4j.Node{ElementId: ..., Labels: ..., Props: ...}`.
- `notifications.Unknown` is the correct constant for unknown classification (not `notifications.UnknownClassification`). `notifications.UnknownSeverity` for severity.
- Only one file per package should have a `// Package foo ...` doc comment. Use `// This file contains ...` for secondary files to avoid `go doc` showing duplicate package docs.
- `buildSelectList` adds implicit aliases: `VarExpr` → variable name; `PropExpr` → `var_name` (underscore, not dot — dot is not a valid unquoted SQL identifier). Explicit `AS alias` always wins.
- `buildMatchForWriteSelect` sorts `scope.Names()` before building columns for deterministic SQL output (map iteration is unordered).
