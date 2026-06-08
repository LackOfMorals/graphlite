# PRD: Result Row Materialisation Performance

## Overview

Reduce per-row heap allocations in the result materialisation path (`result.go`, `types.go`) based on pprof profiling of `BenchmarkCollectLargeResult`. The benchmark currently shows 479K allocs/op and 21 MB/op for 10K rows. Three targeted, non-breaking changes eliminate the dominant allocation sites.

## Goals

- Eliminate the majority of per-row heap allocations in the hot path (`mapColumnValue` → `tryParseNode` / `tryParseRelationship` → `newRecord`).
- Measurably reduce `allocs/op` and `B/op` on `BenchmarkCollectLargeResult`, `BenchmarkRunQuerySimpleSelect`, and `BenchmarkMatchByLabel`.
- Zero API changes — all public types and signatures remain identical.

## Non-Goals

- Lazy or deferred property decoding.
- Changing the SQLite column projection format emitted by `sql/translator.go`.
- Caching decoded nodes/relationships across queries.
- Any change to the `cypher/`, `sql/`, or `store/` packages.

## Requirements

### Functional Requirements

- REQ-F-001: `tryParseNode` and `tryParseRelationship` must be replaced or refactored to use a single typed struct unmarshal (covering both node and relationship shapes in one `json.Unmarshal` call), eliminating all intermediate `map[string]json.RawMessage` allocations.
- REQ-F-002: `newRecord` must store the caller-supplied `keys` slice by reference rather than copying it. The public `Record.Keys()` method already copies for callers and must remain unchanged.
- REQ-F-003: `jsonNumberToElementID` must use `strconv.FormatInt` instead of `fmt.Sprintf` for the `float64` and `int64` branches.
- REQ-F-004: The combined node/relationship detection logic must correctly distinguish a node shape (`id`, `labels`, `props` present; `type` absent) from a relationship shape (`id`, `type`, `start_id`, `end_id`, `props` all present).

### Non-Functional Requirements

- REQ-NF-001: All existing unit tests (`CGO_ENABLED=0 go test -count=1 ./...`) must pass without modification.
- REQ-NF-002: The TCK harness (`CGO_ENABLED=0 go test -tags=tck ./compat/...`) must continue to report 100% pass rate.
- REQ-NF-003: `BenchmarkCollectLargeResult` allocs/op must decrease by at least 25% versus the baseline on the same machine.
- REQ-NF-004: No new exported symbols. No changes to `Node`, `Relationship`, or `Record` field layouts.

## Technical Considerations

**Typed struct unmarshal (REQ-F-001)**

Replace the two-stage `map[string]json.RawMessage` approach with a single shared struct:

```go
type graphElementJSON struct {
    ID      json.Number     `json:"id"`
    Labels  string          `json:"labels"`
    Type    string          `json:"type"`
    StartID json.Number     `json:"start_id"`
    EndID   json.Number     `json:"end_id"`
    Props   json.RawMessage `json:"props"`
}
```

One `json.Unmarshal([]byte(s), &elem)` populates all fields. Shape detection:
- Node: `elem.Labels != ""` (nodes always have a `labels` field, even if empty string) and `elem.Type == ""`.
- Relationship: `elem.Type != ""` and `elem.StartID != ""` and `elem.EndID != ""`.

`tryParseNode` and `tryParseRelationship` become thin wrappers or are inlined into `mapColumnValue` via a single `tryParseGraphElement` helper.

**Keys reference sharing (REQ-F-002)**

`newRecord` currently does `make([]string, len(keys)) + copy`. Since `Result.keys` is constant for the lifetime of a result cursor, `newRecord` can store `keys` directly:

```go
func newRecord(keys []string, values []any) *Record {
    v := make([]any, len(values))
    copy(v, values)
    return &Record{keys: keys, values: v}
}
```

`Record.Keys()` already returns `make + copy`, so callers are unaffected.

**`strconv.FormatInt` (REQ-F-003)**

`fmt.Sprintf("%d", int64(n))` uses reflection and heap-allocates the format args. Replace with `strconv.FormatInt(int64(n), 10)`.

## Acceptance Criteria

- [ ] `CGO_ENABLED=0 go test -count=1 ./...` passes with no failures.
- [ ] `CGO_ENABLED=0 go test -tags=tck ./compat/... -v` reports 235/235 (100%).
- [ ] `CGO_ENABLED=0 go test -run=^$ -bench=BenchmarkCollectLargeResult -benchmem -benchtime=5s ./bench/...` shows allocs/op reduced by ≥25% vs baseline (baseline: 479774 allocs/op).
- [ ] `BenchmarkRunQuerySimpleSelect` and `BenchmarkMatchByLabel` allocs/op also decrease.
- [ ] No new exported symbols added; no existing exported signatures changed.
- [ ] `go vet ./...` clean.

## Out of Scope

- Lazy props decoding.
- Object pooling / `sync.Pool` for `Record` or `Node`.
- Query plan caching (tracked separately as task-016).
- CSV/JSON import path allocations.

## Open Questions

None — profiling data is unambiguous and implementation approach is fully specified.
