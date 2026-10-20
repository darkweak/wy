// Package interp evaluates Go code on the fly inside the running process.
//
// Unlike a tree-walking interpreter, interp compiles every input with the
// real Go compiler and loads it as a plugin. Interpreted code therefore
// supports the entire language (generics, goroutines, unsafe, reflection,
// method sets, embedding...), the whole standard library and any module,
// runs at native speed, and shares memory with the host: values cross the
// boundary as ordinary Go values, not wrappers.
//
// State persists across calls to Eval, like a REPL:
//
//	s, _ := interp.New(interp.Options{})
//	s.Eval(ctx, `type point struct{ x, y int }`)
//	s.Eval(ctx, `p := point{3, 4}`)
//	v, _ := s.Eval(ctx, `math.Hypot(float64(p.x), float64(p.y))`) // v[0] == 5.0
//
// Requirements: a Go toolchain on PATH at run time, and a platform with
// plugin support (Linux, macOS, FreeBSD) with cgo enabled in the host.
package interp

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"plugin"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
	"sync"

	"github.com/darkweak/wy/bridge"
	"github.com/darkweak/wy/internal/gocmd"
	"github.com/darkweak/wy/internal/source"
	"github.com/darkweak/wy/internal/typeexpr"
	"github.com/darkweak/wy/internal/typescope"
)

// Options configure a Session.
type Options struct {
	// Dir is the workspace directory for generated code and plugins. By
	// default a temporary directory is created and removed by Close.
	Dir string
	// WyRoot is the directory of wy's own module. It is found
	// automatically when the host was built from source on this machine.
	WyRoot string
	// HostModuleDir, if set, lets interpreted code import the packages of
	// the host's main module (sharing their live state with the host).
	HostModuleDir string
	// BuildFlags are extra flags for compiling inputs (the host's own flags,
	// such as -race or -tags, are reproduced automatically).
	BuildFlags []string
	// Log, if set, receives the go commands being run.
	Log io.Writer
}

// Session is a stateful interpreter. It is safe for concurrent use; inputs
// are evaluated one at a time.
type Session struct {
	mu      sync.Mutex
	opts    Options
	ws      *workspace
	fset    *token.FileSet
	imp     *pkgImporter
	pkg     *types.Package       // the session package all cells share
	origin  map[types.Object]int // object -> cell that declared it
	imports map[string]string    // remembered explicit imports
	types   source.TypeRegistry  // source of session types and their methods
	cells   []*Cell
	ownDir  bool
}

// Cell is one evaluated input.
type Cell struct {
	N         int
	Source    string
	Generated string // generated Go source, empty if nothing was compiled
	Warnings  []string
}

// New creates a session. It starts warming the build cache in the
// background so the first evaluation is fast.
func New(opts Options) (*Session, error) {
	ws, err := newWorkspace(opts)
	if err != nil {
		return nil, err
	}
	s := &Session{
		opts:    opts,
		ws:      ws,
		fset:    token.NewFileSet(),
		pkg:     types.NewPackage(sessionPath, "session"),
		origin:  map[types.Object]int{},
		imports: map[string]string{},
		ownDir:  opts.Dir == "",
	}
	s.imp = newImporter(ws, s.fset)
	go s.warm()
	return s, nil
}

func (s *Session) warm() {
	dir := filepath.Join(s.ws.dir, "warm")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(
		"package main\nimport (_ \"fmt\"; _ \""+bridgePath+"\")\nfunc Run() {}\n"), 0o644)
	_ = s.ws.buildPlugin(context.Background(), "warm", filepath.Join(s.ws.dir, "bin", "warm.so"))
}

// Close removes the session's temporary workspace. Code that was loaded
// stays in the process (Go cannot unload plugins).
func (s *Session) Close() error {
	if s.ownDir {
		return os.RemoveAll(s.ws.dir)
	}
	return nil
}

// Dir returns the workspace directory.
func (s *Session) Dir() string { return s.ws.dir }

// PanicError is returned when evaluated code panics. The session remains
// usable.
type PanicError struct {
	Value any
	Stack string
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// Eval evaluates src: any mix of imports, declarations and statements. If
// the last statement is an expression, its value(s) are returned.
// Top-level variables, types, functions and constants persist for later
// calls. Missing standard-library imports are added automatically.
//
// If ctx is cancelled while the code runs, Eval returns ctx.Err(); the code
// itself keeps running in the background because goroutines cannot be
// killed.
func (s *Session) Eval(ctx context.Context, src string) ([]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eval(ctx, src, nil)
}

func (s *Session) eval(ctx context.Context, src string, extra map[string]string) ([]any, error) {
	plan, err := s.compile(src, extra)
	if err != nil {
		return nil, err
	}
	cell := &Cell{N: plan.n, Source: src, Generated: plan.code, Warnings: plan.warnings}
	if plan.code == "" {
		return plan.constEcho, nil // a pure constant expression (or nothing)
	}

	run, err := s.load(ctx, plan)
	if err != nil {
		plan.restore()
		return nil, err
	}
	s.commit(plan, cell)

	type outcome struct {
		vals []any
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		var o outcome
		defer func() {
			if r := recover(); r != nil {
				o.err = &PanicError{Value: r, Stack: string(debug.Stack())}
			}
			done <- o
		}()
		o.vals = run()
	}()
	select {
	case o := <-done:
		if o.err != nil {
			return nil, o.err
		}
		if plan.constEcho != nil {
			return plan.constEcho, nil
		}
		if !plan.echo {
			return nil, nil
		}
		return o.vals, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// load writes, builds and opens the plugin for a cell.
func (s *Session) load(ctx context.Context, plan *cellPlan) (func() []any, error) {
	name := fmt.Sprintf("c%d", plan.n)
	dir := filepath.Join(s.ws.dir, name)
	if err := os.MkdirAll(filepath.Join(dir, "main"), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "cell.go"), []byte(plan.code), 0o644); err != nil {
		return nil, err
	}
	main := fmt.Sprintf("package main\n\nimport c %q\n\nfunc Run() []any { c.%s(); return c.%s }\n",
		s.ws.module+"/"+name, runFunc, resultVar)
	if err := os.WriteFile(filepath.Join(dir, "main", "main.go"), []byte(main), 0o644); err != nil {
		return nil, err
	}
	so := filepath.Join(s.ws.dir, "bin", name+".so")
	if err := s.ws.buildPlugin(ctx, name+"/main", so); err != nil {
		return nil, fmt.Errorf("compiling input: %w\n(generated source: %s)", err, filepath.Join(dir, "cell.go"))
	}
	p, err := plugin.Open(so)
	if err != nil {
		if strings.Contains(err.Error(), "seg_count") {
			return nil, fmt.Errorf("loading input: %w\n(macOS dyld rejected a plugin with DWARF debug info, golang.org/issue/81793; "+
				"wy %s links plugins with -ldflags=-w to avoid this: check that this wy is up to date, and run `wy -x` to see the build flags)",
				err, gocmd.WyVersion)
		}
		return nil, fmt.Errorf("loading input: %w", err)
	}
	sym, err := p.Lookup("Run")
	if err != nil {
		return nil, err
	}
	return sym.(func() []any), nil
}

// commit makes a loaded cell's declarations visible to later cells.
func (s *Session) commit(plan *cellPlan, cell *Cell) {
	s.cells = append(s.cells, cell)
	scope := s.pkg.Scope()
	_ = typescope.Delete(scope, runFunc)
	_ = typescope.Delete(scope, tupleFunc)
	for name := range plan.declared {
		if obj := scope.Lookup(name); obj != nil {
			s.origin[obj] = plan.n
		}
	}
	for _, v := range plan.lifted {
		_ = typescope.Delete(scope, v.Name())
		scope.Insert(v)
		s.origin[v] = plan.n
	}
	s.types.Record(plan.decls)
	for name, path := range plan.imports {
		if strings.HasPrefix(name, "_") {
			continue // internal aliases
		}
		s.imports[name] = path
	}
}

// Set defines (or redefines) a session variable holding v, which may be any
// Go value: interpreted code sees it with its real static type when that
// type can be named from the session, otherwise as `any`.
func (s *Session) Set(ctx context.Context, name string, v any) error {
	if !token.IsIdentifier(name) {
		return fmt.Errorf("invalid identifier %q", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := bridge.Put(v)
	extra := map[string]string{bridgeAlias: bridgePath}
	texpr := reflectTypeExpr(reflect.TypeOf(v), extra)
	src := fmt.Sprintf("%s := %s.Take(%q)", name, bridgeAlias, key)
	if texpr != "any" {
		src += ".(" + texpr + ")"
	}
	_, err := s.eval(ctx, src, extra)
	return err
}

// Get returns the current value of a session variable (or of any
// expression).
func (s *Session) Get(ctx context.Context, expr string) (any, error) {
	vals, err := s.Eval(ctx, expr)
	if err != nil {
		return nil, err
	}
	if len(vals) == 0 {
		return nil, errors.New("expression has no value")
	}
	return vals[0], nil
}

// Symbol describes a session-level declaration.
type Symbol struct {
	Name, Kind, Type string
	Cell             int
}

// Symbols lists what the session currently defines.
func (s *Session) Symbols() []Symbol {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Symbol
	scope := s.pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		kind := "var"
		switch obj.(type) {
		case *types.TypeName:
			kind = "type"
		case *types.Func:
			kind = "func"
		case *types.Const:
			kind = "const"
		}
		typ := types.TypeString(obj.Type(), func(p *types.Package) string {
			if isSession(p) {
				return ""
			}
			return p.Name()
		})
		if kind == "type" {
			typ = types.TypeString(obj.Type().Underlying(), func(p *types.Package) string {
				if isSession(p) {
					return ""
				}
				return p.Name()
			})
		}
		out = append(out, Symbol{Name: name, Kind: kind, Type: typ, Cell: s.origin[obj]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Cells returns the inputs evaluated so far.
func (s *Session) Cells() []*Cell {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Cell(nil), s.cells...)
}

// reflectTypeExpr spells t as Go source for Set, registering the imports
// it needs in imports (alias -> path). It returns "any" when t cannot be
// named from the session.
func reflectTypeExpr(t reflect.Type, imports map[string]string) string {
	s, ok := reflectType(t, imports)
	if !ok {
		return "any"
	}
	return s
}

func reflectType(t reflect.Type, imports map[string]string) (string, bool) {
	if t == nil {
		return "", false
	}
	if t.Name() != "" {
		if t.PkgPath() == "" {
			return t.Name(), true
		}
		if t.PkgPath() == "main" || strings.ContainsAny(t.Name(), "[") || !token.IsExported(t.Name()) {
			return "", false
		}
		alias := ""
		for a, p := range imports {
			if p == t.PkgPath() {
				alias = a
			}
		}
		if alias == "" {
			alias = fmt.Sprintf("_r%d", len(imports))
			imports[alias] = t.PkgPath()
		}
		return alias + "." + t.Name(), true
	}
	elem := func(prefix string, e reflect.Type) (string, bool) {
		s, ok := reflectType(e, imports)
		return prefix + s, ok
	}
	switch t.Kind() {
	case reflect.Pointer:
		return elem("*", t.Elem())
	case reflect.Slice:
		return elem("[]", t.Elem())
	case reflect.Array:
		return elem(fmt.Sprintf("[%d]", t.Len()), t.Elem())
	case reflect.Map:
		k, ok := reflectType(t.Key(), imports)
		if !ok {
			return "", false
		}
		return elem("map["+k+"]", t.Elem())
	case reflect.Chan:
		prefix := map[reflect.ChanDir]string{reflect.BothDir: "chan ", reflect.SendDir: "chan<- ", reflect.RecvDir: "<-chan "}[t.ChanDir()]
		return elem(prefix, t.Elem())
	case reflect.Func:
		var in, out []string
		for i := 0; i < t.NumIn(); i++ {
			s, ok := reflectType(t.In(i), imports)
			if !ok {
				return "", false
			}
			if t.IsVariadic() && i == t.NumIn()-1 {
				s = "..." + strings.TrimPrefix(s, "[]")
			}
			in = append(in, s)
		}
		for i := 0; i < t.NumOut(); i++ {
			s, ok := reflectType(t.Out(i), imports)
			if !ok {
				return "", false
			}
			out = append(out, s)
		}
		sig := "func(" + strings.Join(in, ", ") + ")"
		switch len(out) {
		case 0:
		case 1:
			sig += " " + out[0]
		default:
			sig += " (" + strings.Join(out, ", ") + ")"
		}
		return sig, true
	case reflect.Struct:
		var fields []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				return "", false
			}
			s, ok := reflectType(f.Type, imports)
			if !ok {
				return "", false
			}
			if f.Anonymous {
				fields = append(fields, s)
			} else {
				fields = append(fields, f.Name+" "+s)
			}
			if f.Tag != "" {
				fields[len(fields)-1] += " " + fmt.Sprintf("%q", string(f.Tag))
			}
		}
		return "struct{" + strings.Join(fields, "; ") + "}", true
	case reflect.Interface:
		if t.NumMethod() == 0 {
			return "any", true
		}
	}
	return "", false
}

var _ typeexpr.Namer = (*gen)(nil)
