package tree

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"hash/maphash"
	"reflect"
	"runtime"
	"unique"
	"unsafe"
	"weak"
)

// The host functions of weak and unique read the single word of the
// standard types.
var (
	_ [unsafe.Sizeof(weak.Pointer[byte]{}) - unsafe.Sizeof(unsafe.Pointer(nil))]struct{}
	_ [unsafe.Sizeof(unsafe.Pointer(nil)) - unsafe.Sizeof(weak.Pointer[byte]{})]struct{}
	_ [unsafe.Sizeof(unique.Handle[any]{}) - unsafe.Sizeof(unsafe.Pointer(nil))]struct{}
	_ [unsafe.Sizeof(unsafe.Pointer(nil)) - unsafe.Sizeof(unique.Handle[any]{})]struct{}
)

// Host halves of the generic standard functions of tree/stdsrc/overlay: the
// interpreted generic code passes its values boxed in interfaces to these
// functions, which call a compiled instantiation.
func init() {
	for name, fn := range map[string]any{
		"wy/overlay/hash/maphash.comparableHash": func(seed maphash.Seed, v any) uint64 {
			return maphash.Comparable(seed, v)
		},
		"wy/overlay/hash/maphash.writeComparable": func(h *maphash.Hash, v any) {
			maphash.WriteComparable(h, v)
		},
		"wy/overlay/runtime.addCleanup": func(ptr unsafe.Pointer, cleanup func(any), arg any) runtime.Cleanup {
			if a := reflect.ValueOf(arg); a.Kind() == reflect.Pointer || a.Kind() == reflect.UnsafePointer {
				if a.UnsafePointer() == ptr {
					panic("runtime.AddCleanup: ptr is equal to arg, cleanup will never run")
				}
			}
			return runtime.AddCleanup((*byte)(ptr), cleanup, arg)
		},
		// The handle of a weak.Pointer is its only word.
		"weak.registerWeakPointer": func(p unsafe.Pointer) unsafe.Pointer {
			w := weak.Make((*byte)(p))
			return *(*unsafe.Pointer)(unsafe.Pointer(&w))
		},
		"weak.makeStrongFromWeak": func(u unsafe.Pointer) unsafe.Pointer {
			var w weak.Pointer[byte]
			*(*unsafe.Pointer)(unsafe.Pointer(&w)) = u
			return unsafe.Pointer(w.Value())
		},
		// The canonical value of a unique.Handle is its only word.
		"unique.makeHandle": func(v any) unsafe.Pointer {
			h := unique.Make(v)
			return *(*unsafe.Pointer)(unsafe.Pointer(&h))
		},
		"wy/overlay/encoding/json/jsontext.appendQuote":   jsontext.AppendQuote[string],
		"wy/overlay/encoding/json/jsontext.appendUnquote": jsontext.AppendUnquote[string],
		"wy/overlay/encoding/json/jsontext.appendFormat": func(dst []byte, src string, opts []jsontext.Options) ([]byte, error) {
			return jsontext.AppendFormat(dst, src, opts...)
		},
		"wy/overlay/encoding/json/v2.marshalFunc":       jsonMarshalFunc,
		"wy/overlay/encoding/json/v2.marshalToFunc":     jsonMarshalToFunc,
		"wy/overlay/encoding/json/v2.unmarshalFunc":     jsonUnmarshalFunc,
		"wy/overlay/encoding/json/v2.unmarshalFromFunc": jsonUnmarshalFromFunc,
		"wy/overlay/encoding/json/v2.getOption":         jsonGetOption,
	} {
		hostImpls[name] = callHost(fn)
	}
}

// callHost adapts a compiled function to a hostImpl.
func callHost(fn any) hostImpl {
	fv := reflect.ValueOf(fn)
	return func(args []reflect.Value, res []reflect.Type) []reflect.Value {
		out := fv.Call(args)
		for i := range out {
			if out[i].Type() != res[i] {
				out[i] = out[i].Convert(res[i])
			}
		}
		return out
	}
}

// jsonCastable reports whether a function of json/v2 registered for type t
// applies to p, a pointer to the value being (un)marshaled, following the
// rules of json.MarshalFunc: an interface t when the pointer implements it,
// a pointer t when it is the pointer's type, other types when they are the
// value's type.
func jsonCastable(p any, t reflect.Type) bool {
	pt := reflect.TypeOf(p)
	switch t.Kind() {
	case reflect.Interface:
		return pt.Implements(t)
	case reflect.Pointer:
		return pt == t
	default:
		return pt.Elem() == t
	}
}

func jsonAssertCastable(t reflect.Type, marshal bool) {
	switch {
	case t.Kind() == reflect.Interface, t.Kind() == reflect.Pointer && t.Name() == "":
		return
	case t.Kind() != reflect.Pointer && marshal:
		return
	case marshal:
		panic(fmt.Sprintf("input type %v must be an interface type, an unnamed pointer type, or a non-pointer type", t))
	default:
		panic(fmt.Sprintf("input type %v must be an interface type or an unnamed pointer type", t))
	}
}

// jsonArg returns what the interpreted half of a json/v2 function receives
// for p, a pointer to the value being marshaled: a raw pointer, that pointer
// in an interface, the value with its interpreted type if the encoder found
// it boxed in an interface (nil otherwise), and whether a function
// registered for t may apply. A boxed value is marshaled from a copy, and
// matches an interface t through the interpreter's type assertion.
func jsonArg(p any, t reflect.Type) (unsafe.Pointer, any, any, bool) {
	if bp, ok := p.(*boxed); ok {
		v := reflect.ValueOf(bp.v)
		cp := reflect.New(v.Type())
		cp.Elem().Set(v)
		return cp.UnsafePointer(), cp.Interface(), *bp, t.Kind() == reflect.Interface || jsonCastable(cp.Interface(), t)
	}
	return reflect.ValueOf(p).UnsafePointer(), p, nil, jsonCastable(p, t)
}

func jsonMarshalFunc(t reflect.Type, fn func(unsafe.Pointer, any, any) (bool, []byte, error)) *json.Marshalers {
	jsonAssertCastable(t, true)
	return json.MarshalToFunc(func(enc *jsontext.Encoder, p any) error {
		raw, pi, vi, ok := jsonArg(p, t)
		if !ok {
			return errors.ErrUnsupported
		}
		ok, val, err := fn(raw, pi, vi)
		switch {
		case !ok:
			return errors.ErrUnsupported
		case errors.Is(err, errors.ErrUnsupported):
			return errors.New("marshal function of type func(T) ([]byte, error) may not return errors.ErrUnsupported")
		case err != nil:
			return err
		}
		return enc.WriteValue(val)
	})
}

func jsonMarshalToFunc(t reflect.Type, fn func(*jsontext.Encoder, unsafe.Pointer, any, any) (bool, error)) *json.Marshalers {
	jsonAssertCastable(t, true)
	return json.MarshalToFunc(func(enc *jsontext.Encoder, p any) error {
		raw, pi, vi, ok := jsonArg(p, t)
		if !ok {
			return errors.ErrUnsupported
		}
		ok, err := fn(enc, raw, pi, vi)
		if !ok {
			return errors.ErrUnsupported
		}
		return err
	})
}

// Unmarshalers decode into p itself: a value boxed in an interface is not
// addressable, so they skip it.

func jsonUnmarshalFunc(t reflect.Type, fn func([]byte, unsafe.Pointer, any) error) *json.Unmarshalers {
	jsonAssertCastable(t, false)
	return json.UnmarshalFromFunc(func(dec *jsontext.Decoder, p any) error {
		if _, ok := p.(*boxed); ok || !jsonCastable(p, t) {
			return errors.ErrUnsupported
		}
		val, err := dec.ReadValue()
		if err != nil {
			return err
		}
		if err := fn(val, reflect.ValueOf(p).UnsafePointer(), p); err != nil {
			if errors.Is(err, errors.ErrUnsupported) {
				return errors.New("unmarshal function of type func([]byte, T) error may not return errors.ErrUnsupported")
			}
			return err
		}
		return nil
	})
}

func jsonUnmarshalFromFunc(t reflect.Type, fn func(*jsontext.Decoder, unsafe.Pointer, any) error) *json.Unmarshalers {
	jsonAssertCastable(t, false)
	return json.UnmarshalFromFunc(func(dec *jsontext.Decoder, p any) error {
		if _, ok := p.(*boxed); ok || !jsonCastable(p, t) {
			return errors.ErrUnsupported
		}
		return fn(dec, reflect.ValueOf(p).UnsafePointer(), p)
	})
}

// jsonGetOption is json.GetOption for the types of the options of
// encoding/json, encoding/json/v2 and encoding/json/jsontext.
func jsonGetOption(opts json.Options, setter any) (any, bool) {
	switch s := setter.(type) {
	case func(bool) json.Options:
		return json.GetOption(opts, s)
	case func(string) json.Options:
		return json.GetOption(opts, s)
	case func(int) json.Options:
		return json.GetOption(opts, s)
	case func(int64) json.Options:
		return json.GetOption(opts, s)
	case func(*json.Marshalers) json.Options:
		return json.GetOption(opts, s)
	case func(*json.Unmarshalers) json.Options:
		return json.GetOption(opts, s)
	}
	panic(fmt.Sprintf("wy: json.GetOption with a setter of type %T is not supported", setter))
}
