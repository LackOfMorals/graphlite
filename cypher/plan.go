package cypher

// LogicalPlan is a sealed interface implemented by every plan node type.
// The SQL translator performs a type switch over this interface to emit SQL
// fragments. Each concrete plan node carries exactly the information needed
// by the translator — the translator must not re-inspect the AST.
type LogicalPlan interface {
	planNode()
}

// ─────────────────────────────────────────────────────────────────────────────
// Predicate expression types (used in FilterPlan and CreateNodePlan property maps)
// ─────────────────────────────────────────────────────────────────────────────

// Expr is a sealed interface for predicate/expression trees used in filter
// conditions and property value positions.
type Expr interface {
	exprNode()
}

// LiteralExpr is a constant value: string, int64, float64, or bool.
type LiteralExpr struct {
	// Value is the Go value. The dynamic type is one of:
	// string, int64, float64, bool, or nil (for the NULL literal).
	Value any
}

func (*LiteralExpr) exprNode() {}

// ParamRef is a named query parameter reference: $paramName.
type ParamRef struct {
	// Name is the parameter name without the leading "$".
	Name string
}

func (*ParamRef) exprNode() {}

// PropExpr accesses a property of a bound variable: varName.propKey.
type PropExpr struct {
	// Variable is the Cypher variable (e.g. "n").
	Variable string
	// Property is the property key (e.g. "name").
	Property string
}

func (*PropExpr) exprNode() {}

// VarExpr references a whole bound variable (node or relationship).
type VarExpr struct {
	// Name is the Cypher variable name.
	Name string
}

func (*VarExpr) exprNode() {}

// ComparisonExpr is a binary comparison: left op right.
// Op is one of "=", "<>", "<", ">", "<=", ">=".
type ComparisonExpr struct {
	Left  Expr
	Op    string
	Right Expr
}

func (*ComparisonExpr) exprNode() {}

// BoolExpr combines two sub-expressions with AND or OR.
// Op is "AND" or "OR".
type BoolExpr struct {
	Left  Expr
	Op    string
	Right Expr
}

func (*BoolExpr) exprNode() {}

// NotExpr negates a sub-expression.
type NotExpr struct {
	Expr Expr
}

func (*NotExpr) exprNode() {}

// RawExpr carries an unparsed expression as its original text.
// Used as a fallback when the planner defers full expression parsing.
type RawExpr struct {
	Text string
}

func (*RawExpr) exprNode() {}

// NullCheckExpr represents an IS NULL or IS NOT NULL predicate.
// Used for OPTIONAL MATCH patterns where variables may be unbound.
type NullCheckExpr struct {
	// Expr is the subject expression (typically a VarExpr or PropExpr).
	Expr Expr
	// IsNotNull is true for IS NOT NULL, false for IS NULL.
	IsNotNull bool
}

func (*NullCheckExpr) exprNode() {}

// AggCallExpr represents an aggregation function call:
// count(*), count(expr), sum(expr), avg(expr), min(expr), max(expr), collect(expr).
type AggCallExpr struct {
	// Func is the lowercase function name: "count", "sum", "avg", "min", "max", "collect".
	Func string
	// Arg is the argument expression. Nil when CountStar is true.
	Arg Expr
	// Distinct requests COUNT(DISTINCT …) / SUM(DISTINCT …) semantics.
	Distinct bool
	// CountStar is true for count(*) — no argument, matches all rows.
	CountStar bool
}

func (*AggCallExpr) exprNode() {}

// ExistsExpr represents an exists(n.prop) predicate.
// It emits json_extract(props, '$.prop') IS NOT NULL in SQL.
type ExistsExpr struct {
	// Prop is the property expression to test for existence.
	Prop *PropExpr
}

func (*ExistsExpr) exprNode() {}

// InListExpr represents an n.prop IN ['a', 'b', 'c'] predicate.
type InListExpr struct {
	// Expr is the left-hand side expression.
	Expr Expr
	// List is the list of literal values to test membership in.
	List []Expr
	// Not is true for NOT IN.
	Not bool
}

func (*InListExpr) exprNode() {}

// StringMatchExpr represents STARTS WITH, ENDS WITH, and CONTAINS predicates.
type StringMatchExpr struct {
	// Expr is the left-hand side expression (e.g. n.name).
	Expr Expr
	// Pattern is the right-hand side string pattern.
	Pattern Expr
	// Op is one of "STARTS WITH", "ENDS WITH", "CONTAINS".
	Op string
	// Not is true for NOT STARTS WITH / NOT ENDS WITH / NOT CONTAINS.
	Not bool
}

func (*StringMatchExpr) exprNode() {}

// CaseWhenClause is a single WHEN … THEN … branch in a CASE expression.
//
// For the searched form (CASE WHEN cond THEN val …):
//   - Condition holds the boolean predicate.
//   - Value holds the THEN expression.
//   - CaseVal is nil.
//
// For the simple form (CASE subject WHEN val THEN result …):
//   - CaseVal holds the value to compare against the subject.
//   - Value holds the THEN expression.
//   - Condition is nil.
type CaseWhenClause struct {
	// Condition is the WHEN predicate for searched CASE (nil for simple CASE).
	Condition Expr
	// CaseVal is the WHEN value for simple CASE (nil for searched CASE).
	CaseVal Expr
	// Value is the THEN expression.
	Value Expr
}

// CaseExpr represents a CASE … END expression (searched and simple forms).
//
// Searched form: CASE WHEN cond1 THEN val1 [WHEN cond2 THEN val2 ...] [ELSE default] END
// Simple form:   CASE subject WHEN v1 THEN r1 [WHEN v2 THEN r2 ...] [ELSE default] END
//
// Maps directly to SQL CASE … END syntax, which is identical between Cypher and SQLite.
type CaseExpr struct {
	// Subject is the expression to compare in the simple form (nil for searched form).
	Subject Expr
	// WhenClauses holds the ordered list of WHEN … THEN … branches.
	WhenClauses []CaseWhenClause
	// Else is the ELSE expression (nil if no ELSE clause).
	Else Expr
}

func (*CaseExpr) exprNode() {}

// ─────────────────────────────────────────────────────────────────────────────
// MATCH plan nodes
// ─────────────────────────────────────────────────────────────────────────────

// MatchNodePlan represents scanning the nodes table for a single node pattern.
//
//	MATCH (n:Label {prop: val})
type MatchNodePlan struct {
	// Variable is the Cypher variable bound to this node ("" for anonymous).
	Variable string
	// Labels are the required labels (AND semantics).
	Labels []string
	// Props maps property key → Expr for inline property constraints.
	Props map[string]Expr
	// SQLAlias is the SQL table alias assigned by the planner (e.g. "n0").
	SQLAlias string
	// Optional is true when this scan was introduced by an OPTIONAL MATCH.
	Optional bool
}

func (*MatchNodePlan) planNode() {}

// MatchRelPlan represents a single-hop traversal joining nodes and edges.
//
//	MATCH (a)-[r:TYPE]->(b)
type MatchRelPlan struct {
	// RelVariable is the Cypher variable for the relationship ("" if anonymous).
	RelVariable string
	// Types lists the acceptable relationship types (OR semantics; empty = any type).
	Types []string
	// RelProps maps property key → Expr for inline property constraints on the rel.
	RelProps map[string]Expr
	// RelSQLAlias is the SQL alias for the edges table join (e.g. "r0").
	RelSQLAlias string
	// StartVar is the Cypher variable name for the start node.
	StartVar string
	// StartNode contains the node-level constraints for the start node.
	// Its SQLAlias is the alias assigned during planning.
	StartNode MatchNodePlan
	// EndVar is the Cypher variable name for the end node.
	EndVar string
	// EndNode contains the node-level constraints for the destination node.
	EndNode MatchNodePlan
	// ToRight is true when the relationship is directed left→right: (a)-[r]->(b).
	ToRight bool
	// ToLeft is true when the relationship is directed right→left: (a)<-[r]-(b).
	ToLeft bool
	// Undirected is true when no arrow is present: (a)-[r]-(b).
	// The translator must check both directions (start_id and end_id).
	Undirected bool
	// Optional is true when this hop was introduced by an OPTIONAL MATCH.
	Optional bool
}

func (*MatchRelPlan) planNode() {}

// ─────────────────────────────────────────────────────────────────────────────
// Filter plan node
// ─────────────────────────────────────────────────────────────────────────────

// FilterPlan wraps a sub-plan with a WHERE predicate expression tree.
// The translator appends the predicate as a SQL WHERE clause fragment.
type FilterPlan struct {
	// Source is the sub-plan whose output is being filtered.
	Source LogicalPlan
	// Predicate is the typed expression tree for the WHERE condition.
	Predicate Expr
}

func (*FilterPlan) planNode() {}

// ─────────────────────────────────────────────────────────────────────────────
// RETURN plan node
// ─────────────────────────────────────────────────────────────────────────────

// ProjectionItem is one column in a RETURN or WITH clause.
type ProjectionItem struct {
	// Expr is the expression to project.
	Expr Expr
	// Alias is the AS alias, or "" if none.
	Alias string
}

// SortSpec describes one column in an ORDER BY clause.
type SortSpec struct {
	// Expr is the expression to sort by.
	Expr Expr
	// Descending is true for DESC ordering.
	Descending bool
}

// ReturnPlan represents the RETURN clause of a query.
type ReturnPlan struct {
	// Source is the sub-plan whose rows are being projected.
	Source LogicalPlan
	// Distinct requests SELECT DISTINCT.
	Distinct bool
	// Projections is the ordered list of projected columns.
	Projections []ProjectionItem
	// OrderBy is the list of sort columns, in order. Empty = no ORDER BY.
	OrderBy []SortSpec
	// Skip is nil when not present.
	Skip *int64
	// Limit is nil when not present.
	Limit *int64
}

func (*ReturnPlan) planNode() {}

// ─────────────────────────────────────────────────────────────────────────────
// CREATE plan nodes
// ─────────────────────────────────────────────────────────────────────────────

// CreateNodePlan represents creating a single node.
//
//	CREATE (n:Label {props})
type CreateNodePlan struct {
	// Variable is the Cypher variable bound to the new node ("" for anonymous).
	Variable string
	// Labels is the list of labels to assign.
	Labels []string
	// Props maps property key → Expr for the initial property values.
	Props map[string]Expr
}

func (*CreateNodePlan) planNode() {}

// CreateRelPlan represents creating a single relationship.
//
//	CREATE (a)-[:TYPE {props}]->(b)
type CreateRelPlan struct {
	// RelVariable is the Cypher variable for the relationship ("" if anonymous).
	RelVariable string
	// Type is the relationship type string (e.g. "KNOWS").
	Type string
	// StartVar is the Cypher variable for the start node.
	// The translator resolves its ID from the BindingScope or prior CREATE results.
	StartVar string
	// EndVar is the Cypher variable for the end node.
	EndVar string
	// Props maps property key → Expr for the initial property values.
	Props map[string]Expr
}

func (*CreateRelPlan) planNode() {}

// ─────────────────────────────────────────────────────────────────────────────
// SET plan node
// ─────────────────────────────────────────────────────────────────────────────

// SetPropPlan represents a SET n.prop = expr operation.
//
//	SET n.name = 'Alice'
//	SET n.age  = $age
type SetPropPlan struct {
	// Variable is the Cypher variable whose property is being set.
	Variable string
	// Property is the property key.
	Property string
	// Value is the expression providing the new value.
	Value Expr
}

func (*SetPropPlan) planNode() {}

// SetMergePlan represents SET n += {map} — a property merge operation that
// adds or updates keys in the existing props JSON without removing other keys.
//
//	SET n += {a: 1, b: 2}
type SetMergePlan struct {
	// Variable is the Cypher variable whose properties are being merged.
	Variable string
	// Props maps property key → Expr for the values to merge in.
	Props map[string]Expr
}

func (*SetMergePlan) planNode() {}

// ─────────────────────────────────────────────────────────────────────────────
// REMOVE plan nodes
// ─────────────────────────────────────────────────────────────────────────────

// RemovePropPlan represents REMOVE n.prop — deletes one key from the props JSON.
//
//	REMOVE n.age
type RemovePropPlan struct {
	// Variable is the Cypher variable whose property is being removed.
	Variable string
	// Property is the property key to remove.
	Property string
}

func (*RemovePropPlan) planNode() {}

// RemoveLabelPlan represents REMOVE n:Label — removes one or more labels from
// the comma-separated labels column.
//
//	REMOVE n:Admin
type RemoveLabelPlan struct {
	// Variable is the Cypher variable whose label(s) are being removed.
	Variable string
	// Labels is the list of labels to remove.
	Labels []string
}

func (*RemoveLabelPlan) planNode() {}

// ─────────────────────────────────────────────────────────────────────────────
// MERGE plan node
// ─────────────────────────────────────────────────────────────────────────────

// MergePlan represents a MERGE clause — match-or-create semantics.
//
// The execution layer runs:
//  1. A SELECT to check whether a node matching the labels+props exists.
//  2. If not found: INSERT the new node, then run OnCreate SET operations.
//  3. If found: run OnMatch SET operations.
//
// All steps run inside a single transaction for atomicity.
//
//	MERGE (n:Person {name: 'Alice'}) ON CREATE SET n.created = true
type MergePlan struct {
	// Variable is the Cypher variable bound to the merged node.
	Variable string
	// Labels are the required labels (AND semantics).
	Labels []string
	// Props maps property key → Expr for the match/create constraints.
	Props map[string]Expr
	// OnCreate holds SET operations to run when the node was just created.
	OnCreate []SetPropPlan
	// OnMatch holds SET operations to run when the node already existed.
	OnMatch []SetPropPlan
}

func (*MergePlan) planNode() {}

// ─────────────────────────────────────────────────────────────────────────────
// DELETE plan nodes
// ─────────────────────────────────────────────────────────────────────────────

// DeleteNodePlan represents DELETE or DETACH DELETE for a node variable.
//
//	DELETE n          → Detach = false
//	DETACH DELETE n   → Detach = true
type DeleteNodePlan struct {
	// Variable is the Cypher variable to delete.
	Variable string
	// Detach when true emits edge deletion before node deletion.
	// When false, the translator must return an error if any edges reference the node.
	Detach bool
}

func (*DeleteNodePlan) planNode() {}

// DeleteRelPlan represents DELETE for a relationship variable.
//
//	DELETE r
type DeleteRelPlan struct {
	// Variable is the Cypher variable for the relationship to delete.
	Variable string
}

func (*DeleteRelPlan) planNode() {}

// ─────────────────────────────────────────────────────────────────────────────
// Sequence plan node
// ─────────────────────────────────────────────────────────────────────────────

// SequencePlan holds an ordered list of plan nodes that must be executed in
// order. It is used for compound statements such as MATCH + SET, MATCH + DELETE,
// or multi-node CREATE patterns.
type SequencePlan struct {
	Steps []LogicalPlan
}

func (*SequencePlan) planNode() {}

// WithPlan represents a WITH clause that acts as an intermediate pipeline stage.
// The translator uses its Projections to determine GROUP BY columns (non-aggregate
// projections) and emits the aggregation inline (not as a subquery).
//
//	MATCH (n)-[r:KNOWS]->() WITH n, count(r) AS cnt [WHERE having_pred] ...
type WithPlan struct {
	// Source is the sub-plan whose rows are being projected (typically a
	// MatchRelPlan, MatchNodePlan, FilterPlan, or SequencePlan).
	Source LogicalPlan
	// Projections is the ordered list of WITH projections. Projections
	// containing AggCallExpr contribute to GROUP BY determination: non-aggregate
	// projections become GROUP BY columns.
	Projections []ProjectionItem
	// Having is the optional WHERE predicate after WITH (becomes SQL HAVING).
	Having Expr
}

func (*WithPlan) planNode() {}
