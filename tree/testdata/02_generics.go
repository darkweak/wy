//go:build ignore

package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

type Number interface{ ~int | ~float64 }

type Vec[T Number] []T

func (v Vec[T]) Sum() (s T) {
	for _, x := range v {
		s += x
	}
	return
}

type Pair[K cmp.Ordered, V any] struct {
	Key K
	Val V
}

func SortedPairs[K cmp.Ordered, V any](m map[K]V) []Pair[K, V] {
	out := make([]Pair[K, V], 0, len(m))
	for k, v := range m {
		out = append(out, Pair[K, V]{k, v})
	}
	slices.SortFunc(out, func(a, b Pair[K, V]) int { return cmp.Compare(a.Key, b.Key) })
	return out
}

type Stringish interface{ ~string }

func Join[S Stringish](xs []S) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = string(x)
	}
	return strings.Join(parts, "+")
}

type color string

func main() {
	fmt.Println(Vec[float64]{1.5, 2.5}.Sum(), Vec[int]{1, 2, 3}.Sum())
	fmt.Println(SortedPairs(map[string]int{"b": 2, "a": 1}))
	fmt.Println(Join([]color{"red", "blue"}))
	f := Vec[int].Sum
	fmt.Println(f(Vec[int]{4, 5}))
}
