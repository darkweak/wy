package tree

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type evalStep struct {
	src, want, wantErr string
}

func runEval(t *testing.T, in *Interp, steps []evalStep) {
	t.Helper()
	for i, st := range steps {
		vals, err := in.Eval(context.Background(), st.src)
		if st.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), st.wantErr) {
				t.Fatalf("step %d %q: want error %q, got %v", i, st.src, st.wantErr, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("step %d %q: %v", i, st.src, err)
		}
		parts := make([]string, len(vals))
		for j, v := range vals {
			parts[j] = fmt.Sprint(v)
		}
		if got := strings.Join(parts, " "); got != st.want {
			t.Fatalf("step %d %q: got %q want %q", i, st.src, got, st.want)
		}
	}
}

func TestEvalSession(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: `1 + 2`, want: "3"},
		{src: `x := 10`},
		{src: `x * 2`, want: "20"},
		{src: `type point struct{ x, y int }`},
		{src: `func (p point) sum() int { return p.x + p.y }`},
		{src: `p := point{3, 4}; p.sum()`, want: "7"},
		{src: `p.x, p.y`, want: "3 4"},
		{src: `func (p point) String() string { return fmt.Sprintf("<%d,%d>", p.x, p.y) }`},
		{src: `p, point{5, 6}`, want: "{3 4} <5,6>"},
		{src: `n, err := strconv.Atoi("42"); n, err`, want: "42 <nil>"},
		{src: `x := "shadow"; x`, want: "shadow"},
		{src: `undefined + 1`, wantErr: "undefined: undefined"},
		{src: `x`, want: "shadow"},
		{src: `counter := 0; inc := func() int { counter++; return counter }`},
		{src: `inc(); inc()`, want: "2"},
		{src: `slices.Sorted(maps.Keys(map[string]int{"b": 1, "a": 2}))`, want: "[a b]"},
		{src: `var buf strings.Builder; buf.WriteString("hi"); buf.String()`, want: "hi"},
		{src: `unsafe.Sizeof(struct{ a byte; b int64 }{})`, want: "16"},
	})
}
