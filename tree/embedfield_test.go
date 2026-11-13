package tree

import "testing"

// reflect.StructOf rejects some embedded fields whose types have methods;
// promotion still works through the interpreter.
func TestEmbeddedFieldsWithMethods(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		// A native value type with methods, not alone in the struct.
		{src: `type withTime struct { time.Duration; n int }`},
		{src: `w := withTime{time.Second, 2}; w.String(), w.n, w.Seconds()`, want: "1s 2 1"},
		// A native pointer with methods, not the first field.
		{src: `type withBuf struct { n int; *strings.Builder }`},
		{src: `b := withBuf{1, &strings.Builder{}}; b.WriteString("hi"); b.String(), b.n`, want: "hi 1"},
		// Interfaces stay embedded for reflection, and promote methods.
		{src: `type withStringer struct { fmt.Stringer; n int }`},
		{src: `s := withStringer{time.Minute, 3}; s.String(), reflect.TypeOf(s).Field(0).Anonymous`, want: "1m0s true"},
		// Plain structs stay embedded: encoding/json inlines them.
		{src: `type Inner struct{ A int }`},
		{src: `type outer struct { Inner; time.Duration; B int }`},
		{src: `j, err := json.Marshal(outer{Inner{1}, 2, 3}); string(j), err`, want: `{"A":1,"Duration":2,"B":3} <nil>`},
		{src: `o := outer{Inner{1}, time.Hour, 3}; o.A, o.Hours()`, want: "1 1"},
	})
}
