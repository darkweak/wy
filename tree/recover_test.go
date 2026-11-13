package tree

import "testing"

// Values that reflection makes from an interpreted type's reflect type
// recover the interpreted type in type assertions, as protobuf's
// reflect.New(GoReflectType).Interface().(protoreflect.ProtoMessage) needs.
func TestReflectMadeValues(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: `type msg struct{ N int "tag:\"n\"" }`},
		{src: `func (m *msg) Name() string { return fmt.Sprint("msg", m.N) }`},
		{src: `type namer interface{ Name() string }`},
		{src: `rt := reflect.TypeOf(&msg{}).Elem()`},
		{src: `v := reflect.New(rt); v.Elem().Field(0).SetInt(7); n, ok := v.Interface().(namer); n.Name(), ok`, want: "msg7 true"},
		{src: `p, ok := reflect.New(rt).Interface().(*msg); p.N, ok`, want: "0 true"},
		// Basic kinds are not recovered: a float64 stays a float64.
		{src: `type celsius float64`},
		{src: `func (c celsius) String() string { return "C" }`},
		{src: `var f any = 2.5; _, ok := f.(fmt.Stringer); ok`, want: "false"},
	})
}

// Named struct types with the same fields have distinct reflect types, and
// still convert and assign like Go types.
func TestDistinctNamedStructs(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: `type A struct{ X int "json:\"x\"" }`},
		{src: `type B struct{ X int "json:\"x\"" }`},
		{src: `reflect.TypeOf(A{}) != reflect.TypeOf(B{}), reflect.TypeOf(A{}).Field(0).Tag.Get("json")`, want: "true x"},
		{src: `a := A{1}; b := B(a); var u struct{ X int "json:\"x\"" } = a; var c A = u; b.X, u.X, c == a`, want: "1 1 true"},
		{src: `j, err := json.Marshal([]any{a, b, u}); string(j), err`, want: `[{"x":1},{"x":1},{"x":1}] <nil>`},
		{src: `m := map[any]int{a: 1, b: 2, u: 3}; x, y := m[A{1}], m[B{1}]; len(m), x, y`, want: "3 1 2"},
		{src: `var ia, ib any = a, b; ia == ib, ia == any(A{1})`, want: "false true"},
		{src: `pa, ok := reflect.New(reflect.TypeOf(A{})).Interface().(*A); pa.X, ok`, want: "0 true"},
		{src: `_, ok := reflect.New(reflect.TypeOf(A{})).Interface().(*B); ok`, want: "false"},
	})
}
