package interp

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"
	"wy/internal/typescope"

	"wy/internal/source"
	"wy/internal/typeexpr"
)

// How interactive inputs become Go packages
//
// Every input ("cell") N is compiled into its own package <module>/cN, built
// as a plugin and loaded into the running process, where it stays. To let
// later cells use what earlier cells declared, all cells are type-checked as
// if they were one package (path "session"): objects from earlier cells are
// injected into each new cell's package scope, so unexported names resolve
// and redefinitions simply shadow. Code generation then rewrites every
// reference to an earlier cell into a qualified reference (_c3.X_point),
// exporting session identifiers by mangling (point -> X_point) because
// cells are separate Go packages. Top-level variables of a cell (x := ...)
// are lifted to package level, with their inferred type written out, so they
// outlive the cell.

const (
	sessionPath = "session"
	runFunc     = "Wy_run"
	resultVar   = "Wy_result"
	bridgeAlias = "_wy"
	tupleFunc   = "Wy_tuple"
)

// mangle exports a session identifier while staying deterministic, so that
// method names line up across cells and interfaces keep being satisfied.
func mangle(name string) string {
	if name == "_" || name == "init" || token.IsExported(name) {
		return name
	}
	return "X_" + name
}

// hiddenTag is put on struct fields that were unexported in the user's code
// and became exported through mangling, so that encoding packages keep
// ignoring them as they would have.
const hiddenTag = `json:"-" xml:"-" yaml:"-" toml:"-"`

func isSession(p *types.Package) bool { return p != nil && p.Path() == sessionPath }

// cellPlan is the outcome of compiling one input.
type cellPlan struct {
	n         int
	code      string // generated package source ("" when nothing must run)
	pkg       *types.Package
	lifted    []*types.Var
	imports   map[string]string // explicit imports to remember (name -> path)
	echo      bool              // result values are produced at run time
	constEcho []any             // result known at compile time
	warnings  []string
	declared  map[string]bool // package-level names declared by this cell
	restore   func()          // undoes this cell's effect on the session scope
	decls     []source.Unit   // declaration units, for the type registry
}

// CompileError reports type errors in an input.
type CompileError struct{ Errors []string }

func (e *CompileError) Error() string { return strings.Join(e.Errors, "\n") }

func (s *Session) compile(src string, extra map[string]string) (*cellPlan, error) {
	n := len(s.cells) + 1
	fileName := fmt.Sprintf("cell%d", n)
	if extra != nil {
		fileName = "internal"
	}
	units, err := source.Split(source.StripShebang(src))
	if err != nil {
		return nil, err
	}
	var imports, decls, vars, stmts []source.Unit
	for _, u := range units {
		switch u.Kind {
		case source.Package:
			// Whole files are accepted: their package clause is dropped.
		case source.Import:
			imports = append(imports, u)
		case source.Decl:
			decls = append(decls, u)
		case source.Var:
			vars = append(vars, u)
		default:
			stmts = append(stmts, u)
		}
	}
	if len(units) == 0 {
		return &cellPlan{n: n}, nil
	}
	for _, list := range [][]source.Unit{imports, decls, vars, stmts} {
		for i := range list {
			list[i].File = fileName
		}
	}
	decls, notes := s.types.WithRedecls(decls)

	// A trailing "a, b" echoes several values.
	last := units[len(units)-1]
	if k := len(stmts); k > 0 && stmts[k-1].Line == last.Line && stmts[k-1].Col == last.Col && source.IsExprList(stmts[k-1].Text) {
		stmts[k-1].Text = tupleFunc + "(" + stmts[k-1].Text + ")"
		decls = append(decls, source.Unit{Kind: source.Decl, Text: "func " + tupleFunc + "(v ...any) []any { return v }",
			Line: stmts[k-1].Line, Col: 1})
	}

	// Explicit imports of this cell.
	cellImports := map[string]string{}
	var blank []string
	for _, u := range imports {
		f, err := parser.ParseFile(s.fset, fileName, "package p;"+u.Text, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, spec := range f.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			name := ""
			if spec.Name != nil {
				name = spec.Name.Name
			}
			switch name {
			case ".":
				return nil, fmt.Errorf("%s:%d: dot imports are not supported interactively", fileName, u.Line)
			case "_":
				blank = append(blank, path)
				continue
			case "":
				p, err := s.imp.Import(path)
				if err != nil {
					return nil, err
				}
				name = p.Name()
			}
			cellImports[name] = path
		}
	}
	for k, v := range extra {
		cellImports[k] = v
	}

	// Mode 0 keeps var declarations in statement order inside the run
	// function (they are lifted afterwards). If that fails and the cell also
	// declares functions, mode 1 retries with them at package level, which
	// lets functions in the same input refer to those variables.
	plan, err := s.compileMode(n, fileName, cellImports, blank, decls, vars, stmts, false)
	if err != nil && len(vars) > 0 && len(decls) > 0 {
		if p2, err2 := s.compileMode(n, fileName, cellImports, blank, decls, vars, stmts, true); err2 == nil {
			plan, err = p2, nil
		}
	}
	if plan != nil {
		plan.decls = decls
		plan.warnings = append(notes, plan.warnings...)
	}
	return plan, err
}

func (s *Session) compileMode(n int, fileName string, cellImports map[string]string, blank []string,
	decls, vars, stmts []source.Unit, varsAtTop bool) (*cellPlan, error) {

	compose := func(importLines []string) string {
		var b strings.Builder
		b.WriteString("package session\n")
		for _, l := range importLines {
			b.WriteString(l + "\n")
		}
		top := decls
		body := stmts
		if varsAtTop {
			top = append(append([]source.Unit{}, vars...), decls...)
		} else {
			body = mergeByPosition(vars, stmts)
		}
		for _, u := range top {
			b.WriteString(source.LineDirective(unitFile(u, fileName), u.Line, u.Col) + u.Text + "\n")
		}
		b.WriteString("\nfunc " + runFunc + "() {")
		for _, u := range body {
			b.WriteString(source.LineDirective(unitFile(u, fileName), u.Line, u.Col) + u.Text + "\n")
		}
		b.WriteString("}\n")
		return b.String()
	}

	// First parse without imports to discover which package names are used.
	f, err := parser.ParseFile(s.fset, fileName, compose(nil), 0)
	if err != nil {
		return nil, err
	}
	declared := packageLevelNames(f)
	importLines := []string{}
	used := map[string]string{}
	std, _, _ := source.Std()
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
	names := make([]string, 0, len(used))
	for q := range used {
		names = append(names, q)
	}
	sort.Strings(names)
	for _, q := range names {
		importLines = append(importLines, fmt.Sprintf("import %s %q", q, used[q]))
	}
	for _, p := range blank {
		importLines = append(importLines, fmt.Sprintf("import _ %q", p))
	}
	if len(importLines) > 0 {
		if f, err = parser.ParseFile(s.fset, fileName, compose(importLines), 0); err != nil {
			return nil, err
		}
	}

	// Methods must be declared together with their type: earlier cells are
	// already compiled packages that cannot gain methods.
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && len(fd.Recv.List) > 0 {
			base := source.ReceiverBase(fd.Recv.List[0].Type)
			if base != "" && !declared[base] && s.pkg.Scope().Lookup(base) != nil {
				return nil, &CompileError{Errors: []string{fmt.Sprintf(
					"%s: cannot add method %s to %s: declare methods in the same input as their type (redeclare %s together with its methods)",
					s.fset.Position(fd.Name.Pos()), fd.Name.Name, base, base)}}
			}
		}
	}

	// Type-check in the shared session package, where earlier cells'
	// declarations live. Names this cell redeclares or imports are removed
	// first; everything is rolled back if the cell fails.
	scope := s.pkg.Scope()
	restore, err := typescope.Snapshot(scope)
	if err != nil {
		return nil, err
	}
	_ = typescope.Delete(scope, runFunc)
	_ = typescope.Delete(scope, tupleFunc)
	for name := range declared {
		_ = typescope.Delete(scope, name)
	}
	for name := range used {
		_ = typescope.Delete(scope, name)
	}
	pkg := s.pkg
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	run := findRun(f)
	topLocals := topLevelDefIdents(run)
	var lastExpr ast.Node
	if k := len(run.Body.List); k > 0 {
		if es, ok := run.Body.List[k-1].(*ast.ExprStmt); ok {
			lastExpr = es
		}
	}
	var errs []string
	conf := types.Config{
		Importer: s.imp,
		Error: func(err error) {
			te, ok := err.(types.Error)
			if ok && strings.HasPrefix(te.Msg, "declared and not used") && topLocals[te.Pos] {
				return // cell-level variables persist; they need not be used
			}
			if ok && lastExpr != nil && strings.HasSuffix(te.Msg, "is not used") &&
				te.Pos >= lastExpr.Pos() && te.Pos < lastExpr.End() {
				return // a trailing expression is echoed
			}
			errs = append(errs, err.Error())
		},
	}
	_ = types.NewChecker(&conf, s.fset, pkg, info).Files([]*ast.File{f})
	if len(errs) > 0 {
		restore()
		return nil, &CompileError{Errors: errs}
	}

	plan := &cellPlan{n: n, pkg: pkg, imports: cellImports, declared: declared, restore: restore}
	g := &gen{s: s, n: n, pkg: pkg, info: info, lifted: map[types.Object]bool{},
		cells: map[int]bool{}, typeImports: map[string]string{}}

	// Decide which cell-level variables persist.
	var liftDecls []string
	var keepLocal []string
	for _, id := range topLevelDefs(run) {
		obj, _ := info.Defs[id].(*types.Var)
		if obj == nil || obj.Name() == "_" {
			continue
		}
		if declared[obj.Name()] {
			keepLocal = append(keepLocal, obj.Name())
			plan.warnings = append(plan.warnings, fmt.Sprintf("%s is local to this input (it clashes with a declaration)", obj.Name()))
			continue
		}
		texpr, err := typeexpr.String(obj.Type(), g)
		if err != nil {
			keepLocal = append(keepLocal, obj.Name())
			plan.warnings = append(plan.warnings, fmt.Sprintf("%s is local to this input: %v", obj.Name(), err))
			continue
		}
		g.lifted[obj] = true
		liftDecls = append(liftDecls, fmt.Sprintf("var %s %s", mangle(obj.Name()), texpr))
		plan.lifted = append(plan.lifted, types.NewVar(obj.Pos(), pkg, obj.Name(), obj.Type()))
	}

	// Trailing expression: echo its value.
	if k := len(run.Body.List); k > 0 {
		if es, ok := run.Body.List[k-1].(*ast.ExprStmt); ok {
			tv := info.Types[es.X]
			_, named := types.Unalias(tv.Type).(*types.Named)
			switch {
			case isPrintCall(es.X, info):
				// fmt.Println(...) & co: the output is the point, not (n, err).
			case tv.Value != nil && !named:
				plan.constEcho = []any{constValue(tv)}
				run.Body.List = run.Body.List[:k-1]
			case tv.IsNil():
				plan.constEcho = []any{nil}
				run.Body.List = run.Body.List[:k-1]
			case !tv.IsVoid() && tv.Type != nil:
				plan.echo = true
				g.useBridge = true
				args := []ast.Expr{es.X}
				if call, ok := es.X.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == tupleFunc {
						args = call.Args
					}
				}
				run.Body.List[k-1] = &ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent(resultVar)},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{&ast.CallExpr{Fun: ast.NewIdent(bridgeAlias + ".Values"), Args: args}},
				}
			}
		}
	}
	// A cell made only of a constant expression needs no compilation.
	if len(plan.constEcho) > 0 && len(run.Body.List) == 0 && len(f.Decls) == len(importDecls(f))+1 && len(blank) == 0 {
		plan.code = ""
		return plan, nil
	}

	if err := g.rewrite(f, run); err != nil {
		restore()
		return nil, err
	}
	for _, name := range keepLocal {
		run.Body.List = append(run.Body.List, &ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent("_")}, Tok: token.ASSIGN, Rhs: []ast.Expr{ast.NewIdent(name)}})
	}
	pruneImports(f)
	g.addImports(f)
	f.Name = ast.NewIdent(fmt.Sprintf("c%d", n))
	f.Comments = nil

	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.SourcePos | printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	if err := cfg.Fprint(&buf, s.fset, f); err != nil {
		restore()
		return nil, err
	}
	buf.WriteString("\n//line generated:1\n")
	for _, d := range liftDecls {
		buf.WriteString(d + "\n")
	}
	fmt.Fprintf(&buf, "var %s []any\n", resultVar)
	plan.code = buf.String()
	return plan, nil
}

// gen holds code generation state for one cell.
type gen struct {
	s           *Session
	n           int
	pkg         *types.Package
	info        *types.Info
	lifted      map[types.Object]bool
	cells       map[int]bool      // earlier cells referenced
	typeImports map[string]string // import path -> alias, for written-out types
	useBridge   bool
}

// identName decides how an identifier referring to obj is spelled in the
// generated package.
func (g *gen) identName(obj types.Object) (string, bool) {
	switch obj.(type) {
	case nil, *types.PkgName, *types.Builtin, *types.Nil, *types.Label:
		return "", false
	}
	if j, ok := g.s.origin[obj]; ok {
		g.cells[j] = true
		return fmt.Sprintf("_c%d.%s", j, mangle(obj.Name())), true
	}
	if g.lifted[obj] {
		return mangle(obj.Name()), true
	}
	if obj.Pkg() == g.pkg && obj.Parent() == g.pkg.Scope() {
		return mangle(obj.Name()), true
	}
	switch o := obj.(type) {
	case *types.Var:
		if o.IsField() && isSession(o.Pkg()) {
			return mangle(o.Name()), true
		}
	case *types.Func:
		if sig, ok := o.Type().(*types.Signature); ok && sig.Recv() != nil && isSession(o.Pkg()) {
			return mangle(o.Name()), true
		}
	}
	return "", false
}

// TypeName implements typeexpr.Namer.
func (g *gen) TypeName(obj *types.TypeName) (string, error) {
	if obj.Pkg() == nil {
		return obj.Name(), nil // predeclared: error, any, comparable
	}
	if name, ok := g.identName(obj); ok {
		return name, nil
	}
	if isSession(obj.Pkg()) {
		return "", &typeexpr.Unnameable{What: obj.Name() + " (declared inside a function)"}
	}
	if !obj.Exported() || obj.Parent() != obj.Pkg().Scope() {
		return "", &typeexpr.Unnameable{What: obj.Pkg().Path() + "." + obj.Name() + " (not exported)"}
	}
	return g.typeImport(obj.Pkg().Path()) + "." + obj.Name(), nil
}

// Field implements typeexpr.Namer.
func (g *gen) Field(v *types.Var, tag string) (string, string, error) {
	if isSession(v.Pkg()) {
		if !v.Embedded() && !token.IsExported(v.Name()) {
			tag = hiddenTag
		}
		return mangle(v.Name()), tag, nil
	}
	if !v.Exported() {
		return "", "", &typeexpr.Unnameable{What: "struct with unexported field " + v.Name()}
	}
	return v.Name(), tag, nil
}

// Method implements typeexpr.Namer.
func (g *gen) Method(f *types.Func) (string, error) {
	if isSession(f.Pkg()) {
		return mangle(f.Name()), nil
	}
	if !f.Exported() {
		return "", &typeexpr.Unnameable{What: "interface with unexported method " + f.Name()}
	}
	return f.Name(), nil
}

func (g *gen) typeImport(path string) string {
	if a, ok := g.typeImports[path]; ok {
		return a
	}
	a := fmt.Sprintf("_t%d", len(g.typeImports))
	g.typeImports[path] = a
	return a
}

// rewrite renames identifiers and turns top-level variable definitions of
// the run function into assignments to the lifted package-level variables.
func (g *gen) rewrite(f *ast.File, run *ast.FuncDecl) error {
	hideUnexportedFields(f)

	// Lifted definitions first, while identifiers still carry their objects.
	var newList []ast.Stmt
	for _, st := range run.Body.List {
		newList = append(newList, g.liftStmt(st)...)
	}
	run.Body.List = newList

	for id, obj := range g.info.Defs {
		if v, ok := obj.(*types.Var); ok && v.Embedded() {
			continue // spelled through the type name (a Use of the same ident)
		}
		if name, ok := g.identName(obj); ok && !strings.HasPrefix(id.Name, "__wy_tmp_") {
			id.Name = name
		}
	}
	for id, obj := range g.info.Uses {
		if name, ok := g.identName(obj); ok {
			id.Name = name
		}
	}
	return nil
}

// liftStmt rewrites a top-level statement of the run function that defines
// lifted variables.
func (g *gen) liftStmt(st ast.Stmt) []ast.Stmt {
	switch st := st.(type) {
	case *ast.AssignStmt:
		if st.Tok != token.DEFINE {
			return []ast.Stmt{st}
		}
		var newIdents, liftedIdents []*ast.Ident
		for _, e := range st.Lhs {
			id := e.(*ast.Ident)
			if obj := g.info.Defs[id]; obj != nil {
				newIdents = append(newIdents, id)
				if g.lifted[obj] {
					liftedIdents = append(liftedIdents, id)
				}
			}
		}
		if len(liftedIdents) == 0 {
			return []ast.Stmt{st}
		}
		if len(liftedIdents) == len(newIdents) {
			st.Tok = token.ASSIGN
			return []ast.Stmt{st}
		}
		return append([]ast.Stmt{st}, g.viaTemps(liftedIdents)...)
	case *ast.DeclStmt:
		gd, ok := st.Decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			return []ast.Stmt{st}
		}
		var out []ast.Stmt
		var keep []ast.Spec
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			var liftedIdents []*ast.Ident
			for _, id := range vs.Names {
				if g.lifted[g.info.Defs[id]] {
					liftedIdents = append(liftedIdents, id)
				}
			}
			switch {
			case len(liftedIdents) == 0:
				keep = append(keep, vs)
			case len(liftedIdents) == len(vs.Names):
				if len(vs.Values) > 0 {
					lhs := make([]ast.Expr, len(vs.Names))
					for i, id := range vs.Names {
						lhs[i] = id
					}
					out = append(out, &ast.AssignStmt{Lhs: lhs, Tok: token.ASSIGN, Rhs: vs.Values})
				}
			default:
				keep = append(keep, vs)
				out = append(out, g.viaTemps(liftedIdents)...)
			}
		}
		if len(keep) > 0 {
			gd.Specs = keep
			out = append([]ast.Stmt{st}, out...)
		}
		return out
	}
	return []ast.Stmt{st}
}

// viaTemps keeps idents as locals under temporary names and copies them into
// the lifted variables afterwards; used when one statement defines both
// persistent and non-persistent variables.
func (g *gen) viaTemps(ids []*ast.Ident) []ast.Stmt {
	var out []ast.Stmt
	for _, id := range ids {
		orig := id.Name
		id.Name = "__wy_tmp_" + orig
		out = append(out, &ast.AssignStmt{
			Lhs: []ast.Expr{ast.NewIdent(mangle(orig))},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{ast.NewIdent(id.Name)},
		})
	}
	return out
}

func (g *gen) addImports(f *ast.File) {
	var specs []ast.Spec
	cells := make([]int, 0, len(g.cells))
	for j := range g.cells {
		cells = append(cells, j)
	}
	sort.Ints(cells)
	for _, j := range cells {
		specs = append(specs, importSpec(fmt.Sprintf("_c%d", j), fmt.Sprintf("%s/c%d", g.s.ws.module, j)))
	}
	paths := make([]string, 0, len(g.typeImports))
	for p := range g.typeImports {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		specs = append(specs, importSpec(g.typeImports[p], p))
	}
	if g.useBridge {
		specs = append(specs, importSpec(bridgeAlias, bridgePath))
	}
	if len(specs) == 0 {
		return
	}
	f.Decls = append([]ast.Decl{&ast.GenDecl{Tok: token.IMPORT, Lparen: 1, Specs: specs}}, f.Decls...)
}

// pruneImports drops named imports that the rewritten code no longer uses
// (e.g. a package only mentioned in a lifted variable's declaration).
func pruneImports(f *ast.File) {
	used := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				used[id.Name] = true
			}
		}
		return true
	})
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		var keep []ast.Spec
		for _, sp := range gd.Specs {
			is := sp.(*ast.ImportSpec)
			if is.Name == nil || is.Name.Name == "_" || used[is.Name.Name] {
				keep = append(keep, is)
			}
		}
		gd.Specs = keep
	}
}

func importSpec(name, path string) *ast.ImportSpec {
	return &ast.ImportSpec{Name: ast.NewIdent(name), Path: &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(path)}}
}

// hideUnexportedFields gives fields that were unexported in source the
// hiddenTag, splitting declarations like "A, b int" so only b changes.
func hideUnexportedFields(f *ast.File) {
	tag := &ast.BasicLit{Kind: token.STRING, Value: "`" + hiddenTag + "`"}
	ast.Inspect(f, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		var out []*ast.Field
		for _, fld := range st.Fields.List {
			if len(fld.Names) == 0 {
				out = append(out, fld) // embedded
				continue
			}
			var exp, unexp []*ast.Ident
			for _, id := range fld.Names {
				if token.IsExported(id.Name) || id.Name == "_" {
					exp = append(exp, id)
				} else {
					unexp = append(unexp, id)
				}
			}
			if len(exp) > 0 {
				out = append(out, &ast.Field{Names: exp, Type: fld.Type, Tag: fld.Tag})
			}
			if len(unexp) > 0 {
				out = append(out, &ast.Field{Names: unexp, Type: fld.Type, Tag: tag})
			}
		}
		st.Fields.List = out
		return true
	})
}

// isPrintCall reports whether e calls a printing function of fmt or log.
func isPrintCall(e ast.Expr, info *types.Info) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	fn, ok := info.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil {
		return false
	}
	switch fn.Pkg().Path() {
	case "fmt", "log":
		return strings.Contains(fn.Name(), "Print")
	}
	return false
}

func unitFile(u source.Unit, def string) string {
	if u.File != "" {
		return u.File
	}
	return def
}

func findRun(f *ast.File) *ast.FuncDecl {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == runFunc {
			return fd
		}
	}
	return nil
}

func importDecls(f *ast.File) []ast.Decl {
	var out []ast.Decl
	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			out = append(out, d)
		}
	}
	return out
}

// packageLevelNames lists names declared at package level by the user (the
// run function excluded).
func packageLevelNames(f *ast.File) map[string]bool {
	m := map[string]bool{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name != runFunc && d.Name.Name != "init" && d.Name.Name != tupleFunc {
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

// topLevelDefs returns identifiers defined directly in the body of fn by
// := or var.
func topLevelDefs(fn *ast.FuncDecl) []*ast.Ident {
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

func topLevelDefIdents(fn *ast.FuncDecl) map[token.Pos]bool {
	m := map[token.Pos]bool{}
	for _, id := range topLevelDefs(fn) {
		m[id.Pos()] = true
	}
	return m
}

func mergeByPosition(a, b []source.Unit) []source.Unit {
	out := append(append([]source.Unit{}, a...), b...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Col < out[j].Col
	})
	return out
}

// constValue converts a compile-time constant to a Go value of its
// (default) type, keeping arbitrary precision when it does not fit.
func constValue(tv types.TypeAndValue) any {
	v := tv.Value
	basic, _ := tv.Type.Underlying().(*types.Basic)
	if basic != nil && basic.Info()&types.IsUntyped != 0 {
		basic = types.Default(basic).(*types.Basic)
	}
	switch v.Kind() {
	case constant.Bool:
		return constant.BoolVal(v)
	case constant.String:
		return constant.StringVal(v)
	case constant.Int:
		if basic != nil && basic.Kind() == types.Int32 { // rune
			if i, ok := constant.Int64Val(v); ok {
				return rune(i)
			}
		}
		if i, ok := constant.Int64Val(v); ok && basic != nil && basic.Info()&types.IsUnsigned == 0 {
			return int(i)
		}
		if u, ok := constant.Uint64Val(v); ok {
			return u
		}
		return BigConst(v.ExactString())
	case constant.Float:
		f, _ := constant.Float64Val(v)
		return f
	case constant.Complex:
		re, _ := constant.Float64Val(constant.Real(v))
		im, _ := constant.Float64Val(constant.Imag(v))
		return complex(re, im)
	}
	return v.ExactString()
}

// BigConst is an integer constant too large for any Go integer type.
type BigConst string

func (b BigConst) String() string { return string(b) }
