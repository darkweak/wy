//go:build ignore

package main

import "fmt"

func classify(n int) string {
	switch {
	case n < 0:
		return "neg"
	case n == 0:
		return "zero"
	case n%2 == 0:
		return "even"
	}
	return "odd"
}

func fall(n int) []string {
	var out []string
	switch n {
	case 1:
		out = append(out, "one")
		fallthrough
	case 2:
		out = append(out, "two")
		fallthrough
	case 3:
		out = append(out, "three")
	case 4, 5:
		out = append(out, "four-five")
	default:
		out = append(out, "other")
	}
	return out
}

func main() {
	for _, n := range []int{-1, 0, 3, 8} {
		fmt.Print(classify(n), " ")
	}
	fmt.Println()
	fmt.Println(fall(1), fall(2), fall(5), fall(9))

outer:
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if j == 2 {
				continue outer
			}
			if i == 2 {
				break outer
			}
			fmt.Print(i, j, " ")
		}
	}
	fmt.Println()

	i := 0
loop:
	if i < 5 {
		i++
		goto loop
	}
	fmt.Println("goto:", i)

	sum := 0
	for i := range 10 {
		if i%3 == 0 {
			continue
		}
		sum += i
	}
	fmt.Println("sum", sum)

	m := map[string]int{"a": 1, "b": 2, "c": 3}
	total := 0
	for _, v := range m {
		total += v
	}
	fmt.Println("total", total)
	for i, r := range "héllo" {
		fmt.Print(i, ":", string(r), " ")
	}
	fmt.Println()
	arr := [3]int{1, 2, 3}
	for i, v := range arr {
		arr[2] = 100 // ranging over an array copy
		if i == 2 {
			fmt.Println("array copy:", v)
		}
	}
	x := 5
	switch y := x * 2; {
	case y > 5:
		fmt.Println("big", y)
	}
	var k uint8 = 250
	for k < 255 {
		k += 3
	}
	fmt.Println("wrapped", k)
}
