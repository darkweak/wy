//go:build ignore

package main

import (
	"cmp"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sort"
	"strings"
)

type Ordered interface {
	~int | ~int64 | ~float64 | ~string
}

type Tree[K Ordered, V any] struct {
	root *treeNode[K, V]
	size int
}

type treeNode[K Ordered, V any] struct {
	key         K
	val         V
	left, right *treeNode[K, V]
}

func (t *Tree[K, V]) Put(k K, v V) {
	p := &t.root
	for *p != nil {
		switch {
		case k < (*p).key:
			p = &(*p).left
		case k > (*p).key:
			p = &(*p).right
		default:
			(*p).val = v
			return
		}
	}
	*p = &treeNode[K, V]{key: k, val: v}
	t.size++
}

func (t *Tree[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		var walk func(n *treeNode[K, V]) bool
		walk = func(n *treeNode[K, V]) bool {
			if n == nil {
				return true
			}
			return walk(n.left) && yield(n.key, n.val) && walk(n.right)
		}
		walk(t.root)
	}
}

type Pair[A, B any] struct {
	First  A
	Second B
}

func Zip[A, B any](as []A, bs []B) []Pair[A, B] {
	n := min(len(as), len(bs))
	out := make([]Pair[A, B], n)
	for i := range n {
		out[i] = Pair[A, B]{as[i], bs[i]}
	}
	return out
}

func Reduce[T, A any](xs []T, init A, f func(A, T) A) A {
	acc := init
	for _, x := range xs {
		acc = f(acc, x)
	}
	return acc
}

type Stringer interface{ String() string }

type ID int

func (i ID) String() string { return fmt.Sprintf("#%d", int(i)) }

func JoinAll[T Stringer](xs []T) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = x.String()
	}
	return strings.Join(parts, ",")
}

type Number interface{ ~int | ~float64 }

func MaxOf[T Number](xs ...T) (T, error) {
	if len(xs) == 0 {
		var zero T
		return zero, errors.New("empty")
	}
	return slices.Max(xs), nil
}

type List[T any] []T

func (l List[T]) Map(f func(T) T) List[T] {
	out := make(List[T], len(l))
	for i, v := range l {
		out[i] = f(v)
	}
	return out
}

func Keys[M ~map[K]V, K comparable, V any](m M) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

type Set[T comparable] map[T]struct{}

func (s Set[T]) Add(v T)      { s[v] = struct{}{} }
func (s Set[T]) Has(v T) bool { _, ok := s[v]; return ok }

func main() {
	t := &Tree[string, int]{}
	for i, w := range strings.Fields("pear apple fig apple banana") {
		t.Put(w, i)
	}
	for k, v := range t.All() {
		fmt.Print(k, "=", v, " ")
	}
	fmt.Println(t.size)
	fmt.Println(Zip([]int{1, 2, 3}, []string{"a", "b"}))
	fmt.Println(Reduce([]int{1, 2, 3, 4}, "", func(a string, x int) string { return a + fmt.Sprint(x) }))
	fmt.Println(JoinAll([]ID{1, 2, 3}))
	m, err := MaxOf(3.5, 1.5)
	_, err2 := MaxOf[int]()
	fmt.Println(m, err, err2)
	fmt.Println(List[int]{1, 2, 3}.Map(func(i int) int { return i * i }))
	ks := Keys(map[string]bool{"z": true, "a": true})
	sort.Strings(ks)
	fmt.Println(ks)
	s := Set[ID]{}
	s.Add(5)
	fmt.Println(s.Has(5), s.Has(6), len(s))
	people := []Pair[string, int]{{"bo", 30}, {"al", 25}, {"cy", 30}}
	slices.SortStableFunc(people, func(a, b Pair[string, int]) int {
		return cmp.Or(cmp.Compare(b.Second, a.Second), strings.Compare(a.First, b.First))
	})
	fmt.Println(people)
	idx := slices.IndexFunc(people, func(p Pair[string, int]) bool { return p.First == "al" })
	fmt.Println(idx, len(slices.Compact([]int{1, 1, 2, 2, 3})))
	next, stop := iter.Pull(slices.Values([]string{"x", "y"}))
	a, ok1 := next()
	b, ok2 := next()
	_, ok3 := next()
	stop()
	fmt.Println(a, ok1, b, ok2, ok3)
}
