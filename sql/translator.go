// Package sql translates logical plan trees into parameterised SQL strings
// for execution against the storage layer.
//
// The Translator type is the main entry point. It walks a cypher.LogicalPlan
// tree and emits a complete SQL SELECT (or INSERT/UPDATE/DELETE for write
// operations) together with an ordered []any argument slice.
//
// The Dialect interface abstracts all database-specific SQL fragments, so the
// same translator code can target SQLite, DuckDB, or PostgreSQL by swapping
// the dialect.
package sql

import (
	"fmt"
	"strings"

	"github.com/LackOfMorals/graphlite/cypher"
)

// Result carries the output of a single translation pass.
type Result struct {
	// SQL is the fully formed SQL statement, with "?" placeholders.
	SQL string
	// Args is the ordered slice of values to bind to the placeholders.
	Args []any
}

// Translator converts a cypher.LogicalPlan tree into SQL.
//
// A single Translator is used for one query translation; it is NOT safe for
// concurrent use. Create a new Translator per query.
type Translator struct {
	dialect Dialect
	// args accumulates SQL bind arguments in the order they appear.
	args []any
}

// NewTranslator creates a Translator that uses the given Dialect.
func NewTranslator(d Dialect) *Translator {
	return &Translator{dialect: d}
}

// Translate walks plan and returns the SQL string and argument slice.
// It returns an error when the plan contains unsupported constructs.
func (t *Translator) Translate(plan cypher.LogicalPlan, scope *cypher.BindingScope) (Result, error) {
	sql, err := t.translatePlan(plan, scope)
	if err != nil {
		return Result{}, err
	}
	return Result{SQL: sql, Args: t.args}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Top-level dispatch
// ─────────────────────────────────────────────────────────────────────────────

func (t *Translator) translatePlan(plan cypher.LogicalPlan, scope *cypher.BindingScope) (string, error) {
	switch p := plan.(type) {
	case *cypher.ReturnPlan:
		return t.translateReturnPlan(p, scope)
	case *cypher.MatchNodePlan:
		// A standalone MATCH node plan (no RETURN) — not typical for queries but
		// handle defensively by wrapping in a SELECT *.
		return t.translateStandaloneMatch(p, scope)
	case *cypher.FilterPlan:
		// FilterPlan wrapping a match — produce the WHERE predicate text for
		// the outer query.
		return t.translateStandaloneFilter(p, scope)
	default:
		return "", fmt.Errorf("sql: unsupported top-level plan node %T", plan)
	}
}

// translateReturnPlan handles RETURN plans, which include the full SELECT statement.
func (t *Translator) translateReturnPlan(rp *cypher.ReturnPlan, scope *cypher.BindingScope) (string, error) {
	// Gather FROM / JOIN clauses and WHERE conditions from the source plan.
	fc, err := t.buildFromClause(rp.Source, scope)
	if err != nil {
		return "", err
	}

	// Build SELECT list.
	selectList, err := t.buildSelectList(rp.Projections, scope)
	if err != nil {
		return "", err
	}

	var b strings.Builder

	b.WriteString("SELECT ")
	if rp.Distinct {
		b.WriteString("DISTINCT ")
	}
	b.WriteString(selectList)

	if fc.from != "" {
		b.WriteString(" FROM ")
		b.WriteString(fc.from)
	}
	if fc.joins != "" {
		b.WriteByte(' ')
		b.WriteString(fc.joins)
	}

	// Combine source WHERE conditions with any additional filter.
	whereFragments := fc.whereFragments
	if fc.extraWhere != "" {
		whereFragments = append(whereFragments, fc.extraWhere)
	}
	if len(whereFragments) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(whereFragments, " AND "))
	}

	// ORDER BY.
	if len(rp.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, s := range rp.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			sortSQL, err := t.exprToSQL(s.Expr, scope)
			if err != nil {
				return "", fmt.Errorf("sql: ORDER BY expr: %w", err)
			}
			b.WriteString(sortSQL)
			if s.Descending {
				b.WriteString(" DESC")
			} else {
				b.WriteString(" ASC")
			}
		}
	}

	// LIMIT / SKIP (SQLite: LIMIT n OFFSET m).
	if rp.Limit != nil {
		b.WriteString(fmt.Sprintf(" LIMIT %d", *rp.Limit))
	}
	if rp.Skip != nil {
		b.WriteString(fmt.Sprintf(" OFFSET %d", *rp.Skip))
	}

	return b.String(), nil
}

// translateStandaloneMatch handles a bare MatchNodePlan at the top level
// (e.g. when there is no RETURN clause). Selects all columns.
func (t *Translator) translateStandaloneMatch(mnp *cypher.MatchNodePlan, scope *cypher.BindingScope) (string, error) {
	fc, err := t.buildFromClauseForMatchNode(mnp, scope)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("SELECT " + mnp.SQLAlias + ".id, " + mnp.SQLAlias + ".labels, " + mnp.SQLAlias + ".props")
	b.WriteString(" FROM " + fc.from)
	if len(fc.whereFragments) > 0 {
		b.WriteString(" WHERE " + strings.Join(fc.whereFragments, " AND "))
	}
	return b.String(), nil
}

// translateStandaloneFilter handles a FilterPlan at the top level.
func (t *Translator) translateStandaloneFilter(fp *cypher.FilterPlan, scope *cypher.BindingScope) (string, error) {
	// Build from the inner source, then add the filter.
	fc, err := t.buildFromClause(fp.Source, scope)
	if err != nil {
		return "", err
	}
	predSQL, err := t.exprToSQL(fp.Predicate, scope)
	if err != nil {
		return "", fmt.Errorf("sql: WHERE predicate: %w", err)
	}
	// Use a fresh slice to avoid aliasing fc.whereFragments' underlying array.
	allWhere := append(append([]string(nil), fc.whereFragments...), predSQL)

	var b strings.Builder
	b.WriteString("SELECT *")
	if fc.from != "" {
		b.WriteString(" FROM " + fc.from)
	}
	if fc.joins != "" {
		b.WriteString(" " + fc.joins)
	}
	if len(allWhere) > 0 {
		b.WriteString(" WHERE " + strings.Join(allWhere, " AND "))
	}
	return b.String(), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// FROM / JOIN clause builder
// ─────────────────────────────────────────────────────────────────────────────

// fromClause collects all the SQL fragments needed by the outer SELECT.
type fromClause struct {
	// from is the primary table expression (e.g. "nodes n0").
	from string
	// joins is the accumulated JOIN clauses (e.g. "JOIN edges r0 ON ...").
	joins string
	// whereFragments are WHERE conditions accumulated from label checks and
	// inline property constraints on match nodes.
	whereFragments []string
	// extraWhere is a WHERE predicate coming from an explicit FilterPlan.
	extraWhere string
}

// buildFromClause recurses into the source plan and collects the FROM/JOIN/WHERE
// fragments needed to realise the query.
func (t *Translator) buildFromClause(source cypher.LogicalPlan, scope *cypher.BindingScope) (fromClause, error) {
	if source == nil {
		return fromClause{}, nil
	}

	switch s := source.(type) {
	case *cypher.MatchNodePlan:
		return t.buildFromClauseForMatchNode(s, scope)

	case *cypher.MatchRelPlan:
		return t.buildFromClauseForMatchRel(s, scope)

	case *cypher.FilterPlan:
		// Recurse into the source, then accumulate the WHERE predicate.
		fc, err := t.buildFromClause(s.Source, scope)
		if err != nil {
			return fromClause{}, err
		}
		predSQL, err := t.exprToSQL(s.Predicate, scope)
		if err != nil {
			return fromClause{}, fmt.Errorf("sql: WHERE predicate: %w", err)
		}
		fc.extraWhere = predSQL
		return fc, nil

	case *cypher.SequencePlan:
		return t.buildFromClauseForSequence(s, scope)

	default:
		return fromClause{}, fmt.Errorf("sql: unsupported source plan %T in FROM clause", source)
	}
}

// buildFromClauseForMatchNode handles a MatchNodePlan.
func (t *Translator) buildFromClauseForMatchNode(mnp *cypher.MatchNodePlan, scope *cypher.BindingScope) (fromClause, error) {
	alias := mnp.SQLAlias
	fc := fromClause{
		from: "nodes " + alias,
	}

	// Label constraints.
	for _, label := range mnp.Labels {
		pred, args := t.dialect.LabelContains(alias+".labels", label)
		fc.whereFragments = append(fc.whereFragments, pred)
		t.args = append(t.args, args...)
	}

	// Inline property constraints.
	for key, expr := range mnp.Props {
		valSQL, err := t.exprToSQL(expr, scope)
		if err != nil {
			return fromClause{}, fmt.Errorf("sql: node prop constraint %q: %w", key, err)
		}
		jsonExpr := t.dialect.JSONExtract(alias+".props", "$."+key)
		fc.whereFragments = append(fc.whereFragments, jsonExpr+" = "+valSQL)
	}

	return fc, nil
}

// buildFromClauseForMatchRel handles a MatchRelPlan (single-hop or part of a
// multi-hop chain).
func (t *Translator) buildFromClauseForMatchRel(mrp *cypher.MatchRelPlan, scope *cypher.BindingScope) (fromClause, error) {
	startAlias := mrp.StartNode.SQLAlias
	// If StartNode has no alias (e.g. start variable already bound in a prior hop),
	// fall back to resolving from the scope.
	if startAlias == "" && mrp.StartVar != "" {
		startBinding, ok := scope.Resolve(mrp.StartVar)
		if !ok {
			return fromClause{}, fmt.Errorf("sql: start variable %q not in scope", mrp.StartVar)
		}
		startAlias = startBinding.Alias
	}

	endAlias := mrp.EndNode.SQLAlias
	relAlias := mrp.RelSQLAlias

	// Build the primary FROM as "nodes <startAlias>" and JOIN edges + end nodes.
	from := "nodes " + startAlias

	var joinParts []string
	var whereParts []string

	// Start node label constraints.
	for _, label := range mrp.StartNode.Labels {
		pred, args := t.dialect.LabelContains(startAlias+".labels", label)
		whereParts = append(whereParts, pred)
		t.args = append(t.args, args...)
	}

	// Start node inline property constraints.
	for key, expr := range mrp.StartNode.Props {
		valSQL, err := t.exprToSQL(expr, scope)
		if err != nil {
			return fromClause{}, fmt.Errorf("sql: start node prop constraint %q: %w", key, err)
		}
		jsonExpr := t.dialect.JSONExtract(startAlias+".props", "$."+key)
		whereParts = append(whereParts, jsonExpr+" = "+valSQL)
	}

	// JOIN edges table.
	edgeJoinKind := "JOIN"
	if mrp.Optional {
		edgeJoinKind = "LEFT JOIN"
	}
	edgeJoin := fmt.Sprintf("%s edges %s ON ", edgeJoinKind, relAlias)
	if mrp.Undirected {
		// Undirected: match either direction.
		edgeJoin += fmt.Sprintf("(%s.start_id = %s.id OR %s.end_id = %s.id)",
			relAlias, startAlias, relAlias, startAlias)
	} else if mrp.ToRight {
		edgeJoin += fmt.Sprintf("%s.start_id = %s.id", relAlias, startAlias)
	} else {
		// ToLeft: <-[r]-
		edgeJoin += fmt.Sprintf("%s.end_id = %s.id", relAlias, startAlias)
	}
	joinParts = append(joinParts, edgeJoin)

	// JOIN end node.
	nodeJoinKind := "JOIN"
	if mrp.Optional {
		nodeJoinKind = "LEFT JOIN"
	}
	nodeJoin := fmt.Sprintf("%s nodes %s ON ", nodeJoinKind, endAlias)
	if mrp.Undirected {
		nodeJoin += fmt.Sprintf("(%s.id = CASE WHEN %s.start_id = %s.id THEN %s.end_id ELSE %s.start_id END)",
			endAlias, relAlias, startAlias, relAlias, relAlias)
	} else if mrp.ToRight {
		nodeJoin += fmt.Sprintf("%s.id = %s.end_id", endAlias, relAlias)
	} else {
		nodeJoin += fmt.Sprintf("%s.id = %s.start_id", endAlias, relAlias)
	}
	joinParts = append(joinParts, nodeJoin)

	// Relationship type constraints.
	for _, relType := range mrp.Types {
		whereParts = append(whereParts, relAlias+".type = ?")
		t.args = append(t.args, relType)
	}

	// Relationship inline property constraints.
	for key, expr := range mrp.RelProps {
		valSQL, err := t.exprToSQL(expr, scope)
		if err != nil {
			return fromClause{}, fmt.Errorf("sql: rel prop constraint %q: %w", key, err)
		}
		jsonExpr := t.dialect.JSONExtract(relAlias+".props", "$."+key)
		whereParts = append(whereParts, jsonExpr+" = "+valSQL)
	}

	// End node label constraints.
	for _, label := range mrp.EndNode.Labels {
		pred, args := t.dialect.LabelContains(endAlias+".labels", label)
		whereParts = append(whereParts, pred)
		t.args = append(t.args, args...)
	}

	// End node inline property constraints.
	for key, expr := range mrp.EndNode.Props {
		valSQL, err := t.exprToSQL(expr, scope)
		if err != nil {
			return fromClause{}, fmt.Errorf("sql: end node prop constraint %q: %w", key, err)
		}
		jsonExpr := t.dialect.JSONExtract(endAlias+".props", "$."+key)
		whereParts = append(whereParts, jsonExpr+" = "+valSQL)
	}

	fc := fromClause{
		from:           from,
		joins:          strings.Join(joinParts, " "),
		whereFragments: whereParts,
	}
	return fc, nil
}

// buildFromClauseForSequence handles a SequencePlan that appears in the source
// position (a chain of match plans for multi-hop queries).
func (t *Translator) buildFromClauseForSequence(sp *cypher.SequencePlan, scope *cypher.BindingScope) (fromClause, error) {
	if len(sp.Steps) == 0 {
		return fromClause{}, fmt.Errorf("sql: empty SequencePlan")
	}

	// The first step defines the primary FROM table; subsequent steps add JOINs.
	baseFC, err := t.buildFromClause(sp.Steps[0], scope)
	if err != nil {
		return fromClause{}, err
	}

	// Promote any extraWhere from the first step so it is surfaced at the top
	// level (e.g. a FilterPlan wrapping the first hop contributes a WHERE fragment).
	extraWhere := baseFC.extraWhere
	baseFC.extraWhere = ""

	allWhere := baseFC.whereFragments
	allJoins := baseFC.joins

	// Process the remaining steps (additional MATCH hops).
	for _, step := range sp.Steps[1:] {
		fc, err := t.buildFromClause(step, scope)
		if err != nil {
			return fromClause{}, err
		}
		// Each subsequent hop appends JOINs and WHERE conditions.
		if fc.joins != "" {
			if allJoins != "" {
				allJoins += " " + fc.joins
			} else {
				allJoins = fc.joins
			}
		}
		allWhere = append(allWhere, fc.whereFragments...)
		if fc.extraWhere != "" {
			allWhere = append(allWhere, fc.extraWhere)
		}
	}

	return fromClause{
		from:           baseFC.from,
		joins:          allJoins,
		whereFragments: allWhere,
		extraWhere:     extraWhere,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// SELECT list builder
// ─────────────────────────────────────────────────────────────────────────────

// buildSelectList converts the ReturnPlan's projection items into a SQL
// SELECT column list.
func (t *Translator) buildSelectList(projections []cypher.ProjectionItem, scope *cypher.BindingScope) (string, error) {
	if len(projections) == 0 {
		return "*", nil
	}

	parts := make([]string, 0, len(projections))
	for _, proj := range projections {
		colSQL, err := t.exprToSQL(proj.Expr, scope)
		if err != nil {
			return "", fmt.Errorf("sql: SELECT projection: %w", err)
		}
		if proj.Alias != "" {
			colSQL += " AS " + proj.Alias
		}
		parts = append(parts, colSQL)
	}
	return strings.Join(parts, ", "), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Expression → SQL fragment conversion
// ─────────────────────────────────────────────────────────────────────────────

// exprToSQL converts a cypher.Expr node to a SQL fragment. It appends any
// required bind arguments to t.args.
func (t *Translator) exprToSQL(expr cypher.Expr, scope *cypher.BindingScope) (string, error) {
	if expr == nil {
		return "NULL", nil
	}

	switch e := expr.(type) {
	case *cypher.LiteralExpr:
		return t.literalToSQL(e)

	case *cypher.ParamRef:
		// Named parameter: add a placeholder; the actual value will be supplied at
		// execution time by the parameter binding layer (task-015).
		// We store the param name as a sentinel so task-015 can map it to the
		// caller-supplied map[string]any. For now emit "?" and record the param name.
		t.args = append(t.args, paramSentinel{Name: e.Name})
		return "?", nil

	case *cypher.PropExpr:
		// n.prop → json_extract(<alias>.props, '$.prop')
		binding, ok := scope.Resolve(e.Variable)
		if !ok {
			return "", fmt.Errorf("sql: variable %q not in scope", e.Variable)
		}
		return t.dialect.JSONExtract(binding.Alias+".props", "$."+e.Property), nil

	case *cypher.VarExpr:
		// Whole-variable reference: for a node, emit the id/labels/props JSON object;
		// for a relationship, emit the id.
		binding, ok := scope.Resolve(e.Name)
		if !ok {
			return "", fmt.Errorf("sql: variable %q not in scope", e.Name)
		}
		if binding.IsNode {
			// Return a JSON object representing the node.
			return fmt.Sprintf(
				"json_object('id', %[1]s.id, 'labels', %[1]s.labels, 'props', json(%[1]s.props))",
				binding.Alias,
			), nil
		}
		if binding.IsRel {
			return fmt.Sprintf(
				"json_object('id', %[1]s.id, 'type', %[1]s.type, 'start_id', %[1]s.start_id, 'end_id', %[1]s.end_id, 'props', json(%[1]s.props))",
				binding.Alias,
			), nil
		}
		return binding.Column, nil

	case *cypher.ComparisonExpr:
		leftSQL, err := t.exprToSQL(e.Left, scope)
		if err != nil {
			return "", err
		}
		rightSQL, err := t.exprToSQL(e.Right, scope)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("(%s %s %s)", leftSQL, e.Op, rightSQL), nil

	case *cypher.BoolExpr:
		leftSQL, err := t.exprToSQL(e.Left, scope)
		if err != nil {
			return "", err
		}
		rightSQL, err := t.exprToSQL(e.Right, scope)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("(%s %s %s)", leftSQL, e.Op, rightSQL), nil

	case *cypher.NotExpr:
		innerSQL, err := t.exprToSQL(e.Expr, scope)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("(NOT %s)", innerSQL), nil

	case *cypher.RawExpr:
		// RawExpr: unsupported sub-expression; return as-is (best effort).
		// The translator cannot produce correct SQL for this but should not crash.
		return e.Text, nil

	default:
		return "", fmt.Errorf("sql: unsupported expression type %T", expr)
	}
}

// literalToSQL converts a LiteralExpr to a SQL value. String literals are
// added as bind arguments; numeric and boolean literals are inlined.
func (t *Translator) literalToSQL(e *cypher.LiteralExpr) (string, error) {
	switch v := e.Value.(type) {
	case nil:
		return "NULL", nil
	case bool:
		if v {
			return "1", nil
		}
		return "0", nil
	case int64:
		return fmt.Sprintf("%d", v), nil
	case float64:
		return fmt.Sprintf("%g", v), nil
	case string:
		// Strings use bind parameters to avoid injection.
		t.args = append(t.args, v)
		return "?", nil
	default:
		return "", fmt.Errorf("sql: unsupported literal type %T", e.Value)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Parameter sentinel type
// ─────────────────────────────────────────────────────────────────────────────

// paramSentinel is a placeholder value stored in the args slice to identify
// named Cypher parameters. Task-015 (parameter binding) replaces these
// sentinels with actual values from the caller-supplied map[string]any.
type paramSentinel struct {
	Name string
}
