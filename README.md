# graphlite

**Embedded property graph database for Go — openCypher over SQLite, Neo4j driver compatible.**

graphlite lets you write application code once against the standard `neo4j-go-driver` API and run it against Neo4j Aura in production or a local in-process graph in tests and development. No Docker, no network, no separate process.

```go
// One line separates your test graph from your production graph.

// In tests
driver, _ := graphlite.NewDriver(":memory:", nil)

// In production
driver, _ := neo4j.NewDriver("neo4j+s://xxx.databases.neo4j.io", auth)
```

Every query you write against graphlite runs unchanged on Neo4j — same Cypher, same driver API, same result types.

---

## Why graphlite

The `neo4j-go-driver` is the only API you touch. graphlite implements the same interface — `neo4j.Driver`, sessions, managed transactions, `ExecuteQuery` — as an in-process SQLite-backed graph store. That means:

- **Tests run without infrastructure.** No Docker container to spin up, no port to reserve, no shared state between CI workers.
- **Development is friction-free.** Clone the repo, run `go test` — it works. No `docker compose up`.
- **The migration path is one line.** When you're ready to point at a real Neo4j instance, change the constructor. Nothing else moves.

graphlite is intentionally embedded-only. It does not implement the Bolt wire protocol and is not designed to run as a standalone server. This is a deliberate scope choice: staying embedded means staying zero-infrastructure, CGO-free, and deployable anywhere Go runs.

---

## Cypher Compatibility

graphlite achieves **100% pass rate on the openCypher Technology Compatibility Kit** (235/235 executed scenarios). The table below lists supported features.

| Feature | Supported |
|---|:---:|
| `MATCH` — node by label, property, or bare | ✅ |
| `MATCH` — single-hop directed and undirected relationships | ✅ |
| `MATCH` — multi-hop (fixed depth) | ✅ |
| `MATCH` — variable-length paths `[*]`, `[*2..5]`, `[*..3]` | ✅ |
| `OPTIONAL MATCH` | ✅ |
| `WHERE` — comparisons, `AND`, `OR`, `NOT`, `IS NULL`, `IS NOT NULL` | ✅ |
| `WHERE` — `exists()`, string predicates (`CONTAINS`, `STARTS WITH`, `ENDS WITH`) | ✅ |
| `WHERE` — `hasLabel(n, 'Label')` | ✅ |
| `RETURN` with aliases, `ORDER BY`, `LIMIT`, `SKIP` | ✅ |
| `RETURN DISTINCT` | ✅ |
| `WITH` pipeline | ✅ |
| Aggregation — `count()`, `sum()`, `avg()`, `min()`, `max()` | ✅ |
| `collect()` | ✅ |
| `CASE` expressions (simple and generic) | ✅ |
| Named query parameters (`$param`) | ✅ |
| `CREATE` node and relationship | ✅ |
| `SET` property, `SET n += {map}` | ✅ |
| `REMOVE` property, `REMOVE` label | ✅ |
| `DELETE` / `DETACH DELETE` | ✅ |
| `MERGE` with `ON CREATE SET` / `ON MATCH SET` | ✅ |
| Bulk import — JSON, CSV (Neo4j format) | ✅ |
| Bulk export — JSON | ✅ |
| `neo4j.Driver` drop-in (`DriverCompat`) | ✅ |
| `shortestPath()` | ❌ |

Unsupported features return `ErrUnsupportedCypher` — they never silently produce wrong results.

---

## Install

```bash
go get github.com/LackOfMorals/graphlite
```

Requires Go 1.24+. No CGO required. Works on Linux (amd64/arm64), macOS (arm64), and Windows (amd64).

---

## Switching Between graphlite and Neo4j

The typical pattern is a constructor that reads from environment or build tags:

```go
func newDriver(ctx context.Context) (neo4j.DriverWithContext, error) {
    if uri := os.Getenv("NEO4J_URI"); uri != "" {
        auth := neo4j.BasicAuth(os.Getenv("NEO4J_USER"), os.Getenv("NEO4J_PASS"), "")
        return neo4j.NewDriverWithContext(uri, auth)
    }
    // No NEO4J_URI set — use the embedded store.
    return graphlite.NewDriver(":memory:", nil)
}
```

Your application code and tests call `newDriver` — they never import graphlite directly. Set `NEO4J_URI` in production and CI-against-real-Neo4j; leave it unset for local unit tests.

A file-backed store persists across process restarts:

```go
driver, _ := graphlite.NewDriver("/var/data/graph.db", nil)
```

---

## Data Migration

Move graph data between graphlite and any `neo4j.Driver` — including a real Neo4j instance — using `CopyFrom` and `CopyTo`.

```go
// Seed a local graphlite instance from a staging Neo4j database.
staging, _ := neo4j.NewDriverWithContext("neo4j+s://staging.example.com", auth)
local, _   := graphlite.Open(":memory:")

if err := local.CopyFrom(ctx, staging); err != nil {
    log.Fatal(err)
}

// Promote a locally built graph to Neo4j.
if err := local.CopyTo(ctx, staging); err != nil {
    log.Fatal(err)
}
```

`CopyFrom` runs inside a single graphlite transaction — either everything is imported or nothing is. `CopyTo` issues one `CREATE` per node and one per relationship; it is not atomic on the destination.

### Snapshots

`Snapshot` writes an atomic, consistent copy of the database to a file using SQLite `VACUUM INTO`. Works on both file-backed and in-memory databases.

```go
db, _ := graphlite.Open(":memory:")
// ... build or import graph data ...

// Persist the in-memory graph before the process exits.
if err := db.Snapshot("/var/data/graph-checkpoint.db"); err != nil {
    log.Fatal(err)
}

// Reopen the snapshot as a normal database.
snap, _ := graphlite.Open("/var/data/graph-checkpoint.db")
```

---

## Quick Start

### Neo4j Driver API (recommended)

```go
import (
    "github.com/LackOfMorals/graphlite"
    "github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

driver, err := graphlite.NewDriver(":memory:", nil)
if err != nil {
    log.Fatal(err)
}
defer driver.Close(ctx)

// Tier 1 — ExecuteQuery (simplest)
result, err := neo4j.ExecuteQuery(ctx, driver,
    `MATCH (p:Person {name: $name})-[:KNOWS]->(f:Person) RETURN f.name AS name`,
    map[string]any{"name": "Alice"},
    neo4j.EagerResultTransformer,
)

// Tier 2 — Managed transaction
session := driver.NewSession(ctx, neo4j.SessionConfig{})
defer session.Close(ctx)
names, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
    result, err := tx.Run(ctx, `MATCH (n:Person) RETURN n.name AS name`, nil)
    if err != nil {
        return nil, err
    }
    var names []string
    for result.Next(ctx) {
        names = append(names, result.Record().Values[0].(string))
    }
    return names, result.Err()
})

// Tier 3 — Explicit transaction
tx, err := session.BeginTransaction(ctx)
_, err = tx.Run(ctx, `CREATE (n:Person {name: $name})`, map[string]any{"name": "Bob"})
err = tx.Commit(ctx)
```

### Native API

For cases where you don't need driver compatibility — scripting, tooling, one-off data work:

```go
import "github.com/LackOfMorals/graphlite"

db, err := graphlite.Open(":memory:")
if err != nil {
    log.Fatal(err)
}
defer db.Close(ctx)

// Seed from JSON
f, _ := os.Open("testdata/graph.json")
if err := db.Import(ctx, f, graphlite.FormatJSON); err != nil {
    log.Fatal(err)
}

result, err := db.RunQuery(ctx,
    `MATCH (p:Person {name: $name})-[:KNOWS*1..3]->(f:Person) RETURN f.name AS name`,
    map[string]any{"name": "Alice"},
)
```

---

## Architecture

```
graphlite/
├── types.go          ← Node, Relationship, Record, errors (root package)
├── driver.go         ← graphlite.Open, native API, execution engine, Snapshot
├── session.go        ← BeginTx, Tx, auto-commit
├── neo4j.go          ← DriverCompat — satisfies neo4j.Driver
├── importer.go       ← Import/Export (JSON, CSV)
├── migrate.go        ← CopyFrom, CopyTo
├── cypher/
│   ├── ast.go        ← Clause and expression AST types
│   ├── parser.go     ← ANTLR/opencypher CST → AST
│   ├── plan.go       ← LogicalPlan types
│   ├── planner.go    ← AST → LogicalPlan
│   └── scope.go      ← BindingScope: Cypher vars → SQL aliases
├── sql/
│   ├── translator.go ← LogicalPlan → SQL + params
│   └── dialect.go    ← SQL dialect interface (SQLite implementation)
├── store/
│   ├── store.go      ← Store interface
│   ├── sqlite.go     ← modernc.org/sqlite implementation
│   └── schema.go     ← DDL: nodes/edges tables + indexes
├── compat/
│   └── tck_test.go   ← openCypher TCK harness (opt-in: -tags=tck)
└── bench/
    └── *.go          ← benchmark suite
```

Storage uses two tables in SQLite WAL mode. Variable-length path queries use `WITH RECURSIVE` CTEs generated at query time.

```sql
CREATE TABLE nodes (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    labels TEXT    NOT NULL DEFAULT '',   -- comma-separated: "Person,Employee"
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

## Testing

```bash
# Unit and integration tests
CGO_ENABLED=0 go test -count=1 ./...

# TCK harness (openCypher Technology Compatibility Kit)
CGO_ENABLED=0 go test -tags=tck ./compat/... -v

# Property-based tests
CGO_ENABLED=0 go test -run TestRapid ./...

# Benchmarks
CGO_ENABLED=0 go test -run=^$ -bench=. -benchtime=10s ./bench/...
```

---

## API Stability

**No breaking changes are made to the public API after v0.3 without a major version bump.**

This covers the root package and all documented sub-packages. Adding new exported symbols is not a breaking change. See [CONTRIBUTING.md](CONTRIBUTING.md) for the full definition.

| Version | Highlights |
|---|---|
| v0.1 | MATCH, CREATE, SET, DELETE, bulk JSON import, `neo4j.Driver` compat |
| v0.2 | OPTIONAL MATCH, WITH, aggregation, COLLECT, DISTINCT, REMOVE, CSV import/export |
| v0.3 | MERGE (with ON CREATE/ON MATCH), property-based tests, TCK harness |
| **v1.0** | **CASE expressions, variable-length paths, 100% openCypher TCK pass rate** |
| v1.1 | CopyFrom / CopyTo migration, Snapshot, functional options (WithBusyTimeout, WithReadOnly, NewTestDB) |
| post-v1.0 | No breaking changes without a major version bump |

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup, test suite commands, the 5-step guide for adding a Cypher feature, benchmark baseline process, and PR guidelines.

---

## License

Apache 2.0 — see [LICENSE](LICENSE).
