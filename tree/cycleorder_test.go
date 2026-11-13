package tree

import "testing"

// Mutually recursive types get one representation, whichever of them is
// built first (otter's s3fifo small <-> ghost <-> main).
func TestMutualRecursionOrder(t *testing.T) {
	for _, generic := range []bool{false, true} {
		in := New(Options{})
		steps := []evalStep{
			{src: "type mainQ struct{ len int }\ntype small struct{ main *mainQ; ghost *ghost; cost int }\ntype ghost struct{ main *mainQ; small *small; m map[uint64]struct{} }\ntype policy struct{ small *small; main *mainQ; ghost *ghost }"},
			// ghost is built first, then small through policy.
			{src: `g := &ghost{m: map[uint64]struct{}{}}`},
			{src: `p := &policy{small: &small{cost: 3}}`},
			{src: `p.small.ghost = g; g.small = p.small; p.ghost = g; p.small.ghost.small.cost, len(p.ghost.m)`, want: "3 0"},
		}
		if generic {
			steps = []evalStep{
				{src: "type mainQ[K comparable, V any] struct{ len int }\ntype small[K comparable, V any] struct{ main *mainQ[K, V]; ghost *ghost[K, V]; cost int }\ntype ghost[K comparable, V any] struct{ main *mainQ[K, V]; small *small[K, V]; m map[uint64]struct{} }\ntype policy[K comparable, V any] struct{ small *small[K, V]; main *mainQ[K, V]; ghost *ghost[K, V] }"},
				{src: `func newGhost[K comparable, V any]() *ghost[K, V] { return &ghost[K, V]{m: map[uint64]struct{}{}} }`},
				{src: `func newPolicy[K comparable, V any]() *policy[K, V] { s := &small[K, V]{cost: 3}; p := &policy[K, V]{small: s}; g := newGhost[K, V](); s.ghost = g; g.small = s; p.ghost = g; return p }`},
				{src: `g := newGhost[string, []byte]()`},
				{src: `p := newPolicy[string, []byte](); p.small.ghost.small.cost, len(p.ghost.m), g != nil`, want: "3 0 true"},
			}
		}
		runEval(t, in, steps)
	}
}
