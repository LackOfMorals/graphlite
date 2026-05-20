package cypher

import (
	"fmt"
	"strconv"
	"strings"
)

// Plan walks a *Query AST and produces a LogicalPlan tree that the SQL
// translator can consume without re-inspecting the AST.
//
// The caller supplies a *BindingScope (typically a fresh NewScope()) that is
// populated with every named variable encountered during planning. The scope
// is mutated in place; the caller can inspect it after Plan returns.
//
// Plan is not safe for concurrent calls on the same scope.
func Plan(q *Query, scope *BindingScope) (LogicalPlan, error) {
	return planQuery(q, scope)
}

// ─── alias counter ────────────────────────────────────────────────────────────

// aliasCounter hands out monotonically-increasing SQL table aliases to avoid
// collisions when the same table appears multiple times in a JOIN.
type aliasCounter struct {
	nodeCount int
	relCount  int
}

func (a *aliasCounter) nextNode() string {
	alias := fmt.Sprintf("n%d", a.nodeCount)
	a.nodeCount++
	return alias
}

func (a *aliasCounter) nextRel() string {
	alias := fmt.Sprintf("r%d", a.relCount)
	a.relCount++
	return alias
}

// ─── query planning ───────────────────────────────────────────────────────────

func planQuery(q *Query, scope *BindingScope) (LogicalPlan, error) {
	ac := &aliasCounter{}

	// Collect MATCH plans, filter plans (from WHERE), return plan, and
	// write-operation plans in clause order.
	var matchPlans []LogicalPlan   // MATCH plans (in order)
	var filterPred Expr            // WHERE predicate accumulated across clauses
	var returnPlan *ReturnPlan     // RETURN clause (at most one)
	var writePlans []LogicalPlan   // CREATE / SET / DELETE plans
	var base LogicalPlan           // assembled base plan (updated when WITH is encountered)

	for _, clause := range q.Clauses {
		switch c := clause.(type) {
		case *MatchClause:
			plans, where, err := planMatchClause(c, scope, ac)
			if err != nil {
				return nil, err
			}
			matchPlans = append(matchPlans, plans...)
			if where != nil {
				// Combine multiple WHERE predicates with AND.
				if filterPred == nil {
					filterPred = where
				} else {
					filterPred = &BoolExpr{Left: filterPred, Op: "AND", Right: where}
				}
			}

		case *WithClause:
			// Flush accumulated match plans into the current base.
			switch len(matchPlans) {
			case 0:
				// No match plans yet — base unchanged.
			case 1:
				base = matchPlans[0]
			default:
				base = &SequencePlan{Steps: matchPlans}
			}
			matchPlans = nil

			if filterPred != nil {
				if base == nil {
					return nil, fmt.Errorf("cypher: WHERE clause without a preceding MATCH clause")
				}
				base = &FilterPlan{Source: base, Predicate: filterPred}
				filterPred = nil
			}

			// Build WithPlan from the WITH clause items.
			wp, err := planWithClause(c, scope)
			if err != nil {
				return nil, err
			}
			wp.Source = base
			base = wp

		case *ReturnClause:
			rp, err := planReturnClause(c, scope)
			if err != nil {
				return nil, err
			}
			returnPlan = rp

		case *CreateClause:
			plans, err := planCreateClause(c, scope, ac)
			if err != nil {
				return nil, err
			}
			writePlans = append(writePlans, plans...)

		case *SetClause:
			plans, err := planSetClause(c, scope)
			if err != nil {
				return nil, err
			}
			writePlans = append(writePlans, plans...)

		case *DeleteClause:
			plans, err := planDeleteClause(c, scope)
			if err != nil {
				return nil, err
			}
			writePlans = append(writePlans, plans...)

		default:
			return nil, fmt.Errorf("cypher: unsupported clause type %T", clause)
		}
	}

	// Assemble the plan tree.
	//
	// If base has already been set by a WITH clause, matchPlans/filterPred carry
	// any clauses that follow the last WITH (not typical, but handle defensively).
	//
	// The base plan is:
	//   1. If there are MATCH plans, compose them (single plan or SequencePlan).
	//   2. If there is a WHERE predicate, wrap the base plan in FilterPlan.
	//   3. If there is a RETURN clause, wrap in ReturnPlan.
	//   4. If there are write plans, collect everything into a SequencePlan
	//      (MATCH base / filter first, then each write operation in order).

	if base == nil {
		// No WITH clause encountered — assemble from matchPlans as before.
		switch len(matchPlans) {
		case 0:
			// No MATCH clause — write-only or pure CREATE.
		case 1:
			base = matchPlans[0]
		default:
			base = &SequencePlan{Steps: matchPlans}
		}
	} else if len(matchPlans) > 0 {
		// Trailing MATCH plans after a WITH — should not occur in well-formed queries,
		// but handle defensively by appending them to the base.
		extraSteps := make([]LogicalPlan, 0, 1+len(matchPlans))
		extraSteps = append(extraSteps, base)
		extraSteps = append(extraSteps, matchPlans...)
		base = &SequencePlan{Steps: extraSteps}
	}

	// Wrap in FilterPlan if there is a WHERE predicate.
	if filterPred != nil {
		if base == nil {
			return nil, fmt.Errorf("cypher: WHERE clause without a preceding MATCH clause")
		}
		base = &FilterPlan{Source: base, Predicate: filterPred}
	}

	// Wire up RETURN clause.
	if returnPlan != nil {
		if base == nil {
			// RETURN without a MATCH — treat as a standalone projection with nil source.
		}
		returnPlan.Source = base
		base = returnPlan
	}

	// Append write plans.
	if len(writePlans) > 0 {
		if base != nil {
			// MATCH + write: combine into a SequencePlan (MATCH subtree first,
			// then the write operations).
			all := make([]LogicalPlan, 0, 1+len(writePlans))
			all = append(all, base)
			all = append(all, writePlans...)
			base = &SequencePlan{Steps: all}
		} else if len(writePlans) == 1 {
			base = writePlans[0]
		} else {
			base = &SequencePlan{Steps: writePlans}
		}
	}

	if base == nil {
		return nil, fmt.Errorf("cypher: query produced no plan")
	}

	return base, nil
}

// ─── MATCH clause planning ────────────────────────────────────────────────────

// planMatchClause translates a single MatchClause into one or more LogicalPlan
// nodes (one per pattern part) and an optional WHERE predicate expression.
// It populates scope with all named variables introduced by the clause.
func planMatchClause(mc *MatchClause, scope *BindingScope, ac *aliasCounter) ([]LogicalPlan, Expr, error) {
	var plans []LogicalPlan

	for _, part := range mc.Pattern {
		partPlans, err := planPatternPart(part, mc.Optional, scope, ac)
		if err != nil {
			return nil, nil, err
		}
		plans = append(plans, partPlans...)
	}

	// Use the typed WHERE predicate tree built by the parser (task-008).
	return plans, mc.Where, nil
}

// planPatternPart translates a single PatternPart into one or more LogicalPlan
// nodes and updates the BindingScope.
//
// A PatternPart with no chain hops (just a start node) produces a single
// MatchNodePlan. A PatternPart with one or more hops produces a MatchRelPlan
// for each hop; the start node binding is taken from the scope if the variable
// was already bound (from an earlier pattern part in the same MATCH clause).
func planPatternPart(part PatternPart, optional bool, scope *BindingScope, ac *aliasCounter) ([]LogicalPlan, error) {
	if len(part.Chain) == 0 {
		// Pure node pattern: (n), (n:Label), (n:Label {prop: val}).
		nodePlan, err := planNodePattern(part.Start, optional, scope, ac)
		if err != nil {
			return nil, err
		}
		return []LogicalPlan{nodePlan}, nil
	}

	// Relationship chain: each hop becomes a MatchRelPlan.
	// The start node of each hop is either the first node in the chain (for
	// the first hop) or the end node of the previous hop.
	var plans []LogicalPlan
	currentVar := part.Start.Variable

	// Plan the start node (allocates alias, adds to scope, captures labels/props).
	startNodePlan, err := planNodePattern(part.Start, optional, scope, ac)
	if err != nil {
		return nil, err
	}

	for i, hop := range part.Chain {
		if hop.Rel.VarLength {
			return nil, fmt.Errorf("cypher: variable-length paths are not supported in v0.1 (MATCH hop %d)", i)
		}

		relAlias := ac.nextRel()
		relVar := hop.Rel.Variable

		// Determine direction.
		undirected := !hop.Rel.ToLeft && !hop.Rel.ToRight

		// Plan the end node and add it to the scope.
		endNodePlan, err := planNodePatternNewAlias(hop.Node, optional, scope, ac)
		if err != nil {
			return nil, err
		}

		relPlan := &MatchRelPlan{
			RelVariable: relVar,
			Types:       hop.Rel.Types,
			RelProps:    planPropsMap(hop.Rel.Props),
			RelSQLAlias: relAlias,
			StartVar:    currentVar,
			StartNode:   *startNodePlan,
			EndVar:      hop.Node.Variable,
			EndNode:     *endNodePlan,
			ToRight:     hop.Rel.ToRight,
			ToLeft:      hop.Rel.ToLeft,
			Undirected:  undirected,
			Optional:    optional,
		}

		// Bind the relationship variable if named.
		if relVar != "" {
			scope.Bind(relVar, Binding{
				Alias:      relAlias,
				Column:     relAlias + ".id",
				Table:      "edges",
				IsRel:      true,
				IsNullable: optional,
			})
		}

		plans = append(plans, relPlan)
		// For subsequent hops, the "start node" is the current hop's end node.
		currentVar = hop.Node.Variable
		startNodePlan = endNodePlan
	}

	return plans, nil
}

// planNodePattern creates a MatchNodePlan for a node pattern.
// It looks up the variable in scope (reusing the alias if already bound) or
// allocates a fresh alias and adds the binding. Anonymous nodes get an alias
// but no scope entry.
func planNodePattern(np NodePattern, optional bool, scope *BindingScope, ac *aliasCounter) (*MatchNodePlan, error) {
	var alias string
	if np.Variable != "" {
		if existing, ok := scope.Resolve(np.Variable); ok {
			// Variable already bound (e.g. repeated in multi-part MATCH or cycle).
			alias = existing.Alias
		} else {
			alias = ac.nextNode()
			scope.Bind(np.Variable, Binding{
				Alias:      alias,
				Column:     alias + ".id",
				Table:      "nodes",
				IsNode:     true,
				IsNullable: optional,
			})
		}
	} else {
		// Anonymous node: allocate an alias without adding a scope entry.
		alias = ac.nextNode()
	}

	return &MatchNodePlan{
		Variable: np.Variable,
		Labels:   np.Labels,
		Props:    planPropsMap(np.Props),
		SQLAlias: alias,
		Optional: optional,
	}, nil
}

// planNodePatternNewAlias is an alias for planNodePattern used at hop end-nodes
// to make call sites self-documenting.
var planNodePatternNewAlias = planNodePattern

// ─── RETURN clause planning ───────────────────────────────────────────────────

func planReturnClause(rc *ReturnClause, scope *BindingScope) (*ReturnPlan, error) {
	rp := &ReturnPlan{
		Distinct: rc.Distinct,
	}

	for _, item := range rc.Items {
		proj, err := planReturnItem(item, scope)
		if err != nil {
			return nil, err
		}
		rp.Projections = append(rp.Projections, proj)
	}

	for _, si := range rc.OrderBy {
		sortExpr, err := parseExprText(si.ExprText, scope)
		if err != nil {
			return nil, fmt.Errorf("cypher: ORDER BY: %w", err)
		}
		rp.OrderBy = append(rp.OrderBy, SortSpec{
			Expr:       sortExpr,
			Descending: si.Descending,
		})
	}

	rp.Skip = rc.Skip
	rp.Limit = rc.Limit

	return rp, nil
}

func planReturnItem(item ReturnItem, scope *BindingScope) (ProjectionItem, error) {
	if item.Expr != nil {
		return ProjectionItem{Expr: item.Expr, Alias: item.Alias}, nil
	}
	expr, err := parseExprText(item.ExprText, scope)
	if err != nil {
		return ProjectionItem{}, fmt.Errorf("cypher: RETURN item %q: %w", item.ExprText, err)
	}
	return ProjectionItem{Expr: expr, Alias: item.Alias}, nil
}

// ─── WITH clause planning ─────────────────────────────────────────────────────

// planWithClause builds a WithPlan from a *WithClause and updates the scope
// with aggregate and non-aggregate alias bindings.
func planWithClause(wc *WithClause, scope *BindingScope) (*WithPlan, error) {
	wp := &WithPlan{}

	for _, item := range wc.Items {
		proj, err := planWithItem(item, scope)
		if err != nil {
			return nil, err
		}
		wp.Projections = append(wp.Projections, proj)

		// If this projection has an alias, bind it in scope so subsequent
		// RETURN / WHERE clauses can reference it.
		if item.Alias != "" {
			if agg, ok := proj.Expr.(*AggCallExpr); ok {
				// Aggregate alias: bind with AggExpr so the translator can
				// expand the alias back to the full aggregate expression.
				scope.Bind(item.Alias, Binding{
					Column:  item.Alias,
					AggExpr: agg,
				})
			} else if ve, ok := proj.Expr.(*VarExpr); ok {
				// Non-aggregate alias referencing an existing variable: copy binding.
				if b, found := scope.Resolve(ve.Name); found {
					scope.Bind(item.Alias, b)
				}
			} else {
				// Other non-aggregate expressions (e.g. n.name AS author): bind the
				// alias so downstream RETURN clauses can reference it. We reuse
				// AggExpr as an "expand me" pointer — the translator calls exprToSQL
				// on the stored expression to produce the SQL column reference.
				scope.Bind(item.Alias, Binding{
					Column:  item.Alias,
					AggExpr: proj.Expr,
				})
			}
		}
	}

	// Post-WITH WHERE becomes HAVING in SQL.
	if wc.Where != nil {
		wp.Having = wc.Where
	}

	return wp, nil
}

// planWithItem produces a ProjectionItem for a single WITH item.
func planWithItem(item ReturnItem, scope *BindingScope) (ProjectionItem, error) {
	if item.Expr != nil {
		// Typed expression from ANTLR CST path (aggregates, etc.).
		return ProjectionItem{Expr: item.Expr, Alias: item.Alias}, nil
	}
	expr, err := parseExprText(item.ExprText, scope)
	if err != nil {
		return ProjectionItem{}, fmt.Errorf("cypher: WITH item %q: %w", item.ExprText, err)
	}
	return ProjectionItem{Expr: expr, Alias: item.Alias}, nil
}

// ─── CREATE clause planning ───────────────────────────────────────────────────

func planCreateClause(cc *CreateClause, scope *BindingScope, ac *aliasCounter) ([]LogicalPlan, error) {
	var plans []LogicalPlan

	for _, part := range cc.Pattern {
		// Start node.
		if len(part.Chain) == 0 {
			// Pure node CREATE.
			nodePlan := &CreateNodePlan{
				Variable: part.Start.Variable,
				Labels:   part.Start.Labels,
				Props:    planPropsMap(part.Start.Props),
			}
			// If the variable is named, add it to scope so subsequent
			// CREATE relationship clauses can reference it.
			if part.Start.Variable != "" {
				alias := ac.nextNode()
				scope.Bind(part.Start.Variable, Binding{
					Alias:  alias,
					Column: alias + ".id",
					Table:  "nodes",
					IsNode: true,
				})
			}
			plans = append(plans, nodePlan)
		} else {
			// Chain: create the start node if it does not yet exist in scope.
			_, startAlreadyBound := scope.Resolve(part.Start.Variable)
			if part.Start.Variable == "" || !startAlreadyBound {
				startAlias := ac.nextNode()
				nodePlan := &CreateNodePlan{
					Variable: part.Start.Variable,
					Labels:   part.Start.Labels,
					Props:    planPropsMap(part.Start.Props),
				}
				if part.Start.Variable != "" {
					scope.Bind(part.Start.Variable, Binding{
						Alias:  startAlias,
						Column: startAlias + ".id",
						Table:  "nodes",
						IsNode: true,
					})
				}
				plans = append(plans, nodePlan)
			}

			currentVar := part.Start.Variable

			for _, hop := range part.Chain {
				// Create the end node if not in scope.
				endVar := hop.Node.Variable
				_, endAlreadyBound := scope.Resolve(endVar)
				if endVar == "" || !endAlreadyBound {
					endAlias := ac.nextNode()
					endNodePlan := &CreateNodePlan{
						Variable: endVar,
						Labels:   hop.Node.Labels,
						Props:    planPropsMap(hop.Node.Props),
					}
					if endVar != "" {
						scope.Bind(endVar, Binding{
							Alias:  endAlias,
							Column: endAlias + ".id",
							Table:  "nodes",
							IsNode: true,
						})
					}
					plans = append(plans, endNodePlan)
				}

				// Create the relationship.
				relPlan := &CreateRelPlan{
					RelVariable: hop.Rel.Variable,
					Type:        firstRelType(hop.Rel.Types),
					StartVar:    currentVar,
					EndVar:      endVar,
					Props:       planPropsMap(hop.Rel.Props),
				}
				plans = append(plans, relPlan)
				currentVar = endVar
			}
		}
	}

	return plans, nil
}

// ─── SET clause planning ──────────────────────────────────────────────────────

func planSetClause(sc *SetClause, scope *BindingScope) ([]LogicalPlan, error) {
	var plans []LogicalPlan
	for _, item := range sc.Items {
		valueExpr, err := parseExprText(item.ExprText, scope)
		if err != nil {
			return nil, fmt.Errorf("cypher: SET %s.%s: %w", item.Variable, item.Property, err)
		}
		plans = append(plans, &SetPropPlan{
			Variable: item.Variable,
			Property: item.Property,
			Value:    valueExpr,
		})
	}
	return plans, nil
}

// ─── DELETE clause planning ───────────────────────────────────────────────────

func planDeleteClause(dc *DeleteClause, scope *BindingScope) ([]LogicalPlan, error) {
	var plans []LogicalPlan
	for _, exprText := range dc.Exprs {
		varName := strings.TrimSpace(exprText)
		if b, ok := scope.Resolve(varName); ok {
			if b.IsRel {
				plans = append(plans, &DeleteRelPlan{Variable: varName})
				continue
			}
		}
		// Default: treat as a node deletion. The translator will check
		// whether the variable is a node or a relationship at runtime.
		plans = append(plans, &DeleteNodePlan{
			Variable: varName,
			Detach:   dc.Detach,
		})
	}
	return plans, nil
}

// ─── expression text parser ───────────────────────────────────────────────────

// parseExprText converts a raw expression text string (as produced by the
// parser's exprText() helper) into a typed Expr node.
//
// The function handles the common cases needed by the planner:
//   - "n.prop"              → PropExpr
//   - "n" (bare variable)  → VarExpr
//   - "$param"             → ParamRef
//   - string literal       → LiteralExpr (string)
//   - integer literal      → LiteralExpr (int64)
//   - float literal        → LiteralExpr (float64)
//   - "true" / "false"     → LiteralExpr (bool)
//   - "null"               → LiteralExpr (nil)
//   - anything else        → RawExpr (deferred to translator)
//
// The scope is used to distinguish bare variable references from unknown tokens.
func parseExprText(text string, scope *BindingScope) (Expr, error) {
	text = strings.TrimSpace(text)

	if text == "" {
		return &RawExpr{Text: text}, nil
	}

	// $param reference.
	if name, ok := strings.CutPrefix(text, "$"); ok {
		return &ParamRef{Name: name}, nil
	}

	// String literal: 'value' or "value".
	if (strings.HasPrefix(text, "'") && strings.HasSuffix(text, "'")) ||
		(strings.HasPrefix(text, `"`) && strings.HasSuffix(text, `"`)) {
		inner := text[1 : len(text)-1]
		// Unescape doubled quotes.
		inner = strings.ReplaceAll(inner, "''", "'")
		inner = strings.ReplaceAll(inner, `""`, `"`)
		return &LiteralExpr{Value: inner}, nil
	}

	// Boolean literals.
	lower := strings.ToLower(text)
	if lower == "true" {
		return &LiteralExpr{Value: true}, nil
	}
	if lower == "false" {
		return &LiteralExpr{Value: false}, nil
	}
	if lower == "null" {
		return &LiteralExpr{Value: nil}, nil
	}

	// Integer literal.
	if i, err := strconv.ParseInt(text, 10, 64); err == nil {
		return &LiteralExpr{Value: i}, nil
	}

	// Float literal.
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return &LiteralExpr{Value: f}, nil
	}

	// Property access: "n.prop" (exactly one dot, no spaces).
	if idx := strings.Index(text, "."); idx > 0 && !strings.Contains(text, " ") {
		varPart := text[:idx]
		propPart := text[idx+1:]
		// Only treat as PropExpr if varPart looks like a simple identifier.
		if isIdentifier(varPart) && isIdentifier(propPart) {
			return &PropExpr{Variable: varPart, Property: propPart}, nil
		}
	}

	// Bare variable reference — only if the variable is in scope.
	if isIdentifier(text) {
		if _, ok := scope.Resolve(text); ok {
			return &VarExpr{Name: text}, nil
		}
	}

	// Fall back to RawExpr for complex expressions (WHERE sub-expressions,
	// function calls, arithmetic, etc.). Task-008 will add typed parsing
	// for WHERE predicates.
	return &RawExpr{Text: text}, nil
}

// isIdentifier returns true if s looks like a simple Cypher/SQL identifier:
// starts with a letter or underscore, followed by letters, digits, or underscores.
func isIdentifier(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i, c := range s {
		if i == 0 {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
				return false
			}
		} else {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
				return false
			}
		}
	}
	return true
}

// ─── property map helpers ─────────────────────────────────────────────────────

// planPropsMap converts a raw-text property map (from the AST) into a typed
// Expr map. For v0.1 the values are simple: string/number literals, param refs,
// or RawExpr for complex expressions.
func planPropsMap(raw map[string]string) map[string]Expr {
	if len(raw) == 0 {
		return nil
	}
	result := make(map[string]Expr, len(raw))
	for k, v := range raw {
		// The "$" sentinel key is used for whole-properties parameter references
		// (e.g. MATCH (n $param)); leave it as-is for the translator.
		if k == "$" {
			result[k] = &ParamRef{Name: strings.TrimPrefix(v, "$")}
			continue
		}
		// Use a minimal scope for property value expressions (no variables in scope
		// for inline props; they are plain literals or param refs).
		emptyScope := NewScope()
		expr, _ := parseExprText(v, emptyScope)
		result[k] = expr
	}
	return result
}

// firstRelType returns the first relationship type from a list, or "" if empty.
func firstRelType(types []string) string {
	if len(types) == 0 {
		return ""
	}
	return types[0]
}
