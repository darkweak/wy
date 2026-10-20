//go:build ignore

package main

import (
	"errors"
	"fmt"
	"strings"
)

func counter() (func() int, func()) {
	n := 0
	return func() int { n++; return n }, func() { n = 100 }
}

func deferOrder() (s string) {
	defer func() { s += " c" }()
	defer func() { s += " b" }()
	s = "a"
	return s
}

func recoverIt() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered: %v", r)
		}
	}()
	var m map[string]int
	m["x"] = 1
	return nil
}

func nestedPanic() (msg string) {
	defer func() { msg = fmt.Sprint(recover()) }()
	defer func() { panic("second") }()
	panic("first")
}

func deferArgs() string {
	var b strings.Builder
	for i := 0; i < 3; i++ {
		defer fmt.Fprint(&b, i)
	}
	return "deferred"
}

func loopVars() []int {
	var fs []func() int
	for i := 0; i < 3; i++ {
		fs = append(fs, func() int { return i * 10 })
	}
	var out []int
	for _, f := range fs {
		out = append(out, f())
	}
	return out
}

func recoverNotDirect() (r any) {
	defer func() {
		helper := func() any { return recover() }
		r = helper() // not called directly by the deferred function: nil
		if r == nil {
			r = recover()
		}
	}()
	panic("direct")
}

type myErr struct{ code int }

func (e *myErr) Error() string { return fmt.Sprintf("code %d", e.code) }

func mayFail(fail bool) error {
	if fail {
		return &myErr{42}
	}
	var p *myErr
	_ = p
	return nil
}

func main() {
	inc, reset := counter()
	inc()
	inc()
	fmt.Println(inc())
	reset()
	fmt.Println(inc())
	fmt.Println(deferOrder())
	fmt.Println(recoverIt())
	fmt.Println(nestedPanic())
	fmt.Println(deferArgs())
	fmt.Println(loopVars())
	fmt.Println(recoverNotDirect())
	err := mayFail(true)
	var me *myErr
	fmt.Println(err, errors.As(err, &me), me.code, mayFail(false) == nil)
	func() {
		defer func() {
			r := recover()
			re, ok := r.(error)
			fmt.Println("runtime error:", ok, re)
		}()
		var arr []int
		_ = arr[5]
	}()
	func() {
		defer func() { fmt.Println("div:", recover()) }()
		a, b := 1, 0
		fmt.Println(a / b)
	}()
	func() {
		defer func() { fmt.Println("assert:", recover()) }()
		var x any = "str"
		_ = x.(int)
	}()
}
