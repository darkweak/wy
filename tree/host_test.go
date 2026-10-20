package tree

import (
	"context"
	"reflect"
	"testing"

	"wy/tree/native"
)

// A host package, as `wy extract` would register it.
type Greeter interface{ Greet(name string) string }

type greeterWrapper struct{ XWyBox native.Caller }

func (w greeterWrapper) Greet(a0 string) string {
	r := w.XWyBox.Call("Greet", []any{a0})
	return native.As[string](r[0])
}

func init() {
	native.Register(&native.Package{
		Path: "example.com/host/api", Name: "api",
		Types: map[string]reflect.Type{"Greeter": reflect.TypeFor[Greeter]()},
		Funcs: map[string]reflect.Value{
			"Welcome": reflect.ValueOf(func(g Greeter) string { return g.Greet("host") + "!" }),
		},
		Wrappers: map[string]reflect.Type{"Greeter": reflect.TypeFor[greeterWrapper]()},
	})
}

func TestHostPackage(t *testing.T) {
	in := New(Options{})
	ctx := context.Background()
	runEval(t, in, []evalStep{
		{src: `import "example.com/host/api"
type polite struct{ prefix string }
func (p polite) Greet(n string) string { return p.prefix + " " + n }`},
		{src: `api.Welcome(polite{"hello"})`, want: "hello host!"},
	})
	// The host gets an interpreted implementation as its own interface.
	vals, err := in.Eval(ctx, `var g api.Greeter = polite{"hi"}; g`)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := vals[0].(Greeter)
	if !ok {
		t.Fatalf("got %T, want Greeter", vals[0])
	}
	if got := g.Greet("gopher"); got != "hi gopher" {
		t.Fatalf("Greet = %q", got)
	}
	// Interpreted functions come back as native funcs.
	vals, err = in.Eval(ctx, `func(x int) int { return x * x }`)
	if err != nil {
		t.Fatal(err)
	}
	if sq := vals[0].(func(int) int); sq(7) != 49 {
		t.Fatal("square")
	}
}
