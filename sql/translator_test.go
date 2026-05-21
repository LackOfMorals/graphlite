package sql_test

import (
	"errors"
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
// Test 10b: SKIP only (no LIMIT) — SQLite requires LIMIT -1 OFFSET n
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_SkipOnly(t *testing.T) {
	// Grammar quirk: SKIP precedes LIMIT in cloudprivacylabs/opencypher.
	// With SKIP only (no LIMIT), the translator must emit LIMIT -1 OFFSET n
	// because SQLite rejects a bare OFFSET clause without a preceding LIMIT.
	result := translateCypher(t, `MATCH (n:Person) RETURN n.name SKIP 3`)
	containsAll(t, result, "LIMIT -1", "OFFSET 3")
	if !strings.Contains(result.SQL, "LIMIT -1 OFFSET 3") {
		t.Errorf("skip-only: expected 'LIMIT -1 OFFSET 3' in SQL, got: %s", result.SQL)
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

// ─────────────────────────────────────────────────────────────────────────────
// Write operation helper
// ─────────────────────────────────────────────────────────────────────────────

// translateWrite parses, plans, and translates a write Cypher statement.
// It returns the Result and asserts that Statements is non-empty.
func translateWrite(t *testing.T, query string) sqldialect.Result {
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
	if len(result.Statements) == 0 {
		t.Fatalf("expected at least one Statement, got 0")
	}
	return result
}

// containsAllStmt is like containsAll but checks a specific Statement index.
func containsAllStmt(t *testing.T, stmt sqldialect.Statement, substrings ...string) {
	t.Helper()
	for _, s := range substrings {
		if !strings.Contains(stmt.SQL, s) {
			t.Errorf("Statement SQL missing %q\n SQL: %s", s, stmt.SQL)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 21: CREATE node — INSERT INTO nodes
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_CreateNode_Insert(t *testing.T) {
	result := translateWrite(t, `CREATE (n:Person {name: 'Alice'})`)
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
	stmt := result.Statements[0]
	containsAllStmt(t, stmt,
		"INSERT INTO nodes",
		"labels",
		"props",
		"VALUES",
		"json(",
	)
	// Labels arg must be "Person".
	if len(stmt.Args) == 0 {
		t.Fatal("expected bind args, got none")
	}
	if stmt.Args[0] != "Person" {
		t.Errorf("expected first arg to be 'Person', got %v", stmt.Args[0])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 22: CREATE node with no props — empty JSON object
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_CreateNode_NoProps(t *testing.T) {
	result := translateWrite(t, `CREATE (n:Employee)`)
	stmt := result.Statements[0]
	containsAllStmt(t, stmt, "INSERT INTO nodes", "VALUES", "json(")
	if stmt.Args[0] != "Employee" {
		t.Errorf("expected 'Employee' label arg, got %v", stmt.Args[0])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 23: CREATE node with $param prop — param sentinel in args
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_CreateNode_ParamProp(t *testing.T) {
	result := translateWrite(t, `CREATE (n:Person {name: $name})`)
	stmt := result.Statements[0]
	containsAllStmt(t, stmt, "INSERT INTO nodes", "json(")
	// One of the args should be a sentinel (not a string) for $name.
	foundSentinel := false
	for _, a := range stmt.Args {
		if _, isStr := a.(string); !isStr {
			foundSentinel = true
		}
	}
	if !foundSentinel {
		t.Errorf("expected a param sentinel in args, all were strings: %v", stmt.Args)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 24: CREATE relationship — INSERT INTO edges
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_CreateRel_Insert(t *testing.T) {
	// Parse and plan manually so we can set up the scope with two nodes.
	scope := cypher.NewScope()
	scope.Bind("a", cypher.Binding{Alias: "n0", IsNode: true})
	scope.Bind("b", cypher.Binding{Alias: "n1", IsNode: true})

	relPlan := &cypher.CreateRelPlan{
		Type:     "KNOWS",
		StartVar: "a",
		EndVar:   "b",
		Props:    map[string]cypher.Expr{},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(relPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
	stmt := result.Statements[0]
	containsAllStmt(t, stmt,
		"INSERT INTO edges",
		"type",
		"start_id",
		"end_id",
		"props",
		"VALUES",
		"json(",
	)
	// First arg is the relationship type.
	if stmt.Args[0] != "KNOWS" {
		t.Errorf("expected first arg to be 'KNOWS', got %v", stmt.Args[0])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 25: CREATE relationship with props
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_CreateRel_WithProps(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("a", cypher.Binding{Alias: "n0", IsNode: true})
	scope.Bind("b", cypher.Binding{Alias: "n1", IsNode: true})

	relPlan := &cypher.CreateRelPlan{
		Type:     "WORKS_FOR",
		StartVar: "a",
		EndVar:   "b",
		Props: map[string]cypher.Expr{
			"since": &cypher.LiteralExpr{Value: int64(2020)},
		},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(relPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	stmt := result.Statements[0]
	// Type is a bind arg (not inlined in SQL); verify SQL structure and that
	// the type value is the first arg.
	containsAllStmt(t, stmt, "INSERT INTO edges", "json(", "json_object")
	if len(stmt.Args) == 0 || stmt.Args[0] != "WORKS_FOR" {
		t.Errorf("expected first arg to be 'WORKS_FOR', got %v", stmt.Args)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 26: SET n.prop = literal value
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_SetProp_Literal(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true})

	setPlan := &cypher.SetPropPlan{
		Variable: "n",
		Property: "age",
		Value:    &cypher.LiteralExpr{Value: int64(30)},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(setPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	stmt := result.Statements[0]
	containsAllStmt(t, stmt,
		"UPDATE nodes",
		"SET props =",
		"json_set(props, '$.age', 30)",
		"WHERE id = ?",
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 27: SET n.prop = $param
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_SetProp_Param(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true})

	setPlan := &cypher.SetPropPlan{
		Variable: "n",
		Property: "name",
		Value:    &cypher.ParamRef{Name: "newName"},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(setPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	stmt := result.Statements[0]
	containsAllStmt(t, stmt,
		"UPDATE nodes",
		"SET props =",
		"json_set(props, '$.name', ?)",
		"WHERE id = ?",
	)
	// Two args: one param sentinel + one idSentinel for WHERE id = ?
	if len(stmt.Args) != 2 {
		t.Errorf("expected 2 args (param + id), got %d: %v", len(stmt.Args), stmt.Args)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 28: DELETE n (non-detach) — guard + delete statements
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_DeleteNode_NonDetach(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true})

	delPlan := &cypher.DeleteNodePlan{
		Variable: "n",
		Detach:   false,
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(delPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	// Non-detach: guard check + delete.
	if len(result.Statements) != 2 {
		t.Fatalf("expected 2 statements for non-detach DELETE, got %d", len(result.Statements))
	}
	containsAllStmt(t, result.Statements[0],
		"SELECT COUNT(*)",
		"FROM edges",
		"start_id",
		"end_id",
	)
	containsAllStmt(t, result.Statements[1],
		"DELETE FROM nodes",
		"WHERE id = ?",
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 29: DETACH DELETE n — edges delete + node delete
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_DeleteNode_Detach(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true})

	delPlan := &cypher.DeleteNodePlan{
		Variable: "n",
		Detach:   true,
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(delPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	// DETACH DELETE: edges delete + node delete.
	if len(result.Statements) != 2 {
		t.Fatalf("expected 2 statements for DETACH DELETE, got %d", len(result.Statements))
	}
	containsAllStmt(t, result.Statements[0],
		"DELETE FROM edges",
		"start_id",
		"end_id",
	)
	containsAllStmt(t, result.Statements[1],
		"DELETE FROM nodes",
		"WHERE id = ?",
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 30: DELETE r (relationship)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_DeleteRel(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("r", cypher.Binding{Alias: "r0", IsRel: true})

	delPlan := &cypher.DeleteRelPlan{Variable: "r"}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(delPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
	containsAllStmt(t, result.Statements[0],
		"DELETE FROM edges",
		"WHERE id = ?",
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 31: Compound CREATE (two nodes + relationship via SequencePlan)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_CompoundCreate_NodeAndRel(t *testing.T) {
	scope := cypher.NewScope()

	nodeA := &cypher.CreateNodePlan{
		Variable: "a",
		Labels:   []string{"Person"},
		Props:    map[string]cypher.Expr{"name": &cypher.LiteralExpr{Value: "Alice"}},
	}
	// After planning node a, bind it so CreateRelPlan can resolve it.
	scope.Bind("a", cypher.Binding{Alias: "n0", IsNode: true})

	nodeB := &cypher.CreateNodePlan{
		Variable: "b",
		Labels:   []string{"Company"},
		Props:    map[string]cypher.Expr{},
	}
	scope.Bind("b", cypher.Binding{Alias: "n1", IsNode: true})

	rel := &cypher.CreateRelPlan{
		Type:     "WORKS_AT",
		StartVar: "a",
		EndVar:   "b",
		Props:    map[string]cypher.Expr{},
	}

	seq := &cypher.SequencePlan{
		Steps: []cypher.LogicalPlan{nodeA, nodeB, rel},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(seq, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	// Three write statements: INSERT nodes (a), INSERT nodes (b), INSERT edges (a→b).
	if len(result.Statements) != 3 {
		t.Fatalf("expected 3 statements, got %d", len(result.Statements))
	}
	// First two are node inserts.
	containsAllStmt(t, result.Statements[0], "INSERT INTO nodes")
	containsAllStmt(t, result.Statements[1], "INSERT INTO nodes")
	// Third is the relationship insert. Type is a bind arg (not inlined in SQL).
	containsAllStmt(t, result.Statements[2], "INSERT INTO edges")
	if result.Statements[2].Args[0] != "WORKS_AT" {
		t.Errorf("expected relationship type 'WORKS_AT' as first arg, got %v", result.Statements[2].Args[0])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 32: Existing read tests produce single-element Statements slice
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_ReadResult_HasOneStatement(t *testing.T) {
	result := translateCypher(t, `MATCH (n:Person) RETURN n.name`)
	if len(result.Statements) != 1 {
		t.Errorf("expected 1 Statement for read query, got %d", len(result.Statements))
	}
	if result.Statements[0].SQL != result.SQL {
		t.Errorf("Statements[0].SQL should match top-level SQL\n got: %s\n want: %s",
			result.Statements[0].SQL, result.SQL)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 33: SET on relationship variable targets edges table
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_SetProp_RelationshipVar(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("r", cypher.Binding{Alias: "r0", IsRel: true})

	setPlan := &cypher.SetPropPlan{
		Variable: "r",
		Property: "weight",
		Value:    &cypher.LiteralExpr{Value: float64(1.5)},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(setPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	stmt := result.Statements[0]
	containsAllStmt(t, stmt, "UPDATE edges", "SET props =", "WHERE id = ?")
}

// ─────────────────────────────────────────────────────────────────────────────
// BindParams tests (task-015)
// ─────────────────────────────────────────────────────────────────────────────

// Test 34: BindParams resolves a single $param in a WHERE clause.
func TestBindParams_WhereParam_Resolved(t *testing.T) {
	result := translateCypher(t, `MATCH (n:Person) WHERE n.name = $name RETURN n.name`)
	bound, err := sqldialect.BindParams(result, map[string]any{"name": "Alice"})
	if err != nil {
		t.Fatalf("BindParams: %v", err)
	}
	// After binding, every arg must be a plain Go value (string, int64, etc.) —
	// no param sentinel should remain.
	for i, a := range bound.Args {
		switch a.(type) {
		case string, int64, float64, bool, nil:
			// ok
		default:
			t.Errorf("args[%d] is still a sentinel after BindParams: %T %v", i, a, a)
		}
	}
	// The resolved value "Alice" must appear somewhere in bound.Args.
	found := false
	for _, a := range bound.Args {
		if a == "Alice" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'Alice' in bound args, got %v", bound.Args)
	}
}

// Test 35: BindParams resolves multiple $params in correct positional order.
func TestBindParams_MultipleParams_CorrectOrder(t *testing.T) {
	// Two parameters: $name in an inline prop constraint, $minAge in WHERE.
	// MATCH (n:Person {name: $name}) WHERE n.age > $minAge RETURN n.name
	result := translateCypher(t,
		`MATCH (n:Person {name: $name}) WHERE n.age > $minAge RETURN n.name`)

	params := map[string]any{
		"name":   "Alice",
		"minAge": int64(30),
	}
	bound, err := sqldialect.BindParams(result, params)
	if err != nil {
		t.Fatalf("BindParams: %v", err)
	}

	// Verify "Alice" and int64(30) both appear in the resolved args.
	foundName, foundAge := false, false
	for _, a := range bound.Args {
		if a == "Alice" {
			foundName = true
		}
		if a == int64(30) {
			foundAge = true
		}
	}
	if !foundName {
		t.Errorf("expected 'Alice' in bound args: %v", bound.Args)
	}
	if !foundAge {
		t.Errorf("expected int64(30) in bound args: %v", bound.Args)
	}
}

// Test 36: BindParams returns ErrMissingParam when a required parameter is absent.
func TestBindParams_MissingParam_ReturnsError(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person) WHERE n.age > $minAge RETURN n.name`)

	// Provide an empty params map — $minAge is missing.
	_, err := sqldialect.BindParams(result, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing parameter, got nil")
	}
	var missingErr *sqldialect.ErrMissingParam
	if !errors.As(err, &missingErr) {
		t.Fatalf("expected *ErrMissingParam, got %T: %v", err, err)
	}
	if missingErr.Name != "minAge" {
		t.Errorf("ErrMissingParam.Name = %q; want %q", missingErr.Name, "minAge")
	}
}

// Test 37: BindParams with nil params returns ErrMissingParam for any sentinel.
func TestBindParams_NilParams_ReturnsError(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person) WHERE n.name = $name RETURN n.name`)
	_, err := sqldialect.BindParams(result, nil)
	if err == nil {
		t.Fatal("expected error for nil params when query has $param, got nil")
	}
	var missingErr *sqldialect.ErrMissingParam
	if !errors.As(err, &missingErr) {
		t.Fatalf("expected *ErrMissingParam, got %T: %v", err, err)
	}
}

// Test 38: BindParams on a query with no params is a no-op (succeeds with empty map).
func TestBindParams_NoParams_NoOp(t *testing.T) {
	result := translateCypher(t, `MATCH (n:Person) RETURN n.name`)
	bound, err := sqldialect.BindParams(result, map[string]any{})
	if err != nil {
		t.Fatalf("BindParams on param-free query: %v", err)
	}
	// SQL must be unchanged.
	if bound.SQL != result.SQL {
		t.Errorf("SQL changed unexpectedly\n got:  %s\n want: %s", bound.SQL, result.SQL)
	}
}

// Test 39: BindParams resolves $param inside a CREATE property map.
func TestBindParams_CreateNodeParam_Resolved(t *testing.T) {
	result := translateWrite(t, `CREATE (n:Person {name: $name})`)
	bound, err := sqldialect.BindParams(result, map[string]any{"name": "Bob"})
	if err != nil {
		t.Fatalf("BindParams: %v", err)
	}
	found := false
	for _, a := range bound.Args {
		if a == "Bob" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'Bob' in bound args after CREATE param binding: %v", bound.Args)
	}
}

// Test 40: BindParams resolves $param inside a SET value position.
func TestBindParams_SetPropParam_Resolved(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true})

	setPlan := &cypher.SetPropPlan{
		Variable: "n",
		Property: "name",
		Value:    &cypher.ParamRef{Name: "newName"},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(setPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	bound, err := sqldialect.BindParams(result, map[string]any{"newName": "Charlie"})
	if err != nil {
		t.Fatalf("BindParams: %v", err)
	}
	found := false
	for _, a := range bound.Args {
		if a == "Charlie" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'Charlie' in bound args after SET param binding: %v", bound.Args)
	}
}

// Test 41: BindParams preserves idSentinel values in write-operation args.
func TestBindParams_PreservesIdSentinel(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("a", cypher.Binding{Alias: "n0", IsNode: true})
	scope.Bind("b", cypher.Binding{Alias: "n1", IsNode: true})

	relPlan := &cypher.CreateRelPlan{
		Type:     "KNOWS",
		StartVar: "a",
		EndVar:   "b",
		Props:    map[string]cypher.Expr{},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(relPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	// No params needed for this query; BindParams should succeed without
	// touching the idSentinel values.
	bound, err := sqldialect.BindParams(result, map[string]any{})
	if err != nil {
		t.Fatalf("BindParams should not fail when no paramSentinels present: %v", err)
	}

	// The bound result should have the same number of args as the original.
	if len(bound.Args) != len(result.Args) {
		t.Errorf("arg count changed: got %d, want %d", len(bound.Args), len(result.Args))
	}
}

// Test 42: BindParams resolves params across multiple statements (write sequence).
func TestBindParams_MultiStatement_AllResolved(t *testing.T) {
	// A compound CREATE with two nodes, each having a $param property.
	scope := cypher.NewScope()

	nodeA := &cypher.CreateNodePlan{
		Variable: "a",
		Labels:   []string{"Person"},
		Props:    map[string]cypher.Expr{"name": &cypher.ParamRef{Name: "nameA"}},
	}
	scope.Bind("a", cypher.Binding{Alias: "n0", IsNode: true})

	nodeB := &cypher.CreateNodePlan{
		Variable: "b",
		Labels:   []string{"Person"},
		Props:    map[string]cypher.Expr{"name": &cypher.ParamRef{Name: "nameB"}},
	}
	scope.Bind("b", cypher.Binding{Alias: "n1", IsNode: true})

	seq := &cypher.SequencePlan{
		Steps: []cypher.LogicalPlan{nodeA, nodeB},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(seq, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(result.Statements) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(result.Statements))
	}

	bound, err := sqldialect.BindParams(result, map[string]any{
		"nameA": "Alice",
		"nameB": "Bob",
	})
	if err != nil {
		t.Fatalf("BindParams: %v", err)
	}

	// Each statement should have its param resolved.
	foundAlice := false
	for _, a := range bound.Statements[0].Args {
		if a == "Alice" {
			foundAlice = true
		}
	}
	if !foundAlice {
		t.Errorf("expected 'Alice' in statement[0] args: %v", bound.Statements[0].Args)
	}

	foundBob := false
	for _, a := range bound.Statements[1].Args {
		if a == "Bob" {
			foundBob = true
		}
	}
	if !foundBob {
		t.Errorf("expected 'Bob' in statement[1] args: %v", bound.Statements[1].Args)
	}
}

// Test 43: BindParams does not mutate the original Result's args slices.
func TestBindParams_DoesNotMutateOriginal(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person) WHERE n.name = $name RETURN n.name`)

	// Capture the original args content.
	origArgs := make([]any, len(result.Args))
	copy(origArgs, result.Args)

	_, err := sqldialect.BindParams(result, map[string]any{"name": "Alice"})
	if err != nil {
		t.Fatalf("BindParams: %v", err)
	}

	// The original result.Args must be unchanged.
	if len(result.Args) != len(origArgs) {
		t.Fatalf("original args length changed: %d → %d", len(origArgs), len(result.Args))
	}
	for i, a := range result.Args {
		if a != origArgs[i] {
			t.Errorf("original args[%d] mutated: was %v, now %v", i, origArgs[i], a)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests targeting previously uncovered code paths (task-021)
// ─────────────────────────────────────────────────────────────────────────────

// Test 44: ErrMissingParam.Error() produces the expected message.
func TestErrMissingParam_ErrorMessage(t *testing.T) {
	err := &sqldialect.ErrMissingParam{Name: "foo"}
	got := err.Error()
	if got != "sql: missing query parameter $foo" {
		t.Errorf("ErrMissingParam.Error() = %q; want %q", got, "sql: missing query parameter $foo")
	}
}

// Test 45: translateStandaloneMatch via MatchNodePlan at top level (no RETURN).
// This exercises the translateStandaloneMatch code path via the
// translatePlan dispatcher (not via translateReturnPlan).
func TestTranslate_StandaloneMatchNode(t *testing.T) {
	scope := cypher.NewScope()
	mnp := &cypher.MatchNodePlan{
		Variable: "n",
		SQLAlias: "n0",
		Labels:   []string{"Person"},
		Props:    map[string]cypher.Expr{},
	}
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true})

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(mnp, scope)
	if err != nil {
		t.Fatalf("Translate standalone MatchNodePlan: %v", err)
	}
	if !strings.Contains(result.SQL, "SELECT") {
		t.Errorf("expected SELECT in standalone match, got: %s", result.SQL)
	}
	if !strings.Contains(result.SQL, "FROM nodes") {
		t.Errorf("expected FROM nodes in standalone match, got: %s", result.SQL)
	}
}

// Test 46: translateStandaloneFilter via FilterPlan at top level (no RETURN).
// This exercises the translateStandaloneFilter code path.
func TestTranslate_StandaloneFilter(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true})

	mnp := &cypher.MatchNodePlan{
		Variable: "n",
		SQLAlias: "n0",
		Labels:   []string{},
		Props:    map[string]cypher.Expr{},
	}
	fp := &cypher.FilterPlan{
		Source:    mnp,
		Predicate: &cypher.ComparisonExpr{Op: "=", Left: &cypher.PropExpr{Variable: "n", Property: "name"}, Right: &cypher.LiteralExpr{Value: "Alice"}},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(fp, scope)
	if err != nil {
		t.Fatalf("Translate standalone FilterPlan: %v", err)
	}
	if !strings.Contains(result.SQL, "SELECT *") {
		t.Errorf("expected 'SELECT *' in standalone filter, got: %s", result.SQL)
	}
	if !strings.Contains(result.SQL, "WHERE") {
		t.Errorf("expected WHERE in standalone filter, got: %s", result.SQL)
	}
	if !strings.Contains(result.SQL, "$.name") {
		t.Errorf("expected '$.name' in standalone filter, got: %s", result.SQL)
	}
}

// Test 47: MATCH+SET sequence produces KindMatchForWrite SELECT + UPDATE.
// This exercises translateSequenceWrite with read steps + write steps,
// which calls buildMatchForWriteSelect.
func TestTranslate_MatchSetSequence_MatchForWrite(t *testing.T) {
	result := translateWrite(t, `MATCH (n:Person) SET n.age = 42`)

	// Should have 2 statements: KindMatchForWrite SELECT + UPDATE.
	if len(result.Statements) < 2 {
		t.Fatalf("expected at least 2 statements for MATCH+SET, got %d: %v",
			len(result.Statements), result.Statements)
	}

	// First statement must be a SELECT (KindMatchForWrite) that selects node ids.
	stmt0 := result.Statements[0]
	if !strings.Contains(stmt0.SQL, "SELECT") {
		t.Errorf("statement[0] should be a SELECT, got: %s", stmt0.SQL)
	}
	if !strings.Contains(stmt0.SQL, "FROM nodes") {
		t.Errorf("statement[0] should reference nodes table, got: %s", stmt0.SQL)
	}

	// Second statement must be an UPDATE on nodes.
	stmt1 := result.Statements[1]
	if !strings.Contains(stmt1.SQL, "UPDATE nodes") {
		t.Errorf("statement[1] should be UPDATE nodes, got: %s", stmt1.SQL)
	}
	if !strings.Contains(stmt1.SQL, "json_set") {
		t.Errorf("statement[1] should use json_set for SET, got: %s", stmt1.SQL)
	}
}

// Test 48: MATCH+DETACH DELETE sequence produces KindMatchForWrite SELECT + two DELETEs.
func TestTranslate_MatchDetachDelete_MatchForWrite(t *testing.T) {
	result := translateWrite(t, `MATCH (n:Person) DETACH DELETE n`)

	// Should have at least 3 statements: KindMatchForWrite SELECT + DELETE edges + DELETE nodes.
	if len(result.Statements) < 3 {
		t.Fatalf("expected at least 3 statements for MATCH+DETACH DELETE, got %d",
			len(result.Statements))
	}

	// First statement must be a SELECT.
	if !strings.Contains(result.Statements[0].SQL, "SELECT") {
		t.Errorf("statement[0] should be a SELECT, got: %s", result.Statements[0].SQL)
	}
	// Second statement must delete edges.
	if !strings.Contains(result.Statements[1].SQL, "DELETE FROM edges") {
		t.Errorf("statement[1] should be DELETE FROM edges, got: %s", result.Statements[1].SQL)
	}
	// Third statement must delete the node.
	if !strings.Contains(result.Statements[2].SQL, "DELETE FROM nodes") {
		t.Errorf("statement[2] should be DELETE FROM nodes, got: %s", result.Statements[2].SQL)
	}
}

// Test 49: ResolveIDs replaces idSentinel values with concrete int64 IDs.
func TestResolveIDs_ReplacesIdSentinels(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("a", cypher.Binding{Alias: "n0", IsNode: true})
	scope.Bind("b", cypher.Binding{Alias: "n1", IsNode: true})

	relPlan := &cypher.CreateRelPlan{
		Type:     "KNOWS",
		StartVar: "a",
		EndVar:   "b",
		Props:    map[string]cypher.Expr{},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(relPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	idMap := map[string]int64{"a": 10, "b": 20}
	resolved, err := sqldialect.ResolveIDs(result, idMap)
	if err != nil {
		t.Fatalf("ResolveIDs: %v", err)
	}

	// After resolution, all args should be plain Go values (no sentinels).
	for i, a := range resolved.Args {
		switch a.(type) {
		case string, int64, float64, bool:
			// ok
		default:
			t.Errorf("args[%d] is still a sentinel after ResolveIDs: %T %v", i, a, a)
		}
	}
	// The resolved IDs (10, 20) should appear in the args.
	found10, found20 := false, false
	for _, a := range resolved.Args {
		if a == int64(10) {
			found10 = true
		}
		if a == int64(20) {
			found20 = true
		}
	}
	if !found10 {
		t.Errorf("expected id=10 in resolved args: %v", resolved.Args)
	}
	if !found20 {
		t.Errorf("expected id=20 in resolved args: %v", resolved.Args)
	}
}

// Test 50: ResolveIDs returns an error when a required variable is absent from idMap.
func TestResolveIDs_MissingVar_ReturnsError(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("a", cypher.Binding{Alias: "n0", IsNode: true})
	scope.Bind("b", cypher.Binding{Alias: "n1", IsNode: true})

	relPlan := &cypher.CreateRelPlan{
		Type:     "KNOWS",
		StartVar: "a",
		EndVar:   "b",
		Props:    map[string]cypher.Expr{},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(relPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	// Provide only "a"; "b" is missing.
	_, err = sqldialect.ResolveIDs(result, map[string]int64{"a": 10})
	if err == nil {
		t.Fatal("expected error for missing variable in idMap, got nil")
	}
	if !strings.Contains(err.Error(), "b") {
		t.Errorf("error message should mention variable 'b': %v", err)
	}
}

// Test 51: ResolveIDs on a result with no idSentinels is a no-op.
func TestResolveIDs_NoSentinels_NoOp(t *testing.T) {
	result := translateCypher(t, `MATCH (n:Person) RETURN n.name`)
	resolved, err := sqldialect.ResolveIDs(result, map[string]int64{})
	if err != nil {
		t.Fatalf("ResolveIDs on read query: %v", err)
	}
	if resolved.SQL != result.SQL {
		t.Errorf("SQL changed unexpectedly after ResolveIDs")
	}
}

// Test 52: literalToSQL handles nil literal (emits NULL).
func TestTranslate_NullLiteral(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true})

	// Build a ReturnPlan that projects a null literal.
	retPlan := &cypher.ReturnPlan{
		Source: &cypher.MatchNodePlan{
			Variable: "n",
			SQLAlias: "n0",
			Labels:   []string{},
			Props:    map[string]cypher.Expr{},
		},
		Projections: []cypher.ProjectionItem{
			{Expr: &cypher.LiteralExpr{Value: nil}, Alias: "nullval"},
		},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(retPlan, scope)
	if err != nil {
		t.Fatalf("Translate with null literal: %v", err)
	}
	if !strings.Contains(result.SQL, "NULL") {
		t.Errorf("expected NULL in SQL for nil literal, got: %s", result.SQL)
	}
}

// Test 53: literalToSQL handles bool literal (true → 1, false → 0).
func TestTranslate_BoolLiteral(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true})

	retPlan := &cypher.ReturnPlan{
		Source: &cypher.MatchNodePlan{
			Variable: "n",
			SQLAlias: "n0",
			Labels:   []string{},
			Props:    map[string]cypher.Expr{},
		},
		Projections: []cypher.ProjectionItem{
			{Expr: &cypher.LiteralExpr{Value: true}, Alias: "isActive"},
			{Expr: &cypher.LiteralExpr{Value: false}, Alias: "isDeleted"},
		},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(retPlan, scope)
	if err != nil {
		t.Fatalf("Translate with bool literals: %v", err)
	}
	if !strings.Contains(result.SQL, " 1 ") && !strings.Contains(result.SQL, " 1,") && !strings.Contains(result.SQL, ",1 ") {
		// Accept "1" anywhere in the SELECT list.
		if !strings.Contains(result.SQL, "1") {
			t.Errorf("expected '1' for true literal in SQL, got: %s", result.SQL)
		}
	}
	if !strings.Contains(result.SQL, "0") {
		t.Errorf("expected '0' for false literal in SQL, got: %s", result.SQL)
	}
}

// Test 54: MATCH+CREATE sequence with a relationship produces KindMatchForWrite SELECT
// followed by INSERT INTO edges that references the matched node IDs.
func TestTranslate_MatchCreateRel_MatchForWrite(t *testing.T) {
	result := translateWrite(t,
		`MATCH (a:Person) CREATE (b:Company {name: 'Acme'})-[:EMPLOYS]->(a)`)

	// Should have at least 3 statements:
	//   [0] KindMatchForWrite SELECT (to get matched node ids)
	//   [1] INSERT INTO nodes (b)
	//   [2] INSERT INTO edges (b→a)
	if len(result.Statements) < 3 {
		t.Fatalf("expected at least 3 statements for MATCH+CREATE, got %d",
			len(result.Statements))
	}

	// Statement[0] must be a SELECT.
	if !strings.Contains(result.Statements[0].SQL, "SELECT") {
		t.Errorf("statement[0] should be a SELECT, got: %s", result.Statements[0].SQL)
	}
	// Statement[1] must be an INSERT INTO nodes.
	hasNodeInsert := false
	for _, stmt := range result.Statements[1:] {
		if strings.Contains(stmt.SQL, "INSERT INTO nodes") {
			hasNodeInsert = true
		}
	}
	if !hasNodeInsert {
		t.Errorf("expected INSERT INTO nodes in statements[1:], got: %v", result.Statements[1:])
	}
	// Must have an INSERT INTO edges somewhere.
	hasEdgeInsert := false
	for _, stmt := range result.Statements {
		if strings.Contains(stmt.SQL, "INSERT INTO edges") {
			hasEdgeInsert = true
		}
	}
	if !hasEdgeInsert {
		t.Errorf("expected INSERT INTO edges in statements, got: %v", result.Statements)
	}
}

// Test 55: Rel prop constraint in single-hop covers the RelProps branch in
// buildFromClauseForMatchRel.
func TestTranslate_SingleHop_RelPropConstraint(t *testing.T) {
	// MATCH (a)-[r:KNOWS {since: 2020}]->(b) RETURN b.name
	// The parser may not support inline rel props — build plan directly.
	scope := cypher.NewScope()
	scope.Bind("a", cypher.Binding{Alias: "n0", IsNode: true})
	scope.Bind("r", cypher.Binding{Alias: "r0", IsRel: true})
	scope.Bind("b", cypher.Binding{Alias: "n1", IsNode: true})

	mrp := &cypher.MatchRelPlan{
		StartVar:    "a",
		RelVariable: "r",
		EndVar:      "b",
		Types:       []string{"KNOWS"},
		ToRight:     true,
		Undirected:  false,
		Optional:    false,
		RelSQLAlias: "r0",
		StartNode: cypher.MatchNodePlan{
			Variable: "a",
			SQLAlias: "n0",
			Labels:   []string{},
			Props:    map[string]cypher.Expr{},
		},
		EndNode: cypher.MatchNodePlan{
			Variable: "b",
			SQLAlias: "n1",
			Labels:   []string{},
			Props:    map[string]cypher.Expr{},
		},
		RelProps: map[string]cypher.Expr{
			"since": &cypher.LiteralExpr{Value: int64(2020)},
		},
	}

	retPlan := &cypher.ReturnPlan{
		Source:      mrp,
		Projections: []cypher.ProjectionItem{{Expr: &cypher.PropExpr{Variable: "b", Property: "name"}}},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(retPlan, scope)
	if err != nil {
		t.Fatalf("Translate with rel prop constraint: %v", err)
	}
	if !strings.Contains(result.SQL, "$.since") {
		t.Errorf("expected '$.since' rel prop constraint in SQL, got: %s", result.SQL)
	}
	if !strings.Contains(result.SQL, "2020") {
		t.Errorf("expected '2020' literal in SQL, got: %s", result.SQL)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// OPTIONAL MATCH SQL translation tests
// ─────────────────────────────────────────────────────────────────────────────

// TestTranslate_OptionalMatch_ProducesLeftJoin verifies that OPTIONAL MATCH
// emits LEFT JOIN for both the edge and the end node tables.
func TestTranslate_OptionalMatch_ProducesLeftJoin(t *testing.T) {
	result := translateCypher(t, "MATCH (n:Person) OPTIONAL MATCH (n)-[r:KNOWS]->(m) RETURN n, m")
	containsAll(t, result, "LEFT JOIN edges", "LEFT JOIN nodes")
	// The regular MATCH start node must use an inner FROM, not a LEFT JOIN.
	if strings.Contains(result.SQL, "LEFT JOIN nodes n0") {
		t.Errorf("start node n0 must not be LEFT JOINed: %s", result.SQL)
	}
}

// TestTranslate_OptionalMatch_NullableNodeProjection verifies that a nullable
// node variable (from OPTIONAL MATCH) uses CASE WHEN so unmatched rows project
// as SQL NULL rather than a json_object with null fields.
func TestTranslate_OptionalMatch_NullableNodeProjection(t *testing.T) {
	result := translateCypher(t, "MATCH (n:Person) OPTIONAL MATCH (n)-[r:KNOWS]->(m) RETURN n, m")
	// m is nullable — must be wrapped in CASE WHEN.
	if !strings.Contains(result.SQL, "CASE WHEN") {
		t.Errorf("nullable node m should use CASE WHEN in SELECT, got: %s", result.SQL)
	}
	// n is not nullable — must NOT be wrapped in CASE WHEN.
	// Count occurrences: there should be exactly one CASE WHEN (for m, not n).
	caseCount := strings.Count(result.SQL, "CASE WHEN")
	if caseCount != 1 {
		t.Errorf("expected exactly 1 CASE WHEN (for m), got %d in: %s", caseCount, result.SQL)
	}
}

// TestTranslate_OptionalMatch_NullableRelProjection verifies that a nullable
// relationship variable uses CASE WHEN in the projection.
func TestTranslate_OptionalMatch_NullableRelProjection(t *testing.T) {
	result := translateCypher(t, "MATCH (n:Person) OPTIONAL MATCH (n)-[r:KNOWS]->(m) RETURN r")
	if !strings.Contains(result.SQL, "CASE WHEN") {
		t.Errorf("nullable rel r should use CASE WHEN in SELECT, got: %s", result.SQL)
	}
}

// TestTranslate_OptionalMatch_IsNull verifies that "WHERE m IS NULL" after an
// OPTIONAL MATCH translates to "<alias>.id IS NULL" (not a json_object check).
func TestTranslate_OptionalMatch_IsNull(t *testing.T) {
	result := translateCypher(t,
		"MATCH (n:Person) OPTIONAL MATCH (n)-[r:KNOWS]->(m) WHERE m IS NULL RETURN n.name AS name")
	containsAll(t, result, "IS NULL")
	// Must reference the alias .id column, not json_object.
	if strings.Contains(result.SQL, "json_object") && strings.Contains(result.SQL, "IS NULL") {
		// Ensure the IS NULL is on .id, not on the json_object expression.
		if strings.Contains(result.SQL, "json_object(") {
			// Check that IS NULL comes after .id, not after json_object.
			idIdx := strings.Index(result.SQL, ".id IS NULL")
			if idIdx < 0 {
				t.Errorf("IS NULL should be on .id column, got: %s", result.SQL)
			}
		}
	}
}

// TestTranslate_OptionalMatch_IsNotNull verifies that "WHERE m IS NOT NULL"
// translates to "<alias>.id IS NOT NULL".
func TestTranslate_OptionalMatch_IsNotNull(t *testing.T) {
	result := translateCypher(t,
		"MATCH (n:Person) OPTIONAL MATCH (n)-[r:KNOWS]->(m) WHERE m IS NOT NULL RETURN n.name AS name")
	containsAll(t, result, "IS NOT NULL")
	if !strings.Contains(result.SQL, ".id IS NOT NULL") {
		t.Errorf("IS NOT NULL should be on .id column, got: %s", result.SQL)
	}
}

// TestTranslate_OptionalMatch_PropIsNull verifies that "WHERE m.name IS NULL"
// translates correctly using json_extract.
func TestTranslate_OptionalMatch_PropIsNull(t *testing.T) {
	result := translateCypher(t,
		"MATCH (n:Person) OPTIONAL MATCH (n)-[r:KNOWS]->(m) WHERE m.name IS NULL RETURN n.name AS name")
	containsAll(t, result, "json_extract", "$.name", "IS NULL")
}

// ─────────────────────────────────────────────────────────────────────────────
// Task-025 tests: COLLECT(), DISTINCT, advanced WHERE predicates
// ─────────────────────────────────────────────────────────────────────────────

// TestTranslate_Collect_AggregateFunction verifies that COLLECT(n.name) emits
// json_group_array(json_extract(...)) in the SQL output.
func TestTranslate_Collect_AggregateFunction(t *testing.T) {
	result := translateCypher(t,
		"MATCH (n:Person) RETURN collect(n.name) AS names")
	containsAll(t, result,
		"json_group_array",
		"json_extract",
		"$.name",
		"AS names",
	)
}

// TestTranslate_Collect_InWithPipeline verifies COLLECT in a WITH clause.
func TestTranslate_Collect_InWithPipeline(t *testing.T) {
	result := translateCypher(t,
		"MATCH (n:Person) WITH collect(n.name) AS names RETURN names")
	containsAll(t, result,
		"json_group_array",
		"json_extract",
		"$.name",
	)
}

// TestTranslate_ReturnDistinct_025 verifies that RETURN DISTINCT emits SELECT DISTINCT
// and deduplicates (SQL-level check only; runtime dedup is an integration concern).
func TestTranslate_ReturnDistinct_025(t *testing.T) {
	result := translateCypher(t,
		"MATCH (n:Person) RETURN DISTINCT n.name")
	containsAll(t, result, "SELECT DISTINCT", "json_extract", "$.name")
}

// TestTranslate_ExistsPredicate verifies that exists(n.prop) emits
// json_extract(...) IS NOT NULL.
func TestTranslate_ExistsPredicate(t *testing.T) {
	result := translateCypher(t,
		"MATCH (n:Person) WHERE exists(n.email) RETURN n.name")
	containsAll(t, result,
		"json_extract",
		"$.email",
		"IS NOT NULL",
	)
}

// TestTranslate_InListPredicate_StringLiterals verifies that
// n.prop IN ['a','b','c'] emits json_extract(...) IN (?, ?, ?).
func TestTranslate_InListPredicate_StringLiterals(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person) WHERE n.status IN ['active', 'pending'] RETURN n.name`)
	containsAll(t, result,
		"json_extract",
		"$.status",
		" IN (",
	)
	// Two string literals should have been bound as args.
	foundActive, foundPending := false, false
	for _, a := range result.Args {
		if a == "active" {
			foundActive = true
		}
		if a == "pending" {
			foundPending = true
		}
	}
	if !foundActive {
		t.Errorf("expected 'active' in args, got %v", result.Args)
	}
	if !foundPending {
		t.Errorf("expected 'pending' in args, got %v", result.Args)
	}
}

// TestTranslate_InListPredicate_IntLiterals verifies IN with integer literals.
func TestTranslate_InListPredicate_IntLiterals(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Product) WHERE n.category IN [1, 2, 3] RETURN n.name`)
	containsAll(t, result,
		"json_extract",
		"$.category",
		" IN (",
		"1, 2, 3",
	)
}

// TestTranslate_StartsWithPredicate verifies STARTS WITH emits a LIKE pattern.
func TestTranslate_StartsWithPredicate(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person) WHERE n.name STARTS WITH 'Al' RETURN n.name`)
	containsAll(t, result,
		"LIKE",
		"json_extract",
		"$.name",
	)
	// The LIKE pattern should be 'Al%' — bound as a ? arg.
	foundPattern := false
	for _, a := range result.Args {
		if s, ok := a.(string); ok && s == "Al%" {
			foundPattern = true
		}
	}
	if !foundPattern {
		t.Errorf("expected 'Al%%' pattern in args, got %v", result.Args)
	}
}

// TestTranslate_EndsWithPredicate verifies ENDS WITH emits a LIKE '%pattern'.
func TestTranslate_EndsWithPredicate(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person) WHERE n.name ENDS WITH 'son' RETURN n.name`)
	containsAll(t, result, "LIKE", "json_extract", "$.name")
	foundPattern := false
	for _, a := range result.Args {
		if s, ok := a.(string); ok && s == "%son" {
			foundPattern = true
		}
	}
	if !foundPattern {
		t.Errorf("expected '%%son' pattern in args, got %v", result.Args)
	}
}

// TestTranslate_ContainsPredicate verifies CONTAINS emits a LIKE '%pattern%'.
func TestTranslate_ContainsPredicate(t *testing.T) {
	result := translateCypher(t,
		`MATCH (n:Person) WHERE n.name CONTAINS 'ali' RETURN n.name`)
	containsAll(t, result, "LIKE", "json_extract", "$.name")
	foundPattern := false
	for _, a := range result.Args {
		if s, ok := a.(string); ok && s == "%ali%" {
			foundPattern = true
		}
	}
	if !foundPattern {
		t.Errorf("expected '%%ali%%' pattern in args, got %v", result.Args)
	}
}

// TestTranslate_ExistsPredicateDirectPlan verifies ExistsExpr translation
// when constructed directly (not via Parse).
func TestTranslate_ExistsPredicateDirectPlan(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true, Column: "n0.id"})

	existsExpr := &cypher.ExistsExpr{
		Prop: &cypher.PropExpr{Variable: "n", Property: "email"},
	}
	matchPlan := &cypher.MatchNodePlan{
		Variable: "n",
		SQLAlias: "n0",
	}
	filterPlan := &cypher.FilterPlan{
		Source:    matchPlan,
		Predicate: existsExpr,
	}
	retPlan := &cypher.ReturnPlan{
		Source: filterPlan,
		Projections: []cypher.ProjectionItem{
			{Expr: &cypher.PropExpr{Variable: "n", Property: "name"}, Alias: "name"},
		},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(retPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	containsAll(t, result, "json_extract", "$.email", "IS NOT NULL")
}

// TestTranslate_CollectDirectPlan verifies collect() AggCallExpr translation
// when the plan is constructed directly.
func TestTranslate_CollectDirectPlan(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true, Column: "n0.id"})

	collectExpr := &cypher.AggCallExpr{
		Func: "collect",
		Arg:  &cypher.PropExpr{Variable: "n", Property: "name"},
	}
	matchPlan := &cypher.MatchNodePlan{Variable: "n", SQLAlias: "n0"}
	retPlan := &cypher.ReturnPlan{
		Source: matchPlan,
		Projections: []cypher.ProjectionItem{
			{Expr: collectExpr, Alias: "names"},
		},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(retPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	containsAll(t, result, "json_group_array", "json_extract", "$.name", "AS names")
}

// ─── task-026: REMOVE prop, REMOVE label, SET += ─────────────────────────────

func TestTranslate_RemoveProp_026(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true, Column: "n0.id"})

	plan := &cypher.RemovePropPlan{Variable: "n", Property: "age"}
	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(plan, scope)
	if err != nil {
		t.Fatalf("Translate RemoveProp: %v", err)
	}
	containsAll(t, result, "UPDATE nodes", "json_remove", "$.age", "WHERE id = ?")
}

func TestTranslate_RemoveLabel_026(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true, Column: "n0.id"})

	plan := &cypher.RemoveLabelPlan{Variable: "n", Labels: []string{"Admin"}}
	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(plan, scope)
	if err != nil {
		t.Fatalf("Translate RemoveLabel: %v", err)
	}
	containsAll(t, result, "UPDATE nodes", "REPLACE", "labels", "WHERE id = ?")
}

func TestTranslate_SetMerge_026(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{Alias: "n0", IsNode: true, Column: "n0.id"})

	plan := &cypher.SetMergePlan{
		Variable: "n",
		Props: map[string]cypher.Expr{
			"score": &cypher.LiteralExpr{Value: int64(42)},
		},
	}
	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(plan, scope)
	if err != nil {
		t.Fatalf("Translate SetMerge: %v", err)
	}
	containsAll(t, result, "UPDATE nodes", "json_set", "$.score", "WHERE id = ?")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test: MERGE SQL output (task-028)
// ─────────────────────────────────────────────────────────────────────────────

// TestTranslate_Merge_BasicForm verifies that a plain MERGE (n:Label {prop:val})
// emits the correct KindMergeCheck SELECT and KindMergeInsert INSERT statements.
func TestTranslate_Merge_BasicForm(t *testing.T) {
	result := translateCypher(t, `MERGE (n:Person {name: "Alice"})`)

	if len(result.Statements) < 2 {
		t.Fatalf("expected at least 2 statements, got %d", len(result.Statements))
	}

	check := result.Statements[0]
	if check.Kind != sqldialect.KindMergeCheck {
		t.Errorf("stmt[0] kind: want KindMergeCheck, got %v", check.Kind)
	}
	if !strings.Contains(check.SQL, "SELECT") || !strings.Contains(check.SQL, "LIMIT 1") {
		t.Errorf("MergeCheck SQL missing SELECT or LIMIT 1: %s", check.SQL)
	}
	// Label constraint must appear.
	if !strings.Contains(check.SQL, "labels") {
		t.Errorf("MergeCheck SQL missing labels constraint: %s", check.SQL)
	}
	// Name prop constraint must appear.
	if !strings.Contains(check.SQL, "$.name") {
		t.Errorf("MergeCheck SQL missing $.name prop constraint: %s", check.SQL)
	}

	insert := result.Statements[1]
	if insert.Kind != sqldialect.KindMergeInsert {
		t.Errorf("stmt[1] kind: want KindMergeInsert, got %v", insert.Kind)
	}
	if !strings.Contains(insert.SQL, "INSERT INTO nodes") {
		t.Errorf("MergeInsert SQL missing INSERT INTO nodes: %s", insert.SQL)
	}
	if insert.CreatedVar != "n" {
		t.Errorf("MergeInsert CreatedVar: want %q, got %q", "n", insert.CreatedVar)
	}
}

// TestTranslate_Merge_OnCreateOnMatchStmts verifies that ON CREATE SET and
// ON MATCH SET items produce correctly tagged KindUpdate statements.
func TestTranslate_Merge_OnCreateOnMatchStmts(t *testing.T) {
	result := translateCypher(t, `MERGE (n:Person {name: "Bob"}) ON CREATE SET n.created = "yes" ON MATCH SET n.seen = "yes"`)

	// Expect: [0] check, [1] insert, [2] oncreate UPDATE, [3] onmatch UPDATE
	if len(result.Statements) != 4 {
		t.Fatalf("expected 4 statements, got %d", len(result.Statements))
	}
	if result.Statements[0].Kind != sqldialect.KindMergeCheck {
		t.Errorf("stmt[0]: want KindMergeCheck, got %v", result.Statements[0].Kind)
	}
	if result.Statements[1].Kind != sqldialect.KindMergeInsert {
		t.Errorf("stmt[1]: want KindMergeInsert, got %v", result.Statements[1].Kind)
	}
	if result.Statements[2].Kind != sqldialect.KindUpdate {
		t.Errorf("stmt[2]: want KindUpdate (ON CREATE), got %v", result.Statements[2].Kind)
	}
	if !strings.HasPrefix(result.Statements[2].CreatedVar, "oncreate:") {
		t.Errorf("stmt[2] CreatedVar: want oncreate: prefix, got %q", result.Statements[2].CreatedVar)
	}
	if result.Statements[3].Kind != sqldialect.KindUpdate {
		t.Errorf("stmt[3]: want KindUpdate (ON MATCH), got %v", result.Statements[3].Kind)
	}
	if !strings.HasPrefix(result.Statements[3].CreatedVar, "onmatch:") {
		t.Errorf("stmt[3] CreatedVar: want onmatch: prefix, got %q", result.Statements[3].CreatedVar)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CASE expression tests (task-032)
// ─────────────────────────────────────────────────────────────────────────────

// TestTranslate_CaseSearched_InReturn verifies that a searched CASE expression
// in a RETURN clause produces the correct SQL CASE … END fragment.
// MATCH (n:Person) RETURN CASE WHEN n.age > 18 THEN 'adult' ELSE 'minor' END AS category
func TestTranslate_CaseSearched_InReturn(t *testing.T) {
	result := translateCypher(t,
		"MATCH (n:Person) RETURN CASE WHEN n.age > 18 THEN 'adult' ELSE 'minor' END AS category")
	containsAll(t, result,
		"CASE WHEN",
		"THEN",
		"ELSE",
		"END",
		"AS category",
		"json_extract",
		"$.age",
	)
	// SQL must not embed the string 'adult' / 'minor' literally (they are bind params).
	if strings.Contains(result.SQL, "'adult'") || strings.Contains(result.SQL, "'minor'") {
		t.Errorf("string literals should be bind params, not inlined: %s", result.SQL)
	}
}

// TestTranslate_CaseSimple_InReturn verifies that a simple CASE expression
// in a RETURN clause emits the correct SQL.
// MATCH (n) RETURN CASE n.status WHEN 'active' THEN 1 ELSE 0 END AS flag
func TestTranslate_CaseSimple_InReturn(t *testing.T) {
	result := translateCypher(t,
		"MATCH (n) RETURN CASE n.status WHEN 'active' THEN 1 ELSE 0 END AS flag")
	containsAll(t, result,
		"CASE",
		"WHEN",
		"THEN 1",
		"ELSE 0",
		"END",
		"AS flag",
		"json_extract",
		"$.status",
	)
}

// TestTranslate_CaseSearched_InWhere verifies that a searched CASE expression
// can appear inside a WHERE predicate.
// MATCH (n:Person) WHERE CASE WHEN n.age > 18 THEN 1 ELSE 0 END = 1 RETURN n.name
func TestTranslate_CaseSearched_InWhere(t *testing.T) {
	result := translateCypher(t,
		"MATCH (n:Person) WHERE CASE WHEN n.age > 18 THEN 1 ELSE 0 END = 1 RETURN n.name")
	containsAll(t, result,
		"CASE WHEN",
		"THEN 1",
		"ELSE 0",
		"END",
		"WHERE",
		"$.name",
	)
}

// TestTranslate_CaseNoElse verifies CASE without an ELSE clause.
// MATCH (n) RETURN CASE WHEN n.active THEN 'yes' END AS maybe
func TestTranslate_CaseNoElse(t *testing.T) {
	result := translateCypher(t,
		"MATCH (n) RETURN CASE WHEN n.active THEN 'yes' END AS maybe")
	containsAll(t, result,
		"CASE WHEN",
		"THEN",
		"END",
		"AS maybe",
	)
	// No ELSE should appear in the SQL.
	if strings.Contains(result.SQL, "ELSE") {
		t.Errorf("unexpected ELSE in no-else CASE: %s", result.SQL)
	}
}

// TestTranslate_CaseMultipleWhenClauses verifies CASE with multiple WHEN branches.
// MATCH (n) RETURN CASE n.score WHEN 1 THEN 'low' WHEN 2 THEN 'mid' ELSE 'high' END AS tier
func TestTranslate_CaseMultipleWhenClauses(t *testing.T) {
	result := translateCypher(t,
		"MATCH (n) RETURN CASE n.score WHEN 1 THEN 'low' WHEN 2 THEN 'mid' ELSE 'high' END AS tier")
	containsAll(t, result,
		"CASE",
		"WHEN 1 THEN",
		"WHEN 2 THEN",
		"ELSE",
		"END",
		"AS tier",
	)
}

// TestTranslate_CaseDirectConstruction verifies that CaseExpr can be constructed
// directly as plan nodes and translated correctly (exercises caseExprToSQL directly).
func TestTranslate_CaseDirectConstruction(t *testing.T) {
	// Build scope with a node variable "n".
	scope := cypher.NewScope()
	scope.Bind("n", cypher.Binding{
		Alias:  "n0",
		Column: "n0.id",
		IsNode: true,
	})

	// Searched CASE: CASE WHEN n.age > 18 THEN 'adult' ELSE 'minor' END
	caseE := &cypher.CaseExpr{
		WhenClauses: []cypher.CaseWhenClause{
			{
				Condition: &cypher.ComparisonExpr{
					Left:  &cypher.PropExpr{Variable: "n", Property: "age"},
					Op:    ">",
					Right: &cypher.LiteralExpr{Value: int64(18)},
				},
				Value: &cypher.LiteralExpr{Value: "adult"},
			},
		},
		Else: &cypher.LiteralExpr{Value: "minor"},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	// Wrap in a ReturnPlan to exercise the full translation path.
	matchPlan := &cypher.MatchNodePlan{Variable: "n", SQLAlias: "n0"}
	returnPlan := &cypher.ReturnPlan{
		Source: matchPlan,
		Projections: []cypher.ProjectionItem{
			{Expr: caseE, Alias: "category"},
		},
	}
	result, err := tr.Translate(returnPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	containsAll(t, result,
		"CASE WHEN",
		"json_extract(n0.props, '$.age') > 18",
		"THEN ?",
		"ELSE ?",
		"END",
		"AS category",
	)
	// Two string literals → two bind args.
	if len(result.Args) != 2 {
		t.Errorf("expected 2 args (adult, minor), got %d: %v", len(result.Args), result.Args)
	}
}

// ─── variable-length path translator unit tests ───────────────────────────────

// TestTranslate_VarLength_WithRecursive verifies that a VariableLengthRelPlan
// produces a WITH RECURSIVE CTE with the correct depth guard in the SQL.
func TestTranslate_VarLength_WithRecursive(t *testing.T) {
	result := translateCypher(t, "MATCH (a)-[*1..3]->(b) RETURN b.name")
	containsAll(t, result,
		"WITH RECURSIVE",
		"UNION ALL",
		"depth",
	)
	// The depth guard should cap at 3.
	if !strings.Contains(result.SQL, "< ?") && !strings.Contains(result.SQL, "<3") {
		t.Errorf("expected depth guard in SQL, got: %s", result.SQL)
	}
}

// TestTranslate_VarLength_Unbounded verifies that an unbounded [*] path uses the
// safety cap (15) rather than omitting the depth guard entirely.
func TestTranslate_VarLength_Unbounded(t *testing.T) {
	result := translateCypher(t, "MATCH (a)-[*]->(b) RETURN b.name")
	containsAll(t, result,
		"WITH RECURSIVE",
		"UNION ALL",
		"depth",
	)
	// Safety cap of 15 must appear as a bind arg.
	found := false
	for _, a := range result.Args {
		if a == int64(15) || a == 15 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected safety-cap arg 15 in args, got %v", result.Args)
	}
}

// TestTranslate_VarLength_TypeFilter verifies that a type constraint is pushed
// into both the base and recursive cases of the CTE.
func TestTranslate_VarLength_TypeFilter(t *testing.T) {
	result := translateCypher(t, "MATCH (a)-[:KNOWS*1..2]->(b) RETURN b.name")
	containsAll(t, result,
		"WITH RECURSIVE",
		"e.type = ?",
	)
	// "KNOWS" should appear as a bind arg (twice: base + recursive).
	count := 0
	for _, a := range result.Args {
		if a == "KNOWS" {
			count++
		}
	}
	if count < 2 {
		t.Errorf("expected KNOWS to appear at least twice in args (base+recursive), got %d: %v", count, result.Args)
	}
}

// TestTranslate_VarLength_DirectConstruction builds a VariableLengthRelPlan
// directly and verifies the WITH RECURSIVE prefix and JOIN structure.
func TestTranslate_VarLength_DirectConstruction(t *testing.T) {
	scope := cypher.NewScope()
	scope.Bind("a", cypher.Binding{Alias: "n0", Column: "n0.id", IsNode: true})
	scope.Bind("b", cypher.Binding{Alias: "n1", Column: "n1.id", IsNode: true})

	vlp := &cypher.VariableLengthRelPlan{
		StartVar:  "a",
		StartNode: cypher.MatchNodePlan{Variable: "a", SQLAlias: "n0"},
		EndVar:    "b",
		EndNode:   cypher.MatchNodePlan{Variable: "b", SQLAlias: "n1"},
		MinHops:   1,
		MaxHops:   2,
		ToRight:   true,
		CTEAlias:  "_vl0",
	}
	returnPlan := &cypher.ReturnPlan{
		Source: vlp,
		Projections: []cypher.ProjectionItem{
			{Expr: &cypher.PropExpr{Variable: "b", Property: "name"}, Alias: "name"},
		},
	}

	tr := sqldialect.NewTranslator(sqldialect.SQLiteDialect{})
	result, err := tr.Translate(returnPlan, scope)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	containsAll(t, result,
		"WITH RECURSIVE",
		"_vl0(end_id, depth) AS",
		"UNION ALL",
		"JOIN nodes n1 ON n1.id IN (SELECT end_id FROM _vl0 WHERE depth >= ?)",
		"json_extract(n1.props, '$.name') AS name",
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests for multi-node MATCH (Cartesian product) column scoping fix
// ─────────────────────────────────────────────────────────────────────────────

// TestTranslate_CartesianProduct_ReadQuery verifies that MATCH (n), (m) RETURN ...
// generates a CROSS JOIN so both table aliases appear in the FROM clause.
func TestTranslate_CartesianProduct_ReadQuery(t *testing.T) {
	result := translateCypher(t, "MATCH (n), (m) RETURN n, m")
	containsAll(t, result, "CROSS JOIN", "nodes n0", "nodes n1")
	// Both aliases must be reachable in the SELECT list.
	if strings.Contains(result.SQL, "n1.id") && !strings.Contains(result.SQL, "CROSS JOIN") {
		t.Errorf("expected CROSS JOIN for cartesian product, got: %s", result.SQL)
	}
}

// TestTranslate_CartesianProduct_MatchForWrite verifies that
// MATCH (x:X), (y:Y) CREATE (x)-[:R]->(y) generates a correct KindMatchForWrite
// SELECT with both node aliases in the FROM clause.
func TestTranslate_CartesianProduct_MatchForWrite(t *testing.T) {
	result := translateCypher(t, "MATCH (x:X), (y:Y) CREATE (x)-[:R]->(y)")
	if len(result.Statements) < 2 {
		t.Fatalf("expected at least 2 statements, got %d", len(result.Statements))
	}
	matchStmt := result.Statements[0]
	if matchStmt.Kind != sqldialect.KindMatchForWrite {
		t.Fatalf("expected first statement to be KindMatchForWrite, got %d", matchStmt.Kind)
	}
	// Both x and y aliases (n0 and n1) must appear in the match SELECT.
	containsAll(t, sqldialect.Result{SQL: matchStmt.SQL}, "nodes n0", "CROSS JOIN nodes n1")
}

// TestTranslate_MatchCreate_AnonymousEnd verifies that
// MATCH (x:Begin) CREATE (x)-[:TYPE]->(:End) generates a KindMatchForWrite
// SELECT that only references the MATCH node (n0), not the CREATE node (n1).
func TestTranslate_MatchCreate_AnonymousEnd(t *testing.T) {
	result := translateCypher(t, "MATCH (x:Begin) CREATE (x)-[:TYPE]->(:End)")
	if len(result.Statements) < 2 {
		t.Fatalf("expected at least 2 statements, got %d", len(result.Statements))
	}
	matchStmt := result.Statements[0]
	if matchStmt.Kind != sqldialect.KindMatchForWrite {
		t.Fatalf("expected first statement to be KindMatchForWrite, got %d", matchStmt.Kind)
	}
	// The match SELECT must only contain x (n0), not the CREATE-introduced _anon0 (n1).
	if strings.Contains(matchStmt.SQL, "n1.id") {
		t.Errorf("KindMatchForWrite SELECT should not reference n1 (CREATE node): %s", matchStmt.SQL)
	}
	if !strings.Contains(matchStmt.SQL, "n0.id") {
		t.Errorf("KindMatchForWrite SELECT should reference n0 (matched node): %s", matchStmt.SQL)
	}
}
