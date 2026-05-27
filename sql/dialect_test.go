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

	// The predicate must contain four OR branches (exact match + 3 LIKE).
	// args has 4 values: 1 unescaped for =, 3 escaped for LIKE.
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

	want := `( n0.labels = ? OR n0.labels LIKE ? || ',%' ESCAPE '\' OR n0.labels LIKE '%,' || ? ESCAPE '\' OR n0.labels LIKE '%,' || ? || ',%' ESCAPE '\' )`
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

// TestSQLiteDialect_LabelContains_WildcardEscaping verifies that label names
// containing LIKE wildcard characters are escaped so they match literally.
func TestSQLiteDialect_LabelContains_WildcardEscaping(t *testing.T) {
	d := sqldialect.SQLiteDialect{}

	cases := []struct {
		labelName    string
		wantExact    string // arg[0]: unescaped, used in the = branch
		wantEscaped  string // args[1..3]: escaped, used in the LIKE branches
	}{
		{
			labelName:   "50%_off",
			wantExact:   "50%_off",
			wantEscaped: `50\%\_off`,
		},
		{
			labelName:   "100%",
			wantExact:   "100%",
			wantEscaped: `100\%`,
		},
		{
			labelName:   "under_score",
			wantExact:   "under_score",
			wantEscaped: `under\_score`,
		},
		{
			// Backslash must be escaped first to avoid double-escaping.
			labelName:   `back\slash`,
			wantExact:   `back\slash`,
			wantEscaped: `back\\slash`,
		},
		{
			// Label without wildcards: escaped value equals original.
			labelName:   "Person",
			wantExact:   "Person",
			wantEscaped: "Person",
		},
	}

	for _, tc := range cases {
		_, args := d.LabelContains("n0.labels", tc.labelName)
		if len(args) != 4 {
			t.Errorf("label %q: args length = %d; want 4", tc.labelName, len(args))
			continue
		}
		// args[0]: unescaped value for the exact-match (=) branch.
		if args[0] != tc.wantExact {
			t.Errorf("label %q: args[0] (exact) = %q; want %q", tc.labelName, args[0], tc.wantExact)
		}
		// args[1..3]: escaped values for the LIKE branches.
		for i := 1; i <= 3; i++ {
			if args[i] != tc.wantEscaped {
				t.Errorf("label %q: args[%d] (LIKE) = %q; want %q", tc.labelName, i, args[i], tc.wantEscaped)
			}
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
