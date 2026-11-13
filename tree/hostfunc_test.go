package tree

import (
	"context"
	"strings"
	"testing"
)

func TestHostFuncs(t *testing.T) {
	gopath := t.TempDir()
	writeFiles(t, gopath, map[string]string{
		// The assembly stub is excluded by the purego tag in favour of
		// the Go fallback.
		"src/example.com/asm/asm_stub.go":    "//go:build !purego\n\npackage asm\n\nfunc Sum(a, b int) int\n",
		"src/example.com/asm/asm_generic.go": "//go:build purego\n\npackage asm\n\nfunc Sum(a, b int) int { return a + b }\n",
		"src/example.com/rt/rt.go": `package rt

import "unsafe"

//go:linkname nanotime runtime.nanotime
func nanotime() int64

//go:linkname memclr runtime.memclrNoHeapPointers
func memclr(p unsafe.Pointer, n uintptr)

//go:linkname memhash runtime.memhash
func memhash(p unsafe.Pointer, h, s uintptr) uintptr

// Missing has no body and no host implementation.
func Missing() int

var Clock = nanotime

func Check() string {
	b := []byte("abcd")
	memclr(unsafe.Pointer(&b[0]), 3)
	x, y := []byte("same"), []byte("same")
	if memhash(unsafe.Pointer(&x[0]), 1, 4) != memhash(unsafe.Pointer(&y[0]), 1, 4) {
		return "unstable hash"
	}
	t0 := Clock()
	if t0 <= 0 || nanotime() < t0 {
		return "bad clock"
	}
	return string(b)
}
`,
	})
	in := New(Options{GoPath: gopath})
	ctx := context.Background()
	vals, err := in.Eval(ctx, `import (
	"example.com/asm"
	"example.com/rt"
)
asm.Sum(2, 3)`)
	if err != nil {
		t.Fatal(err)
	}
	if vals[0] != 5 {
		t.Fatalf("Sum = %v, want 5", vals[0])
	}
	vals, err = in.Eval(ctx, `rt.Check()`)
	if err != nil {
		t.Fatal(err)
	}
	if vals[0] != "\x00\x00\x00d" {
		t.Fatalf("Check = %q", vals[0])
	}
	if _, err = in.Eval(ctx, `rt.Missing()`); err == nil || !strings.Contains(err.Error(), "has no body") {
		t.Fatalf("Missing: err = %v, want no body", err)
	}
}
