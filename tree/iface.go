package tree

import (
	"encoding/json"
	"fmt"
	"go/types"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/darkweak/wy/tree/native"
)

// Interface values
//
// A Go interface holds a dynamic type and a value. When the dynamic type is
// faithfully represented by its reflect type (native types, and unnamed
// types built only from them), the interface holds the plain value. When
// it is not (an interpreted named type, whose reflect type is just its
// underlying type), the interface holds a boxed value that carries the
// interpreter's type. boxed is comparable whenever the value is, so boxed
// values work as map keys and compare like Go interfaces.
//
// Native interface types (error, io.Reader, ...) cannot hold a boxed value,
// so interpreted values stored in them are wrapped in generated types that
// implement exactly the interpreted type's methods.

type ntype struct {
	in       *Interp
	t        types.Type
	rt       reflect.Type
	faithful bool
	name     string

	mu       sync.Mutex
	methods  map[string]*methodImpl
	impl     map[types.Type]bool
	combo    reflect.Type
	comboSet bool
	families map[reflect.Type]reflect.Type // native.Family base -> member
}

type boxed struct {
	t *ntype
	v any
}

var boxedRT = reflect.TypeFor[boxed]()

// ntypeTable interns dynamic types: identical types (in the go/types
// sense, so byte and uint8, or func(x int) and func(int), are one type)
// share an ntype, which makes type assertions pointer comparisons.
type ntypeTable struct {
	in    *Interp
	mu    sync.Mutex
	exact map[types.Type]*ntype
	byRT  map[reflect.Type][]*ntype
}

func (tt *ntypeTable) get(t types.Type) *ntype {
	tt.mu.Lock()
	if nt, ok := tt.exact[t]; ok {
		tt.mu.Unlock()
		return nt
	}
	tt.mu.Unlock()
	rt := tt.in.rtype(t)
	tt.mu.Lock()
	for _, nt := range tt.byRT[rt] {
		if types.Identical(nt.t, t) {
			tt.exact[t] = nt
			tt.mu.Unlock()
			return nt
		}
	}
	tt.mu.Unlock()
	back := tt.in.typeOfRT(rt)
	nt := &ntype{in: tt.in, t: t, rt: rt, faithful: types.Identical(back, t), name: typeName(t)}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if tt.exact == nil {
		tt.exact = map[types.Type]*ntype{}
		tt.byRT = map[reflect.Type][]*ntype{}
	}
	for _, other := range tt.byRT[rt] {
		if types.Identical(other.t, t) {
			tt.exact[t] = other
			return other
		}
	}
	tt.byRT[rt] = append(tt.byRT[rt], nt)
	tt.exact[t] = nt
	return nt
}

// typeName spells t the way %T and the runtime would in compiled Go
// (main.point, interface {}).
func typeName(t types.Type) string {
	s := types.TypeString(unaliasAll(t), func(p *types.Package) string { return p.Name() })
	s = strings.ReplaceAll(s, "interface{}", "interface {}")
	return anyWord.ReplaceAllString(s, "${1}interface {}")
}

// anyWord matches the predeclared any (not a qualified name like pkg.any).
var anyWord = regexp.MustCompile(`(^|[^.\w])any\b`)

// unaliasAll removes the "any" alias at the top level, which the runtime
// spells "interface {}".
func unaliasAll(t types.Type) types.Type {
	if a, ok := t.(*types.Alias); ok {
		return types.Unalias(a)
	}
	return t
}

func (in *Interp) typeOfRT(rt reflect.Type) types.Type {
	in.rtMu.Lock()
	defer in.rtMu.Unlock()
	return in.natives.typeOf(rt)
}

func isInterface(t types.Type) bool {
	_, ok := t.Underlying().(*types.Interface)
	return ok
}

// dyn returns the dynamic type and value of an interface value; nt is nil
// for a nil interface.
func (in *Interp) dyn(iv reflect.Value) (*ntype, reflect.Value) {
	if iv.IsNil() {
		return nil, reflect.Value{}
	}
	return in.dynOf(iv.Elem())
}

func (in *Interp) dynOf(e reflect.Value) (*ntype, reflect.Value) {
	if e.Type() == boxedRT {
		b := e.Interface().(boxed)
		return b.t, boxValue(b)
	}
	if native.IsWrapper(e.Type()) {
		if b, ok := e.Field(0).Interface().(boxed); ok {
			return b.t, boxValue(b)
		}
	}
	return in.ntypes.get(in.typeOfRT(e.Type())), e
}

func boxValue(b boxed) reflect.Value {
	if b.v == nil {
		return reflect.Zero(b.t.rt)
	}
	return reflect.ValueOf(b.v)
}

// ifaceFrom stores a value of dynamic type nt in an interface of type dst.
func (in *Interp) ifaceFrom(nt *ntype, v reflect.Value, dst reflect.Type) reflect.Value {
	out := reflect.New(dst).Elem()
	if nt.faithful {
		out.Set(v)
		return out
	}
	b := boxed{nt, v.Interface()}
	if dst == anyRT {
		out.Set(reflect.ValueOf(b))
		return out
	}
	out.Set(in.wrapFor(b, dst))
	return out
}

// toIface converts a value of static type src to interface type dst.
func (in *Interp) toIface(v reflect.Value, src types.Type, dst reflect.Type) reflect.Value {
	if isInterface(src) {
		if v.IsNil() {
			return reflect.Zero(dst)
		}
		if v.Type() == dst {
			return v
		}
		nt, e := in.dyn(v)
		return in.ifaceFrom(nt, e, dst)
	}
	return in.ifaceFrom(in.ntypes.get(src), v, dst)
}

// --- method lookup -----------------------------------------------------

type methodImpl struct {
	sel *types.Selection
	fn  *function // interpreted method; nil for a compiled one
	sig *types.Signature
}

// method finds the method name of nt's method set (nil if absent).
func (nt *ntype) method(name string, pkg *types.Package) *methodImpl {
	key := name
	if pkg != nil && !isExported(name) {
		key = pkg.Path() + "." + name
	}
	nt.mu.Lock()
	if m, ok := nt.methods[key]; ok {
		nt.mu.Unlock()
		return m
	}
	nt.mu.Unlock()
	sel := types.NewMethodSet(nt.t).Lookup(pkg, name)
	var m *methodImpl
	if sel != nil {
		m = nt.in.methodImpl(sel)
	}
	nt.mu.Lock()
	if nt.methods == nil {
		nt.methods = map[string]*methodImpl{}
	}
	nt.methods[key] = m
	nt.mu.Unlock()
	return m
}

func isExported(name string) bool { return name != "" && strings.ToUpper(name[:1]) == name[:1] }

func (in *Interp) methodImpl(sel *types.Selection) *methodImpl {
	obj := sel.Obj().(*types.Func)
	m := &methodImpl{sel: sel, sig: obj.Type().(*types.Signature)}
	if _, src := in.funcDecls[obj.Origin()]; src {
		in.mu.Lock()
		m.fn = in.funcFor(obj, recvTypeArgs(obj))
		in.mu.Unlock()
	}
	return m
}

func (in *Interp) methodImplLocked(sel *types.Selection) *methodImpl {
	obj := sel.Obj().(*types.Func)
	m := &methodImpl{sel: sel, sig: obj.Type().(*types.Signature)}
	if _, src := in.funcDecls[obj.Origin()]; src {
		m.fn = in.funcFor(obj, recvTypeArgs(obj))
	}
	return m
}

// recvTypeArgs returns the type arguments of a method's receiver type.
func recvTypeArgs(obj *types.Func) []types.Type {
	recv := obj.Type().(*types.Signature).Recv()
	if recv == nil {
		return nil
	}
	t := recv.Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	if !ok || n.TypeArgs() == nil {
		return nil
	}
	out := make([]types.Type, n.TypeArgs().Len())
	for i := range out {
		out[i] = n.TypeArgs().At(i)
	}
	return out
}

// invoke calls method m on receiver v (of the selection's receiver type).
func (in *Interp) invoke(m *methodImpl, v reflect.Value, args []reflect.Value, spread bool, deferOf *frame) []reflect.Value {
	return in.invokeRecv(m, in.selectRecv(v, m.sel), args, spread, deferOf)
}

// invokeRecv calls method m on a receiver already selected by selectRecv.
func (in *Interp) invokeRecv(m *methodImpl, recv reflect.Value, args []reflect.Value, spread bool, deferOf *frame) []reflect.Value {
	if m.fn == nil && recv.Kind() == reflect.Interface {
		// A method promoted from an embedded interface: dispatch on the
		// interface's dynamic value.
		nt, v := in.dyn(recv)
		if nt == nil {
			panic(runtimeError("invalid memory address or nil pointer dereference"))
		}
		inner := nt.method(m.sel.Obj().Name(), m.sel.Obj().Pkg())
		if inner == nil {
			panic(fmt.Sprintf("wy: %s has no method %s", nt.name, m.sel.Obj().Name()))
		}
		return in.invoke(inner, v, args, spread, deferOf)
	}
	if m.fn != nil {
		all := make([]reflect.Value, 0, len(args)+1)
		all = append(all, recv)
		all = append(all, args...)
		if m.sig.Variadic() && !spread {
			all = packVariadic(m.fn.paramTypes, all)
		}
		return m.fn.call(nil, all, deferOf)
	}
	name := m.sel.Obj().Name()
	if recv.Type() == runtimeFuncPtr && !recv.IsNil() {
		if res, ok := funcMethod(recv.Interface().(*runtime.Func), name, args); ok {
			return res
		}
	}
	mv := recv.MethodByName(name)
	if !mv.IsValid() && recv.CanAddr() {
		mv = recv.Addr().MethodByName(name)
	}
	if !mv.IsValid() {
		panic(fmt.Sprintf("wy: method %s not found on %s", name, recv.Type()))
	}
	mode := modeRaw
	if p := m.sel.Obj().Pkg(); p != nil {
		mode = modeForPackage(p.Path())
	}
	return in.callNativeMode(mv, m.sig, args, spread, mode)
}

// selectRecv walks the embedding path of a method selection and adjusts
// the receiver to the method's pointer-ness.
func (in *Interp) selectRecv(v reflect.Value, sel *types.Selection) reflect.Value {
	path := sel.Index()
	t := sel.Recv()
	for _, idx := range path[:len(path)-1] {
		if p, ok := t.Underlying().(*types.Pointer); ok {
			v = derefChecked(v)
			t = p.Elem()
		}
		st := t.Underlying().(*types.Struct)
		f := st.Field(idx)
		v = in.fieldOf(v, st, idx)
		t = f.Type()
	}
	obj := sel.Obj().(*types.Func)
	recvT := obj.Type().(*types.Signature).Recv().Type()
	_, wantPtr := recvT.(*types.Pointer)
	_, havePtr := t.Underlying().(*types.Pointer)
	if _, isIface := t.Underlying().(*types.Interface); isIface {
		return v
	}
	switch {
	case wantPtr && !havePtr:
		if !v.CanAddr() {
			c := reflect.New(v.Type())
			c.Elem().Set(v)
			return c
		}
		return v.Addr()
	case !wantPtr && havePtr:
		return derefChecked(v)
	}
	return v
}

func derefChecked(v reflect.Value) reflect.Value {
	if v.IsNil() {
		panic(runtimeError("invalid memory address or nil pointer dereference"))
	}
	return v.Elem()
}

// implements reports whether nt implements interface t (cached).
func (nt *ntype) implements(t types.Type) bool {
	nt.mu.Lock()
	if ok, done := nt.impl[t]; done {
		nt.mu.Unlock()
		return ok
	}
	nt.mu.Unlock()
	iface := t.Underlying().(*types.Interface)
	ok := types.Implements(nt.t, iface)
	if !ok && isNativeType(nt.t) {
		// A native type seen through reflection has only its exported
		// methods; accept a match on names for interfaces declared in
		// interpreted code that mention compiled types.
		ok = types.AssignableTo(nt.t, t)
	}
	nt.mu.Lock()
	if nt.impl == nil {
		nt.impl = map[types.Type]bool{}
	}
	nt.impl[t] = ok
	nt.mu.Unlock()
	return ok
}

func isNativeType(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Scope().Lookup(n.Obj().Name()) == n.Obj() && false
}

// --- crossing into compiled code ---------------------------------------

// comboKeys lists the optional methods (see native.Combos) nt has.
func (nt *ntype) comboType() reflect.Type {
	nt.mu.Lock()
	if nt.comboSet {
		defer nt.mu.Unlock()
		return nt.combo
	}
	nt.mu.Unlock()
	ms := types.NewMethodSet(nt.t)
	var keys []string
	check := func(name string, nparams, nresults int) *types.Signature {
		sel := ms.Lookup(nil, name)
		if sel == nil {
			return nil
		}
		sig := sel.Obj().Type().(*types.Signature)
		if sig.Params().Len() != nparams || sig.Results().Len() != nresults {
			return nil
		}
		return sig
	}
	isStr := func(t types.Type) bool {
		return types.Identical(t.Underlying(), types.Typ[types.String]) && t == types.Typ[types.String]
	}
	if s := check("String", 0, 1); s != nil && isStr(s.Results().At(0).Type()) {
		keys = append(keys, "String")
	}
	if s := check("GoString", 0, 1); s != nil && isStr(s.Results().At(0).Type()) {
		keys = append(keys, "GoString")
	}
	if s := check("Format", 2, 0); s != nil {
		keys = append(keys, "Format")
	}
	if s := check("MarshalJSON", 0, 2); s != nil {
		keys = append(keys, "MarshalJSON")
	}
	if s := check("MarshalText", 0, 2); s != nil {
		keys = append(keys, "MarshalText")
	}
	if s := check("Error", 0, 1); s != nil && isStr(s.Results().At(0).Type()) {
		keys = append(keys, "Error")
		if u := check("Unwrap", 0, 1); u != nil {
			if _, multi := u.Results().At(0).Type().(*types.Slice); multi {
				keys = append(keys, "UnwrapMulti")
			} else {
				keys = append(keys, "Unwrap")
			}
		}
		if s := check("Is", 1, 1); s != nil {
			keys = append(keys, "Is")
		}
		if s := check("As", 1, 1); s != nil {
			keys = append(keys, "As")
		}
	}
	var combo reflect.Type
	if len(keys) > 0 {
		combo = comboIndex()[strings.Join(keys, "_")]
	}
	nt.mu.Lock()
	nt.combo, nt.comboSet = combo, true
	nt.mu.Unlock()
	return combo
}

var (
	comboOnce sync.Once
	combos    map[string]reflect.Type
	wrapMu    sync.Mutex
	wrapIdx   map[reflect.Type]reflect.Type
	wrapPkgs  int
)

func comboIndex() map[string]reflect.Type {
	comboOnce.Do(func() {
		combos = map[string]reflect.Type{}
		for _, c := range native.Combos {
			combos[strings.Join(c.Methods, "_")] = c.Type
		}
	})
	return combos
}

// wrapperIndex maps native interface types to their wrappers, rebuilt
// when packages are registered.
func wrapperIndex() map[reflect.Type]reflect.Type {
	wrapMu.Lock()
	defer wrapMu.Unlock()
	paths := native.Paths()
	if wrapIdx != nil && len(paths) == wrapPkgs {
		return wrapIdx
	}
	idx := map[reflect.Type]reflect.Type{}
	sort.Strings(paths)
	for _, p := range paths {
		np := native.Lookup(p)
		for name, w := range np.Wrappers {
			if it := np.Types[name]; it != nil {
				idx[it] = w
			}
		}
	}
	wrapIdx, wrapPkgs = idx, len(paths)
	return idx
}

// familyType returns the member of f matching the optional methods nt has.
func (nt *ntype) familyType(f *native.Family) reflect.Type {
	nt.mu.Lock()
	wt, ok := nt.families[f.Base]
	nt.mu.Unlock()
	if ok {
		return wt
	}
	ms := types.NewMethodSet(nt.t)
	var mask uint
	for i, opt := range f.Optional {
		m := opt.Method(0)
		if sel := ms.Lookup(nil, m.Name); sel != nil && nt.in.rtype(sel.Obj().Type()) == m.Type {
			mask |= 1 << i
		}
	}
	wt = f.Members[mask]
	nt.mu.Lock()
	if nt.families == nil {
		nt.families = map[reflect.Type]reflect.Type{}
	}
	nt.families[f.Base] = wt
	nt.mu.Unlock()
	return wt
}

// wrapFor wraps a boxed value so that it implements the native interface
// type dst.
func (in *Interp) wrapFor(b boxed, dst reflect.Type) reflect.Value {
	if f := native.FamilyOf(dst); f != nil {
		if wt := b.t.familyType(f); wt != nil {
			return newWrapper(wt, b)
		}
	}
	if wt := b.t.comboType(); wt != nil && wt.Implements(dst) {
		return newWrapper(wt, b)
	}
	if wt, ok := wrapperIndex()[dst]; ok {
		return newWrapper(wt, b)
	}
	panic(fmt.Sprintf("wy: cannot pass %s to compiled code as %s (no wrapper for that interface; "+
		"generate one with `wy extract`)", b.t.name, dst))
}

func newWrapper(wt reflect.Type, b boxed) reflect.Value {
	w := reflect.New(wt).Elem()
	w.Field(0).Set(reflect.ValueOf(b))
	return w
}

// toNative prepares an interpreter value of static type st for a compiled
// function expecting target.
func (in *Interp) toNative(v reflect.Value, st types.Type, target reflect.Type) reflect.Value {
	return in.toNativeMode(v, st, target, modeContainer)
}

func (in *Interp) toNativeMode(v reflect.Value, st types.Type, target reflect.Type, mode argMode) reflect.Value {
	if isInterface(st) {
		if v.Kind() == reflect.Interface && !v.IsNil() && v.Elem().Type() == boxedRT {
			b := v.Elem().Interface().(boxed)
			out := reflect.New(target).Elem()
			out.Set(in.boxToNativeMode(b, target, mode))
			return out
		}
		if v.Type() != target {
			out := reflect.New(target).Elem()
			if !v.IsNil() {
				out.Set(v.Elem())
			}
			return out
		}
		return v
	}
	if needsDeep(st) {
		v = in.deepNative(v, st)
	}
	if v.Type() != target {
		if v.Type().ConvertibleTo(target) {
			return v.Convert(target)
		}
		if v.Type().Size() == target.Size() {
			return reinterpret(v, target)
		}
	}
	return v
}

// boxToNative chooses how compiled code sees a boxed value: through a
// wrapper exposing its methods if it has any of interest; as a plain
// pointer if it is one (so reflection-based code such as json.Unmarshal
// can fill it); otherwise still boxed, which prints and marshals like the
// value itself and comes back intact.
func (in *Interp) boxToNative(b boxed, target reflect.Type) reflect.Value {
	return in.boxToNativeMode(b, target, modeContainer)
}

func (in *Interp) boxToNativeMode(b boxed, target reflect.Type, mode argMode) reflect.Value {
	if target != anyRT {
		if wt := b.t.comboType(); wt != nil && wt.Implements(target) {
			return newWrapper(wt, b)
		}
		return in.wrapFor(b, target)
	}
	switch mode {
	case modeContainer:
		if wt := b.t.comboType(); wt != nil {
			return newWrapper(wt, b)
		}
		return reflect.ValueOf(b)
	case modeEncode:
		if wt := b.t.comboType(); wt != nil {
			for _, m := range []string{"MarshalJSON", "MarshalText"} {
				if _, ok := wt.MethodByName(m); ok {
					return newWrapper(wt, b)
				}
			}
		}
	case modeDisplay:
		return in.display(boxValue(b), b.t.t, 0)
	}
	v := boxValue(b)
	if !v.IsValid() {
		return reflect.Zero(anyRT)
	}
	return v
}

func needsDeep(t types.Type) bool {
	switch u := t.Underlying().(type) {
	case *types.Slice:
		return isInterface(u.Elem()) || needsDeep(u.Elem())
	case *types.Array:
		return isInterface(u.Elem()) || needsDeep(u.Elem())
	case *types.Map:
		return isInterface(u.Elem()) || isInterface(u.Key())
	}
	return false
}

// deepNative copies containers whose interface elements hold boxed
// values, converting those elements for compiled code.
func (in *Interp) deepNative(v reflect.Value, st types.Type) reflect.Value {
	switch u := st.Underlying().(type) {
	case *types.Slice, *types.Array:
		var et types.Type
		if s, ok := u.(*types.Slice); ok {
			et = s.Elem()
			if v.IsNil() {
				return v
			}
		} else {
			et = u.(*types.Array).Elem()
		}
		if !containsBoxed(v) {
			return v
		}
		var out reflect.Value
		if v.Kind() == reflect.Slice {
			out = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		} else {
			out = reflect.New(v.Type()).Elem()
		}
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(in.toNative(v.Index(i), et, v.Type().Elem()))
		}
		return out
	case *types.Map:
		if v.IsNil() || !containsBoxed(v) {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(in.toNative(iter.Key(), u.Key(), v.Type().Key()), in.toNative(iter.Value(), u.Elem(), v.Type().Elem()))
		}
		return out
	}
	return v
}

func containsBoxed(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Interface:
		return !v.IsNil() && (v.Elem().Type() == boxedRT || containsBoxed(v.Elem()))
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if containsBoxed(v.Index(i)) {
				return true
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if containsBoxed(iter.Key()) || containsBoxed(iter.Value()) {
				return true
			}
		}
	}
	return false
}

// fromNative converts a value produced by compiled code to the canonical
// representation of static type st.
func (in *Interp) fromNative(v reflect.Value, st types.Type) reflect.Value {
	want := in.rtype(st)
	if !v.IsValid() {
		return reflect.Zero(want)
	}
	if v.Type() == want {
		return v
	}
	if want.Kind() == reflect.Interface {
		out := reflect.New(want).Elem()
		if v.Kind() == reflect.Interface {
			if !v.IsNil() {
				out.Set(v.Elem())
			}
		} else {
			out.Set(v)
		}
		return out
	}
	if v.Type().ConvertibleTo(want) {
		return v.Convert(want)
	}
	if v.Type().Size() == want.Size() {
		return reinterpret(v, want) // a self-referential type's stand-in
	}
	return v
}

// Call implements native.Caller: compiled code calling a method of an
// interpreted value through a wrapper.
func (b boxed) Call(method string, args []any) []any {
	name := method
	if name == "UnwrapMulti" {
		name = "Unwrap"
	}
	in := b.t.in
	m := b.t.method(name, nil)
	if m == nil {
		panic(fmt.Sprintf("wy: %s has no method %s", b.t.name, name))
	}
	params := m.sig.Params()
	vals := make([]reflect.Value, len(args))
	for i, a := range args {
		pt := paramType(m.sig, i)
		if m.sig.Variadic() && i == params.Len()-1 {
			pt = params.At(i).Type()
		}
		if a == nil {
			vals[i] = reflect.Zero(in.rtype(pt))
		} else {
			vals[i] = in.fromNative(reflect.ValueOf(a), pt)
		}
	}
	res := in.invoke(m, boxValue(b), vals, m.sig.Variadic(), nil)
	out := make([]any, len(res))
	for i, r := range res {
		rt := m.sig.Results().At(i).Type()
		r = in.toNative(r, rt, in.rtype(rt))
		if r.Kind() == reflect.Interface {
			if r.IsNil() {
				continue
			}
			out[i] = r.Elem().Interface()
			continue
		}
		out[i] = r.Interface()
	}
	return out
}

// Format makes a boxed value that reached compiled code print like the
// value itself.
func (b boxed) Format(f fmt.State, verb rune) {
	if verb == 'v' || verb == 's' {
		for _, name := range []string{"Error", "String"} {
			if m := b.t.method(name, nil); m != nil && m.sig.Params().Len() == 0 && m.sig.Results().Len() == 1 {
				res := b.t.in.invoke(m, boxValue(b), nil, false, nil)
				fmt.Fprint(f, res[0].String())
				return
			}
		}
	}
	fmt.Fprintf(f, fmt.FormatString(f, verb), b.v)
}

// MarshalJSON makes a boxed value encode like the value itself.
func (b boxed) MarshalJSON() ([]byte, error) { return json.Marshal(b.v) }

// String returns the type name and value, for debugging.
func (b boxed) GoString() string { return fmt.Sprintf("%s(%#v)", b.t.name, b.v) }
