package tree

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"unsafe"
)

// Execution model: source is compiled once into a tree of Go closures.
// Expressions become eval funcs returning reflect.Values, statements become
// exec funcs returning a control-flow code. Every variable is an
// addressable reflect.Value backed by real memory, so &x, closures that
// capture x, and unsafe all observe the same storage.

type eval func(fr *frame) reflect.Value
type evalN func(fr *frame) []reflect.Value
type exec func(fr *frame) ctrl

type ctrl uint8

const (
	cNext ctrl = iota
	cBreak
	cContinue
	cReturn
	cGoto
	cFallthrough
)

type frame struct {
	vars   []reflect.Value
	env    []reflect.Value
	label  string // target of a pending labeled break/continue/goto
	defers []func()

	panicking bool
	panicVal  any
	recovered bool
	deferOf   *frame // set when this call is a deferred call of deferOf
	inDefer   bool   // a deferred call of this frame is being started

	fn     *function
	caller *frame
	pos    token.Pos // position of the call being made (for runtime.Caller)

	iterCtl *iterControl // set inside the body of a range-over-func loop

	results []int            // result slots, for functions with defers
	out     *[]reflect.Value // where to store results after deferred calls

	inline [6]reflect.Value // storage for small functions' variables
}

func newFrame(n int) *frame {
	fr := &frame{}
	if n <= len(fr.inline) {
		fr.vars = fr.inline[:n]
	} else {
		fr.vars = make([]reflect.Value, n)
	}
	return fr
}

// function is a compiled function body (one per instantiation).
type function struct {
	in          *Interp
	name        string
	sig         *types.Signature
	nvars       int
	params      []int // slots for receiver and parameters
	results     []int // slots for results
	resultTypes []reflect.Type
	paramTypes  []reflect.Type
	body        exec
	hasDefer    bool
	variadic    bool
	rt          reflect.Type // func type, for func values
	builtin     func(args []reflect.Value) []reflect.Value
}

// builtinFunc makes a function implemented in Go with signature sig.
func (in *Interp) builtinFunc(name string, sig *types.Signature, impl func(args []reflect.Value) []reflect.Value) *function {
	f := &function{in: in, name: name, sig: sig, builtin: impl, variadic: sig.Variadic()}
	for i := 0; i < sig.Params().Len(); i++ {
		f.paramTypes = append(f.paramTypes, in.rtype(sig.Params().At(i).Type()))
	}
	return f
}

// closure is a function plus captured variables.
type closure struct {
	fn  *function
	env []reflect.Value
}

// call runs the function. caller is the calling frame, if known; when the
// caller is running deferred calls, this call is one of them (and may
// recover).
func (f *function) call(env, args []reflect.Value, caller *frame) (out []reflect.Value) {
	if f.builtin != nil {
		return f.builtin(args)
	}
	fr := newFrame(f.nvars)
	fr.env, fr.fn, fr.caller = env, f, caller
	if caller != nil && caller.inDefer {
		fr.deferOf, caller.inDefer = caller, false
	}
	for i, slot := range f.params {
		v := reflect.New(f.paramTypes[i]).Elem()
		if i < len(args) {
			v.Set(args[i])
		}
		fr.vars[slot] = v
	}
	return f.run(fr)
}

// callOwned is call with arguments that are fresh variables of the
// parameter types, which become the parameters without copying.
func (f *function) callOwned(args []reflect.Value, caller *frame) (out []reflect.Value) {
	if f.builtin != nil {
		return f.builtin(args)
	}
	fr := newFrame(f.nvars)
	fr.fn, fr.caller = f, caller
	for i, slot := range f.params {
		fr.vars[slot] = args[i]
	}
	return f.run(fr)
}

// call1Owned calls a function with one result in a value context: no
// slices are allocated for arguments or results.
func (f *function) call1Owned(args []reflect.Value, caller *frame) reflect.Value {
	if f.builtin != nil || f.hasDefer {
		return f.callOwned(args, caller)[0]
	}
	fr := newFrame(f.nvars)
	fr.fn, fr.caller = f, caller
	for i, slot := range f.params {
		fr.vars[slot] = args[i]
	}
	slot := f.results[0]
	fr.vars[slot] = reflect.New(f.resultTypes[0]).Elem()
	f.body(fr)
	return fr.vars[slot]
}

func (f *function) run(fr *frame) (out []reflect.Value) {
	for i, slot := range f.results {
		fr.vars[slot] = reflect.New(f.resultTypes[i]).Elem()
	}
	if f.hasDefer {
		// Results are read after deferred calls ran (they may change named
		// results, or recover from a panic).
		fr.results, fr.out = f.results, &out
		defer fr.runDefers()
	}
	f.body(fr)
	if len(f.results) == 0 {
		return nil
	}
	out = make([]reflect.Value, len(f.results))
	for i, slot := range f.results {
		out[i] = fr.vars[slot]
	}
	return out
}

// runDefers runs deferred calls in LIFO order, implementing recover: a
// deferred interpreted call receives deferOf = fr, and recover() in it
// stops the panic recorded here.
func (fr *frame) runDefers() {
	if r := recover(); r != nil {
		fr.panicking, fr.panicVal = true, r
	}
	for len(fr.defers) > 0 {
		d := fr.defers[len(fr.defers)-1]
		fr.defers = fr.defers[:len(fr.defers)-1]
		func() {
			defer func() {
				if r := recover(); r != nil {
					// A panic in a deferred call replaces the current one.
					fr.panicking, fr.panicVal, fr.recovered = true, r, false
				}
			}()
			fr.inDefer = true
			d()
			fr.inDefer = false
		}()
	}
	if fr.panicking && !fr.recovered {
		panic(fr.panicVal)
	}
	if fr.out != nil && len(fr.results) > 0 {
		out := make([]reflect.Value, len(fr.results))
		for i, slot := range fr.results {
			out[i] = fr.vars[slot]
		}
		*fr.out = out
	}
}

// recoverValue implements the recover builtin for a call running in fr.
func (fr *frame) recoverValue() any {
	p := fr.deferOf
	if p == nil || !p.panicking || p.recovered {
		return nil
	}
	if _, exit := p.panicVal.(*ExitError); exit {
		return nil
	}
	p.recovered = true
	return p.panicVal
}

// --- func values -------------------------------------------------------

// funcValue turns a closure into a Go func value of type rt. The func can
// be called by compiled code; interpreted callers recognize it and call the
// closure directly (see closureOf).
func (in *Interp) funcValue(c *closure, rt reflect.Type) reflect.Value {
	sig := c.fn.sig
	fv := reflect.MakeFunc(rt, func(args []reflect.Value) []reflect.Value {
		conv := make([]reflect.Value, len(args))
		for i, a := range args {
			conv[i] = in.fromNative(a, paramType(sig, i))
		}
		res := c.call(conv, nil)
		out := make([]reflect.Value, len(res))
		for i, r := range res {
			out[i] = in.toNative(r, sig.Results().At(i).Type(), rt.Out(i))
		}
		return out
	})
	p := funcvalPointer(fv)
	key := uintptr(p)
	in.closures.Store(key, c)
	runtime.AddCleanup((*byte)(p), func(k uintptr) { in.closures.Delete(k) }, key)
	return fv
}

func paramType(sig *types.Signature, i int) types.Type {
	n := sig.Params().Len()
	if sig.Variadic() && i >= n-1 {
		return sig.Params().At(n - 1).Type()
	}
	return sig.Params().At(i).Type()
}

func (c *closure) call(args []reflect.Value, caller *frame) []reflect.Value {
	return c.fn.call(c.env, args, caller)
}

// funcvalPointer returns the closure object behind a func value.
func funcvalPointer(v reflect.Value) unsafe.Pointer {
	x := v.Interface()
	return (*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1]
}

func funcvalPtr(v reflect.Value) uintptr { return uintptr(funcvalPointer(v)) }

func (in *Interp) closureOf(v reflect.Value) *closure {
	if v.IsNil() {
		return nil
	}
	c, ok := in.closures.Load(funcvalPtr(v))
	if !ok {
		return nil
	}
	return c.(*closure)
}

// callValue calls a func value (interpreted or compiled) with
// interpreter-side arguments.
func (in *Interp) callValue(fv reflect.Value, sig *types.Signature, args []reflect.Value, spread bool, deferOf *frame) []reflect.Value {
	if fv.IsNil() {
		panic(runtimeError("invalid memory address or nil pointer dereference"))
	}
	if c := in.closureOf(fv); c != nil {
		if spread || !sig.Variadic() {
			return c.call(args, deferOf)
		}
		return c.call(packVariadic(c.fn.paramTypes, args), deferOf)
	}
	return in.callNative(fv, sig, args, spread)
}

// packVariadic gathers trailing arguments into the variadic slice.
func packVariadic(paramTypes []reflect.Type, args []reflect.Value) []reflect.Value {
	n := len(paramTypes)
	st := paramTypes[n-1]
	s := reflect.MakeSlice(st, 0, len(args)-(n-1))
	for _, a := range args[n-1:] {
		s = reflect.Append(s, a)
	}
	return append(append([]reflect.Value{}, args[:n-1]...), s)
}

// argMode says how interpreted values passed as interface arguments are
// presented to a compiled function, depending on what it does with them.
type argMode uint8

const (
	modeRaw       argMode = iota // reflection-based code: the plain value
	modeDisplay                  // printing (fmt, log): display mirrors
	modeContainer                // stores and returns values: boxed, identity kept
	modeEncode                   // encoders: wrappers when Marshal* methods exist
)

// modeForPackage picks the argument mode for functions of a package.
func modeForPackage(path string) argMode {
	switch path {
	case "fmt", "log", "log/slog", "testing":
		return modeDisplay
	case "sync", "sync/atomic", "context", "container/list", "container/ring", "container/heap":
		return modeContainer
	case "encoding/json", "encoding/xml", "encoding":
		return modeEncode
	}
	return modeRaw
}

// callNative calls a compiled function, converting arguments and results.
func (in *Interp) callNative(fv reflect.Value, sig *types.Signature, args []reflect.Value, spread bool) []reflect.Value {
	return in.callNativeMode(fv, sig, args, spread, modeRaw)
}

func (in *Interp) callNativeMode(fv reflect.Value, sig *types.Signature, args []reflect.Value, spread bool, mode argMode) []reflect.Value {
	if mode == modeDisplay && sig.Variadic() {
		// Printf-style: a string parameter right before ...any.
		n := sig.Params().Len()
		if n >= 2 && types.Identical(sig.Params().At(n-2).Type(), types.Typ[types.String]) &&
			isInterface(sig.Params().At(n-1).Type().(*types.Slice).Elem()) {
			if rewritten, packed := in.printfTypeNames(args, n-2); packed {
				args, spread = rewritten, true
			}
		}
	}
	ft := fv.Type()
	nargs := make([]reflect.Value, len(args))
	for i, a := range args {
		var want reflect.Type
		var st types.Type
		switch {
		case spread && i == len(args)-1:
			want, st = ft.In(ft.NumIn()-1), sig.Params().At(sig.Params().Len()-1).Type()
		case ft.IsVariadic() && i >= ft.NumIn()-1:
			want = ft.In(ft.NumIn() - 1).Elem()
			st = sig.Params().At(sig.Params().Len() - 1).Type().(*types.Slice).Elem()
		default:
			want, st = ft.In(i), sig.Params().At(i).Type()
		}
		display := mode == modeDisplay
		if display && want == anyRT {
			d := in.display(a, st, 0)
			nargs[i] = reflect.New(anyRT).Elem()
			if d.IsValid() {
				nargs[i].Set(d)
			}
			continue
		}
		if display && spread && i == len(args)-1 && want.Kind() == reflect.Slice && want.Elem() == anyRT && !a.IsNil() {
			et := st.(*types.Slice).Elem()
			cp := reflect.MakeSlice(want, a.Len(), a.Len())
			for j := 0; j < a.Len(); j++ {
				if d := in.display(a.Index(j), et, 0); d.IsValid() {
					cp.Index(j).Set(d)
				}
			}
			nargs[i] = cp
			continue
		}
		nargs[i] = in.toNativeMode(a, st, want, mode)
	}
	var res []reflect.Value
	if spread {
		res = fv.CallSlice(nargs)
	} else {
		res = fv.Call(nargs)
	}
	for i, r := range res {
		res[i] = in.fromNative(r, sig.Results().At(i).Type())
	}
	return res
}

// --- compiler state ----------------------------------------------------

type compiler struct {
	in    *Interp
	pkg   *srcPkg
	info  *types.Info
	subst *substMap
	fs    *funcState
	// lastFuncState is the state of the most recently compiled function
	// literal (its captures).
	lastFuncState *funcState
}

type funcState struct {
	outer    *funcState
	slots    map[types.Object]int
	nslots   int
	captures []capture
	capIndex map[types.Object]int
	results  []int
	resTypes []types.Type
	sig      *types.Signature
	hasDefer bool
	name     string
	nlits    int // function literals numbered so far
}

type capture struct {
	obj         types.Object
	fromSlot    int // index in the outer frame's vars, or -1
	fromCapture int
}

func newFuncState(outer *funcState, name string) *funcState {
	return &funcState{outer: outer, slots: map[types.Object]int{}, capIndex: map[types.Object]int{}, name: name}
}

func (fs *funcState) newSlot(obj types.Object) int {
	i := fs.nslots
	fs.nslots++
	if obj != nil {
		fs.slots[obj] = i
	}
	return i
}

// captureIndex returns the env index under which fs sees obj from an
// enclosing function, registering captures along the way. ok is false if
// no enclosing function declares obj.
func (fs *funcState) captureIndex(obj types.Object) (int, bool) {
	if i, ok := fs.capIndex[obj]; ok {
		return i, true
	}
	if fs.outer == nil {
		return 0, false
	}
	c := capture{obj: obj, fromSlot: -1, fromCapture: -1}
	if s, ok := fs.outer.slots[obj]; ok {
		c.fromSlot = s
	} else if ci, ok := fs.outer.captureIndex(obj); ok {
		c.fromCapture = ci
	} else {
		return 0, false
	}
	i := len(fs.captures)
	fs.captures = append(fs.captures, c)
	fs.capIndex[obj] = i
	return i, true
}

func (c *compiler) typeOf(e ast.Expr) types.Type {
	tv, ok := c.info.Types[e]
	if !ok || tv.Type == nil {
		if id, ok := e.(*ast.Ident); ok {
			if obj := c.info.ObjectOf(id); obj != nil {
				return c.subst.typ(obj.Type())
			}
		}
		panic(fmt.Sprintf("wy: no type for expression %s", c.pos(e)))
	}
	return c.subst.typ(tv.Type)
}

func (c *compiler) pos(n ast.Node) string { return c.in.fset.Position(n.Pos()).String() }

// funcFor returns the compiled function for a source-declared function or
// method, instantiated with targs for generic code.
func (in *Interp) funcFor(obj *types.Func, targs []types.Type) *function {
	origin := obj.Origin()
	key := funcKey(origin, targs)
	if f, ok := in.funcs[key]; ok {
		return f
	}
	if b := intrinsicFor(in, origin); b != nil {
		in.funcs[key] = b
		return b
	}
	fd := in.funcDecls[origin]
	if fd == nil {
		panic(fmt.Sprintf("wy: no source for %s", origin.FullName()))
	}
	sm := &substMap{in: in}
	sig := origin.Type().(*types.Signature)
	switch {
	case sig.TypeParams().Len() > 0:
		sm.bind(sig.TypeParams(), targs)
	case sig.RecvTypeParams().Len() > 0:
		sm.bind(sig.RecvTypeParams(), targs)
	}
	c := &compiler{in: in, pkg: fd.pkg, info: fd.pkg.info, subst: sm}
	f := &function{in: in, name: funcName(origin)}
	in.funcs[key] = f
	if fd.decl.Body == nil {
		f.builtin = func([]reflect.Value) []reflect.Value {
			panic(fmt.Sprintf("wy: function %s has no body (implemented outside Go)", origin.FullName()))
		}
		return f
	}
	c.compileFunc(f, fd.decl.Recv, fd.decl.Type, fd.decl.Body, sm.sig(sig), nil)
	return f
}

// funcName spells a function the way the Go runtime does: pkg.F,
// pkg.T.M, pkg.(*T).M.
func funcName(f *types.Func) string {
	pkg := "main"
	if f.Pkg() != nil {
		pkg = f.Pkg().Path()
	}
	sig := f.Type().(*types.Signature)
	if sig.Recv() == nil {
		return pkg + "." + f.Name()
	}
	t := sig.Recv().Type()
	ptr := false
	if p, ok := t.(*types.Pointer); ok {
		t, ptr = p.Elem(), true
	}
	name := "?"
	if n, ok := types.Unalias(t).(*types.Named); ok {
		name = n.Obj().Name()
	}
	if ptr {
		return pkg + ".(*" + name + ")." + f.Name()
	}
	return pkg + "." + name + "." + f.Name()
}

func funcKey(origin *types.Func, targs []types.Type) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%p", origin)
	for _, t := range targs {
		b.WriteString("|" + types.TypeString(t, nil))
	}
	return b.String()
}

// compileFunc compiles a function declaration or literal into f, using the
// compiler's current funcState as the enclosing one (for literals).
func (c *compiler) compileFunc(f *function, recv *ast.FieldList, ft *ast.FuncType, body *ast.BlockStmt, sig *types.Signature, outer *funcState) {
	fs := newFuncState(outer, f.name)
	fs.sig = sig
	saved := c.fs
	c.fs = fs
	defer func() { c.fs = saved }()

	addParam := func(id *ast.Ident, t types.Type) {
		var obj types.Object
		if id != nil {
			obj = c.info.Defs[id]
		}
		f.params = append(f.params, fs.newSlot(obj))
		f.paramTypes = append(f.paramTypes, c.in.rtype(t))
	}
	paramList := func(fl *ast.FieldList, tuple *types.Tuple, start int) {
		k := start
		if fl == nil {
			return
		}
		for _, field := range fl.List {
			if len(field.Names) == 0 {
				addParam(nil, tuple.At(k).Type())
				k++
				continue
			}
			for _, id := range field.Names {
				addParam(id, tuple.At(k).Type())
				k++
			}
		}
	}
	if recv != nil && sig.Recv() != nil {
		var id *ast.Ident
		if len(recv.List) > 0 && len(recv.List[0].Names) > 0 {
			id = recv.List[0].Names[0]
		}
		addParam(id, sig.Recv().Type())
	}
	paramList(ft.Params, sig.Params(), 0)
	f.variadic = sig.Variadic()
	// Results: named results are ordinary locals.
	if ft.Results != nil {
		k := 0
		for _, field := range ft.Results.List {
			names := field.Names
			if len(names) == 0 {
				names = []*ast.Ident{nil}
			}
			for _, id := range names {
				var obj types.Object
				if id != nil {
					obj = c.info.Defs[id]
				}
				t := sig.Results().At(k).Type()
				f.results = append(f.results, fs.newSlot(obj))
				f.resultTypes = append(f.resultTypes, c.in.rtype(t))
				fs.resTypes = append(fs.resTypes, t)
				k++
			}
		}
	}
	fs.results = f.results
	f.sig = sig
	f.body = c.block(body.List)
	f.nvars = fs.nslots
	f.hasDefer = fs.hasDefer
	f.rt = c.in.rtype(sig)
	c.lastFuncState = fs
}

// compileTop compiles code that runs outside any function (package
// variable initializers, interactive inputs).
func (in *Interp) compileTop(sp *srcPkg, build func(c *compiler) exec) (run func(), err error) {
	c := &compiler{in: in, pkg: sp, info: sp.info, subst: &substMap{in: in}}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	fs := newFuncState(nil, "init")
	c.fs = fs
	body := build(c)
	f := &function{in: in, name: "init", nvars: fs.nslots, hasDefer: fs.hasDefer, body: body}
	return func() { f.call(nil, nil, nil) }, nil
}

// initializer compiles a package-level variable initialization.
func (c *compiler) initializer(init *types.Initializer) exec {
	if len(init.Lhs) == 1 {
		v := c.in.globals[init.Lhs[0]]
		rhs := c.expr(init.Rhs)
		conv := c.assignConv(c.typeOf(init.Rhs), init.Lhs[0].Type())
		return func(fr *frame) ctrl {
			v.Set(conv(rhs(fr)))
			return cNext
		}
	}
	rhs := c.exprN(init.Rhs)
	tuple := c.typeOf(init.Rhs).(*types.Tuple)
	var targets []reflect.Value
	var convs []func(reflect.Value) reflect.Value
	for i, l := range init.Lhs {
		if l.Name() == "_" {
			targets = append(targets, reflect.Value{})
			convs = append(convs, nil)
			continue
		}
		targets = append(targets, c.in.globals[l])
		convs = append(convs, c.assignConv(tuple.At(i).Type(), l.Type()))
	}
	return func(fr *frame) ctrl {
		vals := rhs(fr)
		for i, t := range targets {
			if t.IsValid() {
				t.Set(convs[i](vals[i]))
			}
		}
		return cNext
	}
}

func debugStack() []byte { return debug.Stack() }
