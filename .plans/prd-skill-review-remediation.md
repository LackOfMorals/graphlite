# PRD: Skill Review Remediation

## Overview

Three automated skill reviews (`golang-security`, `golang-performance`, `golang-design-patterns`) were run against the graphlite codebase and produced 22 actionable findings documented in `docs/skill-review.md`. This PRD captures all findings as implementable requirements, grouped by category and ordered by priority. The work spans security hardening (SQL injection, access-control bypass, foreign-key enforcement, DoS vectors, path traversal), performance improvements (query plan cache, indexed label lookup, allocation reduction, streaming imports, benchmark suite), and structural design improvements (transaction duplication elimination, idempotent rollback, context-aware iteration, typed Labels).

## Goals

- Eliminate all security vulnerabilities identified in the skill review, starting with HIGH/CRITICAL findings.
- Measurably reduce per-query and per-row allocations and eliminate hot-path full-table scans.
- Remove structural maintenance risks (duplication, manual rollback, fragile string rewriting) that make the codebase harder to extend safely.
- Establish a benchmark suite so future regressions are detectable.

## Non-Goals

- This PRD does not add new openCypher syntax support.
- This PRD does not change the public API surface beyond adding a `WithMaxPathHops` option and formalising the `Labels` type (both backward-compatible changes).
- This PRD does not migrate the project away from SQLite or change the storage model beyond adding the `node_labels` junction table.
- This PRD does not address the `buildOptionalNullRow` post-hoc SQL rewriting finding (REQ-NF-SEC-008) — that finding requires translator redesign in a separate effort.
- This PRD does not implement `WithQueryTimeout` as a configurable option; the recommendation is documentation-only for now.

## Requirements

### Functional Requirements — Security

**REQ-F-SEC-001: Reject `RawExpr` rather than interpolating verbatim SQL**
- File: `sql/translator.go`, line 1617–1620
- The `case *cypher.RawExpr` branch currently returns `e.Text` directly into the generated SQL string, creating a SQL injection vector for any Cypher sub-expression the translator cannot classify.
- Requirement: Replace the `return e.Text, nil` with `return "", fmt.Errorf("sql: unsupported expression %q: cannot translate to safe SQL", e.Text)`. If future callers need best-effort passthrough for trusted internal use, gate it behind an explicit allowlist validated with the `isIdentifier` function from `cypher/planner.go`.
- Acceptance: No path through `exprToSQL` returns unvalidated user-supplied text into the SQL string.

**REQ-F-SEC-002: Validate property key names before JSON path construction**
- File: `sql/translator.go`, approximately lines 649, 723, 742 (all sites that build `$.key` JSON path strings)
- Property keys sourced from the Cypher AST are concatenated directly into `json_extract(col, '$.key')` fragments. The existing `escapeJSONPath` function escapes single quotes but does not reject bracket characters, null bytes, or other JSON path metacharacters.
- Requirement: Before building any `$.key` JSON path fragment, validate the key with the `isIdentifier` function already present in `cypher/planner.go`. Return a descriptive error if validation fails. Document which characters are rejected and why.
- Acceptance: A property key containing `]`, `[`, or a null byte causes `exprToSQL` to return an error rather than producing a malformed JSON path.

**REQ-F-SEC-003: Enforce `PRAGMA foreign_keys = ON` at open time**
- File: `store/sqlite.go`, `Open` function (lines 36–67); `store/schema.go`
- The schema declares `REFERENCES nodes(id)` on the `edges` table, but SQLite silently ignores foreign key constraints unless `PRAGMA foreign_keys = ON` is set per connection. Currently no such PRAGMA is executed, so dangling edges can be inserted.
- Requirement: Add `PRAGMA foreign_keys = ON;` to the `Open` function in `store/sqlite.go`, after the `PRAGMA journal_mode=WAL` line and before schema DDL application. Return an error if the PRAGMA execution fails.
- Acceptance: Attempting to insert an edge whose `start_id` or `end_id` does not exist in `nodes` returns a `FOREIGN KEY constraint failed` error.

**REQ-F-SEC-004: Apply hard cap to user-supplied `MaxHops`**
- File: `sql/translator.go`, approximately line 868 (variable-length path translation)
- The `safetyLimit = 15` constant is applied only when the path is unbounded (`[*]`). An explicit `MaxHops` value (e.g. `[*1..1000]`) bypasses the cap, generating a recursive CTE that can exhaust stack/memory.
- Requirement: Apply the same `safetyLimit` cap to explicit `MaxHops` values. If `MaxHops > safetyLimit`, clamp it and log a warning, or return an error. Expose a `WithMaxPathHops(n int)` option on `DB` so callers can raise the cap deliberately, with documented risk.
- Acceptance: A Cypher query `MATCH p=(a)-[*1..1000]->(b) RETURN p` on an unmodified `DB` is rejected or clamped at 15 hops.

**REQ-F-SEC-005: Add `WithReadOnly` enforcement to `BeginTx`**
- File: `driver.go`, `BeginTx` function (lines 150–160)
- `RunQuery` (line 140–148) checks `d.readOnly` and returns `ErrReadOnly` for mutation statements, but `BeginTx` has no such guard. A caller using `WithReadOnly()` on `Open` can still execute writes by opening a transaction and calling `tx.Run`.
- The doc comment on `WithReadOnly` mentions `PRAGMA query_only=ON` but this PRAGMA is never executed.
- Requirement: Either (a) check `d.readOnly` in `BeginTx` and return `ErrReadOnly` immediately, preventing write transactions from being opened, or (b) issue `PRAGMA query_only=ON` immediately after `Open` when `WithReadOnly` is set, enforcing read-only at the SQLite engine level. Remove or correct the misleading PRAGMA mention in the doc comment.
- Acceptance: Opening a DB with `WithReadOnly()`, then calling `db.BeginTx(ctx)` followed by `tx.Run(ctx, "CREATE (n:X)", nil)` returns an error consistent with the read-only contract.

**REQ-F-SEC-006: Apply path-traversal protection to `Snapshot`**
- File: `store/sqlite.go`, `Snapshot` function (lines 90–96)
- `Open` (lines 79–104 of `driver.go`) applies `filepath.Clean` and symlink resolution to reject `..`-based traversal. `Snapshot` applies only single-quote escaping and has no equivalent traversal protection.
- Requirement: Apply the same `filepath.Clean` / parent-directory symlink resolution logic to the `path` argument in `Snapshot` before executing `VACUUM INTO`. If the resolved path contains `..` components or resolves outside the working directory in the same manner as `Open` enforces, return an error.
- Acceptance: `db.Snapshot("../../etc/malicious.db")` returns an error without executing `VACUUM INTO`.

**REQ-F-SEC-007: Escape `%` and `_` in label names passed to `LabelContains`**
- File: `sql/dialect.go`, `LabelContains` function (lines 147–154)
- Label names are bound as LIKE pattern arguments without escaping SQLite LIKE wildcards. A label name containing `%` or `_` is treated as a wildcard pattern and returns incorrect results.
- Requirement: Apply LIKE-wildcard escaping to `labelName` before binding it in all four LIKE branches. Use the same escaping already present in `stringMatchPattern` (approximately lines 1715–1718 of `sql/translator.go`): escape `%` as `\%`, `_` as `\_`, and append `ESCAPE '\'` to each LIKE predicate. Update the `LabelContains` function signature comment to document this escaping.
- Acceptance: A node with label `"50%_off"` is matched only by an exact label query `MATCH (n:\`50%_off\`)`, not by `MATCH (n:Person)`.

**REQ-F-SEC-008: Implement full openCypher escape sequences in `unquoteString`**
- File: `cypher/parser.go`, `unquoteString` function (lines 1607–1623)
- Only five escape sequences are currently handled (`''`, `""`, `\\`, `\'`, `\"`). The openCypher spec requires `\n`, `\r`, `\t`, `\b`, `\f`, and `\uXXXX` Unicode escapes. Unhandled sequences are silently passed through as literal backslashes, causing incorrect string values at runtime.
- Requirement: Extend `unquoteString` to handle the complete openCypher escape sequence table: `\n` → newline, `\r` → carriage return, `\t` → tab, `\b` → backspace, `\f` → form feed, `\uXXXX` → Unicode code point. Unrecognised backslash sequences should return an error (or optionally pass through for forward-compatibility, with a comment explaining the choice).
- Acceptance: The Cypher literal `'Hello\nWorld'` produces a Go string containing a real newline character. `'A'` produces `"A"`.

**REQ-F-SEC-009: Upgrade stale ANTLR and opencypher dependencies**
- File: `go.mod`
- `github.com/antlr/antlr4/runtime/Go/antlr v0.0.0-20210803070921` is a 2021 pre-release and `github.com/cloudprivacylabs/opencypher v1.0.0` is from 2022. The ANTLR runtime has a history of parser DoS issues.
- Requirement: Run `go get github.com/antlr/antlr4/runtime/Go/antlr@latest` and `go get github.com/cloudprivacylabs/opencypher@latest`, then `go mod tidy`. If a newer version introduces breaking API changes, adapt call sites. Integrate `govulncheck ./...` into the CI pipeline (add to `scripts/` or `.github/workflows/`).
- Acceptance: `go.mod` pins both dependencies to versions released in 2024 or later. `govulncheck ./...` reports no known vulnerabilities.

### Functional Requirements — Performance

**REQ-F-PERF-001: Add a query plan cache to avoid redundant ANTLR parses**
- File: `driver.go`, `buildSQLResult` function
- Every call to `RunQuery` executes the full parse → plan → translate pipeline regardless of whether the same Cypher string has been seen before. On repeated identical queries this is a significant unnecessary overhead.
- Requirement: Add a plan cache (a `sync.Map` or bounded LRU, keyed on the Cypher string) that stores the translated `glsql.Result` (minus bound parameter values). On a cache hit, skip parse/plan/translate and call only `BindParams` with the current parameter values. On a cache miss, run the full pipeline and store the result.
- Cache invalidation: entries are permanent for the lifetime of the DB (Cypher → SQL translation is deterministic and stateless).
- Acceptance: `BenchmarkBuildSQLResult_CacheHit` (see REQ-F-PERF-006) demonstrates cache hits are at least 5× faster than cache misses for a simple `MATCH (n) RETURN n` query.

**REQ-F-PERF-002: Replace LIKE-based label lookup with a `node_labels` junction table**
- Files: `store/schema.go`, `store/sqlite.go` (all `listNodesByLabel` callers), `sql/dialect.go` (`LabelContains`), `sql/translator.go` (label predicate generation)
- The current `LabelContains` generates four LIKE predicates, three of which start with or contain wildcards and cannot use the `idx_nodes_labels` B-tree index. Every `MATCH (n:Label)` query on multi-label nodes is a full table scan.
- Requirement:
  1. Add a `node_labels(node_id INTEGER NOT NULL REFERENCES nodes(id), label TEXT NOT NULL)` junction table to `store/schema.go` with a covering index `CREATE INDEX idx_node_labels_label ON node_labels(label, node_id)`.
  2. Populate `node_labels` rows in `insertNode` and maintain them in `updateNodeLabels` (new helper). Delete matching rows in `deleteNode`.
  3. Update `listNodesByLabel` to `SELECT n.* FROM nodes n JOIN node_labels nl ON n.id = nl.node_id WHERE nl.label = ?`.
  4. Update the SQL translator's label predicate generation to use a subquery or JOIN against `node_labels` rather than calling `LabelContains`.
  5. Update `LabelContains` in `dialect.go` to use the junction table pattern; keep the old implementation behind a build tag or remove it after all call sites are updated.
- Acceptance: `BenchmarkMatchByLabel` (see REQ-F-PERF-006) on a 10,000-node graph with mixed labels shows no full table scan in the SQLite query plan (`EXPLAIN QUERY PLAN` returns an index scan on `idx_node_labels_label`).

**REQ-F-PERF-003: Remove redundant `GetNode` lookups on CSV edge import**
- File: `importer.go`, `importCSVEdges` function (approximately line 308 area and surrounding edge import loop)
- Two `SELECT` round-trips per edge row are executed to validate that the start and end nodes exist before inserting the edge. Once `PRAGMA foreign_keys = ON` is enforced (REQ-F-SEC-003), the database engine enforces this constraint natively.
- Requirement: Remove the two `GetNode` / existence-check calls per edge row in `importCSVEdges`. Rely on the foreign key constraint to reject dangling edges. Handle the resulting SQLite `FOREIGN KEY constraint failed` error and map it to the appropriate graphlite error type.
- Acceptance: CSV edge import throughput improves measurably (tracked by `BenchmarkImportCSVEdges` in REQ-F-PERF-006). No regression in correctness: importing an edge with a non-existent node ID still fails with a clear error.

**REQ-F-PERF-004: Eliminate triple JSON parse pass in `importJSON`**
- File: `importer.go`, `decodeImportJSON` function (approximately lines around the `importMaxDepth` check)
- The current implementation performs three passes over the input: `json.NewDecoder` decode, `checkJSONDepth` second scan, and `json.Unmarshal`. All three passes process the same data.
- Requirement: Merge the depth check and decode into a single streaming pass. Implement a custom `json.Decoder`-based reader that tracks nesting depth while decoding into the target struct, eliminating the separate `checkJSONDepth` scan. The `ErrImportDepthExceeded` sentinel must still be returned when depth exceeds `importMaxDepth`.
- Acceptance: `BenchmarkImportJSON` (see REQ-F-PERF-006) on a 1,000-node payload shows a reduction in CPU time consistent with eliminating two of three parse passes.

**REQ-F-PERF-005: Reduce per-row heap allocations in `result.go` and streaming result paths**
- Files: `result.go` (`Next` function), `driver.go` (`collectMatchRows` function)
- `result.go:Next` allocates three slices (`rawVals`, `ptrs`, `vals`) on every call. `driver.go:collectMatchRows` allocates `vals`, `ptrs`, and `row` slices per row. These drive GC pressure at high query rates.
- Requirement:
  1. Pre-allocate `rawVals`, `ptrs`, and `vals` on the `Result` struct during construction (sized to `len(r.keys)`). Reuse them across `Next` calls.
  2. In `collectMatchRows`, pre-allocate `ptrs` once before the row loop and reuse by resetting (not reallocating) per row.
  3. Optionally: where callers do not retain the `*Record` across loop iterations, consider a `Record` design that holds a reusable `[]any` values slice reset per row. (This is optional if the per-row map allocation is acceptable given the improvement from points 1–2.)
- Acceptance: Allocation count per `Next` call in `BenchmarkCollectLargeResult` (see REQ-F-PERF-006) decreases by at least 3 allocations per row vs. the pre-fix baseline.

**REQ-F-PERF-006: Add benchmark suite for core hot paths**
- File: `bench/` directory (new) or `driver_bench_test.go`, `result_bench_test.go`, `importer_bench_test.go`
- No `Benchmark*` functions exist in the repository. Regressions in query throughput, import speed, and per-row allocation are undetectable.
- Requirement: Add the following benchmarks (at minimum):
  - `BenchmarkRunQuerySimpleSelect` — `MATCH (n) RETURN n` on a 100-node graph, auto-commit mode
  - `BenchmarkRunQueryCreateNode` — `CREATE (n:Person {name: $name}) RETURN n`, auto-commit
  - `BenchmarkCollectLargeResult` — `MATCH (n) RETURN n` on a 10,000-node graph, full `Collect`
  - `BenchmarkImportJSON` — `Import` of a 1,000-node JSON payload
  - `BenchmarkBuildSQLResult_CacheHit` — `buildSQLResult` / `runQuery` called 1,000× with same Cypher string; demonstrates plan cache benefit
  - `BenchmarkMatchByLabel` — `MATCH (n:Person) RETURN n` on a 10,000-node graph with 50% label match; demonstrates junction table benefit
- Each benchmark must call `b.ReportAllocs()`.
- Acceptance: All six benchmarks exist, compile, and run to completion with `go test -bench=. -benchmem ./...`.

### Functional Requirements — Design Patterns

**REQ-F-DESIGN-001: Collapse `sqliteTx` 14-method override duplication**
- File: `store/sqlite.go`, `sqliteTx` struct and its 14 override methods (lines 193–280)
- `sqliteTx` overrides all 14 `Store` CRUD methods to forward to the same private helper functions with `t.tx` instead of `s.db`. This is a maintenance risk: adding a new Store method requires updating both `SQLiteStore` and `sqliteTx`.
- Requirement: Refactor so that `SQLiteStore` holds a single `querier` field (the `querier` interface is already defined at line 288). Set it to `*sql.DB` at `Open` time and to `*sql.Tx` at `Begin` time. Remove `sqliteTx` as a separate struct — `Begin` returns a `*SQLiteStore` with the `querier` set to the transaction. The 14 override methods collapse to the `SQLiteStore` implementations, which already delegate to helper functions taking a `querier`.
- Acceptance: `store/sqlite.go` no longer contains a `sqliteTx` struct or any per-method forwarding overrides. All existing `store/crud_test.go` and `store/store_test.go` tests pass.

**REQ-F-DESIGN-002: Use defer-based transaction rollback in `importer.go`**
- File: `importer.go`, functions `importJSON`, `importCSVNodes`, `importCSVEdges`
- Each function contains 10+ manual `_ = tx.Rollback()` calls spread across error paths. A future developer adding an early return will silently omit a rollback.
- Requirement: Refactor all three import functions to use the standard Go defer+named-return pattern:
  ```go
  func (d *DB) importJSON(ctx context.Context, r io.Reader) (retErr error) {
      tx, err := d.st.Begin(ctx)
      if err != nil { return err }
      defer func() {
          if retErr != nil { _ = tx.Rollback() }
      }()
      // ... body with no manual Rollback calls ...
      return tx.Commit()
  }
  ```
- Remove all inline `_ = tx.Rollback()` calls from the function bodies after the defer is in place.
- Acceptance: `grep -n 'tx.Rollback' importer.go` returns zero results outside of the deferred closure.

**REQ-F-DESIGN-003: Make `Rollback` idempotent (no-op after `Commit`)**
- File: `tx.go`, `Rollback` function (lines 45–53)
- The current `Rollback` returns an error when called on a transaction whose `done` flag is set. The standard Go convention (following `database/sql.Tx`) is that `Rollback` after `Commit` is a no-op returning `nil`. The current behaviour breaks the canonical `defer tx.Rollback()` + `tx.Commit()` pattern.
- Requirement: Change the `if t.done` branch in `Rollback` to return `nil` instead of an error. Add a comment explaining that this follows the `database/sql` convention.
- Acceptance: The following pattern works without error:
  ```go
  tx, _ := db.BeginTx(ctx)
  defer tx.Rollback() // must not error after Commit
  _ = tx.Run(ctx, "CREATE (n:X)", nil)
  tx.Commit()
  ```

**REQ-F-DESIGN-004: Respect context cancellation in `Result.Next`**
- File: `result.go`, `Next` function (line 65)
- `Next(_ context.Context)` accepts a context but ignores it entirely. This creates a false API contract: callers who pass a cancellable context expect the iteration to stop on cancellation; it does not.
- Requirement: Add a context check at the top of `Next`:
  ```go
  func (r *Result) Next(ctx context.Context) bool {
      if ctx.Err() != nil {
          r.err = ctx.Err()
          r.consumed = true
          return false
      }
      // existing body unchanged
  }
  ```
- Acceptance: A test that cancels the context mid-iteration sees `Next` return `false` and `result.Err()` return the context error (e.g. `context.Canceled`).

**REQ-F-DESIGN-005: Define a `Labels` type to replace comma-encoded strings**
- Files: `types.go` (or a new `labels.go`), `store/store.go`, `store/sqlite.go`, `importer.go`, `sql/dialect.go`, `sql/translator.go`, `result.go`
- Node labels are stored and passed as a comma-separated string (`"Person,Employee"`) throughout the codebase. This leaks the encoding into the `Store` interface, `NodeRow.Labels`, import/export, `splitLabels`, and the LIKE-based SQL query. The encoding makes label equality checks fragile and forces LIKE-based SQL.
- Requirement:
  1. Define `type Labels []string` in `types.go` (or a new `labels.go`).
  2. Add `func (l Labels) Encode() string` (produces comma-separated form) and `func DecodeLabels(s string) Labels` (splits on comma, handles empty string as nil/empty).
  3. Update `NodeRow.Labels` from `string` to `Labels`.
  4. Update `Store` interface parameters that accept/return label strings to use `Labels`.
  5. Update all internal callers (`insertNode`, `getNode`, `listNodesByLabel`, `importer.go`, `result.go`) to use `Labels.Encode()` / `DecodeLabels()` rather than raw comma manipulation.
  6. The underlying SQLite storage format (comma-separated text) does not change in this task — that migration is deferred to REQ-F-PERF-002 (junction table).
- Acceptance: No call site in non-store code directly constructs or parses a comma-separated label string. The public `Node.Labels` field is of type `Labels` (or `[]string` — consistent with whichever type the v2 API exposes).

### Non-Functional Requirements

**REQ-NF-001: All existing tests pass after each requirement is implemented**
- `go test ./...` must pass after each individual requirement is completed. No requirement may be deferred on the grounds that it breaks existing tests without simultaneously fixing the tests.

**REQ-NF-002: `go vet ./...` and `golangci-lint run` pass with no new findings**
- The linter baseline established on branch `feature/v2-api-redesign` must not regress. All new code must pass existing lint rules.

**REQ-NF-003: No public API breakage beyond documented changes**
- The only intentional public API changes in this PRD are: (a) `WithMaxPathHops(n int)` option added to `options.go`, (b) `Node.Labels` type changed from `string` to `Labels`/`[]string` (if REQ-F-DESIGN-005 promotes it). All other changes are internal.

**REQ-NF-004: `govulncheck` integration in CI**
- As part of REQ-F-SEC-009, `govulncheck ./...` must be added to the CI pipeline and must report no known vulnerabilities on merge.

**REQ-NF-005: Benchmark baseline established before optimisation work**
- REQ-F-PERF-006 (benchmark suite) must be implemented before REQ-F-PERF-001 through REQ-F-PERF-005 so that before/after numbers can be captured.

## Technical Considerations

**Transaction refactoring (REQ-F-DESIGN-001):** The `querier` interface is already defined at `store/sqlite.go:288`. The refactor consists of changing `SQLiteStore.db *sql.DB` to `SQLiteStore.q querier` and updating `Open` to set `q = db` and `Begin` to return a `&SQLiteStore{q: tx}`. The `Exec()` method on `SQLiteStore` currently returns `s.db` — it would need to return `s.q` (after a type assertion or interface change). Review all `SQLiteStore.DB()` callers before removing the field.

**Query plan cache (REQ-F-PERF-001):** The cache key is the raw Cypher string. The cached value must not include the bound `[]any` arguments — those are injected by `BindParams` on each call. The `glsql.Result` struct stores translated SQL and placeholder positions; verify that it is safe to share across goroutines (read-only after construction). A `sync.Map` is the simplest implementation; an LRU with a cap of 1,024 entries is recommended to avoid unbounded memory growth.

**Junction table migration (REQ-F-PERF-002):** The `schemaDDL` in `store/schema.go` uses `IF NOT EXISTS` guards, making it safe to add the new table and index to existing databases. However, existing databases will have a `node_labels` table with no rows. A migration step is needed: after creating the table, run `INSERT INTO node_labels(node_id, label) SELECT id, value FROM nodes, json_each('[' || replace(labels, ',', '","') || ']')` to backfill existing data. Add this migration to `Open` behind a check (e.g., `SELECT COUNT(*) FROM node_labels` vs. `SELECT COUNT(*) FROM nodes`).

**`Labels` type compatibility (REQ-F-DESIGN-005):** The public `Node` struct currently has `Labels string` (comma-encoded). Changing it to `Labels []string` or the new `Labels` type is a breaking change for any caller inspecting `Node.Labels`. Since this is a v2 branch, the change is acceptable. However, serialisation (JSON export) must be updated so exported JSON uses `"labels": ["Person", "Employee"]` rather than `"labels": "Person,Employee"`.

**Foreign key enforcement and existing tests (REQ-F-SEC-003):** Enabling `PRAGMA foreign_keys = ON` may cause existing tests that insert edges with synthetic IDs (in CRUD tests) to fail if the node IDs are not pre-inserted. Audit `store/crud_test.go` before this change and fix any test that assumes unconstrained edge insertion.

**ANTLR upgrade (REQ-F-SEC-009):** The ANTLR v4 Go runtime has renamed its module path in recent versions. Confirm the new import path before upgrading and update all `antlr` imports in `cypher/` accordingly.

## Acceptance Criteria

The remediation is complete when all of the following are true:

1. `go test ./...` passes with 100% of existing tests green.
2. `golangci-lint run` produces no new findings vs. the pre-remediation baseline.
3. `govulncheck ./...` reports no known vulnerabilities.
4. `EXPLAIN QUERY PLAN` for `MATCH (n:Person) RETURN n` on a multi-label dataset shows an index scan on `node_labels`, not a full scan on `nodes`.
5. A `MATCH (n)-[*1..1000]->(m) RETURN n` query on a default `DB` is rejected or clamped to 15 hops.
6. `db.Snapshot("../../malicious.db")` returns an error without executing `VACUUM INTO`.
7. `db.RunQuery(ctx, "MATCH (n) WHERE n.name = $v RETURN n", {"v": "'; DROP TABLE nodes;--"})` executes safely without SQL injection.
8. `grep -n 'tx.Rollback' importer.go` returns only the deferred closure (zero inline calls).
9. All six benchmarks from REQ-F-PERF-006 compile and run successfully.
10. `go.mod` pins `antlr` and `opencypher` to 2024-or-later releases.

## Out of Scope

- Rewriting `buildOptionalNullRow` to avoid post-hoc SQL string rewriting (finding 8 in the security review). This requires translator redesign and is tracked separately.
- Adding `WithQueryTimeout(d time.Duration)` as a configurable option. The recommendation is to document that callers should use `context.WithTimeout`.
- Migrating `StatementKind` from `int`+`iota` to a sealed interface. The existing approach is acceptable and the inconsistency can be addressed with a comment.
- Addressing all medium-priority performance findings beyond REQ-F-PERF-001 through REQ-F-PERF-005 (e.g., `sync.Pool` for `BindingScope`, `strings.Builder` in translator, `sync.Pool` for sub-`Translator` instances, `ResolveIDsInStatement` helper). These are tracked as future work.
- Modern Go idiom gaps (`slices.Sort`, `strings.CutPrefix`, `t.Context()`) — these are cosmetic and should be addressed in a separate housekeeping PR.

## Open Questions

1. **Plan cache size:** Should the query plan cache be unbounded (simplest, safe for typical use), or capped at a configurable limit via `WithPlanCacheSize(n int)`? An unbounded cache risks memory growth if the application generates many distinct Cypher strings dynamically.

2. **`Labels` type in public API:** Should `Node.Labels` become `graphlite.Labels` (a named type with marshal/unmarshal methods) or `[]string` (simpler, no new type)? The named type allows future behaviour changes without a breaking API change; `[]string` is more immediately usable.

3. **MaxHops behaviour — clamp vs. error:** Should a `MaxHops` value exceeding the cap (a) silently clamp to the cap (callers get results but fewer than requested), or (b) return an error (callers know their query was rejected)? Clamping is more user-friendly; erroring is safer and more explicit.

4. **ANTLR upgrade API breakage:** The ANTLR v4 Go runtime changed its module path from `github.com/antlr/antlr4/runtime/Go/antlr` to `github.com/antlr4-go/antlr/v4` in v4.13.0. Confirm whether the `cloudprivacylabs/opencypher` dependency pins the old path — if so, upgrading both simultaneously may be necessary.

5. **Junction table backfill strategy for existing databases:** The proposed backfill query uses `json_each` with string manipulation to split the comma-separated labels column. Is this approach reliable for label names containing commas (disallowed) or special characters? Confirm that the existing import validation rejects such labels before relying on this backfill.
