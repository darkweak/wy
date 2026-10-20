//go:build ignore

package main

import (
	"fmt"
	"reflect"
)

type Animal interface{ Sound() string }

type Base struct{ Name string }

func (b Base) Sound() string    { return b.Name + ": ..." }
func (b Base) String() string   { return "Base(" + b.Name + ")" }
func (b *Base) Rename(n string) { b.Name = n }

type Dog struct {
	Base
	Tricks int
}

func (d Dog) Sound() string { return d.Name + ": woof" }

func main() {
	d := &Dog{Base{"rex"}, 2}
	var a Animal = d
	d.Rename("max")
	fmt.Println(a.Sound(), d)
	t := reflect.TypeOf(d)
	fmt.Println(t, t.Elem().Name(), t.NumMethod(), t.Elem().NumField())
	m, _ := t.MethodByName("Rename")
	fmt.Println(m.Name, m.Type.NumIn())
	fmt.Printf("%v %+v\n", *d, *d)
}
