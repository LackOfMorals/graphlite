# Product Requirements Document
## graphlite — Embedded Graph Database for Go

**Version:** 0.3  
**Status:** Draft  
**Author:** Jonathan Giffard  
**Last Updated:** May 2026

---

## 1. Overview

### 1.1 Problem Statement

Go developers building against Neo4j Aura have no lightweight local substitute for testing. The options today are all bad: hit a shared dev Aura instance (slow, brittle, costs money per test run), spin up a Docker container (heavy, complicates CI, overkill for unit tests), or mock the driver entirely (fast, but misses query-level bugs and diverges from production behaviour). There is no embedded, file-based option that speaks Cypher — the Go equivalent of SQLite for relational data simply does not exist for graphs.

The closest prior option, KùzuDB, was acquired by Apple in October 2025 and its repository archived. No production-ready Go-native replacement exists.

### 1.2 Proposed Solution

**graphlite** is an embedded, file-based property graph database for Go, backed by SQLite and queryable via a subset of openCypher. The primary design goal is to be a credible local substitute for Neo4j Aura — same Cypher, same driver API surface, zero infrastructure.

The ideal developer workflow:

```go
// production
driver, _ := neo4j.NewDriver("neo4j+s://xxx.databases.neo4j.io", auth)

// tests — one line change, same queries
driver, _ := graphlite.NewDriver(":memory:", nil)
```

Secondary use cases:

- CLI tools that need to persist and query graph data without a server
- Small-to-medium graphs (up to ~5M nodes/edges) embedded in Go applications
- Offline or edge environments where a database server is not viable

The positioning in one sentence: *"Like SQLite, but for graphs — speak Cypher, store a file."*

### 1.3 Non-Goals

graphlite is not:

- A replacement for Neo4j in production workloads
- A distributed or multi-writer database
- A full openCypher TCK-compliant implementation (at v1.0)
- A graph analytics engine (no GDS-equivalent)
- A Bolt wire-protocol server (embedded library only, at v1.0)

---

## 2. Target Users

### 2.1 Primary: Neo4j / Aura Developer Wanting Lightweight Local Testing

**Profile:** Writes production Cypher against a Neo4j Aura instance. Wants a fast, zero-setup way to run integration tests locally without a container or a network call.

**Pain today:** Test suites either hit a shared dev Aura instance (slow, brittle, costs money) or mock the driver (fast but misses query-level bugs and drifts from production behaviour over time).

**What they want:** Point graphlite at `:memory:` in tests, run the exact same Cypher as production, assert on results. One line of change between production and test setup.

### 2.2 Secondary: Go Developer Building Graph-Adjacent Tools

**Profile:** Builds CLI tools, local dev tooling, MCP servers, or data pipelines. Familiar with Neo4j/Cypher. Does not want to run a Docker container or manage a server process for local work.

**Pain today:** Either mocks graph queries in tests, maintains a dev Neo4j instance, or restructures graph data into SQL tables and loses traversal ergonomics.

**What they want:** `graphlite.Open("./my.graph")` and then write Cypher. Exactly like `sql.Open` but for graphs.

### 2.3 Tertiary: Educator / Experimenter

**Profile:** Teaching graph concepts, building demos, exploring graph algorithms on small datasets. Wants something they can `go get` and use immediately without infrastructure.

---

## 3. Requirements

### 3.1 Functional Requirements

#### 3.1.1 Storage

- **FR-S1:** Graph data must persist to a single file on disk (SQLite `.db` file).
- **FR-S2:** Must support an in-memory mode (`:memory:`) for testing.
- **FR-S3:** The on-disk format must be a valid SQLite database, inspectable with standard SQLite tooling.
- **FR-S4:** Schema must support multiple labels per node.
- **FR-S5:** Properties on nodes and relationships must support: `string`, `int64`, `float64`, `bool`, `[]string`, `[]int64`, and `null`.

#### 3.1.2 Cypher — Read Path

- **FR-R1:** `MATCH (n)` — match all nodes.
- **FR-R2:** `MATCH (n:Label)` — match nodes by single label.
- **FR-R3:** `MATCH (n:Label {prop: value})` — match nodes by label and property predicate.
- **FR-R4:** `MATCH (a)-[r:TYPE]->(b)` — single directed hop.
- **FR-R5:** `MATCH (a)-[r:TYPE]-(b)` — single undirected hop.
- **FR-R6:** `MATCH (a)-[r:TYPE]->(b)-[r2:TYPE2]->(c)` — multi-hop patterns (up to 5 hops at v1.0).
- **FR-R7:** `WHERE` with property comparisons: `=`, `<>`, `<`, `>`, `<=`, `>=`.
- **FR-R8:** `WHERE` with `AND`, `OR`, `NOT`.
- **FR-R9:** `WHERE exists(n.prop)` — property existence check.
- **FR-R10:** `WHERE n.prop IS NULL` / `IS NOT NULL`.
- **FR-R11:** `RETURN` with node, relationship, and property projections.
- **FR-R12:** `RETURN` with aliasing (`AS`).
- **FR-R13:** `ORDER BY`, `LIMIT`, `SKIP`.
- **FR-R14:** `OPTIONAL MATCH` — left-join semantics.
- **FR-R15:** `WITH` — pipeline results between query stages.
- **FR-R16:** `WITH` + aggregation functions: `count()`, `sum()`, `avg()`, `min()`, `max()`.
- **FR-R17:** `COLLECT()` — aggregate values into a list.
- **FR-R18:** `DISTINCT` — in both `RETURN` and aggregation context.
- **FR-R19:** `WHERE n.prop IN [...]` — list membership.
- **FR-R20:** `WHERE n.prop STARTS WITH / ENDS WITH / CONTAINS` — string predicates.

#### 3.1.3 Cypher — Write Path

- **FR-W1:** `CREATE (n:Label {props})` — create a node.
- **FR-W2:** `CREATE (a)-[:TYPE {props}]->(b)` — create a relationship.
- **FR-W3:** `CREATE` with pattern: create multiple nodes and relationships in one statement.
- **FR-W4:** `SET n.prop = value` — set a property.
- **FR-W5:** `SET n += {map}` — merge properties from a map (v0.2).
- **FR-W6:** `REMOVE n.prop` — remove a property (v0.2).
- **FR-W7:** `REMOVE n:Label` — remove a label (v0.2).
- **FR-W8:** `DELETE n` — delete a node (error if has relationships).
- **FR-W9:** `DETACH DELETE n` — delete a node and all its relationships.
- **FR-W10:** `DELETE r` — delete a relationship.
- **FR-W11:** `MERGE (n:Label {prop: value})` — basic node merge (v0.3).
- **FR-W12:** `MERGE ... ON CREATE SET ... ON MATCH SET ...` — full merge (v0.3).

#### 3.1.4 Driver Compatibility

The `DriverCompat` adapter is a first-class requirement, not an afterthought. It is the feature that makes the primary use case (Aura test substitute) work with zero query changes.

- **FR-DC1:** graphlite must implement the `neo4j.Driver` interface from `github.com/neo4j/neo4j-go-driver/v6/neo4j`. The v6 API dropped the `WithContext` suffix — `neo4j.Driver` and `neo4j.NewDriver` are the current names. The deprecated v5 aliases (`DriverWithContext`, `NewDriverWithContext`) will be removed in v7 and are not targets.
- **FR-DC2:** `graphlite.NewDriver(uri, auth)` must be a drop-in replacement for `neo4j.NewDriver()` in test code. The URI `:memory:` denotes an in-memory instance; a file path denotes a persistent store.
- **FR-DC3:** All three v6 transaction tiers must be supported:
  - `neo4j.ExecuteQuery()` — the v6 recommended default; graphlite must satisfy the function signature and `EagerResultTransformer` contract.
  - `session.ExecuteRead()` / `session.ExecuteWrite()` with `neo4j.ManagedTransaction` — managed transactions with automatic retry semantics (graphlite does not need to implement retry, but the callback signature must be satisfied).
  - `session.BeginTransaction()` with explicit `Commit()` / `Rollback()` — explicit transaction control.
- **FR-DC4:** `neo4j.SessionConfig` must be accepted in `driver.NewSession()`. `DatabaseName` is accepted and ignored (graphlite is single-database). `AccessMode` is respected to the extent of distinguishing read-only vs read-write sessions.
- **FR-DC5:** Auth config (`neo4j.BasicAuth`, `neo4j.NoAuth`, etc.) is accepted but ignored — no authentication is required or enforced for local instances.
- **FR-DC6:** Result records must satisfy the v6 `neo4j.Record` interface. `record.Get("key")` returns `(any, bool)`. `record.AsMap()` returns `map[string]any`. Nodes returned as `neo4j.Node` with `Props map[string]any` and `Labels []string`. Relationships returned as `neo4j.Relationship` with `Props`, `Type`, `StartElementId`, `EndElementId`.
- **FR-DC7:** Any Cypher feature outside graphlite's supported subset must return a clear, structured error (`ErrUnsupportedCypher`) that identifies the unsupported clause — it never silently returns wrong results. This error must be detectable via `errors.As` alongside the standard `neo4j.IsNeo4jError()` helpers.

#### 3.1.5 Bulk Import

Seeding test graphs programmatically is a first-class workflow for the primary use case. A separate serialisation format avoids forcing users to write many individual `CREATE` statements.

- **FR-I1:** `graphlite.Import(r io.Reader, format ImportFormat)` — load nodes and edges from a reader.
- **FR-I2:** Must support JSON import format at v0.1. Format: `{"nodes": [...], "edges": [...]}` where each node carries `labels` and `props`, each edge carries `type`, `startId`, `endId`, and `props`.
- **FR-I3:** Must support CSV import at v0.2 (separate node and edge files, Neo4j-compatible header format).
- **FR-I4:** `graphlite.Export(w io.Writer, format ExportFormat)` — dump the full graph to a writer (v0.2).
- **FR-I5:** Import must run inside a single transaction for atomicity. On any parse or constraint error, the entire import rolls back.

#### 3.1.6 Query Parameters

- **FR-P1:** All queries must support named parameters via `map[string]any`.
- **FR-P2:** Parameter syntax must match the Neo4j Go driver convention (`$paramName`).
- **FR-P3:** Parameters must be accepted in `WHERE` predicates, `CREATE` property maps, and `MERGE` patterns.

#### 3.1.7 Transactions

- **FR-T1:** Explicit read transactions.
- **FR-T2:** Explicit write transactions.
- **FR-T3:** Auto-commit mode for single-statement execution.
- **FR-T4:** Rollback on error.

#### 3.1.8 Results

- **FR-RE1:** Results from `ExecuteQuery` must be eagerly collected and accessible via `result.Records []neo4j.Record` and `result.Summary`.
- **FR-RE2:** Results from managed and explicit transactions must support lazy iteration via `result.Next(ctx)` / `result.Record()` / `result.Err()`.
- **FR-RE3:** `record.Get("key")` must return `(any, bool)` — matching the v6 driver interface exactly. `record.AsMap()` must return `map[string]any`.
- **FR-RE4:** Nodes in results must be returned as `neo4j.Node` with `Props map[string]any`, `Labels []string`, and `ElementId string`.
- **FR-RE5:** Relationships in results must be returned as `neo4j.Relationship` with `Props map[string]any`, `Type string`, `StartElementId string`, and `EndElementId string`.
- **FR-RE6:** `result.Summary` must expose `Counters()` returning node/relationship created/deleted counts, matching the `neo4j.ResultSummary` interface.

### 3.2 Non-Functional Requirements

- **NFR-1 Performance:** Single-hop `MATCH` on a 1M node graph must return in under 100ms with appropriate indexes. This is a target, not a hard SLA.
- **NFR-2 Binary size:** The library must not add more than 15MB to a compiled Go binary (driven by the SQLite embed cost).
- **NFR-3 CGO:** Must offer a CGO-free build path (via `modernc.org/sqlite`). CGO builds via `mattn/go-sqlite3` may be offered as an opt-in build tag for performance-sensitive users.
- **NFR-4 Concurrency:** Multiple concurrent readers must be supported. Single writer at a time (SQLite WAL mode).
- **NFR-5 Go version:** Must support Go 1.21+.
- **NFR-6 Platforms:** Must build and test on Linux (amd64, arm64), macOS (arm64), and Windows (amd64) in CGO-free mode.

---

## 4. Architecture

### 4.1 Component Overview

```
┌─────────────────────────────────────────────────────┐
│                   Public API                         │
│   driver.go  /  session.go  /  compat.go            │
│   (implements neo4j.Driver interface — v6)          │
└───────────────────────┬─────────────────────────────┘
                        │
          ┌─────────────▼──────────────┐
          │       Cypher Layer          │
          │  parser.go  /  planner.go   │
          │  plan.go    /  scope.go     │
          └─────────────┬──────────────┘
                        │  LogicalPlan
          ┌─────────────▼──────────────┐
          │      SQL Translator         │
          │  translator.go             │
          │  dialect.go  (SQLite)      │
          └─────────────┬──────────────┘
                        │  SQL + params
          ┌─────────────▼──────────────┐
          │      Storage Layer          │
          │  store.go  (interface)     │
          │  sqlite.go (implementation)│
          │  schema.go (DDL)           │
          │  importer.go               │
          └────────────────────────────┘
```

### 4.2 Key Design Decisions

**Parser:** Use `cloudprivacylabs/opencypher` as the parsing and AST layer. This is Apache 2.0 licensed, maintained, and handles the majority of the target Cypher subset. The planner wraps it with a custom AST walk rather than using its in-memory evaluator.

**Storage schema:** Two core tables plus indexes:

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

CREATE INDEX idx_nodes_labels  ON nodes(labels);
CREATE INDEX idx_edges_start   ON edges(start_id);
CREATE INDEX idx_edges_end     ON edges(end_id);
CREATE INDEX idx_edges_type    ON edges(type);
```

**Logical plan types:** The planner emits a `LogicalPlan` tree (not SQL directly), which enables future dialect implementations and makes unit testing of the planner independent of SQL generation.

**Binding scope:** The translator maintains a `BindingScope` — a map of Cypher variable names to their SQL table alias and column expression. This is threaded through the entire AST walk and is the core data structure that makes multi-clause queries (especially `WITH` pipelines) correct.

**SQL dialect interface:** The translator calls a `Dialect` interface for SQL-specific emission. The only implementation at v1.0 is SQLite. This is the extension point for future Postgres/DuckDB backends.

### 4.3 Public API Shape

graphlite exposes two entry points: a native API and a `DriverCompat` adapter that satisfies the Neo4j Go v6 driver interface. Both are supported long-term.

**Native API**

```go
// Open a graph database. Path may be a file path or ":memory:".
db, err := graphlite.Open("./my.graph")
defer db.Close(ctx)

// Bulk import for seeding test graphs
f, _ := os.Open("testdata/graph.json")
err = db.Import(ctx, f, graphlite.FormatJSON)
```

**DriverCompat — drop-in Neo4j v6 driver substitute**

```go
// production code
import "github.com/neo4j/neo4j-go-driver/v6/neo4j"

driver, err := neo4j.NewDriver(
    "neo4j+s://xxx.databases.neo4j.io",
    neo4j.BasicAuth("user", "pass", ""),
)

// test code — one import and one constructor line change
import "github.com/graphlite/graphlite"

driver, err := graphlite.NewDriver(":memory:", nil)

// ── Tier 1: ExecuteQuery (recommended default) ──────────────────────────
result, err := neo4j.ExecuteQuery(ctx, driver,
    `MATCH (p:Person {name: $name})-[:KNOWS]->(f:Person)
     RETURN f.name AS name, f.age AS age ORDER BY f.name`,
    map[string]any{"name": "Alice"},
    neo4j.EagerResultTransformer,
    neo4j.ExecuteQueryWithDatabase("neo4j"),
)
for _, rec := range result.Records {
    name, _ := rec.Get("name")
    fmt.Println(name)
}

// ── Tier 2: Managed transaction (lazy streaming) ─────────────────────────
session := driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: "neo4j"})
defer session.Close(ctx)

names, err := session.ExecuteRead(ctx,
    func(tx neo4j.ManagedTransaction) (any, error) {
        result, err := tx.Run(ctx,
            `MATCH (p:Person) RETURN p.name AS name LIMIT $limit`,
            map[string]any{"limit": 100},
        )
        if err != nil {
            return nil, err
        }
        var names []string
        for result.Next(ctx) {
            name, _ := result.Record().Get("name")
            names = append(names, name.(string))
        }
        return names, result.Err()
    },
)

// ── Tier 3: Explicit transaction ─────────────────────────────────────────
tx, err := session.BeginTransaction(ctx)
_, err = tx.Run(ctx, `CREATE (n:Person {name: $name})`, map[string]any{"name": "Bob"})
err = tx.Commit(ctx)
```

The DriverCompat adapter satisfies `neo4j.Driver` from `neo4j-go-driver/v6`. Auth config is accepted and ignored. Unsupported Cypher returns `ErrUnsupportedCypher` — never silent wrong results.

---

## 5. Compatibility

### 5.1 Cypher Compatibility Table

This table defines the v1.0 target state and is intended to ship as part of the README.

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
| **DriverCompat** (`neo4j.DriverWithContext`) | ✅ | ✅ | ✅ |
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
| List comprehensions | ❌ | ❌ | ❌ |
| `UNION` / `UNION ALL` | ❌ | ❌ | ❌ |
| `CALL` subqueries | ❌ | ❌ | ❌ |
| GDS / graph algorithms | ❌ | ❌ | ❌ |

✅ Supported  🚧 Partial / experimental  ❌ Not supported

> **Note on variable-length paths:** `(a)-[*1..n]->(b)` is explicitly deferred to v1.1. It is technically feasible via recursive CTEs but is not required for the primary use case (Aura test substitute) and adds significant implementation complexity. It will be tracked as a milestone issue.

### 5.2 openCypher TCK

The openCypher Technology Compatibility Kit will be used as the authoritative test suite. The target pass rates are:

| Milestone | TCK Pass Rate Target |
|---|---|
| v0.2 | ~30% |
| v1.0 | ~60% |

TCK results will be published in CI and linked from the README. Failing TCK scenarios will be tracked as known issues, not hidden.

---

## 6. Testing Strategy

### 6.1 Unit Tests

Each layer is tested in isolation:

- **Parser:** verify AST structure for known Cypher inputs.
- **Planner:** given an AST, assert the correct `LogicalPlan` is produced.
- **Translator:** given a `LogicalPlan`, assert the correct SQL string and parameter binding.
- **Storage:** against an in-memory SQLite instance, assert correct node/edge CRUD behaviour.

### 6.2 Integration Tests

End-to-end tests run Cypher queries through the full stack against an in-memory database. The canonical set lives in `testdata/` as `.cypher` fixture files paired with expected result JSON.

These cover the scenarios in the compatibility table above, one test per feature.

### 6.3 TCK Tests

A subset of the openCypher TCK Gherkin scenarios are run via a Godog-based test harness in `compat/tck_test.go`. Skipped scenarios are explicitly listed with a reason. The test run reports a pass rate that is committed to the repo.

### 6.4 Property-Based Tests

`pgregory.net/rapid` is used to generate random graphs (random node counts, label distributions, property maps, edge structures), write them via `CREATE`, and assert full round-trip fidelity via `MATCH`. This catches encoding bugs, JSON escaping issues, and scope tracker edge cases that hand-written tests miss.

### 6.5 Benchmark Tests

Standard Go benchmark suite covering:

- `MATCH` single node by ID
- `MATCH` single node by property (indexed and unindexed)
- Single-hop traversal on 100K / 1M node graphs
- `CREATE` throughput (single and batch)
- `WITH` aggregation pipeline

Benchmarks run in CI on every release tag and results are committed to `bench/results/`.

---

## 7. Milestones

### v0.1 — "Viable as a Test Double"

**Goal:** A developer writing tests against Neo4j Aura can point graphlite at `:memory:`, seed a graph via JSON import, run their production Cypher queries, and assert on results — with one import line and one constructor line changed.

**Scope:**
- Core `MATCH` patterns (single node, single hop, multi-hop up to 5)
- `WHERE` with property predicates and `AND/OR/NOT`
- `RETURN` with aliases, `ORDER BY`, `LIMIT`, `SKIP`
- `CREATE` nodes and relationships
- `SET` property and `DELETE` / `DETACH DELETE`
- Named query parameters (`$param` syntax)
- **DriverCompat adapter** — satisfies `neo4j.DriverWithContext` interface
- **JSON bulk import** — `db.Import(ctx, r, graphlite.FormatJSON)`
- In-memory (`:memory:`) and file-backed storage
- CGO-free build via `modernc.org/sqlite`
- Full unit and integration test suite for covered features
- README with compatibility table, quick start, and honest scope statement

**Success criteria:** A developer can replace `neo4j.NewDriverWithContext` with `graphlite.NewDriverWithContext(":memory:", nil)`, seed via JSON import, and run their existing Cypher test suite with no query changes. All covered features pass their integration tests.

### v0.2 — "Covers the Full Query Pipeline"

**Goal:** The Cypher patterns that appear in 90% of real Neo4j application code work correctly.

**Scope:**
- `OPTIONAL MATCH`
- `WITH` pipelines and aggregation (`count`, `sum`, `avg`, `min`, `max`)
- `COLLECT()`
- `DISTINCT`
- `WHERE exists()`, `IS NULL`, `STARTS WITH`, `CONTAINS`, `ENDS WITH`, `IN`
- `REMOVE` property and label
- `SET n += {map}` property merge
- CSV bulk import (Neo4j-compatible header format)
- JSON/CSV bulk export
- ~30% TCK pass rate published in CI
- Property-based test suite (`pgregory.net/rapid`)

### v0.3 — "API-Stable and Dependable"

**Goal:** Teams can take a dependency on graphlite with confidence it won't break under them.

**Scope:**
- `MERGE` basic form and `ON CREATE / ON MATCH`
- API stability commitment — no breaking changes post-v0.3 without a major version bump
- GoDoc complete for all exported symbols
- Contribution guide and issue templates published
- ~50% TCK pass rate

### v1.0 — "Production-Ready"

**Goal:** Benchmarked, hardened, and ready for broad adoption. The project graduates from experimental to stable.

**Scope:**
- Benchmark suite published and committed to `bench/results/`
- Performance target validated: single-hop `MATCH` on 1M nodes under 100ms
- `CASE` expressions (basic form)
- ~60% TCK pass rate
- Security review of import path (no path traversal, JSON bomb protection)
- GoReleaser-based release pipeline with binaries and checksums

### v1.1 — "Variable-Length Paths"

**Goal:** Add the feature that most distinguishes graph databases from graph-shaped SQL wrappers.

**Scope:**
- `(a)-[*1..n]->(b)` via SQLite recursive CTEs
- Cycle detection for unbounded traversal
- Configurable max depth (default: 10, hard cap: 25)
- Benchmark variable-length path queries at 100K and 1M node scale
- Document graph size guidance and expected performance envelope

---

## 8. Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `cloudprivacylabs/opencypher` parser has gaps in Cypher coverage | Medium | High | Audit parser against target feature list before v0.1 commit. Fork if necessary — it's Apache 2.0. |
| Binding scope tracker bugs cause incorrect SQL for complex multi-clause queries | High | High | Property-based tests catch these early. Invest heavily in planner unit tests before translator. |
| Neo4j Go driver v6 interface changes break DriverCompat adapter | Medium | High | Pin to `neo4j-go-driver/v6` minor version. Monitor upstream releases. The v5→v6 rename (`WithContext` suffix dropped) is the last major API reshape expected before v7. Document the supported driver version in the README. |
| SQLite recursive CTEs too slow for variable-length paths on large graphs | Medium | Low | Deferred to v1.1 — not on the critical path. Benchmark before committing to the feature at that milestone. |
| Neo4j trademark / naming conflict | Low | Medium | Avoid "Neo4j" in the project name, binary, or import path. Name is `graphlite`. |
| openCypher TCK Gherkin runner maintenance burden | Low | Low | Scope the TCK runner to a `compat/` package. Keep it opt-in for contributors. |
| JSON import used to load malicious payloads (JSON bomb, path traversal) | Low | Medium | Add depth/size limits to JSON parser at v0.1. Full security review at v1.0. |

---

## 9. Open Questions

1. **Import path / GitHub org:** `github.com/LackOfMorals/graphlite` or a dedicated org (e.g. `github.com/graphlite/graphlite`)? A dedicated org signals a project rather than a personal experiment and is harder to abandon-signal. Recommended: create the org before the first public commit.

2. **DriverCompat interface version pinning:** The `neo4j.Driver` interface is from `neo4j-go-driver/v6`. The v5 `WithContext` aliases still compile in v6 but are deprecated and will be removed in v7. graphlite should target v6 names only and document this clearly. The structural interface extraction approach (define our own matching interface and rely on Go duck typing) is worth evaluating — it would make graphlite resilient to minor v6 changes without requiring a graphlite release, but it adds friction for users who want `go vet`-level guarantees of compatibility.

3. **`ErrUnsupportedCypher` granularity:** When a query uses an unsupported Cypher feature, should the error identify the specific unsupported clause (e.g. `variable-length path in MATCH at position 12`)? Yes — this is what makes the test-double use case trustworthy. Users need to know whether the error is a graphlite limitation or a query bug.

4. **Index API:** Should graphlite expose explicit Cypher index creation (`CREATE INDEX FOR (n:Person) ON (n.email)`) or manage indexes automatically based on query patterns? Automatic is simpler; explicit gives control. Lean towards automatic at v0.1 with an explicit hint API at v0.3.

5. **Multi-label `MATCH` semantics:** Neo4j requires all labels in a pattern to be present (`MATCH (n:Person:Employee)` matches nodes with both labels). This is the correct behaviour to implement — confirm this is what `cloudprivacylabs/opencypher` produces before v0.1.

6. **BOLT server mode:** Post-v1.0 stretch goal — a minimal Bolt listener that allows existing Neo4j tooling (Neo4j Browser, Bloom, neodash) to connect to a local graphlite instance. Significantly expands the use case. Not in scope for v1.0 but worth a design spike to ensure the architecture doesn't preclude it.

---

## 10. References

- [openCypher Specification](https://opencypher.org/)
- [openCypher TCK](https://github.com/opencypher/openCypher/tree/master/tck)
- [cloudprivacylabs/opencypher](https://github.com/cloudprivacylabs/opencypher) — Apache 2.0 Go openCypher parser/evaluator
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) — pure Go SQLite (CGO-free)
- [mattn/go-sqlite3](https://github.com/mattn/go-sqlite3) — CGO SQLite (optional perf build tag)
- [pgregory.net/rapid](https://github.com/flyingmutant/rapid) — property-based testing for Go
- [neo4j/neo4j-go-driver v6](https://github.com/neo4j/neo4j-go-driver) — driver interface (`neo4j.Driver`) that DriverCompat must satisfy; import path `github.com/neo4j/neo4j-go-driver/v6/neo4j`
- [Neo4j Aura](https://neo4j.com/cloud/platform/aura-graph-database/) — primary production target graphlite is designed to substitute in tests
