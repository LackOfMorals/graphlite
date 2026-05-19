package graphlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// QueryResult — lazy streaming result cursor
// ─────────────────────────────────────────────────────────────────────────────

// QueryResult is a lazy streaming cursor over a set of query result records.
// Call Next to advance the cursor, Record to read the current row, and
// Err to check for iteration errors. Always call Consume or allow the
// iteration to exhaust the result to release underlying resources.
type QueryResult struct {
	rows     *sql.Rows
	keys     []string
	record   *Record
	err      error
	consumed bool
	counters queryCounters
}

// NewQueryResultFromRows constructs a QueryResult, deriving column names from
// the *sql.Rows itself. Returns an error if column names cannot be read.
func NewQueryResultFromRows(rows *sql.Rows) (*QueryResult, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("graphlite: read column names: %w", err)
	}
	return &QueryResult{rows: rows, keys: cols}, nil
}

// Keys returns the projection key names for this result set.
func (r *QueryResult) Keys() []string {
	out := make([]string, len(r.keys))
	copy(out, r.keys)
	return out
}

// Next advances the cursor to the next record. Returns true if a record is
// available; false when the result set is exhausted or an error occurred.
func (r *QueryResult) Next(_ context.Context) bool {
	if r.consumed || r.err != nil {
		return false
	}
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			r.err = err
		}
		r.consumed = true
		return false
	}
	// Scan raw column values.
	rawVals := make([]any, len(r.keys))
	ptrs := make([]any, len(r.keys))
	for i := range rawVals {
		ptrs[i] = &rawVals[i]
	}
	if err := r.rows.Scan(ptrs...); err != nil {
		r.err = fmt.Errorf("graphlite: scan row: %w", err)
		r.consumed = true
		return false
	}
	// Map each raw value to its graph type.
	vals := make([]any, len(r.keys))
	for i, v := range rawVals {
		vals[i] = mapColumnValue(v)
	}
	r.record = NewRecord(r.keys, vals)
	return true
}

// Record returns the current record. Returns nil if Next has not been called
// or if the cursor is exhausted.
func (r *QueryResult) Record() *Record {
	return r.record
}

// Err returns the first error encountered during iteration.
func (r *QueryResult) Err() error {
	return r.err
}

// Consume drains any remaining records, closes the underlying *sql.Rows, and
// returns the ResultSummary. After Consume returns the cursor is closed.
// Consume is safe to call on a write result (where rows is nil).
func (r *QueryResult) Consume(_ context.Context) (ResultSummary, error) {
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
// Collect is safe to call on a write result (where rows is nil).
func (r *QueryResult) Collect(ctx context.Context) ([]*Record, error) {
	var recs []*Record
	for r.Next(ctx) {
		rec := r.Record()
		recs = append(recs, rec)
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.rows != nil {
		if err := r.rows.Close(); err != nil {
			return nil, err
		}
	}
	r.consumed = true
	return recs, nil
}

// QueryCounters is the exported form of write-operation statistics, used by
// callers that need to set counter values on a QueryResult (e.g. the execution
// layer in driver.go after running CREATE / SET / DELETE statements).
type QueryCounters struct {
	NodesCreated         int
	NodesDeleted         int
	RelationshipsCreated int
	RelationshipsDeleted int
	PropertiesSet        int
}

// SetCounters attaches write-operation counters to this result. It is called
// by the execution layer after executing write statements.
func (r *QueryResult) SetCounters(c QueryCounters) {
	r.counters = queryCounters{
		nodesCreated:         c.NodesCreated,
		nodesDeleted:         c.NodesDeleted,
		relationshipsCreated: c.RelationshipsCreated,
		relationshipsDeleted: c.RelationshipsDeleted,
		propertiesSet:        c.PropertiesSet,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// EagerResult — pre-collected result
// ─────────────────────────────────────────────────────────────────────────────

// EagerResult is a pre-collected result containing all records and a summary.
// It is returned by operations that exhaust the result set immediately (e.g.
// the DriverCompat ExecuteQuery tier using EagerResultTransformer).
type EagerResult struct {
	// Keys is the ordered list of projection key names.
	Keys []string
	// Records contains all records returned by the query.
	Records []*Record
	// Summary contains execution statistics.
	Summary ResultSummary
}

// NewEagerResult drains a QueryResult and returns an EagerResult.
func NewEagerResult(ctx context.Context, qr *QueryResult) (*EagerResult, error) {
	return newEagerResult(ctx, qr)
}

// newEagerResult drains a QueryResult and returns an EagerResult.
func newEagerResult(ctx context.Context, qr *QueryResult) (*EagerResult, error) {
	recs, err := qr.Collect(ctx)
	if err != nil {
		return nil, err
	}
	sum, err := qr.Consume(ctx)
	if err != nil {
		return nil, err
	}
	return &EagerResult{
		Keys:    qr.Keys(),
		Records: recs,
		Summary: sum,
	}, nil
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

// MapColumnValue is the exported form of mapColumnValue, exposed for testing.
// Production callers should use mapColumnValue directly.
func MapColumnValue(v any) any { return mapColumnValue(v) }

// SplitLabels is the exported form of splitLabels, exposed for testing.
func SplitLabels(s string) []string { return splitLabels(s) }

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
			if node := tryParseNode(val); node != nil {
				return node
			}
			if rel := tryParseRelationship(val); rel != nil {
				return rel
			}
		}
		return val
	case []byte:
		// SQLite may return JSON columns as []byte.
		s := string(val)
		if len(s) > 0 && s[0] == '{' {
			if node := tryParseNode(s); node != nil {
				return node
			}
			if rel := tryParseRelationship(s); rel != nil {
				return rel
			}
		}
		return s
	default:
		return v
	}
}

// tryParseNode attempts to decode a JSON string as a node object.
// Returns nil if the JSON does not match the node shape.
func tryParseNode(s string) *Node {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil
	}
	// Must have 'id', 'labels', and 'props'; must NOT have 'type' (that would be a rel).
	if _, hasID := raw["id"]; !hasID {
		return nil
	}
	labelsRaw, hasLabels := raw["labels"]
	propsRaw, hasProps := raw["props"]
	if !hasLabels || !hasProps {
		return nil
	}
	if _, hasType := raw["type"]; hasType {
		return nil // relationship shape
	}

	// Decode id.
	var idVal any
	if err := json.Unmarshal(raw["id"], &idVal); err != nil {
		return nil
	}
	elemID := jsonNumberToElementID(idVal)

	// Decode labels (comma-separated string).
	var labelsStr string
	if err := json.Unmarshal(labelsRaw, &labelsStr); err != nil {
		return nil
	}
	labels := splitLabels(labelsStr)

	// Decode props.
	props, err := decodeProps(propsRaw)
	if err != nil {
		return nil
	}

	return &Node{
		ElementId: elemID,
		Labels:    labels,
		Props:     props,
	}
}

// tryParseRelationship attempts to decode a JSON string as a relationship object.
// Returns nil if the JSON does not match the relationship shape.
func tryParseRelationship(s string) *Relationship {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil
	}
	// Must have 'id', 'type', 'start_id', 'end_id', 'props'.
	for _, key := range []string{"id", "type", "start_id", "end_id", "props"} {
		if _, ok := raw[key]; !ok {
			return nil
		}
	}

	var idVal any
	if err := json.Unmarshal(raw["id"], &idVal); err != nil {
		return nil
	}
	var startIDVal any
	if err := json.Unmarshal(raw["start_id"], &startIDVal); err != nil {
		return nil
	}
	var endIDVal any
	if err := json.Unmarshal(raw["end_id"], &endIDVal); err != nil {
		return nil
	}
	var relType string
	if err := json.Unmarshal(raw["type"], &relType); err != nil {
		return nil
	}
	props, err := decodeProps(raw["props"])
	if err != nil {
		return nil
	}

	return &Relationship{
		ElementId:      jsonNumberToElementID(idVal),
		Type:           relType,
		StartElementId: jsonNumberToElementID(startIDVal),
		EndElementId:   jsonNumberToElementID(endIDVal),
		Props:          props,
	}
}

// jsonNumberToElementID converts a JSON-decoded numeric id (float64 or
// json.Number) to a stable string ElementId.
func jsonNumberToElementID(v any) string {
	switch n := v.(type) {
	case float64:
		return fmt.Sprintf("%d", int64(n))
	case json.Number:
		return n.String()
	case int64:
		return fmt.Sprintf("%d", n)
	case string:
		return n
	default:
		return fmt.Sprintf("%v", v)
	}
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
