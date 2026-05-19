// Package cypher wraps the cloudprivacylabs/opencypher ANTLR parser and exposes
// a thin Parse function that produces the graphlite AST types defined in ast.go.
package cypher

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/antlr/antlr4/runtime/Go/antlr"
	"github.com/cloudprivacylabs/opencypher"
	"github.com/cloudprivacylabs/opencypher/parser"
)

// Parse parses a Cypher query string and returns the corresponding *Query AST.
//
// Supported for v0.1: single-part queries containing MATCH, CREATE, SET,
// DELETE/DETACH DELETE, and RETURN clauses. UNION and UNION ALL cause
// ErrUnsupportedCypher to be returned.
//
// WHERE clause expressions are preserved as raw text (WhereExpr field) and
// will be given a typed predicate tree in task-008.
//
// Parse is safe to call from multiple goroutines.
func Parse(input string) (*Query, error) {
	p := opencypher.GetParser(input)

	// Attach a custom error listener so syntax errors surface as Go errors
	// rather than printing to stderr and continuing.
	errLst := &errorCollector{}
	p.RemoveErrorListeners()
	p.AddErrorListener(errLst)

	tree := p.OC_Cypher()
	if errLst.err != nil {
		return nil, fmt.Errorf("cypher syntax error: %w", errLst.err)
	}

	return buildQuery(tree.(*parser.OC_CypherContext))
}

// ─── internal error listener ──────────────────────────────────────────────────

type errorCollector struct {
	antlr.DefaultErrorListener
	err error
}

func (e *errorCollector) SyntaxError(
	_ antlr.Recognizer,
	_ interface{},
	line, col int,
	msg string,
	_ antlr.RecognitionException,
) {
	if e.err == nil {
		e.err = fmt.Errorf("line %d:%d %s", line, col, msg)
	}
}

// ─── CST → AST builder ────────────────────────────────────────────────────────

func buildQuery(ctx *parser.OC_CypherContext) (*Query, error) {
	stmt := ctx.OC_Statement().(*parser.OC_StatementContext)
	queryCtx := stmt.OC_Query().(*parser.OC_QueryContext)

	regQ := queryCtx.OC_RegularQuery()
	if regQ == nil {
		return nil, fmt.Errorf("cypher: standalone CALL is not supported in v0.1")
	}
	rq := regQ.(*parser.OC_RegularQueryContext)

	// Reject UNION queries (GAP-004).
	if len(rq.AllOC_Union()) > 0 {
		return nil, fmt.Errorf("cypher: UNION is not supported in v0.1")
	}

	sq := rq.OC_SingleQuery().(*parser.OC_SingleQueryContext)
	return buildSingleQuery(sq)
}

func buildSingleQuery(ctx *parser.OC_SingleQueryContext) (*Query, error) {
	if spq := ctx.OC_SinglePartQuery(); spq != nil {
		return buildSinglePartQuery(spq.(*parser.OC_SinglePartQueryContext))
	}
	if mpq := ctx.OC_MultiPartQuery(); mpq != nil {
		return buildMultiPartQuery(mpq.(*parser.OC_MultiPartQueryContext))
	}
	return nil, fmt.Errorf("cypher: unrecognised query structure")
}

func buildSinglePartQuery(ctx *parser.OC_SinglePartQueryContext) (*Query, error) {
	q := &Query{}

	// Reading clauses (MATCH).
	for _, rc := range ctx.AllOC_ReadingClause() {
		clause, err := buildReadingClause(rc.(*parser.OC_ReadingClauseContext))
		if err != nil {
			return nil, err
		}
		q.Clauses = append(q.Clauses, clause)
	}

	// Updating clauses (CREATE, SET, DELETE).
	for _, uc := range ctx.AllOC_UpdatingClause() {
		clause, err := buildUpdatingClause(uc.(*parser.OC_UpdatingClauseContext))
		if err != nil {
			return nil, err
		}
		q.Clauses = append(q.Clauses, clause)
	}

	// RETURN clause.
	if ret := ctx.OC_Return(); ret != nil {
		rc, err := buildReturnClause(ret.(*parser.OC_ReturnContext))
		if err != nil {
			return nil, err
		}
		q.Clauses = append(q.Clauses, rc)
	}

	return q, nil
}

func buildMultiPartQuery(ctx *parser.OC_MultiPartQueryContext) (*Query, error) {
	// v0.1 does not support multi-part queries (WITH pipelines).
	// Task-024 adds WITH support. For now, surface a useful error.
	return nil, fmt.Errorf("cypher: multi-part queries (WITH pipelines) are not supported in v0.1")
}

// ─── reading clauses ──────────────────────────────────────────────────────────

func buildReadingClause(ctx *parser.OC_ReadingClauseContext) (Clause, error) {
	if m := ctx.OC_Match(); m != nil {
		return buildMatchClause(m.(*parser.OC_MatchContext))
	}
	return nil, fmt.Errorf("cypher: only MATCH is supported as a reading clause in v0.1 (got %q)", ctx.GetText())
}

func buildMatchClause(ctx *parser.OC_MatchContext) (*MatchClause, error) {
	mc := &MatchClause{
		Optional: ctx.OPTIONAL() != nil,
	}

	parts, err := buildPattern(ctx.OC_Pattern().(*parser.OC_PatternContext))
	if err != nil {
		return nil, err
	}
	mc.Pattern = parts

	if where := ctx.OC_Where(); where != nil {
		mc.WhereExpr = exprText(where.(*parser.OC_WhereContext).OC_Expression())
	}

	return mc, nil
}

// ─── updating clauses ─────────────────────────────────────────────────────────

func buildUpdatingClause(ctx *parser.OC_UpdatingClauseContext) (Clause, error) {
	if c := ctx.OC_Create(); c != nil {
		return buildCreateClause(c.(*parser.OC_CreateContext))
	}
	if s := ctx.OC_Set(); s != nil {
		return buildSetClause(s.(*parser.OC_SetContext))
	}
	if d := ctx.OC_Delete(); d != nil {
		return buildDeleteClause(d.(*parser.OC_DeleteContext))
	}
	return nil, fmt.Errorf("cypher: unsupported updating clause %q in v0.1", ctx.GetText())
}

func buildCreateClause(ctx *parser.OC_CreateContext) (*CreateClause, error) {
	parts, err := buildPattern(ctx.OC_Pattern().(*parser.OC_PatternContext))
	if err != nil {
		return nil, err
	}
	return &CreateClause{Pattern: parts}, nil
}

func buildSetClause(ctx *parser.OC_SetContext) (*SetClause, error) {
	sc := &SetClause{}
	for _, item := range ctx.AllOC_SetItem() {
		si, err := buildSetItem(item.(*parser.OC_SetItemContext))
		if err != nil {
			return nil, err
		}
		sc.Items = append(sc.Items, si)
	}
	return sc, nil
}

func buildSetItem(ctx *parser.OC_SetItemContext) (SetItem, error) {
	// We only support the form: n.prop = expr
	propExpr := ctx.OC_PropertyExpression()
	if propExpr == nil {
		return SetItem{}, fmt.Errorf("cypher: only 'variable.property = expr' SET items are supported in v0.1 (got %q)", ctx.GetText())
	}
	pe := propExpr.(*parser.OC_PropertyExpressionContext)
	atom := pe.OC_Atom()
	if atom == nil {
		return SetItem{}, fmt.Errorf("cypher: SET item has no atom: %q", ctx.GetText())
	}

	varName := atom.(*parser.OC_AtomContext).OC_Variable()
	if varName == nil {
		return SetItem{}, fmt.Errorf("cypher: SET item atom is not a variable: %q", ctx.GetText())
	}

	lookups := pe.AllOC_PropertyLookup()
	if len(lookups) != 1 {
		return SetItem{}, fmt.Errorf("cypher: SET item must have exactly one property lookup (got %d): %q", len(lookups), ctx.GetText())
	}

	propKey := lookups[0].(*parser.OC_PropertyLookupContext).OC_PropertyKeyName()

	exprCtx := ctx.OC_Expression()
	if exprCtx == nil {
		return SetItem{}, fmt.Errorf("cypher: SET item has no expression: %q", ctx.GetText())
	}

	return SetItem{
		Variable: trimWhitespace(varName.GetText()),
		Property: trimWhitespace(propKey.GetText()),
		ExprText: exprText(exprCtx),
	}, nil
}

func buildDeleteClause(ctx *parser.OC_DeleteContext) (*DeleteClause, error) {
	dc := &DeleteClause{
		Detach: ctx.DETACH() != nil,
	}
	for _, expr := range ctx.AllOC_Expression() {
		dc.Exprs = append(dc.Exprs, exprText(expr))
	}
	return dc, nil
}

// ─── RETURN clause ────────────────────────────────────────────────────────────

func buildReturnClause(ctx *parser.OC_ReturnContext) (*ReturnClause, error) {
	pb := ctx.OC_ProjectionBody().(*parser.OC_ProjectionBodyContext)
	rc := &ReturnClause{
		Distinct: pb.DISTINCT() != nil,
	}

	// Projection items.
	items := pb.OC_ProjectionItems().(*parser.OC_ProjectionItemsContext)
	for _, pi := range items.AllOC_ProjectionItem() {
		item, err := buildReturnItem(pi.(*parser.OC_ProjectionItemContext))
		if err != nil {
			return nil, err
		}
		rc.Items = append(rc.Items, item)
	}

	// ORDER BY.
	if order := pb.OC_Order(); order != nil {
		for _, si := range order.(*parser.OC_OrderContext).AllOC_SortItem() {
			rc.OrderBy = append(rc.OrderBy, buildSortItem(si.(*parser.OC_SortItemContext)))
		}
	}

	// SKIP.
	if skip := pb.OC_Skip(); skip != nil {
		v, err := parseInt64Expr(skip.(*parser.OC_SkipContext).OC_Expression())
		if err != nil {
			return nil, fmt.Errorf("cypher: SKIP value must be a non-negative integer literal: %w", err)
		}
		rc.Skip = &v
	}

	// LIMIT.
	if limit := pb.OC_Limit(); limit != nil {
		v, err := parseInt64Expr(limit.(*parser.OC_LimitContext).OC_Expression())
		if err != nil {
			return nil, fmt.Errorf("cypher: LIMIT value must be a non-negative integer literal: %w", err)
		}
		rc.Limit = &v
	}

	return rc, nil
}

func buildReturnItem(ctx *parser.OC_ProjectionItemContext) (ReturnItem, error) {
	ri := ReturnItem{
		ExprText: exprText(ctx.OC_Expression()),
	}
	if alias := ctx.OC_Variable(); alias != nil {
		ri.Alias = trimWhitespace(alias.GetText())
	}
	return ri, nil
}

func buildSortItem(ctx *parser.OC_SortItemContext) SortItem {
	desc := ctx.DESCENDING() != nil || ctx.DESC() != nil
	return SortItem{
		ExprText:   exprText(ctx.OC_Expression()),
		Descending: desc,
	}
}

// ─── pattern helpers ──────────────────────────────────────────────────────────

func buildPattern(ctx *parser.OC_PatternContext) ([]PatternPart, error) {
	var parts []PatternPart
	for _, pp := range ctx.AllOC_PatternPart() {
		part, err := buildPatternPart(pp.(*parser.OC_PatternPartContext))
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func buildPatternPart(ctx *parser.OC_PatternPartContext) (PatternPart, error) {
	pp := PatternPart{}

	if v := ctx.OC_Variable(); v != nil {
		pp.Variable = trimWhitespace(v.GetText())
	}

	anonPart := ctx.OC_AnonymousPatternPart().(*parser.OC_AnonymousPatternPartContext)
	elemCtx := anonPart.OC_PatternElement().(*parser.OC_PatternElementContext)

	// Unwrap nested parentheses: OC_PatternElement can be (OC_PatternElement).
	for elemCtx.OC_PatternElement() != nil {
		elemCtx = elemCtx.OC_PatternElement().(*parser.OC_PatternElementContext)
	}

	// Start node.
	nodeCtx := elemCtx.OC_NodePattern()
	if nodeCtx == nil {
		return PatternPart{}, fmt.Errorf("cypher: pattern element has no start node: %q", ctx.GetText())
	}
	pp.Start = buildNodePattern(nodeCtx.(*parser.OC_NodePatternContext))

	// Chain: alternating relationship + node.
	for _, chain := range elemCtx.AllOC_PatternElementChain() {
		ch, err := buildPatternChain(chain.(*parser.OC_PatternElementChainContext))
		if err != nil {
			return PatternPart{}, err
		}
		pp.Chain = append(pp.Chain, ch)
	}

	return pp, nil
}

func buildPatternChain(ctx *parser.OC_PatternElementChainContext) (PatternChain, error) {
	rel, err := buildRelPattern(ctx.OC_RelationshipPattern().(*parser.OC_RelationshipPatternContext))
	if err != nil {
		return PatternChain{}, err
	}
	node := buildNodePattern(ctx.OC_NodePattern().(*parser.OC_NodePatternContext))
	return PatternChain{Rel: rel, Node: node}, nil
}

func buildNodePattern(ctx *parser.OC_NodePatternContext) NodePattern {
	np := NodePattern{
		Props: make(map[string]string),
	}
	if v := ctx.OC_Variable(); v != nil {
		np.Variable = trimWhitespace(v.GetText())
	}
	if labels := ctx.OC_NodeLabels(); labels != nil {
		for _, lbl := range labels.(*parser.OC_NodeLabelsContext).AllOC_NodeLabel() {
			name := lbl.(*parser.OC_NodeLabelContext).OC_LabelName()
			np.Labels = append(np.Labels, trimWhitespace(name.GetText()))
		}
	}
	if props := ctx.OC_Properties(); props != nil {
		np.Props = buildProperties(props.(*parser.OC_PropertiesContext))
	}
	return np
}

func buildRelPattern(ctx *parser.OC_RelationshipPatternContext) (RelPattern, error) {
	rp := RelPattern{
		ToLeft:  ctx.OC_LeftArrowHead() != nil,
		ToRight: ctx.OC_RightArrowHead() != nil,
		Props:   make(map[string]string),
	}

	if detail := ctx.OC_RelationshipDetail(); detail != nil {
		d := detail.(*parser.OC_RelationshipDetailContext)

		if v := d.OC_Variable(); v != nil {
			rp.Variable = trimWhitespace(v.GetText())
		}

		if rt := d.OC_RelationshipTypes(); rt != nil {
			for _, typeName := range rt.(*parser.OC_RelationshipTypesContext).AllOC_RelTypeName() {
				rp.Types = append(rp.Types, trimWhitespace(typeName.GetText()))
			}
		}

		if d.OC_RangeLiteral() != nil {
			rp.VarLength = true
		}

		if props := d.OC_Properties(); props != nil {
			rp.Props = buildProperties(props.(*parser.OC_PropertiesContext))
		}
	}

	return rp, nil
}

func buildProperties(ctx *parser.OC_PropertiesContext) map[string]string {
	props := make(map[string]string)

	if mapLit := ctx.OC_MapLiteral(); mapLit != nil {
		ml := mapLit.(*parser.OC_MapLiteralContext)
		keys := ml.AllOC_PropertyKeyName()
		exprs := ml.AllOC_Expression()
		for i, key := range keys {
			if i < len(exprs) {
				props[trimWhitespace(key.GetText())] = exprText(exprs[i])
			}
		}
	}
	// If it's a parameter (Properties.Param), we encode it as a raw text entry
	// under the special key "$" so task-015 can detect and handle it.
	// Note: "$" is not a valid Cypher property key name (identifiers cannot start
	// with "$"), so this sentinel key cannot collide with a real property.
	if param := ctx.OC_Parameter(); param != nil {
		props["$"] = "$" + trimWhitespace(param.(*parser.OC_ParameterContext).OC_SymbolicName().GetText())
	}

	return props
}

// ─── expression text helpers ──────────────────────────────────────────────────

// exprText returns the raw source text of an expression context, with leading/
// trailing whitespace stripped. It accepts the IOC_ExpressionContext interface
// returned by all OC_Expression() accessors.
func exprText(ctx parser.IOC_ExpressionContext) string {
	if ctx == nil {
		return ""
	}
	return trimWhitespace(ctx.GetText())
}

// trimWhitespace removes leading and trailing whitespace from a CST text fragment.
// The ANTLR GetText() method concatenates all token texts without spaces; the
// original whitespace is carried in separate SP tokens that are not included in
// child GetText() results. As a result this is a no-op for most identifiers, but
// is kept for safety.
func trimWhitespace(s string) string {
	return strings.TrimSpace(s)
}

// parseInt64Expr parses a simple integer literal expression from the CST.
// Returns an error if the expression is not a plain integer literal.
func parseInt64Expr(ctx parser.IOC_ExpressionContext) (int64, error) {
	if ctx == nil {
		return 0, fmt.Errorf("nil expression")
	}
	text := trimWhitespace(ctx.GetText())
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("expected integer literal, got %q", text)
	}
	return v, nil
}
