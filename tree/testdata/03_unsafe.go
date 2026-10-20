//go:build ignore

package main

import (
	"fmt"
	"unsafe"
)

type header struct {
	a uint8
	b uint64
	c uint16
}

func main() {
	var h header
	fmt.Println(unsafe.Sizeof(h), unsafe.Offsetof(h.b), unsafe.Alignof(h.c))
	xs := []int32{10, 20, 30}
	p := unsafe.Pointer(&xs[0])
	third := *(*int32)(unsafe.Add(p, 2*unsafe.Sizeof(xs[0])))
	s := unsafe.String(unsafe.SliceData([]byte("bytes!")), 5)
	fmt.Println(third, s)
}
