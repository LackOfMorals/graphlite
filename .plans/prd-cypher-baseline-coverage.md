# PRD: Cypher Baseline Coverage (UNWIND, CALL {} subqueries, scalar functions)

## Overview

graphlite's README advertises a "100% openCypher TCK pass rate (235/235 scenarios)," but that figure only counts scenarios that are *executed* — `compat/tck_test.go:91-145` maintains an `unsupportedPatterns` allowlist that skips any scenario whose text contains `UNWIND`, `CALL {`, `FOREACH`, `UNION`, list comprehensions, or any of ~25 ordinary scalar functions (`toLower`, `size`, `abs`, `coalesce`, `id`, `labels`, etc.). These are not exotic features — they are baseline vocabulary that any query ported from Neo4j, Kuzu, or Apache AGE is likely to use. Today, `cypher/parser.go:1392-1425` (`buildFunctionInvocation`) recognizes only the aggregate functions (`count`, `sum`, `avg`, `min`, `max`, `collect`) and `exists()`; every other function call falls through to `&RawExpr{Text: ...}`, which `sql/translator.go:1718-1727` then rejects with `sql: unsupported expression %q: complex expressions are not yet supported in this context` because the raw text fails the identifier allowlist. This PRD closes that gap for `UNWIND`, scalar functions, and `CALL {}` subqueries — the three largest sources of skipped TCK scenarios — without touching variable-length paths, `shortestPath()`, or list/pattern comprehensions (covered by a separate PRD).

## Goals

- Implement `UNWIND <list-expr> AS <var>` as a first-class clause, translated to a `json_each()` lateral join against a JSON-encoded list.
- Implement the ~20 scalar functions currently listed in `compat/tck_test.go`'s skip list: `toLower`, `toUpper`, `trim`, `split`, `size`, `length`, `abs`, `ceil`, `floor`, `round`, `type`, `labels`, `keys`, `id`, `nodes`, `relationships`, `head`, `tail`, `last`, `toString`, `toInteger`, `toFloat`, `toBoolean`, `range`, `coalesce`.
- Implement `CALL { <sub-query> }` uncorrelated and correlated subqueries, translated to a correlated SQL subquery/CTE.
- Remove the corresponding entries from `compat/tck_test.go`'s `unsupportedPatterns` list as each feature lands, and report the new (larger, more meaningful) executed/pass counts in the README.
- Preserve the existing `ErrUnsupportedCypher` behavior for anything still genuinely unsupported (no silent wrong answers).

## Non-Goals

- `UNION`/`UNION ALL` (separate, smaller PRD if pursued — no CTE/algorithm dependency on this work).
- `FOREACH`.
- List comprehensions (`[x IN list WHERE ... | ...]`), pattern comprehensions, and the `any()`/`all()`/`none()`/`single()`/`extract()`/`filter()`/`reduce()` predicate functions — these share machinery with comprehensions and are out of scope here.
- `shortestPath()` / `allShortestPaths()` — covered by `prd-shortest-path-traversal.md`, which builds on the existing variable-length-path recursive CTE machinery.
- `RETURN *`.
- Named path variables (`p = (a)-->(b)`).

## Requirements

### Functional Requirements

- REQ-F-001: `cypher/ast.go` gains an `UnwindClause` type (`Expr Expr`, `Variable string`) alongside the existing `WithClause`/`MatchClause` pattern.
- REQ-F-002: `cypher/parser.go` gains `buildUnwindClause`, wired into `buildReadingClause` (`cypher/parser.go:226`) or a new updating/reading dispatch point, consistent with how `MATCH` and `WITH` are threaded through `buildSinglePartQuery`/`buildMultiPartQuery`.
- REQ-F-003: `cypher/planner.go` gains a `planUnwindClause` producing a new `UnwindPlan` logical-plan node (`cypher/plan.go`), binding `Variable` into the `BindingScope` as a scalar (not a node/relationship).
- REQ-F-004: `sql/translator.go` translates `UnwindPlan` into `, json_each(<list-json-expr>) AS <alias>` in the `FROM`/join clause, exposing `<alias>.value` as the bound variable's column — following the same CTE/join-collection pattern already used for `buildFromClauseForVarLengthRel` (`sql/translator.go:908-1044`).
- REQ-F-005: `buildFunctionInvocation` (`cypher/parser.go:1392`) gains a `ScalarCallExpr` case (new `cypher/plan.go` node) for each function in scope, replacing the current `default: RawExpr` fallthrough for those specific names only — everything else still falls back to `RawExpr` and is still rejected by the translator's identifier allowlist.
- REQ-F-006: `sql/translator.go` translates each `ScalarCallExpr` to its SQLite equivalent:
  - String: `toLower`→`LOWER`, `toUpper`→`UPPER`, `trim`→`TRIM`, `split`→a JSON array built from SQLite's string split (no native `STRING_SPLIT`; reuse the same recursive-CTE splitting technique already used for `labels` in `store/schema.go`'s trigger, or `json_each` over a manually-split string), `size`/`length`→`LENGTH` (string) or `json_array_length` (list), `toString`/`toInteger`/`toFloat`/`toBoolean`→`CAST`.
  - Math: `abs`/`ceil`/`floor`/`round`→native SQLite math functions (`ROUND`, and `CEIL`/`FLOOR` via `modernc.org/sqlite`'s math extension, verified available — see Technical Considerations).
  - Graph-shape: `type(r)`→a literal/column reference to `edges.type`; `labels(n)`→`json_array` built from `node_labels` rows (reusing the existing label-index infrastructure, not the raw `labels` column); `keys(n)`/`keys(r)`→`json_each(props)` key aggregation; `id(n)`/`id(r)`→the existing `nodes.id`/`edges.id` column already selected internally.
  - List: `head`/`tail`/`last`/`range`/`coalesce`→JSON-aware SQL expressions (`json_extract(x, '$[0]')`, etc.) or, where SQLite JSON functions are insufficient, a `CASE` fallback.
  - `nodes(path)`/`relationships(path)` require a materialized path value — defer to whatever path-representation `prd-shortest-path-traversal.md` introduces; if that PRD is not done first, implement these two against an empty/degenerate path type and mark them `ErrUnsupportedCypher` until path values exist.
- REQ-F-007: `cypher/ast.go` gains a `CallSubqueryClause` type wrapping a nested `*Query`. `cypher/parser.go` parses `CALL { ... }` via a new `buildCallSubqueryClause`, reusing `buildSinglePartQuery`/`buildMultiPartQuery` recursively for the inner query.
- REQ-F-008: `cypher/planner.go` plans a correlated `CALL {}` subquery by threading the outer `BindingScope`'s currently-bound variables into the inner query's scope (correlated case), or an isolated scope (uncorrelated case, per openCypher semantics: variables from the outer scope are visible only if explicitly imported via `WITH` inside the subquery in strict Cypher, but graphlite may choose to always correlate for v1 — see Open Questions).
- REQ-F-009: `sql/translator.go` translates a `CallSubqueryClause` into a correlated SQL subquery (a lateral-style join emulated via a correlated scalar/table subquery, since SQLite lacks `LATERAL` — use a correlated `IN`/`EXISTS`-style subquery per matched outer row, following the existing per-matched-row execution pattern already used for `MATCH`+write in `driver.go:356-423` (`execWriteThenSelect`'s per-row loop) as a model for "run inner query once per outer row" semantics if a single SQL statement cannot express it).
- REQ-F-010: Each of the three features gets its own `.feature`-driven or hand-written test fixtures under `testdata/`, plus unit tests in `cypher/`, `sql/`, and root-package integration tests.

### Non-Functional Requirements

- REQ-NF-001: `go build ./...`, `go vet ./...`, and `CGO_ENABLED=0 go test -tags=unit -count=1 ./...` all pass after each of the three features lands.
- REQ-NF-002: The `store/` package is not touched — no new Cypher-typed data crosses into `store/`, consistent with `AGENTS.md`'s architectural constraint.
- REQ-NF-003: The `cypher/` package still does not import `store/` or `sql/`.
- REQ-NF-004: All new SQL fragments use parameterised binding for values (`?` placeholders); the SQLite math/JSON function names themselves are compile-time constants, never built from user input.
- REQ-NF-005: `compat/tck_test.go`'s `passRate < 50.0%` gate continues to pass, and the printed executed/skipped counts are captured in each task's progress log so the delta is visible task-by-task.
- REQ-NF-006: The plan cache (`plan_cache.go`) requires no changes — new clause/expression types are cached the same way existing ones are (keyed on Cypher string, pre-`BindParams`).

## Technical Considerations

**Why `json_each()` for UNWIND:** graphlite already stores all list-valued properties as JSON (`props JSON` column, per `AGENTS.md`'s storage schema). SQLite's built-in `json_each(json)` table-valued function turns a JSON array into a row set with `key`/`value` columns, which is exactly UNWIND's semantics — no new storage or extension is needed.

**`modernc.org/sqlite` math functions:** Standard SQLite does not build in `CEIL`/`FLOOR`/`ABS` beyond `ABS` (which is built in). Verify at task time whether `modernc.org/sqlite` compiles with `SQLITE_ENABLE_MATH_FUNCTIONS` (it does, as of the versions this repo has used historically) before relying on `CEIL`/`FLOOR`; if unavailable, fall back to `CASE WHEN x = CAST(x AS INT) THEN x ELSE CAST(x AS INT) + (x > 0) END`-style expressions.

**`labels()` must go through `node_labels`, not the raw column:** `AGENTS.md` is explicit that label lookups should use `node_labels`/`idx_node_labels_label`, not `LIKE` on `nodes.labels`. `labels(n)` should aggregate from `node_labels` (`json_group_array(label)` with a subquery keyed on `node_id`), keeping the storage abstraction intact.

**`CALL {}` correlation semantics are a real design decision, not just plumbing** — see Open Questions.

**Split() has no native SQLite equivalent** — this is the single trickiest scalar function. The existing `node_labels` trigger already solves "split comma-separated text via recursive CTE" (`store/schema.go`); the same recursive-CTE pattern, parameterised by delimiter, is the natural reuse.

## Acceptance Criteria

- [ ] `UNWIND [1,2,3] AS x RETURN x` executes and returns 3 rows.
- [ ] `UNWIND` correctly scopes `x` so it participates in subsequent `WHERE`/`RETURN`/aggregation.
- [ ] All 24 scalar functions in `compat/tck_test.go`'s current skip list are removed from that list and have passing TCK scenarios.
- [ ] `CALL { MATCH (n:Person) RETURN n.name AS name } RETURN name` executes for the uncorrelated case.
- [ ] A correlated `CALL {}` referencing an outer variable executes per matched outer row and returns the correct cardinality.
- [ ] `compat/tck_test.go`'s reported executed-scenario count increases materially (baseline vs. after, captured in progress logs); pass rate on executed scenarios remains ≥ 50% (ideally 100%).
- [ ] No regression in existing unit test suite or `go vet`.
- [ ] README's feature table and TCK pass-rate claim are updated to reflect the new, larger executed-scenario denominator.

## Out of Scope

- `UNION`/`UNION ALL`, `FOREACH`, `RETURN *`, named path variables.
- List/pattern comprehensions and `any`/`all`/`none`/`single`/`extract`/`filter`/`reduce`.
- `shortestPath()`/`allShortestPaths()` (separate PRD).
- Any change to `store/` schema or indexes.

## Open Questions

- **`CALL {}` correlation semantics**: does graphlite implement strict openCypher scoping (subquery sees outer variables only via explicit importing `WITH`), or always-correlated (simpler, more permissive)? Recommend starting with always-correlated for v1 and tightening later if TCK scenarios demand strict scoping — needs a decision before REQ-F-008/REQ-F-009 are implemented.
- Should `split()`'s delimiter be restricted to a single character for the v1 recursive-CTE implementation, or must multi-character delimiters be supported immediately? Affects the CTE's `INSTR`-based splitting logic.
