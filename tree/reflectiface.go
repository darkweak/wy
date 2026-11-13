package tree

import (
	"go/types"
	"reflect"
	"sync"
)

// Interpreted interface types map to `any` in memory (see types.go), which
// makes them all one type for reflection: reflect.TypeOf((*I)(nil)).Elem()
// would be interface{}, and every type would implement it. Libraries such
// as msgpack select encoders with typ.Implements(customEncoderType).
//
// reflect.TypeOf therefore gives each interpreted interface it exposes a
// token: a distinct compiled empty interface (same layout as any, holds any
// value), and the reflect.Type relations (Implements, AssignableTo,
// ConvertibleTo) involving a token are answered with go/types.

type ifaceTokens struct {
	mu   sync.Mutex
	ts   []*types.Named
	byRT map[reflect.Type]*types.Named
}

var reflectTypeRT = reflect.TypeFor[reflect.Type]()

// ifaceToken returns the token of the interpreted interface t, or nil when
// the pool is exhausted.
func (in *Interp) ifaceToken(t *types.Named) reflect.Type {
	tk := &in.ifaceTokens
	tk.mu.Lock()
	defer tk.mu.Unlock()
	for i, c := range tk.ts {
		if c == t || types.Identical(c, t) {
			return ifacePool[i]
		}
	}
	if len(tk.ts) == len(ifacePool) {
		return nil
	}
	rt := ifacePool[len(tk.ts)]
	tk.ts = append(tk.ts, t)
	if tk.byRT == nil {
		tk.byRT = map[reflect.Type]*types.Named{}
	}
	tk.byRT[rt] = t
	in.rtMu.Lock()
	in.natives.types[rt] = t
	in.rtMu.Unlock()
	return rt
}

func (in *Interp) tokenType(rt reflect.Type) *types.Named {
	tk := &in.ifaceTokens
	tk.mu.Lock()
	defer tk.mu.Unlock()
	return tk.byRT[rt]
}

// exposedRType returns the reflect type of t as reflect.TypeOf shows it,
// when it differs from its storage type because t involves an interpreted
// interface; nil otherwise.
func (in *Interp) exposedRType(t types.Type) reflect.Type {
	switch u := types.Unalias(t).(type) {
	case *types.Named:
		if isInterface(u) && !in.isNativeNamed(u) && in.rtype(u) == anyRT {
			return in.ifaceToken(u)
		}
	case *types.Pointer:
		if e := in.exposedRType(u.Elem()); e != nil {
			return reflect.PointerTo(e)
		}
	case *types.Slice:
		if e := in.exposedRType(u.Elem()); e != nil {
			return reflect.SliceOf(e)
		}
	}
	return nil
}

// reflectTypeOf implements reflect.TypeOf for an interpreted argument; ok is
// false when the compiled reflect.TypeOf gives the right answer.
func (in *Interp) reflectTypeOf(arg reflect.Value) (reflect.Value, bool) {
	if arg.Kind() != reflect.Interface || arg.IsNil() {
		return reflect.Value{}, false
	}
	nt, _ := in.dyn(arg)
	if nt == nil {
		return reflect.Value{}, false
	}
	rt := in.exposedRType(nt.t)
	if rt == nil {
		return reflect.Value{}, false
	}
	out := reflect.New(reflectTypeRT).Elem()
	out.Set(reflect.ValueOf(rt))
	return out, true
}

// goTypeOfRT returns the Go type a reflect type stands for, recovering
// interpreted interfaces (tokens) and named structs.
func (in *Interp) goTypeOfRT(rt reflect.Type) types.Type {
	if t := in.tokenType(rt); t != nil {
		return t
	}
	switch rt.Kind() {
	case reflect.Pointer:
		return types.NewPointer(in.goTypeOfRT(rt.Elem()))
	case reflect.Slice:
		return types.NewSlice(in.goTypeOfRT(rt.Elem()))
	case reflect.Struct:
		in.rtMu.Lock()
		cands := in.rts.byRT[rt]
		in.rtMu.Unlock()
		if len(cands) == 1 {
			return cands[0]
		}
	}
	return in.typeOfRT(rt)
}

// reflectTypeRelation answers recv.<name>(arg) for the reflect.Type methods
// Implements, AssignableTo and ConvertibleTo when a token, or an interpreted
// struct type whose methods reflection cannot see, is involved.
func (in *Interp) reflectTypeRelation(name string, recv reflect.Value, args []reflect.Value) (reflect.Value, bool) {
	if len(args) != 1 || !recv.IsValid() {
		return reflect.Value{}, false
	}
	recvRT, ok1 := recv.Interface().(reflect.Type)
	arg := args[0]
	if arg.Kind() == reflect.Interface {
		if arg.IsNil() {
			return reflect.Value{}, false
		}
		arg = arg.Elem()
	}
	argRT, ok2 := arg.Interface().(reflect.Type)
	if !ok1 || !ok2 || recvRT == nil || argRT == nil {
		return reflect.Value{}, false
	}
	if !in.involvesToken(recvRT) && !in.involvesToken(argRT) && !in.interpretedStruct(recvRT) {
		return reflect.Value{}, false
	}
	T, U := in.goTypeOfRT(recvRT), in.goTypeOfRT(argRT)
	var r bool
	switch name {
	case "Implements":
		iface, ok := U.Underlying().(*types.Interface)
		if !ok {
			return reflect.Value{}, false // the compiled method panics
		}
		r = types.Implements(T, iface)
	case "AssignableTo":
		r = types.AssignableTo(T, U)
	case "ConvertibleTo":
		r = types.ConvertibleTo(T, U)
	default:
		return reflect.Value{}, false
	}
	return reflect.ValueOf(r), true
}

// interpretedStruct reports whether rt is the reflect type of one
// interpreted named struct, or a pointer to one: its methods are only known
// to the interpreter.
func (in *Interp) interpretedStruct(rt reflect.Type) bool {
	if rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	if rt.Kind() != reflect.Struct {
		return false
	}
	in.rtMu.Lock()
	defer in.rtMu.Unlock()
	return len(in.rts.byRT[rt]) == 1
}

func (in *Interp) involvesToken(rt reflect.Type) bool {
	for {
		if in.tokenType(rt) != nil {
			return true
		}
		switch rt.Kind() {
		case reflect.Pointer, reflect.Slice:
			rt = rt.Elem()
		default:
			return false
		}
	}
}

// isReflectTypeRelation reports whether m, called on recvT, is one of the
// reflect.Type methods reflectTypeRelation answers.
func isReflectTypeRelation(recvT types.Type, name string) bool {
	n, ok := types.Unalias(recvT).(*types.Named)
	if !ok || n.Obj().Pkg() == nil || n.Obj().Pkg().Path() != "reflect" || n.Obj().Name() != "Type" {
		return false
	}
	return name == "Implements" || name == "AssignableTo" || name == "ConvertibleTo"
}
