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
