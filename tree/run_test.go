package tree

import (
	"context"
	"os"
	"testing"
)

func TestRunHello(t *testing.T) {
	in := New(Options{})
	err := in.RunFiles(context.Background(), map[string][]byte{"main.go": []byte(`package main

import "fmt"

type point struct{ x, y int }

func (p point) String() string { return fmt.Sprintf("(%d,%d)", p.x, p.y) }

func fib(n int) int {
	if n < 2 {
		return n
	}
	return fib(n-1) + fib(n-2)
}

func main() {
	fmt.Println("hello", fib(20), point{1, 2})
	xs := []int{3, 1, 2}
	for i, x := range xs {
		fmt.Print(i, ":", x, " ")
	}
	fmt.Println()
}
`)})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Stdout
}
