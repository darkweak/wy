package tree

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"math"
	"reflect"
	"strings"
)

// Typed fast paths. Expressions of basic type compile to closures returning
// plain Go scalars, so arithmetic and comparisons do not allocate; values
// are only materialized as reflect.Values where they must be stored.

type (
	intFn   func(*frame) int64
	uintFn  func(*frame) uint64
	floatFn func(*frame) float64
	cplxFn  func(*frame) complex128
	strFn   func(*frame) string
	boolFn  func(*frame) bool
)

type kindClass int

const (
	kOther kindClass = iota
	kInt
	kUint
	kFloat
	kComplex
	kString
	kBool
)

func classOf(t types.Type) (kindClass, int) {
	b, ok := t.Underlying().(*types.Basic)
	if !ok {
		return kOther, 0
	}
	if b.Info()&types.IsUntyped != 0 {
		b = types.Default(b).Underlying().(*types.Basic)
	}
	switch b.Kind() {
	case types.Int, types.Int64:
		return kInt, 64
	case types.Int8:
		return kInt, 8
	case types.Int16:
		return kInt, 16
	case types.Int32:
		return kInt, 32
	case types.Uint, types.Uint64, types.Uintptr:
		return kUint, 64
	case types.Uint8:
		return kUint, 8
	case types.Uint16:
		return kUint, 16
	case types.Uint32:
		return kUint, 32
	case types.Float32:
		return kFloat, 32
	case types.Float64:
		return kFloat, 64
	case types.Complex64:
		return kComplex, 64
	case types.Complex128:
		return kComplex, 128
	case types.String:
		return kString, 0
	case types.Bool:
		return kBool, 0
	}
	return kOther, 0
}

func truncInt(x int64, bits int) int64 {
	if bits == 64 {
		return x
	}
	s := 64 - bits
	return x << s >> s
}

func truncUint(x uint64, bits int) uint64 {
	if bits == 64 {
		return x
	}
	return x & (1<<bits - 1)
}

// localSlot reports whether e is a local variable of the current function
// (fast direct slot access).
func (c *compiler) localSlot(e ast.Expr) (int, bool) {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return 0, false
	}
	obj, ok := c.info.Uses[id].(*types.Var)
	if !ok {
		return 0, false
	}
	s, ok := c.fs.slots[obj]
	return s, ok
}

func (c *compiler) constOf(e ast.Expr) constant.Value {
	if tv, ok := c.info.Types[e]; ok && tv.Value != nil {
		return tv.Value
	}
	return nil
}

func (c *compiler) intExpr(e ast.Expr) intFn {
	e = ast.Unparen(e)
	_, bits := classOf(c.typeOf(e))
	if v := c.constOf(e); v != nil {
		x, _ := constant.Int64Val(constant.ToInt(v))
		return func(*frame) int64 { return x }
	}
	if s, ok := c.localSlot(e); ok {
		return func(fr *frame) int64 { return fr.vars[s].Int() }
	}
	switch e := e.(type) {
	case *ast.BinaryExpr:
		if k, _ := classOf(c.typeOf(e.X)); k == kInt && !isComparison(e.Op) && e.Op != token.SHL && e.Op != token.SHR {
			x, y := c.intExpr(e.X), c.intExpr(e.Y)
			switch e.Op {
			case token.ADD:
				return func(fr *frame) int64 { return truncInt(x(fr)+y(fr), bits) }
			case token.SUB:
				return func(fr *frame) int64 { return truncInt(x(fr)-y(fr), bits) }
			case token.MUL:
				return func(fr *frame) int64 { return truncInt(x(fr)*y(fr), bits) }
			case token.QUO:
				return func(fr *frame) int64 { a := x(fr); return truncInt(a/y(fr), bits) }
			case token.REM:
				return func(fr *frame) int64 { a := x(fr); return truncInt(a%y(fr), bits) }
			case token.AND:
				return func(fr *frame) int64 { return x(fr) & y(fr) }
			case token.OR:
				return func(fr *frame) int64 { return x(fr) | y(fr) }
			case token.XOR:
				return func(fr *frame) int64 { return x(fr) ^ y(fr) }
			case token.AND_NOT:
				return func(fr *frame) int64 { return x(fr) &^ y(fr) }
			}
		}
		if e.Op == token.SHL || e.Op == token.SHR {
			x := c.intExpr(e.X)
			y := c.shiftCount(e.Y)
			if e.Op == token.SHL {
				return func(fr *frame) int64 {
					a, n := x(fr), y(fr)
					if n >= 64 {
						return 0
					}
					return truncInt(a<<n, bits)
				}
			}
			return func(fr *frame) int64 {
				a, n := x(fr), y(fr)
				if n >= 64 {
					n = 63
				}
				return a >> n
			}
		}
	case *ast.UnaryExpr:
		switch e.Op {
		case token.SUB:
			x := c.intExpr(e.X)
			return func(fr *frame) int64 { return truncInt(-x(fr), bits) }
		case token.XOR:
			x := c.intExpr(e.X)
			return func(fr *frame) int64 { return ^x(fr) }
		case token.ADD:
			return c.intExpr(e.X)
		}
	}
	g := c.expr(e)
	return func(fr *frame) int64 { return g(fr).Int() }
}

// shiftCount compiles a shift count, panicking on negative counts.
func (c *compiler) shiftCount(e ast.Expr) uintFn {
	switch k, _ := classOf(c.typeOf(e)); k {
	case kInt:
		x := c.intExpr(e)
		return func(fr *frame) uint64 {
			n := x(fr)
			if n < 0 {
				panic(runtimeError("negative shift amount"))
			}
			return uint64(n)
		}
	default:
		return c.uintExpr(e)
	}
}

func (c *compiler) uintExpr(e ast.Expr) uintFn {
	e = ast.Unparen(e)
	_, bits := classOf(c.typeOf(e))
	if v := c.constOf(e); v != nil {
		x, _ := constant.Uint64Val(constant.ToInt(v))
		return func(*frame) uint64 { return x }
	}
	if s, ok := c.localSlot(e); ok {
		return func(fr *frame) uint64 { return fr.vars[s].Uint() }
	}
	switch e := e.(type) {
	case *ast.BinaryExpr:
		if k, _ := classOf(c.typeOf(e.X)); k == kUint && !isComparison(e.Op) && e.Op != token.SHL && e.Op != token.SHR {
			x, y := c.uintExpr(e.X), c.uintExpr(e.Y)
			switch e.Op {
			case token.ADD:
				return func(fr *frame) uint64 { return truncUint(x(fr)+y(fr), bits) }
			case token.SUB:
				return func(fr *frame) uint64 { return truncUint(x(fr)-y(fr), bits) }
			case token.MUL:
				return func(fr *frame) uint64 { return truncUint(x(fr)*y(fr), bits) }
			case token.QUO:
				return func(fr *frame) uint64 { a := x(fr); return a / y(fr) }
			case token.REM:
				return func(fr *frame) uint64 { a := x(fr); return a % y(fr) }
			case token.AND:
				return func(fr *frame) uint64 { return x(fr) & y(fr) }
			case token.OR:
				return func(fr *frame) uint64 { return x(fr) | y(fr) }
			case token.XOR:
				return func(fr *frame) uint64 { return x(fr) ^ y(fr) }
			case token.AND_NOT:
				return func(fr *frame) uint64 { return x(fr) &^ y(fr) }
			}
		}
		if e.Op == token.SHL || e.Op == token.SHR {
			x := c.uintExpr(e.X)
			y := c.shiftCount(e.Y)
			if e.Op == token.SHL {
				return func(fr *frame) uint64 {
					a, n := x(fr), y(fr)
					if n >= 64 {
						return 0
					}
					return truncUint(a<<n, bits)
				}
			}
			return func(fr *frame) uint64 {
				a, n := x(fr), y(fr)
				if n >= 64 {
					return 0
				}
				return a >> n
			}
		}
	case *ast.UnaryExpr:
		switch e.Op {
		case token.SUB:
			x := c.uintExpr(e.X)
			return func(fr *frame) uint64 { return truncUint(-x(fr), bits) }
		case token.XOR:
			x := c.uintExpr(e.X)
			return func(fr *frame) uint64 { return truncUint(^x(fr), bits) }
		case token.ADD:
			return c.uintExpr(e.X)
		}
	}
	g := c.expr(e)
	return func(fr *frame) uint64 { return g(fr).Uint() }
}

func (c *compiler) floatExpr(e ast.Expr) floatFn {
	e = ast.Unparen(e)
	_, bits := classOf(c.typeOf(e))
	round := func(x float64) float64 { return x }
	if bits == 32 {
		round = func(x float64) float64 { return float64(float32(x)) }
	}
	if v := c.constOf(e); v != nil {
		x, _ := constant.Float64Val(constant.ToFloat(v))
		x = round(x)
		return func(*frame) float64 { return x }
	}
	if s, ok := c.localSlot(e); ok {
		return func(fr *frame) float64 { return fr.vars[s].Float() }
	}
	switch e := e.(type) {
	case *ast.BinaryExpr:
		if k, _ := classOf(c.typeOf(e.X)); k == kFloat && !isComparison(e.Op) {
			x, y := c.floatExpr(e.X), c.floatExpr(e.Y)
			switch e.Op {
			case token.ADD:
				return func(fr *frame) float64 { return round(x(fr) + y(fr)) }
			case token.SUB:
				return func(fr *frame) float64 { return round(x(fr) - y(fr)) }
			case token.MUL:
				return func(fr *frame) float64 { return round(x(fr) * y(fr)) }
			case token.QUO:
				return func(fr *frame) float64 { a := x(fr); return round(a / y(fr)) }
			}
		}
	case *ast.UnaryExpr:
		switch e.Op {
		case token.SUB:
			x := c.floatExpr(e.X)
			return func(fr *frame) float64 { return -x(fr) }
		case token.ADD:
			return c.floatExpr(e.X)
		}
	}
	g := c.expr(e)
	return func(fr *frame) float64 { return g(fr).Float() }
}

func (c *compiler) complexExpr(e ast.Expr) cplxFn {
	e = ast.Unparen(e)
	if v := c.constOf(e); v != nil {
		re, _ := constant.Float64Val(constant.ToFloat(constant.Real(v)))
		im, _ := constant.Float64Val(constant.ToFloat(constant.Imag(v)))
		x := complex(re, im)
		return func(*frame) complex128 { return x }
	}
	if b, ok := e.(*ast.BinaryExpr); ok && !isComparison(b.Op) {
		_, bits := classOf(c.typeOf(e))
		round := func(x complex128) complex128 { return x }
		if bits == 64 {
			round = func(x complex128) complex128 { return complex128(complex64(x)) }
		}
		x, y := c.complexExpr(b.X), c.complexExpr(b.Y)
		switch b.Op {
		case token.ADD:
			return func(fr *frame) complex128 { return round(x(fr) + y(fr)) }
		case token.SUB:
			return func(fr *frame) complex128 { return round(x(fr) - y(fr)) }
		case token.MUL:
			return func(fr *frame) complex128 { return round(x(fr) * y(fr)) }
		case token.QUO:
			return func(fr *frame) complex128 { a := x(fr); return round(a / y(fr)) }
		}
	}
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.SUB {
		x := c.complexExpr(u.X)
		return func(fr *frame) complex128 { return -x(fr) }
	}
	g := c.expr(e)
	return func(fr *frame) complex128 { return g(fr).Complex() }
}

func (c *compiler) strExpr(e ast.Expr) strFn {
	e = ast.Unparen(e)
	if v := c.constOf(e); v != nil {
		s := constant.StringVal(v)
		return func(*frame) string { return s }
	}
	if s, ok := c.localSlot(e); ok {
		return func(fr *frame) string { return fr.vars[s].String() }
	}
	if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
		// Flatten a + b + c into one concatenation.
		var parts []strFn
		var collect func(x ast.Expr)
		collect = func(x ast.Expr) {
			x = ast.Unparen(x)
			if bb, ok := x.(*ast.BinaryExpr); ok && bb.Op == token.ADD && c.constOf(bb) == nil {
				collect(bb.X)
				collect(bb.Y)
				return
			}
			parts = append(parts, c.strExpr(x))
		}
		collect(b)
		return func(fr *frame) string {
			var sb strings.Builder
			for _, p := range parts {
				sb.WriteString(p(fr))
			}
			return sb.String()
		}
	}
	g := c.expr(e)
	return func(fr *frame) string { return g(fr).String() }
}

func isComparison(op token.Token) bool {
	switch op {
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		return true
	}
	return false
}

func (c *compiler) boolExpr(e ast.Expr) boolFn {
	e = ast.Unparen(e)
	if v := c.constOf(e); v != nil {
		b := constant.BoolVal(v)
		return func(*frame) bool { return b }
	}
	if s, ok := c.localSlot(e); ok {
		return func(fr *frame) bool { return fr.vars[s].Bool() }
	}
	switch e := e.(type) {
	case *ast.UnaryExpr:
		if e.Op == token.NOT {
			x := c.boolExpr(e.X)
			return func(fr *frame) bool { return !x(fr) }
		}
	case *ast.BinaryExpr:
		switch e.Op {
		case token.LAND:
			x, y := c.boolExpr(e.X), c.boolExpr(e.Y)
			return func(fr *frame) bool { return x(fr) && y(fr) }
		case token.LOR:
			x, y := c.boolExpr(e.X), c.boolExpr(e.Y)
			return func(fr *frame) bool { return x(fr) || y(fr) }
		}
		if isComparison(e.Op) {
			return c.comparison(e)
		}
	}
	g := c.expr(e)
	return func(fr *frame) bool { return g(fr).Bool() }
}

func (c *compiler) comparison(e *ast.BinaryExpr) boolFn {
	xt, yt := c.typeOf(e.X), c.typeOf(e.Y)
	op := e.Op
	if isNilExpr(c.info, e.X) || isNilExpr(c.info, e.Y) {
		other := e.X
		if isNilExpr(c.info, e.X) {
			other = e.Y
		}
		x := c.expr(other)
		eq := op == token.EQL
		return func(fr *frame) bool {
			v := x(fr)
			return isNilValue(v) == eq
		}
	}
	kx, _ := classOf(xt)
	ky, _ := classOf(yt)
	if kx == ky && kx != kOther {
		switch kx {
		case kInt:
			x, y := c.intExpr(e.X), c.intExpr(e.Y)
			return cmpOrdered(op, x, y)
		case kUint:
			x, y := c.uintExpr(e.X), c.uintExpr(e.Y)
			return cmpOrdered(op, x, y)
		case kFloat:
			x, y := c.floatExpr(e.X), c.floatExpr(e.Y)
			return cmpOrdered(op, x, y)
		case kString:
			x, y := c.strExpr(e.X), c.strExpr(e.Y)
			return cmpOrdered(op, x, y)
		case kBool:
			x, y := c.boolExpr(e.X), c.boolExpr(e.Y)
			if op == token.EQL {
				return func(fr *frame) bool { return x(fr) == y(fr) }
			}
			return func(fr *frame) bool { return x(fr) != y(fr) }
		case kComplex:
			x, y := c.complexExpr(e.X), c.complexExpr(e.Y)
			if op == token.EQL {
				return func(fr *frame) bool { return x(fr) == y(fr) }
			}
			return func(fr *frame) bool { return x(fr) != y(fr) }
		}
	}
	// General equality: interfaces, pointers, structs, arrays, channels.
	x, y := c.expr(e.X), c.expr(e.Y)
	xi, yi := isInterface(xt), isInterface(yt)
	in := c.in
	var eq func(a, b reflect.Value) bool
	switch {
	case xi && yi:
		eq = func(a, b reflect.Value) bool { return in.ifaceEqual(a, b) }
	case xi:
		conv := c.assignConv(yt, xt)
		eq = func(a, b reflect.Value) bool { return in.ifaceEqual(a, conv(b)) }
	case yi:
		conv := c.assignConv(xt, yt)
		eq = func(a, b reflect.Value) bool { return in.ifaceEqual(conv(a), b) }
	default:
		eq = func(a, b reflect.Value) bool { return in.valuesEqual(a, b) }
	}
	if op == token.EQL {
		return func(fr *frame) bool { return eq(x(fr), y(fr)) }
	}
	return func(fr *frame) bool { return !eq(x(fr), y(fr)) }
}

func cmpOrdered[T int64 | uint64 | float64 | string](op token.Token, x, y func(*frame) T) boolFn {
	switch op {
	case token.EQL:
		return func(fr *frame) bool { return x(fr) == y(fr) }
	case token.NEQ:
		return func(fr *frame) bool { return x(fr) != y(fr) }
	case token.LSS:
		return func(fr *frame) bool { return x(fr) < y(fr) }
	case token.LEQ:
		return func(fr *frame) bool { return x(fr) <= y(fr) }
	case token.GTR:
		return func(fr *frame) bool { return x(fr) > y(fr) }
	default:
		return func(fr *frame) bool { return x(fr) >= y(fr) }
	}
}

func isNilExpr(info *types.Info, e ast.Expr) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return false
	}
	_, isNil := info.Uses[id].(*types.Nil)
	return isNil
}

func isNilValue(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface, reflect.UnsafePointer:
		return v.IsNil()
	}
	return false
}

// ifaceEqual compares two interface values like Go's ==.
func (in *Interp) ifaceEqual(a, b reflect.Value) bool {
	na, ea := in.dyn(a)
	nb, eb := in.dyn(b)
	if na != nb {
		return false
	}
	if na == nil {
		return true
	}
	return in.valuesEqual(ea, eb)
}

// valuesEqual compares two values of the same type like Go's ==.
func (in *Interp) valuesEqual(a, b reflect.Value) bool {
	switch a.Kind() {
	case reflect.Interface:
		return in.ifaceEqual(a, b)
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			if a.Type().Field(i).Name[0] == '_' {
				continue
			}
			if !in.valuesEqual(fieldByIndex(a, i), fieldByIndex(b, i)) {
				return false
			}
		}
		return true
	case reflect.Array:
		for i := 0; i < a.Len(); i++ {
			if !in.valuesEqual(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Map, reflect.Func:
		panic(runtimeError("comparing uncomparable type " + a.Type().String()))
	case reflect.Float32, reflect.Float64:
		return a.Float() == b.Float()
	case reflect.Complex64, reflect.Complex128:
		return a.Complex() == b.Complex()
	case reflect.Pointer, reflect.Chan, reflect.UnsafePointer:
		return a.Pointer() == b.Pointer()
	case reflect.String:
		return a.String() == b.String()
	case reflect.Bool:
		return a.Bool() == b.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return a.Int() == b.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return a.Uint() == b.Uint()
	}
	return a.Equal(b)
}

var _ = math.MaxInt
