![logo](https://github.com/darkweak/wy/blob/master/wy.jpg?raw=true)

# wy (Working Yaegi) - Go, on the fly

`wy` runs Go code on the fly: scripts, one-liners, an interactive session,
and an embeddable `Eval` API, like [Yaegi](https://github.com/traefik/yaegi).
It has two engines.

- **Compiled (the default).** Code is compiled by the real Go toolchain and
  runs natively. In interactive and embedded use, each input is compiled as a
  plugin and loaded into the live process. The whole language and every
  library work, at full speed.
- **Tree-walking interpreter (`-tree`, package `tree`).** Like Yaegi, it
  interprets Go source without compiling it. It starts instantly and needs no
  Go toolchain at run time. Unlike Yaegi, it is built on `go/types`, runs on
  real Go memory (so `unsafe` works), and keeps interpreted types' methods
  visible to compiled code. It passes 89% of the Go repository's own runnable
  tests, against 55% for Yaegi, and is faster than Yaegi on every benchmark
  below.

## Comparison

**Probe programs** (`compare/run.sh`; Go 1.24.7, Yaegi v0.16.1, linux/amd64):

| program | wy | wy -tree | yaegi |
|---|---|---|---|
| CPU-bound recursion (fib 32) | ✅ 0.03s | ✅ 2.4s | ✅ 3.8s |
| generics, generic methods, `slices.SortFunc` | ✅ | ✅ | ❌ `undefined selector: slices.SortFunc` |
| `unsafe` (Sizeof, Add, String, SliceData) | ✅ | ✅ | ❌ cannot import `unsafe` |
| range-over-func iterators, `iter`, `maps.Keys` | ✅ | ✅ | ❌ cannot import `iter` |
| reflection on embedded types and method sets | ✅ | ❌ sees structure, not names | ❌ same, then panics |
| `errors.As` with an interpreted error type | ✅ | ✅ | ❌ panics |
| goroutines, channels, `sync/atomic` (200k jobs) | ✅ | ✅ | ✅ |
| `runtime.Caller`, `FuncForPC` | ✅ | ✅ | ❌ reports interpreter internals |
| interface equality, struct map keys, JSON embedding | ✅ | ✅ | ❌ `a == b` wrong |
| generic aliases, recursive generic types | ✅ | ✅ | ❌ `incomplete type T` panic |

**The Go test suite.** These are the 939 programs in `$GOROOT/test` marked
`// run`, from the directories below. A program passes if it exits 0 with
its expected output (`compare/gotest.sh`).

| directory | programs | wy -tree | yaegi |
|---|---|---|---|
| `test/` | 138 | 109 | 54 |
| `test/ken` | 40 | 38 | 33 |
| `test/typeparam` (generics) | 140 | 135 | 59 |
| `test/fixedbugs` | 593 | 526 | 352 |
| `test/chan` | 17 | 17 | 8 |
| `test/interface` | 11 | 11 | 6 |
| **total** | **939** | **836 (89%)** | **512 (55%)** |

Most of the remaining tree-mode failures probe the gc runtime itself:
finalizer and GC timing, allocation counts, the text of stack traces, and
reflection on interpreted type names. The compiled engine passes them by
construction.

**Speed** (best of 3, including startup):

| program | wy (compiled, cached) | wy -tree | yaegi |
|---|---|---|---|
| fib(30), recursive calls | 0.015s | 0.87s | 1.43s |
| 10M-iteration loop, map updates, appends | 0.044s | 0.43s | 0.66s |
| sieve of 2M (slice reads and writes) | 0.017s | 0.20s | 0.37s |

The compiled engine takes about 0.17s to build a script the first time, then
about 10ms per run from its cache, and about 0.3–0.4s per interactive input.
The tree engine starts in milliseconds.

## Install

```sh
go install github.com/darkweak/wy/cmd/wy@latest
go install ./cmd/wy      # or from this directory
```

The compiled engine needs a Go toolchain on `PATH` at run time (or
`WY_GO`). The tree engine (`-tree`) does not.

## Scripts and one-liners

A file can be a normal `package main` program, or a *snippet*: any mix of
imports, declarations and statements, with no package clause and no
`func main`. Missing standard-library imports are added automatically.

```go
#!/usr/bin/env wy
type pair[K comparable, V any] struct{ k K; v V }

counts := map[string]int{}
for _, w := range strings.Fields("a b a c a") { counts[w]++ }
fmt.Println(counts, os.Args[1:])
```

```sh
chmod +x tool && ./tool x y        # works as a #! interpreter
wy tool x y                       # same thing
wy -tree tool x y                 # interpreted: instant start, no toolchain
wy -e 'fmt.Println(time.Now().Weekday())'
echo 'fmt.Println(42)' | wy -
wy run ./cmd/server               # a main package directory
wy build -o tool tool.go          # standalone executable
wy gen tool.go                    # show the generated Go file
```

- Error messages, panics and `runtime.Caller` point at your own file and
  lines, through `//line` directives.
- Compiled binaries are cached by content hash. A repeat run `exec`s the
  cached binary, so signals, stdin and exit codes behave natively.
- Third-party imports are resolved automatically with `go mod tidy`
  (compiled engine).
- A script that lives inside a Go module can import that module's packages.
  The compiled engine builds it inside the module with `go build -overlay`,
  writing nothing to your tree. The tree engine interprets those packages
  from source.

## Interactive session

```
$ wy            # or: wy -tree
wy> type point struct{ x, y int }
wy> func (p point) dist() float64 { return math.Hypot(float64(p.x), float64(p.y)) }
note: redeclared point with its methods; values created earlier keep the previous point type
wy> p := point{3, 4}
wy> p.dist()
5
wy> p.x, p.y
3, 4
wy> go func() { time.Sleep(time.Second); fmt.Println("later") }()
wy> import "github.com/google/uuid"
wy> uuid.New().Version()
VERSION_4
```

- Variables, types, functions and imports persist between inputs.
- A trailing expression, or an `a, b` list, is printed.
- Input continues over several lines while brackets are open.
- Commands: `:vars`, `:src N` (generated Go, compiled engine only),
  `:load FILE`, `:time`, `:reset`, `:quit`.
- Ctrl-C clears the current input or abandons a running one.

## Embedding

**Compiled engine** (`github.com/darkweak/wy/interp`):

```go
s, _ := interp.New(interp.Options{})
defer s.Close()

s.Eval(ctx, `func discount(o model.Order) float64 { ... }`)
v, _ := s.Get(ctx, "discount")
discount := v.(func(model.Order) float64) // a native Go func

s.Set(ctx, "orders", orders)              // enters with its static type []model.Order
vals, _ := s.Eval(ctx, `len(orders)`)     // vals[0] == 2
```

- Interpreted code can import any package the host links, including the
  host's own packages if `Options.HostModuleDir` is set. It then shares
  their live state.
- `go.mod` versions are pinned to the host's build info.
- The host's build flags (`-race`, `-tags`, `-trimpath`, ...) and toolchain
  are reproduced.
- `examples/rules` shows a host loading business rules written in Go at run
  time.

**Tree engine** (`github.com/darkweak/wy/tree`):

```go
in := tree.New(tree.Options{})
in.Eval(ctx, `import "example.com/host/api"
type polite struct{}
func (polite) Greet(n string) string { return "hi " + n }`)
vals, _ := in.Eval(ctx, `var g api.Greeter = polite{}; g`)
g := vals[0].(api.Greeter) // the host's own interface, implemented by interpreted code
```

- The standard library is available out of the box.
- Make other compiled packages available with `wy extract PKG...`, which
  generates symbol tables and interface wrappers like `yaegi extract`. Then
  import the generated package in the host.
- `in.RunSource` runs a program or snippet; `in.RunFiles` runs a main
  package.
- Source packages are found in the module of `Options.ModuleDir`, then in
  the `vendor` directories enclosing the importing package, then under
  `Options.GoPath/src` (GOPATH layout, as Yaegi's `GoPath`).
- Options keep interpreted code apart from the host, as Yaegi does for
  embedded use:
  - `Stdin`, `Stdout` and `Stderr` receive the `fmt.Print`/`Scan`
    functions, the standard logger, `flag.CommandLine` and the `print`
    builtins;
  - `Env` gives the code a private copy of the environment;
  - `Restricted` makes `os.Exit` and `log.Fatal` panic instead of ending
    the host process;
  - `Forbid` lists packages the code may not import, such as `unsafe` and
    `syscall`.

Both engines return errors as type-check errors with input positions, or as
a `PanicError` (the session stays usable). If the context is cancelled,
`Eval` returns, but the code keeps running in the background.

## How the compiled engine works

**Scripts** (`script` package). The snippet is split into top-level units:
imports and declarations go to package level, and statements go into `main`.
Each unit is placed with a `//line` directive so positions map back to the
user's source. Unresolved qualifiers such as `strings.` become imports. The
result is built with `go build` and cached under the content hash.

**Interactive inputs** (`interp` package). Each input N becomes its own
package `cN`, built with `-buildmode=plugin` and loaded with `plugin.Open`.
Several mechanisms make this behave like one evolving program:

1. **One type-checking universe.** All inputs are type-checked with
   `go/types` as one package. Earlier inputs' objects stay in the shared
   scope, a redefinition replaces a name, and a failed input is rolled back.
2. **Lifting.** Top-level `x := …` becomes a package-level variable, with its
   inferred type written out as Go source (`internal/typeexpr`).
3. **Linking by rewriting.** References to earlier inputs become qualified,
   exported references. A later `point{1}` compiles to `_c3.X_point{1}`.
   Mangling is deterministic, so method names line up across inputs.
4. **Type redeclaration.** Adding a method to an earlier type replays that
   type's declaration and its other methods into the new input, like
   redefining a class in Python.
5. **Shared runtime.** Plugins reuse packages the process already loaded, so
   inputs and the host share the same variables and libraries.

## How the tree engine works

- **`go/types` checks everything.** The interpreter executes only checked
  code. Constants, conversions, method sets, generic instantiation, embedding
  and assignability come straight from the reference implementation, so
  there is no second type system to drift from the spec.
- **Closure compilation.** Each function is compiled once into a tree of Go
  closures. Expressions of basic types compile to typed closures
  (`func(*frame) int64`, `float64`, `string`, `bool`), so arithmetic,
  comparisons, loop conditions, local assignments and slice-element stores
  do not allocate.
- **Real memory.** Every type maps to a `reflect` type with gc's layout:
  interpreted structs are `reflect.StructOf` types, and unexported fields
  keep their names. Variables are addressable memory, so `&x`, closures and
  `unsafe` all see the same storage. `unsafe.Sizeof`/`Offsetof`/`Alignof`,
  pointer arithmetic, `unsafe.Slice`/`String`/`StringData`/`SliceData` and
  reinterpreting memory behave as compiled Go does.
- **Self-referential types.** Types such as
  `type stateFn func(*lexer) stateFn`, `type T []T` or linked structs are
  stored through layout-identical opaque stand-ins (a pointer word or a
  slice header). They are reinterpreted at container boundaries, with that
  check precomputed per site.
- **Interfaces.** A value whose dynamic type is an interpreted named type is
  boxed with the interpreter's type, so type switches, assertions, equality
  and map keys behave exactly. When such a value crosses into compiled code,
  it is wrapped in a generated type exposing exactly its methods:
  - generated wrappers for every standard interface;
  - 415 combinations of the optional methods that `fmt`, `errors` and the
    encoders probe for (`String`, `Error`, `Unwrap`, `Is`, `As`, `Format`,
    `MarshalJSON`, ...);
  - every combination of the optional methods probed on an
    `http.ResponseWriter` (`Flush`, `Hijack`, `Push`, `ReadFrom`, `Unwrap`,
    `CloseNotify`), an `io.Reader` (`WriteTo`) and an `io.Writer`
    (`ReadFrom`), so streaming and WebSocket upgrades work through an
    interpreted `ResponseWriter` wrapper.

  Compiled code then sees the same methods it would in a compiled program.
  How a value is presented depends on the callee:
  - printers (`fmt`, `log`) get display mirrors, so nested elements print
    with their methods;
  - containers (`sync.Map`, `context`, `container/*`) get boxes, so values
    come back with their identity;
  - reflection-based code (`sort.Slice`, `text/template`, `json.Unmarshal`)
    gets the plain value.
- **Generics** are monomorphized: each instantiation is compiled with its
  type arguments substituted.
- **Standard library.** Compiled functions are called through generated
  symbol tables. They cover 160 packages, intersected across Linux, macOS and
  Windows so the tables build everywhere. Generic code runs from embedded
  source: `slices`, `maps`, `cmp` and `iter` entirely, plus `sync.OnceValue`,
  `atomic.Pointer[T]` and `reflect.TypeFor` as overlays merged into their
  native packages.
- **Intrinsics.** A few functions depend on things only the interpreter
  knows:
  - `errors.As` compares Go types;
  - `runtime.Caller` and `FuncForPC` read the interpreted call stack;
  - `fmt`'s `%T` is rewritten to the interpreted type name.
- **Runtime semantics.** These follow the spec exactly:
  - `defer`/`recover` follows the "called directly by a deferred function"
    rule;
  - assignments run in two phases;
  - loop variables are per-iteration;
  - `range` over functions supports `break`, `continue`, `return` and labels;
  - `select`, goroutines and channels are the real ones.

## Limitations

**Compiled engine:**

- **Platforms.** Interactive and embedded use needs plugin support (Linux,
  macOS, FreeBSD), cgo and a C compiler. Scripts work anywhere Go does.
  On macOS, plugins are linked without DWARF debug info (`-ldflags=-w`)
  to work around a Go linker bug that recent dyld versions reject
  ("chained fixups, seg_count does not match number of segments",
  [golang/go#81793](https://github.com/golang/go/issues/81793)). Debuggers
  therefore show no source lines for interactive inputs on macOS.
- **Toolchain.** A Go toolchain must be present at run time.
- **Latency.** Each interactive input takes about 0.3–0.4s. The first input
  after installing a new Go version takes several seconds while the standard
  library is compiled for plugin mode.
- **Memory.** Plugins cannot be unloaded, so each input stays in memory.
- **Field names.** Unexported fields of interactive types print as `X_x`
  under `%+v`; `encoding/json` still ignores them.
- **Unnameable variables.** A variable whose type cannot be written down
  stays local to its input.
- **Interactive constructs.** Dot imports and cgo are not supported
  interactively.

**Tree engine:**

- **Reflection sees interpreted types structurally.** Go cannot create named
  types at run time, so `reflect.TypeOf`, `%#v` and `encoding/xml` element
  names see an interpreted type's underlying structure without its name or
  methods. `%T` and `%v` are handled. Yaegi has the same limitation.
- **Speed.** Code runs roughly 10–60× slower than compiled Go (faster than
  Yaegi). Startup is instant.
- **Runtime introspection.** Finalizer and GC timing, allocation counts and
  `runtime.Stack` text reflect the interpreter, not your program.
- **Bodyless functions.** Functions implemented in assembly or via
  `//go:linkname` cannot run, except the few the interpreter provides.
- **Generic APIs of other compiled packages.** Generic functions of compiled
  packages other than the ones listed above are not available, because
  reflection cannot instantiate them.
- **Uncovered packages.** Packages without symbol tables are interpreted
  from source if they belong to the current module. Otherwise, generate
  tables with `wy extract`.
- **Runaway code.** Goroutines cannot be killed, as in the compiled engine.

## Layout

```
cmd/wy/             CLI and REPL (both engines)
script/              snippet wrapping, cached builds, module-aware runs
interp/              compiled engine: type-checking, lifting, codegen, plugins
tree/                tree-walking interpreter
tree/native/         registry of compiled packages, combo wrappers
tree/stdlib/         generated symbol tables for the standard library
tree/stdsrc/         embedded sources: slices, maps, cmp, iter, overlays
bridge/              package shared by host and compiled inputs
internal/source/     snippet splitter, import index, type redeclaration
internal/extract/    symbol table generator (wy extract)
internal/typeexpr/   go/types -> Go source type printer
internal/typescope/  go/types scope editing for incremental checking
internal/gocmd/      go command invocation, host build reproduction
examples/rules/      embedding example
compare/             Yaegi comparison programs, Go test-suite runner
```

Run the tests with `go test ./...`. The tree engine's conformance tests run
each program in `tree/testdata` with both `go run` and the interpreter and
require identical output. The compiled engine's tests build about 100
plugins and take about 30s. Regenerate the standard library tables with
`go generate ./tree/stdlib`.
