//go:build ignore

package main

import (
	"fmt"
	"iter"
	"maps"
	"slices"
)

func Evens(n int) iter.Seq[int] {
	return func(yield func(int) bool) {
		for i := 0; i < n; i += 2 {
			if !yield(i) {
				return
			}
		}
	}
}

func main() {
	for v := range Evens(10) {
		if v > 6 {
			break
		}
		fmt.Print(v, " ")
	}
	fmt.Println()
	fmt.Println(slices.Sorted(maps.Keys(map[string]bool{"z": true, "a": true})))
	fmt.Println(min(3, 1, 2), max(2.5, 1))
	m := map[int]int{1: 1}
	clear(m)
	fmt.Println(len(m))
}
