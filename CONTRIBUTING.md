# Contributing to graphlite

Thank you for your interest in contributing to graphlite. This document covers prerequisites, how to run all test suites, how to add a new Cypher feature, and the benchmark baseline process.

---

## Prerequisites

- Go 1.24 or newer (matches the `go` directive in `go.mod`)
- No CGO required — graphlite uses `modernc.org/sqlite`, a pure-Go SQLite driver
- `git` for version control

No Docker, no database server, no external services. Everything runs in-process.

---

## Getting started

```bash
git clone https://github.com/LackOfMorals/graphlite.git
cd graphlite

# If vendor/ is absent, populate it and restore the neo4j vendor shim
go mod vendor
tail -n +3 scripts/graphlite_bridge.go > vendor/github.com/neo4j/neo4j-go-driver/v6/neo4j/graphlite_bridge.go

# Verify the build
CGO_ENABLED=0 go build ./...
```

---

## Running the test suites

### Unit and integration tests (main suite)

```bash
CGO_ENABLED=0 go test -count=1 ./...
```

This runs all unit tests (parser, planner, translator, store) and the integration tests under each package. The `testdata/` package must be run explicitly:

```bash
CGO_ENABLED=0 go test github.com/LackOfMorals/graphlite/testdata
```

### Property-based tests (rapid)

Property-based tests use `pgregory.net/rapid` and are part of the main package:

```bash
CGO_ENABLED=0 go test -run TestRapid ./...
```

The rapid generators create random graphs and verify full round-trip fidelity through CREATE, MATCH, and JSON import/export cycles.

### TCK harness (openCypher Technology Compatibility Kit)

The TCK harness is opt-in to avoid slowing the main test suite. It uses Godog (Cucumber for Go) and runs inline Gherkin scenarios:

```bash
CGO_ENABLED=0 go test -tags=tck ./compat/... -v
```

Scenarios tagged `@skip` are excluded from execution; a pass-rate banner is printed at the end. All skipped scenarios have an inline `# unsupported: <reason>` comment.

### Benchmarks

```bash
# Run all benchmarks (10 s each)
CGO_ENABLED=0 go test -run=^$ -bench=. -benchtime=10s ./bench/... | tee bench/results/latest.txt

# Run a single benchmark (1 s, fast iteration)
CGO_ENABLED=0 go test -run=^$ -bench=BenchmarkMatchNodeByID -benchtime=1s ./bench/...

# Enable the 1M-node benchmark (disabled by default — ~30 s setup, ~500 MB RAM)
CGO_ENABLED=0 go test -run=^$ -bench=BenchmarkSingleHopTraversal_1M -bench-1m -benchtime=10s ./bench/...
```

### Cross-platform build check

Before opening a PR, verify the CGO-free build passes on all four target platforms:

```bash
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build ./...
GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

---

## How to add a new Cypher feature

Adding a new Cypher clause or expression follows a five-step pipeline:

### Step 1 — Extend the AST (`cypher/ast.go`)

Add a new clause or expression struct under the appropriate section. Every new exported type needs a doc comment and must implement the `Clause` interface (for clauses) or `Expr` interface (for expressions) by adding the sealed `clauseNode()` / `exprNode()` method.

### Step 2 — Extend the parser (`cypher/parser.go`)

Wire the new AST node into `buildUpdatingClause` (for DML clauses) or `buildExprFromCST` (for expressions). The parser walks the ANTLR CST produced by `cloudprivacylabs/opencypher`. Use `opencypher.GetParser()` to access ANTLR `parser.*Context` types; the higher-level evaluator types have unexported fields and cannot be used externally.

Add unit tests in `cypher/parser_test.go` covering at least five representative inputs, including edge cases.

### Step 3 — Add a plan node (`cypher/plan.go`)

Define a new `*Plan` struct implementing `LogicalPlan` via the `planNode()` sealed method. Include doc comments on every exported field. If the feature requires a new expression type, add it as an `*Expr` struct implementing `Expr`.

### Step 4 — Wire the planner (`cypher/planner.go`)

Add a `planXxxClause` function and wire it into the `planQuery` switch statement. Populate the `BindingScope` for any new variables introduced by the clause. Add unit tests in `cypher/planner_test.go` asserting the exact `LogicalPlan` tree shape for each pattern.

### Step 5 — Emit SQL (`sql/translator.go`)

Add a case to `translateWritePlan` (for mutations) or `translatePlan` / `exprToSQL` (for read clauses and expressions). Add unit tests in `sql/translator_test.go` that compare the emitted SQL string against expected fixtures. Add end-to-end integration tests in `testdata/integration_test.go`.

---

## Benchmark baseline process

When a change may affect query performance, capture a new baseline:

1. Run the full benchmark suite on the target hardware before your change:
   ```bash
   CGO_ENABLED=0 go test -run=^$ -bench=. -benchtime=10s ./bench/... | tee bench/results/before.txt
   ```
2. Apply your change.
3. Run the benchmarks again:
   ```bash
   CGO_ENABLED=0 go test -run=^$ -bench=. -benchtime=10s ./bench/... | tee bench/results/after.txt
   ```
4. Compare the results using `benchstat` (installable via `go install golang.org/x/perf/cmd/benchstat@latest`):
   ```bash
   benchstat bench/results/before.txt bench/results/after.txt
   ```
5. Include the `benchstat` output in your PR description if there is a measurable change.
6. On release tags, CI updates `bench/results/latest.txt` automatically.

---

## API stability commitment

**No breaking changes are made to the public API after v0.3 without a major version bump.**

"Breaking change" means any change that would cause a program that compiled and ran correctly against the previous release to fail to compile or produce different behaviour when run against the new release. This includes:

- Removing or renaming exported types, functions, methods, or constants
- Changing function signatures (parameter types, return types, parameter count)
- Changing the semantics of existing functions in incompatible ways
- Changing error types in ways that break `errors.As` / `errors.Is` callers

Additions (new exported symbols) and bug fixes that correct documented-incorrect behaviour are not considered breaking changes.

This commitment covers the root package (`github.com/LackOfMorals/graphlite`) and its sub-packages. Internal packages (sub-packages not documented for external use) are exempt.

For the compatibility table of supported Cypher features, see [README.md — Cypher Compatibility](README.md#cypher-compatibility).

---

## Pull request guidelines

- Open an issue before starting significant work so we can agree on the approach.
- Keep PRs focused: one feature or bug fix per PR.
- All new code must have unit tests. Aim for at least 80% coverage on new packages.
- `go vet ./...` must pass with no warnings.
- `CGO_ENABLED=0 go build ./...` must pass for all four target platforms.
- `CGO_ENABLED=0 go test -count=1 ./...` must pass with no failures.
- Doc comments are required on all exported symbols (types, functions, methods, constants).
- Follow the existing code style (no external linter configuration is required).
- Commit messages should be of the form `graphlite-task-NNN: short description`.
