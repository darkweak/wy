// Package sample exercises every kind of symbol wy extract handles.
package sample

import (
	"fmt"
	"io"
	"math"
)

const (
	Name           = "sample"
	Enabled        = true
	Answer         = 42
	Big            = math.MaxUint64
	Huge           = 1 << 100
	Pi             = 3.25
	Imag           = 1 + 2i
	Typed    Level = 3
	internal       = 7
)

type Level int

func (l Level) String() string { return fmt.Sprintf("L%d", int(l)) }

var Counter int

type Point struct{ X, Y int }

func (p Point) Sum() int { return p.X + p.Y }

// Shape can be implemented by interpreted code.
type Shape interface {
	Area() float64
	Scale(f float64, names ...string) (Shape, error)
	Reset()
}

// Total calls back into Shape implementations.
func Total(shapes ...Shape) float64 {
	t := 0.0
	for _, s := range shapes {
		t += s.Area()
	}
	return t
}

func Grow(s Shape) (float64, error) {
	s2, err := s.Scale(2, "x")
	if err != nil {
		return 0, err
	}
	s2.Reset()
	return s2.Area(), nil
}

// hidden is not wrappable: one of its methods is unexported.
type Hidden interface {
	Do()
	private()
}

// Empty has no methods to forward.
type Empty interface{}

// Closer embeds a standard interface.
type Closer interface{ io.Closer }

type Alias = Point

func Map[T any](x T) T { return x }

type Box[T any] struct{ V T }

func unexported() {}
