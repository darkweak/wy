//go:build ignore

package main

import "fmt"

type Set[T comparable] = map[T]struct{}

type List[T any] struct {
	head *node[T]
}

type node[T any] struct {
	v    T
	next *node[T]
}

func (l *List[T]) Push(v T) { l.head = &node[T]{v, l.head} }

func (l *List[T]) All() func(func(T) bool) {
	return func(yield func(T) bool) {
		for n := l.head; n != nil; n = n.next {
			if !yield(n.v) {
				return
			}
		}
	}
}

func main() {
	s := Set[string]{"a": {}}
	fmt.Println(len(s))
	var l List[string]
	l.Push("x")
	l.Push("y")
	for v := range l.All() {
		fmt.Print(v)
	}
	fmt.Println()
}
