package cypher

import "fmt"

// Binding records how a single Cypher variable maps to SQL.
// The Alias is the SQL table alias assigned during planning; Column is the
// fully-qualified column expression that evaluates to the variable's value.
//
// For a node variable "n" aliased to table alias "n0":
//
//	Alias  = "n0"
//	Column = "n0.id"           (for whole-node references)
//	Table  = "nodes"
//
// For a relationship variable "r" aliased to "r0":
//
//	Alias  = "r0"
//	Column = "r0.id"
//	Table  = "edges"
type Binding struct {
	// Alias is the SQL table alias (e.g. "n0", "r1").
	Alias string
	// Column is the primary SQL column expression for this binding
	// (e.g. "n0.id" for a node, "r0.id" for a relationship).
	Column string
	// Table is "nodes" or "edges".
	Table string
	// IsNode is true when the binding represents a graph node.
	IsNode bool
	// IsRel is true when the binding represents a relationship.
	IsRel bool
	// IsNullable is true when the variable was introduced by an OPTIONAL MATCH.
	// Downstream WHERE and RETURN clauses must handle null values.
	IsNullable bool
	// AggExpr is non-nil for aggregate aliases bound in a WITH clause
	// (e.g. count(r) AS cnt). The translator expands the alias back to the
	// full aggregate expression when rendering the SELECT or HAVING clause.
	AggExpr Expr
}

// BindingScope maps Cypher variable names to their SQL Binding descriptors.
// It supports lexical scoping via a parent pointer: when resolving a variable
// the child scope is checked first, then the parent chain. A new scope is
// created for each WITH pipeline stage.
//
// BindingScope is not safe for concurrent access; it is created and consumed
// on a single goroutine during the plan/translate passes.
type BindingScope struct {
	parent   *BindingScope
	bindings map[string]Binding
}

// NewScope creates a new root BindingScope with no parent.
func NewScope() *BindingScope {
	return &BindingScope{
		bindings: make(map[string]Binding),
	}
}

// Child creates a new BindingScope that shadows the receiver.
// Variables bound in the child do not affect the parent scope.
// Variables not found in the child are resolved from the parent chain.
// Use this to represent the new scope introduced by a WITH clause.
func (s *BindingScope) Child() *BindingScope {
	return &BindingScope{
		parent:   s,
		bindings: make(map[string]Binding),
	}
}

// Bind registers varName in this scope. If varName is already bound in this
// scope (not just a parent), the binding is overwritten (SET clause can rebind).
func (s *BindingScope) Bind(varName string, b Binding) {
	s.bindings[varName] = b
}

// Resolve looks up varName starting from this scope and walking up through
// parent scopes. It returns (Binding, true) when found, or (zero, false) when
// the variable is not visible in any enclosing scope.
func (s *BindingScope) Resolve(varName string) (Binding, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if b, ok := cur.bindings[varName]; ok {
			return b, true
		}
	}
	return Binding{}, false
}

// MustResolve is like Resolve but panics if varName is not found.
// Use only when the planner has already validated that the variable exists.
func (s *BindingScope) MustResolve(varName string) Binding {
	b, ok := s.Resolve(varName)
	if !ok {
		panic(fmt.Sprintf("cypher.BindingScope: unresolved variable %q", varName))
	}
	return b
}

// Names returns all variable names visible in this scope (including those
// inherited from parent scopes). Names defined in the child shadow parents.
func (s *BindingScope) Names() []string {
	seen := make(map[string]bool)
	var names []string
	for cur := s; cur != nil; cur = cur.parent {
		for k := range cur.bindings {
			if !seen[k] {
				seen[k] = true
				names = append(names, k)
			}
		}
	}
	return names
}

// Local returns the names bound in this scope only (not inherited from parents).
func (s *BindingScope) Local() []string {
	names := make([]string, 0, len(s.bindings))
	for k := range s.bindings {
		names = append(names, k)
	}
	return names
}
