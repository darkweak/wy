//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
)

type Point struct{ X, Y int }
type Line struct{ A, B Point }

type Shape interface{ Area() float64 }
type Rect struct{ W, H float64 }

func (r Rect) Area() float64 { return r.W * r.H }

func main() {
	seen := map[Line]int{}
	seen[Line{Point{1, 2}, Point{3, 4}}]++
	seen[Line{Point{1, 2}, Point{3, 4}}]++
	fmt.Println(len(seen), seen[Line{Point{1, 2}, Point{3, 4}}])
	var a, b Shape = Rect{1, 2}, Rect{1, 2}
	fmt.Println(a == b)
	arr := [2]Point{{1, 1}, {2, 2}}
	cp := arr
	cp[0].X = 9
	fmt.Println(arr == cp, arr[0].X)
	out, _ := json.Marshal(struct {
		Shape `json:"shape"`
		Tags  []string `json:"tags,omitempty"`
	}{Rect{2, 3}, nil})
	fmt.Println(string(out))
}
