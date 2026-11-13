package tree

import "testing"

// A string header reinterpreted as a slice header has a garbage capacity,
// that compiled code never checks (olric's partitions.HKey).
func TestSliceOfReinterpretedStringHeader(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: "type hdr struct{ p unsafe.Pointer; n, c int }\nfunc sum(b []byte) (s int) { for len(b) >= 4 { for _, x := range b[:4:4] { s += int(x) }; b = b[4:] }; for _, x := range b { s += int(x) }; return }"},
		{src: `str := strings.Repeat("a", 9); h := hdr{p: unsafe.Pointer(unsafe.StringData(str)), n: 9, c: -6533128120350124710}; b := *(*[]byte)(unsafe.Pointer(&h)); sum(b), len(b[2:5])`, want: "873 3"},
	})
}
