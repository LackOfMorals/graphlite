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
