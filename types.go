// Package graphlite provides an embedded, file-based property graph database
// queryable via a subset of openCypher. It is designed as a zero-infrastructure
// local substitute for Neo4j Aura in testing and development workflows.
package graphlite

import "fmt"

// Node represents a graph node with a unique element ID, a set of labels, and
// a map of properties. All fields map directly to the Neo4j driver's Node type
// so that DriverCompat results can be used interchangeably.
type Node struct {
	// ElementId is the stable string identifier for this node, derived from the
	// underlying SQLite integer primary key.
	ElementId string

	// Labels is the ordered list of label names assigned to this node.
	Labels []string

	// Props holds the node's property values, keyed by property name.
	Props map[string]any
}

// Relationship represents a directed graph edge between two nodes. All fields
// map directly to the Neo4j driver's Relationship type.
type Relationship struct {
	// ElementId is the stable string identifier for this relationship.
	ElementId string

	// Type is the relationship type name (e.g. "KNOWS", "WORKS_FOR").
	Type string

	// StartElementId is the ElementId of the node at the start of the relationship.
	StartElementId string

	// EndElementId is the ElementId of the node at the end of the relationship.
	EndElementId string

	// Props holds the relationship's property values, keyed by property name.
	Props map[string]any
}

// Record is an ordered collection of key-value pairs returned by a query. Keys
// are the projection aliases from the RETURN clause; values may be scalars,
// Node, Relationship, or nil.
type Record struct {
	keys   []string
	values []any
}

// NewRecord constructs a Record from parallel slices of keys and values.
// The two slices must have the same length; if they do not, NewRecord panics.
func NewRecord(keys []string, values []any) *Record {
	if len(keys) != len(values) {
		panic(fmt.Sprintf("graphlite: NewRecord: keys length %d != values length %d", len(keys), len(values)))
	}
	k := make([]string, len(keys))
	v := make([]any, len(values))
	copy(k, keys)
	copy(v, values)
	return &Record{keys: k, values: v}
}

// Get returns the value associated with key and true, or nil and false if key
// is not present in the record.
func (r *Record) Get(key string) (any, bool) {
	for i, k := range r.keys {
		if k == key {
			return r.values[i], true
		}
	}
	return nil, false
}

// AsMap returns all key-value pairs as a new map. Modifications to the returned
// map do not affect the record.
func (r *Record) AsMap() map[string]any {
	m := make(map[string]any, len(r.keys))
	for i, k := range r.keys {
		m[k] = r.values[i]
	}
	return m
}

// Keys returns the ordered list of projection keys for this record.
func (r *Record) Keys() []string {
	out := make([]string, len(r.keys))
	copy(out, r.keys)
	return out
}

// Values returns the ordered list of values for this record.
func (r *Record) Values() []any {
	out := make([]any, len(r.values))
	copy(out, r.values)
	return out
}

// ErrUnsupportedCypher is returned when the planner or translator encounters a
// Cypher construct that graphlite does not support. Use errors.As to inspect the
// Clause and Position fields.
type ErrUnsupportedCypher struct {
	// Clause is the Cypher clause or construct that is not supported
	// (e.g. "CALL", "UNION", "variable-length path").
	Clause string

	// Position is the approximate character offset in the input query where the
	// unsupported construct was detected. Zero means the position is unknown.
	Position int

	// Detail is an optional human-readable description of why the construct is
	// unsupported.
	Detail string
}

// Error implements the error interface.
func (e *ErrUnsupportedCypher) Error() string {
	if e.Position > 0 {
		return fmt.Sprintf("graphlite: unsupported Cypher construct %q at position %d: %s", e.Clause, e.Position, e.Detail)
	}
	if e.Detail != "" {
		return fmt.Sprintf("graphlite: unsupported Cypher construct %q: %s", e.Clause, e.Detail)
	}
	return fmt.Sprintf("graphlite: unsupported Cypher construct %q", e.Clause)
}

// ErrMissingParameter is returned when a parameterised query references a
// $paramName that is not present in the caller-supplied parameter map.
type ErrMissingParameter struct {
	// Name is the parameter name that was expected but not provided.
	Name string
}

// Error implements the error interface.
func (e *ErrMissingParameter) Error() string {
	return fmt.Sprintf("graphlite: missing query parameter $%s", e.Name)
}

// ErrImportDepthExceeded is returned by Import when the input JSON nesting
// depth exceeds the configured maximum (default: 20 levels).
type ErrImportDepthExceeded struct {
	// MaxDepth is the depth limit that was exceeded.
	MaxDepth int
}

// Error implements the error interface.
func (e *ErrImportDepthExceeded) Error() string {
	return fmt.Sprintf("graphlite: import JSON nesting depth exceeds maximum of %d", e.MaxDepth)
}

// ErrImportTooLarge is returned by Import when the input stream exceeds the
// configured maximum size (default: 500 MiB).
type ErrImportTooLarge struct {
	// MaxBytes is the size limit that was exceeded.
	MaxBytes int64
}

// Error implements the error interface.
func (e *ErrImportTooLarge) Error() string {
	return fmt.Sprintf("graphlite: import data exceeds maximum size of %d bytes", e.MaxBytes)
}

// ErrReadOnly is returned when a write query is executed against a database
// opened with WithReadOnly().
var ErrReadOnly = fmt.Errorf("graphlite: database is read-only")

// ErrNoRecords is returned by Result.Single when the result set contains no
// records. It is a sentinel value and can be checked with errors.Is.
var ErrNoRecords = fmt.Errorf("graphlite: result contains no records")

// ErrMultipleRecords is returned by Result.Single when the result set contains
// more than one record. It is a sentinel value and can be checked with errors.Is.
var ErrMultipleRecords = fmt.Errorf("graphlite: result contains multiple records")
