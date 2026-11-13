package tree

import "testing"

// reflect.Type.Implements sees interpreted interfaces, as msgpack's
// typ.Implements(customEncoderType) needs: they are not all `any`.
func TestReflectImplementsInterpretedInterface(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: "type Encoder interface{ EncodeMsgpack() string }\ntype Other interface{ Other() }\ntype withEnc struct{ N int }\nfunc (w *withEnc) EncodeMsgpack() string { return fmt.Sprint(\"enc\", w.N) }\ntype member struct{ Name string }\nfunc (m member) String() string { return m.Name }\nvar encT = reflect.TypeOf((*Encoder)(nil)).Elem()\nvar strT = reflect.TypeOf((*fmt.Stringer)(nil)).Elem()"},
		{src: `encT.Kind() == reflect.Interface, encT == reflect.TypeOf((*Encoder)(nil)).Elem(), encT != reflect.TypeOf((*Other)(nil)).Elem()`, want: "true true true"},
		{src: `mt := reflect.TypeOf(member{}); mt.Implements(encT), reflect.PointerTo(mt).Implements(encT), mt.Implements(strT)`, want: "false false true"},
		{src: `wt := reflect.TypeOf(withEnc{}); wt.Implements(encT), reflect.PointerTo(wt).Implements(encT), reflect.TypeOf(&withEnc{}).Implements(encT)`, want: "false true true"},
		{src: `reflect.TypeOf(3).Implements(encT), reflect.TypeOf(&withEnc{}).AssignableTo(encT), reflect.TypeOf(member{}).AssignableTo(encT)`, want: "false true false"},
		// Values reached through reflection still assert to the interface.
		{src: `v := reflect.ValueOf(&withEnc{N: 4}).Elem(); v.Addr().Interface().(Encoder).EncodeMsgpack()`, want: "enc4"},
		{src: `p := reflect.New(encT); p.Elem().Set(reflect.ValueOf(&withEnc{N: 5})); p.Elem().Interface().(Encoder).EncodeMsgpack()`, want: "enc5"},
		{src: `var e Encoder = &withEnc{N: 6}; reflect.TypeOf(e) == reflect.TypeOf(&withEnc{}), reflect.TypeOf(nil) == nil`, want: "true true"},
	})
}
