//go:build ignore

package main

import (
	"fmt"
	"strings"
)

type lexer struct {
	input string
	pos   int
	out   []string
}

type stateFn func(*lexer) stateFn

func lexWord(l *lexer) stateFn {
	start := l.pos
	for l.pos < len(l.input) && l.input[l.pos] != ' ' {
		l.pos++
	}
	l.out = append(l.out, strings.ToUpper(l.input[start:l.pos]))
	return lexSpace
}

func lexSpace(l *lexer) stateFn {
	if l.pos >= len(l.input) {
		return nil
	}
	l.pos++
	return lexWord
}

type T []T
type P *P
type C chan C
type M map[string]M

func main() {
	l := &lexer{input: "go is fun"}
	for state := stateFn(lexWord); state != nil; {
		state = state(l)
	}
	fmt.Println(l.out)

	t := T{T{}, T{T{}, T{}}}
	fmt.Println(len(t), len(t[1]), len(t[1][0]))
	t = append(t, T{nil, nil, nil})
	fmt.Println(len(t[2]))

	var p P
	q := P(&p)
	fmt.Println(*q == nil, q != nil)

	ch := make(C, 1)
	ch <- ch
	got := <-ch
	fmt.Println(got == ch)

	m := M{"a": M{"b": nil}}
	fmt.Println(len(m["a"]), m["a"]["b"] == nil)

}
