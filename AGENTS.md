# AGENTS.md — graphlite

## Project Overview

graphlite is an embedded property graph database for Go, backed by SQLite and queryable via a subset of openCypher. The primary entry point is `graphlite.Open`; queries are executed with `db.RunQuery` or via explicit transactions started with `db.BeginTx`.

- Module path: `github.com/LackOfMorals/graphlite`
- Go minimum version: 1.24
- SQLite driver: `modernc.org/sqlite` (CGO-free, no mattn/go-sqlite3)

## Feedback Instructions

### Build
```bash
CGO_ENABLED=0 go build ./...
```

### Test (unit)
```bash
CGO_ENABLED=0 go test -tags=unit -count=1 ./...
```

### Test (all, excluding tck)
```bash
CGO_ENABLED=0 go test -count=1 ./...
```

### Vet
```bash
go vet ./...
```

## Package Layout

```
graphlite/
├── types.go        ← Node, Relationship, Record, error types
├── driver.go       ← graphlite.Open, DB, RunQuery, BeginTx
├── interfaces.go   ← exported interfaces (Driver, Session, Transaction, Result, …)
├── session.go      ← session, managedTx, Tx concrete types
├── result.go       ← QueryResult / Result cursor implementation
├── importer.go     ← Import / Export helpers
├── migrate.go      ← neo4j migration helpers (to be removed in v2)
├── neo4jadapter/   ← neo4j DriverCompat (to be removed in v2)
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
CREATE TABLE node_labels (
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    label   TEXT    NOT NULL,
    UNIQUE (node_id, label)
);
-- idx_node_labels_label ON node_labels(label, node_id) — O(log n) label lookups
```

WAL mode is enabled via `PRAGMA journal_mode=WAL` on every open.
`node_labels` is kept in sync automatically by SQLite triggers on nodes INSERT/UPDATE; label lookups use EXISTS subquery or JOIN against node_labels rather than LIKE on nodes.labels.

## Gotchas and Learnings

- `store.Open` calls `db.SetMaxOpenConns(1)` — CRITICAL. Without it, `:memory:` SQLite gives each pool connection a separate database.
- `Consume` and `Collect` on `QueryResult` must guard against `r.rows == nil` — write results have `rows: nil` and `consumed: true`.
- `NewRecord` panics on key/value length mismatch (programmer error).
- `KindMatchForWrite` SELECT must be drained into memory before any write statement — SQLite's single connection cannot hold an open `*sql.Rows` cursor and a concurrent write.
- Labels are stored as comma-separated text in the `labels` column.
- `json_extract(props, '$.key')` is used for property access in SQLite queries.
- `go.mod` `go` directive is 1.24; `modernc.org/sqlite v1.35.0` builds at go 1.24.
- `testdata/` package is excluded from `./...` by Go design. Run explicitly: `CGO_ENABLED=0 go test github.com/LackOfMorals/graphlite/testdata`.
- The vendor shim for neo4j (`scripts/apply-vendor-shim.sh`) is needed only when `vendor/` is present and the neo4j driver is a dependency. In v2, both are removed.
- Only one file per package should have a `// Package foo ...` doc comment.
- `buildPropsJSON` sorts property keys for deterministic SQL output.
- `buildMatchForWriteSelect` sorts `scope.Names()` before building columns for deterministic SQL.
- When deleting files that export methods used in `example_test.go`, also remove the corresponding `Example*` functions — otherwise `go build ./...` fails even if core tests pass.
- neo4j driver fully removed in task-010 via `go mod tidy` + `go mod vendor` (both needed — the repo uses a vendor dir; `go build` fails with "inconsistent vendoring" if only tidy is run).
- `Tx` type lives in `tx.go` (moved from session.go in task-003); context params on Commit/Rollback/Close were removed in task-005 — all were blank identifiers so no behavior changed.
- `DB.Close` still takes `context.Context`; only `Tx` methods are context-free.
- `//go:build ignore` example files (examples/getting_started, examples/neo4j_roundtrip) use deleted v1 APIs and are not compiled by `go build ./...` — they will be rewritten in task-012.
- `interfaces.go` is deleted in v2; all session-layer/compat interfaces (Driver, Session, Transaction, ResultTransformer, etc.) are gone.
- When replacing `NewEagerResult(ctx, qr)` calls, use `qr.Collect(ctx)` to get records directly — no intermediate struct needed.
- `QueryResult` is renamed to `Result` (task-004); `NewQueryResultFromRows` → `NewResultFromRows`; `newInMemoryQueryResult` → `newInMemoryResult`. `NewResultFromRows` is still exported until task-007 unexports it.
- When a rename touches test files that use `NewQueryResultFromRows` via dot-import, update those call sites mechanically in the same task to keep the unit test suite green.
- `ErrNoRecords` and `ErrMultipleRecords` are `fmt.Errorf` sentinels (consistent with `ErrReadOnly`); `errors.Is` works via pointer equality.
- `(*Result).Single()` uses `Consume()` to close the cursor in all paths. In the `ErrMultipleRecords` path, drain/close errors from `Consume()` are intentionally discarded (secondary to the primary sentinel); documented with a comment.
- Task-009 adds test coverage for `Single`, `ErrNoRecords`, and `ErrMultipleRecords` — but task-007 already added it in `result_test.go`.
- All formerly-exported internal helpers are now unexported (task-007): `newResultFromRows`, `newRecord`, `setCounters`, `queryCounters`. `MapColumnValue` and `SplitLabels` wrappers are deleted entirely.
- `driver.go` increments `queryCounters` fields directly (e.g. `ctr.nodesCreated++`); no exported `QueryCounters` struct exists.
- `result_test.go` and `types_test.go` use `graphlite.Open` + `db.RunQuery` to construct test fixtures — no raw `*sql.Rows` or `newResultFromRows` in tests.
- `testdata/integration_test.go` and `compat/tck_test.go` still reference `NewEagerResult` (removed in task-003) — they are out-of-scope for `go build ./...` and will be fixed in task-009.
- `helpers.go` adds `PropertyValue`, `RecordValue`, `GetProperty[T]`, `GetRecordValue[T]`, `CollectT[T]`, `SingleT[T]`. The unexported `propsGetter` interface is implemented by `*Node` and `*Relationship` via `getProps()` methods added to `types.go`-adjacent declarations in `helpers.go`.
- `convertTo[T]` uses `any(zero).(type)` type-switch (not reflection) to coerce JSON-decoded values; SQLite returns JSON numbers as `float64`, so `toInt64` converts `float64→int64` via truncation.
- `graphlite.EagerResult` and `graphlite.NewEagerResult` are deleted in v2. Test packages that need eager collection define a local `eagerResult` struct + `collectResult` helper using `qr.Collect(ctx)` followed by `qr.Consume(ctx)` (idempotent) to get counters.
- `types.go` had a second `// Package graphlite ...` doc block (v1-era text referencing Neo4j Aura); it was removed in task-011. Only `driver.go` carries the package doc comment.
- `testdata/integration_test.go` and `compat/tck_test.go` both define their own `eagerResult`/`collectResult` — they are separate packages and cannot share a common helper without a new exported type.
- `DB.Close` still takes `context.Context` (only `Tx` methods are context-free); any test calling `db.Close()` without args must be fixed to `db.Close(context.Background())`.
- `github.com/antlr/antlr4/runtime/Go/antlr` is locked to the 2021 pseudo-version and CANNOT be upgraded: `cloudprivacylabs/opencypher@v1.0.0`'s generated parser calls `DeserializeFromUInt16`, which was removed in antlr4-go v1.4.10. No newer opencypher release exists that uses the updated `github.com/antlr4-go/antlr/v4` module path.
- `golang.org/x/sys` is pinned at v0.41.0 (not v0.44.0): v0.44.0 fixes GO-2026-5024 but requires Go 1.25. Revisit when minimum Go version is raised to 1.25.
- Plan cache (`plan_cache.go`) is per-`DB` and keyed on Cypher string only. `maxPathHops` is implicitly scoped by the owning DB. `glsql.BindParams` always allocates new slices, so the cached pre-BindParams `glsql.Result` is safely shared read-only across goroutines. Avoid shadowing the builtin `cap` — use `size` or similar parameter names.
- SQLite FOREIGN KEY constraint errors are detected via `strings.Contains(err.Error(), "FOREIGN KEY constraint failed")` — modernc.org/sqlite surfaces the constraint name verbatim in the error string. Catch this in `InsertEdge` callers and return a domain-appropriate error rather than exposing the raw SQLite message.
- CSV node `:ID` values are file-local labels only — the actual SQLite primary keys are AUTOINCREMENT-assigned by `InsertNode`. In a fresh empty DB, sequential inserts give IDs 1, 2, 3, … matching the CSV row order, which benchmarks rely on.
- `node_labels(node_id, label)` junction table is maintained by SQLite triggers (AFTER INSERT / AFTER UPDATE OF labels on nodes). All write paths — including raw SQL from the translator and importer — stay in sync automatically without Go-level changes.
- SQLite triggers use a recursive CTE to split the comma-separated `labels` column because SQLite has no native STRING_SPLIT function.
- `node_labels` has `UNIQUE(node_id, label)` so that `INSERT OR IGNORE` in `backfillMigrationSQL` truly prevents duplicate rows. Without a unique constraint, `INSERT OR IGNORE` is a no-op and does NOT deduplicate.
- `LabelContains` in `sql/dialect.go` now takes `nodeIDExpr` (e.g. `"n0.id"`) not the labels column expression. All translator call sites pass `alias + ".id"` after task-017.
- The backfill migration uses `WHERE NOT EXISTS (... WHERE node_id = n.id)` to skip nodes already populated by triggers (i.e., inserted after the schema upgrade). `INSERT OR IGNORE` handles the edge case where a node partially appears in node_labels.
