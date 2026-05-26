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
```

WAL mode is enabled via `PRAGMA journal_mode=WAL` on every open.

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
- neo4j driver stays in go.mod as indirect dep until task-010 runs `go mod tidy` after all referencing code is gone.
