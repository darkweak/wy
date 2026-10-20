package tree

import (
	"go/types"
	"reflect"
	"strings"
	"sync"
)

// Printing interpreted values
//
// fmt decides how to print a value from its dynamic type: Formatter, error,
// Stringer, then the value's kind, recursively. An interpreted value whose
// type (or the type of a nested element) has such methods is converted to
// a display mirror before it reaches fmt: elements with methods become
// wrappers exposing exactly those methods, and the containers around them
// become the same shape with interface-typed elements, which fmt prints
// identically. Values without such methods are passed unchanged.

var displayCache sync.Map // types.Type -> bool (needs a mirror)

// needsDisplay reports whether a value of static type t may contain
// something fmt would print differently from its reflect representation.
func (in *Interp) needsDisplay(t types.Type) bool {
	if v, ok := displayCache.Load(t); ok {
		return v.(bool)
	}
	displayCache.Store(t, false) // cycle guard
	r := in.computeNeedsDisplay(t)
	displayCache.Store(t, r)
	return r
}

func (in *Interp) computeNeedsDisplay(t types.Type) bool {
	if isInterface(t) {
		return true
	}
	if in.isNativeNamed(t) {
		return false // compiled types print natively
	}
	if nt := in.ntypes.get(t); !nt.faithful && nt.comboType() != nil {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Pointer:
		return in.needsDisplay(u.Elem())
	case *types.Slice:
		return in.needsDisplay(u.Elem())
	case *types.Array:
		return in.needsDisplay(u.Elem())
	case *types.Map:
		return in.needsDisplay(u.Key()) || in.needsDisplay(u.Elem())
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			if in.needsDisplay(u.Field(i).Type()) {
				return true
			}
		}
	}
	return false
}

// display converts v (static type t) for printing by fmt.
func (in *Interp) display(v reflect.Value, t types.Type, depth int) reflect.Value {
	if !v.IsValid() {
		return v
	}
	if isInterface(t) {
		if v.IsNil() {
			return reflect.Zero(anyRT)
		}
		nt, e := in.dyn(v)
		return in.display(e, nt.t, depth)
	}
	nt := in.ntypes.get(t)
	if !nt.faithful {
		if wt := nt.comboType(); wt != nil {
			return newWrapper(wt, boxed{nt, v.Interface()})
		}
	}
	if !in.needsDisplay(t) || depth > 16 {
		return v
	}
	switch u := t.Underlying().(type) {
	case *types.Pointer:
		if v.IsNil() || depth > 0 {
			return v // fmt prints nested pointers as addresses
		}
		inner := in.display(v.Elem(), u.Elem(), depth+1)
		p := reflect.New(inner.Type())
		p.Elem().Set(inner)
		return p
	case *types.Slice, *types.Array:
		var et types.Type
		if s, ok := u.(*types.Slice); ok {
			if v.IsNil() {
				return v
			}
			et = s.Elem()
		} else {
			et = u.(*types.Array).Elem()
		}
		out := reflect.MakeSlice(reflect.SliceOf(anyRT), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			if d := in.display(v.Index(i), et, depth+1); d.IsValid() {
				out.Index(i).Set(d)
			}
		}
		if _, isArr := u.(*types.Array); isArr {
			arr := reflect.New(reflect.ArrayOf(v.Len(), anyRT)).Elem()
			reflect.Copy(arr, out)
			return arr
		}
		return out
	case *types.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(reflect.MapOf(in.displayKeyType(u.Key()), anyRT), v.Len())
		it := v.MapRange()
		for it.Next() {
			k := in.display(it.Key(), u.Key(), depth+1)
			if !k.IsValid() {
				k = reflect.Zero(out.Type().Key())
			}
			e := in.display(it.Value(), u.Elem(), depth+1)
			if !e.IsValid() {
				e = reflect.Zero(anyRT)
			}
			out.SetMapIndex(k, e)
		}
		return out
	case *types.Struct:
		fields := make([]reflect.StructField, u.NumFields())
		vals := make([]reflect.Value, u.NumFields())
		for i := range fields {
			f := u.Field(i)
			fv := in.fieldOf(v, u, i)
			sf := reflect.StructField{Name: f.Name(), Type: fv.Type()}
			if strings.HasPrefix(sf.Name, "_") || sf.Name == "_" {
				sf.Name = "X" + sf.Name
			}
			if in.needsDisplay(f.Type()) {
				sf.Type = anyRT
				fv = in.display(fv, f.Type(), depth+1)
			}
			if !isExported(sf.Name) {
				sf.PkgPath = "wy/display"
			}
			fields[i], vals[i] = sf, fv
		}
		mirror := reflect.New(reflect.StructOf(fields)).Elem()
		for i, fv := range vals {
			if fv.IsValid() {
				fieldByIndex(mirror, i).Set(fv)
			}
		}
		return mirror
	}
	return v
}

func (in *Interp) displayKeyType(k types.Type) reflect.Type {
	if in.needsDisplay(k) {
		return anyRT
	}
	return in.rtype(k)
}

// isNativeNamed reports whether t (or the type t points to) is a named type
// of a compiled package.
func (in *Interp) isNativeNamed(t types.Type) bool {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		if _, named := types.Unalias(t).(*types.Named); !named {
			t = p.Elem()
		}
	}
	n, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	in.rtMu.Lock()
	defer in.rtMu.Unlock()
	return in.natives.reflectOf(n) != nil
}
