package graphlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Result — lazy streaming result cursor
// ─────────────────────────────────────────────────────────────────────────────

// Result is a lazy streaming cursor over a set of query result records.
// Call Next to advance the cursor, Record to read the current row, and
// Err to check for iteration errors. Always call Consume or allow the
// iteration to exhaust the result to release underlying resources.
type Result struct {
	rows     *sql.Rows
	keys     []string
	record   *Record
	err      error
	consumed bool
	counters queryCounters

	// Pre-allocated scan buffers reused across Next calls to reduce per-row
	// heap allocations. rawVals holds raw column values; ptrs holds pointers
	// into rawVals for rows.Scan; vals holds the mapped graph-type values
	// before they are copied into the Record. All three are sized to
	// len(keys) at construction time in newResultFromRows.
	rawVals []any
	ptrs    []any
	vals    []any

	// inMemory holds pre-collected records for in-memory results (no sql.Rows).
	// When non-nil, Next/Record/Consume iterate over this slice instead of rows.
	inMemory    []*Record
	inMemoryPos int
}

// newResultFromRows constructs a Result, deriving column names from
// the *sql.Rows itself. Returns an error if column names cannot be read.
// The scan buffers (rawVals, ptrs, vals) are pre-allocated here and reused
// across all Next calls to avoid per-row heap allocations.
func newResultFromRows(rows *sql.Rows) (*Result, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("graphlite: read column names: %w", err)
	}
	n := len(cols)
	rawVals := make([]any, n)
	ptrs := make([]any, n)
	for i := range rawVals {
		ptrs[i] = &rawVals[i]
	}
	return &Result{
		rows:    rows,
		keys:    cols,
		rawVals: rawVals,
		ptrs:    ptrs,
		vals:    make([]any, n),
	}, nil
}

// newInMemoryResult constructs a Result backed by a pre-collected
// slice of records. Used by the write-then-select execution path when multiple
// result rows must be assembled from several SELECT calls.
func newInMemoryResult(keys []string, records []*Record) *Result {
	if records == nil {
		records = []*Record{}
	}
	return &Result{
		keys:     keys,
		inMemory: records,
	}
}

// Keys returns the projection key names for this result set.
func (r *Result) Keys() []string {
	out := make([]string, len(r.keys))
	copy(out, r.keys)
	return out
}

// Next advances the cursor to the next record. Returns true if a record is
// available; false when the result set is exhausted or an error occurred.
// If the context is already cancelled or has timed out, Next immediately
// returns false and sets Err to ctx.Err().
func (r *Result) Next(ctx context.Context) bool {
	if err := ctx.Err(); err != nil {
		r.err = err
		r.consumed = true
		return false
	}
	if r.consumed || r.err != nil {
		return false
	}
	// In-memory mode: iterate over pre-collected records.
	if r.inMemory != nil {
		if r.inMemoryPos >= len(r.inMemory) {
			r.consumed = true
			return false
		}
		r.record = r.inMemory[r.inMemoryPos]
		r.inMemoryPos++
		return true
	}
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			r.err = err
		}
		r.consumed = true
		return false
	}
	// Scan raw column values into the pre-allocated buffers. rawVals and ptrs
	// are reused across calls (ptrs[i] == &rawVals[i] established at construction).
	if err := r.rows.Scan(r.ptrs...); err != nil {
		r.err = fmt.Errorf("graphlite: scan row: %w", err)
		r.consumed = true
		return false
	}
	// Map each raw value to its graph type, reusing the pre-allocated vals slice.
	// newRecord copies vals internally so it is safe to reuse vals on the next call.
	for i, v := range r.rawVals {
		r.vals[i] = mapColumnValue(v)
	}
	r.record = newRecord(r.keys, r.vals)
	return true
}

// Record returns the current record. Returns nil if Next has not been called
// or if the cursor is exhausted.
func (r *Result) Record() *Record {
	return r.record
}

// Err returns the first error encountered during iteration.
func (r *Result) Err() error {
	return r.err
}

// Consume drains any remaining records, closes the underlying *sql.Rows, and
// returns the ResultSummary. After Consume returns the cursor is closed.
// Consume is safe to call on a write result (where rows is nil) and on
// in-memory results.
func (r *Result) Consume(_ context.Context) (ResultSummary, error) {
	if r.inMemory != nil {
		r.consumed = true
		return &resultSummary{counters: r.counters}, r.err
	}
	if !r.consumed && r.rows != nil {
		// Drain remaining rows so we can release the cursor cleanly.
		for r.rows.Next() {
		}
		if err := r.rows.Err(); err != nil && r.err == nil {
			r.err = err
		}
		r.consumed = true
	}
	if r.rows != nil {
		if err := r.rows.Close(); err != nil && r.err == nil {
			r.err = err
		}
	}
	return &resultSummary{counters: r.counters}, r.err
}

// Collect drains all remaining records into a slice and closes the cursor.
// Collect is safe to call on a write result (where rows is nil) and on
// in-memory results.
func (r *Result) Collect(ctx context.Context) ([]*Record, error) {
	// Fast path for in-memory results: return remaining records directly.
	if r.inMemory != nil {
		recs := r.inMemory[r.inMemoryPos:]
		r.inMemoryPos = len(r.inMemory)
		r.consumed = true
		if r.err != nil {
			return nil, r.err
		}
		return recs, nil
	}
	var recs []*Record
	for r.Next(ctx) {
		rec := r.Record()
		recs = append(recs, rec)
	}
	if r.rows != nil {
		if err := r.rows.Close(); err != nil && r.err == nil {
			r.err = err
		}
		r.rows = nil
	}
	r.consumed = true
	if r.err != nil {
		return nil, r.err
	}
	return recs, nil
}

// Single returns the one and only record from the result set. It is a
// convenience method for queries expected to return exactly one row.
//
//   - If the result set is empty, Single returns (nil, ErrNoRecords).
//   - If the result set has exactly one record, Single returns that record and nil.
//   - If the result set has two or more records, Single drains the cursor and
//     returns (nil, ErrMultipleRecords).
//
// Single always closes the cursor before returning.
func (r *Result) Single(ctx context.Context) (*Record, error) {
	if !r.Next(ctx) {
		// Drain and close.
		_, _ = r.Consume(ctx)
		if r.err != nil {
			return nil, r.err
		}
		return nil, ErrNoRecords
	}
	rec := r.Record()

	// Check whether a second record exists.
	if r.Next(ctx) {
		// Drain remaining records before returning. Any drain/close error is
		// secondary to ErrMultipleRecords, which is the primary signal here.
		_, _ = r.Consume(ctx)
		return nil, ErrMultipleRecords
	}

	// Exactly one record — close the cursor cleanly.
	_, _ = r.Consume(ctx)
	if r.err != nil {
		return nil, r.err
	}
	return rec, nil
}

// setCounters attaches write-operation counters to this result. It is called
// by the execution layer after executing write statements.
func (r *Result) setCounters(c queryCounters) {
	r.counters = c
}

// ─────────────────────────────────────────────────────────────────────────────
// ResultSummary and Counters
// ─────────────────────────────────────────────────────────────────────────────

// queryCounters accumulates write-operation statistics for a single query.
type queryCounters struct {
	nodesCreated         int
	nodesDeleted         int
	relationshipsCreated int
	relationshipsDeleted int
	propertiesSet        int
}

// ResultSummary reports execution statistics and metadata for a completed query.
type ResultSummary interface {
	// Counters returns statistics about graph mutations performed by the query.
	Counters() Counters
}

// Counters reports the number of graph mutations performed by a query.
type Counters interface {
	// NodesCreated returns the number of nodes created.
	NodesCreated() int
	// NodesDeleted returns the number of nodes deleted.
	NodesDeleted() int
	// RelationshipsCreated returns the number of relationships created.
	RelationshipsCreated() int
	// RelationshipsDeleted returns the number of relationships deleted.
	RelationshipsDeleted() int
	// PropertiesSet returns the number of property values written.
	PropertiesSet() int
	// ContainsUpdates returns true when any mutation counter is greater than zero.
	ContainsUpdates() bool
}

// resultSummary is the concrete implementation of ResultSummary.
type resultSummary struct {
	counters queryCounters
}

// Counters implements ResultSummary.
func (s *resultSummary) Counters() Counters {
	return &counters{c: s.counters}
}

// counters is the concrete implementation of Counters.
type counters struct {
	c queryCounters
}

func (c *counters) NodesCreated() int         { return c.c.nodesCreated }
func (c *counters) NodesDeleted() int         { return c.c.nodesDeleted }
func (c *counters) RelationshipsCreated() int { return c.c.relationshipsCreated }
func (c *counters) RelationshipsDeleted() int { return c.c.relationshipsDeleted }
func (c *counters) PropertiesSet() int        { return c.c.propertiesSet }
func (c *counters) ContainsUpdates() bool {
	return c.c.nodesCreated > 0 || c.c.nodesDeleted > 0 ||
		c.c.relationshipsCreated > 0 || c.c.relationshipsDeleted > 0 ||
		c.c.propertiesSet > 0
}

// ─────────────────────────────────────────────────────────────────────────────
// Column value mapper
// ─────────────────────────────────────────────────────────────────────────────

// graphElementJSON is the common shape for both node and relationship JSON
// objects emitted by the SQL translator's VarExpr projections.
type graphElementJSON struct {
	ID      json.Number     `json:"id"`
	Labels  string          `json:"labels"`
	Type    string          `json:"type"`
	StartID json.Number     `json:"start_id"`
	EndID   json.Number     `json:"end_id"`
	Props   json.RawMessage `json:"props"`
}

// mapColumnValue converts a raw SQLite column value to a graph type.
//
// The translator emits whole-node VarExpr projections as:
//
//	json_object('id', n0.id, 'labels', n0.labels, 'props', json(n0.props))
//
// and whole-relationship VarExpr projections as:
//
//	json_object('id', r0.id, 'type', r0.type, 'start_id', r0.start_id, 'end_id', r0.end_id, 'props', json(r0.props))
//
// The resulting column value is a JSON string. This function detects both
// shapes and returns a *Node or *Relationship respectively. All other values
// (scalars, property projections) are returned unchanged.
func mapColumnValue(v any) any {
	switch val := v.(type) {
	case string:
		// JSON object columns from VarExpr projections start with '{'.
		if len(val) > 0 && val[0] == '{' {
			if elem := tryParseGraphElement(val); elem != nil {
				return elem
			}
		}
		return val
	case []byte:
		// SQLite may return JSON columns as []byte.
		s := string(val)
		if len(s) > 0 && s[0] == '{' {
			if elem := tryParseGraphElement(s); elem != nil {
				return elem
			}
		}
		return s
	default:
		return v
	}
}

// tryParseGraphElement attempts to decode a JSON string as either a node or
// relationship object using a single unmarshal call. Returns nil if the JSON
// does not match either shape.
func tryParseGraphElement(s string) any {
	var elem graphElementJSON
	if err := json.Unmarshal([]byte(s), &elem); err != nil {
		return nil
	}
	if elem.ID == "" || len(elem.Props) == 0 {
		return nil
	}
	props, err := decodeProps(elem.Props)
	if err != nil {
		return nil
	}
	if elem.Type != "" && elem.StartID != "" && elem.EndID != "" {
		return &Relationship{
			ElementId:      elem.ID.String(),
			Type:           elem.Type,
			StartElementId: elem.StartID.String(),
			EndElementId:   elem.EndID.String(),
			Props:          props,
		}
	}
	if elem.Type == "" {
		return &Node{
			ElementId: elem.ID.String(),
			Labels:    splitLabels(elem.Labels),
			Props:     props,
		}
	}
	return nil
}

// splitLabels splits a comma-separated labels string into a slice. An empty
// string returns a nil slice (no labels).
func splitLabels(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// decodeProps decodes a JSON props object (raw JSON bytes) into map[string]any.
// An empty JSON object "{}" returns an empty (non-nil) map.
func decodeProps(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("graphlite: decode props: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}
