package tree

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"unicode/utf8"
)

// iterControl carries a range-over-func body's pending control flow out of
// the yield callback.
type iterControl struct {
	pending ctrl
	label   string
	done    bool
}

func (c *compiler) rangeStmt(s *ast.RangeStmt, label string) exec {
	xt := c.typeOf(s.X)
	// Loop variables: fresh per iteration for :=, assigned otherwise.
	type loopVar struct {
		set func(fr *frame, v reflect.Value)
	}
	mkVar := func(e ast.Expr, t types.Type) func(fr *frame, v reflect.Value) {
		if e == nil {
			return nil
		}
		if id, ok := e.(*ast.Ident); ok && id.Name == "_" {
			return nil
		}
		if s.Tok == token.DEFINE {
			slot, rt, ok := c.newVar(e.(*ast.Ident))
			if !ok {
				return nil
			}
			return func(fr *frame, v reflect.Value) {
				nv := reflect.New(rt).Elem()
				nv.Set(v)
				fr.vars[slot] = nv
			}
		}
		st, lt := c.store(e)
		conv := c.assignConv(t, lt)
		return func(fr *frame, v reflect.Value) { st(fr)(conv(v)) }
	}
	// Declare the loop variables before compiling the body that uses them.
	if s.Tok == token.DEFINE {
		for _, e := range []ast.Expr{s.Key, s.Value} {
			if id, ok := e.(*ast.Ident); ok {
				c.newVar(id)
			}
		}
	}
	body := c.block(s.Body.List)
	x := c.expr(s.X)
	// for k, v = range x assigns like k, v = ...: both targets' operands
	// are evaluated before either is assigned.
	pairAssign := func(setK, setV func(*frame, reflect.Value)) (func(*frame, reflect.Value), func(*frame, reflect.Value)) {
		if s.Tok != token.ASSIGN || s.Key == nil || s.Value == nil || setK == nil || setV == nil {
			return setK, setV
		}
		stK, _ := c.store(s.Key)
		stV, _ := c.store(s.Value)
		pending := c.fs.newSlot(nil) // per frame: loops run concurrently
		return func(fr *frame, k reflect.Value) {
				fr.vars[pending] = copyValue(k)
			}, func(fr *frame, v reflect.Value) {
				setKey, setVal := stK(fr), stV(fr)
				setKey(fr.vars[pending])
				setVal(v)
			}
	}

	run := func(fr *frame, r ctrl) (stop bool, res ctrl) { return loopCtl(fr, r, label) }

	switch u := xt.Underlying().(type) {
	case *types.Basic:
		if u.Info()&types.IsInteger != 0 {
			setK := mkVar(s.Key, xt)
			rt := c.in.rtype(xt)
			n := c.intExpr(s.X)
			if u.Info()&types.IsUnsigned != 0 {
				un := c.uintExpr(s.X)
				n = func(fr *frame) int64 { return int64(un(fr)) }
			}
			return func(fr *frame) ctrl {
				limit := n(fr)
				for i := int64(0); i < limit; i++ {
					if setK != nil {
						v := reflect.New(rt).Elem()
						if rt.Kind() >= reflect.Uint && rt.Kind() <= reflect.Uintptr {
							v.SetUint(uint64(i))
						} else {
							v.SetInt(i)
						}
						setK(fr, v)
					}
					if stop, r := run(fr, body(fr)); stop {
						return r
					}
				}
				return cNext
			}
		}
		// string
		setK := mkVar(s.Key, types.Typ[types.Int])
		setV := mkVar(s.Value, types.Typ[types.Rune])
		setK, setV = pairAssign(setK, setV)
		str := c.strExpr(s.X)
		return func(fr *frame) ctrl {
			sv := str(fr)
			for i := 0; i < len(sv); {
				r, size := utf8.DecodeRuneInString(sv[i:])
				if setK != nil {
					setK(fr, reflect.ValueOf(i))
				}
				if setV != nil {
					setV(fr, reflect.ValueOf(r))
				}
				i += size
				if stop, res := run(fr, body(fr)); stop {
					return res
				}
			}
			return cNext
		}
	case *types.Slice, *types.Array, *types.Pointer:
		var et types.Type
		isPtr := false
		switch u := u.(type) {
		case *types.Slice:
			et = u.Elem()
		case *types.Array:
			et = u.Elem()
		case *types.Pointer:
			et = u.Elem().Underlying().(*types.Array).Elem()
			isPtr = true
		}
		setK := mkVar(s.Key, types.Typ[types.Int])
		setV := mkVar(s.Value, et)
		setK, setV = pairAssign(setK, setV)
		_, isArray := u.(*types.Array)
		crt := c.in.rtype(xt)
		if isPtr {
			crt = crt.Elem()
		}
		view := viewer(crt.Elem(), c.in.rtype(et))
		return func(fr *frame) ctrl {
			v := x(fr)
			if isPtr {
				if v.IsNil() {
					if setV != nil {
						panic(runtimeError("invalid memory address or nil pointer dereference"))
					}
					v = reflect.Zero(v.Type().Elem())
				} else {
					v = v.Elem()
				}
			} else if isArray && setV != nil {
				v = copyValue(v) // ranging over an array evaluates a copy
			}
			n := v.Len()
			for i := 0; i < n; i++ {
				if setK != nil {
					setK(fr, reflect.ValueOf(i))
				}
				if setV != nil {
					if view != nil {
						setV(fr, view(v.Index(i)))
					} else {
						setV(fr, v.Index(i))
					}
				}
				if stop, r := run(fr, body(fr)); stop {
					return r
				}
			}
			return cNext
		}
	case *types.Map:
		setK := mkVar(s.Key, u.Key())
		setV := mkVar(s.Value, u.Elem())
		setK, setV = pairAssign(setK, setV)
		krt, vrt := c.in.rtype(u.Key()), c.in.rtype(u.Elem())
		return func(fr *frame) ctrl {
			m := x(fr)
			if m.IsNil() {
				return cNext
			}
			iter := m.MapRange()
			for iter.Next() {
				if setK != nil {
					setK(fr, reinterpret(iter.Key(), krt))
				}
				if setV != nil {
					setV(fr, reinterpret(iter.Value(), vrt))
				}
				if stop, r := run(fr, body(fr)); stop {
					return r
				}
			}
			return cNext
		}
	case *types.Chan:
		setK := mkVar(s.Key, u.Elem())
		ert := c.in.rtype(u.Elem())
		return func(fr *frame) ctrl {
			ch := x(fr)
			if ch.IsNil() {
				select {}
			}
			for {
				v, ok := ch.Recv()
				if !ok {
					return cNext
				}
				if setK != nil {
					setK(fr, reinterpret(v, ert))
				}
				if stop, r := run(fr, body(fr)); stop {
					return r
				}
			}
		}
	case *types.Signature:
		return c.rangeFunc(s, u, label, body, mkVar)
	}
	panic(fmt.Sprintf("wy: cannot range over %s", xt))
}

// rangeFunc implements range over an iterator function: the loop body runs
// inside the yield callback, and break/continue/return/labeled jumps are
// carried out of it.
func (c *compiler) rangeFunc(s *ast.RangeStmt, sig *types.Signature, label string, body exec,
	mkVar func(ast.Expr, types.Type) func(*frame, reflect.Value)) exec {
	yieldSig := sig.Params().At(0).Type().Underlying().(*types.Signature)
	var setters []func(*frame, reflect.Value)
	if yieldSig.Params().Len() > 0 {
		setters = append(setters, mkVar(s.Key, yieldSig.Params().At(0).Type()))
	}
	if yieldSig.Params().Len() > 1 {
		setters = append(setters, mkVar(s.Value, yieldSig.Params().At(1).Type()))
	}
	iterFn := c.expr(s.X)
	yieldRT := c.in.rtype(sig.Params().At(0).Type())
	in := c.in
	return func(fr *frame) ctrl {
		ctl := &iterControl{}
		yield := in.builtinFunc("yield", yieldSig, func(args []reflect.Value) []reflect.Value {
			if ctl.done {
				panic(runtimeError("range function continued iteration after function for loop body returned false"))
			}
			for i, set := range setters {
				if set != nil && i < len(args) {
					set(fr, args[i])
				}
			}
			r := body(fr)
			switch r {
			case cNext:
				return []reflect.Value{trueV}
			case cContinue:
				if fr.label == "" || fr.label == label {
					fr.label = ""
					return []reflect.Value{trueV}
				}
			case cBreak:
				if fr.label == "" || fr.label == label {
					fr.label = ""
					ctl.done = true
					return []reflect.Value{falseV}
				}
			}
			ctl.pending, ctl.label, ctl.done = r, fr.label, true
			return []reflect.Value{falseV}
		})
		yv := in.funcValue(&closure{fn: yield}, yieldRT)
		in.callValue(iterFn(fr), sig, []reflect.Value{yv}, false, nil)
		ctl.done = true
		if ctl.pending != cNext {
			fr.label = ctl.label
			return ctl.pending
		}
		return cNext
	}
}
