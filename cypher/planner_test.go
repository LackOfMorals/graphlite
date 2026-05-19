package cypher_test

import (
	"testing"

	"github.com/LackOfMorals/graphlite/cypher"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func mustPlan(t *testing.T, query string) (cypher.LogicalPlan, *cypher.BindingScope) {
	t.Helper()
	q, err := cypher.Parse(query)
	if err != nil {
		t.Fatalf("Parse(%q): unexpected error: %v", query, err)
	}
	scope := cypher.NewScope()
	plan, err := cypher.Plan(q, scope)
	if err != nil {
		t.Fatalf("Plan(%q): unexpected error: %v", query, err)
	}
	if plan == nil {
		t.Fatalf("Plan(%q): returned nil", query)
	}
	return plan, scope
}

// asSequence unwraps a plan as *SequencePlan; fails if it is a different type.
func asSequence(t *testing.T, plan cypher.LogicalPlan) *cypher.SequencePlan {
	t.Helper()
	sp, ok := plan.(*cypher.SequencePlan)
	if !ok {
		t.Fatalf("expected *SequencePlan, got %T", plan)
	}
	return sp
}

// asReturn unwraps a plan as *ReturnPlan; fails if it is a different type.
func asReturn(t *testing.T, plan cypher.LogicalPlan) *cypher.ReturnPlan {
	t.Helper()
	rp, ok := plan.(*cypher.ReturnPlan)
	if !ok {
		t.Fatalf("expected *ReturnPlan, got %T", plan)
	}
	return rp
}

// asMatchNode unwraps a plan as *MatchNodePlan; fails otherwise.
func asMatchNode(t *testing.T, plan cypher.LogicalPlan) *cypher.MatchNodePlan {
	t.Helper()
	mn, ok := plan.(*cypher.MatchNodePlan)
	if !ok {
		t.Fatalf("expected *MatchNodePlan, got %T", plan)
	}
	return mn
}

// asMatchRel unwraps a plan as *MatchRelPlan; fails otherwise.
func asMatchRel(t *testing.T, plan cypher.LogicalPlan) *cypher.MatchRelPlan {
	t.Helper()
	mr, ok := plan.(*cypher.MatchRelPlan)
	if !ok {
		t.Fatalf("expected *MatchRelPlan, got %T", plan)
	}
	return mr
}

// asFilter unwraps a plan as *FilterPlan; fails otherwise.
func asFilter(t *testing.T, plan cypher.LogicalPlan) *cypher.FilterPlan {
	t.Helper()
	fp, ok := plan.(*cypher.FilterPlan)
	if !ok {
		t.Fatalf("expected *FilterPlan, got %T", plan)
	}
	return fp
}

// ─── test 01: MATCH (n) — all nodes ──────────────────────────────────────────

func TestPlanner_MatchAllNodes(t *testing.T) {
	plan, scope := mustPlan(t, "MATCH (n) RETURN n")

	rp := asReturn(t, plan)
	mn := asMatchNode(t, rp.Source)

	if mn.Variable != "n" {
		t.Errorf("Variable: want %q got %q", "n", mn.Variable)
	}
	if len(mn.Labels) != 0 {
		t.Errorf("Labels: want [] got %v", mn.Labels)
	}
	if len(mn.Props) != 0 {
		t.Errorf("Props: want empty got %v", mn.Props)
	}
	if mn.SQLAlias == "" {
		t.Error("SQLAlias must be non-empty")
	}

	// Scope must have "n" bound.
	b, ok := scope.Resolve("n")
	if !ok {
		t.Fatal("expected 'n' in scope")
	}
	if !b.IsNode {
		t.Error("expected IsNode=true for node variable")
	}
	if b.Alias != mn.SQLAlias {
		t.Errorf("scope alias %q does not match plan alias %q", b.Alias, mn.SQLAlias)
	}
}

// ─── test 02: MATCH (n:Label) — by label ─────────────────────────────────────

func TestPlanner_MatchByLabel(t *testing.T) {
	plan, scope := mustPlan(t, "MATCH (n:Person) RETURN n")

	rp := asReturn(t, plan)
	mn := asMatchNode(t, rp.Source)

	if len(mn.Labels) != 1 || mn.Labels[0] != "Person" {
		t.Errorf("Labels: want [Person] got %v", mn.Labels)
	}

	b, ok := scope.Resolve("n")
	if !ok || !b.IsNode {
		t.Fatalf("expected 'n' bound as node in scope, got ok=%v", ok)
	}
}

// ─── test 03: MATCH (n:Label {prop: val}) — label + property ─────────────────

func TestPlanner_MatchByLabelAndProperty(t *testing.T) {
	plan, _ := mustPlan(t, `MATCH (n:Person {name: 'Alice'}) RETURN n`)

	rp := asReturn(t, plan)
	mn := asMatchNode(t, rp.Source)

	if len(mn.Labels) != 1 || mn.Labels[0] != "Person" {
		t.Errorf("Labels: want [Person] got %v", mn.Labels)
	}
	if len(mn.Props) == 0 {
		t.Error("expected Props to be non-empty")
	}
	nameProp, ok := mn.Props["name"]
	if !ok {
		t.Fatal("expected Props[name] to be set")
	}
	lit, ok := nameProp.(*cypher.LiteralExpr)
	if !ok {
		t.Fatalf("expected *LiteralExpr for name, got %T", nameProp)
	}
	if lit.Value != "Alice" {
		t.Errorf("Props[name]: want %q got %v", "Alice", lit.Value)
	}
}

// ─── test 04: MATCH (n:L1:L2) — multi-label AND semantics ───────────────────

func TestPlanner_MatchMultiLabel(t *testing.T) {
	plan, scope := mustPlan(t, "MATCH (n:Person:Employee) RETURN n")

	rp := asReturn(t, plan)
	mn := asMatchNode(t, rp.Source)

	if len(mn.Labels) != 2 {
		t.Fatalf("expected 2 labels, got %d: %v", len(mn.Labels), mn.Labels)
	}
	labels := map[string]bool{mn.Labels[0]: true, mn.Labels[1]: true}
	if !labels["Person"] || !labels["Employee"] {
		t.Errorf("expected Person and Employee labels, got %v", mn.Labels)
	}

	b, ok := scope.Resolve("n")
	if !ok || !b.IsNode {
		t.Fatalf("expected 'n' in scope as node, ok=%v", ok)
	}
}

// ─── test 05: MATCH (a)-[r:TYPE]->(b) — directed single-hop ─────────────────

func TestPlanner_MatchDirectedSingleHop(t *testing.T) {
	plan, scope := mustPlan(t, "MATCH (a:Person)-[r:KNOWS]->(b:Person) RETURN a, b")

	rp := asReturn(t, plan)
	mr := asMatchRel(t, rp.Source)

	if mr.RelVariable != "r" {
		t.Errorf("RelVariable: want %q got %q", "r", mr.RelVariable)
	}
	if len(mr.Types) != 1 || mr.Types[0] != "KNOWS" {
		t.Errorf("Types: want [KNOWS] got %v", mr.Types)
	}
	if !mr.ToRight {
		t.Error("expected ToRight=true for directed ->")
	}
	if mr.ToLeft || mr.Undirected {
		t.Errorf("expected ToLeft=false, Undirected=false; got ToLeft=%v Undirected=%v", mr.ToLeft, mr.Undirected)
	}
	if mr.StartVar != "a" {
		t.Errorf("StartVar: want %q got %q", "a", mr.StartVar)
	}
	if mr.EndVar != "b" {
		t.Errorf("EndVar: want %q got %q", "b", mr.EndVar)
	}
	if mr.EndNode.Labels[0] != "Person" {
		t.Errorf("EndNode.Labels: want [Person] got %v", mr.EndNode.Labels)
	}
	if mr.RelSQLAlias == "" {
		t.Error("RelSQLAlias must be non-empty")
	}

	// All three variables must be in scope.
	for _, varName := range []string{"a", "b"} {
		b, ok := scope.Resolve(varName)
		if !ok {
			t.Fatalf("expected %q in scope", varName)
		}
		if !b.IsNode {
			t.Errorf("expected %q to be a node binding", varName)
		}
	}
	rb, ok := scope.Resolve("r")
	if !ok {
		t.Fatal("expected 'r' in scope")
	}
	if !rb.IsRel {
		t.Error("expected 'r' to be a relationship binding")
	}
}

// ─── test 06: MATCH (a)-[r:TYPE]-(b) — undirected single-hop ────────────────

func TestPlanner_MatchUndirectedSingleHop(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (a)-[r:KNOWS]-(b) RETURN a, b")

	rp := asReturn(t, plan)
	mr := asMatchRel(t, rp.Source)

	if !mr.Undirected {
		t.Error("expected Undirected=true for -[r]-")
	}
	if mr.ToLeft || mr.ToRight {
		t.Errorf("expected ToLeft=false, ToRight=false; got ToLeft=%v ToRight=%v", mr.ToLeft, mr.ToRight)
	}
}

// ─── test 07: multi-hop chain (2 hops) ───────────────────────────────────────

func TestPlanner_MatchTwoHopChain(t *testing.T) {
	plan, scope := mustPlan(t, "MATCH (a)-[r1:KNOWS]->(b)-[r2:LIKES]->(c) RETURN c")

	rp := asReturn(t, plan)

	// Two hops → SequencePlan with 2 steps.
	seq := asSequence(t, rp.Source)
	if len(seq.Steps) != 2 {
		t.Fatalf("expected 2 steps in SequencePlan, got %d", len(seq.Steps))
	}

	hop1 := asMatchRel(t, seq.Steps[0])
	hop2 := asMatchRel(t, seq.Steps[1])

	if hop1.Types[0] != "KNOWS" {
		t.Errorf("hop1 type: want KNOWS got %q", hop1.Types[0])
	}
	if hop2.Types[0] != "LIKES" {
		t.Errorf("hop2 type: want LIKES got %q", hop2.Types[0])
	}
	if hop1.StartVar != "a" || hop1.EndVar != "b" {
		t.Errorf("hop1 vars: want a→b got %q→%q", hop1.StartVar, hop1.EndVar)
	}
	if hop2.StartVar != "b" || hop2.EndVar != "c" {
		t.Errorf("hop2 vars: want b→c got %q→%q", hop2.StartVar, hop2.EndVar)
	}

	// All five variables in scope.
	for _, v := range []string{"a", "b", "c"} {
		b, ok := scope.Resolve(v)
		if !ok {
			t.Fatalf("variable %q not in scope", v)
		}
		if !b.IsNode {
			t.Errorf("variable %q should be a node", v)
		}
	}
	for _, v := range []string{"r1", "r2"} {
		b, ok := scope.Resolve(v)
		if !ok {
			t.Fatalf("variable %q not in scope", v)
		}
		if !b.IsRel {
			t.Errorf("variable %q should be a relationship", v)
		}
	}
}

// ─── test 08: multi-hop chain (3 hops) ───────────────────────────────────────

func TestPlanner_MatchThreeHopChain(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (a)-[:E1]->(b)-[:E2]->(c)-[:E3]->(d) RETURN d")

	rp := asReturn(t, plan)
	seq := asSequence(t, rp.Source)

	if len(seq.Steps) != 3 {
		t.Fatalf("expected 3 steps for 3-hop chain, got %d", len(seq.Steps))
	}
	types := []string{"E1", "E2", "E3"}
	for i, step := range seq.Steps {
		mr := asMatchRel(t, step)
		if len(mr.Types) != 1 || mr.Types[0] != types[i] {
			t.Errorf("hop %d: want type %q got %v", i, types[i], mr.Types)
		}
	}
}

// ─── test 09: multi-hop chain (5 hops) ───────────────────────────────────────

func TestPlanner_MatchFiveHopChain(t *testing.T) {
	plan, scope := mustPlan(t, "MATCH (a)-[:E1]->(b)-[:E2]->(c)-[:E3]->(d)-[:E4]->(e)-[:E5]->(f) RETURN f")

	rp := asReturn(t, plan)
	seq := asSequence(t, rp.Source)

	if len(seq.Steps) != 5 {
		t.Fatalf("expected 5 steps for 5-hop chain, got %d", len(seq.Steps))
	}

	// All 6 node variables bound in scope.
	for _, v := range []string{"a", "b", "c", "d", "e", "f"} {
		if _, ok := scope.Resolve(v); !ok {
			t.Fatalf("expected %q in scope", v)
		}
	}
}

// ─── test 10: multi-hop chain (4 hops) ───────────────────────────────────────

func TestPlanner_MatchFourHopChain(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (a)-[:E1]->(b)-[:E2]->(c)-[:E3]->(d)-[:E4]->(e) RETURN e")

	rp := asReturn(t, plan)
	seq := asSequence(t, rp.Source)

	if len(seq.Steps) != 4 {
		t.Fatalf("expected 4 steps for 4-hop chain, got %d", len(seq.Steps))
	}
}

// ─── test 11: MATCH with WHERE clause produces typed FilterPlan ──────────────

func TestPlanner_MatchWithWhere(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (n:Person) WHERE n.age > 18 RETURN n")

	rp := asReturn(t, plan)
	fp := asFilter(t, rp.Source)

	// Source of filter must be the match node plan.
	asMatchNode(t, fp.Source)

	// Predicate must be a typed ComparisonExpr: n.age > 18
	if fp.Predicate == nil {
		t.Fatal("expected non-nil Predicate in FilterPlan")
	}
	cmp, ok := fp.Predicate.(*cypher.ComparisonExpr)
	if !ok {
		t.Fatalf("expected *ComparisonExpr predicate, got %T", fp.Predicate)
	}
	if cmp.Op != ">" {
		t.Errorf("expected > operator, got %q", cmp.Op)
	}
	// LHS: n.age
	prop, ok := cmp.Left.(*cypher.PropExpr)
	if !ok {
		t.Fatalf("expected *PropExpr on LHS, got %T", cmp.Left)
	}
	if prop.Variable != "n" || prop.Property != "age" {
		t.Errorf("expected n.age, got %q.%q", prop.Variable, prop.Property)
	}
	// RHS: 18
	lit, ok := cmp.Right.(*cypher.LiteralExpr)
	if !ok {
		t.Fatalf("expected *LiteralExpr on RHS, got %T", cmp.Right)
	}
	if lit.Value != int64(18) {
		t.Errorf("expected int64(18), got %v (%T)", lit.Value, lit.Value)
	}
}

// ─── test 12: MATCH with $param in WHERE produces ParamRef in predicate tree ─

func TestPlanner_MatchWhereWithParam(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (n:Person) WHERE n.name = $name RETURN n")

	rp := asReturn(t, plan)
	fp := asFilter(t, rp.Source)

	if fp.Predicate == nil {
		t.Fatal("expected non-nil predicate")
	}
	// Predicate must be a ComparisonExpr with RHS = ParamRef.
	cmp, ok := fp.Predicate.(*cypher.ComparisonExpr)
	if !ok {
		t.Fatalf("expected *ComparisonExpr, got %T", fp.Predicate)
	}
	if cmp.Op != "=" {
		t.Errorf("expected = operator, got %q", cmp.Op)
	}
	pr, ok := cmp.Right.(*cypher.ParamRef)
	if !ok {
		t.Fatalf("expected *ParamRef on RHS, got %T", cmp.Right)
	}
	if pr.Name != "name" {
		t.Errorf("ParamRef.Name: want %q got %q", "name", pr.Name)
	}
}

// ─── test 13: RETURN with AS alias ───────────────────────────────────────────

func TestPlanner_ReturnWithAlias(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (n:Person) RETURN n.name AS name")

	rp := asReturn(t, plan)
	if len(rp.Projections) != 1 {
		t.Fatalf("expected 1 projection, got %d", len(rp.Projections))
	}
	proj := rp.Projections[0]
	if proj.Alias != "name" {
		t.Errorf("Alias: want %q got %q", "name", proj.Alias)
	}
	pe, ok := proj.Expr.(*cypher.PropExpr)
	if !ok {
		t.Fatalf("expected *PropExpr, got %T", proj.Expr)
	}
	if pe.Variable != "n" || pe.Property != "name" {
		t.Errorf("PropExpr: want n.name got %q.%q", pe.Variable, pe.Property)
	}
}

// ─── test 14: RETURN whole node variable ─────────────────────────────────────

func TestPlanner_ReturnWholeNode(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (n:Person) RETURN n")

	rp := asReturn(t, plan)
	if len(rp.Projections) != 1 {
		t.Fatalf("expected 1 projection, got %d", len(rp.Projections))
	}
	proj := rp.Projections[0]
	ve, ok := proj.Expr.(*cypher.VarExpr)
	if !ok {
		t.Fatalf("expected *VarExpr for whole-node return, got %T", proj.Expr)
	}
	if ve.Name != "n" {
		t.Errorf("VarExpr.Name: want %q got %q", "n", ve.Name)
	}
}

// ─── test 15: RETURN with ORDER BY, SKIP, LIMIT ───────────────────────────────

func TestPlanner_ReturnOrderBySkipLimit(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (n:Person) RETURN n.name AS name ORDER BY name DESC SKIP 5 LIMIT 10")

	rp := asReturn(t, plan)
	if len(rp.OrderBy) != 1 {
		t.Fatalf("expected 1 sort spec, got %d", len(rp.OrderBy))
	}
	if !rp.OrderBy[0].Descending {
		t.Error("expected Descending=true")
	}
	if rp.Skip == nil || *rp.Skip != 5 {
		t.Errorf("Skip: want 5 got %v", rp.Skip)
	}
	if rp.Limit == nil || *rp.Limit != 10 {
		t.Errorf("Limit: want 10 got %v", rp.Limit)
	}
}

// ─── test 16: RETURN DISTINCT ────────────────────────────────────────────────

func TestPlanner_ReturnDistinct(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (n:Person) RETURN DISTINCT n.name")

	rp := asReturn(t, plan)
	if !rp.Distinct {
		t.Error("expected Distinct=true")
	}
}

// ─── test 17: scope binding for undirected hop ───────────────────────────────

func TestPlanner_UndirectedHop_ScopeBindings(t *testing.T) {
	_, scope := mustPlan(t, "MATCH (a)-[r]-(b) RETURN a, b")

	for _, v := range []string{"a", "b"} {
		b, ok := scope.Resolve(v)
		if !ok {
			t.Fatalf("expected %q in scope", v)
		}
		if !b.IsNode {
			t.Errorf("%q should be a node binding", v)
		}
	}
	rb, ok := scope.Resolve("r")
	if !ok {
		t.Fatal("expected 'r' in scope")
	}
	if !rb.IsRel {
		t.Error("expected 'r' to be a relationship binding")
	}
}

// ─── test 18: anonymous variables do not pollute scope ───────────────────────

func TestPlanner_AnonymousNode_NotInScope(t *testing.T) {
	_, scope := mustPlan(t, "MATCH ()-[r:KNOWS]->(b) RETURN b")

	// 'b' and 'r' should be in scope; the anonymous node should not add any name.
	if _, ok := scope.Resolve("b"); !ok {
		t.Fatal("expected 'b' in scope")
	}
	if _, ok := scope.Resolve("r"); !ok {
		t.Fatal("expected 'r' in scope")
	}
	// The scope should not have any entry starting with "" (anonymous).
	for _, name := range scope.Names() {
		if name == "" {
			t.Error("anonymous node variable should not appear in scope Names()")
		}
	}
}

// ─── test 19: MATCH (n:Label {prop: $param}) — param in inline props ─────────

func TestPlanner_MatchInlineParamProp(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (n:Person {name: $name}) RETURN n")

	rp := asReturn(t, plan)
	mn := asMatchNode(t, rp.Source)

	nameProp, ok := mn.Props["name"]
	if !ok {
		t.Fatal("expected Props[name]")
	}
	pr, ok := nameProp.(*cypher.ParamRef)
	if !ok {
		t.Fatalf("expected *ParamRef for $param prop, got %T", nameProp)
	}
	if pr.Name != "name" {
		t.Errorf("ParamRef.Name: want %q got %q", "name", pr.Name)
	}
}

// ─── test 20: MATCH (n:Label {intProp: 42}) — integer literal in inline props ─

func TestPlanner_MatchInlineIntProp(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (n:Item {qty: 42}) RETURN n")

	rp := asReturn(t, plan)
	mn := asMatchNode(t, rp.Source)

	qtyProp, ok := mn.Props["qty"]
	if !ok {
		t.Fatal("expected Props[qty]")
	}
	lit, ok := qtyProp.(*cypher.LiteralExpr)
	if !ok {
		t.Fatalf("expected *LiteralExpr for integer prop, got %T", qtyProp)
	}
	if lit.Value != int64(42) {
		t.Errorf("LiteralExpr.Value: want int64(42) got %v (%T)", lit.Value, lit.Value)
	}
}

// ─── test 21: variable-length path returns error ─────────────────────────────

func TestPlanner_VarLengthPath_Error(t *testing.T) {
	// The parser accepts variable-length paths and sets VarLength=true;
	// the planner must return an error.
	q, err := cypher.Parse("MATCH (a)-[r*1..3]->(b) RETURN b")
	if err != nil {
		// Parser may also reject it — both outcomes are acceptable.
		return
	}
	scope := cypher.NewScope()
	_, planErr := cypher.Plan(q, scope)
	if planErr == nil {
		t.Error("expected error for variable-length path, got nil")
	}
}

// ─── test 22: multiple projections in RETURN ─────────────────────────────────

func TestPlanner_ReturnMultipleProjections(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (n:Person) RETURN n.name AS name, n.age AS age")

	rp := asReturn(t, plan)
	if len(rp.Projections) != 2 {
		t.Fatalf("expected 2 projections, got %d", len(rp.Projections))
	}
	if rp.Projections[0].Alias != "name" {
		t.Errorf("proj[0] alias: want %q got %q", "name", rp.Projections[0].Alias)
	}
	if rp.Projections[1].Alias != "age" {
		t.Errorf("proj[1] alias: want %q got %q", "age", rp.Projections[1].Alias)
	}
}

// ─── test 23: multi-hop — SQL aliases are unique ─────────────────────────────

func TestPlanner_MultiHop_UniqueAliases(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (a)-[r1]->(b)-[r2]->(c) RETURN c")

	rp := asReturn(t, plan)
	seq := asSequence(t, rp.Source)

	mr1 := asMatchRel(t, seq.Steps[0])
	mr2 := asMatchRel(t, seq.Steps[1])

	if mr1.RelSQLAlias == mr2.RelSQLAlias {
		t.Errorf("hop SQL aliases must be unique: both got %q", mr1.RelSQLAlias)
	}
	if mr1.EndNode.SQLAlias == mr2.EndNode.SQLAlias {
		t.Errorf("end node SQL aliases must be unique: both got %q", mr1.EndNode.SQLAlias)
	}
}

// ─── test 24: multi-hop — shared intermediate variable has same alias ─────────

func TestPlanner_MultiHop_SharedIntermediateAlias(t *testing.T) {
	plan, scope := mustPlan(t, "MATCH (a)-[r1]->(b)-[r2]->(c) RETURN c")

	rp := asReturn(t, plan)
	seq := asSequence(t, rp.Source)

	mr1 := asMatchRel(t, seq.Steps[0])
	mr2 := asMatchRel(t, seq.Steps[1])

	// The end node of hop1 is "b"; the start of hop2 is also "b".
	// The EndNode.SQLAlias for hop1 should match the scope alias for "b".
	bBinding, ok := scope.Resolve("b")
	if !ok {
		t.Fatal("expected 'b' in scope")
	}
	if mr1.EndNode.SQLAlias != bBinding.Alias {
		t.Errorf("hop1 EndNode alias %q != scope alias for 'b' %q", mr1.EndNode.SQLAlias, bBinding.Alias)
	}
	// hop2's StartVar references "b" by name — the translator resolves it from scope.
	if mr2.StartVar != "b" {
		t.Errorf("hop2 StartVar: want %q got %q", "b", mr2.StartVar)
	}
}

// ─── test 25: OPTIONAL MATCH — optional flag propagated ──────────────────────

func TestPlanner_OptionalMatch(t *testing.T) {
	plan, scope := mustPlan(t, "MATCH (n:Person) OPTIONAL MATCH (n)-[r:KNOWS]->(m) RETURN n, m")

	rp := asReturn(t, plan)

	// The plan tree for MATCH + OPTIONAL MATCH is a SequencePlan
	// (non-optional node plan + optional rel plan, wrapped in ReturnPlan).
	seq := asSequence(t, rp.Source)
	if len(seq.Steps) < 2 {
		t.Fatalf("expected at least 2 steps for MATCH + OPTIONAL MATCH, got %d", len(seq.Steps))
	}

	// The first step is the non-optional node match.
	mn := asMatchNode(t, seq.Steps[0])
	if mn.Optional {
		t.Error("first MATCH node should not be optional")
	}

	// The second step is the optional relationship match.
	mr := asMatchRel(t, seq.Steps[1])
	if !mr.Optional {
		t.Error("OPTIONAL MATCH relationship should have Optional=true")
	}

	// "m" introduced by OPTIONAL MATCH must be nullable in scope.
	mBinding, ok := scope.Resolve("m")
	if !ok {
		t.Fatal("expected 'm' in scope from OPTIONAL MATCH")
	}
	if !mBinding.IsNullable {
		t.Error("variable 'm' introduced by OPTIONAL MATCH must be IsNullable=true")
	}
}

// ─── task-008 WHERE clause tests ─────────────────────────────────────────────

// asComparison unwraps a plan predicate as *ComparisonExpr; fails otherwise.
func asComparison(t *testing.T, expr cypher.Expr) *cypher.ComparisonExpr {
	t.Helper()
	cmp, ok := expr.(*cypher.ComparisonExpr)
	if !ok {
		t.Fatalf("expected *ComparisonExpr, got %T", expr)
	}
	return cmp
}

// asBoolExpr unwraps an expression as *BoolExpr; fails otherwise.
func asBoolExpr(t *testing.T, expr cypher.Expr) *cypher.BoolExpr {
	t.Helper()
	be, ok := expr.(*cypher.BoolExpr)
	if !ok {
		t.Fatalf("expected *BoolExpr, got %T", expr)
	}
	return be
}

// asNotExpr unwraps an expression as *NotExpr; fails otherwise.
func asNotExpr(t *testing.T, expr cypher.Expr) *cypher.NotExpr {
	t.Helper()
	ne, ok := expr.(*cypher.NotExpr)
	if !ok {
		t.Fatalf("expected *NotExpr, got %T", expr)
	}
	return ne
}

// getFilterPredicate returns the predicate from a ReturnPlan → FilterPlan chain.
func getFilterPredicate(t *testing.T, query string) cypher.Expr {
	t.Helper()
	plan, _ := mustPlan(t, query)
	rp := asReturn(t, plan)
	fp := asFilter(t, rp.Source)
	if fp.Predicate == nil {
		t.Fatal("expected non-nil Predicate")
	}
	return fp.Predicate
}

// ─── test 26: all six comparison operators ───────────────────────────────────

func TestPlanner_WHERE_AllComparisonOperators(t *testing.T) {
	tests := []struct {
		query string
		op    string
	}{
		{"MATCH (n:T) WHERE n.x = 1 RETURN n", "="},
		{"MATCH (n:T) WHERE n.x <> 1 RETURN n", "<>"},
		{"MATCH (n:T) WHERE n.x < 1 RETURN n", "<"},
		{"MATCH (n:T) WHERE n.x > 1 RETURN n", ">"},
		{"MATCH (n:T) WHERE n.x <= 1 RETURN n", "<="},
		{"MATCH (n:T) WHERE n.x >= 1 RETURN n", ">="},
	}
	for _, tc := range tests {
		t.Run(tc.op, func(t *testing.T) {
			pred := getFilterPredicate(t, tc.query)
			cmp := asComparison(t, pred)
			if cmp.Op != tc.op {
				t.Errorf("op: want %q got %q", tc.op, cmp.Op)
			}
			// LHS must be PropExpr n.x
			prop, ok := cmp.Left.(*cypher.PropExpr)
			if !ok {
				t.Fatalf("LHS: expected *PropExpr, got %T", cmp.Left)
			}
			if prop.Variable != "n" || prop.Property != "x" {
				t.Errorf("LHS: want n.x, got %q.%q", prop.Variable, prop.Property)
			}
			// RHS must be LiteralExpr(1)
			lit, ok := cmp.Right.(*cypher.LiteralExpr)
			if !ok {
				t.Fatalf("RHS: expected *LiteralExpr, got %T", cmp.Right)
			}
			if lit.Value != int64(1) {
				t.Errorf("RHS: want int64(1), got %v (%T)", lit.Value, lit.Value)
			}
		})
	}
}

// ─── test 27: AND combinator ─────────────────────────────────────────────────

func TestPlanner_WHERE_AndCombinator(t *testing.T) {
	pred := getFilterPredicate(t, "MATCH (n:Person) WHERE n.age > 18 AND n.active = true RETURN n")

	be := asBoolExpr(t, pred)
	if be.Op != "AND" {
		t.Errorf("expected AND, got %q", be.Op)
	}
	// Left: n.age > 18
	asComparison(t, be.Left)
	// Right: n.active = true
	asComparison(t, be.Right)
}

// ─── test 28: OR combinator ──────────────────────────────────────────────────

func TestPlanner_WHERE_OrCombinator(t *testing.T) {
	pred := getFilterPredicate(t, "MATCH (n:Person) WHERE n.age > 18 OR n.vip = true RETURN n")

	be := asBoolExpr(t, pred)
	if be.Op != "OR" {
		t.Errorf("expected OR, got %q", be.Op)
	}
	asComparison(t, be.Left)
	asComparison(t, be.Right)
}

// ─── test 29: NOT combinator ─────────────────────────────────────────────────

func TestPlanner_WHERE_NotCombinator(t *testing.T) {
	pred := getFilterPredicate(t, "MATCH (n:Person) WHERE NOT n.age < 18 RETURN n")

	ne := asNotExpr(t, pred)
	asComparison(t, ne.Expr)
}

// ─── test 30: nested AND/OR/NOT precedence ───────────────────────────────────

func TestPlanner_WHERE_NestedBooleanLogic(t *testing.T) {
	// NOT n.age < 18 OR n.vip = true
	// Grammar: NOT binds tighter than OR → root is OR(NOT(age<18), vip=true)
	pred := getFilterPredicate(t, "MATCH (n:Person) WHERE NOT n.age < 18 OR n.vip = true RETURN n")

	be := asBoolExpr(t, pred)
	if be.Op != "OR" {
		t.Errorf("expected OR at root, got %q", be.Op)
	}
	asNotExpr(t, be.Left)
	asComparison(t, be.Right)
}

// ─── test 31: $param reference in WHERE ──────────────────────────────────────

func TestPlanner_WHERE_ParamReference(t *testing.T) {
	pred := getFilterPredicate(t, "MATCH (n:Person) WHERE n.age > $minAge RETURN n")

	cmp := asComparison(t, pred)
	if cmp.Op != ">" {
		t.Errorf("expected > operator, got %q", cmp.Op)
	}
	pr, ok := cmp.Right.(*cypher.ParamRef)
	if !ok {
		t.Fatalf("RHS: expected *ParamRef, got %T", cmp.Right)
	}
	if pr.Name != "minAge" {
		t.Errorf("ParamRef.Name: want %q got %q", "minAge", pr.Name)
	}
}

// ─── test 32: multiple $param references in AND clause ───────────────────────

func TestPlanner_WHERE_MultipleParams(t *testing.T) {
	pred := getFilterPredicate(t, "MATCH (n:Person) WHERE n.name = $name AND n.age > $minAge RETURN n")

	be := asBoolExpr(t, pred)
	if be.Op != "AND" {
		t.Errorf("expected AND, got %q", be.Op)
	}

	left := asComparison(t, be.Left)
	if _, ok := left.Right.(*cypher.ParamRef); !ok {
		t.Errorf("left RHS: expected *ParamRef, got %T", left.Right)
	}

	right := asComparison(t, be.Right)
	if _, ok := right.Right.(*cypher.ParamRef); !ok {
		t.Errorf("right RHS: expected *ParamRef, got %T", right.Right)
	}
}

// ─── test 33: WHERE with string literal ──────────────────────────────────────

func TestPlanner_WHERE_StringLiteral(t *testing.T) {
	pred := getFilterPredicate(t, `MATCH (n:Person) WHERE n.name = 'Alice' RETURN n`)

	cmp := asComparison(t, pred)
	lit, ok := cmp.Right.(*cypher.LiteralExpr)
	if !ok {
		t.Fatalf("expected *LiteralExpr, got %T", cmp.Right)
	}
	if lit.Value != "Alice" {
		t.Errorf("expected %q, got %v", "Alice", lit.Value)
	}
}

// ─── test 34: WHERE with boolean literal ─────────────────────────────────────

func TestPlanner_WHERE_BooleanLiteral(t *testing.T) {
	pred := getFilterPredicate(t, "MATCH (n:Person) WHERE n.active = true RETURN n")

	cmp := asComparison(t, pred)
	lit, ok := cmp.Right.(*cypher.LiteralExpr)
	if !ok {
		t.Fatalf("expected *LiteralExpr, got %T", cmp.Right)
	}
	if lit.Value != true {
		t.Errorf("expected true, got %v", lit.Value)
	}
}

// ─── test 35: WHERE with float literal ───────────────────────────────────────

func TestPlanner_WHERE_FloatLiteral(t *testing.T) {
	pred := getFilterPredicate(t, "MATCH (n:Item) WHERE n.price > 9.99 RETURN n")

	cmp := asComparison(t, pred)
	lit, ok := cmp.Right.(*cypher.LiteralExpr)
	if !ok {
		t.Fatalf("expected *LiteralExpr, got %T", cmp.Right)
	}
	if v, ok := lit.Value.(float64); !ok || v != 9.99 {
		t.Errorf("expected float64(9.99), got %v (%T)", lit.Value, lit.Value)
	}
}

// ─── test 36: invalid WHERE syntax returns structured error, not panic ────────

func TestPlanner_WHERE_InvalidSyntax(t *testing.T) {
	// Syntax error in the WHERE predicate itself.
	_, err := cypher.Parse("MATCH (n) WHERE = n.x RETURN n")
	// The parser (or planner) must return an error, not panic.
	if err == nil {
		// Plan should also fail if parse succeeded.
		q, _ := cypher.Parse("MATCH (n) WHERE = n.x RETURN n")
		if q != nil {
			scope := cypher.NewScope()
			_, planErr := cypher.Plan(q, scope)
			if planErr == nil {
				t.Error("expected error for invalid WHERE syntax, got nil")
			}
		}
	}
	// Either the parser or planner returns an error — both are acceptable outcomes.
	// The key requirement is no panic.
}
