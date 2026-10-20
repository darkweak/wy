package interp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func newSession(t *testing.T) *Session {
	t.Helper()
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func show(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, " ")
}

type step struct {
	src     string
	want    string // expected echo (fmt.Sprint of each value, space separated)
	wantErr string // substring of the expected error
}

func runSteps(t *testing.T, s *Session, steps []step) {
	t.Helper()
	ctx := context.Background()
	for i, st := range steps {
		start := time.Now()
		vals, err := s.Eval(ctx, st.src)
		t.Logf("step %d (%v): %q", i, time.Since(start).Round(time.Millisecond), st.src)
		if st.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), st.wantErr) {
				t.Fatalf("step %d: want error containing %q, got %v", i, st.wantErr, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("step %d: %q: unexpected error: %v", i, st.src, err)
		}
		if got := show(vals); got != st.want {
			t.Fatalf("step %d: %q: got %q, want %q", i, st.src, got, st.want)
		}
	}
}

// TestLanguage exercises features that tree-walking interpreters typically
// get wrong or do not support, all within one session.
func TestLanguage(t *testing.T) {
	s := newSession(t)
	runSteps(t, s, []step{
		// Constants are evaluated at compile time, with arbitrary precision.
		{src: `1 << 70`, want: "1180591620717411303424"},
		{src: `2 * time.Second`, want: "2s"},

		// Types with unexported fields and methods, used across inputs.
		{src: `type point struct{ x, y int }
func (p point) dist() float64 { return math.Hypot(float64(p.x), float64(p.y)) }
func (p *point) scale(k int) { p.x *= k; p.y *= k }`},
		{src: `p := point{3, 4}; p.dist()`, want: "5"},
		{src: `p.scale(2); p.x, p.y`, want: "6 8"},
		{src: `q := &p; q.scale(10); p`, want: "{60 80}"},

		// Generics: types, functions, constraints, methods on generic types.
		{src: `type number interface{ ~int | ~int64 | ~float64 }
func sum[T number](xs ...T) (t T) { for _, x := range xs { t += x }; return }
type stack[T any] struct{ items []T }
func (s *stack[T]) push(v T) { s.items = append(s.items, v) }
func (s *stack[T]) pop() (T, bool) {
	var zero T
	if len(s.items) == 0 { return zero, false }
	v := s.items[len(s.items)-1]; s.items = s.items[:len(s.items)-1]; return v, true
}`},
		{src: `sum(1.5, 2.5, 3)`, want: "7"},
		{src: `st := &stack[string]{}; st.push("a"); st.push("b"); st.pop()`, want: "b true"},
		{src: `st.pop()`, want: "a true"},
		{src: `type celsius float64; sum[celsius](10, 20.5)`, want: "30.5"},

		// Interfaces with unexported methods, satisfied across inputs.
		{src: `type shape interface{ area() float64 }
type sq struct{ s float64 }
func (q sq) area() float64 { return q.s * q.s }
type circle struct{ r float64 }
func (c circle) area() float64 { return math.Pi * c.r * c.r }`},
		{src: `shapes := []shape{sq{2}, circle{1}, sq{3}}
total := 0.0
for _, sh := range shapes { total += sh.area() }
math.Round(total*100)/100`, want: "16.14"},
		{src: `var _ shape = sq{}; func describe(s shape) string {
	switch v := s.(type) {
	case sq: return fmt.Sprintf("square %v", v.s)
	case circle: return fmt.Sprintf("circle %v", v.r)
	}
	return "?"
}`},
		{src: `describe(shapes[1])`, want: "circle 1"},

		// Struct embedding and promoted fields/methods across inputs.
		{src: `type named struct{ name string }
func (n named) hello() string { return "hi " + n.name }
type user struct { named; age int }`},
		{src: `u := user{named{"ana"}, 30}; u.hello() + " " + u.name`, want: "hi ana ana"},
		{src: `u.named.name = "bo"; u.hello()`, want: "hi bo"},

		// Closures capturing persistent variables; method values.
		{src: `counter := 0; inc := func() int { counter++; return counter }`},
		{src: `inc(); inc(); inc()`, want: "3"},
		{src: `counter`, want: "3"},
		{src: `f := p.dist; f()`, want: "100"},

		// Goroutines, channels, select, sync, all real.
		{src: `results := make(chan int, 10)
var wg sync.WaitGroup
for i := range 10 { wg.Add(1); go func() { defer wg.Done(); results <- i * i }() }
wg.Wait(); close(results)
total2 := 0
for r := range results { total2 += r }
total2`, want: "285"},
		{src: `select { case <-time.After(time.Millisecond): "x" }`, wantErr: "is not used"},
		{src: `done := make(chan struct{})
go func() { time.Sleep(10 * time.Millisecond); close(done) }()`},
		{src: `func() string { select { case <-done: return "done"; case <-time.After(time.Second): return "timeout" } }()`, want: "done"},

		// Multiple results, error handling, shadowing with :=.
		{src: `n, err := strconv.Atoi("42"); n, err`, want: "42 <nil>"},
		{src: `_, err = strconv.Atoi("x"); err != nil`, want: "true"},
		{src: `n := n + 1; n`, want: "43"},
		{src: `n := "now a string"; n`, want: "now a string"},

		// Redefining a function: later inputs use the new one.
		{src: `func greet() string { return "v1" }`},
		{src: `old := greet`},
		{src: `func greet() string { return "v2" }`},
		{src: `greet() + " " + old()`, want: "v2 v1"},

		// Functions and variables declared together in one input.
		{src: `var hits int
func hit() { hits++ }
hit(); hit(); hits`, want: "2"},
		{src: `hit(); hits`, want: "3"},

		// Anonymous structs, maps of session types, sort, slices, maps.
		{src: `cfg := struct{ host string; port int }{"localhost", 8080}; cfg.port`, want: "8080"},
		{src: `byName := map[string]point{"a": {1, 2}}; byName["a"].y`, want: "2"},
		{src: `xs := []int{5, 2, 9, 1}; slices.Sort(xs); xs`, want: "[1 2 5 9]"},
		{src: `keys := slices.Sorted(maps.Keys(map[string]int{"b": 1, "a": 2})); keys`, want: "[a b]"},

		// Iterators (range over func).
		{src: `func countdown(n int) iter.Seq[int] { return func(yield func(int) bool) { for i := n; i > 0; i-- { if !yield(i) { return } } } }`},
		{src: `var got []int; for v := range countdown(3) { got = append(got, v) }; got`, want: "[3 2 1]"},

		// Reflection and encoding keep Go semantics for unexported fields.
		{src: `type rec struct { Name string; secret int }`},
		{src: `b, _ := json.Marshal(rec{"x", 1}); string(b)`, want: `{"Name":"x"}`},
		{src: `reflect.TypeOf(rec{}).NumField()`, want: "2"},

		// unsafe works.
		{src: `unsafe.Sizeof(struct{ a int32; b int64 }{})`, want: "16"},

		// Labels, goto, defer/recover inside functions.
		{src: `func safeDiv(a, b int) (q int, err error) {
	defer func() { if r := recover(); r != nil { err = fmt.Errorf("recovered: %v", r) } }()
	return a / b, nil
}`},
		{src: `safeDiv(1, 0)`, want: "0 recovered: runtime error: integer divide by zero"},
		// Interpreted types flow into compiled library code through
		// interfaces, with no wrappers to generate.
		{src: `type temp float64
func (t temp) String() string { return fmt.Sprintf("%.1f°C", float64(t)) }`},
		{src: `fmt.Sprint(temp(21.5), []temp{1, 2})`, want: "21.5°C [1.0°C 2.0°C]"},
		{src: `type byLen []string
func (b byLen) Len() int { return len(b) }
func (b byLen) Less(i, j int) bool { return len(b[i]) < len(b[j]) }
func (b byLen) Swap(i, j int) { b[i], b[j] = b[j], b[i] }
ws := byLen{"ccc", "a", "bb"}; sort.Sort(ws); ws`, want: "[a bb ccc]"},
		{src: `rec := httptest.NewRecorder()
h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "hello %s", r.URL.Query().Get("n")) })
h.ServeHTTP(rec, httptest.NewRequest("GET", "/?n=go", nil)); rec.Body.String()`, want: "hello go"},
		{src: `type notFound struct{ key string }
func (e *notFound) Error() string { return e.key + " not found" }
func lookup(k string) error { return fmt.Errorf("lookup: %w", &notFound{k}) }`},
		{src: `var nf *notFound; err2 := lookup("k"); errors.As(err2, &nf), nf.key, err2`, want: "true k lookup: k not found"},
		{src: `outer: for i := 0; ; i++ { for j := 0; j < 3; j++ { if i*j == 4 { fmt.Sprint(i, j); break outer } } }; "ok"`, want: "ok"},
	})
}

func TestErrorsAndRecovery(t *testing.T) {
	s := newSession(t)
	runSteps(t, s, []step{
		{src: `x := 1`},
		{src: `y := undefinedThing + 1`, wantErr: "cell2:1:6: undefined: undefinedThing"},
		// A failed input leaves no trace: y is not defined, x is intact.
		{src: `y`, wantErr: "undefined: y"},
		{src: `x + 1`, want: "2"},
		{src: `x = "str"`, wantErr: "cannot use"},
		// Panics are reported and the session continues.
		{src: `var m map[string]int; m["a"] = 1`, wantErr: "assignment to entry in nil map"},
		{src: `x`, want: "1"},
		// Methods can be added to a type in a later input: the type is
		// redeclared with all of its methods.
		{src: `type t struct{ v int }`},
		{src: `func (x t) m() int { return x.v }`},
		{src: `func (x t) double() int { return 2 * x.m() }`},
		{src: `t{7}.double()`, want: "14"},
		{src: `func (x t) m() int { return x.v + 1 }`},
		{src: `t{7}.double()`, want: "16"},
	})

	var pe *PanicError
	_, err := s.Eval(context.Background(), `panic("boom")`)
	if !errors.As(err, &pe) || pe.Value != "boom" {
		t.Fatalf("want PanicError boom, got %v", err)
	}
}

func TestHostInterop(t *testing.T) {
	s := newSession(t)
	ctx := context.Background()

	// Host values enter with their real static types.
	if err := s.Set(ctx, "limit", 3); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "names", []string{"x", "y", "z", "w"}); err != nil {
		t.Fatal(err)
	}
	host := func(s string) string { return "<" + s + ">" }
	if err := s.Set(ctx, "wrap", host); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "start", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	runSteps(t, s, []step{
		{src: `wrap(strings.Join(names[:limit], ","))`, want: "<x,y,z>"},
		{src: `start.Add(48 * time.Hour).Format(time.DateOnly)`, want: "2026-01-04"},
	})

	// Interpreted functions come back as native Go funcs.
	if _, err := s.Eval(ctx, `func fib(n int) int { if n < 2 { return n }; return fib(n-1) + fib(n-2) }`); err != nil {
		t.Fatal(err)
	}
	v, err := s.Get(ctx, "fib")
	if err != nil {
		t.Fatal(err)
	}
	fib := v.(func(int) int) // a real func value: no reflection wrapper
	if got := fib(30); got != 832040 {
		t.Fatalf("fib(30) = %d", got)
	}

	// Shared memory: a pointer handed to the session is mutated in place.
	counter := new(int)
	if err := s.Set(ctx, "hostCounter", counter); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Eval(ctx, `for range 5 { *hostCounter++ }`); err != nil {
		t.Fatal(err)
	}
	if *counter != 5 {
		t.Fatalf("host counter = %d, want 5", *counter)
	}
}

func TestContextCancel(t *testing.T) {
	s := newSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Warm up so the timeout measures execution, not compilation.
	if _, err := s.Eval(ctx, `stop := make(chan struct{})`); err != nil {
		t.Fatal(err)
	}
	short, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	_, err := s.Eval(short, `<-stop`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	// The session is still usable, and can release the blocked code.
	runSteps(t, s, []step{{src: `close(stop); "released"`, want: "released"}})
}
