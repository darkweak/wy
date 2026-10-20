//go:build ignore

package main

import (
	"errors"
	"fmt"
)

type point struct{ x, y int }
type celsius float64

func (c celsius) String() string { return fmt.Sprintf("%.1f°", float64(c)) }

type myErr struct{ msg string }

func (e *myErr) Error() string { return e.msg }

func main() {
	p := point{1, 2}
	var e error = &myErr{"boom"}
	fmt.Printf("%T %T %T %T %T\n", p, &p, celsius(3), e, []point{})
	fmt.Printf("%v|%+v|%s|%5.1f|%T|%d%%\n", p, p, celsius(2), 3.14159, 42, 7)
	fmt.Printf("%[2]T %[1]v %*d|\n", p, celsius(1), 4, 7)
	fmt.Println(fmt.Sprintf("%T", nil), fmt.Sprintf("%T %v", errors.New("x"), e))
	args := []any{p, celsius(5)}
	fmt.Printf("%T and %v\n", args...)
}
