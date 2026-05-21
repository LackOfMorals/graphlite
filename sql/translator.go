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
	"sort"
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
	// KindMergeCheck is a SELECT used by MERGE to test whether the target node
	// already exists. The execution layer inspects the result to decide whether
	// to run OnCreate or OnMatch SET statements.
	KindMergeCheck
	// KindMergeInsert is the INSERT INTO nodes statement emitted by MERGE when
	// the node did not previously exist.
	KindMergeInsert
	// KindSelectAfterWrite is a SELECT statement emitted at the end of a write
	// batch (e.g. CREATE … RETURN …). The execution layer resolves any idSentinel
	// values in Args (using the idMap populated by preceding write statements),
	// then runs the SELECT and returns its rows as the query result.
	KindSelectAfterWrite
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
	// Special case: ReturnPlan whose source is a write plan (CREATE … RETURN …).
	// Translate the writes first, then emit a KindSelectAfterWrite SELECT that
	// reads back the created/modified nodes by their idSentinel IDs.
	if rp, ok := plan.(*cypher.ReturnPlan); ok {
		if rp.Source != nil {
			writeStmts, handled, err := t.translateWritePlan(rp.Source, scope)
			if err != nil {
				return Result{}, err
			}
			if handled {
				// Translate the RETURN clause as a SELECT that reads back
				// created nodes from the database using their IDs.
				selectStmt, err := t.translateReturnAfterWrite(rp, scope)
				if err != nil {
					return Result{}, err
				}
				allStmts := append(writeStmts, selectStmt)
				return Result{
					SQL:        allStmts[0].SQL,
					Args:       allStmts[0].Args,
					Statements: allStmts,
				}, nil
			}
		}
	}

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

// translateReturnAfterWrite translates a ReturnPlan that follows write operations
// (CREATE … RETURN …). It emits a KindSelectAfterWrite SELECT that reads back
// the created nodes/relationships from the database using idSentinel values in
// the WHERE clause. The idSentinels are resolved by the execution layer after
// write statements run.
//
// Only variables that appear in the RETURN projections are included in the FROM
// clause to avoid unnecessary table scans.
func (t *Translator) translateReturnAfterWrite(rp *cypher.ReturnPlan, scope *cypher.BindingScope) (Statement, error) {
	// Determine which variables are referenced in the RETURN projections.
	// We only add tables for variables that actually appear in the SELECT list.
	referencedVars := collectReferencedVars(rp.Projections)
	for _, s := range rp.OrderBy {
		collectReferencedVarsFromExpr(s.Expr, referencedVars)
	}

	// Build FROM + WHERE id = ? fragments for referenced node and edge variables.
	var fromParts []string  // "nodes <alias>" or "edges <alias>"
	var whereFrags []string // "<alias>.id = ?"
	var sentinelArgs []any

	// We need a deterministic order for the FROM clause; iterate scope names sorted.
	varNames := scope.Names()
	for i := 1; i < len(varNames); i++ {
		for j := i; j > 0 && varNames[j] < varNames[j-1]; j-- {
			varNames[j], varNames[j-1] = varNames[j-1], varNames[j]
		}
	}

	seenAliases := make(map[string]bool)
	for _, name := range varNames {
		b, ok := scope.Resolve(name)
		if !ok {
			continue
		}
		// Skip internal anonymous variables (not referenced in projections).
		if len(referencedVars) > 0 && !referencedVars[name] {
			continue
		}
		if seenAliases[b.Alias] {
			continue
		}
		seenAliases[b.Alias] = true

		if b.IsNode {
			fromParts = append(fromParts, "nodes "+b.Alias)
			whereFrags = append(whereFrags, b.Alias+".id = ?")
			sentinelArgs = append(sentinelArgs, idSentinel{VarName: name, Alias: b.Alias})
		} else if b.IsRel {
			fromParts = append(fromParts, "edges "+b.Alias)
			whereFrags = append(whereFrags, b.Alias+".id = ?")
			sentinelArgs = append(sentinelArgs, idSentinel{VarName: name, Alias: b.Alias})
		}
	}

	// Build SELECT list from RETURN projections.
	retT := &Translator{dialect: t.dialect}
	selectList, err := retT.buildSelectList(rp.Projections, scope)
	if err != nil {
		return Statement{}, fmt.Errorf("sql: RETURN after write SELECT list: %w", err)
	}

	var b strings.Builder
	b.WriteString("SELECT ")
	if rp.Distinct {
		b.WriteString("DISTINCT ")
	}
	b.WriteString(selectList)

	if len(fromParts) > 0 {
		b.WriteString(" FROM ")
		b.WriteString(strings.Join(fromParts, ", "))
	}
	if len(whereFrags) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(whereFrags, " AND "))
	}

	// ORDER BY.
	if len(rp.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, s := range rp.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			sortSQL, err := retT.exprToSQL(s.Expr, scope)
			if err != nil {
				return Statement{}, fmt.Errorf("sql: RETURN after write ORDER BY: %w", err)
			}
			b.WriteString(sortSQL)
			if s.Descending {
				b.WriteString(" DESC")
			} else {
				b.WriteString(" ASC")
			}
		}
	}

	if rp.Limit != nil {
		fmt.Fprintf(&b, " LIMIT %d", *rp.Limit)
		if rp.Skip != nil {
			fmt.Fprintf(&b, " OFFSET %d", *rp.Skip)
		}
	} else if rp.Skip != nil {
		fmt.Fprintf(&b, " LIMIT -1 OFFSET %d", *rp.Skip)
	}

	// The args are: SELECT-list args (literals from projections) + sentinel args
	// (idSentinel values resolved at execution time from idMap).
	var allArgs []any
	allArgs = append(allArgs, retT.args...)
	allArgs = append(allArgs, sentinelArgs...)

	return Statement{
		SQL:  b.String(),
		Args: allArgs,
		Kind: KindSelectAfterWrite,
	}, nil
}

// collectReferencedVars returns a set of Cypher variable names that appear in
// the given projection list. This is used by translateReturnAfterWrite to avoid
// adding unnecessary table entries for anonymous internal variables.
func collectReferencedVars(projections []cypher.ProjectionItem) map[string]bool {
	refs := make(map[string]bool)
	for _, proj := range projections {
		collectReferencedVarsFromExpr(proj.Expr, refs)
	}
	return refs
}

// collectReferencedVarsFromExpr recursively collects variable names from an expression.
func collectReferencedVarsFromExpr(expr cypher.Expr, refs map[string]bool) {
	if expr == nil {
		return
	}
	switch e := expr.(type) {
	case *cypher.VarExpr:
		refs[e.Name] = true
	case *cypher.PropExpr:
		refs[e.Variable] = true
	case *cypher.ComparisonExpr:
		collectReferencedVarsFromExpr(e.Left, refs)
		collectReferencedVarsFromExpr(e.Right, refs)
	case *cypher.BoolExpr:
		collectReferencedVarsFromExpr(e.Left, refs)
		collectReferencedVarsFromExpr(e.Right, refs)
	case *cypher.NotExpr:
		collectReferencedVarsFromExpr(e.Expr, refs)
	case *cypher.AggCallExpr:
		collectReferencedVarsFromExpr(e.Arg, refs)
	case *cypher.NullCheckExpr:
		collectReferencedVarsFromExpr(e.Expr, refs)
	case *cypher.InListExpr:
		collectReferencedVarsFromExpr(e.Expr, refs)
		for _, item := range e.List {
			collectReferencedVarsFromExpr(item, refs)
		}
	case *cypher.StringMatchExpr:
		collectReferencedVarsFromExpr(e.Expr, refs)
		collectReferencedVarsFromExpr(e.Pattern, refs)
	case *cypher.CaseExpr:
		collectReferencedVarsFromExpr(e.Subject, refs)
		for _, wc := range e.WhenClauses {
			collectReferencedVarsFromExpr(wc.Condition, refs)
			collectReferencedVarsFromExpr(wc.CaseVal, refs)
			collectReferencedVarsFromExpr(wc.Value, refs)
		}
		collectReferencedVarsFromExpr(e.Else, refs)
	}
	// LiteralExpr, ParamRef, RawExpr — no variable references.
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
	// buildFromClause no longer side-effects t.args; args are carried in fc.
	fc, err := t.buildFromClause(rp.Source, scope)
	if err != nil {
		return "", err
	}

	// Build SELECT list before assembling FROM/WHERE args so that any
	// arg-producing expressions in the SELECT position (e.g. literal strings)
	// are appended to t.args first, matching their position in the SQL text.
	selectList, err := t.buildSelectList(rp.Projections, scope)
	if err != nil {
		return "", err
	}

	// Assemble bind args in SQL-text order:
	//   CTE args (prepended; CTEs appear before SELECT in SQL text)
	//   → SELECT args (already in t.args from buildSelectList above)
	//   → JOIN ON args (fc.joinArgs; JOIN appears before WHERE in SQL)
	//   → WHERE args (fc.whereArgs)
	//   → extra WHERE args (fc.extraWhereArgs; extraWhere appears after whereFragments)
	// ORDER BY args are appended later by the exprToSQL calls below.
	// CTE args must come first because the WITH RECURSIVE clause is emitted
	// before SELECT in the final SQL string.
	t.args = append(fc.cteArgs, t.args...)
	t.args = append(t.args, fc.joinArgs...)
	t.args = append(t.args, fc.whereArgs...)
	t.args = append(t.args, fc.extraWhereArgs...)

	var b strings.Builder

	// Prepend WITH RECURSIVE if any variable-length path CTEs were collected.
	if len(fc.ctes) > 0 {
		b.WriteString("WITH RECURSIVE ")
		b.WriteString(strings.Join(fc.ctes, ", "))
		b.WriteString(" ")
	}

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

	// GROUP BY (emitted when a WithPlan with aggregate projections is the source).
	if len(fc.groupBy) > 0 {
		b.WriteString(" GROUP BY ")
		b.WriteString(strings.Join(fc.groupBy, ", "))
	}

	// HAVING (post-WITH WHERE predicate, after GROUP BY, before ORDER BY).
	if fc.having != "" {
		t.args = append(t.args, fc.havingArgs...)
		b.WriteString(" HAVING ")
		b.WriteString(fc.having)
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
	// SQLite requires LIMIT before OFFSET; when only SKIP is present we emit
	// LIMIT -1 (unlimited) so that OFFSET is valid syntax.
	if rp.Limit != nil {
		fmt.Fprintf(&b, " LIMIT %d", *rp.Limit)
		if rp.Skip != nil {
			fmt.Fprintf(&b, " OFFSET %d", *rp.Skip)
		}
	} else if rp.Skip != nil {
		fmt.Fprintf(&b, " LIMIT -1 OFFSET %d", *rp.Skip)
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
	// Assemble bind args; standalone node match has only WHERE args (no JOINs).
	t.args = append(t.args, fc.whereArgs...)
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
	// Assemble bind args in SQL order before calling exprToSQL for the predicate:
	//   JOIN ON args → WHERE args → extra WHERE args → predicate args (appended by exprToSQL).
	t.args = append(t.args, fc.joinArgs...)
	t.args = append(t.args, fc.whereArgs...)
	t.args = append(t.args, fc.extraWhereArgs...)
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
	// joinArgs holds bind args for JOIN ON clauses. These must be appended to
	// the translator's args slice before whereArgs because JOIN ON appears before
	// WHERE in SQL text.
	joinArgs []any
	// whereFragments are WHERE conditions accumulated from label checks and
	// inline property constraints on match nodes.
	whereFragments []string
	// whereArgs holds bind args for WHERE clause fragments.
	whereArgs []any
	// extraWhere is a WHERE predicate coming from an explicit FilterPlan.
	extraWhere string
	// extraWhereArgs holds bind args for the extraWhere predicate. These are
	// appended after whereArgs because extraWhere appears after whereFragments.
	extraWhereArgs []any
	// groupBy holds SQL expressions for the GROUP BY clause (no bind args;
	// these are column references like "n0.id").
	groupBy []string
	// having is a HAVING predicate text (for post-WITH WHERE predicates).
	having string
	// havingArgs are bind args for the HAVING predicate.
	havingArgs []any
	// ctes holds complete WITH RECURSIVE CTE definitions for variable-length
	// path patterns. Each entry is the body of one CTE (e.g.
	// "_vl0(end_id, depth) AS (SELECT ...)"). The caller prepends
	// "WITH RECURSIVE " before the SELECT when len(ctes) > 0.
	ctes []string
	// cteArgs holds the bind args for all CTE definitions, in the order they
	// appear within the cte bodies. These must be prepended before all other
	// args because the CTEs appear before the main SELECT in SQL.
	cteArgs []any
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
		// Use a sub-translator so predicate args are captured separately from the
		// source's join/where args. They must be appended after whereArgs at the
		// final assembly point (extraWhere appears after whereFragments in SQL).
		sub := &Translator{dialect: t.dialect}
		predSQL, err := sub.exprToSQL(s.Predicate, scope)
		if err != nil {
			return fromClause{}, fmt.Errorf("sql: WHERE predicate: %w", err)
		}
		fc.extraWhere = predSQL
		fc.extraWhereArgs = sub.args
		return fc, nil

	case *cypher.VariableLengthRelPlan:
		return t.buildFromClauseForVarLengthRel(s, scope)

	case *cypher.SequencePlan:
		return t.buildFromClauseForSequence(s, scope)

	case *cypher.WithPlan:
		return t.buildFromClauseForWithPlan(s, scope)

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

	// Label constraints go to WHERE; args collected into fc.whereArgs so the
	// caller can assemble t.args in SQL order (JOIN ON args before WHERE args).
	for _, label := range mnp.Labels {
		pred, args := t.dialect.LabelContains(alias+".labels", label)
		fc.whereFragments = append(fc.whereFragments, pred)
		fc.whereArgs = append(fc.whereArgs, args...)
	}

	// Inline property constraints.
	for key, expr := range mnp.Props {
		sub := &Translator{dialect: t.dialect}
		valSQL, err := sub.exprToSQL(expr, scope)
		if err != nil {
			return fromClause{}, fmt.Errorf("sql: node prop constraint %q: %w", key, err)
		}
		jsonExpr := t.dialect.JSONExtract(alias+".props", "$."+key)
		fc.whereFragments = append(fc.whereFragments, jsonExpr+" = "+valSQL)
		fc.whereArgs = append(fc.whereArgs, sub.args...)
	}

	return fc, nil
}

// buildFromClauseForMatchRel handles a MatchRelPlan (single-hop or part of a
// multi-hop chain).
//
// For OPTIONAL MATCH (mrp.Optional == true) the relationship-type, relationship-
// property, end-node-label, and end-node-property constraints are placed in the
// JOIN ON clause rather than the WHERE clause. Putting them in WHERE would turn
// the LEFT JOIN into an effective INNER JOIN, eliminating rows where the optional
// pattern did not match.
//
// Arg ordering note: bind args must match the positional "?" order in the SQL.
// For optional match the JOIN ON clauses precede WHERE in SQL, so edge/end-node
// args are appended to t.args before start-node args.
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

	from := "nodes " + startAlias
	var joinParts []string
	var whereParts []string
	fc := fromClause{from: from}

	if mrp.Optional {
		// ── OPTIONAL MATCH path ───────────────────────────────────────────────
		//
		// SQL structure:
		//   FROM nodes n0
		//   LEFT JOIN edges rN ON <direction> [AND type=?] [AND relProps=?...]
		//   LEFT JOIN nodes nM ON <direction> [AND labelCheck=?...] [AND props=?...]
		//   WHERE [startNode label/prop constraints]
		//
		// Args must appear in SQL order: JOIN ON args first, then WHERE args.
		// These are stored in fc.joinArgs and fc.whereArgs respectively so the
		// caller (buildFromClauseForSequence → translateReturnPlan) can assemble
		// t.args in the correct order regardless of which step is processed first.

		// 1. Build edge LEFT JOIN ON clause.
		edgeOnParts := []string{t.directionCond(mrp, relAlias, startAlias)}
		for _, relType := range mrp.Types {
			edgeOnParts = append(edgeOnParts, relAlias+".type = ?")
			fc.joinArgs = append(fc.joinArgs, relType)
		}
		for key, expr := range mrp.RelProps {
			sub := &Translator{dialect: t.dialect}
			valSQL, err := sub.exprToSQL(expr, scope)
			if err != nil {
				return fromClause{}, fmt.Errorf("sql: rel prop constraint %q: %w", key, err)
			}
			edgeOnParts = append(edgeOnParts, t.dialect.JSONExtract(relAlias+".props", "$."+key)+" = "+valSQL)
			fc.joinArgs = append(fc.joinArgs, sub.args...)
		}
		joinParts = append(joinParts, fmt.Sprintf("LEFT JOIN edges %s ON %s",
			relAlias, strings.Join(edgeOnParts, " AND ")))

		// 2. Build end-node LEFT JOIN ON clause.
		nodeOnParts := []string{t.endNodeCond(mrp, relAlias, startAlias, endAlias)}
		for _, label := range mrp.EndNode.Labels {
			pred, args := t.dialect.LabelContains(endAlias+".labels", label)
			nodeOnParts = append(nodeOnParts, pred)
			fc.joinArgs = append(fc.joinArgs, args...)
		}
		for key, expr := range mrp.EndNode.Props {
			sub := &Translator{dialect: t.dialect}
			valSQL, err := sub.exprToSQL(expr, scope)
			if err != nil {
				return fromClause{}, fmt.Errorf("sql: end node prop constraint %q: %w", key, err)
			}
			nodeOnParts = append(nodeOnParts, t.dialect.JSONExtract(endAlias+".props", "$."+key)+" = "+valSQL)
			fc.joinArgs = append(fc.joinArgs, sub.args...)
		}
		joinParts = append(joinParts, fmt.Sprintf("LEFT JOIN nodes %s ON %s",
			endAlias, strings.Join(nodeOnParts, " AND ")))

		// 3. Start-node constraints go to WHERE. Args stored in fc.whereArgs so
		//    the caller can append them after fc.joinArgs when assembling t.args.
		for _, label := range mrp.StartNode.Labels {
			pred, args := t.dialect.LabelContains(startAlias+".labels", label)
			whereParts = append(whereParts, pred)
			fc.whereArgs = append(fc.whereArgs, args...)
		}
		for key, expr := range mrp.StartNode.Props {
			sub := &Translator{dialect: t.dialect}
			valSQL, err := sub.exprToSQL(expr, scope)
			if err != nil {
				return fromClause{}, fmt.Errorf("sql: start node prop constraint %q: %w", key, err)
			}
			whereParts = append(whereParts, t.dialect.JSONExtract(startAlias+".props", "$."+key)+" = "+valSQL)
			fc.whereArgs = append(fc.whereArgs, sub.args...)
		}
	} else {
		// ── Regular MATCH path ────────────────────────────────────────────────
		//
		// All constraints go to WHERE; JOIN ON only carries structural conditions.
		// Args order: start-node → edge → end-node (matches WHERE left-to-right).

		// Start node constraints.
		for _, label := range mrp.StartNode.Labels {
			pred, args := t.dialect.LabelContains(startAlias+".labels", label)
			whereParts = append(whereParts, pred)
			fc.whereArgs = append(fc.whereArgs, args...)
		}
		for key, expr := range mrp.StartNode.Props {
			sub := &Translator{dialect: t.dialect}
			valSQL, err := sub.exprToSQL(expr, scope)
			if err != nil {
				return fromClause{}, fmt.Errorf("sql: start node prop constraint %q: %w", key, err)
			}
			whereParts = append(whereParts, t.dialect.JSONExtract(startAlias+".props", "$."+key)+" = "+valSQL)
			fc.whereArgs = append(fc.whereArgs, sub.args...)
		}

		// Edge JOIN (structural condition only; type/prop constraints go to WHERE).
		joinParts = append(joinParts, fmt.Sprintf("JOIN edges %s ON %s",
			relAlias, t.directionCond(mrp, relAlias, startAlias)))

		// Edge constraints to WHERE.
		for _, relType := range mrp.Types {
			whereParts = append(whereParts, relAlias+".type = ?")
			fc.whereArgs = append(fc.whereArgs, relType)
		}
		for key, expr := range mrp.RelProps {
			sub := &Translator{dialect: t.dialect}
			valSQL, err := sub.exprToSQL(expr, scope)
			if err != nil {
				return fromClause{}, fmt.Errorf("sql: rel prop constraint %q: %w", key, err)
			}
			whereParts = append(whereParts, t.dialect.JSONExtract(relAlias+".props", "$."+key)+" = "+valSQL)
			fc.whereArgs = append(fc.whereArgs, sub.args...)
		}

		// End node JOIN (structural condition only).
		joinParts = append(joinParts, fmt.Sprintf("JOIN nodes %s ON %s",
			endAlias, t.endNodeCond(mrp, relAlias, startAlias, endAlias)))

		// End node constraints to WHERE.
		for _, label := range mrp.EndNode.Labels {
			pred, args := t.dialect.LabelContains(endAlias+".labels", label)
			whereParts = append(whereParts, pred)
			fc.whereArgs = append(fc.whereArgs, args...)
		}
		for key, expr := range mrp.EndNode.Props {
			sub := &Translator{dialect: t.dialect}
			valSQL, err := sub.exprToSQL(expr, scope)
			if err != nil {
				return fromClause{}, fmt.Errorf("sql: end node prop constraint %q: %w", key, err)
			}
			whereParts = append(whereParts, t.dialect.JSONExtract(endAlias+".props", "$."+key)+" = "+valSQL)
			fc.whereArgs = append(fc.whereArgs, sub.args...)
		}
	}

	fc.joins = strings.Join(joinParts, " ")
	fc.whereFragments = whereParts
	return fc, nil
}

// buildFromClauseForVarLengthRel handles a VariableLengthRelPlan by emitting a
// WITH RECURSIVE CTE. The CTE name is vlp.CTEAlias (e.g. "_vl0").
//
// Directed (ToRight):
//
//	WITH RECURSIVE _vl0(end_id, depth) AS (
//	  SELECT e.end_id, 1 FROM edges e WHERE e.start_id = <startAlias>.id [AND type IN (...)]
//	  UNION ALL
//	  SELECT e.end_id, _vl0.depth + 1 FROM edges e JOIN _vl0 ON e.start_id = _vl0.end_id
//	  WHERE _vl0.depth < <maxHops> [AND type IN (...)]
//	)
//
// The main SELECT then JOINs:
//
//	JOIN nodes <endAlias> ON <endAlias>.id IN (SELECT end_id FROM _vl0 WHERE depth >= <minHops>)
//
// Safety cap: if MaxHops == 0 (unbounded) the recursive case always applies a
// depth < 15 guard to prevent runaway queries.
func (t *Translator) buildFromClauseForVarLengthRel(vlp *cypher.VariableLengthRelPlan, scope *cypher.BindingScope) (fromClause, error) {
	startAlias := vlp.StartNode.SQLAlias
	if startAlias == "" && vlp.StartVar != "" {
		b, ok := scope.Resolve(vlp.StartVar)
		if !ok {
			return fromClause{}, fmt.Errorf("sql: start variable %q not in scope for variable-length path", vlp.StartVar)
		}
		startAlias = b.Alias
	}
	endAlias := vlp.EndNode.SQLAlias

	cte := vlp.CTEAlias
	const safetyLimit = 15

	// ── Build type-filter SQL fragment for both base and recursive cases ─────
	// e.g. " AND e.type IN (?, ?)" — appended in both base and recursive cases.
	var typeFilter string
	var typeArgs []any
	for _, rt := range vlp.RelTypes {
		typeArgs = append(typeArgs, rt)
	}
	if len(typeArgs) == 1 {
		typeFilter = " AND e.type = ?"
	} else if len(typeArgs) > 1 {
		placeholders := make([]string, len(typeArgs))
		for i := range placeholders {
			placeholders[i] = "?"
		}
		typeFilter = " AND e.type IN (" + strings.Join(placeholders, ", ") + ")"
	}

	// ── Base case ─────────────────────────────────────────────────────────────
	var baseCond string
	if vlp.Undirected {
		baseCond = fmt.Sprintf("(e.start_id = %s.id OR e.end_id = %s.id)", startAlias, startAlias)
	} else if vlp.ToRight {
		baseCond = fmt.Sprintf("e.start_id = %s.id", startAlias)
	} else {
		// ToLeft: start node is at end_id side.
		baseCond = fmt.Sprintf("e.end_id = %s.id", startAlias)
	}

	var baseEndID string
	if vlp.Undirected {
		baseEndID = fmt.Sprintf("CASE WHEN e.start_id = %s.id THEN e.end_id ELSE e.start_id END", startAlias)
	} else if vlp.ToRight {
		baseEndID = "e.end_id"
	} else {
		baseEndID = "e.start_id"
	}

	// ── Recursive case ────────────────────────────────────────────────────────
	var recCond string
	if vlp.Undirected {
		recCond = fmt.Sprintf("(e.start_id = %s.end_id OR e.end_id = %s.end_id)", cte, cte)
	} else if vlp.ToRight {
		recCond = fmt.Sprintf("e.start_id = %s.end_id", cte)
	} else {
		recCond = fmt.Sprintf("e.end_id = %s.end_id", cte)
	}

	var recEndID string
	if vlp.Undirected {
		recEndID = fmt.Sprintf("CASE WHEN e.start_id = %s.end_id THEN e.end_id ELSE e.start_id END", cte)
	} else if vlp.ToRight {
		recEndID = "e.end_id"
	} else {
		recEndID = "e.start_id"
	}

	// Depth guard in the recursive case.
	maxHops := vlp.MaxHops
	if maxHops == 0 {
		maxHops = safetyLimit // practical cap for unbounded paths
	}
	// The recursive WHERE condition stops expansion when depth reaches the cap.
	// "depth < maxHops" allows the recursive case to add one more hop (depth+1).
	recDepthGuard := fmt.Sprintf("%s.depth < ?", cte)
	var depthArgs []any
	depthArgs = append(depthArgs, maxHops)

	// ── Assemble CTE SQL ──────────────────────────────────────────────────────
	var cteBuf strings.Builder
	fmt.Fprintf(&cteBuf, "%s(end_id, depth) AS (", cte)
	// Base case.
	fmt.Fprintf(&cteBuf, "SELECT %s, 1 FROM edges e WHERE %s%s", baseEndID, baseCond, typeFilter)
	// Recursive case.
	fmt.Fprintf(&cteBuf, " UNION ALL SELECT %s, %s.depth + 1 FROM edges e JOIN %s ON %s WHERE %s%s",
		recEndID, cte, cte, recCond, recDepthGuard, typeFilter)
	cteBuf.WriteByte(')')

	// Args for the CTE: base type args, then recursive (depth guard + type args).
	var allCTEArgs []any
	allCTEArgs = append(allCTEArgs, typeArgs...)   // base case type filter
	allCTEArgs = append(allCTEArgs, depthArgs...)  // recursive depth guard
	allCTEArgs = append(allCTEArgs, typeArgs...)   // recursive type filter

	// ── End-node JOIN ─────────────────────────────────────────────────────────
	// JOIN nodes <endAlias> ON <endAlias>.id IN (SELECT end_id FROM <cte> WHERE depth >= ?)
	minHops := vlp.MinHops
	if minHops <= 0 {
		minHops = 1
	}
	endJoinSQL := fmt.Sprintf("JOIN nodes %s ON %s.id IN (SELECT end_id FROM %s WHERE depth >= ?)",
		endAlias, endAlias, cte)

	// ── Start-node constraints (WHERE) ────────────────────────────────────────
	var whereParts []string
	var whereArgs []any
	for _, label := range vlp.StartNode.Labels {
		pred, args := t.dialect.LabelContains(startAlias+".labels", label)
		whereParts = append(whereParts, pred)
		whereArgs = append(whereArgs, args...)
	}
	for key, expr := range vlp.StartNode.Props {
		sub := &Translator{dialect: t.dialect}
		valSQL, err := sub.exprToSQL(expr, scope)
		if err != nil {
			return fromClause{}, fmt.Errorf("sql: var-length start node prop %q: %w", key, err)
		}
		whereParts = append(whereParts, t.dialect.JSONExtract(startAlias+".props", "$."+key)+" = "+valSQL)
		whereArgs = append(whereArgs, sub.args...)
	}

	// ── End-node constraints (additional WHERE fragments) ─────────────────────
	for _, label := range vlp.EndNode.Labels {
		pred, args := t.dialect.LabelContains(endAlias+".labels", label)
		whereParts = append(whereParts, pred)
		whereArgs = append(whereArgs, args...)
	}
	for key, expr := range vlp.EndNode.Props {
		sub := &Translator{dialect: t.dialect}
		valSQL, err := sub.exprToSQL(expr, scope)
		if err != nil {
			return fromClause{}, fmt.Errorf("sql: var-length end node prop %q: %w", key, err)
		}
		whereParts = append(whereParts, t.dialect.JSONExtract(endAlias+".props", "$."+key)+" = "+valSQL)
		whereArgs = append(whereArgs, sub.args...)
	}

	// ── Assemble fromClause ───────────────────────────────────────────────────
	// cteArgs carries: allCTEArgs (for the CTE body) + [minHops] (for the end-node JOIN).
	// These are prepended to the final args in translateReturnPlan.
	fc := fromClause{
		from:           "nodes " + startAlias,
		joins:          endJoinSQL,
		joinArgs:       []any{int64(minHops)}, // for the "depth >= ?" in end-node JOIN
		whereFragments: whereParts,
		whereArgs:      whereArgs,
		ctes:           []string{cteBuf.String()},
		cteArgs:        allCTEArgs,
	}
	return fc, nil
}

// directionCond returns the structural ON predicate for the edge join
// (direction-only, no type or property constraints).
func (t *Translator) directionCond(mrp *cypher.MatchRelPlan, relAlias, startAlias string) string {
	if mrp.Undirected {
		return fmt.Sprintf("(%s.start_id = %s.id OR %s.end_id = %s.id)",
			relAlias, startAlias, relAlias, startAlias)
	}
	if mrp.ToRight {
		return fmt.Sprintf("%s.start_id = %s.id", relAlias, startAlias)
	}
	// ToLeft: <-[r]-
	return fmt.Sprintf("%s.end_id = %s.id", relAlias, startAlias)
}

// endNodeCond returns the structural ON predicate for the end-node join.
func (t *Translator) endNodeCond(mrp *cypher.MatchRelPlan, relAlias, startAlias, endAlias string) string {
	if mrp.Undirected {
		return fmt.Sprintf("(%s.id = CASE WHEN %s.start_id = %s.id THEN %s.end_id ELSE %s.start_id END)",
			endAlias, relAlias, startAlias, relAlias, relAlias)
	}
	if mrp.ToRight {
		return fmt.Sprintf("%s.id = %s.end_id", endAlias, relAlias)
	}
	return fmt.Sprintf("%s.id = %s.start_id", endAlias, relAlias)
}

// buildFromClauseForSequence handles a SequencePlan that appears in the source
// position (a chain of match plans for multi-hop queries).
//
// Arg ordering invariant: JOIN ON args from all steps are accumulated into
// allJoinArgs; WHERE args from all steps into allWhereArgs. The caller must
// append allJoinArgs before allWhereArgs when assembling t.args, because SQL
// places JOIN ON clauses before WHERE.
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
	extraWhereArgs := baseFC.extraWhereArgs
	baseFC.extraWhere = ""
	baseFC.extraWhereArgs = nil

	var allJoinArgs []any
	var allWhereArgs []any
	allJoinArgs = append(allJoinArgs, baseFC.joinArgs...)
	allWhereArgs = append(allWhereArgs, baseFC.whereArgs...)

	allWhere := baseFC.whereFragments
	allJoins := baseFC.joins
	allCTEs := append([]string(nil), baseFC.ctes...)
	allCTEArgs := append([]any(nil), baseFC.cteArgs...)

	// Process the remaining steps (additional MATCH hops).
	for _, step := range sp.Steps[1:] {
		fc, err := t.buildFromClause(step, scope)
		if err != nil {
			return fromClause{}, err
		}
		// A step that is a plain MatchNodePlan (or any step that contributes only
		// a FROM table with no JOIN clause) must be folded in as a CROSS JOIN so
		// its table alias is visible to the outer SELECT/WHERE. Without this the
		// alias (e.g. n1) would be silently dropped from the query, causing
		// "no such column: n1.id" errors for Cartesian-product patterns such as
		// MATCH (a), (b) or MATCH (x:X), (y:Y) CREATE (x)-[:R]->(y).
		if fc.from != "" && fc.joins == "" {
			crossJoin := "CROSS JOIN " + fc.from
			if allJoins != "" {
				allJoins += " " + crossJoin
			} else {
				allJoins = crossJoin
			}
		} else if fc.joins != "" {
			// Accumulate JOIN fragments and args (JOIN ON appears before WHERE in SQL,
			// so joinArgs must be assembled before whereArgs at the final call site).
			if allJoins != "" {
				allJoins += " " + fc.joins
			} else {
				allJoins = fc.joins
			}
		}
		allJoinArgs = append(allJoinArgs, fc.joinArgs...)
		allWhere = append(allWhere, fc.whereFragments...)
		allWhereArgs = append(allWhereArgs, fc.whereArgs...)
		if fc.extraWhere != "" {
			// An inner FilterPlan: fold its predicate and args into the WHERE
			// accumulator in position (they follow this step's whereFragments).
			allWhere = append(allWhere, fc.extraWhere)
			allWhereArgs = append(allWhereArgs, fc.extraWhereArgs...)
		}
		// Propagate any CTEs from variable-length hops.
		allCTEs = append(allCTEs, fc.ctes...)
		allCTEArgs = append(allCTEArgs, fc.cteArgs...)
	}

	return fromClause{
		from:           baseFC.from,
		joins:          allJoins,
		joinArgs:       allJoinArgs,
		whereFragments: allWhere,
		whereArgs:      allWhereArgs,
		extraWhere:     extraWhere,
		extraWhereArgs: extraWhereArgs,
		ctes:           allCTEs,
		cteArgs:        allCTEArgs,
	}, nil
}

// buildFromClauseForWithPlan handles a WithPlan that acts as the source of a
// ReturnPlan. It builds the FROM/JOIN/WHERE fragments from the WithPlan's
// Source, then computes GROUP BY from the non-aggregate projections and
// (optionally) a HAVING predicate from WithPlan.Having.
func (t *Translator) buildFromClauseForWithPlan(wp *cypher.WithPlan, scope *cypher.BindingScope) (fromClause, error) {
	// Build the FROM clause from the underlying source (MATCH/FILTER plans).
	fc, err := t.buildFromClause(wp.Source, scope)
	if err != nil {
		return fromClause{}, err
	}

	// Determine whether any projection is an aggregate.
	hasAgg := false
	for _, proj := range wp.Projections {
		if isAggExpr(proj.Expr) {
			hasAgg = true
			break
		}
	}

	if hasAgg {
		// Non-aggregate projections become GROUP BY columns.
		for _, proj := range wp.Projections {
			if isAggExpr(proj.Expr) {
				continue
			}
			groupSQL, err := t.toGroupBySQL(proj.Expr, scope)
			if err != nil {
				return fromClause{}, fmt.Errorf("sql: GROUP BY expression: %w", err)
			}
			if groupSQL != "" {
				fc.groupBy = append(fc.groupBy, groupSQL)
			}
		}
	}

	// HAVING predicate (post-WITH WHERE).
	if wp.Having != nil {
		sub := &Translator{dialect: t.dialect}
		havingSQL, err := sub.exprToSQL(wp.Having, scope)
		if err != nil {
			return fromClause{}, fmt.Errorf("sql: HAVING predicate: %w", err)
		}
		fc.having = havingSQL
		fc.havingArgs = sub.args
	}

	return fc, nil
}

// isAggExpr returns true if expr is an AggCallExpr (top-level aggregate call).
func isAggExpr(expr cypher.Expr) bool {
	_, ok := expr.(*cypher.AggCallExpr)
	return ok
}

// toGroupBySQL returns the SQL GROUP BY expression for a non-aggregate
// projection expression. For node/rel variables it uses the .id column
// (the natural grouping key). For property accesses it emits json_extract.
// Literals return "" (no GROUP BY column needed).
func (t *Translator) toGroupBySQL(expr cypher.Expr, scope *cypher.BindingScope) (string, error) {
	switch e := expr.(type) {
	case *cypher.VarExpr:
		b, ok := scope.Resolve(e.Name)
		if !ok {
			return "", fmt.Errorf("sql: variable %q not in scope for GROUP BY", e.Name)
		}
		if b.AggExpr != nil {
			// Aggregate alias — not a GROUP BY column.
			return "", nil
		}
		// Group by the node/rel identity column (e.g. "n0.id").
		return b.Column, nil
	case *cypher.PropExpr:
		b, ok := scope.Resolve(e.Variable)
		if !ok {
			return "", fmt.Errorf("sql: variable %q not in scope for GROUP BY", e.Variable)
		}
		return t.dialect.JSONExtract(b.Alias+".props", "$."+e.Property), nil
	case *cypher.LiteralExpr:
		// Literals are constants — no GROUP BY column needed.
		return "", nil
	default:
		// Fallback: translate and include in GROUP BY.
		sql, err := t.exprToSQL(expr, scope)
		if err != nil {
			return "", err
		}
		return sql, nil
	}
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
		// for a relationship, emit the id. For aggregate aliases, expand to the full
		// aggregate expression.
		binding, ok := scope.Resolve(e.Name)
		if !ok {
			return "", fmt.Errorf("sql: variable %q not in scope", e.Name)
		}
		// Aggregate alias (e.g. cnt from WITH count(r) AS cnt): expand to the
		// full aggregate expression so it can appear in SELECT and ORDER BY.
		if binding.AggExpr != nil {
			return t.exprToSQL(binding.AggExpr, scope)
		}
		if binding.IsNode {
			obj := fmt.Sprintf(
				"json_object('id', %[1]s.id, 'labels', %[1]s.labels, 'props', json(%[1]s.props))",
				binding.Alias,
			)
			if binding.IsNullable {
				// Wrap in CASE WHEN so unmatched OPTIONAL MATCH rows project as NULL,
				// not as a json_object with null fields.
				return fmt.Sprintf("CASE WHEN %s.id IS NULL THEN NULL ELSE %s END", binding.Alias, obj), nil
			}
			return obj, nil
		}
		if binding.IsRel {
			obj := fmt.Sprintf(
				"json_object('id', %[1]s.id, 'type', %[1]s.type, 'start_id', %[1]s.start_id, 'end_id', %[1]s.end_id, 'props', json(%[1]s.props))",
				binding.Alias,
			)
			if binding.IsNullable {
				return fmt.Sprintf("CASE WHEN %s.id IS NULL THEN NULL ELSE %s END", binding.Alias, obj), nil
			}
			return obj, nil
		}
		return binding.Column, nil

	case *cypher.NullCheckExpr:
		// IS NULL / IS NOT NULL: for variable references use the .id column so the
		// check is meaningful on LEFT JOIN results; for property expressions the
		// json_extract result is already NULL when the row is unmatched.
		var innerSQL string
		if ve, ok := e.Expr.(*cypher.VarExpr); ok {
			b, ok := scope.Resolve(ve.Name)
			if !ok {
				return "", fmt.Errorf("sql: variable %q not in scope", ve.Name)
			}
			innerSQL = b.Alias + ".id"
		} else {
			var err error
			innerSQL, err = t.exprToSQL(e.Expr, scope)
			if err != nil {
				return "", err
			}
		}
		if e.IsNotNull {
			return fmt.Sprintf("(%s IS NOT NULL)", innerSQL), nil
		}
		return fmt.Sprintf("(%s IS NULL)", innerSQL), nil

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

	case *cypher.AggCallExpr:
		if e.Func == "collect" {
			// COLLECT(expr) → json_group_array(expr)
			if e.CountStar || e.Arg == nil {
				return "json_group_array(*)", nil
			}
			argSQL, err := t.aggArgToSQL(e.Arg, scope)
			if err != nil {
				return "", fmt.Errorf("sql: collect() argument: %w", err)
			}
			if e.Distinct {
				return fmt.Sprintf("json_group_array(DISTINCT %s)", argSQL), nil
			}
			return fmt.Sprintf("json_group_array(%s)", argSQL), nil
		}
		funcName := strings.ToUpper(e.Func)
		if e.CountStar || e.Arg == nil {
			return funcName + "(*)", nil
		}
		// For aggregate arguments, node/rel VarExprs should emit their id column
		// (e.g. r0.id), not the full JSON object representation.
		argSQL, err := t.aggArgToSQL(e.Arg, scope)
		if err != nil {
			return "", fmt.Errorf("sql: %s() argument: %w", e.Func, err)
		}
		if e.Distinct {
			return fmt.Sprintf("%s(DISTINCT %s)", funcName, argSQL), nil
		}
		return fmt.Sprintf("%s(%s)", funcName, argSQL), nil

	case *cypher.ExistsExpr:
		// exists(n.prop) → json_extract(<alias>.props, '$.prop') IS NOT NULL
		binding, ok := scope.Resolve(e.Prop.Variable)
		if !ok {
			return "", fmt.Errorf("sql: variable %q not in scope for exists()", e.Prop.Variable)
		}
		jsonExpr := t.dialect.JSONExtract(binding.Alias+".props", "$."+e.Prop.Property)
		return fmt.Sprintf("(%s IS NOT NULL)", jsonExpr), nil

	case *cypher.InListExpr:
		// n.prop IN ['a','b','c'] → json_extract(...) IN (?, ?, ?)
		lhsSQL, err := t.exprToSQL(e.Expr, scope)
		if err != nil {
			return "", fmt.Errorf("sql: IN lhs: %w", err)
		}
		if len(e.List) == 0 {
			// Empty IN list — always false.
			if e.Not {
				return "1", nil
			}
			return "0", nil
		}
		placeholders := make([]string, len(e.List))
		for i, item := range e.List {
			ph, err := t.exprToSQL(item, scope)
			if err != nil {
				return "", fmt.Errorf("sql: IN list item %d: %w", i, err)
			}
			placeholders[i] = ph
		}
		op := "IN"
		if e.Not {
			op = "NOT IN"
		}
		return fmt.Sprintf("%s %s (%s)", lhsSQL, op, strings.Join(placeholders, ", ")), nil

	case *cypher.StringMatchExpr:
		// STARTS WITH → lhs LIKE 'pattern%'
		// ENDS WITH   → lhs LIKE '%pattern'
		// CONTAINS    → lhs LIKE '%pattern%'
		lhsSQL, err := t.exprToSQL(e.Expr, scope)
		if err != nil {
			return "", fmt.Errorf("sql: string match lhs: %w", err)
		}
		// Pattern must be a literal string for LIKE; for non-literal RHS we emit a
		// LIKE with bind parameter using SQLite's LIKE pattern.
		patternSQL, patternLiteral, ok := t.stringMatchPattern(e.Pattern, e.Op)
		if ok {
			// Pattern is a string literal — inline the LIKE pattern directly.
			t.args = append(t.args, patternLiteral)
			if e.Not {
				return fmt.Sprintf("(%s NOT LIKE ?)", lhsSQL), nil
			}
			return fmt.Sprintf("(%s LIKE ?)", lhsSQL), nil
		}
		// Non-literal RHS: use SQLite string concatenation to build the LIKE pattern.
		_ = patternSQL
		// Fallback: just emit a parameterised LIKE with the pattern SQL.
		// Build prefix/suffix using CASE / || operators.
		switch e.Op {
		case "STARTS WITH":
			// lhs LIKE (rhs || '%')
			rhsSQL, err := t.exprToSQL(e.Pattern, scope)
			if err != nil {
				return "", err
			}
			if e.Not {
				return fmt.Sprintf("(%s NOT LIKE (%s || '%%'))", lhsSQL, rhsSQL), nil
			}
			return fmt.Sprintf("(%s LIKE (%s || '%%'))", lhsSQL, rhsSQL), nil
		case "ENDS WITH":
			rhsSQL, err := t.exprToSQL(e.Pattern, scope)
			if err != nil {
				return "", err
			}
			if e.Not {
				return fmt.Sprintf("(%s NOT LIKE ('%%' || %s))", lhsSQL, rhsSQL), nil
			}
			return fmt.Sprintf("(%s LIKE ('%%' || %s))", lhsSQL, rhsSQL), nil
		default: // CONTAINS
			rhsSQL, err := t.exprToSQL(e.Pattern, scope)
			if err != nil {
				return "", err
			}
			if e.Not {
				return fmt.Sprintf("(%s NOT LIKE ('%%' || %s || '%%'))", lhsSQL, rhsSQL), nil
			}
			return fmt.Sprintf("(%s LIKE ('%%' || %s || '%%'))", lhsSQL, rhsSQL), nil
		}

	case *cypher.CaseExpr:
		return t.caseExprToSQL(e, scope)

	case *cypher.RawExpr:
		// RawExpr: unsupported sub-expression; return as-is (best effort).
		// The translator cannot produce correct SQL for this but should not crash.
		return e.Text, nil

	default:
		return "", fmt.Errorf("sql: unsupported expression type %T", expr)
	}
}

// caseExprToSQL converts a CaseExpr to a SQL CASE … END fragment.
//
// Searched form (Subject == nil):
//
//	CASE WHEN <cond1> THEN <val1> [WHEN <cond2> THEN <val2> ...] [ELSE <default>] END
//
// Simple form (Subject != nil):
//
//	CASE <subject> WHEN <val1> THEN <result1> [WHEN <val2> THEN <result2> ...] [ELSE <default>] END
func (t *Translator) caseExprToSQL(e *cypher.CaseExpr, scope *cypher.BindingScope) (string, error) {
	if len(e.WhenClauses) == 0 {
		return "NULL", nil
	}

	var b strings.Builder
	b.WriteString("CASE")

	// Simple form: emit the subject expression after CASE.
	if e.Subject != nil {
		subjSQL, err := t.exprToSQL(e.Subject, scope)
		if err != nil {
			return "", fmt.Errorf("sql: CASE subject: %w", err)
		}
		b.WriteByte(' ')
		b.WriteString(subjSQL)
	}

	// Emit each WHEN … THEN … branch.
	for i, clause := range e.WhenClauses {
		b.WriteString(" WHEN ")
		if e.Subject != nil {
			// Simple form: WHEN <value>
			if clause.CaseVal == nil {
				return "", fmt.Errorf("sql: CASE simple form: clause %d missing CaseVal", i)
			}
			valSQL, err := t.exprToSQL(clause.CaseVal, scope)
			if err != nil {
				return "", fmt.Errorf("sql: CASE WHEN value: %w", err)
			}
			b.WriteString(valSQL)
		} else {
			// Searched form: WHEN <condition>
			if clause.Condition == nil {
				return "", fmt.Errorf("sql: CASE searched form: clause %d missing Condition", i)
			}
			condSQL, err := t.exprToSQL(clause.Condition, scope)
			if err != nil {
				return "", fmt.Errorf("sql: CASE WHEN condition: %w", err)
			}
			b.WriteString(condSQL)
		}
		b.WriteString(" THEN ")
		thenSQL, err := t.exprToSQL(clause.Value, scope)
		if err != nil {
			return "", fmt.Errorf("sql: CASE THEN: %w", err)
		}
		b.WriteString(thenSQL)
	}

	// Emit ELSE clause.
	if e.Else != nil {
		b.WriteString(" ELSE ")
		elseSQL, err := t.exprToSQL(e.Else, scope)
		if err != nil {
			return "", fmt.Errorf("sql: CASE ELSE: %w", err)
		}
		b.WriteString(elseSQL)
	}

	b.WriteString(" END")
	return b.String(), nil
}

// stringMatchPattern returns the LIKE pattern string for a string match
// expression where the pattern is a literal string value. It returns
// (pattern, literal, true) when the RHS is a literal string, or
// ("", "", false) when it is not (e.g. a property reference or parameter).
// The caller is responsible for appending the returned literal to t.args.
func (t *Translator) stringMatchPattern(pattern cypher.Expr, op string) (string, string, bool) {
	lit, ok := pattern.(*cypher.LiteralExpr)
	if !ok {
		return "", "", false
	}
	strVal, ok := lit.Value.(string)
	if !ok {
		return "", "", false
	}
	// Escape any existing '%' or '_' wildcards in the literal value so they
	// are treated as plain characters in the LIKE pattern.
	escaped := strings.ReplaceAll(strVal, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, "%", `\%`)
	escaped = strings.ReplaceAll(escaped, "_", `\_`)
	var likePattern string
	switch op {
	case "STARTS WITH":
		likePattern = escaped + "%"
	case "ENDS WITH":
		likePattern = "%" + escaped
	default: // CONTAINS
		likePattern = "%" + escaped + "%"
	}
	return likePattern, likePattern, true
}

// aggArgToSQL converts an expression to SQL for use as an aggregate function
// argument. Node and relationship VarExprs emit their id column (e.g. "r0.id")
// rather than the full JSON object, since aggregating JSON objects is semantically
// wrong and defeats the purpose of counting/summing identifiers.
func (t *Translator) aggArgToSQL(expr cypher.Expr, scope *cypher.BindingScope) (string, error) {
	if ve, ok := expr.(*cypher.VarExpr); ok {
		binding, found := scope.Resolve(ve.Name)
		if !found {
			return "", fmt.Errorf("sql: variable %q not in scope", ve.Name)
		}
		// For node/rel variables inside aggregates, use the id column.
		if binding.IsNode || binding.IsRel {
			return binding.Column, nil
		}
		// Aggregate alias inside another aggregate — shouldn't normally occur,
		// but expand it anyway.
		if binding.AggExpr != nil {
			return t.exprToSQL(binding.AggExpr, scope)
		}
		return binding.Column, nil
	}
	// Non-variable expressions (properties, literals, etc.) delegate to exprToSQL.
	return t.exprToSQL(expr, scope)
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

	case *cypher.SetMergePlan:
		stmt, e := t.translateSetMerge(p, scope)
		if e != nil {
			return nil, true, e
		}
		return []Statement{stmt}, true, nil

	case *cypher.RemovePropPlan:
		stmt, e := t.translateRemoveProp(p, scope)
		if e != nil {
			return nil, true, e
		}
		return []Statement{stmt}, true, nil

	case *cypher.RemoveLabelPlan:
		ss, e := t.translateRemoveLabel(p, scope)
		if e != nil {
			return nil, true, e
		}
		return ss, true, nil

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

	case *cypher.MergePlan:
		ss, e := t.translateMerge(p, scope)
		if e != nil {
			return nil, true, e
		}
		return ss, true, nil

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
	return Statement{SQL: sql, Args: args, Kind: KindInsertEdge, CreatedVar: p.RelVariable}, nil
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

// translateSetMerge emits:
//
//	UPDATE nodes SET props = json_set(props, '$.k1', ?, '$.k2', ?, ...) WHERE id = ?
//
// json_set adds/updates each key without removing keys not mentioned in the map,
// satisfying SET n += {map} merge semantics. Parameters and literals both work
// since each value is a separate bind argument.
func (t *Translator) translateSetMerge(p *cypher.SetMergePlan, scope *cypher.BindingScope) (Statement, error) {
	binding, ok := scope.Resolve(p.Variable)
	if !ok {
		return Statement{}, fmt.Errorf("sql: variable %q not in scope for SET +=", p.Variable)
	}
	if len(p.Props) == 0 {
		return Statement{}, fmt.Errorf("sql: SET += requires at least one property")
	}

	// Build json_set(props, '$.k1', ?, '$.k2', ?, ...) with stable key order.
	keys := make([]string, 0, len(p.Props))
	for k := range p.Props {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var jsonSetParts []string
	var args []any
	jsonSetParts = append(jsonSetParts, "props")
	for _, k := range keys {
		expr := p.Props[k]
		valT := &Translator{dialect: t.dialect}
		valSQL, err := valT.exprToSQL(expr, scope)
		if err != nil {
			return Statement{}, fmt.Errorf("sql: SET += prop %q: %w", k, err)
		}
		jsonSetParts = append(jsonSetParts, fmt.Sprintf("'$.%s'", k), valSQL)
		args = append(args, valT.args...)
	}

	table := "nodes"
	if binding.IsRel {
		table = "edges"
	}

	sentinel := idSentinel{VarName: p.Variable, Alias: binding.Alias}
	jsonSetExpr := fmt.Sprintf("json_set(%s)", strings.Join(jsonSetParts, ", "))
	sqlStr := fmt.Sprintf("UPDATE %s SET props = %s WHERE id = ?", table, jsonSetExpr)
	return Statement{
		SQL:  sqlStr,
		Args: append(args, sentinel),
		Kind: KindUpdate,
	}, nil
}

// translateRemoveProp emits:
//
//	UPDATE nodes SET props = json_remove(props, '$.prop') WHERE id = ?
func (t *Translator) translateRemoveProp(p *cypher.RemovePropPlan, scope *cypher.BindingScope) (Statement, error) {
	binding, ok := scope.Resolve(p.Variable)
	if !ok {
		return Statement{}, fmt.Errorf("sql: variable %q not in scope for REMOVE prop", p.Variable)
	}

	removeExpr := t.dialect.JSONRemove("props", "$."+p.Property)
	table := "nodes"
	if binding.IsRel {
		table = "edges"
	}

	sentinel := idSentinel{VarName: p.Variable, Alias: binding.Alias}
	sqlStr := fmt.Sprintf("UPDATE %s SET props = %s WHERE id = ?", table, removeExpr)
	return Statement{SQL: sqlStr, Args: []any{sentinel}, Kind: KindUpdate}, nil
}

// translateRemoveLabel emits one UPDATE per label to remove, using:
//
//	UPDATE nodes SET labels = TRIM(REPLACE(',' || labels || ',', ',' || ? || ',', ','), ',') WHERE id = ?
//
// Wrapping with commas before REPLACE ensures correct boundary matching for all
// positions (first, last, middle, only).
func (t *Translator) translateRemoveLabel(p *cypher.RemoveLabelPlan, scope *cypher.BindingScope) ([]Statement, error) {
	binding, ok := scope.Resolve(p.Variable)
	if !ok {
		return nil, fmt.Errorf("sql: variable %q not in scope for REMOVE label", p.Variable)
	}
	if binding.IsRel {
		return nil, fmt.Errorf("sql: REMOVE label is not supported for relationships")
	}

	sentinel := idSentinel{VarName: p.Variable, Alias: binding.Alias}
	var stmts []Statement
	for _, label := range p.Labels {
		sqlStr := "UPDATE nodes SET labels = TRIM(REPLACE(',' || labels || ',', ',' || ? || ',', ','), ',') WHERE id = ?"
		stmts = append(stmts, Statement{
			SQL:  sqlStr,
			Args: []any{label, sentinel},
			Kind: KindUpdate,
		})
	}
	return stmts, nil
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

// translateMerge emits the SQL statements for a MERGE clause.
//
// The emitted statement sequence is:
//
//	[0]   KindMergeCheck  SELECT <alias>.id FROM nodes <alias> WHERE … LIMIT 1
//	[1]   KindMergeInsert INSERT INTO nodes (labels, props) VALUES (?, json(?))
//	[2..] KindUpdate      ON CREATE SET stmts (CreatedVar prefixed "oncreate:")
//	[n..] KindUpdate      ON MATCH SET stmts  (CreatedVar prefixed "onmatch:")
//
// The execution layer (execMergeBatch) decides at runtime which branch to run:
//   - MergeCheck returns a row  → populate idMap from existingID, run OnMatch SETs only.
//   - MergeCheck returns no row → INSERT the node, populate idMap from LastInsertId,
//     then run OnCreate SETs only.
//
// All steps are wrapped in a transaction by executeStatements when ex is a *stdsql.DB.
func (t *Translator) translateMerge(p *cypher.MergePlan, scope *cypher.BindingScope) ([]Statement, error) {
	// ── 1. Build the existence-check SELECT ──────────────────────────────────
	// We need a fresh MatchNodePlan to reuse buildFromClauseForMatchNode.
	// Build label and prop WHERE fragments directly.
	var whereParts []string
	var whereArgs []any

	// We need a dummy alias for the check query. Look up scope.
	binding, hasBound := scope.Resolve(p.Variable)
	var alias string
	if hasBound && binding.Alias != "" {
		alias = binding.Alias
	} else {
		alias = "m0"
	}

	for _, label := range p.Labels {
		pred, args := t.dialect.LabelContains(alias+".labels", label)
		whereParts = append(whereParts, pred)
		whereArgs = append(whereArgs, args...)
	}
	// Sort prop keys for deterministic SQL.
	propKeys := make([]string, 0, len(p.Props))
	for k := range p.Props {
		propKeys = append(propKeys, k)
	}
	sort.Strings(propKeys)

	// Collect any external variable aliases referenced by MERGE prop value
	// expressions (e.g. person.bornIn references alias n0). These need to be
	// present in the MERGE check FROM clause so the property can be resolved.
	// We deduplicate by alias and emit CROSS JOIN nodes <alias> WHERE <alias>.id = ?
	// with an idSentinel so the execution layer resolves the ID from idMap.
	type externalRef struct {
		varName string
		alias   string
	}
	seenExternal := make(map[string]bool)
	var externalRefs []externalRef

	for _, key := range propKeys {
		expr := p.Props[key]
		sub := &Translator{dialect: t.dialect}
		valSQL, err := sub.exprToSQL(expr, scope)
		if err != nil {
			return nil, fmt.Errorf("sql: MERGE check prop %q: %w", key, err)
		}
		whereParts = append(whereParts, t.dialect.JSONExtract(alias+".props", "$."+key)+" = "+valSQL)
		whereArgs = append(whereArgs, sub.args...)

		// Collect external variable references from this prop's expression.
		if pe, ok := expr.(*cypher.PropExpr); ok {
			if pe.Variable != p.Variable {
				b, ok := scope.Resolve(pe.Variable)
				if ok && b.IsNode && !seenExternal[b.Alias] && b.Alias != alias {
					seenExternal[b.Alias] = true
					externalRefs = append(externalRefs, externalRef{varName: pe.Variable, alias: b.Alias})
				}
			}
		}
	}

	// Build the FROM clause: primary MERGE node table, plus any externally
	// referenced node tables needed to resolve prop value expressions.
	var checkSQL strings.Builder
	fmt.Fprintf(&checkSQL, "SELECT %s.id FROM nodes %s", alias, alias)
	var checkExtraArgs []any
	for _, ref := range externalRefs {
		fmt.Fprintf(&checkSQL, " JOIN nodes %s ON %s.id = ?", ref.alias, ref.alias)
		checkExtraArgs = append(checkExtraArgs, idSentinel{VarName: ref.varName, Alias: ref.alias})
	}
	// Combine: external JOIN args first (they appear before WHERE in SQL), then
	// the regular WHERE args (label checks and prop comparisons).
	var checkArgs []any
	checkArgs = append(checkArgs, checkExtraArgs...)
	checkArgs = append(checkArgs, whereArgs...)

	if len(whereParts) > 0 {
		checkSQL.WriteString(" WHERE ")
		checkSQL.WriteString(strings.Join(whereParts, " AND "))
	}
	checkSQL.WriteString(" LIMIT 1")

	checkStmt := Statement{
		SQL:        checkSQL.String(),
		Args:       checkArgs,
		Kind:       KindMergeCheck,
		CreatedVar: p.Variable,
	}

	// ── 2. Build the INSERT statement (used when node does not exist) ────────
	labels := strings.Join(p.Labels, ",")
	propsJSON, propsArgs, err := t.buildPropsJSON(p.Props, scope)
	if err != nil {
		return nil, fmt.Errorf("sql: MERGE insert props: %w", err)
	}
	var insertStmt Statement
	if len(externalRefs) > 0 {
		// When the MERGE props reference external node variables (e.g.
		// MERGE (city {name: person.bornIn})), we can't use a plain VALUES
		// INSERT because the external variable's props are not in scope.
		// Instead, emit an INSERT … SELECT that JOINs against the external
		// node(s), resolving their props via the id = ? condition.
		var insertSQL strings.Builder
		fmt.Fprintf(&insertSQL, "INSERT INTO nodes (labels, props) SELECT ?, json(%s) FROM", propsJSON)
		var joinArgs []any
		for i, ref := range externalRefs {
			if i == 0 {
				fmt.Fprintf(&insertSQL, " nodes %s", ref.alias)
			} else {
				fmt.Fprintf(&insertSQL, ", nodes %s", ref.alias)
			}
			joinArgs = append(joinArgs, idSentinel{VarName: ref.varName, Alias: ref.alias})
		}
		var whereParts2 []string
		for _, ref := range externalRefs {
			whereParts2 = append(whereParts2, fmt.Sprintf("%s.id = ?", ref.alias))
		}
		if len(whereParts2) > 0 {
			fmt.Fprintf(&insertSQL, " WHERE %s", strings.Join(whereParts2, " AND "))
		}
		var insertArgs []any
		insertArgs = append(insertArgs, labels)
		insertArgs = append(insertArgs, propsArgs...)
		insertArgs = append(insertArgs, joinArgs...)
		insertStmt = Statement{
			SQL:        insertSQL.String(),
			Args:       insertArgs,
			Kind:       KindMergeInsert,
			CreatedVar: p.Variable,
		}
	} else {
		var insertArgs []any
		insertArgs = append(insertArgs, labels)
		insertArgs = append(insertArgs, propsArgs...)
		insertStmt = Statement{
			SQL:        fmt.Sprintf("INSERT INTO nodes (labels, props) VALUES (?, json(%s))", propsJSON),
			Args:       insertArgs,
			Kind:       KindMergeInsert,
			CreatedVar: p.Variable,
		}
	}

	// ── 3. Build ON CREATE SET statements ───────────────────────────────────
	var onCreateStmts []Statement
	for i := range p.OnCreate {
		sp := &p.OnCreate[i]
		stmt, err := t.translateSetProp(sp, scope)
		if err != nil {
			return nil, fmt.Errorf("sql: MERGE ON CREATE SET: %w", err)
		}
		// Tag as ON CREATE by prepending "C:" to CreatedVar (checked by executor).
		stmt.CreatedVar = "oncreate:" + stmt.CreatedVar
		onCreateStmts = append(onCreateStmts, stmt)
	}

	// ── 4. Build ON MATCH SET statements ────────────────────────────────────
	var onMatchStmts []Statement
	for i := range p.OnMatch {
		sp := &p.OnMatch[i]
		stmt, err := t.translateSetProp(sp, scope)
		if err != nil {
			return nil, fmt.Errorf("sql: MERGE ON MATCH SET: %w", err)
		}
		// Tag as ON MATCH by prepending "M:" to CreatedVar.
		stmt.CreatedVar = "onmatch:" + stmt.CreatedVar
		onMatchStmts = append(onMatchStmts, stmt)
	}

	// Return all statements in order: check, insert, onCreateStmts, onMatchStmts.
	// The execution layer uses the Kind field to dispatch each statement correctly.
	stmts := []Statement{checkStmt, insertStmt}
	stmts = append(stmts, onCreateStmts...)
	stmts = append(stmts, onMatchStmts...)
	return stmts, nil
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

// collectPlanAliases walks a plan tree and collects all SQL table aliases that
// will be present in the FROM/JOIN clause when the plan is translated. This is
// used by buildMatchForWriteSelect to filter out scope variables whose aliases
// were introduced by write operations (CREATE / MERGE) and therefore do not
// have a corresponding table row in the read-phase SELECT.
func collectPlanAliases(plan cypher.LogicalPlan, aliases map[string]bool) {
	if plan == nil {
		return
	}
	switch p := plan.(type) {
	case *cypher.MatchNodePlan:
		if p.SQLAlias != "" {
			aliases[p.SQLAlias] = true
		}
	case *cypher.MatchRelPlan:
		if p.StartNode.SQLAlias != "" {
			aliases[p.StartNode.SQLAlias] = true
		}
		if p.RelSQLAlias != "" {
			aliases[p.RelSQLAlias] = true
		}
		if p.EndNode.SQLAlias != "" {
			aliases[p.EndNode.SQLAlias] = true
		}
	case *cypher.VariableLengthRelPlan:
		if p.StartNode.SQLAlias != "" {
			aliases[p.StartNode.SQLAlias] = true
		}
		if p.EndNode.SQLAlias != "" {
			aliases[p.EndNode.SQLAlias] = true
		}
	case *cypher.FilterPlan:
		collectPlanAliases(p.Source, aliases)
	case *cypher.SequencePlan:
		for _, step := range p.Steps {
			collectPlanAliases(step, aliases)
		}
	case *cypher.WithPlan:
		collectPlanAliases(p.Source, aliases)
	}
	// Write plan nodes (CreateNodePlan, CreateRelPlan, MergePlan, DeleteNodePlan,
	// DeleteRelPlan, SetPropPlan, etc.) introduce no table aliases in the FROM
	// clause of the read-phase SELECT, so they are deliberately not listed here.
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
	// Assemble matchT.args in SQL order (JOIN ON before WHERE).
	matchT.args = append(matchT.args, fc.joinArgs...)
	matchT.args = append(matchT.args, fc.whereArgs...)
	matchT.args = append(matchT.args, fc.extraWhereArgs...)

	// Collect the SQL table aliases that are actually present in the FROM/JOIN
	// clause. We use these to filter scope variables: only variables whose alias
	// appears in the read-phase FROM/JOIN should be included in the SELECT list.
	// Variables introduced by write operations (CREATE, MERGE) have aliases that
	// are NOT in the FROM clause, so including them would cause "no such column"
	// SQL errors at execution time.
	readAliases := make(map[string]bool)
	for _, step := range readSteps {
		collectPlanAliases(step, readAliases)
	}

	// Build the SELECT list: one "<alias>.id AS <varName>" per named variable
	// whose alias is present in the read-phase FROM/JOIN clause.
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
		// Skip variables whose SQL alias is not in the read-phase FROM/JOIN.
		// These are write-introduced variables (e.g. anonymous CREATE nodes)
		// that have no table row in the read-phase SELECT.
		if len(readAliases) > 0 && !readAliases[b.Alias] {
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
