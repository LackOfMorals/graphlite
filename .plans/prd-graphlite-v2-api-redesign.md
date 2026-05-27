# PRD: graphlite v2 API Redesign

## Overview

graphlite v1.x carries the neo4j Go driver as an indirect dependency and shapes its public API around `neo4j.DriverWithContext`. This creates unnecessary dependency weight for users who just want a lightweight embedded graph database, and it locks graphlite's design to Neo4j's release cycle. v2.0 removes the neo4j driver dependency entirely, defines a clean public API that graphlite owns outright, and provides example programs to cover the common side-by-side usage patterns developers need.

## Goals

- Remove `github.com/neo4j/neo4j-go-driver` as a dependency (direct or indirect) from the graphlite module.
- Define a minimal, stable public API surface that graphlite owns outright.
- Keep the API immediately recognisable to developers familiar with the neo4j Go driver v6, without being a structural copy of it.
- Provide working example programs for common side-by-side usage with the neo4j driver.
- Update the README to reflect the new value proposition.
- Maintain 100% openCypher TCK pass rate (235/235 scenarios).

## Non-Goals

- graphlite will not implement the Bolt wire protocol or run as a network server.
- graphlite will not provide a first-party `CopyFrom`/`CopyTo` method — this becomes user-space code, documented by examples.
- graphlite will not provide a compat shim that satisfies `neo4j.Driver` or `neo4j.DriverWithContext`.
- No spatial types (Point2D/3D), ML vector types, or neo4j temporal types beyond JSON-serialisable Go primitives.
- No `SessionConfig`, `BookmarkManager`, `AccessMode`, or `TransactionConfig` — no cluster, no causal consistency.

## Requirements

### Functional Requirements

- REQ-F-001: `Open(path string, opts ...Option) (*DB, error)` is the single entry point. `:memory:` and file paths both work.
- REQ-F-002: `(*DB) RunQuery(ctx, cypher, params) (*Result, error)` executes in auto-commit mode and returns a concrete `*Result`.
- REQ-F-003: `(*DB) BeginTx(ctx) (*Tx, error)` starts an explicit transaction. The unused `bool` parameter from v1.x is removed.
- REQ-F-004: `(*Tx) Commit() error`, `(*Tx) Rollback() error`, `(*Tx) Close() error` have no `context.Context` parameter (no network, nothing to cancel).
- REQ-F-005: `(*Tx) Run(ctx, cypher, params) (*Result, error)` returns concrete `*Result`.
- REQ-F-006: `*Result` exposes `Next(ctx)`, `Record()`, `Err()`, `Keys()`, `Collect(ctx)`, `Single(ctx)`, `Consume(ctx)`. `Single` is new in v2.0.
- REQ-F-007: `*Record` exposes `Get(key)`, `Keys()`, `Values()`, `AsMap()`. `Values()` is a method returning a copy (not a public field).
- REQ-F-008: `Node` and `Relationship` structs retain identical field names and types to v1.x (`ElementId`, `Labels`, `Props`, `Type`, `StartElementId`, `EndElementId`).
- REQ-F-009: Generic helpers `GetProperty[T]`, `GetRecordValue[T]`, `CollectT[T]`, `SingleT[T]` are added. Signatures match the neo4j driver v6 equivalents.
- REQ-F-010: `(*DB) Import(ctx, r, Format) error` and `(*DB) Export(ctx, w, Format) error` are unchanged.
- REQ-F-011: `(*DB) Snapshot(path) error` is unchanged.
- REQ-F-012: `NewTestDB(t, opts...) *DB` is unchanged.
- REQ-F-013: `WithBusyTimeout(d)` and `WithReadOnly()` options are unchanged.
- REQ-F-014: All structured error types (`ErrUnsupportedCypher`, `ErrMissingParameter`, `ErrImportDepthExceeded`, `ErrImportTooLarge`) are retained with existing fields.
- REQ-F-015: `ErrReadOnly` sentinel is retained.
- REQ-F-016: `ResultSummary` and `Counters` interfaces are retained.
- REQ-F-017: Working example programs are provided under `examples/` for: backend switch, copy-from-Neo4j, copy-to-Neo4j. Each has its own `go.mod` that imports both graphlite and the neo4j driver.

### Non-Functional Requirements

- REQ-NF-001: `go get github.com/LackOfMorals/graphlite` must not transitively pull in `github.com/neo4j/neo4j-go-driver` at any version.
- REQ-NF-002: `go vet ./...` passes with no exported symbols beyond those listed in the API surface.
- REQ-NF-003: openCypher TCK pass rate remains 100% (235/235 scenarios).
- REQ-NF-004: All existing unit and integration tests pass (updated where needed for the new API).
- REQ-NF-005: The module continues to target Go 1.24.
- REQ-NF-006: The public API surface is documented consistently; no exported symbol is left without a doc comment.

## Technical Considerations

**Dependency removal:** The neo4j driver enters via `neo4jadapter/` and `migrate.go` (which reference `neo4j.DriverWithContext`, `neo4j.Session`, etc.). Removing those files and the `neo4jadapter/` package should be sufficient to drop the driver from `go.mod`. Run `go mod tidy` to verify.

**`QueryResult` → `Result` rename:** v1.x exported `QueryResult` as a concrete type and `Result` as an interface. In v2.0 there is only the concrete `*Result`. All call sites returning `Result` (the interface) must be updated to return `*Result`. The `Result` interface type and name is freed up for the concrete type.

**Session layer removal:** The `Session`, `ManagedTransaction`, `ManagedTransactionWork`, `Transaction` interfaces and their concrete implementations (`session`, `managedTx`) are removed. `*DB` becomes the direct entry point. `ExecuteRead`/`ExecuteWrite` patterns are not part of the v2.0 API — developers use `BeginTx` directly for explicit transactions, or `RunQuery` for auto-commit.

**`Tx.Commit/Rollback/Close` context removal:** These currently accept `context.Context` for neo4j driver interface compatibility. SQLite commit/rollback have no network operation to cancel. The parameter is removed. `defer tx.Close()` becomes the idiomatic cleanup pattern.

**`Single(ctx)` implementation:** Drain up to two records. If zero: return `nil, ErrNoRecords` (new sentinel). If exactly one: return it. If two or more: drain remainder and return `nil, ErrMultipleRecords` (new sentinel). Both errors should be checkable with `errors.Is`.

**Generic helpers:** `GetProperty[T]` and `GetRecordValue[T]` require `Node` and `Relationship` to expose their props map via a method or interface. The cleanest approach is a small unexported interface `{ getProps() map[string]any }` that both implement, matching the pattern in the neo4j driver.

**`CollectT` and `SingleT`:** These are standalone generic functions on top of `Collect`/`Single`, so they can live in a new file `helpers.go` with no structural changes to `Result`.

**Internal exports to unexport:** `NewQueryResultFromRows`, `QueryCounters`, `SetCounters`, `MapColumnValue`, `SplitLabels`, `NewRecord`, `NewEagerResult`. Tests that use these must be updated to go through the public API.

**Files to delete:** `migrate.go`, `neo4jadapter/` (entire directory), `neo4j.go` (if it exists as a compat file), any compat layer files. `migrate_test.go` must be rewritten or removed.

**`EagerResult` and `ResultTransformer`:** These exist to support the `ExecuteQuery` generic function on the compat layer. With the session layer removed, they have no purpose and are deleted.

**`NewDriver`/`AuthToken`/`NoAuth`:** Vestigial compat shims — deleted.

**`BeginTx` bool parameter:** The `bool readOnly` parameter is silently ignored in v1.x. Remove it.

## Acceptance Criteria

- [ ] `go.mod` contains no reference to `github.com/neo4j/neo4j-go-driver` at any version.
- [ ] `go get github.com/LackOfMorals/graphlite` does not pull in the neo4j driver transitively.
- [ ] TCK pass rate is 100% (235/235).
- [ ] All unit tests pass (`go test -tags=unit ./...`).
- [ ] `go vet ./...` passes cleanly.
- [ ] No exported symbol exists beyond those in §4 of the PRD.
- [ ] `(*Result) Single(ctx)` exists and behaves correctly for 0, 1, and 2+ records.
- [ ] `GetProperty[T]`, `GetRecordValue[T]`, `CollectT[T]`, `SingleT[T]` exist and are tested.
- [ ] `(*Tx) Commit()`, `Rollback()`, `Close()` have no context parameter.
- [ ] `(*DB) BeginTx(ctx)` has no bool parameter.
- [ ] `QueryResult` type is renamed to `Result`; no exported `QueryResult` symbol remains.
- [ ] `migrate.go`, `neo4jadapter/` are deleted.
- [ ] `NewDriver`, `AuthToken`, `NoAuth`, `EagerResult`, `ResultTransformer`, `ExecuteQuery`, `Session`, `ManagedTransaction`, `ManagedTransactionWork`, `Transaction` interfaces are all removed.
- [ ] `QueryCounters`, `SetCounters`, `MapColumnValue`, `SplitLabels`, `NewRecord`, `NewEagerResult` are unexported.
- [ ] Example programs under `examples/backend_switch/`, `examples/copy_from_neo4j/`, `examples/copy_to_neo4j/` compile and include their own `go.mod`.
- [ ] README reflects the new value proposition (no references to `NewDriver`, `DriverCompat`, `CopyFrom`, `CopyTo`).

## Out of Scope

- Bolt wire protocol / network server mode.
- `CopyFrom`/`CopyTo` as first-party methods (covered by examples).
- Spatial, vector, or neo4j-specific temporal types.
- `SessionConfig`, `BookmarkManager`, `AccessMode`, `TransactionConfig`.
- A compatibility shim satisfying `neo4j.DriverWithContext`.

## Open Questions

- None — the API surface is fully specified.

---

## API Surface Reference

### Public Symbols (complete list for v2.0)

**Opening:**
- `func Open(path string, opts ...Option) (*DB, error)`
- `func NewTestDB(t testing.TB, opts ...Option) *DB`
- `func WithBusyTimeout(d time.Duration) Option`
- `func WithReadOnly() Option`
- `type Option func(*dbConfig)` (opaque)

**DB:**
- `func (d *DB) RunQuery(ctx context.Context, cypher string, params map[string]any) (*Result, error)`
- `func (d *DB) BeginTx(ctx context.Context) (*Tx, error)`
- `func (d *DB) Import(ctx context.Context, r io.Reader, format Format) error`
- `func (d *DB) Export(ctx context.Context, w io.Writer, format Format) error`
- `func (d *DB) Snapshot(path string) error`
- `func (d *DB) Close(ctx context.Context) error`

**Tx:**
- `func (t *Tx) Run(ctx context.Context, cypher string, params map[string]any) (*Result, error)`
- `func (t *Tx) Commit() error`
- `func (t *Tx) Rollback() error`
- `func (t *Tx) Close() error`

**Result:**
- `func (r *Result) Next(ctx context.Context) bool`
- `func (r *Result) Record() *Record`
- `func (r *Result) Err() error`
- `func (r *Result) Keys() []string`
- `func (r *Result) Collect(ctx context.Context) ([]*Record, error)`
- `func (r *Result) Single(ctx context.Context) (*Record, error)`
- `func (r *Result) Consume(ctx context.Context) (ResultSummary, error)`

**Record:**
- `func (r *Record) Get(key string) (any, bool)`
- `func (r *Record) Keys() []string`
- `func (r *Record) Values() []any`
- `func (r *Record) AsMap() map[string]any`

**Graph types:**
- `type Node struct { ElementId string; Labels []string; Props map[string]any }`
- `type Relationship struct { ElementId, Type, StartElementId, EndElementId string; Props map[string]any }`

**Generic helpers:**
- `type PropertyValue interface { bool | int64 | float64 | string | []byte | []any | map[string]any }`
- `type RecordValue interface { PropertyValue | Node | Relationship }`
- `func GetProperty[T PropertyValue](entity interface{ getProps() map[string]any }, key string) (T, error)`
- `func GetRecordValue[T RecordValue](record *Record, key string) (T, bool, error)`
- `func CollectT[T any](ctx context.Context, result *Result, mapper func(*Record) (T, error)) ([]T, error)`
- `func SingleT[T any](ctx context.Context, result *Result, mapper func(*Record) (T, error)) (T, error)`

**Result summary:**
- `type ResultSummary interface { Counters() Counters }`
- `type Counters interface { ContainsUpdates() bool; NodesCreated() int; NodesDeleted() int; RelationshipsCreated() int; RelationshipsDeleted() int; PropertiesSet() int }`

**Format:**
- `type Format int`
- `const FormatJSON Format`, `const FormatCSVNodes Format`, `const FormatCSVEdges Format`
- `type ExportFormat int`
- `const ExportFormatJSON ExportFormat`, `const ExportFormatCSV ExportFormat`

**Errors:**
- `var ErrReadOnly`
- `var ErrNoRecords` (new)
- `var ErrMultipleRecords` (new)
- `type ErrUnsupportedCypher struct { Clause string; Position int; Detail string }`
- `type ErrMissingParameter struct { Name string }`
- `type ErrImportDepthExceeded struct { MaxDepth int }`
- `type ErrImportTooLarge struct { MaxBytes int64 }`

### Removed Symbols

| Symbol | Reason |
|---|---|
| `NewDriver(uri, AuthToken, ...Option)` | Requires neo4j driver import |
| `AuthToken` | Vestigial compat |
| `NoAuth()` | Vestigial compat |
| `Driver` interface | Session layer removed |
| `Session` interface | Session layer removed |
| `ManagedTransaction` interface | Session layer removed |
| `ManagedTransactionWork` type | Session layer removed |
| `Transaction` interface | Replaced by concrete `*Tx` |
| `EagerResult` struct | Compat layer only |
| `NewEagerResult` | Compat layer only |
| `ResultTransformer[T]` interface | Compat layer only |
| `EagerResultTransformer()` | Compat layer only |
| `ExecuteQuery[T]()` | Compat layer only |
| `(*DB) CopyFrom` | Requires neo4j driver import |
| `(*DB) CopyTo` | Requires neo4j driver import |
| `(*DB) BeginTx(ctx, bool)` | Bool param removed |
| `(*Tx) Commit(ctx)` | ctx param removed |
| `(*Tx) Rollback(ctx)` | ctx param removed |
| `(*Tx) Close(ctx)` | ctx param removed |
| `QueryResult` type | Renamed to `Result` |
| `QueryCounters` struct | Unexported |
| `(*QueryResult) SetCounters` | Unexported |
| `MapColumnValue` | Unexported |
| `SplitLabels` | Unexported |
| `NewRecord` | Unexported |
| `neo4jadapter/` package | Requires neo4j driver import |
