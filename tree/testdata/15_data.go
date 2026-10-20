//go:build ignore

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Address struct {
	City string `json:"city"`
	Zip  string `json:"zip,omitempty"`
}

type Person struct {
	Name    string         `json:"name"`
	Age     int            `json:"age"`
	Address *Address       `json:"address,omitempty"`
	Tags    []string       `json:"tags"`
	Meta    map[string]any `json:"meta,omitempty"`
	secret  string
}

type node struct {
	val         int
	left, right *node
}

func (n *node) insert(v int) *node {
	if n == nil {
		return &node{val: v}
	}
	if v < n.val {
		n.left = n.left.insert(v)
	} else {
		n.right = n.right.insert(v)
	}
	return n
}

func (n *node) walk(f func(int)) {
	if n == nil {
		return
	}
	n.left.walk(f)
	f(n.val)
	n.right.walk(f)
}

type list struct {
	head *elem
}
type elem struct {
	v    string
	next *elem
	kids []elem
	meta map[string]*elem
}

func main() {
	p := Person{Name: "Ana", Age: 30, Address: &Address{City: "Paris"}, Tags: []string{"a", "b"}, secret: "x"}
	b, _ := json.Marshal(p)
	fmt.Println(string(b))
	var q Person
	err := json.Unmarshal([]byte(`{"name":"Bo","age":7,"tags":["z"],"meta":{"k":1.5,"l":[1,"x"]}}`), &q)
	fmt.Println(err, q.Name, q.Age, q.Tags, q.Address == nil, q.Meta["k"], q.Meta["l"])
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", " ")
	enc.Encode(map[string]int{"b": 2, "a": 1})
	fmt.Print(buf.String())

	var root *node
	for _, v := range []int{5, 3, 8, 1, 4, 9} {
		root = root.insert(v)
	}
	var out []string
	root.walk(func(v int) { out = append(out, strconv.Itoa(v)) })
	fmt.Println(strings.Join(out, ","))

	l := list{head: &elem{v: "a", next: &elem{v: "b"}, kids: []elem{{v: "k1"}, {v: "k2"}}, meta: map[string]*elem{}}}
	l.head.meta["self"] = l.head
	fmt.Println(l.head.next.v, len(l.head.kids), l.head.kids[1].v, l.head.meta["self"].v)

	s := []int{5, 2, 8, 1}
	s2 := slices.Clone(s)
	slices.Sort(s2)
	idx, found := slices.BinarySearch(s2, 5)
	fmt.Println(s, s2, idx, found, slices.Max(s), slices.Contains(s, 8), slices.Index(s, 8))
	m := map[string]int{"x": 1, "y": 2}
	keys := slices.Sorted(maps.Keys(m))
	fmt.Println(keys, slices.Collect(maps.Values(map[int]int{1: 10})))
	grid := make([][]rune, 2)
	for i := range grid {
		grid[i] = []rune(strings.Repeat(".", 3))
	}
	grid[1][2] = '#'
	for _, row := range grid {
		fmt.Println(string(row))
	}
	str := "héllo, 世界"
	fmt.Println(len(str), utf8.RuneCountInString(str), []byte(str[:3]), []rune(str)[1], str[1:3] == "é")
	bs := []byte("abc")
	bs = append(bs, "def"...)
	fmt.Println(string(bs), cap(bs) >= 6, string(rune(65)), strconv.Quote(string(bs[1:4])))
	arr := [5]int{1, 2, 3, 4, 5}
	sl := arr[1:3:4]
	sl = append(sl, 99)
	fmt.Println(arr, sl, len(sl), cap(sl))
	sl = append(sl, 100)
	sl[0] = -1
	fmt.Println(arr, sl)
	type pair struct {
		a int
		b string
	}
	set := map[pair]bool{{1, "x"}: true}
	fmt.Println(set[pair{1, "x"}], set[pair{2, "x"}], pair{1, "x"} == pair{1, "x"})
	copy(arr[:], []int{9, 9})
	fmt.Println(arr)
	var anys []any = []any{1, "a", nil, 2.5, []int{1}}
	fmt.Println(anys...)
	fmt.Printf("%v %+v %d %5.2f %q %x %T\n", p.Address, *p.Address, 42, 3.14159, "q", 255, 3)
}
