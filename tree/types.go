package tree

import (
	"errors"
	"fmt"
	"go/constant"
	"go/token"
	"go/types"
	"reflect"
	"strings"
	"unsafe"

	"github.com/darkweak/wy/internal/source"
	"github.com/darkweak/wy/tree/native"
)

// Values of interpreted code live in real Go memory: every types.Type is
// mapped to a reflect.Type with the layout the gc compiler would give it,
// which is what makes unsafe, Sizeof/Offsetof and pointer arithmetic work.
//
//   - Native named types (from compiled packages) map to themselves.
//   - Interpreted named types map to their underlying type; their identity
//     and methods are tracked by the interpreter (see boxing in iface.go).
//   - Interface types map to `any`, except native interfaces (including
//     error), which map to themselves.
//   - A reference cycle through a named type (type node struct{ next *node })
//     cannot be expressed with reflect, so the inner occurrence is stored as
//     a layout-identical opaque type (unsafe.Pointer, or a slice header) and
//     reinterpreted on access.

var (
	anyRT      = reflect.TypeFor[any]()
	errorRT    = reflect.TypeFor[error]()
	unsafePtrT = reflect.TypeFor[unsafe.Pointer]()
	sliceHdrRT = reflect.TypeFor[struct {
		Data     unsafe.Pointer
		Len, Cap int
	}]()
	boolRT   = reflect.TypeFor[bool]()
	intRT    = reflect.TypeFor[int]()
	stringRT = reflect.TypeFor[string]()
)

var basicRT = map[types.BasicKind]reflect.Type{
	types.Bool: reflect.TypeFor[bool](), types.String: reflect.TypeFor[string](),
	types.Int: reflect.TypeFor[int](), types.Int8: reflect.TypeFor[int8](), types.Int16: reflect.TypeFor[int16](),
	types.Int32: reflect.TypeFor[int32](), types.Int64: reflect.TypeFor[int64](),
	types.Uint: reflect.TypeFor[uint](), types.Uint8: reflect.TypeFor[uint8](), types.Uint16: reflect.TypeFor[uint16](),
	types.Uint32: reflect.TypeFor[uint32](), types.Uint64: reflect.TypeFor[uint64](), types.Uintptr: reflect.TypeFor[uintptr](),
	types.Float32: reflect.TypeFor[float32](), types.Float64: reflect.TypeFor[float64](),
	types.Complex64: reflect.TypeFor[complex64](), types.Complex128: reflect.TypeFor[complex128](),
	types.UnsafePointer: unsafePtrT,
}

// errCycle signals that a named type was reached while its own reflect
// type is being built. The returned reflect type, if not nil, is a
// layout-identical stand-in that can be used in its place.
var errCycle = errors.New("type cycle")

type rtypes struct {
	in         *Interp
	cache      map[types.Type]reflect.Type
	named      map[*types.Named]reflect.Type
	inProgress map[*types.Named]bool
}

func newRtypes(in *Interp) *rtypes {
	return &rtypes{in: in, cache: map[types.Type]reflect.Type{}, named: map[*types.Named]reflect.Type{},
		inProgress: map[*types.Named]bool{}}
}

// rtype returns the canonical reflect type of t.
func (in *Interp) rtype(t types.Type) reflect.Type {
	in.rtMu.Lock()
	defer in.rtMu.Unlock()
	rt, err := in.rts.get(t)
	if err != nil && (err != errCycle || rt == nil) {
		panic(fmt.Sprintf("wy: cannot represent type %s", t))
	}
	return rt
}

// standIn returns the opaque type that can represent a value of type t
// whose reflect type is not available yet, or nil if t is not
// pointer-shaped or a slice.
func standIn(t types.Type) reflect.Type {
	switch u := t.Underlying().(type) {
	case *types.Pointer, *types.Map, *types.Chan, *types.Signature:
		return unsafePtrT
	case *types.Slice:
		return sliceHdrRT
	case *types.Basic:
		if u.Kind() == types.UnsafePointer {
			return unsafePtrT
		}
	}
	return nil
}

// get returns the reflect type of t. A result with errCycle is valid but
// depends on a type still being built, so it is not cached.
func (r *rtypes) get(t types.Type) (rt reflect.Type, err error) {
	if rt, ok := r.cache[t]; ok {
		return rt, nil
	}
	defer func() {
		if err == nil {
			r.cache[t] = rt
		}
	}()
	// component resolves a component type: a cycle to a type with no
	// stand-in makes the enclosing composite opaque (ok false).
	cycle := false
	component := func(c types.Type) (reflect.Type, bool) {
		rt, err := r.get(c)
		if err == errCycle {
			cycle = true
			return rt, rt != nil
		}
		if err != nil {
			panic(err)
		}
		return rt, true
	}
	done := func(rt reflect.Type) (reflect.Type, error) {
		if cycle {
			return rt, errCycle
		}
		return rt, nil
	}
	defer func() {
		if p := recover(); p != nil {
			if e, ok := p.(error); ok {
				rt, err = nil, e
				return
			}
			panic(p)
		}
	}()
	switch t := t.(type) {
	case *types.Basic:
		if t.Info()&types.IsUntyped != 0 {
			if t.Kind() == types.UntypedNil {
				return unsafePtrT, nil
			}
			return r.get(types.Default(t))
		}
		return basicRT[t.Kind()], nil
	case *types.Alias:
		return r.get(types.Unalias(t))
	case *types.Named:
		return r.namedType(t)
	case *types.Interface:
		return anyRT, nil
	case *types.Pointer:
		elem, ok := component(t.Elem())
		if !ok || elem == unsafePtrT && cycle || elem == sliceHdrRT && cycle {
			if !ok {
				return unsafePtrT, errCycle
			}
		}
		return done(reflect.PointerTo(elem))
	case *types.Slice:
		elem, ok := component(t.Elem())
		if !ok {
			return sliceHdrRT, errCycle
		}
		return done(reflect.SliceOf(elem))
	case *types.Array:
		elem, ok := component(t.Elem())
		if !ok {
			return nil, errCycle
		}
		return done(reflect.ArrayOf(int(t.Len()), elem))
	case *types.Map:
		k, ok1 := component(t.Key())
		v, ok2 := component(t.Elem())
		if !ok1 || !ok2 {
			return unsafePtrT, errCycle
		}
		return done(reflect.MapOf(k, v))
	case *types.Chan:
		elem, ok := component(t.Elem())
		if !ok {
			return unsafePtrT, errCycle
		}
		dir := reflect.BothDir
		switch t.Dir() {
		case types.SendOnly:
			dir = reflect.SendDir
		case types.RecvOnly:
			dir = reflect.RecvDir
		}
		return done(reflect.ChanOf(dir, elem))
	case *types.Signature:
		var in, out []reflect.Type
		for i := 0; i < t.Params().Len(); i++ {
			p, ok := component(t.Params().At(i).Type())
			if !ok {
				return unsafePtrT, errCycle
			}
			in = append(in, p)
		}
		for i := 0; i < t.Results().Len(); i++ {
			p, ok := component(t.Results().At(i).Type())
			if !ok {
				return unsafePtrT, errCycle
			}
			out = append(out, p)
		}
		return done(reflect.FuncOf(in, out, t.Variadic()))
	case *types.Struct:
		fields := make([]reflect.StructField, t.NumFields())
		for i := range fields {
			f := t.Field(i)
			ft, ok := component(f.Type())
			if !ok {
				return nil, errCycle
			}
			name := f.Name()
			if name == "_" {
				name = fmt.Sprintf("_%d", i)
			}
			sf := reflect.StructField{Name: name, Type: ft, Tag: reflect.StructTag(t.Tag(i))}
			if !token.IsExported(name) {
				sf.PkgPath = "wy/" + pkgPathOf(f.Pkg())
			}
			if f.Embedded() && token.IsExported(name) && ft.Kind() != reflect.UnsafePointer && ft != sliceHdrRT {
				sf.Anonymous = true
			}
			fields[i] = sf
		}
		// A struct is complete even if fields stand in for types being
		// built: those fields are reinterpreted on access.
		return reflect.StructOf(fields), nil
	case *types.TypeParam:
		return nil, fmt.Errorf("unsubstituted type parameter %s", t)
	case *types.Tuple:
		return nil, fmt.Errorf("tuple type")
	}
	return nil, fmt.Errorf("unsupported type %T", t)
}

func pkgPathOf(p *types.Package) string {
	if p == nil {
		return "main"
	}
	return p.Path()
}

func (r *rtypes) namedType(t *types.Named) (reflect.Type, error) {
	if rt, ok := r.named[t]; ok {
		return rt, nil
	}
	if rt := r.in.natives.reflectOf(t); rt != nil {
		r.named[t] = rt
		return rt, nil
	}
	if t.Obj().Pkg() == nil && t.Obj().Name() == "error" {
		return errorRT, nil
	}
	if _, ok := t.Underlying().(*types.Interface); ok {
		return anyRT, nil
	}
	if r.inProgress[t] {
		return standIn(t), errCycle
	}
	r.inProgress[t] = true
	rt, err := r.get(t.Underlying())
	delete(r.inProgress, t)
	if err != nil && (err != errCycle || rt == nil) {
		return rt, err
	}
	if err == errCycle && r.dependsOnOther(t) {
		// Part of a larger cycle still being built: valid, not final.
		return rt, errCycle
	}
	r.named[t] = rt
	return rt, nil
}

// dependsOnOther reports whether another named type is still being built
// (t's representation may then contain its stand-in).
func (r *rtypes) dependsOnOther(t *types.Named) bool {
	for n := range r.inProgress {
		if n != t {
			return true
		}
	}
	return false
}

// --- native packages as go/types packages ------------------------------

// natives converts reflect types of compiled packages into go/types types
// and builds go/types packages from the native registry.
type natives struct {
	in    *Interp
	types map[reflect.Type]types.Type
	back  map[*types.Named]reflect.Type
	pkgs  map[string]*types.Package
	done  map[string]bool
}

func newNatives(in *Interp) *natives {
	return &natives{in: in, types: map[reflect.Type]types.Type{}, back: map[*types.Named]reflect.Type{},
		pkgs: map[string]*types.Package{}, done: map[string]bool{}}
}

func (n *natives) reflectOf(t *types.Named) reflect.Type {
	if rt, ok := n.back[t]; ok {
		return rt
	}
	if o := t.Origin(); o != t {
		return nil
	}
	return nil
}

func (n *natives) pkg(path string) *types.Package {
	if p, ok := n.pkgs[path]; ok {
		return p
	}
	name := source.GuessName(path)
	if np := native.Lookup(path); np != nil {
		name = np.Name
	}
	p := types.NewPackage(path, name)
	n.pkgs[path] = p
	return p
}

// typeOf maps a reflect type of a compiled package to go/types.
func (n *natives) typeOf(rt reflect.Type) types.Type {
	if t, ok := n.types[rt]; ok {
		return t
	}
	if rt == errorRT {
		return types.Universe.Lookup("error").Type()
	}
	if rt == unsafePtrT {
		return types.Typ[types.UnsafePointer] // reflect names it unsafe.Pointer
	}
	if rt.Name() != "" && rt.PkgPath() != "" {
		return n.namedOf(rt)
	}
	t := n.structural(rt)
	n.types[rt] = t
	return t
}

var basicKinds = map[reflect.Kind]types.BasicKind{
	reflect.Bool: types.Bool, reflect.String: types.String,
	reflect.Int: types.Int, reflect.Int8: types.Int8, reflect.Int16: types.Int16, reflect.Int32: types.Int32, reflect.Int64: types.Int64,
	reflect.Uint: types.Uint, reflect.Uint8: types.Uint8, reflect.Uint16: types.Uint16, reflect.Uint32: types.Uint32,
	reflect.Uint64: types.Uint64, reflect.Uintptr: types.Uintptr,
	reflect.Float32: types.Float32, reflect.Float64: types.Float64,
	reflect.Complex64: types.Complex64, reflect.Complex128: types.Complex128,
	reflect.UnsafePointer: types.UnsafePointer,
}

func (n *natives) structural(rt reflect.Type) types.Type {
	if k, ok := basicKinds[rt.Kind()]; ok {
		return types.Typ[k]
	}
	switch rt.Kind() {
	case reflect.Pointer:
		return types.NewPointer(n.typeOf(rt.Elem()))
	case reflect.Slice:
		return types.NewSlice(n.typeOf(rt.Elem()))
	case reflect.Array:
		return types.NewArray(n.typeOf(rt.Elem()), int64(rt.Len()))
	case reflect.Map:
		return types.NewMap(n.typeOf(rt.Key()), n.typeOf(rt.Elem()))
	case reflect.Chan:
		dir := types.SendRecv
		switch rt.ChanDir() {
		case reflect.SendDir:
			dir = types.SendOnly
		case reflect.RecvDir:
			dir = types.RecvOnly
		}
		return types.NewChan(dir, n.typeOf(rt.Elem()))
	case reflect.Func:
		return n.signature(rt, nil)
	case reflect.Struct:
		fields := make([]*types.Var, rt.NumField())
		tags := make([]string, rt.NumField())
		for i := range fields {
			f := rt.Field(i)
			p := n.pkg(strings.TrimPrefix(f.PkgPath, "wy/"))
			if f.PkgPath == "" {
				p = nil
			}
			fields[i] = types.NewField(token.NoPos, p, f.Name, n.typeOf(f.Type), f.Anonymous)
			tags[i] = string(f.Tag)
		}
		return types.NewStruct(fields, tags)
	case reflect.Interface:
		var methods []*types.Func
		for i := 0; i < rt.NumMethod(); i++ {
			m := rt.Method(i)
			var p *types.Package
			if m.PkgPath != "" {
				p = n.pkg(m.PkgPath)
			}
			methods = append(methods, types.NewFunc(token.NoPos, p, m.Name, n.signature(m.Type, nil)))
		}
		return types.NewInterfaceType(methods, nil).Complete()
	}
	panic("wy: unsupported native type " + rt.String())
}

func (n *natives) signature(rt reflect.Type, recv *types.Var) *types.Signature {
	var params, results []*types.Var
	for i := 0; i < rt.NumIn(); i++ {
		params = append(params, types.NewParam(token.NoPos, nil, "", n.typeOf(rt.In(i))))
	}
	for i := 0; i < rt.NumOut(); i++ {
		results = append(results, types.NewParam(token.NoPos, nil, "", n.typeOf(rt.Out(i))))
	}
	return types.NewSignatureType(recv, nil, nil, types.NewTuple(params...), types.NewTuple(results...), rt.IsVariadic())
}

func (n *natives) namedOf(rt reflect.Type) types.Type {
	// Instances of iter.Seq/Seq2 come from compiled code (strings.SplitSeq
	// and friends); map them to the interpreted iter package's types.
	if rt.PkgPath() == "iter" && rt.Kind() == reflect.Func {
		if t := n.iterInstance(rt); t != nil {
			n.types[rt] = t
			return t
		}
	}
	p := n.pkg(rt.PkgPath())
	obj := types.NewTypeName(token.NoPos, p, rt.Name(), nil)
	named := types.NewNamed(obj, nil, nil)
	n.types[rt] = named
	n.back[named] = rt
	named.SetUnderlying(n.structural(rt).Underlying())
	if rt.Kind() != reflect.Interface {
		seen := map[string]bool{}
		for i := 0; i < rt.NumMethod(); i++ {
			m := rt.Method(i)
			seen[m.Name] = true
			recv := types.NewParam(token.NoPos, p, "", named)
			named.AddMethod(types.NewFunc(token.NoPos, p, m.Name, n.methodSig(m.Type, recv)))
		}
		pt := reflect.PointerTo(rt)
		for i := 0; i < pt.NumMethod(); i++ {
			m := pt.Method(i)
			if seen[m.Name] {
				continue
			}
			recv := types.NewParam(token.NoPos, p, "", types.NewPointer(named))
			named.AddMethod(types.NewFunc(token.NoPos, p, m.Name, n.methodSig(m.Type, recv)))
		}
	}
	return named
}

// methodSig converts a reflect method type (receiver first) to a signature.
func (n *natives) methodSig(rt reflect.Type, recv *types.Var) *types.Signature {
	var params, results []*types.Var
	for i := 1; i < rt.NumIn(); i++ {
		params = append(params, types.NewParam(token.NoPos, nil, "", n.typeOf(rt.In(i))))
	}
	for i := 0; i < rt.NumOut(); i++ {
		results = append(results, types.NewParam(token.NoPos, nil, "", n.typeOf(rt.Out(i))))
	}
	return types.NewSignatureType(recv, nil, nil, types.NewTuple(params...), types.NewTuple(results...), rt.IsVariadic())
}

func (n *natives) iterInstance(rt reflect.Type) types.Type {
	iterPkg := n.in.pkgs["iter"]
	if iterPkg == nil || rt.NumIn() != 1 {
		return nil
	}
	yield := rt.In(0)
	var args []types.Type
	for i := 0; i < yield.NumIn(); i++ {
		args = append(args, n.typeOf(yield.In(i)))
	}
	name := "Seq"
	if len(args) == 2 {
		name = "Seq2"
	}
	origin := iterPkg.Scope().Lookup(name)
	if origin == nil {
		return nil
	}
	inst, err := types.Instantiate(n.in.tctx, origin.Type(), args, false)
	if err != nil {
		return nil
	}
	return inst
}

// buildPackage creates the go/types view of a registered native package.
func (n *natives) buildPackage(np *native.Package) *types.Package {
	p := n.pkg(np.Path)
	if n.done[np.Path] {
		return p
	}
	n.done[np.Path] = true
	scope := p.Scope()
	for name, rt := range np.Types {
		t := n.typeOf(rt)
		if named, ok := t.(*types.Named); ok && named.Obj().Pkg() == p && named.Obj().Name() == name {
			scope.Insert(named.Obj())
			continue
		}
		scope.Insert(types.NewTypeName(token.NoPos, p, name, t)) // alias
	}
	for name, fv := range np.Funcs {
		scope.Insert(types.NewFunc(token.NoPos, p, name, n.signature(fv.Type(), nil)))
	}
	for name, vp := range np.Vars {
		scope.Insert(types.NewVar(token.NoPos, p, name, n.typeOf(vp.Type().Elem())))
	}
	for name, c := range np.Consts {
		var t types.Type
		if c.Type != nil {
			t = n.typeOf(c.Type)
		} else {
			t = types.Typ[map[string]types.BasicKind{
				"bool": types.UntypedBool, "int": types.UntypedInt, "rune": types.UntypedRune,
				"float": types.UntypedFloat, "complex": types.UntypedComplex, "string": types.UntypedString,
			}[c.Untyped]]
		}
		scope.Insert(types.NewConst(token.NoPos, p, name, t, c.Value))
	}
	p.MarkComplete()
	return p
}

var _ = constant.MakeBool

// reinterpret views the bits of v as type to, which has the same layout
// (used where a stand-in type stores a value of a self-referential type).
func reinterpret(v reflect.Value, to reflect.Type) reflect.Value {
	if !v.IsValid() || v.Type() == to {
		return v
	}
	if !v.CanAddr() {
		tmp := reflect.New(v.Type()).Elem()
		tmp.Set(v)
		v = tmp
	}
	return reflect.NewAt(to, v.Addr().UnsafePointer()).Elem()
}

// viewer returns a function reinterpreting values stored as storage into
// the canonical type, or nil when no reinterpretation is needed.
func viewer(storage, canonical reflect.Type) func(reflect.Value) reflect.Value {
	if storage == canonical {
		return nil
	}
	return func(v reflect.Value) reflect.Value { return reinterpret(v, canonical) }
}
