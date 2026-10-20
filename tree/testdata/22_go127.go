// Language and library additions of Go 1.25 through 1.27.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
)

// Go 1.26: a generic type may refer to itself in its own type parameter list.
type Adder[A Adder[A]] interface{ Add(A) A }

type N int

func (n N) Add(m N) N { return n + m }

func Sum[A Adder[A]](xs ...A) A {
	var z A
	for _, x := range xs {
		z = z.Add(x)
	}
	return z
}

type point struct{ X, Y int }

type myErr struct{ code int }

func (e myErr) Error() string { return fmt.Sprint("code ", e.code) }

func ptrTo[T any](v T) *T { return new(v) }

func main() {
	// Go 1.26: new(expr).
	p := new(42)
	*p++
	fmt.Println(*p)
	f := new(1.5)
	fmt.Printf("%T %v\n", f, *f)
	s := new(fmt.Sprint("a", "b"))
	fmt.Println(*s)
	pt := new(point{1, 2})
	pt.Y = 5
	fmt.Println(*pt)
	q := ptrTo("generic")
	fmt.Println(*q)
	var i8 *int8 = new(int8(-3))
	fmt.Println(*i8)

	fmt.Println(Sum[N](1, 2, 3))

	// Go 1.26: errors.AsType.
	err := fmt.Errorf("wrap: %w", &fs.PathError{Op: "open", Path: "x", Err: errors.ErrUnsupported})
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		fmt.Println("AsType", pe.Path)
	}
	_, ok := errors.AsType[myErr](err)
	fmt.Println("AsType myErr", ok)
	joined := errors.Join(errors.New("a"), fmt.Errorf("b: %w", myErr{7}))
	if me, ok := errors.AsType[myErr](joined); ok {
		fmt.Println("AsType joined", me.code)
	}
	_, ok = errors.AsType[myErr](nil)
	fmt.Println("AsType nil", ok)

	// Go 1.25: reflect.TypeAssert.
	v, ok := reflect.TypeAssert[int](reflect.ValueOf(7))
	fmt.Println("TypeAssert", v, ok)
	_, ok = reflect.TypeAssert[string](reflect.ValueOf(7))
	fmt.Println("TypeAssert string", ok)
	var e error = myErr{3}
	ev, ok := reflect.TypeAssert[error](reflect.ValueOf(&e).Elem())
	fmt.Println("TypeAssert error", ev, ok)
}
