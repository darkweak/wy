package tree

import (
	"encoding/json"
	"fmt"
	"go/types"
	"reflect"
	"strings"
	"sync"
	"unsafe"
)

// JSON mirrors
//
// An interpreted named type's reflect type has no methods, so compiled
// encoders (encoding/json, encoding/xml) do not see the MarshalJSON,
// UnmarshalJSON, MarshalText or UnmarshalText of interpreted types nested in
// the values they are given: a struct field of type
//
//	type RetentionPolicy int // with MarshalJSON
//
// is encoded as a number. Encoding arguments that contain such types are
// therefore replaced by a mirror: a value of a reflect type in which every
// such type is a pointer to a JSON proxy, a compiled type whose methods call
// the interpreted ones. Each proxy type of the pool stands for one
// interpreted type. After a decoding call, the mirror is copied back into
// the interpreter's value.

var jsonMethodNames = [...]string{"MarshalJSON", "UnmarshalJSON", "MarshalText", "UnmarshalText"}

type jsonSlot struct {
	in *Interp
	t  types.Type // the interpreted type
}

var (
	jsonSlotsMu sync.Mutex
	jsonSlots   [len(jsonProxyPool)]jsonSlot
	jsonSlotsN  int
)

type jsonMirrors struct {
	mu         sync.Mutex
	rts        map[types.Type]reflect.Type // nil value: no mirror needed
	inProgress map[types.Type]bool
}

// jsonSlot returns the proxy type standing for t, or nil when the pool is
// exhausted.
func (in *Interp) jsonProxyType(t types.Type) reflect.Type {
	jsonSlotsMu.Lock()
	defer jsonSlotsMu.Unlock()
	for i := 0; i < jsonSlotsN; i++ {
		if s := jsonSlots[i]; s.in == in && types.Identical(s.t, t) {
			return jsonProxyPool[i]
		}
	}
	if jsonSlotsN == len(jsonProxyPool) {
		return nil
	}
	jsonSlots[jsonSlotsN] = jsonSlot{in: in, t: t}
	jsonSlotsN++
	return jsonProxyPool[jsonSlotsN-1]
}

func jsonSlotOf(i int) jsonSlot {
	jsonSlotsMu.Lock()
	defer jsonSlotsMu.Unlock()
	return jsonSlots[i]
}

// jsonMethod returns the interpreted encoding method name of *t, or nil.
func (in *Interp) jsonMethod(t types.Type, name string) *methodImpl {
	if !in.hasJSONMethods(t) {
		return nil
	}
	return in.ntypes.get(types.NewPointer(t)).method(name, nil)
}

// hasJSONMethods reports whether t is an interpreted type with one of the
// methods compiled encoders look for.
func (in *Interp) hasJSONMethods(t types.Type) bool {
	n, ok := types.Unalias(t).(*types.Named)
	if !ok || in.isNativeNamed(n) || isInterface(n) {
		return false
	}
	ms := types.NewMethodSet(types.NewPointer(n))
	for _, name := range jsonMethodNames {
		if ms.Lookup(nil, name) != nil {
			return true
		}
	}
	return false
}

// jsonMirrorRT returns the mirror type of t, or nil if values of t encode
// correctly as they are.
func (in *Interp) jsonMirrorRT(t types.Type) reflect.Type {
	m := &in.jsonMirrors
	m.mu.Lock()
	if m.rts == nil {
		m.rts, m.inProgress = map[types.Type]reflect.Type{}, map[types.Type]bool{}
	}
	if rt, ok := m.rts[t]; ok {
		m.mu.Unlock()
		return rt
	}
	if m.inProgress[t] {
		// A recursive type: the inner occurrence encodes as it is.
		m.mu.Unlock()
		return nil
	}
	m.inProgress[t] = true
	m.mu.Unlock()

	rt := in.buildJSONMirrorRT(t)

	m.mu.Lock()
	delete(m.inProgress, t)
	m.rts[t] = rt
	m.mu.Unlock()
	return rt
}

func (in *Interp) buildJSONMirrorRT(t types.Type) reflect.Type {
	t = types.Unalias(t)
	if in.hasJSONMethods(t) {
		if pt := in.jsonProxyType(t); pt != nil {
			return reflect.PointerTo(pt)
		}
		return nil
	}
	if n, ok := t.(*types.Named); ok && in.isNativeNamed(n) {
		return nil
	}
	switch u := t.Underlying().(type) {
	case *types.Pointer:
		if e := in.jsonMirrorRT(u.Elem()); e != nil {
			return reflect.PointerTo(e)
		}
	case *types.Slice:
		if e := in.jsonMirrorRT(u.Elem()); e != nil {
			return reflect.SliceOf(e)
		}
	case *types.Array:
		if e := in.jsonMirrorRT(u.Elem()); e != nil {
			return reflect.ArrayOf(int(u.Len()), e)
		}
	case *types.Map:
		if e := in.jsonMirrorRT(u.Elem()); e != nil {
			return reflect.MapOf(in.rtype(u.Key()), e)
		}
	case *types.Struct:
		srt := in.rtype(t)
		if srt.Kind() != reflect.Struct || srt.NumField() != u.NumFields() {
			return nil
		}
		fields := make([]reflect.StructField, u.NumFields())
		needed := false
		for i := range fields {
			sf := srt.Field(i)
			f := reflect.StructField{Name: sf.Name, PkgPath: sf.PkgPath, Type: sf.Type, Tag: sf.Tag, Anonymous: sf.Anonymous}
			if sf.IsExported() {
				if e := in.jsonMirrorRT(u.Field(i).Type()); e != nil {
					f.Type, needed = e, true
					if f.Anonymous && e.Kind() == reflect.Pointer && e.Elem().Kind() != reflect.Struct {
						f.Anonymous = false
					}
				}
			}
			fields[i] = f
		}
		if needed {
			if rt, ok := tryStructOf(fields); ok {
				return rt
			}
		}
	}
	return nil
}

func isProxyPtr(rt reflect.Type) bool {
	if rt.Kind() != reflect.Pointer {
		return false
	}
	e := rt.Elem()
	return e.Kind() == reflect.Struct && e.NumField() == 1 && e.Field(0).Name == "p" &&
		e.PkgPath() == jsonProxyPool[0].PkgPath() && strings.HasPrefix(e.Name(), "jsonProxy")
}

func proxyPtr(px reflect.Value) *unsafe.Pointer {
	return (*unsafe.Pointer)(px.UnsafePointer())
}

// storageOf returns a pointer to v's memory, copying v if not addressable.
func storageOf(v reflect.Value) unsafe.Pointer {
	if !v.CanAddr() {
		c := reflect.New(v.Type()).Elem()
		c.Set(v)
		v = c
	}
	return v.Addr().UnsafePointer()
}

// fieldAt returns field i of the struct v, addressable and settable when v
// is addressable.
func fieldAt(v reflect.Value, i int) reflect.Value {
	sf := v.Type().Field(i)
	if !v.CanAddr() {
		return v.Field(i)
	}
	return reflect.NewAt(sf.Type, unsafe.Add(v.Addr().UnsafePointer(), sf.Offset)).Elem()
}

func omitEmpty(tag reflect.StructTag) bool {
	_, opts, _ := strings.Cut(tag.Get("json"), ",")
	for _, o := range strings.Split(opts, ",") {
		if o == "omitempty" {
			return true
		}
	}
	return false
}

// toJSONMirror returns the mirror of v, of type t and mirror type mrt.
func (in *Interp) toJSONMirror(v reflect.Value, t types.Type, mrt reflect.Type, seen map[unsafe.Pointer]reflect.Value) reflect.Value {
	if v.Type() == mrt {
		return v
	}
	if isProxyPtr(mrt) && in.hasJSONMethods(t) {
		px := reflect.New(mrt.Elem())
		*proxyPtr(px) = storageOf(v)
		return px
	}
	u := types.Unalias(t).Underlying()
	out := reflect.New(mrt).Elem()
	switch mrt.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return out
		}
		if m, ok := seen[v.UnsafePointer()]; ok {
			return m
		}
		p := reflect.New(mrt.Elem())
		seen[v.UnsafePointer()] = p
		p.Elem().Set(in.toJSONMirror(v.Elem(), u.(*types.Pointer).Elem(), mrt.Elem(), seen))
		return p
	case reflect.Slice:
		if v.IsNil() {
			return out
		}
		et := u.(*types.Slice).Elem()
		out = reflect.MakeSlice(mrt, v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(in.toJSONMirror(v.Index(i), et, mrt.Elem(), seen))
		}
	case reflect.Array:
		et := u.(*types.Array).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(in.toJSONMirror(v.Index(i), et, mrt.Elem(), seen))
		}
	case reflect.Map:
		if v.IsNil() {
			return out
		}
		et := u.(*types.Map).Elem()
		out = reflect.MakeMapWithSize(mrt, v.Len())
		it := v.MapRange()
		for it.Next() {
			out.SetMapIndex(it.Key(), in.toJSONMirror(it.Value(), et, mrt.Elem(), seen))
		}
	case reflect.Struct:
		st := u.(*types.Struct)
		for i := 0; i < mrt.NumField(); i++ {
			mf := mrt.Field(i)
			if !mf.IsExported() {
				continue
			}
			rf := fieldAt(v, i)
			if isProxyPtr(mf.Type) && omitEmpty(mf.Tag) && rf.IsZero() {
				continue
			}
			out.Field(i).Set(in.toJSONMirror(rf, st.Field(i).Type(), mf.Type, seen))
		}
	}
	return out
}

// fromJSONMirror copies the mirror m back into dst (settable), of type t.
func (in *Interp) fromJSONMirror(m, dst reflect.Value, t types.Type, seen map[unsafe.Pointer]bool) {
	if m.Type() == dst.Type() {
		dst.Set(m)
		return
	}
	if isProxyPtr(m.Type()) && in.hasJSONMethods(t) {
		if m.IsNil() {
			return
		}
		p := *proxyPtr(m)
		if p != nil && (!dst.CanAddr() || p != dst.Addr().UnsafePointer()) {
			dst.Set(reflect.NewAt(dst.Type(), p).Elem())
		}
		return
	}
	u := types.Unalias(t).Underlying()
	switch m.Kind() {
	case reflect.Pointer:
		if m.IsNil() {
			dst.Set(reflect.Zero(dst.Type()))
			return
		}
		if seen[m.UnsafePointer()] {
			return
		}
		seen[m.UnsafePointer()] = true
		if dst.IsNil() {
			dst.Set(reflect.New(dst.Type().Elem()))
		}
		in.fromJSONMirror(m.Elem(), dst.Elem(), u.(*types.Pointer).Elem(), seen)
	case reflect.Slice:
		if m.IsNil() {
			dst.Set(reflect.Zero(dst.Type()))
			return
		}
		et := u.(*types.Slice).Elem()
		out := reflect.MakeSlice(dst.Type(), m.Len(), m.Len())
		for i := 0; i < m.Len(); i++ {
			in.fromJSONMirror(m.Index(i), out.Index(i), et, seen)
		}
		dst.Set(out)
	case reflect.Array:
		et := u.(*types.Array).Elem()
		for i := 0; i < m.Len(); i++ {
			in.fromJSONMirror(m.Index(i), dst.Index(i), et, seen)
		}
	case reflect.Map:
		if m.IsNil() {
			dst.Set(reflect.Zero(dst.Type()))
			return
		}
		et := u.(*types.Map).Elem()
		out := reflect.MakeMapWithSize(dst.Type(), m.Len())
		it := m.MapRange()
		for it.Next() {
			val := reflect.New(dst.Type().Elem()).Elem()
			in.fromJSONMirror(it.Value(), val, et, seen)
			out.SetMapIndex(it.Key(), val)
		}
		dst.Set(out)
	case reflect.Struct:
		st := u.(*types.Struct)
		for i := 0; i < m.NumField(); i++ {
			if !m.Type().Field(i).IsExported() {
				continue
			}
			in.fromJSONMirror(m.Field(i), fieldAt(dst, i), st.Field(i).Type(), seen)
		}
	}
}

// jsonMirrorArg returns the mirror to pass to a compiled encoder for the
// value v of type t, and for decoding calls a function copying it back.
func (in *Interp) jsonMirrorArg(v reflect.Value, t types.Type, decode bool) (reflect.Value, func(), bool) {
	mrt := in.jsonMirrorRT(t)
	if mrt == nil {
		return reflect.Value{}, nil, false
	}
	m := in.toJSONMirror(v, t, mrt, map[unsafe.Pointer]reflect.Value{})
	var back func()
	if decode && v.Kind() == reflect.Pointer && !v.IsNil() {
		et := types.Unalias(t).Underlying().(*types.Pointer).Elem()
		back = func() {
			if !m.IsNil() {
				in.fromJSONMirror(m.Elem(), v.Elem(), et, map[unsafe.Pointer]bool{})
			}
		}
	}
	return m, back, true
}

// marshalJSONValue encodes v, of interpreted type t, with its mirror.
func (in *Interp) marshalJSONValue(v reflect.Value, t types.Type) ([]byte, error) {
	if mrt := in.jsonMirrorRT(t); mrt != nil {
		m := in.toJSONMirror(v, t, mrt, map[unsafe.Pointer]reflect.Value{})
		return json.Marshal(m.Interface())
	}
	return json.Marshal(v.Interface())
}

func (in *Interp) callJSONMethod(m *methodImpl, recv reflect.Value, args ...reflect.Value) []reflect.Value {
	return in.invoke(m, recv, args, false, nil)
}

func errorResult(v reflect.Value) error {
	if !v.IsValid() || v.Kind() == reflect.Interface && v.IsNil() {
		return nil
	}
	if e, ok := v.Interface().(error); ok {
		return e
	}
	return nil
}

func proxyMarshalJSON(slot int, p unsafe.Pointer) ([]byte, error) {
	s := jsonSlotOf(slot)
	in, t := s.in, s.t
	rt := in.rtype(t)
	if p == nil {
		p = reflect.New(rt).UnsafePointer()
	}
	ptr := reflect.NewAt(rt, p)
	if m := in.jsonMethod(t, "MarshalJSON"); m != nil {
		res := in.callJSONMethod(m, ptr)
		b, _ := res[0].Interface().([]byte)
		return b, errorResult(res[1])
	}
	if m := in.jsonMethod(t, "MarshalText"); m != nil {
		res := in.callJSONMethod(m, ptr)
		if err := errorResult(res[1]); err != nil {
			return nil, err
		}
		b, _ := res[0].Interface().([]byte)
		return json.Marshal(string(b))
	}
	// Only decoding methods: encode the underlying value.
	return in.marshalJSONValue(ptr.Elem(), t.Underlying())
}

func proxyUnmarshalJSON(slot int, pp *unsafe.Pointer, b []byte) error {
	s := jsonSlotOf(slot)
	in, t := s.in, s.t
	rt := in.rtype(t)
	if *pp == nil {
		*pp = reflect.New(rt).UnsafePointer()
	}
	ptr := reflect.NewAt(rt, *pp)
	if m := in.jsonMethod(t, "UnmarshalJSON"); m != nil {
		res := in.callJSONMethod(m, ptr, reflect.ValueOf(b))
		return errorResult(res[0])
	}
	if m := in.jsonMethod(t, "UnmarshalText"); m != nil {
		if string(b) == "null" {
			return nil
		}
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return fmt.Errorf("json: cannot unmarshal %s into Go value of type %s", b, t)
		}
		res := in.callJSONMethod(m, ptr, reflect.ValueOf([]byte(str)))
		return errorResult(res[0])
	}
	// Only encoding methods: decode the underlying value.
	u := t.Underlying()
	if mrt := in.jsonMirrorRT(types.NewPointer(u)); mrt != nil {
		v := reflect.NewAt(in.rtype(u), *pp)
		m, back, _ := in.jsonMirrorArg(v, types.NewPointer(u), true)
		if err := json.Unmarshal(b, m.Interface()); err != nil {
			return err
		}
		if back != nil {
			back()
		}
		return nil
	}
	return json.Unmarshal(b, reflect.NewAt(in.rtype(u), *pp).Interface())
}
