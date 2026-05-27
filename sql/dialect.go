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
//   - Comma-separated label membership testing (LabelContains)
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

	// LabelContains returns a SQL predicate that is true when the comma-separated
	// text column colExpr contains labelName as a whole label (not a substring of
	// another label).
	//
	// The colExpr is a fully-qualified column reference such as "n0.labels".
	// labelName is an unquoted label string; implementations must escape LIKE
	// wildcard characters ('%', '_', '\') before using labelName in LIKE branches
	// so that label names containing those characters match only themselves.
	//
	//	SQLite emits four OR branches to cover all positions in the list:
	//	  exact match:    colExpr = ?           (unescaped labelName)
	//	  prefix:         colExpr LIKE ? || ',%' ESCAPE '\'
	//	  suffix:         colExpr LIKE '%,' || ? ESCAPE '\'
	//	  middle:         colExpr LIKE '%,' || ? || ',%' ESCAPE '\'
	//
	// args receives the label values in the order they must appear in the SQL
	// argument slice: one unescaped value for the exact-match branch, followed
	// by three escaped values for the LIKE branches.
	LabelContains(colExpr, labelName string) (predicate string, args []any)

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

// LabelContains returns a predicate that tests whether the comma-separated
// labels column contains labelName as a whole label entry. Four OR branches
// cover all positions in the list:
//
//  1. Exact match (the entire column is exactly labelName)
//  2. Prefix     (labelName is the first entry, followed by a comma)
//  3. Suffix     (labelName is the last entry, preceded by a comma)
//  4. Middle     (labelName is surrounded by commas)
//
// labelName is escaped before use in the three LIKE branches: backslashes are
// doubled, '%' is replaced with '\%', and '_' is replaced with '\_'. This
// prevents label names that contain SQL LIKE wildcard characters from matching
// unintended labels. Each LIKE branch carries an explicit ESCAPE '\' clause.
// The exact-match (=) branch receives the original unescaped value.
//
// The returned args slice contains four values: the unescaped name for the
// exact-match branch, then three copies of the escaped name for the LIKE
// branches.
//
//	LabelContains("n0.labels", "Person") →
//	  "( n0.labels = ? OR n0.labels LIKE ? || ',%' ESCAPE '\' OR n0.labels LIKE '%,' || ? ESCAPE '\' OR n0.labels LIKE '%,' || ? || ',%' ESCAPE '\' )"
//	  ["Person", "Person", "Person", "Person"]
func (SQLiteDialect) LabelContains(colExpr, labelName string) (string, []any) {
	// Escape LIKE wildcard characters so label names containing '%', '_', or '\'
	// are matched literally rather than as SQL LIKE patterns.
	// Order matters: backslash must be escaped first to avoid double-escaping.
	escaped := strings.ReplaceAll(labelName, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, "%", `\%`)
	escaped = strings.ReplaceAll(escaped, "_", `\_`)

	predicate := fmt.Sprintf(
		`( %[1]s = ? OR %[1]s LIKE ? || ',%%' ESCAPE '\' OR %[1]s LIKE '%%,' || ? ESCAPE '\' OR %[1]s LIKE '%%,' || ? || ',%%' ESCAPE '\' )`,
		colExpr,
	)
	args := []any{labelName, escaped, escaped, escaped}
	return predicate, args
}

// Placeholder always returns "?" for SQLite positional parameter style.
// The index n is accepted for interface compatibility but is not used.
func (SQLiteDialect) Placeholder(_ int) string {
	return "?"
}
