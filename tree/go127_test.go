package tree

import "testing"

// Language and library additions of Go 1.25 through 1.27.

func TestNewExpr(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		// new(T) keeps allocating a zero value.
		{src: `*new(int), *new(string) == "", *new((int))`, want: "0 true 0"},
		{src: `type point struct{ X, Y int }`},
		{src: `*new(point)`, want: "{0 0}"},
		{src: `p := new(any); *p == nil`, want: "true"},

		// new(expr) with untyped constants takes their default type.
		{src: `p := new(42); fmt.Sprintf("%T %v", p, *p)`, want: "*int 42"},
		{src: `p := new(1.5); fmt.Sprintf("%T %v", p, *p)`, want: "*float64 1.5"},
		{src: `var p *int32 = new('x'); *p`, want: "120"},
		{src: `p := new("s"); fmt.Sprintf("%T %v", p, *p)`, want: "*string s"},
		{src: `p := new(true); fmt.Sprintf("%T %v", p, *p)`, want: "*bool true"},
		{src: `p := new(2i); fmt.Sprintf("%T %v", p, *p)`, want: "*complex128 (0+2i)"},

		// Typed expressions.
		{src: `p := new(int8(-3)); fmt.Sprintf("%T %v", p, *p)`, want: "*int8 -3"},
		{src: `var p *int64 = new(int64(7)); *p`, want: "7"},
		{src: `p := new(point{1, 2}); p.Y = 5; *p`, want: "{1 5}"},
		{src: `p := new([]int{1, 2}); *p = append(*p, 3); *p`, want: "[1 2 3]"},
		{src: `p := new(map[string]int{"a": 1}); n := (*p)["a"]; n`, want: "1"},
		{src: `p := new(strings.Repeat("ab", 2)); *p`, want: "abab"},
		{src: `p := new(any(5)); fmt.Sprintf("%T %v", p, *p)`, want: "*interface {} 5"},
		{src: `p := new(error(nil)); *p == nil`, want: "true"},
		{src: `f := new(func() int { return 9 }); (*f)()`, want: "9"},

		// The variable is a copy, distinct on each call.
		{src: `x := 10; p := new(x); *p = 20; x, *p`, want: "10 20"},
		{src: `a, b := new(1), new(1); a == b, *a == *b`, want: "false true"},
		{src: `ps := []*int{}; for i := range 3 { ps = append(ps, new(i)) }; *ps[0], *ps[1], *ps[2]`, want: "0 1 2"},

		// The operand is evaluated exactly once.
		{src: `n := 0; next := func() int { n++; return n }`},
		{src: `p := new(next()); *p, n`, want: "1 1"},

		// Inside generic code, both forms.
		{src: `func ptrTo[T any](v T) *T { return new(v) }`},
		{src: `func zero[T any]() *T { return new(T) }`},
		{src: `*ptrTo("g"), *ptrTo(3.5), *ptrTo(point{1, 1})`, want: "g 3.5 {1 1}"},
		{src: `*zero[int](), *zero[string]() == ""`, want: "0 true"},
	})
}

func TestSelfReferentialConstraint(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: `type Adder[A Adder[A]] interface{ Add(A) A }`},
		{src: `type N int`},
		{src: `func (n N) Add(m N) N { return n + m }`},
		{src: `func Sum[A Adder[A]](xs ...A) A { var z A; for _, x := range xs { z = z.Add(x) }; return z }`},
		{src: `Sum[N](1, 2, 3)`, want: "6"},
		{src: `Sum(N(4), N(5))`, want: "9"},
	})
}

func TestErrorsAsType(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: `type codeErr struct{ code int }`},
		{src: `func (e codeErr) Error() string { return fmt.Sprint("code ", e.code) }`},
		{src: `type ptrErr struct{ msg string }`},
		{src: `func (e *ptrErr) Error() string { return e.msg }`},
		// asErr matches codeErr through an As method.
		{src: `type asErr struct{}`},
		{src: `func (asErr) Error() string { return "as" }`},
		{src: `func (asErr) As(target any) bool { if p, ok := target.(*codeErr); ok { *p = codeErr{99}; return true }; return false }`},

		{src: `e, ok := errors.AsType[codeErr](codeErr{1}); e.code, ok`, want: "1 true"},
		{src: `e, ok := errors.AsType[codeErr](fmt.Errorf("a: %w", fmt.Errorf("b: %w", codeErr{2}))); e.code, ok`, want: "2 true"},
		{src: `e, ok := errors.AsType[*ptrErr](fmt.Errorf("w: %w", &ptrErr{"p"})); e.msg, ok`, want: "p true"},
		{src: `e, ok := errors.AsType[*fs.PathError](fmt.Errorf("w: %w", &fs.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist})); e.Path, ok`, want: "x true"},

		// Multi-error trees are searched depth first.
		{src: `e, ok := errors.AsType[codeErr](errors.Join(errors.New("x"), nil, fmt.Errorf("y: %w", codeErr{3}), codeErr{4})); e.code, ok`, want: "3 true"},

		// The As method is honoured.
		{src: `e, ok := errors.AsType[codeErr](fmt.Errorf("w: %w", asErr{})); e.code, ok`, want: "99 true"},

		// Interface targets.
		{src: `e, ok := errors.AsType[interface{ error; Timeout() bool }](codeErr{1}); e == nil, ok`, want: "true false"},
		{src: `e, ok := errors.AsType[error](codeErr{5}); e, ok`, want: "code 5 true"},

		// No match: zero value and false.
		{src: `e, ok := errors.AsType[codeErr](errors.New("plain")); e.code, ok`, want: "0 false"},
		{src: `e, ok := errors.AsType[*ptrErr](nil); e == nil, ok`, want: "true false"},
		{src: `_, ok := errors.AsType[*codeErr](codeErr{1}); ok`, want: "false"},
		{src: `_, ok := errors.AsType[ptrErr](&ptrErr{"x"}); ok`, wantErr: "does not satisfy error"},
	})
}

func TestReflectTypeAssert(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: `v, ok := reflect.TypeAssert[int](reflect.ValueOf(7)); v, ok`, want: "7 true"},
		{src: `v, ok := reflect.TypeAssert[string](reflect.ValueOf(7)); v == "", ok`, want: "true false"},
		{src: `v, ok := reflect.TypeAssert[fmt.Stringer](reflect.ValueOf(time.Second)); v, ok`, want: "1s true"},

		// Values held in interfaces are unwrapped.
		{src: `var x any = 3; v, ok := reflect.TypeAssert[int](reflect.ValueOf(&x).Elem()); v, ok`, want: "3 true"},
		{src: `var e error = errors.New("boom"); v, ok := reflect.TypeAssert[error](reflect.ValueOf(&e).Elem()); v, ok`, want: "boom true"},
		{src: `var e error; v, ok := reflect.TypeAssert[error](reflect.ValueOf(&e).Elem()); v == nil, ok`, want: "true false"},

		// Exported struct fields are accessible.
		{src: `type box struct{ N int; hidden int }`},
		{src: `v, ok := reflect.TypeAssert[int](reflect.ValueOf(box{N: 4}).Field(0)); v, ok`, want: "4 true"},

		// Invalid values panic like the real implementation.
		{src: `func catch(f func()) (r any) { defer func() { r = recover() }(); f(); return }`},
		{src: `fmt.Sprint(catch(func() { reflect.TypeAssert[int](reflect.Value{}) }))`, want: "reflect: call of reflect.TypeAssert on zero Value"},
	})
}

func TestNewStdlibPackages(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		// TagOID was mangled in the previous symbol tables.
		{src: `asn1.TagOID`, want: "6"},

		{src: `u := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8"); u.String()`, want: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"},
		{src: `uuid.Nil() == uuid.UUID{}, uuid.New() != uuid.New()`, want: "true true"},
		{src: `_, err := uuid.Parse("nope"); err != nil`, want: "true"},
		{src: `a, b := uuid.NewV7(), uuid.NewV7(); a.Compare(b) <= 0`, want: "true"},

		{src: `import jsonv2 "encoding/json/v2"`},
		{src: `out, err := jsonv2.Marshal(map[string]int{"b": 2, "a": 1}, jsonv2.Deterministic(true)); string(out), err`, want: `{"a":1,"b":2} <nil>`},
		{src: `var v struct{ A []int }; err := jsonv2.Unmarshal([]byte(` + "`" + `{"A":[1,2]}` + "`" + `), &v); v.A, err`, want: "[1 2] <nil>"},
		{src: `v := jsontext.Value(` + "`" + `{ "a" : 1 }` + "`" + `); err := v.Compact(); string(v), err`, want: `{"a":1} <nil>`},
	})
}

func TestRefreshedGenericStdlib(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: `slices.Sorted(slices.Values([]int{3, 1, 2}))`, want: "[1 2 3]"},
		{src: `slices.Collect(slices.Chunk([]int{1, 2, 3, 4, 5}, 2))`, want: "[[1 2] [3 4] [5]]"},
		{src: `slices.Insert([]int{1, 4}, 1, 2, 3)`, want: "[1 2 3 4]"},
		{src: `slices.Delete([]int{1, 2, 3, 4}, 1, 3)`, want: "[1 4]"},
		{src: `slices.AppendSeq([]int(nil), slices.Values([]int{})) == nil`, want: "true"},
		{src: `slices.SortedFunc(maps.Keys(map[string]int{"b": 1, "a": 2, "c": 3}), func(x, y string) int { return cmp.Compare(y, x) })`, want: "[c b a]"},
		{src: `m := maps.Collect(maps.All(map[int]bool{1: true})); v := m[1]; len(m), v`, want: "1 true"},
		{src: `cmp.Or("", "x", "y")`, want: "x"},
	})
}

// unsafe.Sizeof, Alignof and Offsetof are not constant when their operand's
// type depends on a type parameter (slices.Insert relies on it).
func TestUnsafeSizeofGeneric(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: `func size[T any]() uintptr { var v T; return unsafe.Sizeof(v) }`},
		{src: `func align[T any](v T) uintptr { return unsafe.Alignof(v) }`},
		{src: `type pair[T any] struct{ A byte; B T }`},
		{src: `func offB[T any]() uintptr { var p pair[T]; return unsafe.Offsetof(p.B) }`},
		{src: `type inner struct{ X byte; Y int64 }`},
		{src: `type outer[T any] struct{ A T; inner }`},
		{src: `func offY[T any]() uintptr { var o outer[T]; return unsafe.Offsetof(o.Y) }`},
		{src: `size[int8](), size[int64](), size[[3]int32](), size[struct{}]()`, want: "1 8 12 0"},
		{src: `align(int16(0)), align(int64(0)), align(struct{ a byte }{})`, want: "2 8 1"},
		{src: `offB[byte](), offB[int32](), offB[int64]()`, want: "1 4 8"},
		{src: `offY[byte](), offY[[16]byte]()`, want: "16 24"},
		{src: `fmt.Sprintf("%T", size[int]())`, want: "uintptr"},
		// The operand is not evaluated.
		{src: `func noEval[T any](p *T) uintptr { return unsafe.Sizeof(*p) }`},
		{src: `noEval[int64](nil)`, want: "8"},

		{src: `slices.Insert([]int{1, 4}, 1, 2, 3)`, want: "[1 2 3 4]"},
		{src: `s := []int{1, 2, 3, 4, 5}; slices.Insert(s[:2], 1, s[3:]...)`, want: "[1 4 5 2]"},
		{src: `slices.Replace([]string{"a", "b", "c"}, 1, 2, "x", "y")`, want: "[a x y c]"},
	})
}
