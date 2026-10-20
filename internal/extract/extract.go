// Package extract generates native symbol tables (see wy/tree/native) for
// compiled packages, so that the tree-walking interpreter can call them.
// For every exported, non-generic symbol it emits a reflect handle, and for
// every interface type a wrapper struct that implements it by forwarding to
// interpreted code. Symbols are intersected across several GOOS values so
// the generated files compile on all of them.
package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/constant"
	"go/format"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"wy/internal/gocmd"
)

// Options configure a generation run.
type Options struct {
	Packages []string // import paths to extract
	OutDir   string   // directory for generated files
	PkgName  string   // package clause of generated files
	GOOS     []string // platforms whose common API is extracted (default: current)
	Dir      string   // directory to run `go list` in (module context)
	Native   string   // import path of the native registry package
	Log      io.Writer
}

type symbol struct {
	name string
	kind string // func, var, type, const, iface
}

// Run generates one file per package.
func Run(ctx context.Context, opts Options) error {
	if len(opts.GOOS) == 0 {
		opts.GOOS = []string{os.Getenv("GOOS")}
		if opts.GOOS[0] == "" {
			out, err := gocmd.Run(ctx, opts.Dir, nil, "env", "GOOS")
			if err != nil {
				return err
			}
			opts.GOOS[0] = strings.TrimSpace(string(out))
		}
	}
	if opts.Native == "" {
		opts.Native = "wy/tree/native"
	}
	// Load the packages for every platform; keep the first for generation.
	var primary map[string]*types.Package
	common := map[string]map[symbol]bool{}
	for i, goos := range opts.GOOS {
		pkgs, err := load(ctx, opts.Dir, goos, opts.Packages)
		if err != nil {
			return fmt.Errorf("GOOS=%s: %w", goos, err)
		}
		for _, path := range opts.Packages {
			p := pkgs[path]
			if p == nil {
				return fmt.Errorf("GOOS=%s: package %s not loaded", goos, path)
			}
			syms := symbols(p)
			if i == 0 {
				common[path] = syms
				continue
			}
			for s := range common[path] {
				if !syms[s] {
					delete(common[path], s)
				}
			}
		}
		if i == 0 {
			primary = pkgs
		}
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return err
	}
	for _, path := range opts.Packages {
		src, err := generate(primary[path], common[path], opts)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		name := "zsym_" + strings.NewReplacer("/", "_", ".", "_", "-", "_").Replace(path) + ".go"
		if err := os.WriteFile(filepath.Join(opts.OutDir, name), src, 0o644); err != nil {
			return err
		}
		if opts.Log != nil {
			fmt.Fprintf(opts.Log, "%s: %d symbols\n", path, len(common[path]))
		}
	}
	return nil
}

func load(ctx context.Context, dir, goos string, paths []string) (map[string]*types.Package, error) {
	env := []string{"GOOS=" + goos, "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off"}
	if goos == "windows" || goos == "darwin" {
		env = append(env, "GOARCH=amd64")
	}
	args := append([]string{"list", "-e", "-export", "-deps", "-json=ImportPath,Export,Error"}, paths...)
	out, err := gocmd.Run(ctx, dir, env, args...)
	if err != nil {
		return nil, err
	}
	exports := map[string]string{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p struct {
			ImportPath, Export string
			Error              *struct{ Err string }
		}
		if err := dec.Decode(&p); err != nil {
			return nil, err
		}
		if p.Error != nil {
			return nil, fmt.Errorf("%s: %s", p.ImportPath, p.Error.Err)
		}
		exports[p.ImportPath] = p.Export
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(f)
	})
	pkgs := map[string]*types.Package{}
	for _, path := range paths {
		p, err := imp.Import(path)
		if err != nil {
			return nil, err
		}
		pkgs[path] = p
	}
	return pkgs, nil
}

func isGeneric(obj types.Object) bool {
	switch o := obj.(type) {
	case *types.Func:
		return o.Type().(*types.Signature).TypeParams().Len() > 0
	case *types.TypeName:
		if n, ok := o.Type().(*types.Named); ok && n.TypeParams().Len() > 0 && n.Obj() == o {
			return true
		}
		if a, ok := o.Type().(*types.Alias); ok && a.TypeParams().Len() > 0 {
			return true
		}
	}
	return false
}

func symbols(p *types.Package) map[symbol]bool {
	out := map[symbol]bool{}
	scope := p.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() || isGeneric(obj) {
			continue
		}
		switch o := obj.(type) {
		case *types.Func:
			out[symbol{name, "func"}] = true
		case *types.Var:
			out[symbol{name, "var"}] = true
		case *types.Const:
			out[symbol{name, "const"}] = true
		case *types.TypeName:
			if _, isTP := o.Type().(*types.TypeParam); isTP {
				continue
			}
			out[symbol{name, "type"}] = true
			if !o.IsAlias() && wrappable(o) {
				out[symbol{name, "iface"}] = true
			}
		}
	}
	return out
}

// wrappable reports whether an interface can be implemented by a wrapper:
// it needs at least one method, all exported and spelled with nameable
// types.
func wrappable(o *types.TypeName) bool {
	it, ok := o.Type().Underlying().(*types.Interface)
	if !ok || it.NumMethods() == 0 || !it.IsMethodSet() {
		return false
	}
	for i := 0; i < it.NumMethods(); i++ {
		m := it.Method(i)
		if !m.Exported() || m.Name() == "XWyBox" || !nameable(m.Type()) {
			return false
		}
	}
	return true
}

func nameable(t types.Type) bool {
	ok := true
	var walk func(types.Type)
	seen := map[types.Type]bool{}
	walk = func(t types.Type) {
		if !ok || seen[t] {
			return
		}
		seen[t] = true
		switch t := t.(type) {
		case *types.Named:
			o := t.Obj()
			if o.Pkg() != nil && (!o.Exported() || strings.Contains(o.Pkg().Path(), "internal") || strings.HasPrefix(o.Pkg().Path(), "vendor/")) {
				ok = false
				return
			}
			if t.TypeArgs() != nil {
				for i := 0; i < t.TypeArgs().Len(); i++ {
					walk(t.TypeArgs().At(i))
				}
			}
		case *types.Alias:
			walk(types.Unalias(t))
		case *types.Pointer:
			walk(t.Elem())
		case *types.Slice:
			walk(t.Elem())
		case *types.Array:
			walk(t.Elem())
		case *types.Map:
			walk(t.Key())
			walk(t.Elem())
		case *types.Chan:
			walk(t.Elem())
		case *types.Signature:
			for i := 0; i < t.Params().Len(); i++ {
				walk(t.Params().At(i).Type())
			}
			for i := 0; i < t.Results().Len(); i++ {
				walk(t.Results().At(i).Type())
			}
		case *types.Struct:
			for i := 0; i < t.NumFields(); i++ {
				if !t.Field(i).Exported() {
					ok = false
				}
				walk(t.Field(i).Type())
			}
		case *types.Interface:
			for i := 0; i < t.NumMethods(); i++ {
				if !t.Method(i).Exported() {
					ok = false
				}
				walk(t.Method(i).Type())
			}
		case *types.TypeParam:
			ok = false
		}
	}
	walk(t)
	return ok
}

type importSet struct {
	byPath map[string]string
	used   map[string]bool
}

func (is *importSet) alias(p *types.Package) string {
	if a, ok := is.byPath[p.Path()]; ok {
		return a
	}
	base := p.Name()
	a := base
	for i := 2; is.used[a] || a == "reflect" || a == "native" || a == "constant"; i++ {
		a = base + strconv.Itoa(i)
	}
	is.byPath[p.Path()] = a
	is.used[a] = true
	return a
}

func generate(p *types.Package, syms map[symbol]bool, opts Options) ([]byte, error) {
	imps := &importSet{byPath: map[string]string{}, used: map[string]bool{}}
	self := imps.alias(p)
	qual := func(q *types.Package) string { return imps.alias(q) }

	var funcs, vars, typs, consts, wraps []string
	var wrapperDecls bytes.Buffer
	names := make([]symbol, 0, len(syms))
	for s := range syms {
		names = append(names, s)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i].name != names[j].name {
			return names[i].name < names[j].name
		}
		return names[i].kind < names[j].kind
	})
	wrapPrefix := "_" + strings.NewReplacer("/", "_", ".", "_", "-", "_").Replace(p.Path()) + "_"
	for _, s := range names {
		obj := p.Scope().Lookup(s.name)
		ref := self + "." + s.name
		switch s.kind {
		case "func":
			funcs = append(funcs, fmt.Sprintf("%q: reflect.ValueOf(%s),", s.name, ref))
		case "var":
			vars = append(vars, fmt.Sprintf("%q: reflect.ValueOf(&%s),", s.name, ref))
		case "type":
			typs = append(typs, fmt.Sprintf("%q: reflect.TypeFor[%s](),", s.name, ref))
		case "const":
			c, err := constExpr(obj.(*types.Const), ref)
			if err != nil {
				return nil, err
			}
			consts = append(consts, fmt.Sprintf("%q: %s,", s.name, c))
		case "iface":
			wname := wrapPrefix + s.name
			if err := writeWrapper(&wrapperDecls, wname, obj.Type().Underlying().(*types.Interface), qual); err != nil {
				return nil, err
			}
			wraps = append(wraps, fmt.Sprintf("%q: reflect.TypeFor[%s](),", s.name, wname))
		}
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by wy extract; DO NOT EDIT.\n\npackage %s\n\nimport (\n", opts.PkgName)
	paths := make([]string, 0, len(imps.byPath))
	for path := range imps.byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	needConstant := len(consts) > 0
	if needConstant {
		b.WriteString("\t\"go/constant\"\n")
	}
	b.WriteString("\t\"reflect\"\n\n")
	fmt.Fprintf(&b, "\t%q\n", opts.Native)
	empty := len(funcs)+len(vars)+len(typs)+len(consts)+len(wraps) == 0
	for _, path := range paths {
		alias := imps.byPath[path]
		if empty && path == p.Path() {
			alias = "_" // nothing to reference; keep the import for its side effects
		}
		fmt.Fprintf(&b, "\t%s %q\n", alias, path)
	}
	b.WriteString(")\n\nvar _ reflect.Type\n\n")
	if len(consts) > 0 {
		b.WriteString("var _ = constant.MakeBool\n\n")
	}
	fmt.Fprintf(&b, "func init() {\n\tnative.Register(&native.Package{\n\t\tPath: %q,\n\t\tName: %q,\n", p.Path(), p.Name())
	section := func(field, typ string, lines []string) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(&b, "\t\t%s: map[string]%s{\n", field, typ)
		for _, l := range lines {
			b.WriteString("\t\t\t" + l + "\n")
		}
		b.WriteString("\t\t},\n")
	}
	section("Funcs", "reflect.Value", funcs)
	section("Vars", "reflect.Value", vars)
	section("Types", "reflect.Type", typs)
	section("Consts", "native.Const", consts)
	section("Wrappers", "reflect.Type", wraps)
	b.WriteString("\t})\n}\n\n")
	b.Write(wrapperDecls.Bytes())
	src, err := format.Source(b.Bytes())
	if err != nil {
		return b.Bytes(), fmt.Errorf("formatting generated code: %w", err)
	}
	return src, nil
}

// constExpr writes a native.Const literal. Values that may differ between
// platforms (strings, booleans, integers that fit 64 bits) are taken from
// the referenced constant itself when the generated code is compiled.
func constExpr(c *types.Const, ref string) (string, error) {
	val := c.Val()
	typed := ""
	basic, isBasic := c.Type().(*types.Basic)
	untyped := isBasic && basic.Info()&types.IsUntyped != 0
	if untyped {
		k := map[types.BasicKind]string{
			types.UntypedBool: "bool", types.UntypedInt: "int", types.UntypedRune: "rune",
			types.UntypedFloat: "float", types.UntypedComplex: "complex", types.UntypedString: "string",
		}[basic.Kind()]
		typed = fmt.Sprintf("Untyped: %q", k)
	} else {
		typed = fmt.Sprintf("Type: reflect.TypeOf(%s)", ref)
	}
	var v string
	switch val.Kind() {
	case constant.Bool:
		v = fmt.Sprintf("constant.MakeBool(bool(%s))", ref)
	case constant.String:
		v = fmt.Sprintf("constant.MakeString(string(%s))", ref)
	case constant.Int:
		if _, ok := constant.Int64Val(val); ok {
			v = fmt.Sprintf("constant.MakeInt64(int64(%s))", ref)
		} else if _, ok := constant.Uint64Val(val); ok {
			v = fmt.Sprintf("constant.MakeUint64(uint64(%s))", ref)
		} else {
			v = fmt.Sprintf("constant.MakeFromLiteral(%q, native.TokenINT, 0)", val.ExactString())
		}
	case constant.Float:
		v = fmt.Sprintf("native.Float(%q)", val.ExactString())
	case constant.Complex:
		re, im := constant.Real(val), constant.Imag(val)
		v = fmt.Sprintf("constant.BinaryOp(native.Float(%q), native.TokenADD, constant.MakeImag(native.Float(%q)))", re.ExactString(), im.ExactString())
	default:
		return "", fmt.Errorf("constant %s: unknown kind", c.Name())
	}
	return fmt.Sprintf("{Value: %s, %s}", v, typed), nil
}

func writeWrapper(b *bytes.Buffer, name string, it *types.Interface, qual types.Qualifier) error {
	fmt.Fprintf(b, "type %s struct{ XWyBox native.Caller }\n\n", name)
	for i := 0; i < it.NumMethods(); i++ {
		m := it.Method(i)
		sig := m.Type().(*types.Signature)
		var params, args []string
		for j := 0; j < sig.Params().Len(); j++ {
			pt := sig.Params().At(j).Type()
			ts := types.TypeString(pt, qual)
			if sig.Variadic() && j == sig.Params().Len()-1 {
				ts = "..." + types.TypeString(pt.(*types.Slice).Elem(), qual)
			}
			params = append(params, fmt.Sprintf("a%d %s", j, ts))
			args = append(args, fmt.Sprintf("a%d", j))
		}
		var results, rets []string
		for j := 0; j < sig.Results().Len(); j++ {
			ts := types.TypeString(sig.Results().At(j).Type(), qual)
			results = append(results, ts)
			rets = append(rets, fmt.Sprintf("native.As[%s](r[%d])", ts, j))
		}
		res := strings.Join(results, ", ")
		if len(results) > 1 {
			res = "(" + res + ")"
		}
		call := "nil"
		if len(args) > 0 {
			call = "[]any{" + strings.Join(args, ", ") + "}"
		}
		fmt.Fprintf(b, "func (w %s) %s(%s) %s {\n", name, m.Name(), strings.Join(params, ", "), res)
		if len(results) == 0 {
			fmt.Fprintf(b, "\tw.XWyBox.Call(%q, %s)\n}\n\n", m.Name(), call)
		} else {
			fmt.Fprintf(b, "\tr := w.XWyBox.Call(%q, %s)\n\treturn %s\n}\n\n", m.Name(), call, strings.Join(rets, ", "))
		}
	}
	return nil
}
