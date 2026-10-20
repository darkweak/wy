package tree

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"unicode/utf8"
)

// block compiles a statement list, resolving gotos to labels in it.
func (c *compiler) block(list []ast.Stmt) exec {
	stmts := make([]exec, 0, len(list))
	labels := map[string]int{}
	for _, s := range list {
		if ls, ok := s.(*ast.LabeledStmt); ok {
			labels[ls.Label.Name] = len(stmts)
		}
		stmts = append(stmts, c.stmt(s))
	}
	switch len(stmts) {
	case 0:
		return func(*frame) ctrl { return cNext }
	case 1:
		if len(labels) == 0 {
			return stmts[0]
		}
	}
	return func(fr *frame) ctrl {
		for i := 0; i < len(stmts); i++ {
			r := stmts[i](fr)
			if r == cNext {
				continue
			}
			if r == cGoto {
				if j, ok := labels[fr.label]; ok {
					fr.label = ""
					i = j - 1
					continue
				}
			}
			return r
		}
		return cNext
	}
}

func (c *compiler) stmt(s ast.Stmt) exec {
	switch s := s.(type) {
	case *ast.EmptyStmt:
		return func(*frame) ctrl { return cNext }
	case *ast.ExprStmt:
		if call, ok := ast.Unparen(s.X).(*ast.CallExpr); ok {
			f := c.exprN(call)
			return func(fr *frame) ctrl { f(fr); return cNext }
		}
		if u, ok := ast.Unparen(s.X).(*ast.UnaryExpr); ok && u.Op == token.ARROW {
			f := c.recv(u, false)
			return func(fr *frame) ctrl { f(fr); return cNext }
		}
		f := c.expr(s.X)
		return func(fr *frame) ctrl { f(fr); return cNext }
	case *ast.AssignStmt:
		return c.assign(s)
	case *ast.IncDecStmt:
		op := token.ADD
		if s.Tok == token.DEC {
			op = token.SUB
		}
		return c.opAssign(s.X, op, nil)
	case *ast.DeclStmt:
		return c.declStmt(s)
	case *ast.BlockStmt:
		return c.block(s.List)
	case *ast.IfStmt:
		return c.ifStmt(s)
	case *ast.ForStmt:
		return c.forStmt(s, "")
	case *ast.RangeStmt:
		return c.rangeStmt(s, "")
	case *ast.SwitchStmt:
		return c.switchStmt(s, "")
	case *ast.TypeSwitchStmt:
		return c.typeSwitch(s, "")
	case *ast.SelectStmt:
		return c.selectStmt(s, "")
	case *ast.LabeledStmt:
		return c.labeled(s)
	case *ast.BranchStmt:
		return c.branch(s)
	case *ast.ReturnStmt:
		return c.returnStmt(s)
	case *ast.GoStmt:
		return c.goStmt(s)
	case *ast.DeferStmt:
		return c.deferStmt(s)
	case *ast.SendStmt:
		ch := c.expr(s.Chan)
		v := c.convertedExpr(s.Value, c.typeOf(s.Chan).Underlying().(*types.Chan).Elem())
		est := c.in.rtype(c.typeOf(s.Chan)).Elem()
		return func(fr *frame) ctrl {
			cv := ch(fr)
			val := storeAs(est, v(fr))
			if cv.IsNil() {
				select {}
			}
			cv.Send(val)
			return cNext
		}
	}
	panic(fmt.Sprintf("wy: unsupported statement %T at %s", s, c.pos(s)))
}

// --- declarations and assignment -----------------------------------------

func (c *compiler) declStmt(s *ast.DeclStmt) exec {
	gd := s.Decl.(*ast.GenDecl)
	if gd.Tok != token.VAR {
		return func(*frame) ctrl { return cNext }
	}
	var execs []exec
	for _, spec := range gd.Specs {
		vs := spec.(*ast.ValueSpec)
		var lhs []ast.Expr
		for _, id := range vs.Names {
			lhs = append(lhs, id)
		}
		execs = append(execs, c.define(lhs, vs.Values, true))
	}
	return seq(execs)
}

func seq(execs []exec) exec {
	if len(execs) == 1 {
		return execs[0]
	}
	return func(fr *frame) ctrl {
		for _, e := range execs {
			e(fr)
		}
		return cNext
	}
}

// newVar allocates a slot for a variable defined at this point; executing
// the returned function creates a fresh variable (each execution of a
// declaration creates a new variable, which closures may capture).
func (c *compiler) newVar(id *ast.Ident) (slot int, rt reflect.Type, ok bool) {
	obj := c.info.Defs[id]
	if obj == nil || id.Name == "_" {
		return 0, nil, false
	}
	if s, exists := c.fs.slots[obj]; exists {
		return s, c.in.rtype(c.subst.typ(obj.Type())), true
	}
	return c.fs.newSlot(obj), c.in.rtype(c.subst.typ(obj.Type())), true
}

// define compiles x, y := a, b and var x, y = a, b. For :=, identifiers
// that already exist are assigned instead.
func (c *compiler) define(lhs, rhs []ast.Expr, isVar bool) exec {
	type target struct {
		slot  int
		rt    reflect.Type
		fresh bool
		store func(fr *frame, v reflect.Value)
		typ   types.Type
	}
	targets := make([]target, len(lhs))
	for i, l := range lhs {
		id := l.(*ast.Ident)
		if id.Name == "_" {
			targets[i] = target{slot: -1}
			if len(rhs) > 0 {
				targets[i].typ = nil
			}
			continue
		}
		if obj, ok := c.info.Defs[id]; ok && obj != nil {
			if g, isGlobal := c.in.globals[obj]; isGlobal {
				// A session-level variable (interactive input).
				targets[i] = target{slot: -1, typ: c.subst.typ(obj.Type()), store: func(_ *frame, v reflect.Value) { g.Set(v) }}
				continue
			}
			slot, rt, _ := c.newVar(id)
			targets[i] = target{slot: slot, rt: rt, fresh: true, typ: c.subst.typ(obj.Type())}
			continue
		}
		// Redeclared in :=, i.e. assigned.
		obj := c.info.Uses[id].(*types.Var)
		ref := c.varRef(obj)
		targets[i] = target{slot: -1, typ: c.subst.typ(obj.Type()), store: func(fr *frame, v reflect.Value) { ref(fr).Set(v) }}
	}
	if len(rhs) == 0 {
		return func(fr *frame) ctrl {
			for _, t := range targets {
				if t.fresh {
					fr.vars[t.slot] = reflect.New(t.rt).Elem()
				}
			}
			return cNext
		}
	}
	var vals evalN
	var srcTypes []types.Type
	if len(rhs) == 1 && len(lhs) > 1 {
		vals = c.exprN(rhs[0])
		srcTypes = tupleTypes(c.typeOf(rhs[0]), len(lhs))
	} else {
		evals := make([]eval, len(rhs))
		for i, r := range rhs {
			if targets[i].typ != nil {
				evals[i] = c.elemExpr(r, targets[i].typ)
			} else {
				evals[i] = c.expr(r)
			}
			srcTypes = append(srcTypes, nil)
		}
		vals = func(fr *frame) []reflect.Value {
			out := make([]reflect.Value, len(evals))
			for i, e := range evals {
				out[i] = e(fr)
			}
			return out
		}
	}
	convs := make([]func(reflect.Value) reflect.Value, len(lhs))
	for i := range lhs {
		if srcTypes[i] != nil && targets[i].typ != nil {
			convs[i] = c.assignConv(srcTypes[i], targets[i].typ)
		}
	}
	if len(rhs) == 0 {
		// handled above
	}
	if len(lhs) == 1 && targets[0].fresh {
		t := targets[0]
		single := c.elemExpr(rhs[0], t.typ)
		return func(fr *frame) ctrl {
			v := reflect.New(t.rt).Elem()
			v.Set(single(fr))
			fr.vars[t.slot] = v
			return cNext
		}
	}
	return func(fr *frame) ctrl {
		vs := vals(fr)
		for i, t := range targets {
			v := vs[i]
			if convs[i] != nil {
				v = convs[i](v)
			}
			switch {
			case t.fresh:
				nv := reflect.New(t.rt).Elem()
				nv.Set(v)
				fr.vars[t.slot] = nv
			case t.store != nil:
				t.store(fr, v)
			}
		}
		return cNext
	}
}

func tupleTypes(t types.Type, n int) []types.Type {
	out := make([]types.Type, n)
	if tup, ok := t.(*types.Tuple); ok {
		for i := 0; i < n && i < tup.Len(); i++ {
			out[i] = tup.At(i).Type()
		}
		return out
	}
	// Comma-ok forms: value and bool.
	out[0] = t
	if n > 1 {
		out[1] = types.Typ[types.Bool]
	}
	return out
}

// store compiles an assignment target: it returns a function storing a
// value (already converted to the target type).
func (c *compiler) store(l ast.Expr) (func(fr *frame) func(v reflect.Value), types.Type) {
	l = ast.Unparen(l)
	if id, ok := l.(*ast.Ident); ok && id.Name == "_" {
		return func(*frame) func(reflect.Value) { return func(reflect.Value) {} }, nil
	}
	t := c.typeOf(l)
	if ix, ok := l.(*ast.IndexExpr); ok {
		if mt, ok := c.typeOf(ix.X).Underlying().(*types.Map); ok {
			m := c.expr(ix.X)
			k := c.mapKey(ix.Index, mt, c.typeOf(ix.X))
			vst := c.in.rtype(c.typeOf(ix.X)).Elem()
			return func(fr *frame) func(reflect.Value) {
				mv, kv := m(fr), k(fr)
				return func(v reflect.Value) {
					if mv.IsNil() {
						panic(plainError("assignment to entry in nil map"))
					}
					mv.SetMapIndex(kv, storeAs(vst, v))
				}
			}, t
		}
	}
	// Go assigns in two phases: the operands of index expressions and
	// pointer indirections are evaluated first, and the indexing,
	// indirection (with its checks) and store happen in the second phase.
	switch x := l.(type) {
	case *ast.StarExpr:
		p := c.expr(x.X)
		return func(fr *frame) func(reflect.Value) {
			pv := p(fr)
			return func(v reflect.Value) { derefChecked(pv).Set(v) }
		}, t
	case *ast.IndexExpr:
		xt := c.typeOf(x.X)
		_, isPtr := xt.Underlying().(*types.Pointer)
		var base eval
		if _, isArr := xt.Underlying().(*types.Array); isArr {
			base = c.addr(x.X)
		} else {
			base = c.expr(x.X)
		}
		idx := c.intIndex(x.Index)
		return func(fr *frame) func(reflect.Value) {
			b, i := base(fr), idx(fr)
			if !isPtr && b.Kind() != reflect.Array {
				b = copyValue(b) // a slice operand is evaluated now
			} else if isPtr {
				b = copyValue(b)
			}
			return func(v reflect.Value) {
				if isPtr {
					b = derefChecked(b)
				}
				if i < 0 || i >= b.Len() {
					panic(runtimeError(fmt.Sprintf("index out of range [%d] with length %d", i, b.Len())))
				}
				slot := b.Index(i)
				slot.Set(storeAs(slot.Type(), v))
			}
		}, t
	case *ast.SelectorExpr:
		if sel, ok := c.info.Selections[x]; ok && sel.Kind() == types.FieldVal {
			base := c.addr(x.X)
			var cur reflect.Value
			path := c.fieldPath(func(*frame) reflect.Value { return cur }, c.typeOf(x.X), sel.Index())
			_, viaPtr := c.typeOf(x.X).Underlying().(*types.Pointer)
			return func(fr *frame) func(reflect.Value) {
				b := base(fr)
				if viaPtr {
					b = copyValue(b) // the pointer's value now, not the variable
				}
				return func(v reflect.Value) {
					cur = b
					path(fr).Set(v)
				}
			}, t
		}
	}
	ref := c.addr(l)
	return func(fr *frame) func(reflect.Value) {
		dst := ref(fr)
		return func(v reflect.Value) { dst.Set(v) }
	}, t
}

func (c *compiler) assign(s *ast.AssignStmt) exec {
	switch s.Tok {
	case token.DEFINE:
		return c.define(s.Lhs, s.Rhs, false)
	case token.ASSIGN:
	default:
		op := map[token.Token]token.Token{
			token.ADD_ASSIGN: token.ADD, token.SUB_ASSIGN: token.SUB, token.MUL_ASSIGN: token.MUL,
			token.QUO_ASSIGN: token.QUO, token.REM_ASSIGN: token.REM, token.AND_ASSIGN: token.AND,
			token.OR_ASSIGN: token.OR, token.XOR_ASSIGN: token.XOR, token.SHL_ASSIGN: token.SHL,
			token.SHR_ASSIGN: token.SHR, token.AND_NOT_ASSIGN: token.AND_NOT,
		}[s.Tok]
		return c.opAssign(s.Lhs[0], op, s.Rhs[0])
	}
	// Fast path: local scalar = expression.
	if len(s.Lhs) == 1 && len(s.Rhs) == 1 {
		if slot, ok := c.localSlot(s.Lhs[0]); ok {
			if f := c.scalarSetter(slot, s.Lhs[0], s.Rhs[0]); f != nil {
				return f
			}
		}
		if f := c.elemStore(s.Lhs[0], s.Rhs[0]); f != nil {
			return f
		}
		st, lt := c.store(s.Lhs[0])
		var rhs eval
		if lt == nil {
			rhs = c.expr(s.Rhs[0])
		} else {
			rhs = c.elemExpr(s.Rhs[0], lt)
		}
		return func(fr *frame) ctrl {
			set := st(fr)
			set(rhs(fr))
			return cNext
		}
	}
	// Tuple assignment: evaluate everything, copy, then store.
	stores := make([]func(fr *frame) func(reflect.Value), len(s.Lhs))
	ltypes := make([]types.Type, len(s.Lhs))
	for i, l := range s.Lhs {
		stores[i], ltypes[i] = c.store(l)
	}
	var vals evalN
	var srcTypes []types.Type
	if len(s.Rhs) == 1 {
		vals = c.exprN(s.Rhs[0])
		srcTypes = tupleTypes(c.typeOf(s.Rhs[0]), len(s.Lhs))
	} else {
		evals := make([]eval, len(s.Rhs))
		for i, r := range s.Rhs {
			if ltypes[i] != nil {
				evals[i] = c.elemExpr(r, ltypes[i])
			} else {
				evals[i] = c.expr(r)
			}
		}
		srcTypes = make([]types.Type, len(s.Rhs))
		vals = func(fr *frame) []reflect.Value {
			out := make([]reflect.Value, len(evals))
			for i, e := range evals {
				out[i] = copyValue(e(fr))
			}
			return out
		}
	}
	convs := make([]func(reflect.Value) reflect.Value, len(s.Lhs))
	for i := range s.Lhs {
		if srcTypes[i] != nil && ltypes[i] != nil {
			convs[i] = c.assignConv(srcTypes[i], ltypes[i])
		}
	}
	return func(fr *frame) ctrl {
		sets := make([]func(reflect.Value), len(stores))
		for i, st := range stores {
			sets[i] = st(fr)
		}
		vs := vals(fr)
		for i, set := range sets {
			v := vs[i]
			if convs[i] != nil {
				v = convs[i](v)
			}
			set(v)
		}
		return cNext
	}
}

func copyValue(v reflect.Value) reflect.Value {
	if !v.IsValid() || !v.CanAddr() {
		return v
	}
	n := reflect.New(v.Type()).Elem()
	n.Set(v)
	return n
}

// elemStore compiles s[i] = expr for a slice (or array variable) of a
// basic element type without allocation. Evaluation order follows Go's
// two-phase assignment: s, i and expr, then the bounds check and store.
func (c *compiler) elemStore(l, r ast.Expr) exec {
	ix, ok := ast.Unparen(l).(*ast.IndexExpr)
	if !ok {
		return nil
	}
	xt := c.typeOf(ix.X)
	var base eval
	switch xt.Underlying().(type) {
	case *types.Slice:
		base = c.expr(ix.X)
	case *types.Array:
		if _, ok := c.localSlot(ix.X); !ok {
			return nil
		}
		base = c.expr(ix.X)
	default:
		return nil
	}
	et := c.typeOf(l)
	if isInterface(et) || c.in.rtype(xt).Elem() != c.in.rtype(et) {
		return nil
	}
	if !types.Identical(c.typeOf(r).Underlying(), et.Underlying()) {
		if b, ok := c.typeOf(r).(*types.Basic); !ok || b.Info()&types.IsUntyped == 0 {
			return nil
		}
	}
	idx := c.intIndex(ix.Index)
	elem := func(fr *frame, b reflect.Value, i int) reflect.Value {
		if i < 0 || i >= b.Len() {
			panic(runtimeError(fmt.Sprintf("index out of range [%d] with length %d", i, b.Len())))
		}
		return b.Index(i)
	}
	switch k, _ := classOf(et); k {
	case kInt:
		f := c.intExpr(r)
		return func(fr *frame) ctrl { b, i := base(fr), idx(fr); v := f(fr); elem(fr, b, i).SetInt(v); return cNext }
	case kUint:
		f := c.uintExpr(r)
		return func(fr *frame) ctrl { b, i := base(fr), idx(fr); v := f(fr); elem(fr, b, i).SetUint(v); return cNext }
	case kFloat:
		f := c.floatExpr(r)
		return func(fr *frame) ctrl { b, i := base(fr), idx(fr); v := f(fr); elem(fr, b, i).SetFloat(v); return cNext }
	case kString:
		f := c.strExpr(r)
		return func(fr *frame) ctrl { b, i := base(fr), idx(fr); v := f(fr); elem(fr, b, i).SetString(v); return cNext }
	case kBool:
		f := c.boolExpr(r)
		return func(fr *frame) ctrl { b, i := base(fr), idx(fr); v := f(fr); elem(fr, b, i).SetBool(v); return cNext }
	}
	return nil
}

// scalarSetter compiles local = expr for basic types without allocation.
func (c *compiler) scalarSetter(slot int, l, r ast.Expr) exec {
	lt := c.typeOf(l)
	if isInterface(lt) {
		return nil
	}
	if !types.Identical(c.typeOf(r).Underlying(), lt.Underlying()) {
		if b, ok := c.typeOf(r).(*types.Basic); !ok || b.Info()&types.IsUntyped == 0 {
			return nil
		}
	}
	switch k, _ := classOf(lt); k {
	case kInt:
		f := c.intExpr(r)
		return func(fr *frame) ctrl { fr.vars[slot].SetInt(f(fr)); return cNext }
	case kUint:
		f := c.uintExpr(r)
		return func(fr *frame) ctrl { fr.vars[slot].SetUint(f(fr)); return cNext }
	case kFloat:
		f := c.floatExpr(r)
		return func(fr *frame) ctrl { fr.vars[slot].SetFloat(f(fr)); return cNext }
	case kString:
		f := c.strExpr(r)
		return func(fr *frame) ctrl { fr.vars[slot].SetString(f(fr)); return cNext }
	case kBool:
		f := c.boolExpr(r)
		return func(fr *frame) ctrl { fr.vars[slot].SetBool(f(fr)); return cNext }
	}
	return nil
}

// opAssign compiles x op= y (and x++, x-- with r == nil), evaluating x's
// location once.
func (c *compiler) opAssign(l ast.Expr, op token.Token, r ast.Expr) exec {
	lt := c.typeOf(l)
	if slot, ok := c.localSlot(l); ok && (op == token.ADD || op == token.SUB) {
		if k, bits := classOf(lt); k == kInt {
			var y intFn = func(*frame) int64 { return 1 }
			if r != nil {
				y = c.intExpr(r)
			}
			if op == token.SUB {
				inner := y
				y = func(fr *frame) int64 { return -inner(fr) }
			}
			return func(fr *frame) ctrl {
				v := fr.vars[slot]
				v.SetInt(truncInt(v.Int()+y(fr), bits))
				return cNext
			}
		}
	}
	rt := c.in.rtype(lt)
	k, bits := classOf(lt)
	// Map elements are not addressable: read, compute, write back.
	var loc func(fr *frame) (get func() reflect.Value, set func(reflect.Value))
	if ix, ok := ast.Unparen(l).(*ast.IndexExpr); ok {
		if mt, ok := c.typeOf(ix.X).Underlying().(*types.Map); ok {
			m := c.expr(ix.X)
			key := c.convertedExpr(ix.Index, mt.Key())
			zero := reflect.Zero(rt)
			loc = func(fr *frame) (func() reflect.Value, func(reflect.Value)) {
				mv, kv := m(fr), key(fr)
				return func() reflect.Value {
						v := mv.MapIndex(kv)
						if !v.IsValid() {
							return zero
						}
						return v
					}, func(v reflect.Value) {
						if mv.IsNil() {
							panic(plainError("assignment to entry in nil map"))
						}
						mv.SetMapIndex(kv, v)
					}
			}
		}
	}
	if loc == nil {
		ref := c.addr(l)
		loc = func(fr *frame) (func() reflect.Value, func(reflect.Value)) {
			dst := ref(fr)
			return func() reflect.Value { return dst }, func(v reflect.Value) { dst.Set(v) }
		}
	}
	switch k {
	case kInt:
		var y intFn
		switch {
		case r == nil:
			y = func(*frame) int64 { return 1 }
		case op == token.SHL || op == token.SHR:
			n := c.shiftCount(r)
			y = func(fr *frame) int64 { return int64(min(n(fr), 64)) }
		default:
			y = c.intExpr(r)
		}
		return func(fr *frame) ctrl {
			get, set := loc(fr)
			a := get().Int()
			b := y(fr)
			var res int64
			switch op {
			case token.ADD:
				res = a + b
			case token.SUB:
				res = a - b
			case token.MUL:
				res = a * b
			case token.QUO:
				res = a / b
			case token.REM:
				res = a % b
			case token.AND:
				res = a & b
			case token.OR:
				res = a | b
			case token.XOR:
				res = a ^ b
			case token.AND_NOT:
				res = a &^ b
			case token.SHL:
				if b >= 64 {
					res = 0
				} else {
					res = a << b
				}
			case token.SHR:
				if b >= 64 {
					b = 63
				}
				res = a >> b
			}
			v := reflect.New(rt).Elem()
			v.SetInt(truncInt(res, bits))
			set(v)
			return cNext
		}
	case kUint:
		var y uintFn
		switch {
		case r == nil:
			y = func(*frame) uint64 { return 1 }
		case op == token.SHL || op == token.SHR:
			y = c.shiftCount(r)
		default:
			y = c.uintExpr(r)
		}
		return func(fr *frame) ctrl {
			get, set := loc(fr)
			a := get().Uint()
			b := y(fr)
			var res uint64
			switch op {
			case token.ADD:
				res = a + b
			case token.SUB:
				res = a - b
			case token.MUL:
				res = a * b
			case token.QUO:
				res = a / b
			case token.REM:
				res = a % b
			case token.AND:
				res = a & b
			case token.OR:
				res = a | b
			case token.XOR:
				res = a ^ b
			case token.AND_NOT:
				res = a &^ b
			case token.SHL:
				if b >= 64 {
					res = 0
				} else {
					res = a << b
				}
			case token.SHR:
				if b >= 64 {
					res = 0
				} else {
					res = a >> b
				}
			}
			v := reflect.New(rt).Elem()
			v.SetUint(truncUint(res, bits))
			set(v)
			return cNext
		}
	case kFloat:
		y := func(*frame) float64 { return 1 }
		if r != nil {
			y = c.floatExpr(r)
		}
		return func(fr *frame) ctrl {
			get, set := loc(fr)
			a := get().Float()
			b := y(fr)
			var res float64
			switch op {
			case token.ADD:
				res = a + b
			case token.SUB:
				res = a - b
			case token.MUL:
				res = a * b
			case token.QUO:
				res = a / b
			}
			v := reflect.New(rt).Elem()
			v.SetFloat(res)
			set(v)
			return cNext
		}
	case kComplex:
		y := func(*frame) complex128 { return 1 }
		if r != nil {
			y = c.complexExpr(r)
		}
		return func(fr *frame) ctrl {
			get, set := loc(fr)
			a := get().Complex()
			b := y(fr)
			var res complex128
			switch op {
			case token.ADD:
				res = a + b
			case token.SUB:
				res = a - b
			case token.MUL:
				res = a * b
			case token.QUO:
				res = a / b
			}
			v := reflect.New(rt).Elem()
			v.SetComplex(res)
			set(v)
			return cNext
		}
	case kString:
		y := c.strExpr(r)
		return func(fr *frame) ctrl {
			get, set := loc(fr)
			v := reflect.New(rt).Elem()
			v.SetString(get().String() + y(fr))
			set(v)
			return cNext
		}
	}
	panic(fmt.Sprintf("wy: unsupported assignment operator at %s", c.pos(l)))
}

// --- control flow --------------------------------------------------------

func (c *compiler) ifStmt(s *ast.IfStmt) exec {
	var init exec
	if s.Init != nil {
		init = c.stmt(s.Init)
	}
	cond := c.boolExpr(s.Cond)
	then := c.block(s.Body.List)
	var els exec
	if s.Else != nil {
		els = c.stmt(s.Else)
	}
	return func(fr *frame) ctrl {
		if init != nil {
			init(fr)
		}
		if cond(fr) {
			return then(fr)
		}
		if els != nil {
			return els(fr)
		}
		return cNext
	}
}

// loopCtl interprets a body's control code for a loop with the given label:
// it returns (stop, result).
func loopCtl(fr *frame, r ctrl, label string) (bool, ctrl) {
	switch r {
	case cBreak:
		if fr.label == "" || fr.label == label {
			fr.label = ""
			return true, cNext
		}
		return true, r
	case cContinue:
		if fr.label == "" || fr.label == label {
			fr.label = ""
			return false, cNext
		}
		return true, r
	case cReturn, cGoto:
		return true, r
	}
	return false, cNext
}

func (c *compiler) forStmt(s *ast.ForStmt, label string) exec {
	var init, post exec
	var loopVars []int
	if s.Init != nil {
		init = c.stmt(s.Init)
		if as, ok := s.Init.(*ast.AssignStmt); ok && as.Tok == token.DEFINE {
			for _, l := range as.Lhs {
				if obj := c.info.Defs[l.(*ast.Ident)]; obj != nil {
					loopVars = append(loopVars, c.fs.slots[obj])
				}
			}
		}
	}
	cond := func(*frame) bool { return true }
	if s.Cond != nil {
		cond = c.boolExpr(s.Cond)
	}
	if s.Post != nil {
		post = c.stmt(s.Post)
	}
	body := c.block(s.Body.List)
	// Go 1.22 semantics: each iteration has its own copy of the loop
	// variables, but only closures can tell; copy only if the body may
	// capture them.
	perIter := len(loopVars) > 0 && containsFuncLit(s.Body)
	return func(fr *frame) ctrl {
		if init != nil {
			init(fr)
		}
		for cond(fr) {
			stop, r := loopCtl(fr, body(fr), label)
			if stop {
				return r
			}
			if perIter {
				for _, slot := range loopVars {
					old := fr.vars[slot]
					nv := reflect.New(old.Type()).Elem()
					nv.Set(old)
					fr.vars[slot] = nv
				}
			}
			if post != nil {
				post(fr)
			}
		}
		return cNext
	}
}

func containsFuncLit(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncLit, *ast.GoStmt, *ast.DeferStmt:
			found = true
		case *ast.UnaryExpr:
			if n.(*ast.UnaryExpr).Op == token.AND {
				found = true
			}
		}
		return !found
	})
	return found
}

func (c *compiler) labeled(s *ast.LabeledStmt) exec {
	label := s.Label.Name
	switch inner := s.Stmt.(type) {
	case *ast.ForStmt:
		return c.forStmt(inner, label)
	case *ast.RangeStmt:
		return c.rangeStmt(inner, label)
	case *ast.SwitchStmt:
		return c.switchStmt(inner, label)
	case *ast.TypeSwitchStmt:
		return c.typeSwitch(inner, label)
	case *ast.SelectStmt:
		return c.selectStmt(inner, label)
	}
	return c.stmt(s.Stmt)
}

func (c *compiler) branch(s *ast.BranchStmt) exec {
	label := ""
	if s.Label != nil {
		label = s.Label.Name
	}
	var r ctrl
	switch s.Tok {
	case token.BREAK:
		r = cBreak
	case token.CONTINUE:
		r = cContinue
	case token.GOTO:
		r = cGoto
	case token.FALLTHROUGH:
		return func(*frame) ctrl { return cFallthrough }
	}
	return func(fr *frame) ctrl {
		fr.label = label
		return r
	}
}

func (c *compiler) returnStmt(s *ast.ReturnStmt) exec {
	fs := c.fs
	if len(s.Results) == 0 {
		return func(*frame) ctrl { return cReturn }
	}
	slots := fs.results
	if len(s.Results) == 1 && len(slots) > 1 {
		vals := c.exprN(s.Results[0])
		srcs := tupleTypes(c.typeOf(s.Results[0]), len(slots))
		convs := make([]func(reflect.Value) reflect.Value, len(slots))
		for i := range slots {
			convs[i] = c.assignConv(srcs[i], fs.resTypes[i])
		}
		return func(fr *frame) ctrl {
			vs := vals(fr)
			for i, slot := range slots {
				fr.vars[slot].Set(convs[i](vs[i]))
			}
			return cReturn
		}
	}
	if len(slots) == 1 {
		slot := slots[0]
		if k, _ := classOf(fs.resTypes[0]); k == kInt && !isInterface(fs.resTypes[0]) {
			f := c.intExpr(s.Results[0])
			return func(fr *frame) ctrl { fr.vars[slot].SetInt(f(fr)); return cReturn }
		}
		v := c.elemExpr(s.Results[0], fs.resTypes[0])
		return func(fr *frame) ctrl {
			fr.vars[slot].Set(v(fr))
			return cReturn
		}
	}
	evals := make([]eval, len(s.Results))
	for i, r := range s.Results {
		evals[i] = c.elemExpr(r, fs.resTypes[i])
	}
	return func(fr *frame) ctrl {
		vals := make([]reflect.Value, len(evals))
		for i, e := range evals {
			vals[i] = copyValue(e(fr))
		}
		for i, slot := range slots {
			fr.vars[slot].Set(vals[i])
		}
		return cReturn
	}
}

// --- switch ----------------------------------------------------------------

func (c *compiler) switchStmt(s *ast.SwitchStmt, label string) exec {
	var init exec
	if s.Init != nil {
		init = c.stmt(s.Init)
	}
	type clause struct {
		conds []boolFn
		body  exec
	}
	var clauses []clause
	def := -1
	var tag eval
	var tagT types.Type
	tagSlot := -1
	if s.Tag != nil {
		tag = c.expr(s.Tag)
		tagT = c.typeOf(s.Tag)
		tagSlot = c.fs.newSlot(nil)
	}
	for i, st := range s.Body.List {
		cc := st.(*ast.CaseClause)
		var conds []boolFn
		if cc.List == nil {
			def = i
		}
		for _, e := range cc.List {
			if tag == nil {
				if k, _ := classOf(c.typeOf(e)); k == kBool {
					conds = append(conds, c.boolExpr(e))
					continue
				}
				// A tagless switch compares cases with true.
				x := c.expr(e)
				in := c.in
				tv := c.in.toIface(reflect.ValueOf(true), types.Typ[types.Bool], anyRT)
				conds = append(conds, func(fr *frame) bool { return in.ifaceEqual(x(fr), tv) })
				continue
			}
			conds = append(conds, c.caseEqual(e, tagT, tagSlot))
		}
		clauses = append(clauses, clause{conds, c.block(cc.Body)})
	}
	return func(fr *frame) ctrl {
		if init != nil {
			init(fr)
		}
		if tag != nil {
			fr.vars[tagSlot] = copyValue(tag(fr))
		}
		chosen := -1
	search:
		for i, cl := range clauses {
			for _, cond := range cl.conds {
				if cond(fr) {
					chosen = i
					break search
				}
			}
		}
		if chosen < 0 {
			chosen = def
		}
		for chosen >= 0 && chosen < len(clauses) {
			r := clauses[chosen].body(fr)
			switch r {
			case cFallthrough:
				chosen++
				continue
			case cBreak:
				if fr.label == "" || fr.label == label {
					fr.label = ""
					return cNext
				}
			}
			return r
		}
		return cNext
	}
}

// caseEqual compares the switch tag (kept in tagSlot) with a case
// expression.
func (c *compiler) caseEqual(e ast.Expr, tagT types.Type, tagSlot int) boolFn {
	x := c.expr(e)
	xt := c.typeOf(e)
	in := c.in
	tagI, xI := isInterface(tagT), isInterface(xt)
	var conv func(reflect.Value) reflect.Value
	if tagI && !xI {
		conv = c.assignConv(xt, tagT)
	}
	var tconv func(reflect.Value) reflect.Value
	if xI && !tagI {
		tconv = c.assignConv(tagT, xt)
	}
	return func(fr *frame) bool {
		t := fr.vars[tagSlot]
		v := x(fr)
		if conv != nil {
			v = conv(v)
		}
		if tconv != nil {
			t = tconv(t)
		}
		if !v.IsValid() {
			return isNilValue(t)
		}
		return in.valuesEqual(t, v)
	}
}

func (c *compiler) typeSwitch(s *ast.TypeSwitchStmt, label string) exec {
	var init exec
	if s.Init != nil {
		init = c.stmt(s.Init)
	}
	var x eval
	var bind *ast.Ident
	switch a := s.Assign.(type) {
	case *ast.AssignStmt:
		bind = a.Lhs[0].(*ast.Ident)
		x = c.expr(a.Rhs[0].(*ast.TypeAssertExpr).X)
	case *ast.ExprStmt:
		x = c.expr(a.X.(*ast.TypeAssertExpr).X)
	}
	type clause struct {
		checks  []func(reflect.Value) (reflect.Value, bool) // nil check entry means "case nil"
		nilCase bool
		slot    int
		rt      reflect.Type
		bindAny bool
		body    exec
	}
	var clauses []clause
	def := -1
	in := c.in
	for i, st := range s.Body.List {
		cc := st.(*ast.CaseClause)
		cl := clause{slot: -1}
		if cc.List == nil {
			def = i
		}
		for _, e := range cc.List {
			if isNilExpr(c.info, e) {
				cl.nilCase = true
				continue
			}
			cl.checks = append(cl.checks, in.assertion(c.typeOf(e)))
		}
		if bind != nil {
			if obj := c.info.Implicits[cc]; obj != nil {
				t := c.subst.typ(obj.Type())
				cl.slot = c.fs.newSlot(obj)
				cl.rt = c.in.rtype(t)
				cl.bindAny = len(cc.List) != 1 || isInterface(t) && !(len(cl.checks) == 1)
			}
		}
		cl.body = c.block(cc.Body)
		clauses = append(clauses, cl)
	}
	return func(fr *frame) ctrl {
		if init != nil {
			init(fr)
		}
		iv := x(fr)
		chosen := -1
		var val reflect.Value
	search:
		for i, cl := range clauses {
			if cl.nilCase && iv.IsNil() {
				chosen = i
				break
			}
			for _, chk := range cl.checks {
				if v, ok := chk(iv); ok {
					chosen, val = i, v
					break search
				}
			}
		}
		if chosen < 0 {
			chosen = def
		}
		if chosen < 0 {
			return cNext
		}
		cl := clauses[chosen]
		if cl.slot >= 0 {
			nv := reflect.New(cl.rt).Elem()
			switch {
			case len(cl.checks) == 1 && val.IsValid() && !cl.nilCase:
				nv.Set(val)
			case cl.rt.Kind() == reflect.Interface && !iv.IsNil():
				nt, e := in.dyn(iv)
				nv.Set(in.ifaceFrom(nt, e, cl.rt))
			}
			fr.vars[cl.slot] = nv
		}
		r := cl.body(fr)
		if r == cBreak && (fr.label == "" || fr.label == label) {
			fr.label = ""
			return cNext
		}
		return r
	}
}

// --- select ----------------------------------------------------------------

func (c *compiler) selectStmt(s *ast.SelectStmt, label string) exec {
	type comm struct {
		dir    reflect.SelectDir
		ch     eval
		val    eval
		recvV  int // slot for received value, -1
		recvOK int
		assign func(fr *frame, v reflect.Value, ok bool)
		body   exec
	}
	var comms []comm
	for _, st := range s.Body.List {
		cc := st.(*ast.CommClause)
		cm := comm{recvV: -1, recvOK: -1}
		switch x := cc.Comm.(type) {
		case nil:
			cm.dir = reflect.SelectDefault
		case *ast.SendStmt:
			cm.dir = reflect.SelectSend
			cm.ch = c.expr(x.Chan)
			cm.val = c.convertedExpr(x.Value, c.typeOf(x.Chan).Underlying().(*types.Chan).Elem())
		case *ast.ExprStmt:
			cm.dir = reflect.SelectRecv
			cm.ch = c.expr(ast.Unparen(x.X).(*ast.UnaryExpr).X)
		case *ast.AssignStmt:
			cm.dir = reflect.SelectRecv
			u := ast.Unparen(x.Rhs[0]).(*ast.UnaryExpr)
			cm.ch = c.expr(u.X)
			if x.Tok == token.DEFINE {
				for i, l := range x.Lhs {
					slot, rt, ok := c.newVar(l.(*ast.Ident))
					if !ok {
						continue
					}
					if i == 0 {
						cm.recvV = slot
						s, r := slot, rt
						prev := cm.assign
						cm.assign = func(fr *frame, v reflect.Value, ok bool) {
							nv := reflect.New(r).Elem()
							nv.Set(v)
							fr.vars[s] = nv
							if prev != nil {
								prev(fr, v, ok)
							}
						}
					} else {
						s := slot
						prev := cm.assign
						cm.assign = func(fr *frame, v reflect.Value, ok bool) {
							if prev != nil {
								prev(fr, v, ok)
							}
							nv := reflect.New(boolRT).Elem()
							nv.SetBool(ok)
							fr.vars[s] = nv
						}
					}
				}
			} else {
				var sets []func(fr *frame) func(reflect.Value)
				for _, l := range x.Lhs {
					st, _ := c.store(l)
					sets = append(sets, st)
				}
				cm.assign = func(fr *frame, v reflect.Value, ok bool) {
					sets[0](fr)(v)
					if len(sets) > 1 {
						sets[1](fr)(reflect.ValueOf(ok))
					}
				}
			}
		}
		cm.body = c.block(cc.Body)
		comms = append(comms, cm)
	}
	return func(fr *frame) ctrl {
		cases := make([]reflect.SelectCase, len(comms))
		for i, cm := range comms {
			cases[i].Dir = cm.dir
			if cm.ch != nil {
				ch := cm.ch(fr)
				cases[i].Chan = ch
				if ch.IsNil() {
					cases[i].Chan = reflect.Value{}
				}
			}
			if cm.val != nil {
				cases[i].Send = copyValue(cm.val(fr))
			}
		}
		if len(cases) == 0 {
			select {}
		}
		chosen, v, ok := reflect.Select(cases)
		cm := comms[chosen]
		if cm.assign != nil {
			if !v.IsValid() {
				v = reflect.Zero(cases[chosen].Chan.Type().Elem())
			}
			cm.assign(fr, v, ok)
		}
		r := cm.body(fr)
		if r == cBreak && (fr.label == "" || fr.label == label) {
			fr.label = ""
			return cNext
		}
		return r
	}
}

// --- go and defer ------------------------------------------------------------

// deferredCall compiles the call of a go or defer statement: function and
// arguments are evaluated immediately, the call happens later.
func (c *compiler) deferredCall(call *ast.CallExpr) func(fr *frame) func(deferOf *frame) {
	if id := builtinIdent(c.info, call.Fun); id != nil {
		// Builtins: evaluate arguments now by capturing them.
		var args []eval
		for _, a := range call.Args {
			args = append(args, c.expr(a))
		}
		b := c.builtinDeferred(id.Name, call)
		return func(fr *frame) func(*frame) {
			vals := make([]reflect.Value, len(args))
			for i, a := range args {
				vals[i] = copyValue(a(fr))
			}
			return func(deferOf *frame) { b(fr, vals, deferOf) }
		}
	}
	if tv := c.info.Types[call.Fun]; tv.IsType() {
		conv := c.conversion(call)
		return func(fr *frame) func(*frame) {
			return func(*frame) { conv(fr) }
		}
	}
	cl := c.callee(call.Fun)
	args := c.args(call, cl.sig)
	spread := call.Ellipsis.IsValid()
	return func(fr *frame) func(*frame) {
		inv := cl.bind(fr)
		vals := args(fr)
		for i := range vals {
			vals[i] = copyValue(vals[i])
		}
		return func(deferOf *frame) { inv(vals, spread, deferOf) }
	}
}

func (c *compiler) builtinDeferred(name string, call *ast.CallExpr) func(fr *frame, vals []reflect.Value, deferOf *frame) {
	switch name {
	case "recover":
		// "defer recover()" acts as a recover called by the deferring
		// function itself.
		// It is a no-op while that function is itself panicking: it then
		// runs from the panic, not from the function.
		return func(fr *frame, _ []reflect.Value, _ *frame) {
			if fr.panicking && !fr.recovered {
				return
			}
			fr.recoverValue()
		}
	case "panic":
		return func(fr *frame, vals []reflect.Value, _ *frame) {
			v := vals[0]
			if v.Kind() == reflect.Interface {
				if v.IsNil() {
					panic(nil)
				}
				panic(v.Elem().Interface())
			}
			panic(v.Interface())
		}
	}
	b := c.builtin(name, call)
	return func(fr *frame, _ []reflect.Value, _ *frame) { b(fr) }
}

func (c *compiler) goStmt(s *ast.GoStmt) exec {
	prep := c.deferredCall(s.Call)
	return func(fr *frame) ctrl {
		run := prep(fr)
		go run(nil)
		return cNext
	}
}

func (c *compiler) deferStmt(s *ast.DeferStmt) exec {
	c.fs.hasDefer = true
	prep := c.deferredCall(s.Call)
	return func(fr *frame) ctrl {
		run := prep(fr)
		fr.defers = append(fr.defers, func() { run(fr) })
		return cNext
	}
}

var _ = utf8.RuneError
