package cypher

import (
	"testing"
)

// TestPlanInterface verifies that all concrete plan node types satisfy LogicalPlan.
// These are compile-time checks expressed as runtime assertions to catch drift.
func TestPlanInterface(t *testing.T) {
	var _ LogicalPlan = (*MatchNodePlan)(nil)
	var _ LogicalPlan = (*MatchRelPlan)(nil)
	var _ LogicalPlan = (*FilterPlan)(nil)
	var _ LogicalPlan = (*ReturnPlan)(nil)
	var _ LogicalPlan = (*CreateNodePlan)(nil)
	var _ LogicalPlan = (*CreateRelPlan)(nil)
	var _ LogicalPlan = (*SetPropPlan)(nil)
	var _ LogicalPlan = (*DeleteNodePlan)(nil)
	var _ LogicalPlan = (*DeleteRelPlan)(nil)
	var _ LogicalPlan = (*SequencePlan)(nil)
}

// TestExprInterface verifies that all expression types satisfy Expr.
func TestExprInterface(t *testing.T) {
	var _ Expr = (*LiteralExpr)(nil)
	var _ Expr = (*ParamRef)(nil)
	var _ Expr = (*PropExpr)(nil)
	var _ Expr = (*VarExpr)(nil)
	var _ Expr = (*ComparisonExpr)(nil)
	var _ Expr = (*BoolExpr)(nil)
	var _ Expr = (*NotExpr)(nil)
	var _ Expr = (*RawExpr)(nil)
}

// TestMatchNodePlan_Fields verifies field initialisation.
func TestMatchNodePlan_Fields(t *testing.T) {
	plan := &MatchNodePlan{
		Variable: "n",
		Labels:   []string{"Person"},
		Props: map[string]Expr{
			"name": &LiteralExpr{Value: "Alice"},
		},
		SQLAlias: "n0",
		Optional: false,
	}

	if plan.Variable != "n" {
		t.Errorf("unexpected Variable: %q", plan.Variable)
	}
	if len(plan.Labels) != 1 || plan.Labels[0] != "Person" {
		t.Errorf("unexpected Labels: %v", plan.Labels)
	}
	if plan.SQLAlias != "n0" {
		t.Errorf("unexpected SQLAlias: %q", plan.SQLAlias)
	}

	nameProp, ok := plan.Props["name"]
	if !ok {
		t.Fatal("missing 'name' property")
	}
	lit, ok := nameProp.(*LiteralExpr)
	if !ok {
		t.Fatalf("expected *LiteralExpr, got %T", nameProp)
	}
	if lit.Value != "Alice" {
		t.Errorf("unexpected literal value: %v", lit.Value)
	}
}

// TestMatchRelPlan_Fields verifies all direction flags.
func TestMatchRelPlan_Fields(t *testing.T) {
	tests := []struct {
		name       string
		toRight    bool
		toLeft     bool
		undirected bool
	}{
		{"directed right", true, false, false},
		{"directed left", false, true, false},
		{"undirected", false, false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := &MatchRelPlan{
				RelVariable: "r",
				Types:       []string{"KNOWS"},
				RelSQLAlias: "r0",
				StartVar:    "a",
				EndVar:      "b",
				ToRight:     tc.toRight,
				ToLeft:      tc.toLeft,
				Undirected:  tc.undirected,
			}
			if plan.ToRight != tc.toRight {
				t.Errorf("ToRight: want %v got %v", tc.toRight, plan.ToRight)
			}
			if plan.ToLeft != tc.toLeft {
				t.Errorf("ToLeft: want %v got %v", tc.toLeft, plan.ToLeft)
			}
			if plan.Undirected != tc.undirected {
				t.Errorf("Undirected: want %v got %v", tc.undirected, plan.Undirected)
			}
		})
	}
}

// TestFilterPlan_WrapsSource verifies the source/predicate relationship.
func TestFilterPlan_WrapsSource(t *testing.T) {
	source := &MatchNodePlan{Variable: "n", SQLAlias: "n0"}
	pred := &ComparisonExpr{
		Left:  &PropExpr{Variable: "n", Property: "age"},
		Op:    ">",
		Right: &LiteralExpr{Value: int64(18)},
	}
	filter := &FilterPlan{Source: source, Predicate: pred}

	if filter.Source != source {
		t.Error("FilterPlan.Source mismatch")
	}
	cmp, ok := filter.Predicate.(*ComparisonExpr)
	if !ok {
		t.Fatalf("expected *ComparisonExpr, got %T", filter.Predicate)
	}
	if cmp.Op != ">" {
		t.Errorf("unexpected Op: %q", cmp.Op)
	}
}

// TestReturnPlan_Fields verifies projections, sort, limit, skip.
func TestReturnPlan_Fields(t *testing.T) {
	skip := int64(5)
	limit := int64(10)
	plan := &ReturnPlan{
		Distinct: true,
		Projections: []ProjectionItem{
			{Expr: &PropExpr{Variable: "n", Property: "name"}, Alias: "name"},
		},
		OrderBy: []SortSpec{
			{Expr: &PropExpr{Variable: "n", Property: "name"}, Descending: true},
		},
		Skip:  &skip,
		Limit: &limit,
	}

	if !plan.Distinct {
		t.Error("expected Distinct=true")
	}
	if len(plan.Projections) != 1 {
		t.Fatalf("expected 1 projection, got %d", len(plan.Projections))
	}
	if plan.Projections[0].Alias != "name" {
		t.Errorf("unexpected alias: %q", plan.Projections[0].Alias)
	}
	if *plan.Skip != 5 {
		t.Errorf("unexpected Skip: %d", *plan.Skip)
	}
	if *plan.Limit != 10 {
		t.Errorf("unexpected Limit: %d", *plan.Limit)
	}
	if !plan.OrderBy[0].Descending {
		t.Error("expected Descending=true in sort spec")
	}
}

// TestCreateNodePlan_Fields tests the create node plan with param props.
func TestCreateNodePlan_Fields(t *testing.T) {
	plan := &CreateNodePlan{
		Variable: "n",
		Labels:   []string{"Person", "Employee"},
		Props: map[string]Expr{
			"name": &ParamRef{Name: "name"},
			"age":  &LiteralExpr{Value: int64(30)},
		},
	}

	if len(plan.Labels) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(plan.Labels))
	}
	nameProp, ok := plan.Props["name"]
	if !ok {
		t.Fatal("missing name prop")
	}
	if _, ok := nameProp.(*ParamRef); !ok {
		t.Fatalf("expected *ParamRef for name, got %T", nameProp)
	}
}

// TestCreateRelPlan_Fields verifies start/end variable references.
func TestCreateRelPlan_Fields(t *testing.T) {
	plan := &CreateRelPlan{
		RelVariable: "r",
		Type:        "KNOWS",
		StartVar:    "a",
		EndVar:      "b",
		Props:       map[string]Expr{"since": &LiteralExpr{Value: int64(2020)}},
	}

	if plan.Type != "KNOWS" {
		t.Errorf("unexpected Type: %q", plan.Type)
	}
	if plan.StartVar != "a" || plan.EndVar != "b" {
		t.Errorf("unexpected start/end: %q %q", plan.StartVar, plan.EndVar)
	}
}

// TestSetPropPlan_Fields verifies variable, property, and value expression.
func TestSetPropPlan_Fields(t *testing.T) {
	plan := &SetPropPlan{
		Variable: "n",
		Property: "name",
		Value:    &ParamRef{Name: "newName"},
	}

	if plan.Variable != "n" || plan.Property != "name" {
		t.Errorf("unexpected variable/property: %q.%q", plan.Variable, plan.Property)
	}
	param, ok := plan.Value.(*ParamRef)
	if !ok {
		t.Fatalf("expected *ParamRef, got %T", plan.Value)
	}
	if param.Name != "newName" {
		t.Errorf("unexpected param name: %q", param.Name)
	}
}

// TestDeleteNodePlan_DetachFlag verifies the detach boolean.
func TestDeleteNodePlan_DetachFlag(t *testing.T) {
	normal := &DeleteNodePlan{Variable: "n", Detach: false}
	detach := &DeleteNodePlan{Variable: "n", Detach: true}

	if normal.Detach {
		t.Error("expected Detach=false for DELETE")
	}
	if !detach.Detach {
		t.Error("expected Detach=true for DETACH DELETE")
	}
}

// TestDeleteRelPlan_Fields verifies relationship variable reference.
func TestDeleteRelPlan_Fields(t *testing.T) {
	plan := &DeleteRelPlan{Variable: "r"}
	if plan.Variable != "r" {
		t.Errorf("unexpected variable: %q", plan.Variable)
	}
}

// TestSequencePlan_MultipleSteps verifies ordered plan composition.
func TestSequencePlan_MultipleSteps(t *testing.T) {
	steps := []LogicalPlan{
		&MatchNodePlan{Variable: "n", SQLAlias: "n0"},
		&SetPropPlan{Variable: "n", Property: "age", Value: &LiteralExpr{Value: int64(25)}},
	}
	plan := &SequencePlan{Steps: steps}

	if len(plan.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(plan.Steps))
	}
	if _, ok := plan.Steps[0].(*MatchNodePlan); !ok {
		t.Errorf("expected *MatchNodePlan at index 0, got %T", plan.Steps[0])
	}
	if _, ok := plan.Steps[1].(*SetPropPlan); !ok {
		t.Errorf("expected *SetPropPlan at index 1, got %T", plan.Steps[1])
	}
}

// TestBoolExpr_NestedPrecedence verifies nested AND/OR/NOT trees.
func TestBoolExpr_NestedPrecedence(t *testing.T) {
	// NOT (n.age > 18 AND n.active = true)
	andExpr := &BoolExpr{
		Left: &ComparisonExpr{
			Left:  &PropExpr{Variable: "n", Property: "age"},
			Op:    ">",
			Right: &LiteralExpr{Value: int64(18)},
		},
		Op: "AND",
		Right: &ComparisonExpr{
			Left:  &PropExpr{Variable: "n", Property: "active"},
			Op:    "=",
			Right: &LiteralExpr{Value: true},
		},
	}
	notExpr := &NotExpr{Expr: andExpr}

	inner, ok := notExpr.Expr.(*BoolExpr)
	if !ok {
		t.Fatalf("expected *BoolExpr inside NotExpr, got %T", notExpr.Expr)
	}
	if inner.Op != "AND" {
		t.Errorf("expected AND, got %q", inner.Op)
	}
}

// TestParamRef_Name verifies param reference fields.
func TestParamRef_Name(t *testing.T) {
	p := &ParamRef{Name: "userId"}
	if p.Name != "userId" {
		t.Errorf("unexpected Name: %q", p.Name)
	}
}

// TestLiteralExpr_Types verifies all supported value types.
func TestLiteralExpr_Types(t *testing.T) {
	cases := []any{
		"hello",
		int64(42),
		float64(3.14),
		true,
		false,
		nil,
	}
	for _, v := range cases {
		lit := &LiteralExpr{Value: v}
		if lit.Value != v {
			t.Errorf("expected %v, got %v", v, lit.Value)
		}
	}
}
