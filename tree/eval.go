package tree

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"wy/internal/source"
	"wy/internal/typescope"
	"wy/tree/native"
	"wy/tree/stdsrc"
)

// Interactive evaluation
//
// Eval runs inputs (cells) in a persistent session, as a REPL does. All
// cells are type-checked as one package "main": earlier declarations stay
// in its scope, a redeclaration replaces a name, and a cell that fails to
// type-check is rolled back. Top-level variables defined by statements
// (x := 1) become globals of the session, so they outlive their cell.

const runName = "Wy_run"

type session struct {
	pkg     *types.Package
	imports map[string]string
	types   source.TypeRegistry
	n       int
	notes   []string // notes about the last input

	resultTypes []types.Type // static types of the last input's results
}

func (in *Interp) sessionState() *session {
	if in.session == nil {
		in.session = &session{pkg: types.NewPackage("main", "main"), imports: map[string]string{}}
		in.pkgs["main"] = in.session.pkg
	}
	return in.session
}

// Notes returns remarks about the last evaluated input (such as a type
// being redeclared to add methods).
func (in *Interp) Notes() []string {
	if in.session == nil {
		return nil
	}
	return in.session.notes
}

// Eval evaluates src, which may mix imports, declarations and statements.
// If src ends with an expression (or a list "a, b"), its values are
// returned. Declarations and top-level variables persist.
func (in *Interp) Eval(ctx context.Context, src string) ([]any, error) {
	in.loadMu.Lock()
	run, nvals, err := in.prepareCell(src)
	in.loadMu.Unlock()
	if err != nil {
		return nil, err
	}
	var vals []reflect.Value
	done := make(chan error, 1)
	go func() {
		done <- in.protect(func() { vals = run() })
	}()
	select {
	case err := <-done:
		if err != nil {
			return nil, err
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([]any, 0, nvals)
	for i, v := range vals {
		var t types.Type
		if i < len(in.session.resultTypes) {
			t = in.session.resultTypes[i]
		}
		out = append(out, in.export(v, t))
	}
	return out, nil
}

// export converts an interpreter value for the host: interpreted named
// types keep printing like themselves (see display.go).
func (in *Interp) export(v reflect.Value, t types.Type) any {
	if !v.IsValid() {
		return nil
	}
	if t != nil && !isInterface(t) {
		if d := in.display(v, t, 0); d.IsValid() {
			return d.Interface()
		}
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		if v.Type() != anyRT {
			// A compiled interface type: its value (possibly a wrapper of an
			// interpreted value) is usable by the host as is.
			return v.Elem().Interface()
		}
		nt, e := in.dyn(v)
		if nt.faithful {
			return e.Interface()
		}
		return in.display(e, nt.t, 0).Interface()
	}
	return v.Interface()
}

type exprResult struct {
	evals []evalN
	types []types.Type
}

// prepareCell type-checks and compiles one input, returning a function
// that runs it.
func (in *Interp) prepareCell(src string) (func() []reflect.Value, int, error) {
	s := in.sessionState()
	s.n++
	fileName := fmt.Sprintf("cell%d", s.n)
	units, err := source.Split(source.StripShebang(src))
	if err != nil {
		return nil, 0, err
	}
	var imports, decls, stmts []source.Unit
	for _, u := range units {
		switch u.Kind {
		case source.Package:
		case source.Import:
			imports = append(imports, u)
		case source.Decl:
			decls = append(decls, u)
		default:
			stmts = append(stmts, u)
		}
	}
	if len(units) == 0 {
		return func() []reflect.Value { return nil }, 0, nil
	}
	for _, list := range [][]source.Unit{imports, decls, stmts} {
		for i := range list {
			list[i].File = fileName
		}
	}
	decls, s.notes = s.types.WithRedecls(decls)
	tuple := false
	if k := len(stmts); k > 0 && stmts[k-1].Line == units[len(units)-1].Line && stmts[k-1].Col == units[len(units)-1].Col &&
		source.IsExprList(stmts[k-1].Text) {
		stmts[k-1].Text = "_ = []any{" + stmts[k-1].Text + "}"
		tuple = true
	}

	// Explicit imports of this cell.
	cellImports := map[string]string{}
	for _, u := range imports {
		f, err := parser.ParseFile(in.fset, fileName, "package p;"+u.Text, parser.ImportsOnly)
		if err != nil {
			return nil, 0, err
		}
		for _, spec := range f.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			p, err := in.importPkg(path)
			if err != nil {
				return nil, 0, err
			}
			name := p.Name()
			if spec.Name != nil {
				name = spec.Name.Name
			}
			cellImports[name] = path
		}
	}

	compose := func(importLines []string) string {
		var b strings.Builder
		b.WriteString("package main\n")
		for _, l := range importLines {
			b.WriteString(l + "\n")
		}
		for _, u := range decls {
			b.WriteString(source.LineDirective(u.File, u.Line, u.Col) + u.Text + "\n")
		}
		b.WriteString("\nfunc " + runName + "() {")
		for _, u := range stmts {
			b.WriteString(source.LineDirective(fileName, u.Line, u.Col) + u.Text + "\n")
		}
		b.WriteString("}\n")
		return b.String()
	}
	f, err := parser.ParseFile(in.fset, fileName, compose(nil), 0)
	if err != nil {
		return nil, 0, err
	}
	declared := declaredNames(f)
	used := map[string]string{}
	std := stdIndex()
	for _, q := range source.UnresolvedQualifiers(f) {
		if path, ok := cellImports[q]; ok {
			used[q] = path
			continue
		}
		if s.pkg.Scope().Lookup(q) != nil || declared[q] {
			continue
		}
		if path, ok := s.imports[q]; ok {
			used[q] = path
		} else if path, ok := std[q]; ok {
			used[q] = path
		}
	}
	var importLines []string
	names := make([]string, 0, len(used))
	for q := range used {
		names = append(names, q)
	}
	sort.Strings(names)
	for _, q := range names {
		importLines = append(importLines, fmt.Sprintf("import %s %q", q, used[q]))
	}
	if len(importLines) > 0 {
		if f, err = parser.ParseFile(in.fset, fileName, compose(importLines), 0); err != nil {
			return nil, 0, err
		}
	}

	scope := s.pkg.Scope()
	restore, err := typescope.Snapshot(scope)
	if err != nil {
		return nil, 0, err
	}
	_ = typescope.Delete(scope, runName)
	for name := range declared {
		_ = typescope.Delete(scope, name)
	}
	for name := range used {
		_ = typescope.Delete(scope, name)
	}
	run := findFunc(f, runName)
	topLocals := map[token.Pos]bool{}
	for _, id := range topDefs(run) {
		topLocals[id.Pos()] = true
	}
	var lastExpr ast.Node
	if k := len(run.Body.List); k > 0 {
		if es, ok := run.Body.List[k-1].(*ast.ExprStmt); ok {
			lastExpr = es
		}
	}
	var errs []error
	conf := in.config(nil)
	conf.Error = func(err error) {
		te, ok := err.(types.Error)
		if ok && strings.HasPrefix(te.Msg, "declared and not used") && topLocals[te.Pos] {
			return
		}
		if ok && lastExpr != nil && strings.HasSuffix(te.Msg, "is not used") && te.Pos >= lastExpr.Pos() && te.Pos < lastExpr.End() {
			return
		}
		errs = append(errs, err)
	}
	info := in.newInfo()
	_ = types.NewChecker(conf, in.fset, s.pkg, info).Files([]*ast.File{f})
	if len(errs) > 0 {
		restore()
		return nil, 0, &CheckError{Errors: errs}
	}

	sp := &srcPkg{path: "main", pkg: s.pkg, info: info, files: []*ast.File{f}}
	in.registerDecls(sp, sp.files)
	for _, obj := range info.Defs {
		if v, ok := obj.(*types.Var); ok && v.Parent() == scope {
			if _, exists := in.globals[v]; !exists {
				in.globals[v] = reflect.New(in.rtype(v.Type())).Elem()
			}
		}
	}
	// Cell-level variables live as globals.
	var lifted []*types.Var
	for _, id := range topDefs(run) {
		if v, ok := info.Defs[id].(*types.Var); ok && id.Name != "_" {
			in.globals[v] = reflect.New(in.rtype(v.Type())).Elem()
			lifted = append(lifted, v)
		}
	}
	// The trailing expression's values are the result.
	var result *exprResult
	body := run.Body.List
	if lastExpr != nil {
		es := lastExpr.(*ast.ExprStmt)
		tv := info.Types[es.X]
		if !tv.IsVoid() && tv.Type != nil {
			result = &exprResult{}
			body = body[:len(body)-1]
		}
	}
	if tuple {
		as := body[len(body)-1].(*ast.AssignStmt)
		result = &exprResult{}
		body = body[:len(body)-1]
		lit := as.Rhs[0].(*ast.CompositeLit)
		for _, e := range lit.Elts {
			result.types = append(result.types, info.Types[e].Type)
		}
		lastExpr = &ast.ExprStmt{X: lit}
	}

	in.mu.Lock()
	var inits []func()
	var cellRun func()
	var resultVals []reflect.Value
	compileErr := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		for _, init := range info.InitOrder {
			init := init
			r, err := in.compileTop(sp, func(c *compiler) exec { return c.initializer(init) })
			if err != nil {
				return err
			}
			inits = append(inits, r)
		}
		r, err := in.compileTop(sp, func(c *compiler) exec {
			stmts := c.block(body)
			if result == nil {
				return stmts
			}
			x := lastExpr.(*ast.ExprStmt).X
			if lit, ok := x.(*ast.CompositeLit); ok && tuple {
				for _, e := range lit.Elts {
					result.evals = append(result.evals, c.exprN(e))
				}
			} else {
				result.evals = append(result.evals, c.exprN(x))
				if tup, ok := info.Types[x].Type.(*types.Tuple); ok {
					for i := 0; i < tup.Len(); i++ {
						result.types = append(result.types, tup.At(i).Type())
					}
				} else {
					result.types = append(result.types, info.Types[x].Type)
				}
			}
			evals := result.evals
			return func(fr *frame) ctrl {
				r := stmts(fr)
				var out []reflect.Value
				for _, ev := range evals {
					out = append(out, ev(fr)...)
				}
				resultVals = out
				return r
			}
		})
		cellRun = r
		return err
	}()
	in.mu.Unlock()
	if compileErr != nil {
		restore()
		return nil, 0, compileErr
	}

	// Commit: later cells see this cell's declarations.
	_ = typescope.Delete(scope, runName)
	for _, v := range lifted {
		_ = typescope.Delete(scope, v.Name())
		pv := types.NewVar(v.Pos(), s.pkg, v.Name(), v.Type())
		scope.Insert(pv)
		in.globals[pv] = in.globals[v]
	}
	for name, path := range cellImports {
		s.imports[name] = path
	}
	s.types.Record(decls)

	s.resultTypes = nil
	if result != nil {
		s.resultTypes = result.types
	}
	return func() []reflect.Value {
		for _, i := range inits {
			i()
		}
		resultVals = nil
		cellRun()
		return resultVals
	}, 1, nil
}

func declaredNames(f *ast.File) map[string]bool {
	m := map[string]bool{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name != runName && d.Name.Name != "init" {
				m[d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					m[sp.Name.Name] = true
				case *ast.ValueSpec:
					for _, id := range sp.Names {
						m[id.Name] = true
					}
				}
			}
		}
	}
	return m
}

func findFunc(f *ast.File, name string) *ast.FuncDecl {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == name {
			return fd
		}
	}
	return nil
}

// topDefs returns identifiers defined directly in fn's body.
func topDefs(fn *ast.FuncDecl) []*ast.Ident {
	var out []*ast.Ident
	for _, st := range fn.Body.List {
		switch st := st.(type) {
		case *ast.AssignStmt:
			if st.Tok == token.DEFINE {
				for _, e := range st.Lhs {
					if id, ok := e.(*ast.Ident); ok {
						out = append(out, id)
					}
				}
			}
		case *ast.DeclStmt:
			if gd, ok := st.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
				for _, sp := range gd.Specs {
					out = append(out, sp.(*ast.ValueSpec).Names...)
				}
			}
		}
	}
	return out
}

var (
	stdOnce sync.Once
	stdIdx  map[string]string
)

// stdIndex maps package names to import paths for automatic imports,
// from the packages the interpreter knows (no Go toolchain needed).
func stdIndex() map[string]string {
	stdOnce.Do(func() {
		stdIdx = map[string]string{}
		paths := native.Paths()
		for _, p := range []string{"cmp", "slices", "maps", "iter"} {
			if stdsrc.Package(p) != nil {
				paths = append(paths, p)
			}
		}
		sort.Strings(paths)
		for _, p := range paths {
			name := source.GuessName(p)
			if np := native.Lookup(p); np != nil {
				name = np.Name
			}
			if old, ok := stdIdx[name]; !ok || len(p) < len(old) {
				stdIdx[name] = p
			}
		}
		stdIdx["unsafe"] = "unsafe"
		for name, p := range map[string]string{"rand": "math/rand", "template": "text/template", "scanner": "text/scanner",
			"pprof": "runtime/pprof", "trace": "runtime/trace", "debug": "runtime/debug", "constant": "go/constant"} {
			stdIdx[name] = p
		}
	})
	return stdIdx
}

// StdIndex returns the package-name index used for automatic imports
// (package name -> import path) of the packages this interpreter provides.
func StdIndex() map[string]string { return stdIndex() }

// Symbol describes a session-level declaration.
type Symbol struct{ Name, Kind, Type string }

// Symbols lists what the interactive session defines.
func (in *Interp) Symbols() []Symbol {
	in.loadMu.Lock()
	defer in.loadMu.Unlock()
	if in.session == nil {
		return nil
	}
	scope := in.session.pkg.Scope()
	var out []Symbol
	for _, name := range scope.Names() {
		if name == runName {
			continue
		}
		obj := scope.Lookup(name)
		kind := "var"
		t := obj.Type()
		switch obj.(type) {
		case *types.TypeName:
			kind, t = "type", t.Underlying()
		case *types.Func:
			kind = "func"
		case *types.Const:
			kind = "const"
		}
		out = append(out, Symbol{name, kind, types.TypeString(t, func(p *types.Package) string {
			if p.Path() == "main" {
				return ""
			}
			return p.Name()
		})})
	}
	return out
}
