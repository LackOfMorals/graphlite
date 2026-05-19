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

// ─── test 11: MATCH with WHERE clause ────────────────────────────────────────

func TestPlanner_MatchWithWhere(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (n:Person) WHERE n.age > 18 RETURN n")

	rp := asReturn(t, plan)
	fp := asFilter(t, rp.Source)

	// Source of filter must be the match node plan.
	asMatchNode(t, fp.Source)

	// Predicate must be a RawExpr (WHERE clause is stored raw for now).
	if fp.Predicate == nil {
		t.Fatal("expected non-nil Predicate in FilterPlan")
	}
	raw, ok := fp.Predicate.(*cypher.RawExpr)
	if !ok {
		t.Fatalf("expected *RawExpr predicate (task-008 upgrades to typed), got %T", fp.Predicate)
	}
	if raw.Text == "" {
		t.Error("expected non-empty RawExpr.Text")
	}
}

// ─── test 12: MATCH with $param in WHERE ─────────────────────────────────────

func TestPlanner_MatchWhereWithParam(t *testing.T) {
	plan, _ := mustPlan(t, "MATCH (n:Person) WHERE n.name = $name RETURN n")

	rp := asReturn(t, plan)
	fp := asFilter(t, rp.Source)

	if fp.Predicate == nil {
		t.Fatal("expected non-nil predicate")
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
