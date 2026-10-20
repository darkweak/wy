//go:build ignore

package main

import (
	"fmt"
	"unsafe"
)

type header struct {
	tag   uint8
	size  uint32
	value float64
	name  string
}

type sliceHeader struct {
	data     unsafe.Pointer
	len, cap int
}

func main() {
	var h header
	fmt.Println(unsafe.Sizeof(h), unsafe.Alignof(h.size), unsafe.Offsetof(h.value), unsafe.Offsetof(h.name))

	// Pointer arithmetic over a struct's fields.
	h = header{tag: 7, size: 99, value: 2.5, name: "hdr"}
	base := unsafe.Pointer(&h)
	size := *(*uint32)(unsafe.Add(base, unsafe.Offsetof(h.size)))
	val := (*float64)(unsafe.Pointer(uintptr(base) + unsafe.Offsetof(h.value)))
	*val = 7.5
	fmt.Println(size, h.value)

	// Reinterpreting memory.
	f := 1.0
	bits := *(*uint64)(unsafe.Pointer(&f))
	fmt.Printf("%#x\n", bits)
	arr := [4]byte{1, 2, 3, 4}
	u := *(*uint32)(unsafe.Pointer(&arr))
	fmt.Println(u == 0x04030201 || u == 0x01020304)

	// Slices and strings without copying.
	xs := []int32{10, 20, 30, 40}
	sh := (*sliceHeader)(unsafe.Pointer(&xs))
	fmt.Println(sh.len, sh.cap, *(*int32)(sh.data))
	view := unsafe.Slice(&xs[1], 2)
	view[0] = 200
	fmt.Println(xs, len(view))
	b := []byte("hello")
	s := unsafe.String(unsafe.SliceData(b), len(b))
	fmt.Println(s, len(s))
	sd := unsafe.StringData("abc")
	fmt.Println(*sd, string(unsafe.Slice(sd, 3)))

	// Pointer to a struct field, then back to the struct (container_of).
	type node struct {
		id   int
		link *int
	}
	n := &node{id: 3}
	fieldPtr := unsafe.Pointer(&n.link)
	back := (*node)(unsafe.Add(fieldPtr, -int(unsafe.Offsetof(n.link))))
	fmt.Println(back == n, back.id)
	var nilp *int
	fmt.Println(unsafe.Pointer(nilp) == nil, uintptr(unsafe.Pointer(nilp)))
}
