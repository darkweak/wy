// Package tree is wy's tree-walking interpreter: it runs Go source
// without compiling it, like Yaegi, so it starts instantly and needs no Go
// toolchain at run time.
//
// It differs from Yaegi in how it is built:
//
//   - Type checking is done by go/types, the reference implementation, so
//     constants, conversions, method sets, generics and assignability follow
//     the spec exactly. The interpreter only executes checked code.
//   - Values live in real Go memory with gc-compatible layout (reflect types
//     built from the checked types), so unsafe, Sizeof/Offsetof, pointer
//     arithmetic and unsafe.Slice/String work on interpreted data.
//   - Interpreted types keep their identity inside interfaces, and are
//     presented to compiled code through generated wrappers that expose
//     exactly their methods (fmt.Stringer, error with Unwrap/Is/As, ...).
//   - Generic standard packages (slices, maps, cmp, iter) run from source.
//
// Code is compiled to a tree of Go closures before running, and compiled
// standard-library functions are called natively.
package tree

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/darkweak/wy/tree/native"
	_ "github.com/darkweak/wy/tree/stdlib"
	"github.com/darkweak/wy/tree/stdsrc"
)

// Options configure an interpreter.
type Options struct {
	// ModuleDir is a directory inside a Go module: packages of that module
	// can be imported (they are interpreted from source).
	ModuleDir string
	// Args are the program's command-line arguments (os.Args[1:]).
	Args []string
	// GoPath is a GOPATH-style root: packages that are neither standard,
	// registered, in the module of ModuleDir nor vendored are interpreted
	// from GoPath/src/<import path>.
	GoPath string
	// Forbid lists packages that interpreted code may not import, such as
	// "unsafe" or "syscall"; a path also forbids its subpackages. The
	// standard sources embedded in wy (slices, maps, ...) are exempt.
	Forbid []string
	// Env, if not nil, is the environment seen by interpreted code
	// ("key=value" entries): os.Getenv, os.Setenv and the other environment
	// functions use a private copy of it instead of the process environment.
	Env []string
	// Restricted keeps interpreted code from ending or signalling the host
	// process: os.Exit panics with an *ExitError (which interpreted code
	// cannot recover), the standard logger's Fatal functions panic like
	// Panic, and os.FindProcess refuses the host process.
	Restricted bool
	// Stdin is read by the fmt.Scan functions, and is os.Stdin for
	// interpreted code if it is an *os.File. Default: os.Stdin.
	Stdin io.Reader
	// Stdout receives the output of the fmt.Print functions, and is
	// os.Stdout for interpreted code if it is an *os.File. Default:
	// os.Stdout.
	Stdout io.Writer
	// Stderr receives the output of the print and println builtins, of the
	// standard logger and of flag.CommandLine, and is os.Stderr for
	// interpreted code if it is an *os.File. Default: os.Stderr.
	Stderr io.Writer
}

// Interp is a tree-walking Go interpreter.
type Interp struct {
	opts    Options
	fset    *token.FileSet
	tctx    *types.Context
	sizes   types.Sizes
	natives *natives

	rtMu sync.Mutex
	rts  *rtypes

	loadMu    sync.Mutex // serializes package loading and evaluation
	mu        sync.Mutex // guards compilation (never held while running code)
	pkgs      map[string]*types.Package
	src       map[string]*srcPkg
	globals   map[types.Object]reflect.Value
	funcDecls map[*types.Func]*funcDecl
	funcs     map[string]*function
	ntypes    ntypeTable
	modPath   string
	modRoot   string

	closures sync.Map // funcval pointer -> *closure

	overrides map[string]*native.Package // per-interpreter replacements of native packages

	session *session // interactive state (Eval)

}

type srcPkg struct {
	path  string
	pkg   *types.Package
	info  *types.Info
	files []*ast.File
}

type funcDecl struct {
	decl *ast.FuncDecl
	pkg  *srcPkg
}

// New creates an interpreter.
func New(opts Options) *Interp {
	in := &Interp{
		opts:      opts,
		fset:      token.NewFileSet(),
		tctx:      types.NewContext(),
		sizes:     types.SizesFor("gc", runtime.GOARCH),
		pkgs:      map[string]*types.Package{},
		src:       map[string]*srcPkg{},
		globals:   map[types.Object]reflect.Value{},
		funcDecls: map[*types.Func]*funcDecl{},
		funcs:     map[string]*function{},
	}
	in.natives = newNatives(in)
	in.rts = newRtypes(in)
	in.ntypes.in = in
	if opts.Stdin == nil {
		in.opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		in.opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		in.opts.Stderr = os.Stderr
	}
	in.overrides = in.hostOverrides()
	if opts.ModuleDir != "" {
		in.findModule(opts.ModuleDir)
	}
	// iter is needed to map compiled iterator types; load it up front.
	in.loadMu.Lock()
	if _, err := in.importPkg("iter", ""); err != nil {
		panic(err)
	}
	in.loadMu.Unlock()
	return in
}

func (in *Interp) findModule(dir string) {
	dir, _ = filepath.Abs(dir)
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
					in.modPath, in.modRoot = strings.Trim(f[1], `"`), dir
					return
				}
			}
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

// Import implements types.Importer, for the code being interpreted: it
// applies Options.Forbid.
func (in *Interp) Import(path string) (*types.Package, error) {
	return importer{in: in}.ImportFrom(path, "", 0)
}

func (in *Interp) importPkg(path, fromDir string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	if p, ok := in.pkgs[path]; ok {
		if p == nil {
			return nil, fmt.Errorf("import cycle through %s", path)
		}
		return p, nil
	}
	if files := stdsrc.Package(path); files != nil {
		return in.loadSource(path, files, true)
	}
	if np := native.Lookup(path); np != nil {
		in.rtMu.Lock()
		p := in.natives.buildPackage(np)
		in.rtMu.Unlock()
		in.pkgs[path] = p
		if files := stdsrc.Overlay(path); files != nil {
			// Generic declarations of the package, interpreted from
			// source, join its scope.
			op, err := in.loadSource("wy/overlay/"+path, files, true)
			if err != nil {
				return nil, err
			}
			for _, name := range op.Scope().Names() {
				if obj := op.Scope().Lookup(name); obj.Exported() && p.Scope().Lookup(name) == nil {
					p.Scope().Insert(obj)
				}
			}
		}
		return p, nil
	}
	if dir := in.sourceDir(path, fromDir); dir != "" {
		files, err := readPackageDir(dir)
		if err != nil {
			return nil, err
		}
		return in.loadSource(path, files, false)
	}
	return nil, fmt.Errorf("package %s is not available to the interpreter (no compiled symbols or source for it; "+
		"generate them with `wy extract`, vendor it, or use wy's compiled mode)", path)
}

// readPackageDir reads the non-test Go files of dir that match the current
// build context.
func readPackageDir(dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	ctx := build.Default
	files := map[string][]byte{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if ok, err := ctx.MatchFile(dir, name); err != nil || !ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		files[filepath.Join(dir, name)] = data
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no Go files in %s", dir)
	}
	return files, nil
}

func (in *Interp) newInfo() *types.Info {
	return &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Implicits:  map[ast.Node]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Instances:  map[*ast.Ident]types.Instance{},
		Scopes:     map[ast.Node]*types.Scope{},
	}
}

// config returns the type-checking configuration of a package; trusted
// packages (wy's embedded sources) are exempt from Options.Forbid.
func (in *Interp) config(errs *[]error, trusted bool) *types.Config {
	return &types.Config{
		Importer: importer{in: in, trusted: trusted},
		Sizes:    in.sizes,
		Context:  in.tctx,
		Error: func(err error) {
			if errs != nil {
				*errs = append(*errs, err)
			}
		},
	}
}

// loadSource parses, type-checks and initializes a source package.
func (in *Interp) loadSource(path string, files map[string][]byte, trusted bool) (*types.Package, error) {
	in.pkgs[path] = nil // cycle marker
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var astFiles []*ast.File
	for _, n := range names {
		f, err := parser.ParseFile(in.fset, n, files[n], parser.SkipObjectResolution)
		if err != nil {
			delete(in.pkgs, path)
			return nil, err
		}
		astFiles = append(astFiles, f)
	}
	var errs []error
	info := in.newInfo()
	pkg, _ := in.config(&errs, trusted).Check(path, in.fset, astFiles, info)
	if len(errs) > 0 {
		delete(in.pkgs, path)
		return nil, &CheckError{Errors: errs}
	}
	in.pkgs[path] = pkg
	sp := &srcPkg{path: path, pkg: pkg, info: info, files: astFiles}
	in.src[path] = sp
	in.registerDecls(sp, astFiles)
	if err := in.initPackage(sp, astFiles); err != nil {
		return nil, err
	}
	return pkg, nil
}

func (in *Interp) registerDecls(sp *srcPkg, files []*ast.File) {
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				if obj, ok := sp.info.Defs[fd.Name].(*types.Func); ok {
					in.funcDecls[obj] = &funcDecl{decl: fd, pkg: sp}
				}
			}
		}
	}
}

// initPackage allocates package variables, runs their initializers in
// dependency order, then the init functions.
func (in *Interp) initPackage(sp *srcPkg, files []*ast.File) error {
	scope := sp.pkg.Scope()
	for _, name := range scope.Names() {
		if v, ok := scope.Lookup(name).(*types.Var); ok {
			if _, exists := in.globals[v]; !exists {
				in.globals[v] = reflect.New(in.rtype(v.Type())).Elem()
			}
		}
	}
	for _, init := range sp.info.InitOrder {
		for _, v := range init.Lhs {
			if _, ok := in.globals[v]; !ok {
				in.globals[v] = reflect.New(in.rtype(v.Type())).Elem()
			}
		}
		in.mu.Lock()
		run, err := in.compileTop(sp, func(c *compiler) exec { return c.initializer(init) })
		in.mu.Unlock()
		if err != nil {
			return err
		}
		if err := in.protect(func() { run() }); err != nil {
			return err
		}
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Name.Name != "init" || fd.Body == nil {
				continue
			}
			obj := sp.info.Defs[fd.Name].(*types.Func)
			fn, err := in.lockedFuncFor(obj, nil)
			if err != nil {
				return err
			}
			if err := in.protect(func() { fn.call(nil, nil, nil) }); err != nil {
				return err
			}
		}
	}
	return nil
}

// lockedFuncFor compiles a function under the compile lock, turning
// compilation failures into errors.
func (in *Interp) lockedFuncFor(obj *types.Func, targs []types.Type) (fn *function, err error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return in.funcFor(obj, targs), nil
}

// CheckError reports type errors.
type CheckError struct{ Errors []error }

func (e *CheckError) Error() string {
	msgs := make([]string, len(e.Errors))
	for i, err := range e.Errors {
		msgs[i] = err.Error()
	}
	return strings.Join(msgs, "\n")
}

// PanicError is returned when interpreted code panics and does not recover.
type PanicError struct {
	Value any
	Stack string
}

func (e *PanicError) Error() string { return "panic: " + formatPanic(e.Value) }

// ExitError is returned when interpreted code calls os.Exit.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

func formatPanic(v any) string {
	switch x := v.(type) {
	case error:
		s := x.Error()
		if _, ok := v.(runtimeError); ok {
			return s
		}
		var re runtime.Error
		if errors.As(x, &re) {
			return s
		}
		return s
	case boxed:
		return fmt.Sprintf("%v", x)
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	}
	return fmt.Sprintf("%v", v)
}

// protect runs f, converting a panic into a *PanicError.
func (in *Interp) protect(f func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if ee, ok := r.(*ExitError); ok {
				err = ee
				return
			}
			err = &PanicError{Value: r, Stack: string(debugStack())}
		}
	}()
	f()
	return nil
}

// RunFiles interprets a main package made of the given files (name ->
// source) and calls its main function.
func (in *Interp) RunFiles(ctx context.Context, files map[string][]byte) error {
	in.loadMu.Lock()
	pkg, err := in.loadSourceMain(files)
	if err != nil {
		in.loadMu.Unlock()
		return err
	}
	mainObj, _ := pkg.Scope().Lookup("main").(*types.Func)
	if mainObj == nil {
		in.loadMu.Unlock()
		return errors.New("function main is undeclared in the main package")
	}
	fn, err := in.lockedFuncFor(mainObj, nil)
	in.loadMu.Unlock()
	if err != nil {
		return err
	}
	return in.protect(func() { fn.call(nil, nil, nil) })
}

func (in *Interp) loadSourceMain(files map[string][]byte) (*types.Package, error) {
	pkg, err := in.loadSource("main", files, false)
	if err != nil {
		return nil, err
	}
	if pkg.Name() != "main" {
		return nil, fmt.Errorf("package %s is not a main package", pkg.Name())
	}
	return pkg, nil
}
