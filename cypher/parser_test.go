package cypher_test

import (
	"testing"

	"github.com/LackOfMorals/graphlite/cypher"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func mustParse(t *testing.T, input string) *cypher.Query {
	t.Helper()
	q, err := cypher.Parse(input)
	if err != nil {
		t.Fatalf("Parse(%q): unexpected error: %v", input, err)
	}
	if q == nil {
		t.Fatalf("Parse(%q): returned nil Query", input)
	}
	return q
}

func getMatch(t *testing.T, q *cypher.Query, idx int) *cypher.MatchClause {
	t.Helper()
	if idx >= len(q.Clauses) {
		t.Fatalf("clause index %d out of range (len=%d)", idx, len(q.Clauses))
	}
	m, ok := q.Clauses[idx].(*cypher.MatchClause)
	if !ok {
		t.Fatalf("clause[%d] is %T, want *MatchClause", idx, q.Clauses[idx])
	}
	return m
}

func getReturn(t *testing.T, q *cypher.Query, idx int) *cypher.ReturnClause {
	t.Helper()
	if idx >= len(q.Clauses) {
		t.Fatalf("clause index %d out of range (len=%d)", idx, len(q.Clauses))
	}
	rc, ok := q.Clauses[idx].(*cypher.ReturnClause)
	if !ok {
		t.Fatalf("clause[%d] is %T, want *ReturnClause", idx, q.Clauses[idx])
	}
	return rc
}

func getCreate(t *testing.T, q *cypher.Query, idx int) *cypher.CreateClause {
	t.Helper()
	if idx >= len(q.Clauses) {
		t.Fatalf("clause index %d out of range (len=%d)", idx, len(q.Clauses))
	}
	cc, ok := q.Clauses[idx].(*cypher.CreateClause)
	if !ok {
		t.Fatalf("clause[%d] is %T, want *CreateClause", idx, q.Clauses[idx])
	}
	return cc
}

func getSet(t *testing.T, q *cypher.Query, idx int) *cypher.SetClause {
	t.Helper()
	if idx >= len(q.Clauses) {
		t.Fatalf("clause index %d out of range (len=%d)", idx, len(q.Clauses))
	}
	sc, ok := q.Clauses[idx].(*cypher.SetClause)
	if !ok {
		t.Fatalf("clause[%d] is %T, want *SetClause", idx, q.Clauses[idx])
	}
	return sc
}

func getDelete(t *testing.T, q *cypher.Query, idx int) *cypher.DeleteClause {
	t.Helper()
	if idx >= len(q.Clauses) {
		t.Fatalf("clause index %d out of range (len=%d)", idx, len(q.Clauses))
	}
	dc, ok := q.Clauses[idx].(*cypher.DeleteClause)
	if !ok {
		t.Fatalf("clause[%d] is %T, want *DeleteClause", idx, q.Clauses[idx])
	}
	return dc
}

// ─── test 01: MATCH (n) — all nodes ──────────────────────────────────────────

func TestParse_MatchAllNodes(t *testing.T) {
	q := mustParse(t, "MATCH (n) RETURN n")

	m := getMatch(t, q, 0)
	if m.Optional {
		t.Error("expected non-optional MATCH")
	}
	if len(m.Pattern) != 1 {
		t.Fatalf("expected 1 pattern part, got %d", len(m.Pattern))
	}
	pp := m.Pattern[0]
	if pp.Start.Variable != "n" {
		t.Errorf("start variable: got %q, want %q", pp.Start.Variable, "n")
	}
	if len(pp.Start.Labels) != 0 {
		t.Errorf("expected no labels, got %v", pp.Start.Labels)
	}
	if len(pp.Chain) != 0 {
		t.Errorf("expected no chain hops, got %d", len(pp.Chain))
	}

	ret := getReturn(t, q, 1)
	if len(ret.Items) != 1 {
		t.Fatalf("expected 1 return item, got %d", len(ret.Items))
	}
}

// ─── test 02: MATCH (n:Label) — by label ─────────────────────────────────────

func TestParse_MatchByLabel(t *testing.T) {
	q := mustParse(t, "MATCH (n:Person) RETURN n.name")

	m := getMatch(t, q, 0)
	pp := m.Pattern[0]

	if pp.Start.Variable != "n" {
		t.Errorf("start variable: got %q, want %q", pp.Start.Variable, "n")
	}
	if len(pp.Start.Labels) != 1 || pp.Start.Labels[0] != "Person" {
		t.Errorf("labels: got %v, want [Person]", pp.Start.Labels)
	}
}

// ─── test 03: MATCH (n:Label {prop: val}) — by label + property ──────────────

func TestParse_MatchByLabelAndProperty(t *testing.T) {
	q := mustParse(t, `MATCH (n:Person {name: 'Alice'}) RETURN n`)

	m := getMatch(t, q, 0)
	pp := m.Pattern[0]

	if len(pp.Start.Labels) != 1 || pp.Start.Labels[0] != "Person" {
		t.Errorf("labels: got %v", pp.Start.Labels)
	}
	if pp.Start.Props["name"] == "" {
		t.Errorf("expected Props[name] to be set, got %v", pp.Start.Props)
	}
}

// ─── test 04: MATCH (n:L1:L2) — multi-label AND semantics ───────────────────

func TestParse_MatchMultiLabel(t *testing.T) {
	q := mustParse(t, "MATCH (n:Person:Employee) RETURN n")

	m := getMatch(t, q, 0)
	pp := m.Pattern[0]

	if len(pp.Start.Labels) != 2 {
		t.Fatalf("expected 2 labels, got %d: %v", len(pp.Start.Labels), pp.Start.Labels)
	}
	labels := map[string]bool{pp.Start.Labels[0]: true, pp.Start.Labels[1]: true}
	if !labels["Person"] || !labels["Employee"] {
		t.Errorf("expected labels Person and Employee, got %v", pp.Start.Labels)
	}
}

// ─── test 05: MATCH (a)-[r:TYPE]->(b) — directed single-hop ─────────────────

func TestParse_MatchDirectedSingleHop(t *testing.T) {
	q := mustParse(t, "MATCH (a:Person)-[r:KNOWS]->(b:Person) RETURN a, b")

	m := getMatch(t, q, 0)
	if len(m.Pattern) != 1 {
		t.Fatalf("expected 1 pattern part, got %d", len(m.Pattern))
	}
	pp := m.Pattern[0]

	if pp.Start.Variable != "a" {
		t.Errorf("start variable: got %q, want %q", pp.Start.Variable, "a")
	}
	if len(pp.Chain) != 1 {
		t.Fatalf("expected 1 hop, got %d", len(pp.Chain))
	}

	hop := pp.Chain[0]
	if hop.Rel.Variable != "r" {
		t.Errorf("rel variable: got %q, want %q", hop.Rel.Variable, "r")
	}
	if len(hop.Rel.Types) != 1 || hop.Rel.Types[0] != "KNOWS" {
		t.Errorf("rel types: got %v, want [KNOWS]", hop.Rel.Types)
	}
	if !hop.Rel.ToRight {
		t.Error("expected directed right (ToRight=true)")
	}
	if hop.Rel.ToLeft {
		t.Error("expected ToLeft=false for -[r]->> direction")
	}
	if hop.Node.Variable != "b" {
		t.Errorf("end node variable: got %q, want %q", hop.Node.Variable, "b")
	}
}

// ─── test 06: MATCH (a)-[r:TYPE]-(b) — undirected single-hop ────────────────

func TestParse_MatchUndirectedSingleHop(t *testing.T) {
	q := mustParse(t, "MATCH (a)-[r:KNOWS]-(b) RETURN a, b")

	m := getMatch(t, q, 0)
	pp := m.Pattern[0]
	hop := pp.Chain[0]

	if hop.Rel.ToLeft {
		t.Error("expected ToLeft=false for undirected")
	}
	if hop.Rel.ToRight {
		t.Error("expected ToRight=false for undirected")
	}
}

// ─── test 07: multi-hop chain (3 hops) ───────────────────────────────────────

func TestParse_MatchThreeHops(t *testing.T) {
	q := mustParse(t, "MATCH (a)-[:KNOWS]->(b)-[:LIKES]->(c)-[:OWNS]->(d) RETURN d")

	m := getMatch(t, q, 0)
	pp := m.Pattern[0]

	if len(pp.Chain) != 3 {
		t.Fatalf("expected 3 hops, got %d", len(pp.Chain))
	}
	if pp.Chain[0].Rel.Types[0] != "KNOWS" {
		t.Errorf("hop0 type: got %q, want KNOWS", pp.Chain[0].Rel.Types[0])
	}
	if pp.Chain[1].Rel.Types[0] != "LIKES" {
		t.Errorf("hop1 type: got %q, want LIKES", pp.Chain[1].Rel.Types[0])
	}
	if pp.Chain[2].Rel.Types[0] != "OWNS" {
		t.Errorf("hop2 type: got %q, want OWNS", pp.Chain[2].Rel.Types[0])
	}
}

// ─── test 08: WHERE clause produces a typed predicate tree ───────────────────

func TestParse_WhereClauseTypedTree(t *testing.T) {
	q := mustParse(t, "MATCH (n:Person) WHERE n.age > 18 AND n.active = true RETURN n")

	m := getMatch(t, q, 0)
	if m.Where == nil {
		t.Error("expected non-nil Where predicate tree")
	}
	// Expect AND at the root: (n.age > 18) AND (n.active = true)
	boolExpr, ok := m.Where.(*cypher.BoolExpr)
	if !ok {
		t.Fatalf("expected *BoolExpr at root, got %T", m.Where)
	}
	if boolExpr.Op != "AND" {
		t.Errorf("expected AND, got %q", boolExpr.Op)
	}
}

// ─── test 09: RETURN with alias, ORDER BY, SKIP, LIMIT ───────────────────────
// Note: this grammar requires SKIP before LIMIT (unlike some other Cypher dialects).

func TestParse_ReturnWithAliasOrderByLimitSkip(t *testing.T) {
	q := mustParse(t, "MATCH (n:Person) RETURN n.name AS name, n.age AS age ORDER BY age DESC SKIP 5 LIMIT 10")

	ret := getReturn(t, q, 1)

	if len(ret.Items) != 2 {
		t.Fatalf("expected 2 return items, got %d", len(ret.Items))
	}
	if ret.Items[0].Alias != "name" {
		t.Errorf("item[0] alias: got %q, want %q", ret.Items[0].Alias, "name")
	}
	if ret.Items[1].Alias != "age" {
		t.Errorf("item[1] alias: got %q, want %q", ret.Items[1].Alias, "age")
	}

	if len(ret.OrderBy) != 1 {
		t.Fatalf("expected 1 sort item, got %d", len(ret.OrderBy))
	}
	if !ret.OrderBy[0].Descending {
		t.Error("expected DESC ordering")
	}

	if ret.Limit == nil || *ret.Limit != 10 {
		t.Errorf("LIMIT: got %v, want 10", ret.Limit)
	}
	if ret.Skip == nil || *ret.Skip != 5 {
		t.Errorf("SKIP: got %v, want 5", ret.Skip)
	}
}

// ─── test 10: CREATE node ──────────────────────────────────────────────────

func TestParse_CreateNode(t *testing.T) {
	q := mustParse(t, `CREATE (n:Person {name: 'Alice', age: 30})`)

	cc := getCreate(t, q, 0)
	if len(cc.Pattern) != 1 {
		t.Fatalf("expected 1 pattern part, got %d", len(cc.Pattern))
	}
	pp := cc.Pattern[0]

	if len(pp.Start.Labels) != 1 || pp.Start.Labels[0] != "Person" {
		t.Errorf("labels: got %v, want [Person]", pp.Start.Labels)
	}
	if pp.Start.Props["name"] == "" {
		t.Errorf("expected Props[name] to be set, got %v", pp.Start.Props)
	}
	if pp.Start.Props["age"] == "" {
		t.Errorf("expected Props[age] to be set, got %v", pp.Start.Props)
	}
}

// ─── test 11: CREATE relationship ─────────────────────────────────────────────
// Note: MATCH (a:Person), (b:Person) is a single MATCH clause with 2 pattern parts.
// So the CREATE clause is at index 1 (not 2).

func TestParse_CreateRelationship(t *testing.T) {
	q := mustParse(t, "MATCH (a:Person), (b:Person) CREATE (a)-[:KNOWS]->(b)")

	cc := getCreate(t, q, 1)
	if len(cc.Pattern) != 1 {
		t.Fatalf("expected 1 pattern part, got %d", len(cc.Pattern))
	}
	pp := cc.Pattern[0]

	if len(pp.Chain) != 1 {
		t.Fatalf("expected 1 hop in CREATE pattern, got %d", len(pp.Chain))
	}
	hop := pp.Chain[0]
	if len(hop.Rel.Types) != 1 || hop.Rel.Types[0] != "KNOWS" {
		t.Errorf("rel type: got %v, want [KNOWS]", hop.Rel.Types)
	}
	if !hop.Rel.ToRight {
		t.Error("expected directed right")
	}
}

// ─── test 12: SET clause ──────────────────────────────────────────────────────

func TestParse_SetClause(t *testing.T) {
	q := mustParse(t, "MATCH (n:Person) SET n.age = 31")

	sc := getSet(t, q, 1)
	if len(sc.Items) != 1 {
		t.Fatalf("expected 1 set item, got %d", len(sc.Items))
	}
	item := sc.Items[0]
	if item.Variable != "n" {
		t.Errorf("variable: got %q, want %q", item.Variable, "n")
	}
	if item.Property != "age" {
		t.Errorf("property: got %q, want %q", item.Property, "age")
	}
	if item.ExprText == "" {
		t.Error("expected non-empty ExprText")
	}
}

// ─── test 13: SET with $param ─────────────────────────────────────────────────

func TestParse_SetWithParam(t *testing.T) {
	q := mustParse(t, "MATCH (n:Person) SET n.name = $newName")

	sc := getSet(t, q, 1)
	item := sc.Items[0]
	if item.Property != "name" {
		t.Errorf("property: got %q, want %q", item.Property, "name")
	}
	// ExprText should contain the parameter reference.
	if item.ExprText == "" {
		t.Error("expected non-empty ExprText for $param")
	}
}

// ─── test 14: DELETE clause ────────────────────────────────────────────────────

func TestParse_DeleteNode(t *testing.T) {
	q := mustParse(t, "MATCH (n:Person) DELETE n")

	dc := getDelete(t, q, 1)
	if dc.Detach {
		t.Error("expected Detach=false for DELETE")
	}
	if len(dc.Exprs) != 1 {
		t.Fatalf("expected 1 delete expr, got %d", len(dc.Exprs))
	}
	if dc.Exprs[0] != "n" {
		t.Errorf("delete expr: got %q, want %q", dc.Exprs[0], "n")
	}
}

// ─── test 15: DETACH DELETE ───────────────────────────────────────────────────

func TestParse_DetachDelete(t *testing.T) {
	q := mustParse(t, "MATCH (n:Person) DETACH DELETE n")

	dc := getDelete(t, q, 1)
	if !dc.Detach {
		t.Error("expected Detach=true for DETACH DELETE")
	}
}

// ─── test 16: $param in WHERE produces ParamRef nodes ────────────────────────

func TestParse_ParamInWhere(t *testing.T) {
	q := mustParse(t, "MATCH (n:Person) WHERE n.name = $name AND n.age > $minAge RETURN n")

	m := getMatch(t, q, 0)
	if m.Where == nil {
		t.Error("expected non-nil Where predicate tree for WHERE with $params")
	}
	// Root should be AND.
	boolExpr, ok := m.Where.(*cypher.BoolExpr)
	if !ok {
		t.Fatalf("expected *BoolExpr at root, got %T", m.Where)
	}
	if boolExpr.Op != "AND" {
		t.Errorf("expected AND, got %q", boolExpr.Op)
	}
	// Left side: n.name = $name → ComparisonExpr with RHS = ParamRef
	cmp, ok := boolExpr.Left.(*cypher.ComparisonExpr)
	if !ok {
		t.Fatalf("left of AND: expected *ComparisonExpr, got %T", boolExpr.Left)
	}
	if cmp.Op != "=" {
		t.Errorf("expected = operator, got %q", cmp.Op)
	}
	if _, ok := cmp.Right.(*cypher.ParamRef); !ok {
		t.Errorf("right of = expected *ParamRef, got %T", cmp.Right)
	}
}

// ─── test 17: RETURN DISTINCT ────────────────────────────────────────────────

func TestParse_ReturnDistinct(t *testing.T) {
	q := mustParse(t, "MATCH (n:Person) RETURN DISTINCT n.name")

	ret := getReturn(t, q, 1)
	if !ret.Distinct {
		t.Error("expected Distinct=true")
	}
}

// ─── test 18: OPTIONAL MATCH ─────────────────────────────────────────────────

func TestParse_OptionalMatch(t *testing.T) {
	q := mustParse(t, "MATCH (n:Person) OPTIONAL MATCH (n)-[r:KNOWS]->(m) RETURN n, m")

	m0 := getMatch(t, q, 0)
	if m0.Optional {
		t.Error("first MATCH should not be optional")
	}

	m1 := getMatch(t, q, 1)
	if !m1.Optional {
		t.Error("second MATCH should be optional")
	}
}

// ─── test 19: UNION is rejected ───────────────────────────────────────────────

func TestParse_UnionRejected(t *testing.T) {
	_, err := cypher.Parse("MATCH (n) RETURN n UNION MATCH (m) RETURN m")
	if err == nil {
		t.Error("expected error for UNION query, got nil")
	}
}

// ─── test 20: syntax error ────────────────────────────────────────────────────

func TestParse_SyntaxError(t *testing.T) {
	_, err := cypher.Parse("MATCH (n RETURN n")
	if err == nil {
		t.Error("expected error for invalid Cypher syntax, got nil")
	}
}

// ─── test 21: WHERE OR/NOT combinators produce typed nodes ───────────────────

func TestParse_WhereOrNotCombinators(t *testing.T) {
	q := mustParse(t, "MATCH (n:Person) WHERE NOT n.age < 18 OR n.vip = true RETURN n")

	m := getMatch(t, q, 0)
	if m.Where == nil {
		t.Error("expected non-nil Where predicate tree for OR/NOT clause")
	}
	// Root: OR
	boolExpr, ok := m.Where.(*cypher.BoolExpr)
	if !ok {
		t.Fatalf("expected *BoolExpr at root, got %T", m.Where)
	}
	if boolExpr.Op != "OR" {
		t.Errorf("expected OR at root, got %q", boolExpr.Op)
	}
	// Left side: NOT (n.age < 18)
	if _, ok := boolExpr.Left.(*cypher.NotExpr); !ok {
		t.Errorf("left of OR: expected *NotExpr, got %T", boolExpr.Left)
	}
}

// ─── test 22: 5-hop chain ─────────────────────────────────────────────────────

func TestParse_FiveHopChain(t *testing.T) {
	q := mustParse(t, "MATCH (a)-[:E1]->(b)-[:E2]->(c)-[:E3]->(d)-[:E4]->(e)-[:E5]->(f) RETURN f")

	m := getMatch(t, q, 0)
	pp := m.Pattern[0]

	if len(pp.Chain) != 5 {
		t.Fatalf("expected 5 hops, got %d", len(pp.Chain))
	}
}

// ─── test 23: $param in CREATE properties ────────────────────────────────────

func TestParse_CreateWithParam(t *testing.T) {
	q := mustParse(t, "CREATE (n:Person {name: $name, age: $age})")

	cc := getCreate(t, q, 0)
	pp := cc.Pattern[0]

	if pp.Start.Props["name"] == "" {
		t.Errorf("expected Props[name] to be set, got %v", pp.Start.Props)
	}
	if pp.Start.Props["age"] == "" {
		t.Errorf("expected Props[age] to be set, got %v", pp.Start.Props)
	}
}

// ─── test 24: compound CREATE (node + relationship in one CREATE) ─────────────

func TestParse_CompoundCreate(t *testing.T) {
	q := mustParse(t, "CREATE (a:Person {name: 'Alice'})-[:KNOWS]->(b:Person {name: 'Bob'})")

	cc := getCreate(t, q, 0)
	if len(cc.Pattern) != 1 {
		t.Fatalf("expected 1 pattern part, got %d", len(cc.Pattern))
	}
	pp := cc.Pattern[0]

	if len(pp.Start.Labels) != 1 || pp.Start.Labels[0] != "Person" {
		t.Errorf("start labels: got %v", pp.Start.Labels)
	}
	if len(pp.Chain) != 1 {
		t.Fatalf("expected 1 chain hop, got %d", len(pp.Chain))
	}
	if len(pp.Chain[0].Node.Labels) != 1 || pp.Chain[0].Node.Labels[0] != "Person" {
		t.Errorf("end node labels: got %v", pp.Chain[0].Node.Labels)
	}
}

// ─── test 25: ORDER BY multiple columns ──────────────────────────────────────

func TestParse_OrderByMultipleColumns(t *testing.T) {
	q := mustParse(t, "MATCH (n:Person) RETURN n.name AS name, n.age AS age ORDER BY name ASC, age DESC")

	ret := getReturn(t, q, 1)
	if len(ret.OrderBy) != 2 {
		t.Fatalf("expected 2 sort items, got %d", len(ret.OrderBy))
	}
	if ret.OrderBy[0].Descending {
		t.Error("first sort item: expected ASC")
	}
	if !ret.OrderBy[1].Descending {
		t.Error("second sort item: expected DESC")
	}
}

// ─── test 26–32: openCypher string escape sequences ──────────────────────────

// parseLiteralString is a helper that parses a Cypher WHERE clause containing a
// single string equality comparison and returns the parsed string value.
func parseLiteralString(t *testing.T, cypherLiteral string) string {
	t.Helper()
	// Build: MATCH (n) WHERE n.x = <literal> RETURN n
	q := mustParse(t, "MATCH (n) WHERE n.x = "+cypherLiteral+" RETURN n")
	m := getMatch(t, q, 0)
	if m.Where == nil {
		t.Fatal("expected non-nil Where")
	}
	cmp, ok := m.Where.(*cypher.ComparisonExpr)
	if !ok {
		t.Fatalf("expected *ComparisonExpr, got %T", m.Where)
	}
	lit, ok := cmp.Right.(*cypher.LiteralExpr)
	if !ok {
		t.Fatalf("expected *LiteralExpr on RHS, got %T", cmp.Right)
	}
	s, ok := lit.Value.(string)
	if !ok {
		t.Fatalf("expected string value, got %T(%v)", lit.Value, lit.Value)
	}
	return s
}

func TestParse_StringEscape_Newline(t *testing.T) {
	got := parseLiteralString(t, `'Hello\nWorld'`)
	want := "Hello\nWorld"
	if got != want {
		t.Errorf("\\n escape: got %q, want %q", got, want)
	}
}

func TestParse_StringEscape_Tab(t *testing.T) {
	got := parseLiteralString(t, `'\t'`)
	want := "\t"
	if got != want {
		t.Errorf("\\t escape: got %q, want %q", got, want)
	}
}

func TestParse_StringEscape_CarriageReturn(t *testing.T) {
	got := parseLiteralString(t, `'\r'`)
	want := "\r"
	if got != want {
		t.Errorf("\\r escape: got %q, want %q", got, want)
	}
}

func TestParse_StringEscape_Backspace(t *testing.T) {
	got := parseLiteralString(t, `'\b'`)
	want := "\b"
	if got != want {
		t.Errorf("\\b escape: got %q, want %q", got, want)
	}
}

func TestParse_StringEscape_FormFeed(t *testing.T) {
	got := parseLiteralString(t, `'\f'`)
	want := "\f"
	if got != want {
		t.Errorf("\\f escape: got %q, want %q", got, want)
	}
}

func TestParse_StringEscape_Unicode(t *testing.T) {
	// 'A' in Cypher decodes to the Unicode code point U+0041 = 'A'.
	// Use a double-quoted Go string so the backslash is literal.
	got := parseLiteralString(t, "'\\u0041'")
	want := "A"
	if got != want {
		t.Errorf("\\u0041 escape: got %q, want %q", got, want)
	}
}

func TestParse_StringEscape_UnrecognisedReturnsError(t *testing.T) {
	// \q is not a valid openCypher escape sequence; Parse should return an error.
	_, err := cypher.Parse(`MATCH (n) WHERE n.x = '\q' RETURN n`)
	if err == nil {
		t.Error("expected error for unrecognised escape sequence \\q, got nil")
	}
}
