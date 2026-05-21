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
// WHERE clause expressions are parsed into a typed Expr tree (ComparisonExpr,
// BoolExpr, NotExpr, ParamRef, PropExpr, LiteralExpr). Unsupported sub-expression
// forms fall back to RawExpr without error.
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
	_ any,
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
	// Only support single-stage WITH pipelines for now.
	if len(ctx.AllOC_With()) > 1 {
		return nil, fmt.Errorf("cypher: multiple WITH stages are not yet supported")
	}

	q := &Query{}

	// Reading clauses (MATCH) that appear before the WITH.
	for _, rc := range ctx.AllOC_ReadingClause() {
		clause, err := buildReadingClause(rc.(*parser.OC_ReadingClauseContext))
		if err != nil {
			return nil, err
		}
		q.Clauses = append(q.Clauses, clause)
	}

	// Updating clauses (CREATE, SET, DELETE) before WITH — uncommon but grammar allows it.
	for _, uc := range ctx.AllOC_UpdatingClause() {
		clause, err := buildUpdatingClause(uc.(*parser.OC_UpdatingClauseContext))
		if err != nil {
			return nil, err
		}
		q.Clauses = append(q.Clauses, clause)
	}

	// WITH clause(s).
	for _, w := range ctx.AllOC_With() {
		wc, err := buildWithClause(w.(*parser.OC_WithContext))
		if err != nil {
			return nil, err
		}
		q.Clauses = append(q.Clauses, wc)
	}

	// Final single-part query (RETURN, etc.).
	if sp := ctx.OC_SinglePartQuery(); sp != nil {
		finalQ, err := buildSinglePartQuery(sp.(*parser.OC_SinglePartQueryContext))
		if err != nil {
			return nil, err
		}
		q.Clauses = append(q.Clauses, finalQ.Clauses...)
	}

	return q, nil
}

// buildWithClause parses an OC_WithContext into a WithClause AST node.
func buildWithClause(ctx *parser.OC_WithContext) (*WithClause, error) {
	pb := ctx.OC_ProjectionBody().(*parser.OC_ProjectionBodyContext)
	wc := &WithClause{
		Distinct: pb.DISTINCT() != nil,
	}

	items := pb.OC_ProjectionItems().(*parser.OC_ProjectionItemsContext)
	for _, pi := range items.AllOC_ProjectionItem() {
		item, err := buildReturnItem(pi.(*parser.OC_ProjectionItemContext))
		if err != nil {
			return nil, err
		}
		wc.Items = append(wc.Items, item)
	}

	if order := pb.OC_Order(); order != nil {
		for _, si := range order.(*parser.OC_OrderContext).AllOC_SortItem() {
			wc.OrderBy = append(wc.OrderBy, buildSortItem(si.(*parser.OC_SortItemContext)))
		}
	}

	if skip := pb.OC_Skip(); skip != nil {
		v, err := parseInt64Expr(skip.(*parser.OC_SkipContext).OC_Expression())
		if err != nil {
			return nil, fmt.Errorf("cypher: WITH SKIP: %w", err)
		}
		wc.Skip = &v
	}

	if limit := pb.OC_Limit(); limit != nil {
		v, err := parseInt64Expr(limit.(*parser.OC_LimitContext).OC_Expression())
		if err != nil {
			return nil, fmt.Errorf("cypher: WITH LIMIT: %w", err)
		}
		wc.Limit = &v
	}

	// Post-WITH WHERE (becomes HAVING in SQL when aggregates are present).
	if where := ctx.OC_Where(); where != nil {
		expr, err := buildExprFromCST(where.(*parser.OC_WhereContext).OC_Expression())
		if err != nil {
			return nil, fmt.Errorf("cypher: WITH WHERE: %w", err)
		}
		wc.Where = expr
	}

	return wc, nil
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
		whereExpr, err := buildExprFromCST(where.(*parser.OC_WhereContext).OC_Expression())
		if err != nil {
			return nil, fmt.Errorf("cypher: WHERE clause: %w", err)
		}
		mc.Where = whereExpr
	}

	return mc, nil
}

// ─── updating clauses ─────────────────────────────────────────────────────────

func buildUpdatingClause(ctx *parser.OC_UpdatingClauseContext) (Clause, error) {
	if c := ctx.OC_Create(); c != nil {
		return buildCreateClause(c.(*parser.OC_CreateContext))
	}
	if m := ctx.OC_Merge(); m != nil {
		return buildMergeClause(m.(*parser.OC_MergeContext))
	}
	if s := ctx.OC_Set(); s != nil {
		return buildSetClause(s.(*parser.OC_SetContext))
	}
	if d := ctx.OC_Delete(); d != nil {
		return buildDeleteClause(d.(*parser.OC_DeleteContext))
	}
	if r := ctx.OC_Remove(); r != nil {
		return buildRemoveClause(r.(*parser.OC_RemoveContext))
	}
	return nil, fmt.Errorf("cypher: unsupported updating clause %q in v0.1", ctx.GetText())
}

// buildMergeClause parses an OC_MergeContext into a MergeClause AST node.
// Grammar: MERGE OC_PatternPart (ON CREATE OC_Set | ON MATCH OC_Set)*
func buildMergeClause(ctx *parser.OC_MergeContext) (*MergeClause, error) {
	part, err := buildPatternPart(ctx.OC_PatternPart().(*parser.OC_PatternPartContext))
	if err != nil {
		return nil, fmt.Errorf("cypher: MERGE pattern: %w", err)
	}

	mc := &MergeClause{Pattern: part}

	for _, action := range ctx.AllOC_MergeAction() {
		a := action.(*parser.OC_MergeActionContext)
		setCtx := a.OC_Set()
		if setCtx == nil {
			continue
		}
		sc, err := buildSetClause(setCtx.(*parser.OC_SetContext))
		if err != nil {
			return nil, fmt.Errorf("cypher: MERGE action SET: %w", err)
		}
		// Distinguish ON CREATE vs ON MATCH by checking which token is present.
		if a.CREATE() != nil {
			mc.OnCreate = append(mc.OnCreate, sc.Items...)
		} else {
			// MATCH token → ON MATCH
			mc.OnMatch = append(mc.OnMatch, sc.Items...)
		}
	}

	return mc, nil
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
	// Form 1: n.prop = expr  (OC_PropertyExpression present)
	if propExpr := ctx.OC_PropertyExpression(); propExpr != nil {
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

	// Forms 2, 3, 4 all have OC_Variable.
	varCtx := ctx.OC_Variable()
	if varCtx == nil {
		return SetItem{}, fmt.Errorf("cypher: unrecognised SET item form: %q", ctx.GetText())
	}
	varName := trimWhitespace(varCtx.GetText())

	// Form 3: n += {map}
	// The grammar uses T__3 (the "+=" token). We detect it by inspecting the raw
	// context text: after stripping the variable and surrounding spaces, the
	// text begins with "+=".
	fullText := ctx.GetText()
	afterVar := strings.TrimPrefix(fullText, varName)
	afterVar = strings.TrimLeft(afterVar, " \t")
	if strings.HasPrefix(afterVar, "+=") {
		// Extract the map expression from OC_Expression.
		exprCtx := ctx.OC_Expression()
		if exprCtx == nil {
			return SetItem{}, fmt.Errorf("cypher: SET += item has no expression: %q", fullText)
		}
		props := buildPropertiesFromExprText(exprCtx)
		return SetItem{
			Variable: varName,
			Merge:    true,
			Props:    props,
		}, nil
	}

	return SetItem{}, fmt.Errorf("cypher: only 'variable.property = expr' and 'variable += {map}' SET items are supported (got %q)", ctx.GetText())
}

// buildPropertiesFromExprText attempts to parse a map-literal expression from
// an OC_ExpressionContext and return it as a map[string]string. This is used
// for SET n += {map} where the RHS must be a map literal. If the expression is
// not a map literal, an empty map is returned (the translator will handle it as
// a no-op or fall back gracefully).
func buildPropertiesFromExprText(exprCtx parser.IOC_ExpressionContext) map[string]string {
	props := make(map[string]string)
	if exprCtx == nil {
		return props
	}
	// Drill down the expression hierarchy to find a map literal:
	// OC_Expression → OC_OrExpression → ... → OC_Atom → OC_MapLiteral
	orCtx := exprCtx.(*parser.OC_ExpressionContext).OC_OrExpression()
	if orCtx == nil {
		return props
	}
	xors := orCtx.(*parser.OC_OrExpressionContext).AllOC_XorExpression()
	if len(xors) != 1 {
		return props
	}
	ands := xors[0].(*parser.OC_XorExpressionContext).AllOC_AndExpression()
	if len(ands) != 1 {
		return props
	}
	nots := ands[0].(*parser.OC_AndExpressionContext).AllOC_NotExpression()
	if len(nots) != 1 {
		return props
	}
	cmpCtx := nots[0].(*parser.OC_NotExpressionContext).OC_ComparisonExpression()
	if cmpCtx == nil {
		return props
	}
	addSub := cmpCtx.(*parser.OC_ComparisonExpressionContext).OC_AddOrSubtractExpression()
	if addSub == nil {
		return props
	}
	mulDivs := addSub.(*parser.OC_AddOrSubtractExpressionContext).AllOC_MultiplyDivideModuloExpression()
	if len(mulDivs) != 1 {
		return props
	}
	powers := mulDivs[0].(*parser.OC_MultiplyDivideModuloExpressionContext).AllOC_PowerOfExpression()
	if len(powers) != 1 {
		return props
	}
	unarys := powers[0].(*parser.OC_PowerOfExpressionContext).AllOC_UnaryAddOrSubtractExpression()
	if len(unarys) != 1 {
		return props
	}
	slnCtx := unarys[0].(*parser.OC_UnaryAddOrSubtractExpressionContext).OC_StringListNullOperatorExpression()
	if slnCtx == nil {
		return props
	}
	propLabelsCtx := slnCtx.(*parser.OC_StringListNullOperatorExpressionContext).OC_PropertyOrLabelsExpression()
	if propLabelsCtx == nil {
		return props
	}
	atomCtx := propLabelsCtx.(*parser.OC_PropertyOrLabelsExpressionContext).OC_Atom()
	if atomCtx == nil {
		return props
	}
	litCtx := atomCtx.(*parser.OC_AtomContext).OC_Literal()
	if litCtx == nil {
		return props
	}
	mapLitCtx := litCtx.(*parser.OC_LiteralContext).OC_MapLiteral()
	if mapLitCtx == nil {
		return props
	}
	ml := mapLitCtx.(*parser.OC_MapLiteralContext)
	keys := ml.AllOC_PropertyKeyName()
	exprs := ml.AllOC_Expression()
	for i, key := range keys {
		if i < len(exprs) {
			props[trimWhitespace(key.GetText())] = exprText(exprs[i])
		}
	}
	return props
}

// buildRemoveClause parses an OC_RemoveContext into a RemoveClause AST node.
func buildRemoveClause(ctx *parser.OC_RemoveContext) (*RemoveClause, error) {
	rc := &RemoveClause{}
	for _, item := range ctx.AllOC_RemoveItem() {
		ri, err := buildRemoveItem(item.(*parser.OC_RemoveItemContext))
		if err != nil {
			return nil, err
		}
		rc.Items = append(rc.Items, ri)
	}
	return rc, nil
}

// buildRemoveItem parses a single OC_RemoveItemContext.
// Two forms:
//   - variable NodeLabels → REMOVE n:Label
//   - PropertyExpression  → REMOVE n.prop
func buildRemoveItem(ctx *parser.OC_RemoveItemContext) (RemoveItem, error) {
	// Form: variable NodeLabels → REMOVE n:Label
	if labelsCtx := ctx.OC_NodeLabels(); labelsCtx != nil {
		varCtx := ctx.OC_Variable()
		if varCtx == nil {
			return RemoveItem{}, fmt.Errorf("cypher: REMOVE label item has no variable: %q", ctx.GetText())
		}
		varName := trimWhitespace(varCtx.GetText())
		var labels []string
		for _, lbl := range labelsCtx.(*parser.OC_NodeLabelsContext).AllOC_NodeLabel() {
			name := lbl.(*parser.OC_NodeLabelContext).OC_LabelName()
			labels = append(labels, trimWhitespace(name.GetText()))
		}
		return RemoveItem{Variable: varName, IsProp: false, Labels: labels}, nil
	}

	// Form: PropertyExpression → REMOVE n.prop
	if propExprCtx := ctx.OC_PropertyExpression(); propExprCtx != nil {
		pe := propExprCtx.(*parser.OC_PropertyExpressionContext)
		atom := pe.OC_Atom()
		if atom == nil {
			return RemoveItem{}, fmt.Errorf("cypher: REMOVE property item has no atom: %q", ctx.GetText())
		}
		varCtx := atom.(*parser.OC_AtomContext).OC_Variable()
		if varCtx == nil {
			return RemoveItem{}, fmt.Errorf("cypher: REMOVE property item atom is not a variable: %q", ctx.GetText())
		}
		lookups := pe.AllOC_PropertyLookup()
		if len(lookups) != 1 {
			return RemoveItem{}, fmt.Errorf("cypher: REMOVE property item must have exactly one property lookup (got %d): %q", len(lookups), ctx.GetText())
		}
		propKey := lookups[0].(*parser.OC_PropertyLookupContext).OC_PropertyKeyName()
		return RemoveItem{
			Variable: trimWhitespace(varCtx.GetText()),
			IsProp:   true,
			Property: trimWhitespace(propKey.GetText()),
		}, nil
	}

	return RemoveItem{}, fmt.Errorf("cypher: unrecognised REMOVE item: %q", ctx.GetText())
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
	// Parse typed expression for aggregation and other complex forms.
	// Errors are silently ignored so legacy fallback (ExprText) is always available.
	if expr, err := buildExprFromCST(ctx.OC_Expression()); err == nil {
		ri.Expr = expr
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

		if rl := d.OC_RangeLiteral(); rl != nil {
			rp.VarLength = true
			rp.MinHops, rp.MaxHops = parseRangeLiteral(rl.GetText())
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

// parseRangeLiteral parses the raw text of an OC_RangeLiteral (the part after
// the '*' in a variable-length relationship pattern) and returns the min and max
// hop counts.
//
// The raw text from the ANTLR CST has whitespace stripped by GetText(); the
// leading '*' is always present. Examples:
//
//	"*"      → min=1, max=0 (unbounded)
//	"*2"     → min=2, max=2 (exactly 2)
//	"*2..5"  → min=2, max=5
//	"*..5"   → min=1, max=5
//	"*2.."   → min=2, max=0 (unbounded)
func parseRangeLiteral(text string) (min, max int) {
	// Strip leading '*'.
	s := strings.TrimPrefix(text, "*")
	if s == "" {
		// [*] — one or more hops, unbounded.
		return 1, 0
	}

	dotIdx := strings.Index(s, "..")
	if dotIdx < 0 {
		// No ".." — single integer means exactly N hops.
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return 1, 0 // fallback
		}
		return n, n
	}

	// Has "..": split into left and right parts.
	left := s[:dotIdx]
	right := s[dotIdx+2:]

	if left == "" {
		min = 1
	} else {
		n, err := strconv.Atoi(left)
		if err != nil || n < 0 {
			min = 1
		} else {
			min = n
		}
	}
	if right == "" {
		max = 0 // unbounded
	} else {
		n, err := strconv.Atoi(right)
		if err != nil || n < 0 {
			max = 0
		} else {
			max = n
		}
	}
	return min, max
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

// ─── WHERE expression tree builder ───────────────────────────────────────────
//
// buildExprFromCST walks the ANTLR CST for an OC_Expression and produces a
// typed Expr node. Unsupported sub-expression forms fall back to RawExpr so
// that the tree is always returned; errors are only returned for malformed CST
// nodes (nil pointers from the grammar, not for unrecognised expressions).
//
// Grammar hierarchy (simplified for v0.1):
//
//	OC_Expression
//	  └── OC_OrExpression (one or more XOR-separated sub-expressions)
//	        └── OC_XorExpression (one or more AND-separated sub-expressions — we map XOR to OR for now via fallback)
//	              └── OC_AndExpression (one or more NOT-separated sub-expressions)
//	                    └── OC_NotExpression (optional NOT prefix, then ComparisonExpression)
//	                          └── OC_ComparisonExpression (left-hand AddOrSubtract + optional comparisons)
//	                                └── OC_PartialComparisonExpression (operator + right-hand side)
//	                                      └── OC_AddOrSubtractExpression → ... → OC_PropertyOrLabelsExpression → OC_Atom

// buildExprFromCST converts an ANTLR OC_ExpressionContext into a typed Expr.
func buildExprFromCST(ctx parser.IOC_ExpressionContext) (Expr, error) {
	if ctx == nil {
		return &RawExpr{Text: ""}, nil
	}
	orCtx := ctx.(*parser.OC_ExpressionContext).OC_OrExpression()
	if orCtx == nil {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	return buildOrExpr(orCtx.(*parser.OC_OrExpressionContext))
}

// buildOrExpr handles OC_OrExpression: one or more XOR-expressions joined by OR.
func buildOrExpr(ctx *parser.OC_OrExpressionContext) (Expr, error) {
	xors := ctx.AllOC_XorExpression()
	if len(xors) == 0 {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	left, err := buildXorExpr(xors[0].(*parser.OC_XorExpressionContext))
	if err != nil {
		return nil, err
	}
	for _, xCtx := range xors[1:] {
		right, err := buildXorExpr(xCtx.(*parser.OC_XorExpressionContext))
		if err != nil {
			return nil, err
		}
		left = &BoolExpr{Left: left, Op: "OR", Right: right}
	}
	return left, nil
}

// buildXorExpr handles OC_XorExpression: one or more AND-expressions joined by XOR.
// XOR is mapped to OR for v0.1 (it is uncommon in practice; a RawExpr would also work,
// but this keeps the tree typed). True XOR support is deferred.
func buildXorExpr(ctx *parser.OC_XorExpressionContext) (Expr, error) {
	ands := ctx.AllOC_AndExpression()
	if len(ands) == 0 {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	left, err := buildAndExpr(ands[0].(*parser.OC_AndExpressionContext))
	if err != nil {
		return nil, err
	}
	for _, aCtx := range ands[1:] {
		right, err := buildAndExpr(aCtx.(*parser.OC_AndExpressionContext))
		if err != nil {
			return nil, err
		}
		// XOR mapped to OR for v0.1; annotate via Op string.
		left = &BoolExpr{Left: left, Op: "XOR", Right: right}
	}
	return left, nil
}

// buildAndExpr handles OC_AndExpression: one or more NOT-expressions joined by AND.
func buildAndExpr(ctx *parser.OC_AndExpressionContext) (Expr, error) {
	nots := ctx.AllOC_NotExpression()
	if len(nots) == 0 {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	left, err := buildNotExpr(nots[0].(*parser.OC_NotExpressionContext))
	if err != nil {
		return nil, err
	}
	for _, nCtx := range nots[1:] {
		right, err := buildNotExpr(nCtx.(*parser.OC_NotExpressionContext))
		if err != nil {
			return nil, err
		}
		left = &BoolExpr{Left: left, Op: "AND", Right: right}
	}
	return left, nil
}

// buildNotExpr handles OC_NotExpression: optional NOT prefix followed by a
// ComparisonExpression.
func buildNotExpr(ctx *parser.OC_NotExpressionContext) (Expr, error) {
	// Count NOT tokens: even count = no effective negation; odd = negation.
	notCount := len(ctx.AllNOT())
	cmpCtx := ctx.OC_ComparisonExpression()
	if cmpCtx == nil {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	inner, err := buildComparisonExpr(cmpCtx.(*parser.OC_ComparisonExpressionContext))
	if err != nil {
		return nil, err
	}
	if notCount%2 == 1 {
		return &NotExpr{Expr: inner}, nil
	}
	return inner, nil
}

// buildComparisonExpr handles OC_ComparisonExpression:
// a left-hand AddOrSubtract expression followed by zero or more partial comparisons.
// Multiple partial comparisons (e.g. 1 < x < 10) are chained with AND.
func buildComparisonExpr(ctx *parser.OC_ComparisonExpressionContext) (Expr, error) {
	lhsCtx := ctx.OC_AddOrSubtractExpression()
	if lhsCtx == nil {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	lhs, err := buildAddOrSubtractExpr(lhsCtx.(*parser.OC_AddOrSubtractExpressionContext))
	if err != nil {
		return nil, err
	}

	parts := ctx.AllOC_PartialComparisonExpression()
	if len(parts) == 0 {
		// Pure value expression (no comparison operator) — just return the LHS.
		return lhs, nil
	}

	var result Expr
	for _, pCtx := range parts {
		partial := pCtx.(*parser.OC_PartialComparisonExpressionContext)
		op, rhs, err := buildPartialComparison(partial)
		if err != nil {
			return nil, err
		}
		cmp := &ComparisonExpr{Left: lhs, Op: op, Right: rhs}
		if result == nil {
			result = cmp
		} else {
			result = &BoolExpr{Left: result, Op: "AND", Right: cmp}
		}
	}
	return result, nil
}

// buildPartialComparison extracts the operator and right-hand side from a
// OC_PartialComparisonExpressionContext.
func buildPartialComparison(ctx *parser.OC_PartialComparisonExpressionContext) (string, Expr, error) {
	// The operator is the first token child before the RHS expression.
	// We reconstruct it from the context text by inspecting the first token.
	// The ANTLR grammar encodes comparison operators as T__2 (=), T__17 (<>),
	// T__18 (<), T__19 (>), T__20 (<=), T__21 (>=).
	// Use GetText() on the full context; the first non-space char(s) give the op.
	fullText := ctx.GetText()
	var op string
	switch {
	case strings.HasPrefix(fullText, "<>"):
		op = "<>"
	case strings.HasPrefix(fullText, "<="):
		op = "<="
	case strings.HasPrefix(fullText, ">="):
		op = ">="
	case strings.HasPrefix(fullText, "="):
		op = "="
	case strings.HasPrefix(fullText, "<"):
		op = "<"
	case strings.HasPrefix(fullText, ">"):
		op = ">"
	default:
		// Unrecognised operator — fall back.
		return "", &RawExpr{Text: fullText}, nil
	}

	rhsCtx := ctx.OC_AddOrSubtractExpression()
	if rhsCtx == nil {
		return op, &RawExpr{Text: ""}, nil
	}
	rhs, err := buildAddOrSubtractExpr(rhsCtx.(*parser.OC_AddOrSubtractExpressionContext))
	if err != nil {
		return op, nil, err
	}
	return op, rhs, nil
}

// buildAddOrSubtractExpr traverses the arithmetic expression hierarchy down to
// the terminal PropertyOrLabelsExpression. For v0.1 WHERE clauses we only need
// the simple forms (no arithmetic); arithmetic sub-expressions fall back to RawExpr.
func buildAddOrSubtractExpr(ctx *parser.OC_AddOrSubtractExpressionContext) (Expr, error) {
	mulDivs := ctx.AllOC_MultiplyDivideModuloExpression()
	if len(mulDivs) != 1 {
		// Arithmetic expression — not supported for v0.1; fall back.
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	return buildMulDivExpr(mulDivs[0].(*parser.OC_MultiplyDivideModuloExpressionContext))
}

// buildMulDivExpr traverses OC_MultiplyDivideModuloExpression down to PowerOfExpression.
func buildMulDivExpr(ctx *parser.OC_MultiplyDivideModuloExpressionContext) (Expr, error) {
	powers := ctx.AllOC_PowerOfExpression()
	if len(powers) != 1 {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	return buildPowerOfExpr(powers[0].(*parser.OC_PowerOfExpressionContext))
}

// buildPowerOfExpr traverses OC_PowerOfExpression down to UnaryAddOrSubtract.
func buildPowerOfExpr(ctx *parser.OC_PowerOfExpressionContext) (Expr, error) {
	unarys := ctx.AllOC_UnaryAddOrSubtractExpression()
	if len(unarys) != 1 {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	return buildUnaryExpr(unarys[0].(*parser.OC_UnaryAddOrSubtractExpressionContext))
}

// buildUnaryExpr traverses OC_UnaryAddOrSubtractExpression down to StringListNull.
func buildUnaryExpr(ctx *parser.OC_UnaryAddOrSubtractExpressionContext) (Expr, error) {
	slnCtx := ctx.OC_StringListNullOperatorExpression()
	if slnCtx == nil {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	// Unary minus / plus — fall back for v0.1.
	// Check for leading minus token in the text.
	text := trimWhitespace(ctx.GetText())
	if strings.HasPrefix(text, "-") || strings.HasPrefix(text, "+") {
		return &RawExpr{Text: text}, nil
	}
	return buildStringListNullExpr(slnCtx.(*parser.OC_StringListNullOperatorExpressionContext))
}

// buildStringListNullExpr handles OC_StringListNullOperatorExpression.
// Handles:
//   - IS NULL / IS NOT NULL → NullCheckExpr
//   - IN [...] → InListExpr
//   - STARTS WITH / ENDS WITH / CONTAINS → StringMatchExpr
//
// Other suffixes fall back to RawExpr.
func buildStringListNullExpr(ctx *parser.OC_StringListNullOperatorExpressionContext) (Expr, error) {
	propLabelsCtx := ctx.OC_PropertyOrLabelsExpression()
	if propLabelsCtx == nil {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	base, err := buildPropertyOrLabelsExpr(propLabelsCtx.(*parser.OC_PropertyOrLabelsExpressionContext))
	if err != nil {
		return nil, err
	}

	// Handle STARTS WITH / ENDS WITH / CONTAINS.
	strOps := ctx.AllOC_StringOperatorExpression()
	if len(strOps) == 1 {
		strOp := strOps[0].(*parser.OC_StringOperatorExpressionContext)
		var op string
		switch {
		case strOp.STARTS() != nil:
			op = "STARTS WITH"
		case strOp.ENDS() != nil:
			op = "ENDS WITH"
		case strOp.CONTAINS() != nil:
			op = "CONTAINS"
		default:
			return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
		}
		// The RHS is OC_PropertyOrLabelsExpression on the StringOperatorExpression.
		rhsCtx := strOp.OC_PropertyOrLabelsExpression()
		if rhsCtx == nil {
			return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
		}
		rhs, err := buildPropertyOrLabelsExpr(rhsCtx.(*parser.OC_PropertyOrLabelsExpressionContext))
		if err != nil {
			return nil, err
		}
		return &StringMatchExpr{Expr: base, Pattern: rhs, Op: op}, nil
	}
	if len(strOps) > 1 {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}

	// Handle IN list operator.
	listOps := ctx.AllOC_ListOperatorExpression()
	if len(listOps) == 1 {
		listOp := listOps[0].(*parser.OC_ListOperatorExpressionContext)
		if listOp.IN() != nil {
			// The RHS is OC_PropertyOrLabelsExpression: should be a list literal.
			rhsPropCtx := listOp.OC_PropertyOrLabelsExpression()
			if rhsPropCtx == nil {
				return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
			}
			// Unwrap to atom to find the list literal.
			listExprs, ok := extractListLiteralExprs(rhsPropCtx.(*parser.OC_PropertyOrLabelsExpressionContext))
			if !ok {
				// Fallback for variable references to list values.
				return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
			}
			var items []Expr
			for _, exprCtx := range listExprs {
				item, err := buildExprFromCST(exprCtx)
				if err != nil {
					return nil, err
				}
				items = append(items, item)
			}
			return &InListExpr{Expr: base, List: items}, nil
		}
		// Other list operators (subscript access) — fall back.
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	if len(listOps) > 1 {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}

	// Handle IS NULL / IS NOT NULL.
	nullOps := ctx.AllOC_NullOperatorExpression()
	if len(nullOps) == 1 {
		nullOp := nullOps[0].(*parser.OC_NullOperatorExpressionContext)
		return &NullCheckExpr{Expr: base, IsNotNull: nullOp.NOT() != nil}, nil
	}
	if len(nullOps) > 1 {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	return base, nil
}

// extractListLiteralExprs attempts to extract the expression list from a
// OC_PropertyOrLabelsExpressionContext that wraps a list literal atom.
// Returns the list of expressions and true if the atom is a list literal,
// false otherwise.
func extractListLiteralExprs(ctx *parser.OC_PropertyOrLabelsExpressionContext) ([]parser.IOC_ExpressionContext, bool) {
	atomCtx := ctx.OC_Atom()
	if atomCtx == nil {
		return nil, false
	}
	litCtx := atomCtx.(*parser.OC_AtomContext).OC_Literal()
	if litCtx == nil {
		return nil, false
	}
	listLit := litCtx.(*parser.OC_LiteralContext).OC_ListLiteral()
	if listLit == nil {
		return nil, false
	}
	return listLit.(*parser.OC_ListLiteralContext).AllOC_Expression(), true
}

// buildPropertyOrLabelsExpr handles OC_PropertyOrLabelsExpression:
// an atom optionally followed by one or more property lookups.
func buildPropertyOrLabelsExpr(ctx *parser.OC_PropertyOrLabelsExpressionContext) (Expr, error) {
	atomCtx := ctx.OC_Atom()
	if atomCtx == nil {
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
	atom, err := buildAtomExpr(atomCtx.(*parser.OC_AtomContext))
	if err != nil {
		return nil, err
	}

	lookups := ctx.AllOC_PropertyLookup()
	if len(lookups) == 0 {
		return atom, nil
	}
	// Single property lookup: n.prop
	if len(lookups) == 1 {
		varExpr, ok := atom.(*VarExpr)
		if !ok {
			// Not a simple variable — fall back.
			return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
		}
		propKey := trimWhitespace(lookups[0].(*parser.OC_PropertyLookupContext).OC_PropertyKeyName().GetText())
		return &PropExpr{Variable: varExpr.Name, Property: propKey}, nil
	}
	// Multiple lookups (nested property access) — fall back.
	return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
}

// buildFunctionInvocation parses a function call into an Expr.
// Aggregation functions (count, sum, avg, min, max) produce AggCallExpr.
// All other functions fall back to RawExpr.
func buildFunctionInvocation(ctx *parser.OC_FunctionInvocationContext) (Expr, error) {
	nameCtx := ctx.OC_FunctionName()
	funcName := strings.ToLower(trimWhitespace(nameCtx.GetText()))
	distinct := ctx.DISTINCT() != nil
	args := ctx.AllOC_Expression()

	switch funcName {
	case "count", "sum", "avg", "min", "max", "collect":
		if len(args) == 0 {
			// count() with no arguments → treat as count(*)
			return &AggCallExpr{Func: funcName, CountStar: funcName == "count", Distinct: distinct}, nil
		}
		arg, err := buildExprFromCST(args[0])
		if err != nil {
			return nil, err
		}
		return &AggCallExpr{Func: funcName, Arg: arg, Distinct: distinct}, nil
	case "exists":
		// exists(n.prop) → ExistsExpr
		if len(args) == 0 {
			return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
		}
		inner, err := buildExprFromCST(args[0])
		if err != nil {
			return nil, err
		}
		if pe, ok := inner.(*PropExpr); ok {
			return &ExistsExpr{Prop: pe}, nil
		}
		// Fallback: treat as IS NOT NULL predicate on the inner expression.
		return &NullCheckExpr{Expr: inner, IsNotNull: true}, nil
	default:
		return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
	}
}

// buildAtomExpr converts an OC_AtomContext into an Expr.
// Handles: literals, $params, variables, parenthesized sub-expressions,
// COUNT(*), and function invocations.
func buildAtomExpr(ctx *parser.OC_AtomContext) (Expr, error) {
	// Literal
	if litCtx := ctx.OC_Literal(); litCtx != nil {
		return buildLiteralExpr(litCtx.(*parser.OC_LiteralContext))
	}
	// Parameter reference: $param
	if paramCtx := ctx.OC_Parameter(); paramCtx != nil {
		name := trimWhitespace(paramCtx.(*parser.OC_ParameterContext).OC_SymbolicName().GetText())
		return &ParamRef{Name: name}, nil
	}
	// Variable
	if varCtx := ctx.OC_Variable(); varCtx != nil {
		name := trimWhitespace(varCtx.GetText())
		return &VarExpr{Name: name}, nil
	}
	// Parenthesized expression: recurse
	if parenCtx := ctx.OC_ParenthesizedExpression(); parenCtx != nil {
		inner := parenCtx.(*parser.OC_ParenthesizedExpressionContext).OC_Expression()
		if inner == nil {
			return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
		}
		return buildExprFromCST(inner)
	}
	// COUNT(*) — special atom rule in the grammar (COUNT token followed by (*))
	if ctx.COUNT() != nil {
		return &AggCallExpr{Func: "count", CountStar: true}, nil
	}
	// Generic function invocation (count(n), sum(n.age), etc.)
	if fi := ctx.OC_FunctionInvocation(); fi != nil {
		return buildFunctionInvocation(fi.(*parser.OC_FunctionInvocationContext))
	}
	// CASE expression
	if ce := ctx.OC_CaseExpression(); ce != nil {
		return buildCaseExpr(ce.(*parser.OC_CaseExpressionContext))
	}
	// Fallback for unsupported atom types (list comprehension, etc.)
	return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
}

// buildCaseExpr converts an OC_CaseExpressionContext into a CaseExpr.
//
// The ANTLR grammar produces two forms. In both cases, AllOC_CaseAlternatives()
// returns the WHEN/THEN pairs, and AllOC_Expression() on the CASE context itself
// returns only the top-level (non-alternative) expressions:
//
//	Searched: CASE WHEN cond THEN val [WHEN...] [ELSE elseExpr] END
//	  - AllOC_Expression() = [] if no ELSE, [elseExpr] if ELSE present.
//	  - AllOC_CaseAlternatives() = [alt0, alt1, ...], each with [cond, val].
//
//	Simple:   CASE subject WHEN val THEN result [WHEN...] [ELSE elseExpr] END
//	  - AllOC_Expression() = [subject] if no ELSE, [subject, elseExpr] if ELSE.
//	  - AllOC_CaseAlternatives() = [alt0, ...], each with [val, result].
//
// Disambiguation: if the expression count exceeds the else count (0 or 1), the
// first expression is the subject (simple form).
func buildCaseExpr(ctx *parser.OC_CaseExpressionContext) (Expr, error) {
	alts := ctx.AllOC_CaseAlternatives()
	allExprs := ctx.AllOC_Expression()
	hasElse := ctx.ELSE() != nil

	// In simple form: allExprs has subject [+ ELSE], so len > elseCount.
	// In searched form: allExprs has only ELSE (or nothing).
	elseCount := 0
	if hasElse {
		elseCount = 1
	}
	isSimple := len(allExprs) > elseCount

	ce := &CaseExpr{}

	// Extract subject for simple form (always the first expression).
	if isSimple {
		if len(allExprs) == 0 {
			return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
		}
		subj, err := buildExprFromCST(allExprs[0])
		if err != nil {
			return nil, fmt.Errorf("cypher: CASE subject: %w", err)
		}
		ce.Subject = subj
	}

	// Build WHEN … THEN … clauses from each OC_CaseAlternatives child.
	// Each alternative holds exactly two expressions: [0]=WHEN value/cond, [1]=THEN result.
	for _, alt := range alts {
		altCtx := alt.(*parser.OC_CaseAlternativesContext)
		altExprs := altCtx.AllOC_Expression()
		if len(altExprs) < 2 {
			return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
		}
		whenExpr, err := buildExprFromCST(altExprs[0])
		if err != nil {
			return nil, fmt.Errorf("cypher: CASE WHEN: %w", err)
		}
		thenExpr, err := buildExprFromCST(altExprs[1])
		if err != nil {
			return nil, fmt.Errorf("cypher: CASE THEN: %w", err)
		}
		clause := CaseWhenClause{Value: thenExpr}
		if isSimple {
			clause.CaseVal = whenExpr
		} else {
			clause.Condition = whenExpr
		}
		ce.WhenClauses = append(ce.WhenClauses, clause)
	}

	// Extract ELSE expression (always the last element of allExprs when present).
	if hasElse && len(allExprs) > 0 {
		elseExpr, err := buildExprFromCST(allExprs[len(allExprs)-1])
		if err != nil {
			return nil, fmt.Errorf("cypher: CASE ELSE: %w", err)
		}
		ce.Else = elseExpr
	}

	return ce, nil
}

// buildLiteralExpr converts an OC_LiteralContext into a LiteralExpr.
func buildLiteralExpr(ctx *parser.OC_LiteralContext) (Expr, error) {
	// NULL
	if ctx.NULL() != nil {
		return &LiteralExpr{Value: nil}, nil
	}
	// Boolean
	if boolCtx := ctx.OC_BooleanLiteral(); boolCtx != nil {
		text := strings.ToUpper(trimWhitespace(boolCtx.GetText()))
		return &LiteralExpr{Value: text == "TRUE"}, nil
	}
	// Number
	if numCtx := ctx.OC_NumberLiteral(); numCtx != nil {
		return buildNumberLiteralExpr(numCtx.(*parser.OC_NumberLiteralContext))
	}
	// String literal — strip surrounding quotes and unescape.
	if ctx.StringLiteral() != nil {
		raw := trimWhitespace(ctx.StringLiteral().GetText())
		val := unquoteString(raw)
		return &LiteralExpr{Value: val}, nil
	}
	return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
}

// buildNumberLiteralExpr converts OC_NumberLiteralContext into a LiteralExpr.
func buildNumberLiteralExpr(ctx *parser.OC_NumberLiteralContext) (Expr, error) {
	if intCtx := ctx.OC_IntegerLiteral(); intCtx != nil {
		text := trimWhitespace(intCtx.GetText())
		v, err := strconv.ParseInt(text, 0, 64) // base 0 handles hex/octal prefixes
		if err != nil {
			return &RawExpr{Text: text}, nil
		}
		return &LiteralExpr{Value: v}, nil
	}
	if dblCtx := ctx.OC_DoubleLiteral(); dblCtx != nil {
		text := trimWhitespace(dblCtx.GetText())
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return &RawExpr{Text: text}, nil
		}
		return &LiteralExpr{Value: v}, nil
	}
	return &RawExpr{Text: trimWhitespace(ctx.GetText())}, nil
}

// unquoteString strips surrounding single or double quotes from a Cypher string
// literal and unescapes the internal escape sequences.
func unquoteString(s string) string {
	if len(s) < 2 {
		return s
	}
	if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
		inner := s[1 : len(s)-1]
		inner = strings.ReplaceAll(inner, "''", "'")
		inner = strings.ReplaceAll(inner, `""`, `"`)
		inner = strings.ReplaceAll(inner, `\\`, `\`)
		inner = strings.ReplaceAll(inner, `\'`, `'`)
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		return inner
	}
	return s
}
