package tree

import (
	"go/types"
)

// substMap substitutes type parameters with type arguments. Generic code is
// compiled once per instantiation (monomorphization), with every type in
// the body passed through typ.
type substMap struct {
	in  *Interp
	env map[*types.TypeParam]types.Type
}

func (s *substMap) bind(params *types.TypeParamList, args []types.Type) {
	if s.env == nil {
		s.env = map[*types.TypeParam]types.Type{}
	}
	for i := 0; i < params.Len() && i < len(args); i++ {
		s.env[params.At(i)] = args[i]
	}
}

func (s *substMap) empty() bool { return s == nil || len(s.env) == 0 }

func (s *substMap) sig(t *types.Signature) *types.Signature {
	if s.empty() {
		return t
	}
	return s.typ(t).(*types.Signature)
}

func (s *substMap) typ(t types.Type) types.Type {
	if s.empty() || t == nil {
		return t
	}
	return s.walk(t, map[types.Type]types.Type{})
}

func (s *substMap) walk(t types.Type, seen map[types.Type]types.Type) types.Type {
	if r, ok := seen[t]; ok {
		return r
	}
	switch t := t.(type) {
	case *types.TypeParam:
		if r, ok := s.env[t]; ok {
			return r
		}
		return t
	case *types.Basic:
		return t
	case *types.Pointer:
		e := s.walk(t.Elem(), seen)
		if e == t.Elem() {
			return t
		}
		return types.NewPointer(e)
	case *types.Slice:
		e := s.walk(t.Elem(), seen)
		if e == t.Elem() {
			return t
		}
		return types.NewSlice(e)
	case *types.Array:
		e := s.walk(t.Elem(), seen)
		if e == t.Elem() {
			return t
		}
		return types.NewArray(e, t.Len())
	case *types.Map:
		k, v := s.walk(t.Key(), seen), s.walk(t.Elem(), seen)
		if k == t.Key() && v == t.Elem() {
			return t
		}
		return types.NewMap(k, v)
	case *types.Chan:
		e := s.walk(t.Elem(), seen)
		if e == t.Elem() {
			return t
		}
		return types.NewChan(t.Dir(), e)
	case *types.Tuple:
		if t == nil {
			return t
		}
		vars := make([]*types.Var, t.Len())
		changed := false
		for i := range vars {
			v := t.At(i)
			nt := s.walk(v.Type(), seen)
			if nt != v.Type() {
				changed = true
			}
			vars[i] = types.NewParam(v.Pos(), v.Pkg(), v.Name(), nt)
		}
		if !changed {
			return t
		}
		return types.NewTuple(vars...)
	case *types.Signature:
		params := s.walk(t.Params(), seen).(*types.Tuple)
		results := s.walk(t.Results(), seen).(*types.Tuple)
		recv := t.Recv()
		if recv != nil && !isInterface(recv.Type()) {
			rt := s.walk(recv.Type(), seen)
			if rt != recv.Type() {
				recv = types.NewParam(recv.Pos(), recv.Pkg(), recv.Name(), rt)
			}
		}
		if params == t.Params() && results == t.Results() && recv == t.Recv() && t.TypeParams().Len() == 0 {
			return t
		}
		return types.NewSignatureType(recv, nil, nil, params, results, t.Variadic())
	case *types.Struct:
		fields := make([]*types.Var, t.NumFields())
		tags := make([]string, t.NumFields())
		changed := false
		for i := range fields {
			f := t.Field(i)
			nt := s.walk(f.Type(), seen)
			if nt != f.Type() {
				changed = true
			}
			fields[i] = types.NewField(f.Pos(), f.Pkg(), f.Name(), nt, f.Embedded())
			tags[i] = t.Tag(i)
		}
		if !changed {
			return t
		}
		return types.NewStruct(fields, tags)
	case *types.Interface:
		seen[t] = t // methods' receivers refer back to the interface
		var methods []*types.Func
		changed := false
		for i := 0; i < t.NumExplicitMethods(); i++ {
			m := t.ExplicitMethod(i)
			ns := s.walk(m.Type(), seen).(*types.Signature)
			if ns != m.Type() {
				changed = true
			}
			methods = append(methods, types.NewFunc(m.Pos(), m.Pkg(), m.Name(), ns))
		}
		var embeds []types.Type
		for i := 0; i < t.NumEmbeddeds(); i++ {
			e := s.walk(t.EmbeddedType(i), seen)
			if e != t.EmbeddedType(i) {
				changed = true
			}
			embeds = append(embeds, e)
		}
		if !changed {
			return t
		}
		return types.NewInterfaceType(methods, embeds).Complete()
	case *types.Union:
		terms := make([]*types.Term, t.Len())
		for i := range terms {
			terms[i] = types.NewTerm(t.Term(i).Tilde(), s.walk(t.Term(i).Type(), seen))
		}
		return types.NewUnion(terms)
	case *types.Named:
		args := t.TypeArgs()
		if args == nil || args.Len() == 0 {
			return t
		}
		nargs := make([]types.Type, args.Len())
		changed := false
		for i := range nargs {
			nargs[i] = s.walk(args.At(i), seen)
			if nargs[i] != args.At(i) {
				changed = true
			}
		}
		if !changed {
			return t
		}
		inst, err := types.Instantiate(s.in.tctx, t.Origin(), nargs, false)
		if err != nil {
			panic(err)
		}
		return inst
	case *types.Alias:
		return s.walk(types.Unalias(t), seen)
	}
	return t
}
