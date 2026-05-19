package sql_test

import (
	"strings"
	"testing"

	"github.com/LackOfMorals/graphlite/cypher"
	sqldialect "github.com/LackOfMorals/graphlite/sql"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helper: parse + plan + translate
// ─────────────────────────────────────────────────────────────────────────────

func translateCypher(t *testing.T, query string) sqldialect.Result {
	t.Helper()

	q, err := cypher.Parse(query)
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	scope := cypher.NewScope()
	plan, err := cypher.Plan(q, scope)
	if err != nil {
		t.Fatalf("Plan(%q): %v", query, err)
	}
	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(plan, scope)
	if err != nil {
		t.Fatalf("Translate(%q): %v", query, err)
	}
	return result
}

// containsAll fails if result.SQL does not contain all of the given substrings.
func containsAll(t *testing.T, result sqldialect.Result, substrings ...string) {
	t.Helper()
	for _, s := range substrings {
		if !strings.Contains(result.SQL, s) {
			t.Errorf("SQL missing %q\n SQL: %s", s, result.SQL)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 01: MATCH (n) RETURN n
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_MatchAllNodes_ReturnNode(t *testing.T) {
	result := translateCypher(t, "MATCH (n) RETURN n")
	containsAll(t, result,
		"SELECT",
		"FROM nodes",
		"json_object",
	)
	// No WHERE clause needed for unconstrained match.
	if strings.Contains(result.SQL, " WHERE ") {
		t.Errorf("unexpected WHERE in unconstrained MATCH: %s", result.SQL)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 02: MATCH (n:Person) RETURN n.name
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_MatchByLabel_ReturnProp(t *testing.T) {
	result := translateCypher(t, "MATCH (n:Person) RETURN n.name")
	containsAll(t, result,
		"SELECT",
		"FROM nodes",
		"WHERE",
		"labels",   // label check
		"json_extract", // property access
		"$.name",
	)
	// Args should contain four copies of "Person" for the label check.
	if len(result.Args) != 4 {
		t.Errorf("expected 4 args (label check), got %d: %v", len(result.Args), result.Args)
	}
	for i, a := range result.Args {
		if a != "Person" {
			t.Errorf("arg[%d] = %v; want %q", i, a, "Person")
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 03: MATCH (n:Person) WHERE n.age > 30 RETURN n.name AS name
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_MatchLabelWhere_ReturnAlias(t *testing.T) {
	result := translateCypher(t, `MATCH (n:Person) WHERE n.age > 30 RETURN n.name AS name`)
	containsAll(t, result,
		"SELECT",
		"json_extract",
		"$.name",
		" AS name",
		"WHERE",
		"$.age",
		">",
		"30",
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 04: Parameter reference in WHERE ($p)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_WhereParam(t *testing.T) {
	result := translateCypher(t, `MATCH (n:Person) WHERE n.name = $name RETURN n.name`)
	containsAll(t, result,
		"WHERE",
		"json_extract",
		"$.name",
		"=",
		"?",
	)
	// We expect at least one sentinel for $name plus four args for the label check.
	if len(result.Args) < 5 {
		t.Errorf("expected at least 5 args (4 label + 1 param), got %d", len(result.Args))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 05: Single-hop directed MATCH (a)-[r:KNOWS]->(b) RETURN b.name
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_SingleHopDirected_ReturnProp(t *testing.T) {
	result := translateCypher(t, `MATCH (a)-[r:KNOWS]->(b) RETURN b.name`)
	containsAll(t, result,
		"SELECT",
		"FROM nodes",
		"JOIN edges",  // relationship join
		"JOIN nodes",  // end node join
		"start_id",
		"end_id",
		"$.name",
	)
	// Relationship type check.
	containsAll(t, result, "type")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 06: Single-hop undirected MATCH (a)-[r:LIKES]-(b) RETURN a.name, b.name
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_SingleHopUndirected(t *testing.T) {
	result := translateCypher(t, `MATCH (a)-[r:LIKES]-(b) RETURN a.name, b.name`)
	// Undirected must check both directions.
	containsAll(t, result,
		"start_id",
		"end_id",
		"OR",
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 07: Multi-hop (3-hop) MATCH
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_ThreeHop_MultipleJoins(t *testing.T) {
	result := translateCypher(t,
		`MATCH (a)-[r1:KNOWS]->(b)-[r2:LIKES]->(c) RETURN c.name`)
	// Two hops → two JOIN edges + two JOIN nodes.
	edgeJoinCount := strings.Count(result.SQL, "JOIN edges")
	if edgeJoinCount < 2 {
		t.Errorf("expected at least 2 JOIN edges for 2-hop, got %d\nSQL: %s",
			edgeJoinCount, result.SQL)
	}
	nodeJoinCount := strings.Count(result.SQL, "JOIN nodes")
	if nodeJoinCount < 2 {
		t.Errorf("expected at least 2 JOIN nodes for 2-hop, got %d\nSQL: %s",
			nodeJoinCount, result.SQL)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 08: ORDER BY ASC/DESC
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_OrderByAscDesc(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person) RETURN n.name, n.age ORDER BY n.name ASC, n.age DESC`)
	containsAll(t, result,
		"ORDER BY",
		"ASC",
		"DESC",
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 09: LIMIT and SKIP
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_LimitSkip(t *testing.T) {
	// Grammar quirk: SKIP before LIMIT in cloudprivacylabs/opencypher.
	result := translateCypher(t,
		`MATCH (n:Person) RETURN n.name SKIP 5 LIMIT 10`)
	containsAll(t, result,
		"LIMIT 10",
		"OFFSET 5",
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 10: LIMIT only
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_LimitOnly(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person) RETURN n.name LIMIT 3`)
	containsAll(t, result, "LIMIT 3")
	if strings.Contains(result.SQL, "OFFSET") {
		t.Errorf("unexpected OFFSET when SKIP not specified: %s", result.SQL)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 11: RETURN DISTINCT
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_ReturnDistinct(t *testing.T) {
	result := translateCypher(t, `MATCH (n:Person) RETURN DISTINCT n.name`)
	containsAll(t, result, "SELECT DISTINCT")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 12: AND / OR / NOT in WHERE
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_WhereAndOrNot(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person) WHERE n.age > 18 AND NOT n.name = 'Bob' RETURN n.name`)
	containsAll(t, result,
		"AND",
		"NOT",
		"$.age",
		"$.name",
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 13: Inline node property constraint
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_InlineNodePropConstraint(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person {name: 'Alice'}) RETURN n.age`)
	containsAll(t, result,
		"WHERE",
		"json_extract",
		"$.name",
		"?",  // string literal becomes a bind arg
	)
	// Args: 4 label args + 1 string literal for 'Alice'.
	if len(result.Args) < 5 {
		t.Errorf("expected at least 5 args, got %d: %v", len(result.Args), result.Args)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 14: Multi-label MATCH AND semantics
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_MultiLabel(t *testing.T) {
	result := translateCypher(t, `MATCH (n:Person:Employee) RETURN n.name`)
	// Both label predicates must produce AND semantics in the WHERE clause.
	// Label values are passed as bind args (not inlined in SQL).
	containsAll(t, result, "AND", "WHERE", "labels")
	// There should be 8 args: 4 for "Person" + 4 for "Employee".
	if len(result.Args) != 8 {
		t.Errorf("expected 8 args (4 per label), got %d: %v", len(result.Args), result.Args)
	}
	// Check both label values appear in args.
	foundPerson, foundEmployee := false, false
	for _, a := range result.Args {
		if a == "Person" {
			foundPerson = true
		}
		if a == "Employee" {
			foundEmployee = true
		}
	}
	if !foundPerson {
		t.Errorf("expected 'Person' in args: %v", result.Args)
	}
	if !foundEmployee {
		t.Errorf("expected 'Employee' in args: %v", result.Args)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 15: Parameter position in args matches ? order in SQL
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_ParamPositionCorrect(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person) WHERE n.age > $minAge RETURN n.name`)
	// We expect: 4 label args + 1 param sentinel for $minAge.
	if len(result.Args) != 5 {
		t.Errorf("expected 5 args, got %d: %v", len(result.Args), result.Args)
	}
	// The last arg should be a paramSentinel for "minAge".
	// We can't access unexported paramSentinel, but we can verify it's not a string.
	last := result.Args[4]
	if _, isString := last.(string); isString {
		t.Errorf("expected param sentinel at args[4], got string %q", last)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 16: Directed hop — left direction
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_LeftDirectedHop(t *testing.T) {
	result := translateCypher(t, `MATCH (a)<-[r:KNOWS]-(b) RETURN a.name`)
	// Left-directed: edge end_id should match the start node (a).
	containsAll(t, result, "end_id")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 17: Multiple projections with aliases
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_MultipleProjectionsWithAliases(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person) RETURN n.name AS personName, n.age AS personAge`)
	containsAll(t, result,
		"AS personName",
		"AS personAge",
		"$.name",
		"$.age",
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 18: Golden-fixture comparison for label+property MATCH
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_GoldenFixture_LabelPropMatch(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person) WHERE n.name = $p RETURN n.name AS name`)

	// The SQL must:
	// 1. SELECT json_extract with AS name
	// 2. FROM nodes <alias>
	// 3. WHERE label check AND property check
	containsAll(t, result,
		"SELECT",
		"json_extract",
		"$.name",
		" AS name",
		"FROM nodes",
		"WHERE",
		"labels",
		"AND",
	)

	// Param count: 4 label args + 1 param sentinel.
	if len(result.Args) != 5 {
		t.Errorf("expected 5 args, got %d", len(result.Args))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 19: Single-hop with end node label constraint
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_SingleHop_EndNodeLabel(t *testing.T) {
	result := translateCypher(t,
		`MATCH (a:Person)-[r:KNOWS]->(b:Company) RETURN b.name`)
	// Structural checks in the SQL.
	containsAll(t, result,
		"JOIN edges",
		"JOIN nodes",
		"WHERE",
	)
	// Both label values must appear in bind args (not inlined in SQL).
	foundPerson, foundCompany := false, false
	for _, a := range result.Args {
		if a == "Person" {
			foundPerson = true
		}
		if a == "Company" {
			foundCompany = true
		}
	}
	if !foundPerson {
		t.Errorf("expected 'Person' in args: %v", result.Args)
	}
	if !foundCompany {
		t.Errorf("expected 'Company' in args: %v", result.Args)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 20: RETURN whole relationship variable
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_ReturnRelationshipVar(t *testing.T) {
	result := translateCypher(t,
		`MATCH (a)-[r:KNOWS]->(b) RETURN r`)
	containsAll(t, result,
		"json_object",
		"type",
		"start_id",
		"end_id",
	)
}
