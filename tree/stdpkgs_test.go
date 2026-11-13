package tree

import "testing"

func TestSourceStdlibPackages(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: `import (
	"structs"
	"unique"
	"weak"
)`},
		{src: `type node struct{ v int }`},
		{src: `n := &node{4}; w := weak.Make(n); w.Value().v, w == weak.Make(n), weak.Make[node](nil).Value() == nil`, want: "4 true true"},
		{src: `func dropped() weak.Pointer[[1 << 16]byte] { return weak.Make(new([1 << 16]byte)) }`},
		{src: `d := dropped(); for i := 0; i < 10 && d.Value() != nil; i++ { runtime.GC() }; d.Value() == nil`, want: "true"},
		{src: `type myInt int`},
		{src: `unique.Make("a") == unique.Make("a"), unique.Make("a") == unique.Make("b"), unique.Make(node{1}).Value().v`, want: "true false 1"},
		{src: `any(unique.Make(1)) == any(unique.Make(myInt(1))), unique.Make(myInt(2)).Value()`, want: "false 2"},
		{src: `type layout struct{ _ structs.HostLayout; x int32 }`},
		{src: `unsafe.Sizeof(layout{})`, want: "4"},
		{src: `import "plugin"`},
		{src: `p, err := plugin.Open("x.so"); p == nil, err`, want: "true plugin: not implemented"},
	})
}
