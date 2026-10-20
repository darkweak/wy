package typeexpr

import (
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

const src = `package p

import (
	"io"
	"time"
	"unsafe"
)

type T struct{ A int }
type G[K comparable, V any] struct{ k K; v V }
type A = T
type GA[X any] = G[string, X]
type unexported int

var (
	basic   int
	untyped = 1.5
	ptr     *T
	slice   [][]byte
	array   [4]string
	mp      map[string]*T
	ch      chan int
	sendCh  chan<- int
	recvCh  <-chan int
	chCh    chan (<-chan int)
	chSend  chan chan<- int
	fn      func(int, ...string) error
	fnNone  func()
	fnMulti func(a, b int) (int, error)
	st      struct{ X int "json:\"x\""; io.Reader; y string }
	empty   interface{}
	anyV    any
	iface   interface{ io.Reader; Close() error }
	gen     G[int, []T]
	alias   A
	galias  GA[int]
	dur     time.Duration
	up      unsafe.Pointer
	hidden  unexported
)

func F[P any](p P) P { return p }

func Local() { type local int; var l local; _ = l }
`

// namer qualifies every named type by its package name and refuses
// unexported names from package p, the way the interpreter refuses
// types it cannot spell.
type namer struct{}

func (namer) TypeName(obj *types.TypeName) (string, error) {
	if obj.Pkg() == nil {
		return obj.Name(), nil
	}
	if !obj.Exported() {
		return "", &Unnameable{obj.Name()}
	}
	return obj.Pkg().Name() + "." + obj.Name(), nil
}

func (namer) Field(v *types.Var, tag string) (string, string, error) {
	return v.Name(), tag, nil
}

func (namer) Method(f *types.Func) (string, error) { return f.Name(), nil }

func check(t *testing.T) (*types.Package, *types.Info) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: importer.Default()}
	pkg, err := conf.Check("p", fset, []*ast.File{f}, info)
	if err != nil {
		t.Fatal(err)
	}
	return pkg, info
}

func TestString(t *testing.T) {
	pkg, _ := check(t)
	tests := map[string]string{
		"basic":   "int",
		"untyped": "float64",
		"ptr":     "*p.T",
		"slice":   "[][]byte",
		"array":   "[4]string",
		"mp":      "map[string]*p.T",
		"ch":      "chan int",
		"sendCh":  "chan<- int",
		"recvCh":  "<-chan int",
		"chCh":    "chan (<-chan int)",
		"chSend":  "chan chan<- int",
		"fn":      "func(int, ...string) error",
		"fnNone":  "func()",
		"fnMulti": "func(int, int) (int, error)",
		"st":      `struct{X int "json:\"x\""; io.Reader; y string}`,
		"empty":   "interface{}",
		"anyV":    "any",
		"iface":   "interface{io.Reader; Close() error}",
		"gen":     "p.G[int, []p.T]",
		"alias":   "p.A",
		"galias":  "p.GA[int]",
		"dur":     "time.Duration",
	}
	for name, want := range tests {
		obj := pkg.Scope().Lookup(name)
		if obj == nil {
			t.Fatalf("%s not declared", name)
		}
		got, err := String(obj.Type(), namer{})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}

func TestUntypedConstant(t *testing.T) {
	for kind, want := range map[types.BasicKind]string{
		types.UntypedInt:    "int",
		types.UntypedFloat:  "float64",
		types.UntypedRune:   "rune",
		types.UntypedString: "string",
		types.UntypedBool:   "bool",
	} {
		got, err := String(types.Typ[kind], namer{})
		if err != nil || got != want {
			t.Errorf("%v: got %q, %v; want %q", types.Typ[kind], got, err, want)
		}
	}
}

func TestUnnameable(t *testing.T) {
	pkg, info := check(t)
	var local types.Type
	for id, obj := range info.Defs {
		if id.Name == "l" {
			local = obj.Type()
		}
	}
	fsig := pkg.Scope().Lookup("F").Type().(*types.Signature)
	tests := map[string]struct {
		typ  types.Type
		what string
	}{
		"unsafe.Pointer": {pkg.Scope().Lookup("up").Type(), "unsafe.Pointer"},
		"unexported":     {pkg.Scope().Lookup("hidden").Type(), "unexported"},
		"local type":     {local, "local"},
		"type parameter": {fsig.Params().At(0).Type(), "type parameter P"},
		"tuple":          {fsig.Results(), "tuple"},
		"nested":         {types.NewSlice(types.NewPointer(pkg.Scope().Lookup("hidden").Type())), "unexported"},
	}
	for name, tc := range tests {
		got, err := String(tc.typ, namer{})
		var u *Unnameable
		if !errors.As(err, &u) {
			t.Errorf("%s: got %q, %v; want *Unnameable", name, got, err)
			continue
		}
		if !strings.Contains(u.What, tc.what) || !strings.HasPrefix(err.Error(), "cannot name type ") {
			t.Errorf("%s: error %q, want it to mention %q", name, err, tc.what)
		}
	}
}

// aliasRefuser refuses to name aliases, so String must fall back to the
// aliased type.
type aliasRefuser struct{ namer }

func (a aliasRefuser) TypeName(obj *types.TypeName) (string, error) {
	if obj.IsAlias() {
		return "", &Unnameable{obj.Name()}
	}
	return a.namer.TypeName(obj)
}

func TestAliasFallback(t *testing.T) {
	pkg, _ := check(t)
	for name, want := range map[string]string{
		"alias":  "p.T",
		"galias": "p.G[string, int]",
	} {
		got, err := String(pkg.Scope().Lookup(name).Type(), aliasRefuser{})
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, want)
		}
	}
}

// renamer rewrites field and method names and tags.
type renamer struct{ namer }

func (renamer) Field(v *types.Var, tag string) (string, string, error) {
	if !v.Exported() {
		return "", "", errors.New("unexported field " + v.Name())
	}
	return "F" + v.Name(), "", nil
}

func (renamer) Method(f *types.Func) (string, error) { return "M" + f.Name(), nil }

func TestNamerSpellings(t *testing.T) {
	st := types.NewStruct([]*types.Var{
		types.NewField(token.NoPos, nil, "X", types.Typ[types.Int], false),
	}, []string{`json:"x"`})
	got, err := String(st, renamer{})
	if err != nil || got != "struct{FX int}" {
		t.Errorf("struct: got %q, %v", got, err)
	}
	pkg, _ := check(t)
	if _, err := String(pkg.Scope().Lookup("st").Type(), renamer{}); err == nil || !strings.Contains(err.Error(), "unexported field y") {
		t.Errorf("struct with unexported field: err = %v", err)
	}
	got, err = String(pkg.Scope().Lookup("iface").Type(), renamer{})
	if err != nil || got != "interface{io.Reader; MClose() error}" {
		t.Errorf("interface: got %q, %v", got, err)
	}
}
