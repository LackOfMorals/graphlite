// Package sql translates logical plan trees into parameterised SQL strings
// for execution against the storage layer.
//
// The Dialect interface is the extension point for supporting multiple SQL
// backends (SQLite, DuckDB, PostgreSQL). Each method emits one SQL fragment;
// the translator assembles these fragments into complete statements.
package sql

import (
	"fmt"
	"strings"
)

// Dialect defines the SQL dialect-specific fragments required by the translator.
// Implementations must be stateless and safe for concurrent use.
//
// The interface covers four categories of variation:
//   - Identifier quoting (QuoteIdentifier)
//   - JSON property extraction (JSONExtract, JSONSet, JSONRemove)
//   - Label membership testing via the node_labels junction table (LabelContains)
//   - Positional parameter placeholders (Placeholder)
type Dialect interface {
	// QuoteIdentifier returns the identifier quoted for safe use in SQL.
	// For SQLite this is double-quoting: QuoteIdentifier("my table") → `"my table"`.
	QuoteIdentifier(name string) string

	// JSONExtract returns a SQL expression that extracts the value at jsonPath
	// from the column named by colExpr.
	//
	//	SQLite: json_extract(<colExpr>, '<jsonPath>')
	//
	// jsonPath must be a valid JSON path starting with '$', e.g. "$.name".
	JSONExtract(colExpr, jsonPath string) string

	// JSONSet returns a SQL expression that sets the value at jsonPath in the
	// column named colExpr to valueExpr (a SQL expression or placeholder).
	//
	//	SQLite: json_set(<colExpr>, '<jsonPath>', <valueExpr>)
	JSONSet(colExpr, jsonPath, valueExpr string) string

	// JSONRemove returns a SQL expression that removes the key at jsonPath from
	// the column named colExpr.
	//
	//	SQLite: json_remove(<colExpr>, '<jsonPath>')
	JSONRemove(colExpr, jsonPath string) string

	// LabelContains returns a SQL predicate that is true when the node identified
	// by nodeIDExpr has labelName recorded in the node_labels junction table.
	//
	// nodeIDExpr is a SQL expression evaluating to the node's integer id,
	// e.g. "n0.id". labelName is the unquoted label string.
	//
	// SQLite emits an EXISTS subquery that uses the idx_node_labels_label index:
	//
	//	EXISTS (SELECT 1 FROM node_labels WHERE node_id = <nodeIDExpr> AND label = ?)
	//
	// args contains exactly one value (labelName) bound to the "?" placeholder.
	LabelContains(nodeIDExpr, labelName string) (predicate string, args []any)

	// Placeholder returns the SQL positional placeholder for the nth argument
	// (0-indexed). SQLite always uses "?"; PostgreSQL would use "$1", "$2", etc.
	Placeholder(n int) string
}

// ─────────────────────────────────────────────────────────────────────────────
// SQLiteDialect
// ─────────────────────────────────────────────────────────────────────────────

// SQLiteDialect implements Dialect for modernc.org/sqlite (and any SQLite3
// database). It is the only concrete dialect at v1.0.
//
// SQLiteDialect is zero-value constructible and stateless; use it as a value
// or as a pointer — both satisfy the Dialect interface.
type SQLiteDialect struct{}

// Verify compile-time conformance.
var _ Dialect = SQLiteDialect{}

// QuoteIdentifier double-quotes the identifier, escaping any embedded double
// quotes by doubling them (SQL standard).
//
//	QuoteIdentifier("name")    → `"name"`
//	QuoteIdentifier(`a"b`)     → `"a""b"`
func (SQLiteDialect) QuoteIdentifier(name string) string {
	// Escape embedded double-quotes by doubling them, then wrap the result.
	var b strings.Builder
	b.Grow(len(name) + 2)
	b.WriteByte('"')
	for _, ch := range name {
		if ch == '"' {
			b.WriteString(`""`)
		} else {
			b.WriteRune(ch)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// JSONExtract emits `json_extract(<colExpr>, '<jsonPath>')`.
//
//	JSONExtract("n0.props", "$.name") → `json_extract(n0.props, '$.name')`
func (SQLiteDialect) JSONExtract(colExpr, jsonPath string) string {
	return fmt.Sprintf("json_extract(%s, '%s')", colExpr, escapeJSONPath(jsonPath))
}

// JSONSet emits `json_set(<colExpr>, '<jsonPath>', <valueExpr>)`.
//
//	JSONSet("n0.props", "$.age", "?") → `json_set(n0.props, '$.age', ?)`
func (SQLiteDialect) JSONSet(colExpr, jsonPath, valueExpr string) string {
	return fmt.Sprintf("json_set(%s, '%s', %s)", colExpr, escapeJSONPath(jsonPath), valueExpr)
}

// JSONRemove emits `json_remove(<colExpr>, '<jsonPath>')`.
//
//	JSONRemove("n0.props", "$.age") → `json_remove(n0.props, '$.age')`
func (SQLiteDialect) JSONRemove(colExpr, jsonPath string) string {
	return fmt.Sprintf("json_remove(%s, '%s')", colExpr, escapeJSONPath(jsonPath))
}

// escapeJSONPath escapes single quotes in a JSON path string by doubling them,
// preventing SQL injection via backtick-quoted property names that contain
// single-quote characters (e.g. `it'sKey` → `$.it''sKey`).
func escapeJSONPath(p string) string {
	return strings.ReplaceAll(p, "'", "''")
}

// LabelContains returns a predicate that tests whether the node identified by
// nodeIDExpr has labelName in the node_labels junction table. It uses an EXISTS
// subquery that is resolved via the idx_node_labels_label(label, node_id) index,
// avoiding a full scan of the nodes table.
//
// nodeIDExpr must be a SQL expression evaluating to the node's integer id,
// e.g. "n0.id". No LIKE wildcard escaping is needed because the predicate
// uses an equality check on the label column.
//
//	LabelContains("n0.id", "Person") →
//	  "EXISTS (SELECT 1 FROM node_labels WHERE node_id = n0.id AND label = ?)"
//	  ["Person"]
func (SQLiteDialect) LabelContains(nodeIDExpr, labelName string) (string, []any) {
	predicate := fmt.Sprintf(
		"EXISTS (SELECT 1 FROM node_labels WHERE node_id = %s AND label = ?)",
		nodeIDExpr,
	)
	return predicate, []any{labelName}
}

// Placeholder always returns "?" for SQLite positional parameter style.
// The index n is accepted for interface compatibility but is not used.
func (SQLiteDialect) Placeholder(_ int) string {
	return "?"
}
