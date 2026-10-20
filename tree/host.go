package tree

import (
	"errors"
	"flag"
	"fmt"
	"go/types"
	"log"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/darkweak/wy/tree/native"
)

// importer resolves the imports of one package being type-checked.
type importer struct {
	in      *Interp
	trusted bool // wy's embedded sources: exempt from Options.Forbid
}

func (im importer) Import(path string) (*types.Package, error) {
	return im.ImportFrom(path, "", 0)
}

// ImportFrom implements types.ImporterFrom: dir is the directory of the
// importing package, where vendor directories are looked up.
func (im importer) ImportFrom(path, dir string, _ types.ImportMode) (*types.Package, error) {
	if !im.trusted {
		for _, f := range im.in.opts.Forbid {
			if path == f || strings.HasPrefix(path, f+"/") {
				return nil, fmt.Errorf("import of package %s is not allowed", path)
			}
		}
	}
	return im.in.importPkg(path, dir)
}

// sourceDir returns the directory holding the source of a package that is
// neither standard nor registered, or "": the module of Options.ModuleDir,
// then the vendor directories enclosing the importing package (from the
// innermost), then Options.GoPath.
func (in *Interp) sourceDir(path, fromDir string) string {
	if in.modPath != "" && (path == in.modPath || strings.HasPrefix(path, in.modPath+"/")) {
		return filepath.Join(in.modRoot, strings.TrimPrefix(strings.TrimPrefix(path, in.modPath), "/"))
	}
	rel := filepath.FromSlash(path)
	for _, v := range in.vendorDirs(fromDir) {
		if d := filepath.Join(v, rel); isDir(d) {
			return d
		}
	}
	if in.opts.GoPath != "" {
		if d := filepath.Join(in.opts.GoPath, "src", rel); isDir(d) {
			return d
		}
	}
	return ""
}

// vendorDirs lists the vendor directories visible from a package directory,
// innermost first. Like the go command, it only looks inside the module
// root and GoPath/src; code without a directory (Eval inputs) sees the
// module's vendor directory.
func (in *Interp) vendorDirs(fromDir string) []string {
	var roots []string
	if in.modRoot != "" {
		roots = append(roots, in.modRoot)
	}
	if in.opts.GoPath != "" {
		if src, err := filepath.Abs(filepath.Join(in.opts.GoPath, "src")); err == nil {
			roots = append(roots, src)
		}
	}
	dir := ""
	if fromDir != "" && fromDir != "." {
		dir, _ = filepath.Abs(fromDir)
	}
	var out []string
	for _, root := range roots {
		if dir == "" || !within(dir, root) {
			continue
		}
		for d := dir; ; d = filepath.Dir(d) {
			out = append(out, filepath.Join(d, "vendor"))
			if d == root {
				break
			}
		}
		return out
	}
	if in.modRoot != "" {
		out = append(out, filepath.Join(in.modRoot, "vendor"))
	}
	return out
}

func within(dir, root string) bool {
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// errRestricted is returned by the functions that Options.Restricted denies.
var errRestricted = errors.New("restricted by the interpreter")

// hostOverrides returns the native packages whose symbols this interpreter
// replaces, so that interpreted code writes to the interpreter's streams
// and environment rather than the host process's.
func (in *Interp) hostOverrides() map[string]*native.Package {
	stdin, stdout, stderr := in.opts.Stdin, in.opts.Stdout, in.opts.Stderr
	out := map[string]*native.Package{}
	override := func(path string, funcs, vars map[string]reflect.Value) {
		np := native.Lookup(path)
		if np == nil {
			return
		}
		cp := *np
		cp.Funcs = maps.Clone(np.Funcs)
		cp.Vars = maps.Clone(np.Vars)
		maps.Copy(cp.Funcs, funcs)
		maps.Copy(cp.Vars, vars)
		out[path] = &cp
	}

	override("fmt", map[string]reflect.Value{
		"Print":   reflect.ValueOf(func(a ...any) (int, error) { return fmt.Fprint(stdout, a...) }),
		"Printf":  reflect.ValueOf(func(f string, a ...any) (int, error) { return fmt.Fprintf(stdout, f, a...) }),
		"Println": reflect.ValueOf(func(a ...any) (int, error) { return fmt.Fprintln(stdout, a...) }),
		"Scan":    reflect.ValueOf(func(a ...any) (int, error) { return fmt.Fscan(stdin, a...) }),
		"Scanf":   reflect.ValueOf(func(f string, a ...any) (int, error) { return fmt.Fscanf(stdin, f, a...) }),
		"Scanln":  reflect.ValueOf(func(a ...any) (int, error) { return fmt.Fscanln(stdin, a...) }),
	}, nil)

	l := log.New(stderr, "", log.LstdFlags)
	logFuncs := map[string]reflect.Value{
		"Default":   reflect.ValueOf(func() *log.Logger { return l }),
		"Fatal":     reflect.ValueOf(l.Fatal),
		"Fatalf":    reflect.ValueOf(l.Fatalf),
		"Fatalln":   reflect.ValueOf(l.Fatalln),
		"Flags":     reflect.ValueOf(l.Flags),
		"Output":    reflect.ValueOf(l.Output),
		"Panic":     reflect.ValueOf(l.Panic),
		"Panicf":    reflect.ValueOf(l.Panicf),
		"Panicln":   reflect.ValueOf(l.Panicln),
		"Prefix":    reflect.ValueOf(l.Prefix),
		"Print":     reflect.ValueOf(l.Print),
		"Printf":    reflect.ValueOf(l.Printf),
		"Println":   reflect.ValueOf(l.Println),
		"SetFlags":  reflect.ValueOf(l.SetFlags),
		"SetOutput": reflect.ValueOf(l.SetOutput),
		"SetPrefix": reflect.ValueOf(l.SetPrefix),
		"Writer":    reflect.ValueOf(l.Writer),
	}
	if in.opts.Restricted {
		logFuncs["Fatal"] = reflect.ValueOf(l.Panic)
		logFuncs["Fatalf"] = reflect.ValueOf(l.Panicf)
		logFuncs["Fatalln"] = reflect.ValueOf(l.Panicln)
	}
	override("log", logFuncs, nil)

	cl := flag.NewFlagSet(os.Args[0], flag.PanicOnError)
	cl.SetOutput(stderr)
	override("flag", nil, map[string]reflect.Value{"CommandLine": reflect.ValueOf(&cl)})

	osFuncs := map[string]reflect.Value{}
	osVars := map[string]reflect.Value{}
	if f, ok := stdin.(*os.File); ok {
		osVars["Stdin"] = reflect.ValueOf(&f)
	}
	if f, ok := stdout.(*os.File); ok {
		osVars["Stdout"] = reflect.ValueOf(&f)
	}
	if f, ok := stderr.(*os.File); ok {
		osVars["Stderr"] = reflect.ValueOf(&f)
	}
	if in.opts.Restricted {
		osFuncs["Exit"] = reflect.ValueOf(func(code int) { panic(&ExitError{Code: code}) })
		osFuncs["FindProcess"] = reflect.ValueOf(func(pid int) (*os.Process, error) {
			if pid == os.Getpid() {
				return nil, errRestricted
			}
			return os.FindProcess(pid)
		})
	}
	if in.opts.Env != nil {
		maps.Copy(osFuncs, envFuncs(in.opts.Env))
	}
	override("os", osFuncs, osVars)
	return out
}

// envFuncs implements the os environment functions over a private copy of
// env.
func envFuncs(env []string) map[string]reflect.Value {
	var mu sync.RWMutex
	vars := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	lookup := func(key string) (string, bool) {
		mu.RLock()
		defer mu.RUnlock()
		v, ok := vars[key]
		return v, ok
	}
	getenv := func(key string) string {
		v, _ := lookup(key)
		return v
	}
	return map[string]reflect.Value{
		"Getenv":    reflect.ValueOf(getenv),
		"LookupEnv": reflect.ValueOf(lookup),
		"ExpandEnv": reflect.ValueOf(func(s string) string { return os.Expand(s, getenv) }),
		"Setenv": reflect.ValueOf(func(key, value string) error {
			if key == "" || strings.ContainsAny(key, "=\x00") {
				return &os.SyscallError{Syscall: "setenv", Err: errors.New("invalid argument")}
			}
			mu.Lock()
			defer mu.Unlock()
			vars[key] = value
			return nil
		}),
		"Unsetenv": reflect.ValueOf(func(key string) error {
			mu.Lock()
			defer mu.Unlock()
			delete(vars, key)
			return nil
		}),
		"Clearenv": reflect.ValueOf(func() {
			mu.Lock()
			defer mu.Unlock()
			clear(vars)
		}),
		"Environ": reflect.ValueOf(func() []string {
			mu.RLock()
			defer mu.RUnlock()
			out := make([]string, 0, len(vars))
			for k, v := range vars {
				out = append(out, k+"="+v)
			}
			return out
		}),
	}
}
