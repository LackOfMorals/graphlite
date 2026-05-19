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

// StatementKind classifies a Statement so the execution layer can dispatch it
// correctly without parsing the SQL string.
type StatementKind int

const (
	// KindSelect is a read-only SELECT query. The execution layer calls
	// QueryContext and wraps the rows in a QueryResult.
	KindSelect StatementKind = iota
	// KindMatchForWrite is a SELECT emitted as the first step in a MATCH+write
	// sequence. It returns one column per matched variable, named by the Cypher
	// variable name, containing the row's integer id. The execution layer runs
	// this first to populate the idMap before executing write statements.
	KindMatchForWrite
	// KindInsertNode is INSERT INTO nodes. After execution, LastInsertId() is
	// bound to CreatedVar in the execution layer's idMap.
	KindInsertNode
	// KindInsertEdge is INSERT INTO edges.
	KindInsertEdge
	// KindUpdate is UPDATE nodes/edges SET …
	KindUpdate
	// KindDeleteGuard is SELECT COUNT(*) used as a guard before a non-detach node
	// delete. The execution layer aborts if the count is > 0.
	KindDeleteGuard
	// KindDeleteEdges is DELETE FROM edges …
	KindDeleteEdges
	// KindDeleteNodes is DELETE FROM nodes …
	KindDeleteNodes
	// KindExec is any other DML statement.
	KindExec
)

// Statement is a single parameterised SQL statement together with its bind
// arguments. Write operations that require multiple statements (e.g.
// DETACH DELETE, or a compound CREATE) return multiple Statements.
type Statement struct {
	// SQL is a single fully-formed SQL statement, with "?" placeholders.
	SQL string
	// Args is the ordered slice of values to bind to the placeholders.
	Args []any
	// Kind classifies the statement so the execution layer can dispatch without
	// string-matching on SQL prefixes.
	Kind StatementKind
	// CreatedVar is the Cypher variable name whose ID is produced by an INSERT
	// statement (e.g. "n" for CREATE (n:Label)). Empty for non-INSERT statements.
	// The execution layer uses this to map last-insert rowids back to variables
	// so subsequent idSentinel references can be resolved.
	CreatedVar string
	// MatchedVars is populated on KindMatchForWrite statements. Each entry maps
	// a Cypher variable name to the column alias in the SELECT result that
	// contains its integer id. The execution layer scans each row and populates
	// idMap[varName] = id for every row returned.
	MatchedVars map[string]string
}

// Result carries the output of a single translation pass.
//
// For read queries (SELECT) there is always exactly one Statement, and the
// top-level SQL/Args fields mirror it for convenience.
//
// For write queries there may be multiple Statements; the SQL and Args fields
// contain the first statement only — callers should iterate Statements for
// multi-statement write operations.
type Result struct {
	// SQL is the fully formed SQL statement, with "?" placeholders.
	// For multi-statement write plans this is the first statement's SQL.
	SQL string
	// Args is the ordered slice of values to bind to the placeholders.
	// For multi-statement write plans this is the first statement's args.
	Args []any
	// Statements carries all generated SQL statements in execution order.
	// Single-statement results always have len(Statements) == 1.
	Statements []Statement
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
//
// For read plans the result contains a single SQL SELECT statement.
// For write plans (CREATE, SET, DELETE, SequencePlan of write ops) the result
// may contain multiple Statements; the caller must execute them all in order.
func (t *Translator) Translate(plan cypher.LogicalPlan, scope *cypher.BindingScope) (Result, error) {
	// Try write-plan translation first (handles CREATE/SET/DELETE/SequencePlan).
	stmts, handled, err := t.translateWritePlan(plan, scope)
	if err != nil {
		return Result{}, err
	}
	if handled {
		if len(stmts) == 0 {
			return Result{}, fmt.Errorf("sql: write plan produced no statements")
		}
		return Result{
			SQL:        stmts[0].SQL,
			Args:       stmts[0].Args,
			Statements: stmts,
		}, nil
	}

	// Fall back to read-plan translation (SELECT queries).
	sqlStr, err := t.translatePlan(plan, scope)
	if err != nil {
		return Result{}, err
	}
	stmt := Statement{SQL: sqlStr, Args: t.args, Kind: KindSelect}
	return Result{SQL: sqlStr, Args: t.args, Statements: []Statement{stmt}}, nil
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
		fmt.Fprintf(&b, " LIMIT %d", *rp.Limit)
	}
	if rp.Skip != nil {
		fmt.Fprintf(&b, " OFFSET %d", *rp.Skip)
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
		alias := proj.Alias
		// When no explicit alias is given, use the variable name for VarExpr and
		// PropExpr projections so the result column is named predictably.
		if alias == "" {
			switch e := proj.Expr.(type) {
			case *cypher.VarExpr:
				alias = e.Name
			case *cypher.PropExpr:
				// Use underscore separator to produce a valid SQL identifier
				// (dot is not valid in unquoted column aliases).
				alias = e.Variable + "_" + e.Property
			}
		}
		if alias != "" {
			colSQL += " AS " + alias
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
// Parameter sentinel type and named parameter binding
// ─────────────────────────────────────────────────────────────────────────────

// paramSentinel is a placeholder value stored in the args slice to identify
// named Cypher parameters. BindParams replaces these sentinels with actual
// values from the caller-supplied map[string]any.
type paramSentinel struct {
	Name string
}

// ErrMissingParam is returned by BindParams when a parameterised query
// references a $paramName that is not present in the caller-supplied params map.
type ErrMissingParam struct {
	// Name is the parameter name that was expected but not provided.
	Name string
}

// Error implements the error interface.
func (e *ErrMissingParam) Error() string {
	return fmt.Sprintf("sql: missing query parameter $%s", e.Name)
}

// BindParams resolves all named parameter sentinels in result's Statements
// against the provided params map and returns a new Result with concrete values
// in every Args slice. The idSentinel values (write-operation node/rel IDs) are
// left untouched — they are resolved by the execution layer at runtime.
//
// If any $paramName referenced by the query is absent from params, BindParams
// returns a *ErrMissingParam error and leaves result unchanged.
//
// params may be nil or empty, in which case any paramSentinel in result causes
// an error.
func BindParams(result Result, params map[string]any) (Result, error) {
	// Fast path: nothing to do when params is empty and there are no sentinels.
	// We still do a full scan to catch any undeclared sentinel references.
	newStmts := make([]Statement, len(result.Statements))
	for i, stmt := range result.Statements {
		resolvedArgs, err := resolveArgs(stmt.Args, params)
		if err != nil {
			return result, err
		}
		newStmts[i] = Statement{
			SQL:         stmt.SQL,
			Args:        resolvedArgs,
			Kind:        stmt.Kind,
			CreatedVar:  stmt.CreatedVar,
			MatchedVars: stmt.MatchedVars,
		}
	}
	out := Result{
		Statements: newStmts,
	}
	if len(newStmts) > 0 {
		out.SQL = newStmts[0].SQL
		out.Args = newStmts[0].Args
	}
	return out, nil
}

// resolveArgs walks an args slice and replaces every paramSentinel with the
// corresponding value from params. idSentinel values are preserved unchanged.
func resolveArgs(args []any, params map[string]any) ([]any, error) {
	if len(args) == 0 {
		return args, nil
	}
	// Allocate a new slice so the original Result is not mutated.
	out := make([]any, len(args))
	for i, a := range args {
		if ps, ok := a.(paramSentinel); ok {
			val, found := params[ps.Name]
			if !found {
				return nil, &ErrMissingParam{Name: ps.Name}
			}
			out[i] = val
		} else {
			// Preserve plain values and idSentinel values unchanged.
			out[i] = a
		}
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Write-plan translation
// ─────────────────────────────────────────────────────────────────────────────

// translateWritePlan attempts to translate a write-operation plan node.
// If the plan is a write plan it sets handled=true and returns the Statements.
// If the plan is a read plan it returns handled=false (the caller falls back to
// the read-plan path).
func (t *Translator) translateWritePlan(plan cypher.LogicalPlan, scope *cypher.BindingScope) (stmts []Statement, handled bool, err error) {
	switch p := plan.(type) {
	case *cypher.CreateNodePlan:
		stmt, e := t.translateCreateNode(p, scope)
		if e != nil {
			return nil, true, e
		}
		return []Statement{stmt}, true, nil

	case *cypher.CreateRelPlan:
		stmt, e := t.translateCreateRel(p, scope)
		if e != nil {
			return nil, true, e
		}
		return []Statement{stmt}, true, nil

	case *cypher.SetPropPlan:
		stmt, e := t.translateSetProp(p, scope)
		if e != nil {
			return nil, true, e
		}
		return []Statement{stmt}, true, nil

	case *cypher.DeleteNodePlan:
		ss, e := t.translateDeleteNode(p, scope)
		if e != nil {
			return nil, true, e
		}
		return ss, true, nil

	case *cypher.DeleteRelPlan:
		stmt, e := t.translateDeleteRel(p, scope)
		if e != nil {
			return nil, true, e
		}
		return []Statement{stmt}, true, nil

	case *cypher.SequencePlan:
		ss, handled2, e := t.translateSequenceWrite(p, scope)
		if e != nil {
			return nil, handled2, e
		}
		return ss, handled2, nil

	default:
		// Not a write plan.
		return nil, false, nil
	}
}

// translateCreateNode emits:
//
//	INSERT INTO nodes (labels, props) VALUES (?, json(?))
//
// Labels are joined as comma-separated text. Property values are encoded as a
// JSON object. The returned Statement uses a fresh args slice.
func (t *Translator) translateCreateNode(p *cypher.CreateNodePlan, scope *cypher.BindingScope) (Statement, error) {
	// Build the comma-separated labels string.
	labels := strings.Join(p.Labels, ",")

	// Build the JSON props object: {"key": value, ...}
	// Property values that are string literals use bind args; others are inlined.
	// We collect them as a JSON object literal using json() to validate at insert time.
	propsJSON, propsArgs, err := t.buildPropsJSON(p.Props, scope)
	if err != nil {
		return Statement{}, fmt.Errorf("sql: CREATE node props: %w", err)
	}

	var args []any
	args = append(args, labels)
	args = append(args, propsArgs...)

	sql := fmt.Sprintf("INSERT INTO nodes (labels, props) VALUES (?, json(%s))", propsJSON)
	return Statement{SQL: sql, Args: args, Kind: KindInsertNode, CreatedVar: p.Variable}, nil
}

// translateCreateRel emits:
//
//	INSERT INTO edges (type, start_id, end_id, props) VALUES (?, ?, ?, json(?))
//
// The start and end node ID positions in Args hold idSentinel values (not
// literal int64s). The execution layer (task-016) resolves each idSentinel to
// an actual int64 by looking up the variable's last-insert rowid or querying
// the scope. The relationship type and props are bound normally.
func (t *Translator) translateCreateRel(p *cypher.CreateRelPlan, scope *cypher.BindingScope) (Statement, error) {
	if p.Type == "" {
		return Statement{}, fmt.Errorf("sql: CREATE relationship requires a type")
	}

	// Resolve start and end node aliases from scope.
	startBinding, ok := scope.Resolve(p.StartVar)
	if !ok {
		return Statement{}, fmt.Errorf("sql: start variable %q not in scope for CREATE relationship", p.StartVar)
	}
	if !startBinding.IsNode {
		return Statement{}, fmt.Errorf("sql: start variable %q is not a node (got rel=%v)", p.StartVar, startBinding.IsRel)
	}
	endBinding, ok := scope.Resolve(p.EndVar)
	if !ok {
		return Statement{}, fmt.Errorf("sql: end variable %q not in scope for CREATE relationship", p.EndVar)
	}
	if !endBinding.IsNode {
		return Statement{}, fmt.Errorf("sql: end variable %q is not a node (got rel=%v)", p.EndVar, endBinding.IsRel)
	}

	// Build the JSON props object.
	propsJSON, propsArgs, err := t.buildPropsJSON(p.Props, scope)
	if err != nil {
		return Statement{}, fmt.Errorf("sql: CREATE relationship props: %w", err)
	}

	// Args layout: [type, idSentinel(start), idSentinel(end), ...propsArgs]
	// The two idSentinels are replaced by the execution layer with actual int64 IDs.
	var args []any
	args = append(args, p.Type)
	args = append(args, idSentinel{VarName: p.StartVar, Alias: startBinding.Alias})
	args = append(args, idSentinel{VarName: p.EndVar, Alias: endBinding.Alias})
	args = append(args, propsArgs...)

	sql := fmt.Sprintf("INSERT INTO edges (type, start_id, end_id, props) VALUES (?, ?, ?, json(%s))", propsJSON)
	return Statement{SQL: sql, Args: args, Kind: KindInsertEdge}, nil
}

// translateSetProp emits:
//
//	UPDATE nodes SET props = json_set(props, '$.prop', ?) WHERE id = ?
//
// The node ID is provided as an idSentinel so the execution layer resolves it
// from the BindingScope at runtime.
//
// For relationship variables the table is "edges" instead of "nodes".
func (t *Translator) translateSetProp(p *cypher.SetPropPlan, scope *cypher.BindingScope) (Statement, error) {
	binding, ok := scope.Resolve(p.Variable)
	if !ok {
		return Statement{}, fmt.Errorf("sql: variable %q not in scope for SET", p.Variable)
	}

	// Build the value SQL fragment (with its own fresh arg list).
	valTranslator := &Translator{dialect: t.dialect}
	valSQL, err := valTranslator.exprToSQL(p.Value, scope)
	if err != nil {
		return Statement{}, fmt.Errorf("sql: SET value expr: %w", err)
	}

	// In an UPDATE statement the column reference is unqualified ("props", not "n0.props").
	jsonSetExpr := t.dialect.JSONSet("props", "$."+p.Property, valSQL)

	table := "nodes"
	if binding.IsRel {
		table = "edges"
	}

	var args []any
	args = append(args, valTranslator.args...)                                  // value bind args (if any)
	args = append(args, idSentinel{VarName: p.Variable, Alias: binding.Alias}) // WHERE id = ?

	sql := fmt.Sprintf("UPDATE %s SET props = %s WHERE id = ?", table, jsonSetExpr)
	return Statement{SQL: sql, Args: args, Kind: KindUpdate}, nil
}

// translateDeleteNode emits the SQL for DELETE / DETACH DELETE of a node.
//
// Non-detach:
//
//	The translator emits a guard check (SELECT COUNT(*) FROM edges WHERE …)
//	followed by the DELETE. The execution layer must abort if the guard returns
//	a non-zero count. We represent this as two Statements:
//	  [0] guard: SELECT COUNT(*) FROM edges WHERE start_id = ? OR end_id = ?
//	  [1] delete: DELETE FROM nodes WHERE id = ?
//
// Detach (DETACH DELETE):
//
//	Two Statements:
//	  [0] DELETE FROM edges WHERE start_id = ? OR end_id = ?
//	  [1] DELETE FROM nodes WHERE id = ?
func (t *Translator) translateDeleteNode(p *cypher.DeleteNodePlan, scope *cypher.BindingScope) ([]Statement, error) {
	binding, ok := scope.Resolve(p.Variable)
	if !ok {
		return nil, fmt.Errorf("sql: variable %q not in scope for DELETE", p.Variable)
	}

	sentinel := idSentinel{VarName: p.Variable, Alias: binding.Alias}

	if p.Detach {
		// DETACH DELETE: remove edges first, then the node.
		edgesStmt := Statement{
			SQL:  "DELETE FROM edges WHERE start_id = ? OR end_id = ?",
			Args: []any{sentinel, sentinel},
			Kind: KindDeleteEdges,
		}
		nodeStmt := Statement{
			SQL:  "DELETE FROM nodes WHERE id = ?",
			Args: []any{sentinel},
			Kind: KindDeleteNodes,
		}
		return []Statement{edgesStmt, nodeStmt}, nil
	}

	// Non-detach: guard check + delete.
	guardStmt := Statement{
		SQL:  "SELECT COUNT(*) FROM edges WHERE start_id = ? OR end_id = ?",
		Args: []any{sentinel, sentinel},
		Kind: KindDeleteGuard,
	}
	nodeStmt := Statement{
		SQL:  "DELETE FROM nodes WHERE id = ?",
		Args: []any{sentinel},
		Kind: KindDeleteNodes,
	}
	return []Statement{guardStmt, nodeStmt}, nil
}

// translateDeleteRel emits:
//
//	DELETE FROM edges WHERE id = ?
func (t *Translator) translateDeleteRel(p *cypher.DeleteRelPlan, scope *cypher.BindingScope) (Statement, error) {
	binding, ok := scope.Resolve(p.Variable)
	if !ok {
		return Statement{}, fmt.Errorf("sql: variable %q not in scope for DELETE relationship", p.Variable)
	}

	sentinel := idSentinel{VarName: p.Variable, Alias: binding.Alias}
	return Statement{
		SQL:  "DELETE FROM edges WHERE id = ?",
		Args: []any{sentinel},
		Kind: KindDeleteEdges,
	}, nil
}

// translateSequenceWrite handles a SequencePlan that contains write operations.
//
// If all steps are read plans it returns handled=false so the caller falls back
// to the read path.
//
// When there are both read steps (MATCH/FILTER) and write steps, it first emits
// a KindMatchForWrite SELECT that returns the id of every named variable in
// scope (so the execution layer can populate idMap before running the write
// statements), then appends the write Statements.
func (t *Translator) translateSequenceWrite(sp *cypher.SequencePlan, scope *cypher.BindingScope) ([]Statement, bool, error) {
	var writeStmts []Statement
	var readSteps []cypher.LogicalPlan
	anyWrite := false

	for _, step := range sp.Steps {
		stmts, handled, err := t.translateWritePlan(step, scope)
		if err != nil {
			return nil, true, err
		}
		if !handled {
			// Read step (MATCH, FILTER) preceding write steps.
			readSteps = append(readSteps, step)
			continue
		}
		anyWrite = true
		writeStmts = append(writeStmts, stmts...)
	}

	if !anyWrite {
		// All steps were read plans — this is a read sequence, not a write sequence.
		return nil, false, nil
	}

	var allStmts []Statement

	// If there are read steps, emit a KindMatchForWrite SELECT first so the
	// execution layer can map variable names to integer ids.
	if len(readSteps) > 0 {
		matchStmt, err := t.buildMatchForWriteSelect(readSteps, scope)
		if err != nil {
			return nil, true, err
		}
		allStmts = append(allStmts, matchStmt)
	}

	allStmts = append(allStmts, writeStmts...)
	return allStmts, true, nil
}

// buildMatchForWriteSelect generates a SELECT that returns the integer id of
// every named variable visible in scope after running the given read steps.
// The result columns are named by the Cypher variable name (e.g. "n", "r").
// The execution layer scans this SELECT row-by-row and, for each row, runs the
// subsequent write statements with the resolved IDs.
func (t *Translator) buildMatchForWriteSelect(readSteps []cypher.LogicalPlan, scope *cypher.BindingScope) (Statement, error) {
	// Build a SequencePlan of the read steps and use buildFromClause to get
	// the FROM/JOIN/WHERE fragments.
	var src cypher.LogicalPlan
	if len(readSteps) == 1 {
		src = readSteps[0]
	} else {
		src = &cypher.SequencePlan{Steps: readSteps}
	}

	// Use a fresh Translator so we don't pollute t.args (the match SELECT has
	// its own parameter bindings that have already been applied by BindParams).
	matchT := &Translator{dialect: t.dialect}
	fc, err := matchT.buildFromClause(src, scope)
	if err != nil {
		return Statement{}, fmt.Errorf("sql: match-for-write FROM clause: %w", err)
	}

	// Build the SELECT list: one "<alias>.id AS <varName>" per named variable.
	// Sort variable names for deterministic column ordering.
	varNames := scope.Names()
	for i := 1; i < len(varNames); i++ {
		for j := i; j > 0 && varNames[j] < varNames[j-1]; j-- {
			varNames[j], varNames[j-1] = varNames[j-1], varNames[j]
		}
	}
	var cols []string
	matchedVars := make(map[string]string) // varName → colAlias (same as varName here)
	for _, varName := range varNames {
		b, ok := scope.Resolve(varName)
		if !ok {
			continue
		}
		col := fmt.Sprintf("%s.id AS %s", b.Alias, varName)
		cols = append(cols, col)
		matchedVars[varName] = varName
	}
	if len(cols) == 0 {
		cols = []string{"1"} // fallback (should not happen)
	}

	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(strings.Join(cols, ", "))
	if fc.from != "" {
		b.WriteString(" FROM ")
		b.WriteString(fc.from)
	}
	if fc.joins != "" {
		b.WriteByte(' ')
		b.WriteString(fc.joins)
	}
	all := fc.whereFragments
	if fc.extraWhere != "" {
		all = append(all, fc.extraWhere)
	}
	if len(all) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(all, " AND "))
	}

	return Statement{
		SQL:         b.String(),
		Args:        matchT.args,
		Kind:        KindMatchForWrite,
		MatchedVars: matchedVars,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Props JSON builder
// ─────────────────────────────────────────────────────────────────────────────

// buildPropsJSON builds the SQL fragment and bind args needed to construct a
// JSON object from a map[string]Expr. It returns a SQL expression suitable for
// wrapping in json(…), along with the bind arguments.
//
// For an empty props map it returns ("'{}'", nil) so INSERT always has a valid
// JSON props column.
//
// The returned SQL fragment uses json_object(key1, val1, key2, val2, ...) when
// there are properties, and the string literal '{}' otherwise.
func (t *Translator) buildPropsJSON(props map[string]cypher.Expr, scope *cypher.BindingScope) (string, []any, error) {
	if len(props) == 0 {
		return "'{}'", nil, nil
	}

	// Sort keys for deterministic SQL output (map iteration order is undefined).
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	// Simple insertion sort — props maps are always small.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}

	parts := make([]string, 0, len(keys)*2)
	var args []any
	for _, key := range keys {
		expr := props[key]
		// Use a fresh translator so we can collect the arg values cleanly.
		sub := &Translator{dialect: t.dialect}
		valSQL, err := sub.exprToSQL(expr, scope)
		if err != nil {
			return "", nil, fmt.Errorf("property %q: %w", key, err)
		}
		parts = append(parts, "'"+key+"'", valSQL)
		args = append(args, sub.args...)
	}

	return "json_object(" + strings.Join(parts, ", ") + ")", args, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ID sentinel type and resolution
// ─────────────────────────────────────────────────────────────────────────────

// idSentinel is stored in the Args slice of write Statements in positions where
// a node or relationship integer ID is required. The execution layer (task-016)
// resolves the actual int64 value from prior INSERT results or the BindingScope.
type idSentinel struct {
	// VarName is the Cypher variable name (e.g. "n", "r").
	VarName string
	// Alias is the SQL table alias assigned by the planner (e.g. "n0", "r0").
	Alias string
}

// ResolveIDs replaces every idSentinel in result's Statements with the
// corresponding int64 value from idMap (keyed by Cypher variable name).
// If any sentinel variable is absent from idMap, ResolveIDs returns an error.
// The original Result is never mutated — fresh slices are always allocated.
func ResolveIDs(result Result, idMap map[string]int64) (Result, error) {
	newStmts := make([]Statement, len(result.Statements))
	for i, stmt := range result.Statements {
		resolved, err := resolveIDArgs(stmt.Args, idMap)
		if err != nil {
			return result, err
		}
		newStmts[i] = Statement{
			SQL:         stmt.SQL,
			Args:        resolved,
			Kind:        stmt.Kind,
			CreatedVar:  stmt.CreatedVar,
			MatchedVars: stmt.MatchedVars,
		}
	}
	out := Result{Statements: newStmts}
	if len(newStmts) > 0 {
		out.SQL = newStmts[0].SQL
		out.Args = newStmts[0].Args
	}
	return out, nil
}

// resolveIDArgs replaces idSentinel values in an args slice with int64 IDs.
func resolveIDArgs(args []any, idMap map[string]int64) ([]any, error) {
	if len(args) == 0 {
		return args, nil
	}
	out := make([]any, len(args))
	for i, a := range args {
		if s, ok := a.(idSentinel); ok {
			id, found := idMap[s.VarName]
			if !found {
				return nil, fmt.Errorf("sql: no resolved ID for variable %q (alias %q)", s.VarName, s.Alias)
			}
			out[i] = id
		} else {
			out[i] = a
		}
	}
	return out, nil
}
