package source

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
)

// TypeRegistry remembers the source of the types declared by interactive
// inputs and of their methods. Methods cannot be added to a type that an
// earlier input declared, so when an input declares a method on such a
// type, the type's declaration and its other methods are replayed into the
// input: the type is redeclared as a whole.
type TypeRegistry struct {
	decls map[string]*typeDecl
}

type typeDecl struct {
	typ     Unit
	methods map[string]Unit
	order   []string
}

// DeclInfo describes a declaration unit: the types it declares, or the
// receiver base type and name of a method.
func DeclInfo(u Unit) (types []string, recv, method string) {
	f, err := parser.ParseFile(token.NewFileSet(), "", "package p\n"+u.Text, parser.SkipObjectResolution)
	if err != nil || len(f.Decls) == 0 {
		return nil, "", ""
	}
	switch d := f.Decls[0].(type) {
	case *ast.GenDecl:
		if d.Tok == token.TYPE {
			for _, sp := range d.Specs {
				types = append(types, sp.(*ast.TypeSpec).Name.Name)
			}
		}
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) > 0 {
			return nil, ReceiverBase(d.Recv.List[0].Type), d.Name.Name
		}
	}
	return types, "", ""
}

// ReceiverBase returns the name of a method receiver's base type.
func ReceiverBase(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

// WithRedecls returns decls extended with the declarations of earlier
// types that decls add methods to, and notes for the user.
func (r *TypeRegistry) WithRedecls(decls []Unit) ([]Unit, []string) {
	declaredHere := map[string]bool{}
	newMethods := map[string]map[string]bool{}
	var order []string
	for _, u := range decls {
		ts, recv, m := DeclInfo(u)
		for _, t := range ts {
			declaredHere[t] = true
		}
		if recv != "" {
			if newMethods[recv] == nil {
				newMethods[recv] = map[string]bool{}
				order = append(order, recv)
			}
			newMethods[recv][m] = true
		}
	}
	var extra []Unit
	var notes []string
	pulled := map[string]bool{}
	for _, recv := range order {
		td := r.decls[recv]
		if declaredHere[recv] || td == nil {
			continue
		}
		key := fmt.Sprintf("%s:%d:%d", td.typ.File, td.typ.Line, td.typ.Col)
		if !pulled[key] {
			pulled[key] = true
			extra = append(extra, td.typ)
		}
		for _, name := range td.order {
			if !newMethods[recv][name] {
				extra = append(extra, td.methods[name])
			}
		}
		notes = append(notes, fmt.Sprintf("redeclared %s with its methods; values created earlier keep the previous %s type", recv, recv))
	}
	return append(extra, decls...), notes
}

// Record updates the registry from the declarations of a committed input.
func (r *TypeRegistry) Record(decls []Unit) {
	if r.decls == nil {
		r.decls = map[string]*typeDecl{}
	}
	for _, u := range decls {
		if ts, _, _ := DeclInfo(u); len(ts) > 0 {
			for _, t := range ts {
				r.decls[t] = &typeDecl{typ: u, methods: map[string]Unit{}}
			}
		}
	}
	for _, u := range decls {
		if _, recv, m := DeclInfo(u); recv != "" {
			if td := r.decls[recv]; td != nil {
				if _, seen := td.methods[m]; !seen {
					td.order = append(td.order, m)
				}
				td.methods[m] = u
			}
		}
	}
}
