package tree

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"math"
	"reflect"
	"unicode/utf8"
	"unsafe"
)

// runtimeError is a run-time panic raised by the interpreter itself, with
// the same message the Go runtime would print.
type runtimeError string

func (e runtimeError) Error() string { return "runtime error: " + string(e) }
func (e runtimeError) RuntimeError() {}

// expr compiles an expression producing a single value in the canonical
// representation of its type.
func (c *compiler) expr(e ast.Expr) eval {
	tv := c.info.Types[e]
	if tv.Value != nil {
		v := c.constValue(tv.Value, c.typeOf(e))
		return func(*frame) reflect.Value { return v }
	}
	switch e := e.(type) {
	case *ast.ParenExpr:
		return c.expr(e.X)
	case *ast.Ident:
		return c.ident(e)
	case *ast.BasicLit:
		panic("wy: non-constant literal")
	case *ast.FuncLit:
		return c.funcLit(e)
	case *ast.CompositeLit:
		return c.compositeLit(e, c.typeOf(e))
	case *ast.SelectorExpr:
		return c.selector(e)
	case *ast.IndexExpr:
		return c.index(e)
	case *ast.IndexListExpr:
		return c.genericFuncValue(e.X)
	case *ast.SliceExpr:
		return c.sliceExpr(e)
	case *ast.StarExpr:
		x := c.expr(e.X)
		if view := viewer(c.in.rtype(c.typeOf(e.X)).Elem(), c.in.rtype(c.typeOf(e))); view != nil {
			return func(fr *frame) reflect.Value { return view(derefChecked(x(fr))) }
		}
		return func(fr *frame) reflect.Value { return derefChecked(x(fr)) }
	case *ast.UnaryExpr:
		return c.unary(e)
	case *ast.BinaryExpr:
		return c.binary(e)
	case *ast.CallExpr:
		return c.call1(e)
	case *ast.TypeAssertExpr:
		f := c.typeAssert(e, false)
		return func(fr *frame) reflect.Value { return f(fr)[0] }
	case *ast.KeyValueExpr:
		panic("wy: unexpected key/value expression")
	}
	panic(fmt.Sprintf("wy: unsupported expression %T at %s", e, c.pos(e)))
}

// constValue materializes a constant as a value of type t.
func (c *compiler) constValue(val constant.Value, t types.Type) reflect.Value {
	if b, ok := t.Underlying().(*types.Basic); ok && b.Info()&types.IsUntyped != 0 {
		t = types.Default(t)
	}
	rt := c.in.rtype(t)
	if rt.Kind() == reflect.Interface {
		// A constant converted to an interface: box its default type.
		dt := types.Default(c.untypedOf(val))
		return c.in.toIface(c.constValue(val, dt), dt, rt)
	}
	v := reflect.New(rt).Elem()
	switch rt.Kind() {
	case reflect.Bool:
		v.SetBool(constant.BoolVal(val))
	case reflect.String:
		v.SetString(constant.StringVal(val))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		x, _ := constant.Int64Val(constant.ToInt(val))
		v.SetInt(x)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		x, _ := constant.Uint64Val(constant.ToInt(val))
		v.SetUint(x)
	case reflect.Float32, reflect.Float64:
		x, _ := constant.Float64Val(constant.ToFloat(val))
		v.SetFloat(x)
	case reflect.Complex64, reflect.Complex128:
		re, _ := constant.Float64Val(constant.ToFloat(constant.Real(val)))
		im, _ := constant.Float64Val(constant.ToFloat(constant.Imag(val)))
		v.SetComplex(complex(re, im))
	default:
		panic(fmt.Sprintf("wy: constant of type %s", t))
	}
	return v
}

func (c *compiler) untypedOf(val constant.Value) types.Type {
	switch val.Kind() {
	case constant.Bool:
		return types.Typ[types.UntypedBool]
	case constant.String:
		return types.Typ[types.UntypedString]
	case constant.Int:
		return types.Typ[types.UntypedInt]
	case constant.Float:
		return types.Typ[types.UntypedFloat]
	}
	return types.Typ[types.UntypedComplex]
}

// --- identifiers and variables -----------------------------------------

func (c *compiler) ident(id *ast.Ident) eval {
	obj := c.info.Uses[id]
	if obj == nil {
		obj = c.info.Defs[id]
	}
	switch o := obj.(type) {
	case *types.Nil:
		t := c.typeOf(id)
		if b, ok := t.(*types.Basic); ok && b.Kind() == types.UntypedNil {
			return func(*frame) reflect.Value { return reflect.Value{} }
		}
		z := reflect.Zero(c.in.rtype(t))
		return func(*frame) reflect.Value { return z }
	case *types.Var:
		return c.varRef(o)
	case *types.Func:
		return c.funcRef(o, c.instanceArgs(id))
	case *types.Const:
		v := c.constValue(o.Val(), c.typeOf(id))
		return func(*frame) reflect.Value { return v }
	}
	panic(fmt.Sprintf("wy: unsupported identifier %s (%T) at %s", id.Name, obj, c.pos(id)))
}

func (c *compiler) instanceArgs(id *ast.Ident) []types.Type {
	inst, ok := c.info.Instances[id]
	if !ok || inst.TypeArgs == nil {
		return nil
	}
	out := make([]types.Type, inst.TypeArgs.Len())
	for i := range out {
		out[i] = c.subst.typ(inst.TypeArgs.At(i))
	}
	return out
}

// varRef returns the storage of a variable (addressable).
func (c *compiler) varRef(o *types.Var) eval {
	if s, ok := c.fs.slots[o]; ok {
		return func(fr *frame) reflect.Value { return fr.vars[s] }
	}
	if i, ok := c.fs.captureIndex(o); ok {
		return func(fr *frame) reflect.Value { return fr.env[i] }
	}
	if g, ok := c.in.globals[o]; ok {
		return func(*frame) reflect.Value { return g }
	}
	if o.Pkg() != nil {
		if np := c.in.nativePkg(o.Pkg().Path()); np != nil {
			if p, ok := np.Vars[o.Name()]; ok {
				v := p.Elem()
				want := c.in.rtype(o.Type())
				if v.Type() != want {
					v = reflect.NewAt(want, v.Addr().UnsafePointer()).Elem()
				}
				return func(*frame) reflect.Value { return v }
			}
		}
	}
	panic(fmt.Sprintf("wy: unknown variable %s", o.Name()))
}

// funcRef returns a function as a value.
func (c *compiler) funcRef(o *types.Func, targs []types.Type) eval {
	if o.Pkg() != nil {
		if np := c.in.nativePkg(o.Pkg().Path()); np != nil {
			if fv, ok := np.Funcs[o.Name()]; ok {
				return func(*frame) reflect.Value { return fv }
			}
		}
	}
	fn := c.in.funcFor(o, targs)
	var once reflect.Value
	in := c.in
	return func(*frame) reflect.Value {
		if !once.IsValid() {
			once = in.funcValue(&closure{fn: fn}, fn.rtOrSig())
		}
		return once
	}
}

func (f *function) rtOrSig() reflect.Type {
	if f.rt == nil {
		f.rt = f.in.rtype(f.sig)
	}
	return f.rt
}

func (in *Interp) nativePkg(path string) *nativeInfo { return in.nativeInfo(path) }

func (c *compiler) genericFuncValue(x ast.Expr) eval {
	var id *ast.Ident
	switch x := x.(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		id = x.Sel
	}
	if id != nil {
		if o, ok := c.info.Uses[id].(*types.Func); ok {
			return c.funcRef(o, c.instanceArgs(id))
		}
	}
	panic(fmt.Sprintf("wy: unsupported generic expression at %s", c.pos(x)))
}

// --- closures ------------------------------------------------------------

func (c *compiler) funcLit(e *ast.FuncLit) eval {
	sig := c.typeOf(e).(*types.Signature)
	f := &function{in: c.in, name: c.litName()}
	c.compileFunc(f, nil, e.Type, e.Body, sig, c.fs)
	inner := c.lastFuncState
	caps := inner.captures
	rt := c.in.rtype(sig)
	in := c.in
	return func(fr *frame) reflect.Value {
		env := make([]reflect.Value, len(caps))
		for i, cp := range caps {
			if cp.fromSlot >= 0 {
				env[i] = fr.vars[cp.fromSlot]
			} else {
				env[i] = fr.env[cp.fromCapture]
			}
		}
		return in.funcValue(&closure{fn: f, env: env}, rt)
	}
}

// litName names a function literal like the Go runtime: main.main.func1,
// and main.main.func1.1 for a literal inside a literal.
func (c *compiler) litName() string {
	c.fs.nlits++
	if c.fs.outer != nil {
		return fmt.Sprintf("%s.%d", c.fs.name, c.fs.nlits)
	}
	return fmt.Sprintf("%s.func%d", c.fs.name, c.fs.nlits)
}

// closureLit compiles a func literal that is called directly (defer, go,
// immediate call), avoiding the func-value round trip.
func (c *compiler) closureLit(e *ast.FuncLit) func(fr *frame) *closure {
	sig := c.typeOf(e).(*types.Signature)
	f := &function{in: c.in, name: c.litName()}
	c.compileFunc(f, nil, e.Type, e.Body, sig, c.fs)
	caps := c.lastFuncState.captures
	return func(fr *frame) *closure {
		env := make([]reflect.Value, len(caps))
		for i, cp := range caps {
			if cp.fromSlot >= 0 {
				env[i] = fr.vars[cp.fromSlot]
			} else {
				env[i] = fr.env[cp.fromCapture]
			}
		}
		return &closure{fn: f, env: env}
	}
}

// --- composite literals ----------------------------------------------------

func (c *compiler) compositeLit(e *ast.CompositeLit, t types.Type) eval {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		inner := c.compositeLit(e, p.Elem())
		return func(fr *frame) reflect.Value {
			v := inner(fr)
			if v.CanAddr() {
				return v.Addr()
			}
			n := reflect.New(v.Type())
			n.Elem().Set(v)
			return n
		}
	}
	rt := c.in.rtype(t)
	switch u := t.Underlying().(type) {
	case *types.Struct:
		type fieldInit struct {
			idx  int
			val  eval
			off  uintptr
			ftRT reflect.Type
		}
		var inits []fieldInit
		for i, elt := range e.Elts {
			idx := i
			valExpr := elt
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				name := kv.Key.(*ast.Ident).Name
				for j := 0; j < u.NumFields(); j++ {
					if u.Field(j).Name() == name {
						idx = j
					}
				}
				valExpr = kv.Value
			}
			ft := u.Field(idx).Type()
			inits = append(inits, fieldInit{idx, c.convertedExpr(valExpr, ft), rt.Field(idx).Offset, c.in.rtype(ft)})
		}
		return func(fr *frame) reflect.Value {
			v := reflect.New(rt).Elem()
			base := v.Addr().UnsafePointer()
			for _, fi := range inits {
				reflect.NewAt(fi.ftRT, unsafe.Add(base, fi.off)).Elem().Set(fi.val(fr))
			}
			return v
		}
	case *types.Array, *types.Slice:
		var et types.Type
		isSlice := false
		if a, ok := u.(*types.Array); ok {
			et = a.Elem()
		} else {
			et = u.(*types.Slice).Elem()
			isSlice = true
		}
		type elemInit struct {
			idx int
			val eval
		}
		var inits []elemInit
		next, max := 0, 0
		for _, elt := range e.Elts {
			valExpr := elt
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				k := c.constOf(kv.Key)
				i, _ := constant.Int64Val(constant.ToInt(k))
				next = int(i)
				valExpr = kv.Value
			}
			inits = append(inits, elemInit{next, c.elemExpr(valExpr, et)})
			next++
			if next > max {
				max = next
			}
		}
		est := rt.Elem()
		if isSlice {
			arrT := reflect.ArrayOf(max, rt.Elem())
			return func(fr *frame) reflect.Value {
				arr := reflect.New(arrT).Elem()
				for _, ei := range inits {
					arr.Index(ei.idx).Set(storeAs(est, ei.val(fr)))
				}
				return arr.Slice(0, max)
			}
		}
		return func(fr *frame) reflect.Value {
			v := reflect.New(rt).Elem()
			for _, ei := range inits {
				v.Index(ei.idx).Set(storeAs(est, ei.val(fr)))
			}
			return v
		}
	case *types.Map:
		type kv struct{ k, v eval }
		var kvs []kv
		for _, elt := range e.Elts {
			p := elt.(*ast.KeyValueExpr)
			kvs = append(kvs, kv{c.elemExpr(p.Key, u.Key()), c.elemExpr(p.Value, u.Elem())})
		}
		kst, vst := rt.Key(), rt.Elem()
		return func(fr *frame) reflect.Value {
			m := reflect.MakeMapWithSize(rt, len(kvs))
			for _, p := range kvs {
				m.SetMapIndex(storeAs(kst, p.k(fr)), storeAs(vst, p.v(fr)))
			}
			return m
		}
	}
	panic(fmt.Sprintf("wy: unsupported composite literal of type %s", t))
}

// elemExpr compiles an element of a composite literal, which may itself be
// an untyped composite literal (elided type).
func (c *compiler) elemExpr(e ast.Expr, t types.Type) eval {
	if cl, ok := e.(*ast.CompositeLit); ok && cl.Type == nil {
		return c.convertedLit(cl, t)
	}
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		if cl, ok := u.X.(*ast.CompositeLit); ok && cl.Type == nil {
			return c.convertedLit(cl, t)
		}
	}
	return c.convertedExpr(e, t)
}

func (c *compiler) convertedLit(cl *ast.CompositeLit, t types.Type) eval {
	inner := c.compositeLit(cl, c.typeOf(cl))
	conv := c.assignConv(c.typeOf(cl), t)
	return func(fr *frame) reflect.Value { return conv(inner(fr)) }
}

// convertedExpr compiles e and converts it for assignment to type t.
func (c *compiler) convertedExpr(e ast.Expr, t types.Type) eval {
	x := c.expr(e)
	conv := c.assignConv(c.typeOf(e), t)
	if conv == nil {
		return x
	}
	return func(fr *frame) reflect.Value { return conv(x(fr)) }
}

// assignConv returns the conversion applied when a value of type src is
// assigned to a location of type dst (interface boxing, nil, channel
// direction). The result is never nil.
func (c *compiler) assignConv(src, dst types.Type) func(reflect.Value) reflect.Value {
	return c.in.assignConv(src, dst)
}

func (in *Interp) assignConv(src, dst types.Type) func(reflect.Value) reflect.Value {
	drt := in.rtype(dst)
	if b, ok := src.(*types.Basic); ok && b.Kind() == types.UntypedNil {
		z := reflect.Zero(drt)
		return func(reflect.Value) reflect.Value { return z }
	}
	if isInterface(dst) {
		if isInterface(src) && in.rtype(src) == drt {
			return func(v reflect.Value) reflect.Value {
				if !v.IsValid() {
					return reflect.Zero(drt)
				}
				return v
			}
		}
		return func(v reflect.Value) reflect.Value {
			if !v.IsValid() {
				return reflect.Zero(drt)
			}
			return in.toIface(v, src, drt)
		}
	}
	srt := in.rtype(src)
	if srt == drt {
		return func(v reflect.Value) reflect.Value {
			if !v.IsValid() {
				return reflect.Zero(drt)
			}
			return v
		}
	}
	return func(v reflect.Value) reflect.Value {
		if !v.IsValid() {
			return reflect.Zero(drt)
		}
		if v.Type() == drt {
			return v
		}
		if !v.Type().ConvertibleTo(drt) {
			return reinterpret(v, drt)
		}
		return v.Convert(drt)
	}
}

// --- selectors -----------------------------------------------------------

func (c *compiler) selector(e *ast.SelectorExpr) eval {
	sel, ok := c.info.Selections[e]
	if !ok {
		// Qualified identifier: pkg.Name.
		return c.ident(e.Sel)
	}
	switch sel.Kind() {
	case types.FieldVal:
		return c.fieldPath(c.expr(e.X), c.typeOf(e.X), sel.Index())
	case types.MethodVal:
		return c.methodValue(e, sel)
	case types.MethodExpr:
		return c.methodExpr(e, sel)
	}
	panic("wy: unknown selection")
}

// fieldPath follows a field selection path from a value of type t,
// dereferencing pointers along the way. The result is addressable when the
// path goes through a pointer or starts from an addressable value.
func (c *compiler) fieldPath(x eval, t types.Type, path []int) eval {
	type step struct {
		deref bool
		off   uintptr
		ft    reflect.Type
	}
	var steps []step
	for _, idx := range path {
		deref := false
		if p, ok := t.Underlying().(*types.Pointer); ok {
			deref = true
			t = p.Elem()
		}
		st := t.Underlying().(*types.Struct)
		srt := c.in.rtype(t)
		ft := st.Field(idx).Type()
		steps = append(steps, step{deref, srt.Field(idx).Offset, c.in.rtype(ft)})
		t = ft
	}
	return func(fr *frame) reflect.Value {
		v := x(fr)
		for _, s := range steps {
			if s.deref {
				v = derefChecked(v)
			}
			if !v.CanAddr() {
				tmp := reflect.New(v.Type()).Elem()
				tmp.Set(v)
				v = tmp
			}
			v = reflect.NewAt(s.ft, unsafe.Add(v.Addr().UnsafePointer(), s.off)).Elem()
		}
		return v
	}
}

// fieldOf returns field i of struct value v (static type st).
func (in *Interp) fieldOf(v reflect.Value, st *types.Struct, i int) reflect.Value {
	if !v.CanAddr() {
		tmp := reflect.New(v.Type()).Elem()
		tmp.Set(v)
		v = tmp
	}
	off := v.Type().Field(i).Offset
	return reflect.NewAt(in.rtype(st.Field(i).Type()), unsafe.Add(v.Addr().UnsafePointer(), off)).Elem()
}

// fieldByIndex reads field i of a struct value without reflect's
// restrictions on unexported fields.
func fieldByIndex(v reflect.Value, i int) reflect.Value {
	if !v.CanAddr() {
		tmp := reflect.New(v.Type()).Elem()
		tmp.Set(v)
		v = tmp
	}
	f := v.Type().Field(i)
	return reflect.NewAt(f.Type, unsafe.Add(v.Addr().UnsafePointer(), f.Offset)).Elem()
}

// methodValue compiles x.M used as a value (bound method).
func (c *compiler) methodValue(e *ast.SelectorExpr, sel *types.Selection) eval {
	sig := c.typeOf(e).(*types.Signature)
	rt := c.in.rtype(sig)
	call := c.methodCaller(e, sel)
	in := c.in
	return func(fr *frame) reflect.Value {
		bound := call.bind(fr)
		f := in.builtinFunc(sel.Obj().Name(), sig, func(args []reflect.Value) []reflect.Value {
			return bound(args, sig.Variadic(), nil)
		})
		return in.funcValue(&closure{fn: f}, rt)
	}
}

// methodExpr compiles T.M used as a function value.
func (c *compiler) methodExpr(e *ast.SelectorExpr, sel *types.Selection) eval {
	sig := c.typeOf(e).(*types.Signature)
	rt := c.in.rtype(sig)
	recvT := c.subst.typ(sel.Recv())
	in := c.in
	m := c.staticMethod(sel, recvT)
	var once reflect.Value
	return func(fr *frame) reflect.Value {
		if once.IsValid() {
			return once
		}
		f := in.builtinFunc(sel.Obj().Name(), sig, func(args []reflect.Value) []reflect.Value {
			if isInterface(recvT) {
				nt, v := in.dyn(args[0])
				if nt == nil {
					panic(runtimeError("invalid memory address or nil pointer dereference"))
				}
				return in.invoke(nt.method(sel.Obj().Name(), sel.Obj().Pkg()), v, args[1:], sig.Variadic(), nil)
			}
			return in.invoke(m, args[0], args[1:], sig.Variadic(), nil)
		})
		once = in.funcValue(&closure{fn: f}, rt)
		return once
	}
}

// staticMethod resolves a method selection on a concrete type at compile
// time.
func (c *compiler) staticMethod(sel *types.Selection, recvT types.Type) *methodImpl {
	name := sel.Obj().Name()
	ms := types.NewMethodSet(recvT)
	s := ms.Lookup(sel.Obj().Pkg(), name)
	if s == nil {
		// Pointer methods on addressable values.
		s = types.NewMethodSet(types.NewPointer(recvT)).Lookup(sel.Obj().Pkg(), name)
		if s == nil {
			panic(fmt.Sprintf("wy: method %s not found on %s", name, recvT))
		}
		return c.in.methodImplLocked(s)
	}
	return c.in.methodImplLocked(s)
}

// --- index and slice -------------------------------------------------------

func (c *compiler) index(e *ast.IndexExpr) eval {
	if _, ok := c.info.Instances[identOf(e.X)]; ok {
		return c.genericFuncValue(e.X)
	}
	xt := c.typeOf(e.X)
	switch u := xt.Underlying().(type) {
	case *types.Map:
		m := c.expr(e.X)
		k := c.mapKey(e.Index, u, xt)
		zero := reflect.Zero(c.in.rtype(u.Elem()))
		view := viewer(c.in.rtype(xt).Elem(), c.in.rtype(u.Elem()))
		return func(fr *frame) reflect.Value {
			v := m(fr).MapIndex(k(fr))
			if !v.IsValid() {
				return zero
			}
			if view != nil {
				return view(v)
			}
			return v
		}
	case *types.Basic: // string
		s := c.strExpr(e.X)
		i := c.intIndex(e.Index)
		rt := c.in.rtype(c.typeOf(e))
		return func(fr *frame) reflect.Value {
			str, idx := s(fr), i(fr)
			if idx < 0 || idx >= len(str) {
				panic(runtimeError(fmt.Sprintf("index out of range [%d] with length %d", idx, len(str))))
			}
			v := reflect.New(rt).Elem()
			v.SetUint(uint64(str[idx]))
			return v
		}
	}
	x := c.expr(e.X)
	i := c.intIndex(e.Index)
	_, isPtr := xt.Underlying().(*types.Pointer)
	crt := c.in.rtype(xt)
	if isPtr {
		crt = crt.Elem()
	}
	view := viewer(crt.Elem(), c.in.rtype(c.typeOf(e)))
	return func(fr *frame) reflect.Value {
		v := x(fr)
		if isPtr {
			v = derefChecked(v)
		}
		idx := i(fr)
		if idx < 0 || idx >= v.Len() {
			panic(runtimeError(fmt.Sprintf("index out of range [%d] with length %d", idx, v.Len())))
		}
		if view != nil {
			return view(v.Index(idx))
		}
		return v.Index(idx)
	}
}

// mapKey compiles a map key, converted to the map's key storage type.
func (c *compiler) mapKey(e ast.Expr, u *types.Map, mt types.Type) eval {
	k := c.convertedExpr(e, u.Key())
	storage := c.in.rtype(mt).Key()
	if storage == c.in.rtype(u.Key()) {
		return k
	}
	return func(fr *frame) reflect.Value { return reinterpret(k(fr), storage) }
}

// storeAs converts a canonical value to a container's storage type.
func storeAs(storage reflect.Type, v reflect.Value) reflect.Value {
	if v.IsValid() && v.Type() != storage {
		return reinterpret(v, storage)
	}
	return v
}

func identOf(e ast.Expr) *ast.Ident {
	switch e := e.(type) {
	case *ast.Ident:
		return e
	case *ast.SelectorExpr:
		return e.Sel
	}
	return nil
}

// intIndex compiles an index or length operand of any integer type.
func (c *compiler) intIndex(e ast.Expr) func(*frame) int {
	switch k, _ := classOf(c.typeOf(e)); k {
	case kUint:
		u := c.uintExpr(e)
		return func(fr *frame) int {
			x := u(fr)
			if x > math.MaxInt {
				panic(runtimeError(fmt.Sprintf("index out of range [%d]", x)))
			}
			return int(x)
		}
	default:
		x := c.intExpr(e)
		return func(fr *frame) int { return int(x(fr)) }
	}
}

func (c *compiler) sliceExpr(e *ast.SliceExpr) eval {
	xt := c.typeOf(e.X)
	opt := func(x ast.Expr) func(*frame) int {
		if x == nil {
			return nil
		}
		return c.intIndex(x)
	}
	lo, hi, max := opt(e.Low), opt(e.High), opt(e.Max)
	if b, ok := xt.Underlying().(*types.Basic); ok && b.Info()&types.IsString != 0 {
		s := c.strExpr(e.X)
		rt := c.in.rtype(c.typeOf(e))
		return func(fr *frame) reflect.Value {
			str := s(fr)
			l, h := 0, len(str)
			if lo != nil {
				l = lo(fr)
			}
			if hi != nil {
				h = hi(fr)
			}
			if l < 0 || h > len(str) || l > h {
				panic(runtimeError(fmt.Sprintf("slice bounds out of range [%d:%d] with length %d", l, h, len(str))))
			}
			v := reflect.New(rt).Elem()
			v.SetString(str[l:h])
			return v
		}
	}
	x := c.expr(e.X)
	_, isPtr := xt.Underlying().(*types.Pointer)
	resRT := c.in.rtype(c.typeOf(e))
	return func(fr *frame) reflect.Value {
		v := x(fr)
		if isPtr {
			v = derefChecked(v)
		}
		if v.Kind() == reflect.Array && !v.CanAddr() {
			tmp := reflect.New(v.Type()).Elem()
			tmp.Set(v)
			v = tmp
		}
		l, h := 0, v.Len()
		if lo != nil {
			l = lo(fr)
		}
		if hi != nil {
			h = hi(fr)
		}
		capacity := v.Cap()
		var r reflect.Value
		if max != nil {
			m := max(fr)
			if l < 0 || h < l || m < h || m > capacity {
				panic(runtimeError(fmt.Sprintf("slice bounds out of range [%d:%d:%d] with capacity %d", l, h, m, capacity)))
			}
			r = v.Slice3(l, h, m)
		} else {
			if l < 0 || h < l || h > capacity {
				panic(runtimeError(fmt.Sprintf("slice bounds out of range [%d:%d] with capacity %d", l, h, capacity)))
			}
			r = v.Slice(l, h)
		}
		if r.Type() != resRT {
			r = r.Convert(resRT)
		}
		return r
	}
}

// --- unary and binary ------------------------------------------------------

func (c *compiler) unary(e *ast.UnaryExpr) eval {
	t := c.typeOf(e)
	rt := c.in.rtype(t)
	switch e.Op {
	case token.AND:
		if cl, ok := ast.Unparen(e.X).(*ast.CompositeLit); ok {
			return c.compositeLit(cl, t)
		}
		x := c.addr(e.X)
		return func(fr *frame) reflect.Value { return x(fr).Addr() }
	case token.ARROW:
		f := c.recv(e, false)
		return func(fr *frame) reflect.Value { return f(fr)[0] }
	case token.NOT:
		b := c.boolExpr(e)
		return boolBox(b, rt)
	}
	return c.boxScalar(e, rt)
}

func (c *compiler) binary(e *ast.BinaryExpr) eval {
	t := c.typeOf(e)
	rt := c.in.rtype(t)
	if isComparison(e.Op) || e.Op == token.LAND || e.Op == token.LOR {
		return boolBox(c.boolExpr(e), rt)
	}
	return c.boxScalar(e, rt)
}

// boxScalar evaluates a basic-typed expression through its fast path and
// materializes the result.
func (c *compiler) boxScalar(e ast.Expr, rt reflect.Type) eval {
	switch k, _ := classOf(c.typeOf(e)); k {
	case kInt:
		f := c.intExpr(e)
		return func(fr *frame) reflect.Value { v := reflect.New(rt).Elem(); v.SetInt(f(fr)); return v }
	case kUint:
		f := c.uintExpr(e)
		return func(fr *frame) reflect.Value { v := reflect.New(rt).Elem(); v.SetUint(f(fr)); return v }
	case kFloat:
		f := c.floatExpr(e)
		return func(fr *frame) reflect.Value { v := reflect.New(rt).Elem(); v.SetFloat(f(fr)); return v }
	case kComplex:
		f := c.complexExpr(e)
		return func(fr *frame) reflect.Value { v := reflect.New(rt).Elem(); v.SetComplex(f(fr)); return v }
	case kString:
		f := c.strExpr(e)
		return func(fr *frame) reflect.Value { v := reflect.New(rt).Elem(); v.SetString(f(fr)); return v }
	case kBool:
		return boolBox(c.boolExpr(e), rt)
	}
	panic(fmt.Sprintf("wy: unsupported operation at %s", c.pos(e)))
}

var (
	trueV  = reflect.ValueOf(true)
	falseV = reflect.ValueOf(false)
)

func boolBox(b boolFn, rt reflect.Type) eval {
	if rt == boolRT {
		return func(fr *frame) reflect.Value {
			if b(fr) {
				return trueV
			}
			return falseV
		}
	}
	return func(fr *frame) reflect.Value { v := reflect.New(rt).Elem(); v.SetBool(b(fr)); return v }
}

// addr compiles an addressable operand (for &x and assignments).
func (c *compiler) addr(e ast.Expr) eval {
	switch e := ast.Unparen(e).(type) {
	case *ast.CompositeLit:
		inner := c.compositeLit(e, c.typeOf(e))
		return func(fr *frame) reflect.Value {
			v := inner(fr)
			if v.CanAddr() {
				return v
			}
			n := reflect.New(v.Type()).Elem()
			n.Set(v)
			return n
		}
	}
	return c.expr(e)
}

// --- type assertions -------------------------------------------------------

// typeAssert compiles x.(T); with commaOk it yields (value, ok).
func (c *compiler) typeAssert(e *ast.TypeAssertExpr, commaOk bool) evalN {
	x := c.expr(e.X)
	srcT := c.typeOf(e.X)
	T := c.typeOf(e.Type)
	rt := c.in.rtype(T)
	check := c.in.assertion(T)
	in := c.in
	srcName := typeName(srcT)
	return func(fr *frame) []reflect.Value {
		v, ok := check(x(fr))
		if commaOk {
			if !ok {
				v = reflect.Zero(rt)
			}
			return []reflect.Value{v, reflect.ValueOf(ok)}
		}
		if !ok {
			nt, _ := in.dyn(x(fr))
			got := "nil"
			if nt != nil {
				got = nt.name
			}
			if isInterface(T) && nt != nil {
				panic(&typeAssertionError{fmt.Sprintf("interface conversion: %s is not %s: missing method", got, typeName(T))})
			}
			msg := fmt.Sprintf("interface conversion: %s is %s, not %s", srcName, got, typeName(T))
			if got == typeName(T) {
				msg += " (types from different scopes)"
			}
			panic(&typeAssertionError{msg})
		}
		return []reflect.Value{v}
	}
}

type typeAssertionError struct{ msg string }

func (e *typeAssertionError) Error() string { return e.msg }
func (e *typeAssertionError) RuntimeError() {}

// assertion returns a checker for interface values against type T.
func (in *Interp) assertion(T types.Type) func(iv reflect.Value) (reflect.Value, bool) {
	rt := in.rtype(T)
	if isInterface(T) {
		return func(iv reflect.Value) (reflect.Value, bool) {
			nt, e := in.dyn(iv)
			if nt == nil || !nt.implements(T) {
				return reflect.Value{}, false
			}
			return in.ifaceFrom(nt, e, rt), true
		}
	}
	want := in.ntypes.get(T)
	return func(iv reflect.Value) (reflect.Value, bool) {
		nt, e := in.dyn(iv)
		if nt != want {
			return reflect.Value{}, false
		}
		if e.Type() != rt {
			e = e.Convert(rt)
		}
		return e, true
	}
}

var _ = utf8.RuneError
