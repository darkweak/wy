//go:build ignore

package main

import (
	"fmt"
	"sort"
	"strings"
)

type Shape interface {
	Area() float64
	Perimeter() float64
}

type Named interface {
	Shape
	Name() string
}

type Rect struct{ W, H float64 }
type Square struct{ Rect }
type Circle struct{ R float64 }

func (r Rect) Area() float64      { return r.W * r.H }
func (r Rect) Perimeter() float64 { return 2 * (r.W + r.H) }
func (r Rect) Name() string       { return "rect" }
func (s Square) Name() string     { return "square" }
func (c *Circle) Area() float64   { return 3 * c.R * c.R }
func (c *Circle) Perimeter() float64 {
	return 6 * c.R
}
func (c *Circle) Name() string { return "circle" }

type Celsius float64
type Temps []Celsius

func (t Temps) Len() int           { return len(t) }
func (t Temps) Less(i, j int) bool { return t[i] < t[j] }
func (t Temps) Swap(i, j int)      { t[i], t[j] = t[j], t[i] }

func (c Celsius) String() string { return fmt.Sprintf("%.1f°C", float64(c)) }

type Stack[T any] struct{ items []T }

func (s *Stack[T]) Push(v T) { s.items = append(s.items, v) }
func (s *Stack[T]) Pop() T {
	v := s.items[len(s.items)-1]
	s.items = s.items[:len(s.items)-1]
	return v
}
func (s Stack[T]) Len() int { return len(s.items) }
func Map[T, U any](xs []T, f func(T) U) []U {
	out := make([]U, 0, len(xs))
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}

type Number interface{ ~int | ~float64 }

func Sum[T Number](xs ...T) T {
	var s T
	for _, x := range xs {
		s += x
	}
	return s
}

func describe(v any) string {
	switch x := v.(type) {
	case nil:
		return "nil"
	case int, int64:
		return fmt.Sprintf("integer %v", x)
	case string:
		return "string " + x
	case fmt.Stringer:
		return "stringer " + x.String()
	case Shape:
		return fmt.Sprintf("shape %.1f", x.Area())
	case []int:
		return fmt.Sprint("ints ", len(x))
	default:
		return fmt.Sprintf("other %T", x)
	}
}

func main() {
	shapes := []Named{Rect{2, 3}, Square{Rect{2, 2}}, &Circle{1}}
	for _, s := range shapes {
		fmt.Printf("%s %.1f %.1f\n", s.Name(), s.Area(), s.Perimeter())
	}
	var sh Shape = Square{Rect{3, 3}}
	if n, ok := sh.(Named); ok {
		fmt.Println("named:", n.Name())
	}
	_, isCircle := sh.(*Circle)
	fmt.Println("is circle:", isCircle)

	ts := Temps{30, 10, 20}
	sort.Sort(ts)
	fmt.Println(ts, ts[0])
	fmt.Println(fmt.Sprintf("%v|%s|%d", Celsius(1.5), Celsius(2), 3))

	st := &Stack[string]{}
	st.Push("a")
	st.Push("b")
	fmt.Println(st.Len(), st.Pop(), st.Len())
	fmt.Println(Map([]int{1, 2, 3}, func(i int) string { return strings.Repeat("*", i) }))
	fmt.Println(Sum(1, 2, 3), Sum(1.5, 2.5), Sum[Celsius](1, 2))

	for _, v := range []any{nil, 1, int64(2), "s", Celsius(3), Rect{1, 1}, []int{1}, 2.5} {
		fmt.Println(describe(v))
	}
	area := Rect.Area
	per := (*Circle).Perimeter
	bound := Rect{4, 5}.Area
	fmt.Println(area(Rect{1, 2}), per(&Circle{2}), bound())
}
