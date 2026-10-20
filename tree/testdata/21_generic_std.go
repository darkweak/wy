//go:build ignore

package main

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
)

type config struct{ name string }

func main() {
	calls := 0
	get := sync.OnceValue(func() int { calls++; return 42 })
	fmt.Println(get(), get(), calls)
	pair := sync.OnceValues(func() (string, error) { return "ok", nil })
	s, err := pair()
	fmt.Println(s, err)
	var p atomic.Pointer[config]
	fmt.Println(p.Load() == nil)
	p.Store(&config{"a"})
	old := p.Swap(&config{"b"})
	fmt.Println(old.name, p.Load().name, p.CompareAndSwap(old, nil), p.Load().name)
	fmt.Println(reflect.TypeFor[int](), reflect.TypeFor[error]())
}
