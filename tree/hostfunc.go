package tree

import (
	"go/ast"
	"go/types"
	"hash/maphash"
	"math/rand/v2"
	"reflect"
	"strings"
	"time"
	"unsafe"
)

// hostImpl implements, in the host program, a source function that has no Go
// body: compiled Go provides it in assembly or pulls it from the runtime or
// another package with //go:linkname. res holds the reflect types of the
// function's results.
type hostImpl func(args []reflect.Value, res []reflect.Type) []reflect.Value

// hostImpls is keyed by the //go:linkname target of a function, or by its own
// "importpath.Name" for assembly stubs.
var hostImpls = map[string]hostImpl{
	"runtime.nanotime":  func(_ []reflect.Value, res []reflect.Type) []reflect.Value { return results(res, nanotime()) },
	"runtime.cputicks":  func(_ []reflect.Value, res []reflect.Type) []reflect.Value { return results(res, nanotime()) },
	"runtime.fastrand":  func(_ []reflect.Value, res []reflect.Type) []reflect.Value { return results(res, rand.Uint32()) },
	"runtime.cheaprand": func(_ []reflect.Value, res []reflect.Type) []reflect.Value { return results(res, rand.Uint32()) },
	"runtime.fastrand64": func(_ []reflect.Value, res []reflect.Type) []reflect.Value {
		return results(res, rand.Uint64())
	},
	"runtime.cheaprand64": func(_ []reflect.Value, res []reflect.Type) []reflect.Value {
		return results(res, rand.Uint64())
	},
	"runtime.memhash": func(args []reflect.Value, res []reflect.Type) []reflect.Value {
		b := unsafe.Slice((*byte)(args[0].UnsafePointer()), args[2].Uint())
		return results(res, memhash(b, args[1].Uint()))
	},
	"runtime.strhash": func(args []reflect.Value, res []reflect.Type) []reflect.Value {
		s := *(*string)(args[0].UnsafePointer())
		return results(res, memhash(unsafe.Slice(unsafe.StringData(s), len(s)), args[1].Uint()))
	},
	"runtime.memclrNoHeapPointers": func(args []reflect.Value, _ []reflect.Type) []reflect.Value {
		clear(unsafe.Slice((*byte)(args[0].UnsafePointer()), args[1].Uint()))
		return nil
	},
	// The auxiliary vector is not exposed: callers fall back to
	// /proc/self/auxv or to no hardware capabilities.
	"runtime.getAuxv": zeroResults,
	// Unsupported: callers fall back to the getrandom system call.
	"runtime.vgetrandom": zeroResults,
	// CPU feature registers read in assembly: report no optional
	// features, so that callers take their portable code paths.
	"golang.org/x/sys/cpu.cpuid":                zeroResults,
	"golang.org/x/sys/cpu.xgetbv":               zeroResults,
	"golang.org/x/sys/cpu.getisar0":             zeroResults,
	"golang.org/x/sys/cpu.getisar1":             zeroResults,
	"golang.org/x/sys/cpu.getpfr0":              zeroResults,
	"golang.org/x/sys/cpu.getzfr0":              zeroResults,
	"golang.org/x/sys/cpu.getmmfr1":             zeroResults,
	"golang.org/x/sys/cpu.darwinSupportsAVX512": zeroResults,
}

var (
	hostStart = time.Now()
	hostSeed  = maphash.MakeSeed()
)

func nanotime() int64 { return int64(time.Since(hostStart)) + 1 }

func memhash(b []byte, seed uint64) uint64 {
	var h maphash.Hash
	h.SetSeed(hostSeed)
	var s [8]byte
	for i := range s {
		s[i] = byte(seed >> (8 * i))
	}
	h.Write(s[:])
	h.Write(b)
	return h.Sum64()
}

// results converts vals to the result types res; a nil value is the zero
// value of its type.
func results(res []reflect.Type, vals ...any) []reflect.Value {
	out := make([]reflect.Value, len(res))
	for i, rt := range res {
		if i >= len(vals) || vals[i] == nil {
			out[i] = reflect.Zero(rt)
			continue
		}
		out[i] = reflect.ValueOf(vals[i]).Convert(rt)
	}
	return out
}

func zeroResults(_ []reflect.Value, res []reflect.Type) []reflect.Value { return results(res) }

// hostFunc returns the host implementation of fn, a function without a
// body, or nil.
func (in *Interp) hostFunc(fn *types.Func) *function {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() != nil {
		return nil
	}
	impl := hostImpls[in.linknames[fn]]
	if impl == nil {
		impl = hostImpls[fn.Pkg().Path()+"."+fn.Name()]
	}
	if impl == nil {
		return nil
	}
	res := make([]reflect.Type, sig.Results().Len())
	for i := range res {
		res[i] = in.rtype(sig.Results().At(i).Type())
	}
	return in.builtinFunc(fn.Name(), sig, func(args []reflect.Value) []reflect.Value {
		return impl(args, res)
	})
}

// collectLinknames records the //go:linkname targets of the functions of pkg
// that have no body.
func (in *Interp) collectLinknames(pkg *types.Package, files []*ast.File) {
	for _, f := range files {
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				rest, ok := strings.CutPrefix(c.Text, "//go:linkname ")
				if !ok {
					continue
				}
				fields := strings.Fields(rest)
				if len(fields) != 2 {
					continue
				}
				if fn, ok := pkg.Scope().Lookup(fields[0]).(*types.Func); ok {
					in.linknames[fn] = fields[1]
				}
			}
		}
	}
}
