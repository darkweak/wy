package tree

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"math"
	"reflect"
	"runtime"
	"strings"
	"unsafe"
)

// exprN compiles an expression producing several values: calls, and the
// comma-ok forms of map index, type assertion and receive.
func (c *compiler) exprN(e ast.Expr) evalN {
	if tv := c.info.Types[e]; tv.Value != nil {
		x := c.expr(e)
		return func(fr *frame) []reflect.Value { return []reflect.Value{x(fr)} }
	}
	switch e := ast.Unparen(e).(type) {
	case *ast.CallExpr:
		return c.call(e)
	case *ast.TypeAssertExpr:
		return c.typeAssert(e, true)
	case *ast.IndexExpr:
		u := c.typeOf(e.X).Underlying().(*types.Map)
		m := c.expr(e.X)
		k := c.convertedExpr(e.Index, u.Key())
		zero := reflect.Zero(c.in.rtype(u.Elem()))
		return func(fr *frame) []reflect.Value {
			v := m(fr).MapIndex(k(fr))
			if !v.IsValid() {
				return []reflect.Value{zero, falseV}
			}
			return []reflect.Value{v, trueV}
		}
	case *ast.UnaryExpr:
		return c.recv(e, true)
	}
	x := c.expr(e)
	return func(fr *frame) []reflect.Value { return []reflect.Value{x(fr)} }
}

// recv compiles <-ch (with ok when commaOk).
func (c *compiler) recv(e *ast.UnaryExpr, commaOk bool) evalN {
	ch := c.expr(e.X)
	elemRT := c.in.rtype(c.typeOf(e.X).Underlying().(*types.Chan).Elem())
	return func(fr *frame) []reflect.Value {
		cv := ch(fr)
		var v reflect.Value
		ok := false
		if cv.IsNil() {
			select {} // receiving from a nil channel blocks forever
		}
		v, ok = cv.Recv()
		if !v.IsValid() {
			v = reflect.Zero(elemRT)
		} else if v.Type() != elemRT {
			v = reinterpret(v, elemRT)
		}
		if commaOk {
			return []reflect.Value{v, reflect.ValueOf(ok)}
		}
		return []reflect.Value{v}
	}
}

// call1 compiles a call, conversion or builtin used for a single value.
func (c *compiler) call1(e *ast.CallExpr) eval {
	if tv := c.info.Types[e.Fun]; tv.IsType() {
		return c.conversion(e)
	}
	if id, ok := ast.Unparen(e.Fun).(*ast.Ident); ok {
		if b, ok := c.info.Uses[id].(*types.Builtin); ok {
			return c.builtin(b.Name(), e)
		}
	}
	if sel, ok := ast.Unparen(e.Fun).(*ast.SelectorExpr); ok {
		if b, ok := c.info.Uses[sel.Sel].(*types.Builtin); ok {
			return c.builtin(b.Name(), e)
		}
	}
	if f := c.directCall1(e); f != nil {
		return f
	}
	f := c.call(e)
	return func(fr *frame) reflect.Value {
		r := f(fr)
		if len(r) == 0 {
			return reflect.Value{}
		}
		return r[0]
	}
}

// directCall1 compiles a call of a declared, single-result,
// non-variadic function used as a value, without intermediate slices.
func (c *compiler) directCall1(e *ast.CallExpr) eval {
	if e.Ellipsis.IsValid() || isMultiArg(c, e) {
		return nil
	}
	cl := c.callee(e.Fun)
	if cl.direct == nil || cl.sig.Variadic() || cl.sig.Results().Len() != 1 {
		return nil
	}
	fn := cl.direct
	pos := e.Lparen
	ps := cl.sig.Params()
	owned := make([]eval, len(e.Args))
	for i, a := range e.Args {
		owned[i] = c.ownedArg(a, ps.At(i).Type())
	}
	switch len(owned) {
	case 0:
		return func(fr *frame) reflect.Value {
			fr.pos = pos
			return fn.call1Owned(nil, fr)
		}
	case 1:
		a0 := owned[0]
		return func(fr *frame) reflect.Value {
			var a [1]reflect.Value
			a[0] = a0(fr)
			fr.pos = pos
			return fn.call1Owned(a[:], fr)
		}
	case 2:
		a0, a1 := owned[0], owned[1]
		return func(fr *frame) reflect.Value {
			var a [2]reflect.Value
			a[0], a[1] = a0(fr), a1(fr)
			fr.pos = pos
			return fn.call1Owned(a[:], fr)
		}
	}
	return func(fr *frame) reflect.Value {
		a := make([]reflect.Value, len(owned))
		for i, o := range owned {
			a[i] = o(fr)
		}
		fr.pos = pos
		return fn.call1Owned(a, fr)
	}
}

// callee describes how to invoke the function part of a call.
type callee struct {
	// bind evaluates the function (and receiver) and returns an invoker.
	bind func(fr *frame) func(args []reflect.Value, spread bool, deferOf *frame) []reflect.Value
	sig  *types.Signature
	// paramRTs, when set, lets arguments be evaluated straight into the
	// callee's parameter types (interpreted functions).
	direct *function
}

// call compiles a call expression (not a conversion).
func (c *compiler) call(e *ast.CallExpr) evalN {
	if tv := c.info.Types[e.Fun]; tv.IsType() {
		conv := c.conversion(e)
		return func(fr *frame) []reflect.Value { return []reflect.Value{conv(fr)} }
	}
	if id := builtinIdent(c.info, e.Fun); id != nil {
		b := c.builtin(id.Name, e)
		return func(fr *frame) []reflect.Value {
			v := b(fr)
			if !v.IsValid() {
				return nil
			}
			return []reflect.Value{v}
		}
	}
	cl := c.callee(e.Fun)
	args := c.args(e, cl.sig)
	spread := e.Ellipsis.IsValid()
	pos := e.Lparen
	if cl.direct != nil && !spread && !cl.sig.Variadic() && !isMultiArg(c, e) {
		// Direct call of a declared function: evaluate the arguments into
		// fresh parameter variables that the callee adopts.
		fn := cl.direct
		ps := cl.sig.Params()
		owned := make([]eval, len(e.Args))
		for i, a := range e.Args {
			owned[i] = c.ownedArg(a, ps.At(i).Type())
		}
		switch len(owned) {
		case 1:
			a0 := owned[0]
			return func(fr *frame) []reflect.Value {
				a := []reflect.Value{a0(fr)}
				fr.pos = pos
				return fn.callOwned(a, fr)
			}
		}
		return func(fr *frame) []reflect.Value {
			a := make([]reflect.Value, len(owned))
			for i, o := range owned {
				a[i] = o(fr)
			}
			fr.pos = pos
			return fn.callOwned(a, fr)
		}
	}
	if cl.direct != nil && !spread {
		fn := cl.direct
		variadic := cl.sig.Variadic()
		return func(fr *frame) []reflect.Value {
			a := args(fr)
			if variadic {
				a = packVariadic(fn.paramTypes, a)
			}
			fr.pos = pos
			return fn.call(nil, a, fr)
		}
	}
	return func(fr *frame) []reflect.Value {
		inv := cl.bind(fr)
		a := args(fr)
		fr.pos = pos
		return inv(a, spread, fr)
	}
}

// isMultiArg reports whether a call passes one multi-valued argument.
func isMultiArg(c *compiler, e *ast.CallExpr) bool {
	if len(e.Args) != 1 {
		return false
	}
	t, ok := c.typeOf(e.Args[0]).(*types.Tuple)
	return ok && t.Len() > 1
}

// ownedArg compiles an argument into a fresh variable of the parameter
// type t.
func (c *compiler) ownedArg(a ast.Expr, t types.Type) eval {
	rt := c.in.rtype(t)
	if !isInterface(t) && types.Identical(c.typeOf(a).Underlying(), t.Underlying()) || isUntypedConst(c, a) {
		switch k, _ := classOf(t); k {
		case kInt:
			f := c.intExpr(a)
			return func(fr *frame) reflect.Value { v := reflect.New(rt).Elem(); v.SetInt(f(fr)); return v }
		case kFloat:
			f := c.floatExpr(a)
			return func(fr *frame) reflect.Value { v := reflect.New(rt).Elem(); v.SetFloat(f(fr)); return v }
		case kString:
			f := c.strExpr(a)
			return func(fr *frame) reflect.Value { v := reflect.New(rt).Elem(); v.SetString(f(fr)); return v }
		case kBool:
			f := c.boolExpr(a)
			return func(fr *frame) reflect.Value { v := reflect.New(rt).Elem(); v.SetBool(f(fr)); return v }
		}
	}
	x := c.elemExpr(a, t)
	return func(fr *frame) reflect.Value {
		v := reflect.New(rt).Elem()
		v.Set(x(fr))
		return v
	}
}

func isUntypedConst(c *compiler, e ast.Expr) bool {
	tv := c.info.Types[e]
	if tv.Value == nil {
		return false
	}
	b, ok := tv.Type.(*types.Basic)
	return ok && b.Info()&types.IsUntyped != 0
}

func builtinIdent(info *types.Info, fun ast.Expr) *ast.Ident {
	switch f := ast.Unparen(fun).(type) {
	case *ast.Ident:
		if _, ok := info.Uses[f].(*types.Builtin); ok {
			return f
		}
	case *ast.SelectorExpr:
		if _, ok := info.Uses[f.Sel].(*types.Builtin); ok {
			return f.Sel
		}
	}
	return nil
}

// args compiles call arguments, converted to parameter types. A single
// multi-valued argument is expanded.
func (c *compiler) args(e *ast.CallExpr, sig *types.Signature) evalN {
	params := sig.Params()
	paramT := func(i int) types.Type {
		if sig.Variadic() && i >= params.Len()-1 {
			last := params.At(params.Len() - 1).Type()
			if e.Ellipsis.IsValid() {
				return last
			}
			return last.(*types.Slice).Elem()
		}
		return params.At(i).Type()
	}
	if len(e.Args) == 1 {
		if tup, ok := c.typeOf(e.Args[0]).(*types.Tuple); ok && tup.Len() > 1 {
			multi := c.exprN(e.Args[0])
			convs := make([]func(reflect.Value) reflect.Value, tup.Len())
			for i := range convs {
				convs[i] = c.assignConv(tup.At(i).Type(), paramT(i))
			}
			return func(fr *frame) []reflect.Value {
				vals := multi(fr)
				out := make([]reflect.Value, len(vals))
				for i, v := range vals {
					out[i] = convs[i](v)
				}
				return out
			}
		}
	}
	evals := make([]eval, len(e.Args))
	for i, a := range e.Args {
		evals[i] = c.elemExpr(a, paramT(i))
	}
	return func(fr *frame) []reflect.Value {
		out := make([]reflect.Value, len(evals))
		for i, ev := range evals {
			out[i] = ev(fr)
		}
		return out
	}
}

// callee compiles the function part of a call.
func (c *compiler) callee(fun ast.Expr) callee {
	fun = ast.Unparen(fun)
	sig, _ := c.typeOf(fun).Underlying().(*types.Signature)
	var id *ast.Ident
	switch f := fun.(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		if sel, ok := c.info.Selections[f]; ok {
			switch sel.Kind() {
			case types.MethodVal:
				mc := c.methodCaller(f, sel)
				return callee{bind: mc.bind, sig: sig}
			}
		} else {
			id = f.Sel
		}
	case *ast.IndexExpr:
		id = identOf(f.X)
	case *ast.IndexListExpr:
		id = identOf(f.X)
	}
	if id != nil {
		if o, ok := c.info.Uses[id].(*types.Func); ok && o.Type().(*types.Signature).Recv() == nil {
			targs := c.instanceArgs(id)
			if o.Pkg() != nil {
				if np := c.in.nativePkg(o.Pkg().Path()); np != nil {
					if fv, ok := np.Funcs[o.Name()]; ok {
						if b := nativeIntrinsic(c.in, o.Pkg().Path()+"."+o.Name()); b != nil {
							return callee{sig: sig, bind: b}
						}
						in := c.in
						mode := modeForPackage(o.Pkg().Path())
						inv := func(args []reflect.Value, spread bool, _ *frame) []reflect.Value {
							return in.callNativeMode(fv, sig, args, spread, mode)
						}
						return callee{sig: sig, bind: func(*frame) func([]reflect.Value, bool, *frame) []reflect.Value { return inv }}
					}
				}
			}
			fn := c.in.funcFor(o, targs)
			inv := func(args []reflect.Value, spread bool, deferOf *frame) []reflect.Value {
				if fn.variadic && !spread {
					args = packVariadic(fn.paramTypes, args)
				}
				return fn.call(nil, args, deferOf)
			}
			return callee{sig: sig, direct: fn, bind: func(*frame) func([]reflect.Value, bool, *frame) []reflect.Value { return inv }}
		}
	}
	if lit, ok := fun.(*ast.FuncLit); ok {
		mk := c.closureLit(lit)
		return callee{sig: sig, bind: func(fr *frame) func([]reflect.Value, bool, *frame) []reflect.Value {
			cl := mk(fr)
			return func(args []reflect.Value, spread bool, deferOf *frame) []reflect.Value {
				if cl.fn.variadic && !spread {
					args = packVariadic(cl.fn.paramTypes, args)
				}
				return cl.call(args, deferOf)
			}
		}}
	}
	fv := c.expr(fun)
	in := c.in
	return callee{sig: sig, bind: func(fr *frame) func([]reflect.Value, bool, *frame) []reflect.Value {
		f := fv(fr)
		return func(args []reflect.Value, spread bool, deferOf *frame) []reflect.Value {
			return in.callValue(f, sig, args, spread, deferOf)
		}
	}}
}

type methodCall struct {
	bind func(fr *frame) func(args []reflect.Value, spread bool, deferOf *frame) []reflect.Value
}

// methodCaller compiles x.M for a call or method value: the receiver is
// evaluated by bind.
func (c *compiler) methodCaller(e *ast.SelectorExpr, sel *types.Selection) methodCall {
	in := c.in
	recvT := c.typeOf(e.X)
	obj := sel.Obj().(*types.Func)
	name, pkg := obj.Name(), obj.Pkg()
	path := sel.Index()
	msig := obj.Type().(*types.Signature)
	_, ifaceMethod := msig.Recv().Type().Underlying().(*types.Interface)
	if ifaceMethod && len(path) == 1 && !isInterface(recvT) {
		// A constraint's method called on a type parameter, now
		// instantiated with a concrete type.
		ifaceMethod = false
	}

	if ifaceMethod {
		// Walk embedded fields to the interface value, then dispatch.
		var x eval
		if len(path) > 1 {
			x = c.fieldPath(c.expr(e.X), recvT, path[:len(path)-1])
		} else {
			x = c.expr(e.X)
		}
		var ptrDeref bool
		if len(path) == 1 {
			_, ptrDeref = recvT.Underlying().(*types.Pointer)
		}
		return methodCall{bind: func(fr *frame) func([]reflect.Value, bool, *frame) []reflect.Value {
			iv := x(fr)
			if ptrDeref {
				iv = derefChecked(iv)
			}
			nt, v := in.dyn(iv)
			if nt == nil {
				panic(runtimeError("invalid memory address or nil pointer dereference"))
			}
			return func(args []reflect.Value, spread bool, deferOf *frame) []reflect.Value {
				m := nt.method(name, pkg)
				if m == nil {
					panic(fmt.Sprintf("wy: %s has no method %s", nt.name, name))
				}
				return in.invoke(m, v, args, spread, deferOf)
			}
		}}
	}

	// Concrete receiver: resolve statically.
	s := types.NewMethodSet(recvT).Lookup(pkg, name)
	addrOf := false
	if s == nil {
		s = types.NewMethodSet(types.NewPointer(recvT)).Lookup(pkg, name)
		addrOf = true
	}
	if s == nil {
		panic(fmt.Sprintf("wy: method %s not found on %s at %s", name, recvT, c.pos(e)))
	}
	m := in.methodImplLocked(s)
	x := c.addr(e.X)
	_, recvIsPtr := msigRecv(m).(*types.Pointer)
	return methodCall{bind: func(fr *frame) func([]reflect.Value, bool, *frame) []reflect.Value {
		v := x(fr)
		if addrOf {
			if !v.CanAddr() {
				tmp := reflect.New(v.Type()).Elem()
				tmp.Set(v)
				v = tmp
			}
			v = v.Addr()
		} else if !recvIsPtr && len(s.Index()) == 1 && v.Kind() != reflect.Pointer {
			// Value receiver: the method sees a copy, evaluated now.
			cp := reflect.New(v.Type()).Elem()
			cp.Set(v)
			v = cp
		}
		// The receiver is selected now: a method value of a nil pointer
		// with a value receiver panics when it is evaluated.
		recv := in.selectRecv(v, m.sel)
		return func(args []reflect.Value, spread bool, deferOf *frame) []reflect.Value {
			return in.invokeRecv(m, recv, args, spread, deferOf)
		}
	}}
}

func msigRecv(m *methodImpl) types.Type {
	return m.sig.Recv().Type()
}

// --- conversions -------------------------------------------------------------

func (c *compiler) conversion(e *ast.CallExpr) eval {
	dst := c.typeOf(e.Fun)
	src := c.typeOf(e.Args[0])
	drt := c.in.rtype(dst)
	if isNilExpr(c.info, e.Args[0]) {
		z := reflect.Zero(drt)
		return func(*frame) reflect.Value { return z }
	}
	if isInterface(dst) {
		return c.convertedExpr(e.Args[0], dst)
	}
	if tv := c.info.Types[e.Args[0]]; tv.Value != nil {
		if _, tp := c.info.Types[e.Fun].Type.(*types.TypeParam); tp {
			// A constant converted to a type parameter: materialize it
			// directly in the instantiated type.
			v := c.constValue(tv.Value, dst)
			return func(*frame) reflect.Value { return v }
		}
	}
	x := c.expr(e.Args[0])
	srt := c.in.rtype(src)
	switch {
	case drt.Kind() == reflect.UnsafePointer && srt.Kind() == reflect.Pointer:
		return func(fr *frame) reflect.Value {
			v := reflect.New(drt).Elem()
			v.SetPointer(x(fr).UnsafePointer())
			return v
		}
	case drt.Kind() == reflect.UnsafePointer && srt.Kind() == reflect.Uintptr:
		return func(fr *frame) reflect.Value {
			v := reflect.New(drt).Elem()
			v.SetPointer(uintptrToPointer(uintptr(x(fr).Uint())))
			return v
		}
	case drt.Kind() == reflect.Pointer && srt.Kind() == reflect.UnsafePointer:
		elem := drt.Elem()
		return func(fr *frame) reflect.Value {
			p := x(fr).UnsafePointer()
			if p == nil {
				return reflect.Zero(drt)
			}
			return reflect.NewAt(elem, p)
		}
	case drt.Kind() == reflect.Uintptr && srt.Kind() == reflect.UnsafePointer:
		return func(fr *frame) reflect.Value {
			v := reflect.New(drt).Elem()
			v.SetUint(uint64(uintptr(x(fr).UnsafePointer())))
			return v
		}
	case drt.Kind() == reflect.String && (srt.Kind() == reflect.Int64 || srt.Kind() == reflect.Int || srt.Kind() == reflect.Int32 ||
		srt.Kind() == reflect.Uint64 || srt.Kind() == reflect.Uint32 || srt.Kind() == reflect.Uint || srt.Kind() == reflect.Int16 ||
		srt.Kind() == reflect.Int8 || srt.Kind() == reflect.Uint8 || srt.Kind() == reflect.Uint16 || srt.Kind() == reflect.Uintptr):
		// string(rune): out-of-range values become U+FFFD.
		signed := srt.Kind() >= reflect.Int && srt.Kind() <= reflect.Int64
		return func(fr *frame) reflect.Value {
			v := x(fr)
			var r rune = 0xFFFD
			if signed {
				if i := v.Int(); i >= 0 && i <= 0x10FFFF {
					r = rune(i)
				}
			} else if u := v.Uint(); u <= 0x10FFFF {
				r = rune(u)
			}
			out := reflect.New(drt).Elem()
			out.SetString(string(r))
			return out
		}
	}
	if srt == drt {
		return func(fr *frame) reflect.Value {
			v := x(fr)
			cp := reflect.New(drt).Elem()
			cp.Set(v)
			return cp
		}
	}
	// Slice to array (pointer) conversions check lengths like Go does.
	if srt.Kind() == reflect.Slice && (drt.Kind() == reflect.Array || drt.Kind() == reflect.Pointer && drt.Elem().Kind() == reflect.Array) {
		n := 0
		if drt.Kind() == reflect.Array {
			n = drt.Len()
		} else {
			n = drt.Elem().Len()
		}
		return func(fr *frame) reflect.Value {
			v := x(fr)
			if v.Len() < n {
				panic(runtimeError(fmt.Sprintf("cannot convert slice with length %d to array or pointer to array with length %d", v.Len(), n)))
			}
			return v.Convert(drt)
		}
	}
	if !srt.ConvertibleTo(drt) && srt.Size() == drt.Size() {
		// Types identical in Go whose representations differ only by
		// stand-ins for self-referential types.
		return func(fr *frame) reflect.Value { return reinterpret(x(fr), drt) }
	}
	return func(fr *frame) reflect.Value { return x(fr).Convert(drt) }
}

// --- builtins ------------------------------------------------------------

func (c *compiler) builtin(name string, e *ast.CallExpr) eval {
	if len(e.Args) == 1 && name != "print" && name != "println" {
		if tup, ok := c.typeOf(e.Args[0]).(*types.Tuple); ok && tup.Len() > 1 {
			return c.builtinTuple(name, e, tup)
		}
	}
	in := c.in
	resT := c.typeOf(e)
	switch name {
	case "len", "cap":
		at := c.typeOf(e.Args[0])
		if b, ok := at.Underlying().(*types.Basic); ok && b.Info()&types.IsString != 0 {
			s := c.strExpr(e.Args[0])
			return func(fr *frame) reflect.Value { return reflect.ValueOf(len(s(fr))) }
		}
		x := c.expr(e.Args[0])
		_, isPtr := at.Underlying().(*types.Pointer)
		capOf := name == "cap"
		return func(fr *frame) reflect.Value {
			v := x(fr)
			if isPtr {
				if v.IsNil() {
					return reflect.ValueOf(v.Type().Elem().Len())
				}
				v = v.Elem()
			}
			if capOf {
				return reflect.ValueOf(v.Cap())
			}
			return reflect.ValueOf(v.Len())
		}
	case "append":
		st := c.typeOf(e.Args[0])
		srt := c.in.rtype(st)
		s := c.expr(e.Args[0])
		if e.Ellipsis.IsValid() {
			other := c.expr(e.Args[1])
			if b, ok := c.typeOf(e.Args[1]).Underlying().(*types.Basic); ok && b.Info()&types.IsString != 0 {
				return func(fr *frame) reflect.Value {
					v := s(fr)
					bs := []byte(other(fr).String())
					return reflect.AppendSlice(v, reflect.ValueOf(bs).Convert(srt))
				}
			}
			return func(fr *frame) reflect.Value {
				v := s(fr)
				o := other(fr)
				if o.Type() != srt {
					o = o.Convert(srt)
				}
				return reflect.AppendSlice(v, o)
			}
		}
		et := st.Underlying().(*types.Slice).Elem()
		est := srt.Elem()
		elems := make([]eval, len(e.Args)-1)
		for i, a := range e.Args[1:] {
			elems[i] = c.elemExpr(a, et)
		}
		return func(fr *frame) reflect.Value {
			v := s(fr)
			if len(elems) == 1 {
				return reflect.Append(v, storeAs(est, copyValue(elems[0](fr))))
			}
			// Values are copied as they are evaluated: appending may write
			// over storage an argument still refers to.
			vals := make([]reflect.Value, len(elems))
			for i, el := range elems {
				vals[i] = storeAs(est, copyValue(el(fr)))
			}
			return reflect.Append(v, vals...)
		}
	case "copy":
		dst, src := c.expr(e.Args[0]), c.expr(e.Args[1])
		return func(fr *frame) reflect.Value { return reflect.ValueOf(reflect.Copy(dst(fr), src(fr))) }
	case "delete":
		m := c.expr(e.Args[0])
		k := c.convertedExpr(e.Args[1], c.typeOf(e.Args[0]).Underlying().(*types.Map).Key())
		return func(fr *frame) reflect.Value {
			mv := m(fr)
			if !mv.IsNil() {
				mv.SetMapIndex(k(fr), reflect.Value{})
			}
			return reflect.Value{}
		}
	case "clear":
		x := c.expr(e.Args[0])
		return func(fr *frame) reflect.Value {
			v := x(fr)
			if !v.IsNil() {
				v.Clear()
			}
			return reflect.Value{}
		}
	case "close":
		ch := c.expr(e.Args[0])
		return func(fr *frame) reflect.Value {
			v := ch(fr)
			if v.IsNil() {
				panic(plainError("close of nil channel"))
			}
			v.Close()
			return reflect.Value{}
		}
	case "make":
		t := c.typeOf(e.Args[0])
		rt := c.in.rtype(t)
		var sizes []func(*frame) int
		for _, a := range e.Args[1:] {
			sizes = append(sizes, c.intIndex(a))
		}
		switch t.Underlying().(type) {
		case *types.Slice:
			return func(fr *frame) reflect.Value {
				n := sizes[0](fr)
				cp := n
				if len(sizes) > 1 {
					cp = sizes[1](fr)
				}
				if n < 0 {
					panic(runtimeError("makeslice: len out of range"))
				}
				if cp < n {
					panic(runtimeError("makeslice: cap out of range"))
				}
				return reflect.MakeSlice(rt, n, cp)
			}
		case *types.Map:
			return func(fr *frame) reflect.Value {
				n := 0
				if len(sizes) > 0 {
					n = sizes[0](fr)
				}
				return reflect.MakeMapWithSize(rt, n)
			}
		case *types.Chan:
			bidi := reflect.ChanOf(reflect.BothDir, rt.Elem())
			return func(fr *frame) reflect.Value {
				n := 0
				if len(sizes) > 0 {
					n = sizes[0](fr)
				}
				ch := reflect.MakeChan(bidi, n)
				if rt != bidi {
					ch = ch.Convert(rt)
				}
				return ch
			}
		}
	case "new":
		if tv := c.info.Types[e.Args[0]]; tv.IsType() {
			rt := c.in.rtype(c.typeOf(e.Args[0]))
			return func(*frame) reflect.Value { return reflect.New(rt) }
		}
		// Go 1.26: new(expr) allocates a variable initialized to expr.
		elem := c.typeOf(e).(*types.Pointer).Elem()
		rt := c.in.rtype(elem)
		x := c.convertedExpr(e.Args[0], elem)
		return func(fr *frame) reflect.Value {
			p := reflect.New(rt)
			p.Elem().Set(x(fr))
			return p
		}
	case "panic":
		x := c.convertedExpr(e.Args[0], types.NewInterfaceType(nil, nil))
		return func(fr *frame) reflect.Value {
			v := x(fr)
			if v.IsNil() {
				panic(&runtime.PanicNilError{})
			}
			panic(v.Elem().Interface())
		}
	case "recover":
		return func(fr *frame) reflect.Value {
			out := reflect.New(anyRT).Elem()
			if r := fr.recoverValue(); r != nil {
				out.Set(reflect.ValueOf(r))
			}
			return out
		}
	case "print", "println":
		var xs []evalN
		for _, a := range e.Args {
			if _, multi := c.typeOf(a).(*types.Tuple); multi {
				xs = append(xs, c.exprN(a))
				continue
			}
			x := c.expr(a)
			xs = append(xs, func(fr *frame) []reflect.Value { return []reflect.Value{x(fr)} })
		}
		ln := name == "println"
		return func(fr *frame) reflect.Value {
			var b strings.Builder
			n := 0
			for _, x := range xs {
				for _, v := range x(fr) {
					if ln && n > 0 {
						b.WriteByte(' ')
					}
					b.WriteString(printValue(v, nil))
					n++
				}
			}
			if ln {
				b.WriteByte('\n')
			}
			fmt.Fprint(in.opts.Stderr, b.String())
			return reflect.Value{}
		}
	case "min", "max":
		return c.minMax(name == "min", e, resT)
	case "complex":
		re, im := c.floatExpr(e.Args[0]), c.floatExpr(e.Args[1])
		rt := c.in.rtype(resT)
		return func(fr *frame) reflect.Value {
			v := reflect.New(rt).Elem()
			v.SetComplex(complex(re(fr), im(fr)))
			return v
		}
	case "real", "imag":
		x := c.complexExpr(e.Args[0])
		rt := c.in.rtype(resT)
		isReal := name == "real"
		return func(fr *frame) reflect.Value {
			v := reflect.New(rt).Elem()
			z := x(fr)
			if isReal {
				v.SetFloat(real(z))
			} else {
				v.SetFloat(imag(z))
			}
			return v
		}
	// package unsafe
	case "Sizeof", "Alignof", "Offsetof":
		// Constant except for operands whose type depends on a type
		// parameter; their value is fixed once instantiated. The operand
		// is not evaluated.
		var n uintptr
		switch name {
		case "Sizeof":
			n = c.in.rtype(c.typeOf(e.Args[0])).Size()
		case "Alignof":
			n = uintptr(c.in.rtype(c.typeOf(e.Args[0])).Align())
		case "Offsetof":
			sel := ast.Unparen(e.Args[0]).(*ast.SelectorExpr)
			rt := c.in.rtype(c.typeOf(sel.X))
			for _, i := range c.info.Selections[sel].Index() {
				if rt.Kind() == reflect.Pointer {
					rt = rt.Elem()
				}
				f := rt.Field(i)
				n += f.Offset
				rt = f.Type
			}
		}
		v := reflect.ValueOf(n).Convert(c.in.rtype(resT))
		return func(*frame) reflect.Value { return v }
	case "Add":
		p := c.expr(e.Args[0])
		n := c.intExpr(e.Args[1])
		return func(fr *frame) reflect.Value {
			v := reflect.New(unsafePtrT).Elem()
			v.SetPointer(unsafe.Add(p(fr).UnsafePointer(), n(fr)))
			return v
		}
	case "Slice":
		p := c.expr(e.Args[0])
		n := c.unsafeLen(e.Args[1])
		elem := c.in.rtype(c.typeOf(e.Args[0]).Underlying().(*types.Pointer).Elem())
		rt := c.in.rtype(resT)
		return func(fr *frame) reflect.Value {
			ptr := p(fr)
			cnt := n(fr)
			var up unsafe.Pointer
			if ptr.IsValid() && !ptr.IsNil() {
				up = ptr.UnsafePointer()
			}
			checkUnsafeLen("unsafe.Slice", up, cnt, elem.Size())
			if up == nil {
				return reflect.Zero(rt)
			}
			return reflect.SliceAt(elem, up, int(cnt))
		}
	case "SliceData":
		s := c.expr(e.Args[0])
		rt := c.in.rtype(resT)
		return func(fr *frame) reflect.Value {
			v := s(fr)
			if v.IsNil() {
				return reflect.Zero(rt)
			}
			return reflect.NewAt(rt.Elem(), v.UnsafePointer())
		}
	case "String":
		p := c.expr(e.Args[0])
		n := c.unsafeLen(e.Args[1])
		return func(fr *frame) reflect.Value {
			ptr := p(fr)
			cnt := n(fr)
			var up unsafe.Pointer
			if ptr.IsValid() && !ptr.IsNil() {
				up = ptr.UnsafePointer()
			}
			checkUnsafeLen("unsafe.String", up, cnt, 1)
			if cnt == 0 {
				return reflect.ValueOf("")
			}
			return reflect.ValueOf(unsafe.String((*byte)(up), int(cnt)))
		}
	case "StringData":
		s := c.strExpr(e.Args[0])
		return func(fr *frame) reflect.Value { return reflect.ValueOf(unsafe.StringData(s(fr))) }
	}
	panic(fmt.Sprintf("wy: unsupported builtin %s at %s", name, c.pos(e)))
}

// builtinTuple compiles a builtin call whose single argument is a
// multi-valued call, f(g()), by binding g's results to temporaries.
func (c *compiler) builtinTuple(name string, e *ast.CallExpr, tup *types.Tuple) eval {
	multi := c.exprN(e.Args[0])
	call := *e
	call.Args = nil
	var slots []int
	for i := 0; i < tup.Len(); i++ {
		v := types.NewVar(e.Pos(), nil, fmt.Sprintf("wy_arg%d", i), tup.At(i).Type())
		id := ast.NewIdent(v.Name())
		c.info.Uses[id] = v
		c.info.Types[id] = types.TypeAndValue{Type: v.Type()}
		slots = append(slots, c.fs.newSlot(v))
		call.Args = append(call.Args, id)
	}
	c.info.Types[&call] = c.info.Types[e]
	b := c.builtin(name, &call)
	return func(fr *frame) reflect.Value {
		for i, v := range multi(fr) {
			fr.vars[slots[i]] = copyValue(v)
		}
		return b(fr)
	}
}

// unsafeLen compiles the length operand of unsafe.Slice/String, keeping
// out-of-range unsigned values detectable.
func (c *compiler) unsafeLen(e ast.Expr) func(*frame) int64 {
	if k, _ := classOf(c.typeOf(e)); k == kUint {
		u := c.uintExpr(e)
		return func(fr *frame) int64 {
			x := u(fr)
			if x > math.MaxInt64 {
				return -1
			}
			return int64(x)
		}
	}
	return c.intExpr(e)
}

// checkUnsafeLen applies the run-time checks of unsafe.Slice and
// unsafe.String.
func checkUnsafeLen(fn string, p unsafe.Pointer, n int64, size uintptr) {
	if p == nil && n != 0 {
		panic(runtimeError(fn + ": ptr is nil and len is not zero"))
	}
	if n < 0 {
		panic(runtimeError(fn + ": len out of range"))
	}
	const maxAlloc = 1 << 48
	if size > 0 && uint64(n) > maxAlloc/uint64(size) {
		panic(runtimeError(fn + ": len out of range"))
	}
	if mem := uintptr(uint64(n) * uint64(size)); p != nil && mem > -uintptr(p) {
		panic(runtimeError(fn + ": ptr+len overflows"))
	}
}

type plainError string

func (e plainError) Error() string { return string(e) }
func (e plainError) RuntimeError() {}

func (c *compiler) minMax(isMin bool, e *ast.CallExpr, resT types.Type) eval {
	rt := c.in.rtype(resT)
	switch k, _ := classOf(resT); k {
	case kInt:
		var fs []intFn
		for _, a := range e.Args {
			fs = append(fs, c.intExpr(a))
		}
		return func(fr *frame) reflect.Value {
			r := fs[0](fr)
			for _, f := range fs[1:] {
				x := f(fr)
				if isMin == (x < r) {
					r = x
				}
			}
			v := reflect.New(rt).Elem()
			v.SetInt(r)
			return v
		}
	case kUint:
		var fs []uintFn
		for _, a := range e.Args {
			fs = append(fs, c.uintExpr(a))
		}
		return func(fr *frame) reflect.Value {
			r := fs[0](fr)
			for _, f := range fs[1:] {
				x := f(fr)
				if isMin == (x < r) {
					r = x
				}
			}
			v := reflect.New(rt).Elem()
			v.SetUint(r)
			return v
		}
	case kFloat:
		var fs []floatFn
		for _, a := range e.Args {
			fs = append(fs, c.floatExpr(a))
		}
		return func(fr *frame) reflect.Value {
			r := fs[0](fr)
			for _, f := range fs[1:] {
				x := f(fr)
				switch {
				case math.IsNaN(x) || math.IsNaN(r):
					r = math.NaN()
				case isMin:
					r = math.Min(r, x)
				default:
					r = math.Max(r, x)
				}
			}
			v := reflect.New(rt).Elem()
			v.SetFloat(r)
			return v
		}
	case kString:
		var fs []strFn
		for _, a := range e.Args {
			fs = append(fs, c.strExpr(a))
		}
		return func(fr *frame) reflect.Value {
			r := fs[0](fr)
			for _, f := range fs[1:] {
				x := f(fr)
				if isMin == (x < r) {
					r = x
				}
			}
			v := reflect.New(rt).Elem()
			v.SetString(r)
			return v
		}
	}
	panic("wy: min/max of unsupported type")
}

// printValue formats a value like the print builtin.
func printValue(v reflect.Value, t types.Type) string {
	if !v.IsValid() {
		return "nil"
	}
	switch v.Kind() {
	case reflect.Float32, reflect.Float64:
		return printFloat(v.Float())
	case reflect.Complex64, reflect.Complex128:
		c := v.Complex()
		return "(" + printFloat(real(c)) + printFloat(imag(c)) + "i)"
	case reflect.Interface:
		if v.IsNil() {
			return "(0x0,0x0)"
		}
		return fmt.Sprintf("(%v)", v.Elem().Interface())
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Slice:
		return fmt.Sprintf("%#x", v.Pointer())
	}
	return fmt.Sprintf("%v", v.Interface())
}

var _ = token.ADD

// printFloat formats like the runtime's print builtin: +d.dddddde+ddd.
func printFloat(v float64) string {
	switch {
	case v != v:
		return "NaN"
	case v+v == v && v > 0:
		return "+Inf"
	case v+v == v && v < 0:
		return "-Inf"
	}
	const n = 7 // digits printed
	var buf [n + 7]byte
	buf[0] = '+'
	e := 0 // exp
	if v == 0 {
		if 1/v < 0 {
			buf[0] = '-'
		}
	} else {
		if v < 0 {
			v = -v
			buf[0] = '-'
		}
		// normalize
		for v >= 10 {
			e++
			v /= 10
		}
		for v < 1 {
			e--
			v *= 10
		}
		// round
		h := 5.0
		for i := 0; i < n; i++ {
			h /= 10
		}
		v += h
		if v >= 10 {
			e++
			v /= 10
		}
	}
	// format +d.dddd+edd
	for i := 0; i < n; i++ {
		s := int(v)
		buf[i+2] = byte(s + '0')
		v -= float64(s)
		v *= 10
	}
	buf[1] = buf[2]
	buf[2] = '.'
	buf[n+2] = 'e'
	buf[n+3] = '+'
	if e < 0 {
		e = -e
		buf[n+3] = '-'
	}
	buf[n+4] = byte(e/100 + '0')
	buf[n+5] = byte(e/10)%10 + '0'
	buf[n+6] = byte(e%10) + '0'
	return string(buf[:])
}

// uintptrToPointer performs the program's unsafe.Pointer(uintptr)
// conversion. In compiled Go such arithmetic is checked within a single
// expression; the interpreter evaluates it in steps, so checkptr (enabled
// by -race) cannot relate the result to its allocation.
//
//go:nocheckptr
func uintptrToPointer(u uintptr) unsafe.Pointer {
	return unsafe.Pointer(u) //nolint:govet // requested by the interpreted program
}
