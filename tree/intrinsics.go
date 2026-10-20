package tree

import (
	"go/types"
	"reflect"

	"github.com/darkweak/wy/tree/native"
)

type nativeInfo = native.Package

// nativeInfo returns the compiled package behind an import path, unless the
// package is interpreted from source.
func (in *Interp) nativeInfo(path string) *native.Package {
	if _, src := in.src[path]; src {
		return nil
	}
	if p := in.overrides[path]; p != nil {
		return p
	}
	return native.Lookup(path)
}

type invoker = func(args []reflect.Value, spread bool, deferOf *frame) []reflect.Value

// nativeIntrinsic replaces compiled functions whose behaviour depends on
// Go types or call stacks that only the interpreter knows. The result binds
// the calling frame.
func nativeIntrinsic(in *Interp, name string) func(fr *frame) invoker {
	switch name {
	case "errors.As":
		return func(*frame) invoker { return in.errorsAs }
	case "runtime.Caller":
		return func(fr *frame) invoker {
			return func(args []reflect.Value, _ bool, _ *frame) []reflect.Value {
				return in.runtimeCaller(fr, int(args[0].Int()))
			}
		}
	case "runtime.FuncForPC":
		return func(*frame) invoker {
			return func(args []reflect.Value, _ bool, _ *frame) []reflect.Value {
				return []reflect.Value{reflect.ValueOf(funcForPC(uintptr(args[0].Uint())))}
			}
		}
	}
	return nil
}

// intrinsicFor implements source functions that have no Go body (they are
// provided by the runtime in compiled Go).
func intrinsicFor(in *Interp, fn *types.Func) *function {
	if fn.Pkg() == nil {
		return nil
	}
	switch fn.Pkg().Path() + "." + fn.Name() {
	case "maps.clone":
		return &function{in: in, name: "maps.clone", builtin: func(args []reflect.Value) []reflect.Value {
			iv := args[0]
			out := reflect.New(anyRT).Elem()
			if iv.IsNil() {
				return []reflect.Value{out}
			}
			nt, m := in.dyn(iv)
			if m.IsNil() {
				out.Set(iv)
				return []reflect.Value{out}
			}
			c := reflect.MakeMapWithSize(m.Type(), m.Len())
			it := m.MapRange()
			for it.Next() {
				c.SetMapIndex(it.Key(), it.Value())
			}
			return []reflect.Value{in.ifaceFrom(nt, c, anyRT)}
		}}
	}
	return nil
}

// errorsAs is errors.As with interpreter types: the target's element type
// and the error chain's dynamic types are compared as Go types.
func (in *Interp) errorsAs(args []reflect.Value, _ bool, _ *frame) []reflect.Value {
	errV, target := args[0], args[1]
	nt, tv := in.dyn(target)
	if nt == nil {
		panic("errors: target cannot be nil")
	}
	ptr, ok := nt.t.Underlying().(*types.Pointer)
	if !ok || tv.IsNil() {
		panic("errors: target must be a non-nil pointer")
	}
	elemT := ptr.Elem()
	errIface := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	if !isInterface(elemT) && !types.Implements(elemT, errIface) {
		panic("errors: *target must be interface or implement error")
	}
	check := in.assertion(elemT)
	dst := tv.Elem()
	var walk func(e reflect.Value) bool
	walk = func(e reflect.Value) bool {
		for {
			if e.IsNil() {
				return false
			}
			if v, ok := check(e); ok {
				dst.Set(v)
				return true
			}
			ent, ev := in.dyn(e)
			if m := ent.method("As", nil); m != nil && m.sig.Params().Len() == 1 && m.sig.Results().Len() == 1 {
				arg := in.toIface(target, types.NewInterfaceType(nil, nil), anyRT)
				if in.invoke(m, ev, []reflect.Value{arg}, false, nil)[0].Bool() {
					return true
				}
			}
			m := ent.method("Unwrap", nil)
			if m == nil || m.sig.Params().Len() != 0 || m.sig.Results().Len() != 1 {
				return false
			}
			r := in.invoke(m, ev, nil, false, nil)[0]
			if r.Kind() == reflect.Slice {
				for i := 0; i < r.Len(); i++ {
					if walk(r.Index(i)) {
						return true
					}
				}
				return false
			}
			e = r
		}
	}
	return []reflect.Value{reflect.ValueOf(walk(errV))}
}
