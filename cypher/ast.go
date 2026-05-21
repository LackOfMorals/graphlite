// Package cypher defines the AST types produced by the graphlite Cypher parser and
// consumed by the planner and SQL translator. Types are intentionally minimal for
// the v0.1 feature set; additional clause types are added in later milestones.
//
// # Parser Coverage Audit (v0.1 target)
//
// The table below records what the cloudprivacylabs/opencypher ANTLR parser accepts
// and what our AST can express for each v0.1 feature.
//
//	Feature                              Parser status        AST status
//	MATCH (n)                            ✅ supported         ✅ NodePattern
//	MATCH (n:Label)                      ✅ supported         ✅ NodePattern.Labels
//	MATCH (n:Label {prop: val})          ✅ supported         ✅ NodePattern.Props
//	MATCH (a:L1:L2) multi-label AND      ✅ supported         ✅ multiple Labels entries; AND semantics required by planner
//	MATCH (a)-[r:TYPE]->(b) directed     ✅ supported         ✅ RelPattern + direction flags
//	MATCH (a)-[r:TYPE]-(b) undirected    ✅ supported         ✅ RelPattern (ToLeft=false, ToRight=false)
//	Multi-hop chains (up to 5 hops)      ✅ supported         ✅ PatternChain slice
//	WHERE comparisons (=,<>,<,>,<=,>=)   ✅ supported         ✅ ComparisonExpr with correct Op
//	WHERE AND / OR / NOT                 ✅ supported         ✅ BoolExpr (AND/OR) and NotExpr
//	WHERE $param references              ✅ supported         ✅ ParamRef nodes in predicate tree
//	RETURN n.prop AS alias               ✅ supported         ✅ ReturnItem + Alias
//	RETURN n, r (whole node/rel)         ✅ supported         ✅ ReturnItem with ExprText = variable name
//	ORDER BY expr ASC/DESC               ✅ supported         ✅ SortItem
//	LIMIT integer                        ✅ supported         ✅ ReturnClause.Limit
//	SKIP integer                         ✅ supported         ✅ ReturnClause.Skip
//	RETURN DISTINCT                      ✅ supported         ✅ ReturnClause.Distinct
//	CREATE (n:Label {props})             ✅ supported         ✅ CreateClause + NodePattern
//	CREATE (a)-[:TYPE]->(b)              ✅ supported         ✅ CreateClause + PatternChain
//	SET n.prop = value                   ✅ supported         ✅ SetItem
//	SET n.prop = $param                  ✅ supported         ✅ SetItem (ExprText contains param ref)
//	DELETE n                             ✅ supported         ✅ DeleteClause (Detach=false)
//	DETACH DELETE n                      ✅ supported         ✅ DeleteClause (Detach=true)
//	Named $params in property maps       ✅ supported         ⚠️  stored as raw ExprText; task-015 adds resolution
//
// # Known Gaps vs. v0.1 Feature List
//
//   - GAP-001 (RESOLVED): WHERE clause is now parsed into a typed predicate tree
//     (ComparisonExpr, BoolExpr, NotExpr, ParamRef, PropExpr, LiteralExpr). Completed
//     in task-008. Complex unsupported sub-expressions fall back to RawExpr.
//   - GAP-002: Property map values in CREATE / SET are stored as raw ExprText strings
//     (the ANTLR CST expression text). Task-015 (parameter binding) will resolve
//     $param references to concrete values from the caller-supplied map.
//   - GAP-003: Variable-length path patterns (e.g. (a)-[*1..5]->(b)) are detected
//     and parse without panic, but the VarLength flag is set on the RelPattern and
//     the planner must return ErrUnsupportedCypher for v0.1.
//   - GAP-004: UNION and UNION ALL are parsed correctly by the ANTLR grammar but
//     are not in scope for v0.1; Parse() returns ErrUnsupportedCypher when unions
//     are detected.
//
// # Grammar Quirks (cloudprivacylabs/opencypher v1.0.0)
//
//   - SKIP/LIMIT ordering: the grammar requires SKIP before LIMIT in RETURN clauses
//     (i.e., "RETURN ... SKIP 5 LIMIT 10"), unlike standard openCypher which permits
//     LIMIT first. Our parser accepts whichever the grammar allows; the SQL translator
//     must handle both Skip and Limit being nil independently.
//   - MATCH (a), (b) syntax: multiple comma-separated patterns in one MATCH clause
//     produce a single *MatchClause with multiple PatternParts (not multiple MatchClauses).
//   - Multi-part queries (WITH pipelines) are rejected with ErrUnsupportedCypher; they
//     will be supported starting at task-024 (v0.2 milestone).
package cypher

// Query is the root AST node produced by Parse. For v0.1, only single-part
// queries without UNION are supported.
type Query struct {
	// Clauses holds the ordered sequence of clauses in the query.
	// Each element is one of: *MatchClause, *CreateClause, *SetClause,
	// *DeleteClause, *ReturnClause.
	Clauses []Clause
}

// Clause is a sealed interface implemented by each top-level clause type.
// Callers use a type switch to dispatch on the concrete type.
type Clause interface {
	clauseNode()
}

// MatchClause represents a MATCH or OPTIONAL MATCH clause.
//
//	MATCH (n:Person {name: $name}) WHERE n.age > 18
type MatchClause struct {
	Optional bool
	// Pattern is the list of pattern parts in the MATCH clause.
	// Each PatternPart is an independent rooted path.
	Pattern []PatternPart
	// Where is the typed predicate expression tree for the WHERE clause.
	// Nil when no WHERE clause is present. Set by the parser in task-008.
	Where Expr
}

func (*MatchClause) clauseNode() {}

// CreateClause represents a CREATE clause.
//
//	CREATE (n:Person {name: 'Alice'})
//	CREATE (a)-[:KNOWS]->(b)
type CreateClause struct {
	// Pattern is the list of pattern parts to create.
	Pattern []PatternPart
}

func (*CreateClause) clauseNode() {}

// SetClause represents a SET clause.
//
//	SET n.name = 'Alice', n.age = $age
//	SET n += {a: 1, b: 2}
type SetClause struct {
	Items []SetItem
}

func (*SetClause) clauseNode() {}

// SetItem represents one assignment in a SET clause.
//
// Two forms:
//   - n.prop = expr  (Merge=false, Property is set, Props is nil)
//   - n += {map}     (Merge=true, Property is "", Props holds the map pairs)
type SetItem struct {
	// Variable is the Cypher variable name (e.g. "n").
	Variable string
	// Property is the property key being set (e.g. "name"). Empty for += form.
	Property string
	// ExprText is the raw text of the right-hand-side expression for n.prop = expr.
	// Empty for the += merge form (Props holds the key/value pairs instead).
	ExprText string
	// Merge is true for SET n += {map} (property merge without overwriting other keys).
	Merge bool
	// Props holds the key/value pairs for the SET n += {map} form.
	// Each value is the raw expression text (literal or $param).
	// Nil for the n.prop = expr form.
	Props map[string]string
}

// MergeClause represents a MERGE clause with optional ON CREATE SET and ON MATCH SET actions.
//
//	MERGE (n:Person {name: 'Alice'})
//	MERGE (n:Person {name: 'Alice'}) ON CREATE SET n.created = true ON MATCH SET n.seen = true
type MergeClause struct {
	// Pattern is the single pattern part to merge (match-or-create).
	Pattern PatternPart
	// OnCreate holds the SET items to apply if the node/relationship was just created.
	OnCreate []SetItem
	// OnMatch holds the SET items to apply if the node/relationship already existed.
	OnMatch []SetItem
}

func (*MergeClause) clauseNode() {}

// RemoveClause represents a REMOVE clause.
//
//	REMOVE n.prop
//	REMOVE n:Label
type RemoveClause struct {
	Items []RemoveItem
}

func (*RemoveClause) clauseNode() {}

// RemoveItem represents one item in a REMOVE clause.
//
// Two forms:
//   - n.prop   (IsProp=true, Property is the key to remove)
//   - n:Label  (IsProp=false, Labels holds the labels to remove)
type RemoveItem struct {
	// Variable is the Cypher variable name (e.g. "n").
	Variable string
	// IsProp is true for REMOVE n.prop, false for REMOVE n:Label.
	IsProp bool
	// Property is the property key to remove. Empty for label removal.
	Property string
	// Labels is the list of labels to remove. Empty for property removal.
	Labels []string
}

// DeleteClause represents a DELETE or DETACH DELETE clause.
//
//	DELETE n
//	DETACH DELETE n
type DeleteClause struct {
	Detach bool
	// Exprs holds the raw text of each expression to delete (variable names).
	Exprs []string
}

func (*DeleteClause) clauseNode() {}

// ReturnClause represents a RETURN clause with optional ORDER BY, SKIP, LIMIT.
//
//	RETURN n.name AS name ORDER BY name DESC LIMIT 10
type ReturnClause struct {
	Distinct bool
	Items    []ReturnItem
	OrderBy  []SortItem
	// Skip is nil when not present.
	Skip *int64
	// Limit is nil when not present.
	Limit *int64
}

func (*ReturnClause) clauseNode() {}

// WithClause represents an intermediate WITH clause in a multi-part query.
// It acts as a pipeline stage: rows from prior MATCH clauses are aggregated
// or projected before being passed to the following clauses.
//
//	MATCH (n:Person)-[r:KNOWS]->() WITH n, count(r) AS cnt WHERE cnt > 1 RETURN n.name
type WithClause struct {
	Distinct bool
	Items    []ReturnItem
	OrderBy  []SortItem
	// Skip is nil when not present.
	Skip *int64
	// Limit is nil when not present.
	Limit *int64
	// Where is the optional post-WITH WHERE predicate (becomes SQL HAVING when
	// aggregate functions are present in Items).
	Where Expr
}

func (*WithClause) clauseNode() {}

// ReturnItem is one projection in a RETURN or WITH clause.
type ReturnItem struct {
	// ExprText is the raw expression text (e.g. "n.name", "n", "count(n)").
	// Kept for backward compatibility; new code paths also populate Expr.
	ExprText string
	// Alias is the AS alias, or "" if none.
	Alias string
	// Expr is the typed expression parsed from the ANTLR CST. Nil when the
	// item was produced by the legacy ExprText path (existing single-part queries).
	Expr Expr
}

// SortItem represents one column in an ORDER BY clause.
type SortItem struct {
	// ExprText is the expression to sort by.
	ExprText string
	// Descending is true for DESC ordering.
	Descending bool
}

// PatternPart is a rooted path: a start node optionally followed by one or
// more alternating relationship/node chains.
//
//	(a:Person)-[:KNOWS]->(b:Person)-[:LIKES]->(c:Item)
type PatternPart struct {
	// Variable is the optional name bound to the whole path (rarely used).
	Variable string
	// Start is the first (leftmost) node pattern.
	Start NodePattern
	// Chain holds the alternating relationship+node pairs that extend the path.
	Chain []PatternChain
}

// NodePattern is a single node in a pattern: (varName:Label1:Label2 {props}).
type NodePattern struct {
	// Variable is the Cypher variable name, or "" for anonymous nodes.
	Variable string
	// Labels are the required labels (AND semantics).
	Labels []string
	// Props maps property key → raw expression text.
	// Empty map means no inline properties constraint.
	Props map[string]string
	// HasExplicitProps is true when the node pattern included an explicit
	// property map in the source Cypher (even an empty map "{}"). This
	// distinguishes (n {}) from (n) — the former re-binds an existing node
	// which is a VariableAlreadyBound error in CREATE.
	HasExplicitProps bool
}

// PatternChain is one hop in a path: a relationship pattern followed by a node.
//
//	-[:TYPE {props}]->(n:Label)
type PatternChain struct {
	Rel  RelPattern
	Node NodePattern
}

// RelPattern is a relationship in a pattern.
type RelPattern struct {
	// Variable is the Cypher variable name, or "".
	Variable string
	// Types holds the acceptable relationship types (OR semantics within types,
	// but typically v0.1 uses exactly one type). Empty means any type.
	Types []string
	// Props maps property key → raw expression text.
	Props map[string]string
	// ToLeft is true when the arrow points left: <-[r]-
	ToLeft bool
	// ToRight is true when the arrow points right: -[r]->
	ToRight bool
	// VarLength is true when a variable-length range was specified (*1..5).
	// When true, MinHops and MaxHops carry the parsed bounds.
	VarLength bool
	// MinHops is the minimum number of hops (inclusive). Default 1 when VarLength
	// is true but no lower bound was specified (e.g. [*] or [*..5]).
	MinHops int
	// MaxHops is the maximum number of hops (inclusive). 0 means unbounded
	// (e.g. [*] or [*2..]).
	MaxHops int
}
