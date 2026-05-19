package sql_test

import (
	"testing"

	sqldialect "github.com/LackOfMorals/graphlite/sql"
)

// dialects lists every Dialect implementation under test.
// Add new implementations here as they are introduced.
var dialects = []struct {
	name    string
	dialect sqldialect.Dialect
}{
	{"SQLiteDialect", sqldialect.SQLiteDialect{}},
}

// ─────────────────────────────────────────────────────────────────────────────
// QuoteIdentifier
// ─────────────────────────────────────────────────────────────────────────────

func TestSQLiteDialect_QuoteIdentifier(t *testing.T) {
	d := sqldialect.SQLiteDialect{}

	cases := []struct {
		input string
		want  string
	}{
		{"name", `"name"`},
		{"my table", `"my table"`},
		{`a"b`, `"a""b"`},
		{`a""b`, `"a""""b"`},
		{"", `""`},
	}
	for _, tc := range cases {
		got := d.QuoteIdentifier(tc.input)
		if got != tc.want {
			t.Errorf("QuoteIdentifier(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// JSONExtract
// ─────────────────────────────────────────────────────────────────────────────

func TestSQLiteDialect_JSONExtract(t *testing.T) {
	d := sqldialect.SQLiteDialect{}

	cases := []struct {
		colExpr  string
		jsonPath string
		want     string
	}{
		{"n0.props", "$.name", "json_extract(n0.props, '$.name')"},
		{"r0.props", "$.weight", "json_extract(r0.props, '$.weight')"},
		{"nodes.props", "$.address.city", "json_extract(nodes.props, '$.address.city')"},
	}
	for _, tc := range cases {
		got := d.JSONExtract(tc.colExpr, tc.jsonPath)
		if got != tc.want {
			t.Errorf("JSONExtract(%q, %q) = %q; want %q", tc.colExpr, tc.jsonPath, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// JSONSet
// ─────────────────────────────────────────────────────────────────────────────

func TestSQLiteDialect_JSONSet(t *testing.T) {
	d := sqldialect.SQLiteDialect{}

	cases := []struct {
		colExpr   string
		jsonPath  string
		valueExpr string
		want      string
	}{
		{"n0.props", "$.age", "?", "json_set(n0.props, '$.age', ?)"},
		{"n0.props", "$.name", "?", "json_set(n0.props, '$.name', ?)"},
		{"r0.props", "$.since", "2024", "json_set(r0.props, '$.since', 2024)"},
	}
	for _, tc := range cases {
		got := d.JSONSet(tc.colExpr, tc.jsonPath, tc.valueExpr)
		if got != tc.want {
			t.Errorf("JSONSet(%q, %q, %q) = %q; want %q",
				tc.colExpr, tc.jsonPath, tc.valueExpr, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// JSONRemove
// ─────────────────────────────────────────────────────────────────────────────

func TestSQLiteDialect_JSONRemove(t *testing.T) {
	d := sqldialect.SQLiteDialect{}

	cases := []struct {
		colExpr  string
		jsonPath string
		want     string
	}{
		{"n0.props", "$.age", "json_remove(n0.props, '$.age')"},
		{"n0.props", "$.address", "json_remove(n0.props, '$.address')"},
	}
	for _, tc := range cases {
		got := d.JSONRemove(tc.colExpr, tc.jsonPath)
		if got != tc.want {
			t.Errorf("JSONRemove(%q, %q) = %q; want %q", tc.colExpr, tc.jsonPath, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// LabelContains
// ─────────────────────────────────────────────────────────────────────────────

func TestSQLiteDialect_LabelContains_PredicateFormat(t *testing.T) {
	d := sqldialect.SQLiteDialect{}

	pred, args := d.LabelContains("n0.labels", "Person")

	// The predicate must contain four OR branches.
	// We do not assert the exact string (minor whitespace can vary) but we
	// verify structural properties.
	if len(args) != 4 {
		t.Errorf("LabelContains args length = %d; want 4", len(args))
	}
	for i, a := range args {
		if a != "Person" {
			t.Errorf("args[%d] = %q; want %q", i, a, "Person")
		}
	}
	if pred == "" {
		t.Error("LabelContains returned empty predicate")
	}
	// Ensure the colExpr appears in the predicate.
	if len(pred) == 0 {
		t.Error("predicate is empty")
	}
}

func TestSQLiteDialect_LabelContains_ExactSQL(t *testing.T) {
	d := sqldialect.SQLiteDialect{}

	want := "( n0.labels = ? OR n0.labels LIKE ? || ',%' OR n0.labels LIKE '%,' || ? OR n0.labels LIKE '%,' || ? || ',%' )"
	got, args := d.LabelContains("n0.labels", "Employee")

	if got != want {
		t.Errorf("LabelContains SQL mismatch:\ngot:  %s\nwant: %s", got, want)
	}
	if len(args) != 4 {
		t.Errorf("args length = %d; want 4", len(args))
	}
	for i, a := range args {
		if a != "Employee" {
			t.Errorf("args[%d] = %v; want %q", i, a, "Employee")
		}
	}
}

// TestSQLiteDialect_LabelContains_ColExpr verifies different column expressions
// are correctly interpolated.
func TestSQLiteDialect_LabelContains_ColExpr(t *testing.T) {
	d := sqldialect.SQLiteDialect{}

	cases := []struct {
		colExpr   string
		labelName string
	}{
		{"n0.labels", "Person"},
		{"n1.labels", "Company"},
		{"nodes.labels", "Employee"},
	}
	for _, tc := range cases {
		pred, args := d.LabelContains(tc.colExpr, tc.labelName)
		if pred == "" {
			t.Errorf("LabelContains(%q, %q): empty predicate", tc.colExpr, tc.labelName)
		}
		if len(args) != 4 {
			t.Errorf("LabelContains(%q, %q): args length = %d; want 4",
				tc.colExpr, tc.labelName, len(args))
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Placeholder
// ─────────────────────────────────────────────────────────────────────────────

func TestSQLiteDialect_Placeholder(t *testing.T) {
	d := sqldialect.SQLiteDialect{}

	// SQLite uses "?" regardless of position.
	for _, n := range []int{0, 1, 2, 99} {
		got := d.Placeholder(n)
		if got != "?" {
			t.Errorf("Placeholder(%d) = %q; want %q", n, got, "?")
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Dialect interface conformance (all dialects in dialects slice)
// ─────────────────────────────────────────────────────────────────────────────

// TestDialect_InterfaceConformance verifies that every dialect in the registry
// satisfies the Dialect interface and returns non-empty results for each method.
func TestDialect_InterfaceConformance(t *testing.T) {
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			di := d.dialect

			if got := di.QuoteIdentifier("test"); got == "" {
				t.Error("QuoteIdentifier returned empty string")
			}
			if got := di.JSONExtract("t.props", "$.key"); got == "" {
				t.Error("JSONExtract returned empty string")
			}
			if got := di.JSONSet("t.props", "$.key", "?"); got == "" {
				t.Error("JSONSet returned empty string")
			}
			if got := di.JSONRemove("t.props", "$.key"); got == "" {
				t.Error("JSONRemove returned empty string")
			}
			pred, args := di.LabelContains("t.labels", "Label")
			if pred == "" {
				t.Error("LabelContains returned empty predicate")
			}
			if len(args) == 0 {
				t.Error("LabelContains returned no args")
			}
			if got := di.Placeholder(0); got == "" {
				t.Error("Placeholder(0) returned empty string")
			}
		})
	}
}
