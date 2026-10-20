// Package typeexpr renders a go/types type as Go source, letting the caller
// decide how named types, fields and methods are spelled. It is what lets the
// interpreter write down the type of a variable whose type was only inferred.
package typeexpr

import (
	"fmt"
	"go/types"
	"strconv"
	"strings"
)

// Namer chooses spellings for the parts of a type that depend on where the
// generated code lives.
type Namer interface {
	// TypeName spells a reference to a defined type or alias, including any
	// package qualifier. It returns an error when the type cannot be named.
	TypeName(obj *types.TypeName) (string, error)
	// Field spells a struct field name and returns its tag.
	Field(v *types.Var, tag string) (name, newTag string, err error)
	// Method spells an interface method name.
	Method(f *types.Func) (string, error)
}

// Unnameable is returned for types that generated code cannot spell, such
// as unexported types of other packages or function-local types.
type Unnameable struct{ What string }

func (e *Unnameable) Error() string { return "cannot name type " + e.What }

// String renders t.
func String(t types.Type, n Namer) (string, error) {
	var b strings.Builder
	w := writer{&b, n, map[types.Type]bool{}}
	if err := w.typ(t); err != nil {
		return "", err
	}
	return b.String(), nil
}

type writer struct {
	b     *strings.Builder
	n     Namer
	stack map[types.Type]bool
}

func (w writer) s(s string) { w.b.WriteString(s) }

func (w writer) typ(t types.Type) error {
	switch t := t.(type) {
	case *types.Basic:
		if t.Info()&types.IsUntyped != 0 {
			return w.typ(types.Default(t))
		}
		if t.Kind() == types.UnsafePointer {
			return &Unnameable{"unsafe.Pointer"} // caller handles unsafe explicitly
		}
		w.s(t.Name())
	case *types.Pointer:
		w.s("*")
		return w.typ(t.Elem())
	case *types.Slice:
		w.s("[]")
		return w.typ(t.Elem())
	case *types.Array:
		w.s("[" + strconv.FormatInt(t.Len(), 10) + "]")
		return w.typ(t.Elem())
	case *types.Map:
		w.s("map[")
		if err := w.typ(t.Key()); err != nil {
			return err
		}
		w.s("]")
		return w.typ(t.Elem())
	case *types.Chan:
		switch t.Dir() {
		case types.SendRecv:
			w.s("chan ")
			// chan (<-chan T) needs parentheses.
			if c, ok := t.Elem().(*types.Chan); ok && c.Dir() == types.RecvOnly {
				w.s("(")
				if err := w.typ(c); err != nil {
					return err
				}
				w.s(")")
				return nil
			}
		case types.SendOnly:
			w.s("chan<- ")
		case types.RecvOnly:
			w.s("<-chan ")
		}
		return w.typ(t.Elem())
	case *types.Signature:
		w.s("func")
		return w.signature(t)
	case *types.Struct:
		w.s("struct{")
		for i := 0; i < t.NumFields(); i++ {
			if i > 0 {
				w.s("; ")
			}
			f := t.Field(i)
			name, tag, err := w.n.Field(f, t.Tag(i))
			if err != nil {
				return err
			}
			if !f.Embedded() {
				w.s(name + " ")
			}
			if err := w.typ(f.Type()); err != nil {
				return err
			}
			if tag != "" {
				w.s(" " + strconv.Quote(tag))
			}
		}
		w.s("}")
	case *types.Interface:
		if t.Empty() && t.NumEmbeddeds() == 0 {
			w.s("interface{}")
			return nil
		}
		w.s("interface{")
		first := true
		sep := func() {
			if !first {
				w.s("; ")
			}
			first = false
		}
		for i := 0; i < t.NumEmbeddeds(); i++ {
			sep()
			if err := w.typ(t.EmbeddedType(i)); err != nil {
				return err
			}
		}
		for i := 0; i < t.NumExplicitMethods(); i++ {
			sep()
			m := t.ExplicitMethod(i)
			name, err := w.n.Method(m)
			if err != nil {
				return err
			}
			w.s(name)
			if err := w.signature(m.Type().(*types.Signature)); err != nil {
				return err
			}
		}
		w.s("}")
	case *types.Union:
		for i := 0; i < t.Len(); i++ {
			if i > 0 {
				w.s(" | ")
			}
			term := t.Term(i)
			if term.Tilde() {
				w.s("~")
			}
			if err := w.typ(term.Type()); err != nil {
				return err
			}
		}
	case *types.Named:
		name, err := w.n.TypeName(t.Obj())
		if err != nil {
			return err
		}
		w.s(name)
		return w.typeArgs(t.TypeArgs())
	case *types.Alias:
		name, err := w.n.TypeName(t.Obj())
		if err != nil {
			// Fall back to the aliased type.
			return w.typ(types.Unalias(t))
		}
		w.s(name)
		return w.typeArgs(t.TypeArgs())
	case *types.TypeParam:
		return &Unnameable{"type parameter " + t.Obj().Name()}
	case *types.Tuple:
		return &Unnameable{"tuple " + t.String()}
	default:
		return &Unnameable{fmt.Sprintf("%T", t)}
	}
	return nil
}

func (w writer) typeArgs(l *types.TypeList) error {
	if l == nil || l.Len() == 0 {
		return nil
	}
	w.s("[")
	for i := 0; i < l.Len(); i++ {
		if i > 0 {
			w.s(", ")
		}
		if err := w.typ(l.At(i)); err != nil {
			return err
		}
	}
	w.s("]")
	return nil
}

func (w writer) signature(sig *types.Signature) error {
	if err := w.tuple(sig.Params(), sig.Variadic()); err != nil {
		return err
	}
	res := sig.Results()
	switch res.Len() {
	case 0:
		return nil
	case 1:
		w.s(" ")
		return w.typ(res.At(0).Type())
	}
	w.s(" ")
	return w.tuple(res, false)
}

func (w writer) tuple(t *types.Tuple, variadic bool) error {
	w.s("(")
	for i := 0; i < t.Len(); i++ {
		if i > 0 {
			w.s(", ")
		}
		typ := t.At(i).Type()
		if variadic && i == t.Len()-1 {
			w.s("...")
			typ = typ.(*types.Slice).Elem()
		}
		if err := w.typ(typ); err != nil {
			return err
		}
	}
	w.s(")")
	return nil
}
