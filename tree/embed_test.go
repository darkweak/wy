package tree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, src := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A GOPATH holding a plugin with a subpackage and a vendored dependency,
// laid out as Traefik downloads plugins.
func pluginGoPath(t *testing.T) string {
	gopath := t.TempDir()
	writeFiles(t, gopath, map[string]string{
		"src/example.com/plugin/go.mod": "module example.com/plugin\n\ngo 1.24\n\nrequire example.org/dep v1.0.0\n",
		"src/example.com/plugin/plugin.go": `package plugin

import (
	"example.com/plugin/internal/greet"
	"example.org/dep"
)

func Hello() string { return greet.Prefix() + dep.Name() }
`,
		"src/example.com/plugin/internal/greet/greet.go": `package greet

import "example.org/dep"

func Prefix() string { return "hello " + dep.Name()[:0] }
`,
		"src/example.com/plugin/vendor/modules.txt":            "# example.org/dep v1.0.0\n## explicit\nexample.org/dep\n",
		"src/example.com/plugin/vendor/example.org/dep/dep.go": "package dep\n\nfunc Name() string { return \"vendored\" }\n",
		// A copy outside the plugin's vendor directory must not be used.
		"src/example.org/dep/dep.go": "package dep\n\nfunc Name() string { return \"gopath\" }\n",
	})
	return gopath
}

func TestGoPathAndVendor(t *testing.T) {
	in := New(Options{GoPath: pluginGoPath(t)})
	vals, err := in.Eval(context.Background(), `import "example.com/plugin"
plugin.Hello()`)
	if err != nil {
		t.Fatal(err)
	}
	if vals[0] != "hello vendored" {
		t.Fatalf("Hello() = %v, want hello vendored", vals[0])
	}
}

func TestModuleVendor(t *testing.T) {
	gopath := pluginGoPath(t)
	in := New(Options{ModuleDir: filepath.Join(gopath, "src", "example.com", "plugin")})
	vals, err := in.Eval(context.Background(), `import "example.org/dep"
dep.Name()`)
	if err != nil {
		t.Fatal(err)
	}
	if vals[0] != "vendored" {
		t.Fatalf("Name() = %v, want vendored", vals[0])
	}
}

func TestForbid(t *testing.T) {
	gopath := t.TempDir()
	writeFiles(t, gopath, map[string]string{
		"src/example.com/deep/deep.go": "package deep\n\nimport \"example.org/mem\"\n\nvar N = mem.Size()\n",
		"src/example.com/deep/vendor/example.org/mem/mem.go": `package mem

import "unsafe"

func Size() uintptr { return unsafe.Sizeof(0) }
`,
	})
	ctx := context.Background()
	in := New(Options{GoPath: gopath, Forbid: []string{"unsafe", "syscall"}})
	for _, src := range []string{
		`import "unsafe"`,
		`import "syscall"`,
		`import "syscall/js"`,
		`import "example.com/deep"`, // through a vendored dependency
	} {
		_, err := in.Eval(ctx, src)
		if err == nil || !strings.Contains(err.Error(), "is not allowed") {
			t.Errorf("%s: err = %v, want a forbidden import", src, err)
		}
	}
	// wy's own sources of slices and maps import unsafe.
	vals, err := in.Eval(ctx, `import "slices"
slices.Max([]int{1, 3, 2})`)
	if err != nil {
		t.Fatal(err)
	}
	if vals[0] != 3 {
		t.Fatalf("slices.Max = %v", vals[0])
	}
}

func TestStreams(t *testing.T) {
	var stdout, stderr bytes.Buffer
	in := New(Options{Stdin: strings.NewReader("42\n"), Stdout: &stdout, Stderr: &stderr})
	_, err := in.Eval(context.Background(), `import ("fmt"; "log")
var n int
fmt.Scan(&n)
fmt.Println("out", n)
fmt.Printf("%d\n", n+1)
p := fmt.Print
p("via value\n")
log.SetFlags(0)
log.Println("err")
log.Default().Print("default")
println("builtin")`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "out 42\n43\nvia value\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "err\ndefault\nbuiltin\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

func TestRestricted(t *testing.T) {
	var stderr bytes.Buffer
	in := New(Options{Restricted: true, Stderr: &stderr})
	ctx := context.Background()

	_, err := in.Eval(ctx, `import "os"
func() {
	defer func() { recover() }()
	os.Exit(3)
}()`)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 {
		t.Fatalf("os.Exit: err = %v, want exit status 3", err)
	}

	_, err = in.Eval(ctx, `import "log"
log.Fatal("boom")`)
	var pe *PanicError
	if !errors.As(err, &pe) || !strings.Contains(stderr.String(), "boom") {
		t.Fatalf("log.Fatal: err = %v, stderr = %q", err, stderr.String())
	}

	vals, err := in.Eval(ctx, `_, err := os.FindProcess(os.Getpid()); err != nil`)
	if err != nil {
		t.Fatal(err)
	}
	if vals[0] != true {
		t.Fatal("os.FindProcess of the host process succeeded")
	}
}

func TestEnv(t *testing.T) {
	t.Setenv("WY_HOST_ONLY", "host")
	in := New(Options{Env: []string{"WY_A=1"}})
	vals, err := in.Eval(context.Background(), `import "os"
os.Setenv("WY_B", "2")
_, host := os.LookupEnv("WY_HOST_ONLY")
os.Getenv("WY_A"), os.ExpandEnv("$WY_B"), host`)
	if err != nil {
		t.Fatal(err)
	}
	if vals[0] != "1" || vals[1] != "2" || vals[2] != false {
		t.Fatalf("got %v", vals)
	}
	if _, ok := os.LookupEnv("WY_B"); ok {
		t.Fatal("os.Setenv changed the host environment")
	}
}

// Optional methods survive the crossing into compiled code, and only the
// ones the interpreted type has.
func TestInterfaceFamilies(t *testing.T) {
	in := New(Options{})
	vals, err := in.Eval(context.Background(), `import ("io"; "net/http")
type flushing struct{ http.ResponseWriter }
func (w flushing) Flush() {}
func (w flushing) Unwrap() http.ResponseWriter { return w.ResponseWriter }
type plain struct{ http.ResponseWriter }
type unwrapping struct{ http.ResponseWriter }
func (w unwrapping) Unwrap() http.ResponseWriter { return w.ResponseWriter }
type reader struct{}
func (reader) Read(p []byte) (int, error) { return 0, io.EOF }
func (reader) WriteTo(w io.Writer) (int64, error) { n, err := w.Write([]byte("to")); return int64(n), err }
func wrap(w http.ResponseWriter) (http.ResponseWriter, http.ResponseWriter, http.ResponseWriter) {
	return flushing{w}, plain{w}, unwrapping{w}
}
var r io.Reader = reader{}
wrap, r`)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f, p, u := vals[0].(func(http.ResponseWriter) (http.ResponseWriter, http.ResponseWriter, http.ResponseWriter))(rec)
	if _, ok := f.(http.Flusher); !ok {
		t.Error("Flush lost")
	}
	if _, ok := f.(http.Hijacker); ok {
		t.Error("Hijack added")
	}
	if _, ok := p.(http.Flusher); ok {
		t.Error("Flush added")
	}
	// ResponseController reaches the recorder's own Flush through Unwrap.
	if err := http.NewResponseController(u).Flush(); err != nil || !rec.Flushed {
		t.Errorf("ResponseController.Flush: %v, flushed %v", err, rec.Flushed)
	}
	if _, ok := vals[1].(io.WriterTo); !ok {
		t.Error("WriteTo lost")
	}
	var b bytes.Buffer
	if _, err := io.Copy(&b, vals[1].(io.Reader)); err != nil || b.String() != "to" {
		t.Errorf("io.Copy = %q, %v", b.String(), err)
	}
}

// Compiled code runs the same interpreted functions from many goroutines
// (an HTTP handler serving concurrent requests): stores must not share
// state between calls.
func TestConcurrentCalls(t *testing.T) {
	in := New(Options{})
	vals, err := in.Eval(context.Background(), `type state struct{ n, k, v int }
func set(s *state, n int) int {
	for i := 0; i < 100; i++ {
		s.n = n
		if s.n != n {
			return -1
		}
	}
	for s.k, s.v = range []int{n} {
	}
	return s.n + s.v
}
set`)
	if err != nil {
		t.Fatal(err)
	}
	vals2, err := in.Eval(context.Background(), `func() *state { return &state{} }`)
	if err != nil {
		t.Fatal(err)
	}
	set := reflect.ValueOf(vals[0])
	mk := reflect.ValueOf(vals2[0])
	errs := make(chan error, 64)
	for g := range 64 {
		go func() {
			for range 50 {
				s := mk.Call(nil)[0]
				got := set.Call([]reflect.Value{s, reflect.ValueOf(g)})[0].Int()
				if got != int64(2*g) {
					errs <- fmt.Errorf("goroutine %d: got %d", g, got)
					return
				}
			}
			errs <- nil
		}()
	}
	for range 64 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
