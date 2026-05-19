package cypher

import (
	"sort"
	"testing"
)

func TestNewScope_Empty(t *testing.T) {
	s := NewScope()
	if s == nil {
		t.Fatal("NewScope returned nil")
	}
	names := s.Names()
	if len(names) != 0 {
		t.Fatalf("expected empty scope, got %v", names)
	}
}

func TestBind_And_Resolve(t *testing.T) {
	s := NewScope()
	b := Binding{Alias: "n0", Column: "n0.id", Table: "nodes", IsNode: true}
	s.Bind("n", b)

	got, ok := s.Resolve("n")
	if !ok {
		t.Fatal("Resolve returned false for bound variable")
	}
	if got.Alias != "n0" || got.Column != "n0.id" || got.Table != "nodes" {
		t.Fatalf("unexpected binding: %+v", got)
	}
}

func TestResolve_Missing(t *testing.T) {
	s := NewScope()
	_, ok := s.Resolve("nothere")
	if ok {
		t.Fatal("expected false for missing variable")
	}
}

func TestMustResolve_Panics(t *testing.T) {
	s := NewScope()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected MustResolve to panic for missing variable")
		}
	}()
	s.MustResolve("missing")
}

func TestMustResolve_OK(t *testing.T) {
	s := NewScope()
	b := Binding{Alias: "r0", Column: "r0.id", Table: "edges", IsRel: true}
	s.Bind("r", b)

	// Should not panic.
	got := s.MustResolve("r")
	if got.Alias != "r0" {
		t.Fatalf("unexpected alias: %s", got.Alias)
	}
}

func TestChild_Scope_Resolve_From_Parent(t *testing.T) {
	parent := NewScope()
	parent.Bind("n", Binding{Alias: "n0", Table: "nodes", IsNode: true})

	child := parent.Child()
	got, ok := child.Resolve("n")
	if !ok {
		t.Fatal("child could not resolve parent binding")
	}
	if got.Alias != "n0" {
		t.Fatalf("unexpected alias from parent: %s", got.Alias)
	}
}

func TestChild_Scope_Shadows_Parent(t *testing.T) {
	parent := NewScope()
	parent.Bind("n", Binding{Alias: "n0", Table: "nodes", IsNode: true})

	child := parent.Child()
	child.Bind("n", Binding{Alias: "n1", Table: "nodes", IsNode: true})

	got, ok := child.Resolve("n")
	if !ok {
		t.Fatal("Resolve failed")
	}
	if got.Alias != "n1" {
		t.Fatalf("expected child binding n1, got %s", got.Alias)
	}

	// Parent must still see its own binding.
	gotParent, ok := parent.Resolve("n")
	if !ok {
		t.Fatal("parent Resolve failed")
	}
	if gotParent.Alias != "n0" {
		t.Fatalf("expected parent binding n0, got %s", gotParent.Alias)
	}
}

func TestChild_DoesNotAffectParent(t *testing.T) {
	parent := NewScope()
	child := parent.Child()
	child.Bind("x", Binding{Alias: "x0", Table: "nodes", IsNode: true})

	_, ok := parent.Resolve("x")
	if ok {
		t.Fatal("binding in child should not be visible in parent")
	}
}

func TestNames_IncludesInherited(t *testing.T) {
	parent := NewScope()
	parent.Bind("a", Binding{Alias: "a0"})
	parent.Bind("b", Binding{Alias: "b0"})

	child := parent.Child()
	child.Bind("c", Binding{Alias: "c0"})

	names := child.Names()
	sort.Strings(names)
	expected := []string{"a", "b", "c"}
	if len(names) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, names)
	}
	for i, n := range expected {
		if names[i] != n {
			t.Fatalf("expected %v at index %d, got %v", n, i, names[i])
		}
	}
}

func TestLocal_ReturnsOnlyLocalBindings(t *testing.T) {
	parent := NewScope()
	parent.Bind("a", Binding{Alias: "a0"})

	child := parent.Child()
	child.Bind("b", Binding{Alias: "b0"})
	child.Bind("c", Binding{Alias: "c0"})

	local := child.Local()
	sort.Strings(local)
	if len(local) != 2 {
		t.Fatalf("expected 2 local bindings, got %v", local)
	}
	if local[0] != "b" || local[1] != "c" {
		t.Fatalf("unexpected local names: %v", local)
	}
}

func TestMultiHop_VariableTracking(t *testing.T) {
	// Simulate: MATCH (a)-[r1]->(b)-[r2]->(c)
	// a, r1, b are in scope from hop 1; r2, c added in hop 2.
	s := NewScope()
	s.Bind("a", Binding{Alias: "a0", Table: "nodes", IsNode: true})
	s.Bind("r1", Binding{Alias: "r1_0", Table: "edges", IsRel: true})
	s.Bind("b", Binding{Alias: "b0", Table: "nodes", IsNode: true})
	s.Bind("r2", Binding{Alias: "r2_0", Table: "edges", IsRel: true})
	s.Bind("c", Binding{Alias: "c0", Table: "nodes", IsNode: true})

	for _, name := range []string{"a", "r1", "b", "r2", "c"} {
		_, ok := s.Resolve(name)
		if !ok {
			t.Fatalf("expected to resolve %q in multi-hop scope", name)
		}
	}
}

func TestWith_Pipeline_Scoping(t *testing.T) {
	// Simulate a WITH pipeline: stage1 binds n and r; WITH projects only n;
	// stage2 should only see n (not r).
	stage1 := NewScope()
	stage1.Bind("n", Binding{Alias: "n0", Table: "nodes", IsNode: true})
	stage1.Bind("r", Binding{Alias: "r0", Table: "edges", IsRel: true})

	// WITH n — start a new child scope with only "n" projected through.
	stage2 := NewScope() // fresh scope, NOT a child (WITH resets visibility)
	b, _ := stage1.Resolve("n")
	stage2.Bind("n", b)

	_, hasN := stage2.Resolve("n")
	_, hasR := stage2.Resolve("r")
	if !hasN {
		t.Fatal("stage2 should see 'n' projected through WITH")
	}
	if hasR {
		t.Fatal("stage2 should NOT see 'r' not projected through WITH")
	}
}

func TestNullable_Binding(t *testing.T) {
	s := NewScope()
	s.Bind("optN", Binding{
		Alias:      "n0",
		Table:      "nodes",
		IsNode:     true,
		IsNullable: true,
	})

	got, ok := s.Resolve("optN")
	if !ok {
		t.Fatal("expected to resolve nullable binding")
	}
	if !got.IsNullable {
		t.Fatal("expected IsNullable=true on resolved binding")
	}
}

func TestBind_Overwrite_InSameScope(t *testing.T) {
	s := NewScope()
	s.Bind("n", Binding{Alias: "n0"})
	s.Bind("n", Binding{Alias: "n1"}) // overwrite

	got, ok := s.Resolve("n")
	if !ok {
		t.Fatal("expected binding after overwrite")
	}
	if got.Alias != "n1" {
		t.Fatalf("expected n1 after overwrite, got %s", got.Alias)
	}
}

func TestDeepChain_Scopes(t *testing.T) {
	// Three levels: root → child1 → child2
	root := NewScope()
	root.Bind("a", Binding{Alias: "a0"})

	child1 := root.Child()
	child1.Bind("b", Binding{Alias: "b0"})

	child2 := child1.Child()
	child2.Bind("c", Binding{Alias: "c0"})

	// child2 should see all three
	for _, name := range []string{"a", "b", "c"} {
		_, ok := child2.Resolve(name)
		if !ok {
			t.Fatalf("expected to resolve %q from deepest scope", name)
		}
	}

	// child1 should see a and b but not c
	_, hasC := child1.Resolve("c")
	if hasC {
		t.Fatal("child1 should not see c defined only in child2")
	}
}
