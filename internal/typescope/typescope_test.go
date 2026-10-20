package typescope

import (
	"go/token"
	"go/types"
	"slices"
	"testing"
)

func newScope(names ...string) *types.Scope {
	s := types.NewScope(nil, token.NoPos, token.NoPos, "test")
	for _, n := range names {
		s.Insert(types.NewVar(token.NoPos, nil, n, types.Typ[types.Int]))
	}
	return s
}

func TestDelete(t *testing.T) {
	s := newScope("a", "b", "c")
	if err := Delete(s, "b"); err != nil {
		t.Fatal(err)
	}
	if got := s.Names(); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("names after delete = %v", got)
	}
	if s.Lookup("b") != nil {
		t.Fatal("b still found")
	}
	// Deleting a missing name is a no-op.
	if err := Delete(s, "zz"); err != nil {
		t.Fatal(err)
	}
	// A deleted name can be inserted again.
	if alt := s.Insert(types.NewVar(token.NoPos, nil, "b", types.Typ[types.String])); alt != nil {
		t.Fatalf("re-insert of b conflicted with %v", alt)
	}
}

func TestDeleteEmptyScope(t *testing.T) {
	s := newScope()
	if err := Delete(s, "x"); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d", s.Len())
	}
}

func TestSnapshot(t *testing.T) {
	s := newScope("a", "b")
	orig := s.Lookup("a")
	restore, err := Snapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	Delete(s, "a")
	s.Insert(types.NewVar(token.NoPos, nil, "a", types.Typ[types.String]))
	s.Insert(types.NewVar(token.NoPos, nil, "new", types.Typ[types.Int]))
	if got := s.Names(); !slices.Equal(got, []string{"a", "b", "new"}) {
		t.Fatalf("names before restore = %v", got)
	}
	restore()
	if got := s.Names(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("names after restore = %v", got)
	}
	if s.Lookup("a") != orig {
		t.Fatal("restore did not bring back the original object")
	}
	// Restoring twice is harmless.
	s.Insert(types.NewVar(token.NoPos, nil, "x", types.Typ[types.Int]))
	restore()
	if got := s.Names(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("names after second restore = %v", got)
	}
}
