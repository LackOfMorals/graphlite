# graphlite

**Embedded graph database for Go — backed by SQLite, queryable via openCypher.**

graphlite is a zero-infrastructure local substitute for Neo4j Aura, designed for testing and development workflows. The same Cypher queries, the same driver API, no Docker containers, no network calls.

```go
// production
driver, _ := neo4j.NewDriver("neo4j+s://xxx.databases.neo4j.io", auth)

// tests — one line change, same queries
driver, _ := graphlite.NewDriver(":memory:", nil)
```

> **Status:** Early development (v0.1 in progress). Not yet ready for production use.

---

## Scope

graphlite is:

- A CGO-free, embedded property graph database for Go
- A drop-in local substitute for Neo4j Aura in test code
- A single-file graph store (like SQLite, but for graphs)

graphlite is **not**:

- A production replacement for Neo4j
- A distributed or multi-writer database
- A full openCypher TCK-compliant engine (at v0.1)
- A Bolt wire-protocol server

---

## Cypher Compatibility

| Feature | v0.1 | v0.2 | v1.0 |
|---|---|---|---|
| `MATCH` single node | ✅ | ✅ | ✅ |
| `MATCH` by label | ✅ | ✅ | ✅ |
| `MATCH` by property | ✅ | ✅ | ✅ |
| Single-hop directed relationship | ✅ | ✅ | ✅ |
| Single-hop undirected relationship | ✅ | ✅ | ✅ |
| Multi-hop (2–5 hops) | ✅ | ✅ | ✅ |
| `WHERE` comparisons | ✅ | ✅ | ✅ |
| `WHERE AND / OR / NOT` | ✅ | ✅ | ✅ |
| `RETURN` with aliases | ✅ | ✅ | ✅ |
| `ORDER BY / LIMIT / SKIP` | ✅ | ✅ | ✅ |
| Named query parameters | ✅ | ✅ | ✅ |
| `CREATE` node | ✅ | ✅ | ✅ |
| `CREATE` relationship | ✅ | ✅ | ✅ |
| `SET` property | ✅ | ✅ | ✅ |
| `DELETE` / `DETACH DELETE` | ✅ | ✅ | ✅ |
| **DriverCompat** (`neo4j.Driver`) | ✅ | ✅ | ✅ |
| **Bulk import** (JSON) | ✅ | ✅ | ✅ |
| `OPTIONAL MATCH` | ❌ | ✅ | ✅ |
| `WITH` pipeline | ❌ | ✅ | ✅ |
| Aggregation (`count`, `sum`, etc.) | ❌ | ✅ | ✅ |
| `COLLECT()` | ❌ | ✅ | ✅ |
| `DISTINCT` | ❌ | ✅ | ✅ |
| `WHERE exists()` / `IS NULL` | ❌ | ✅ | ✅ |
| String predicates (`CONTAINS` etc.) | ❌ | ✅ | ✅ |
| `REMOVE` property / label | ❌ | ✅ | ✅ |
| `SET n += {map}` | ❌ | ✅ | ✅ |
| Bulk import (CSV, Neo4j format) | ❌ | ✅ | ✅ |
| Bulk export (JSON) | ❌ | ✅ | ✅ |
| `MERGE` (basic) | ❌ | ❌ | ✅ |
| `MERGE ON CREATE / ON MATCH` | ❌ | ❌ | ✅ |
| `CASE` expressions | ❌ | ❌ | 🚧 |
| Variable-length paths `*1..n` | ❌ | ❌ | ❌ |
| `shortestPath()` | ❌ | ❌ | ❌ |

✅ Supported  🚧 Partial / experimental  ❌ Not supported

Unsupported Cypher features return `ErrUnsupportedCypher` — they never silently produce wrong results.

---

## Install

```bash
go get github.com/LackOfMorals/graphlite
```

Requires Go 1.21+. No CGO required.

---

## Quick Start

### Native API

```go
import "github.com/LackOfMorals/graphlite"

db, err := graphlite.Open(":memory:")
if err != nil {
    log.Fatal(err)
}
defer db.Close(ctx)

// Bulk-seed from JSON
f, _ := os.Open("testdata/graph.json")
if err := db.Import(ctx, f, graphlite.FormatJSON); err != nil {
    log.Fatal(err)
}

// Run a Cypher query
result, err := db.RunQuery(ctx,
    `MATCH (p:Person {name: $name})-[:KNOWS]->(f:Person) RETURN f.name AS name`,
    map[string]any{"name": "Alice"},
)
```

### DriverCompat — Neo4j v6 drop-in

```go
import (
    "github.com/LackOfMorals/graphlite"
    "github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// Replace neo4j.NewDriver with graphlite.NewDriver in tests
driver, err := graphlite.NewDriver(":memory:", nil)
defer driver.Close(ctx)

// All three v6 transaction tiers work unchanged:

// Tier 1 — ExecuteQuery
result, err := neo4j.ExecuteQuery(ctx, driver,
    `MATCH (n:Person) RETURN n.name AS name`,
    nil, neo4j.EagerResultTransformer,
)

// Tier 2 — Managed transaction
session := driver.NewSession(ctx, neo4j.SessionConfig{})
defer session.Close(ctx)
names, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
    result, err := tx.Run(ctx, `MATCH (n:Person) RETURN n.name AS name`, nil)
    // ...
    return names, result.Err()
})

// Tier 3 — Explicit transaction
tx, err := session.BeginTransaction(ctx)
_, err = tx.Run(ctx, `CREATE (n:Person {name: $name})`, map[string]any{"name": "Bob"})
err = tx.Commit(ctx)
```

---

## Architecture

```
graphlite/
├── types.go          ← Node, Relationship, Record, errors (root package)
├── driver.go         ← graphlite.Open, native API
├── session.go        ← BeginTx, Tx, auto-commit
├── neo4j.go          ← DriverCompat — satisfies neo4j.Driver
├── importer.go       ← Import/Export
├── cypher/
│   ├── parser.go     ← thin wrapper around cloudprivacylabs/opencypher
│   ├── plan.go       ← LogicalPlan types
│   ├── planner.go    ← AST → LogicalPlan
│   └── scope.go      ← BindingScope: Cypher vars → SQL aliases
├── sql/
│   ├── translator.go ← LogicalPlan → SQL + params
│   └── dialect.go    ← SQL dialect interface (SQLite implementation)
├── store/
│   ├── store.go      ← Store interface
│   ├── sqlite.go     ← modernc.org/sqlite implementation
│   └── schema.go     ← DDL for nodes/edges tables + indexes
├── compat/
│   └── tck_test.go   ← openCypher TCK harness (opt-in: -tags=tck)
└── testdata/
    └── *.cypher      ← fixture tests
```

Storage uses two tables backed by SQLite WAL mode:

```sql
CREATE TABLE nodes (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    labels TEXT    NOT NULL DEFAULT '',   -- comma-separated, e.g. "Person,Employee"
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

---

## Build

```bash
# CGO-free (default, all platforms)
CGO_ENABLED=0 go build ./...

# Run tests
go test ./...
```

---

## Supported Platforms

| Platform | Architecture | CGO-free |
|---|---|---|
| Linux | amd64 | ✅ |
| Linux | arm64 | ✅ |
| macOS | arm64 | ✅ |
| Windows | amd64 | ✅ |

---

## License

Apache 2.0 — see [LICENSE](LICENSE).
