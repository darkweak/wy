package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// wyBin is the wy command built once for the whole test run.
var wyBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wy-cmd-test")
	if err != nil {
		panic(err)
	}
	wyBin = filepath.Join(dir, "wy")
	if runtime.GOOS == "windows" {
		wyBin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", wyBin, ".").CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		panic("building wy: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type result struct {
	stdout, stderr string
	code           int
}

// wy runs the wy command in dir with stdin and returns its outcome. The
// build cache is per test so runs do not interfere.
func wy(t *testing.T, dir, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(wyBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "WY_CACHE="+t.TempDir())
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := result{stdout.String(), stderr.String(), 0}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		r.code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r result) ok(t *testing.T, stdout string) {
	t.Helper()
	if r.code != 0 || r.stdout != stdout {
		t.Fatalf("exit %d\nstdout: %q\nwant:   %q\nstderr: %s", r.code, r.stdout, stdout, r.stderr)
	}
}

// engines runs a subtest with each engine.
func engines(t *testing.T, f func(t *testing.T, flags []string)) {
	t.Run("compiled", func(t *testing.T) { f(t, nil) })
	t.Run("tree", func(t *testing.T) { f(t, []string{"-tree"}) })
}

const snippet = `type pair[K comparable, V any] struct{ k K; v V }

counts := map[string]int{}
for _, w := range strings.Fields("a b a c a") { counts[w]++ }
p := pair[string, int]{"a", counts["a"]}
fmt.Println(p.k, p.v, len(counts), os.Args[1:])
`

func TestExpr(t *testing.T) {
	engines(t, func(t *testing.T, flags []string) {
		wy(t, t.TempDir(), "", append(flags, "-e", `fmt.Println(strings.ToUpper("hi"), os.Args[1:])`, "x", "-y")...).
			ok(t, "HI [x -y]\n")
	})
}

func TestStdin(t *testing.T) {
	engines(t, func(t *testing.T, flags []string) {
		wy(t, t.TempDir(), snippet, append(flags, "-", "q")...).ok(t, "a 3 3 [q]\n")
	})
}

func TestScriptFile(t *testing.T) {
	engines(t, func(t *testing.T, flags []string) {
		dir := t.TempDir()
		file := filepath.Join(dir, "tool")
		os.WriteFile(file, []byte("#!/usr/bin/env wy\n"+snippet), 0o755)
		wy(t, dir, "", append(flags, "tool", "a", "b")...).ok(t, "a 3 3 [a b]\n")
		wy(t, dir, "", append(flags, "run", "tool", "c")...).ok(t, "a 3 3 [c]\n")
	})
}

func TestMainPackageFile(t *testing.T) {
	engines(t, func(t *testing.T, flags []string) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "prog.go"), []byte(`package main

import "fmt"

func main() { fmt.Println("main package") }
`), 0o644)
		wy(t, dir, "", append(flags, "prog.go")...).ok(t, "main package\n")
	})
}

func TestRunDir(t *testing.T) {
	engines(t, func(t *testing.T, flags []string) {
		dir := t.TempDir()
		app := filepath.Join(dir, "app")
		os.MkdirAll(app, 0o755)
		// Compiled mode builds the directory with go build, which needs a module.
		os.WriteFile(filepath.Join(app, "go.mod"), []byte("module app\n"), 0o644)
		os.WriteFile(filepath.Join(app, "main.go"), []byte(`package main

import (
	"fmt"
	"os"
)

func main() { fmt.Println(greet(), os.Args[1:]) }
`), 0o644)
		os.WriteFile(filepath.Join(app, "greet.go"), []byte(`package main

func greet() string { return "hello from dir" }
`), 0o644)
		os.WriteFile(filepath.Join(app, "greet_test.go"), []byte(`package main

this file must be ignored
`), 0o644)
		wy(t, dir, "", append(flags, "run", "app", "z")...).ok(t, "hello from dir [z]\n")
	})
}

func TestExitCodes(t *testing.T) {
	engines(t, func(t *testing.T, flags []string) {
		dir := t.TempDir()
		r := wy(t, dir, "", append(flags, "-e", `fmt.Println("bye"); os.Exit(3)`)...)
		if r.code != 3 || r.stdout != "bye\n" {
			t.Fatalf("os.Exit(3): exit %d, stdout %q, stderr %s", r.code, r.stdout, r.stderr)
		}
		r = wy(t, dir, "", append(flags, "-e", `panic("boom")`)...)
		if r.code != 2 || !strings.Contains(r.stderr, "panic: boom") {
			t.Fatalf("panic: exit %d, stderr %s", r.code, r.stderr)
		}
		r = wy(t, dir, "", append(flags, "-e", `x := 1`)...)
		if r.code != 1 || !strings.Contains(r.stderr, "declared and not used") {
			t.Fatalf("compile error: exit %d, stderr %s", r.code, r.stderr)
		}
	})
}

func TestErrorPositions(t *testing.T) {
	engines(t, func(t *testing.T, flags []string) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "bad.go"), []byte("fmt.Println(1)\n\nvar s string = 42\n"), 0o644)
		r := wy(t, dir, "", append(flags, "bad.go")...)
		if r.code != 1 || !strings.Contains(r.stderr, "bad.go:3") {
			t.Fatalf("exit %d, stderr %s; want an error at bad.go:3", r.code, r.stderr)
		}
	})
}

func TestMissingFile(t *testing.T) {
	r := wy(t, t.TempDir(), "", "nope.go")
	if r.code != 1 || !strings.HasPrefix(r.stderr, "wy: ") {
		t.Fatalf("exit %d, stderr %s", r.code, r.stderr)
	}
	r = wy(t, t.TempDir(), "", "run")
	if r.code != 1 || !strings.Contains(r.stderr, "missing file or directory") {
		t.Fatalf("exit %d, stderr %s", r.code, r.stderr)
	}
}

func TestGen(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "s.go"), []byte(snippet), 0o644)
	r := wy(t, dir, "", "gen", "s.go")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"package main", "func main()", `"strings"`, `"fmt"`, `"os"`, "//line"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("generated source lacks %q:\n%s", want, r.stdout)
		}
	}
	if r := wy(t, dir, "", "gen"); r.code != 1 || !strings.Contains(r.stderr, "usage: wy gen FILE") {
		t.Fatalf("gen without file: exit %d, stderr %s", r.code, r.stderr)
	}
}

func TestBuild(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.go"), []byte(`fmt.Println("built", os.Args[1:])`), 0o644)
	wy(t, dir, "", "build", "hello.go").ok(t, "")
	out, err := exec.Command(filepath.Join(dir, "hello"), "1").Output()
	if err != nil || string(out) != "built [1]\n" {
		t.Fatalf("running built binary: %q, %v", out, err)
	}
	wy(t, dir, "", "build", "-o", "other", "hello.go").ok(t, "")
	if _, err := os.Stat(filepath.Join(dir, "other")); err != nil {
		t.Fatal(err)
	}
	if r := wy(t, dir, "", "build"); r.code != 1 || !strings.Contains(r.stderr, "usage: wy build") {
		t.Fatalf("build without file: exit %d, stderr %s", r.code, r.stderr)
	}
}

func TestCache(t *testing.T) {
	dir := t.TempDir()
	cache := t.TempDir()
	cmd := exec.Command(wyBin, "cache")
	cmd.Env = append(os.Environ(), "WY_CACHE="+cache)
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != cache {
		t.Fatalf("wy cache = %q, %v; want %s", out, err, cache)
	}
	// A run fills the cache, and clean empties it.
	run := exec.Command(wyBin, "-e", `fmt.Println(1)`)
	run.Dir = dir
	run.Env = cmd.Env
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	scripts := filepath.Join(cache, "scripts")
	if entries, _ := os.ReadDir(scripts); len(entries) == 0 {
		t.Fatal("no cached script after a run")
	}
	clean := exec.Command(wyBin, "cache", "clean")
	clean.Env = cmd.Env
	if out, err := clean.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := os.Stat(scripts); !os.IsNotExist(err) {
		t.Fatalf("cached scripts remain after clean: %v", err)
	}
}

func TestVersionAndHelp(t *testing.T) {
	r := wy(t, t.TempDir(), "", "version")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "wy ") || !strings.Contains(r.stdout, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Fatalf("version: exit %d, %q", r.code, r.stdout)
	}
	r = wy(t, t.TempDir(), "", "help")
	if r.code != 0 || r.stdout != usage {
		t.Fatalf("help: exit %d, %q", r.code, r.stdout)
	}
	r = wy(t, t.TempDir(), "", "-h")
	if r.code != 0 || r.stderr != usage {
		t.Fatalf("-h: exit %d, stderr %q", r.code, r.stderr)
	}
	r = wy(t, t.TempDir(), "", "-nosuchflag")
	if r.code != 1 {
		t.Fatalf("unknown flag: exit %d", r.code)
	}
}

func TestREPL(t *testing.T) {
	engines(t, func(t *testing.T, flags []string) {
		in := strings.Join([]string{
			"x := 40",
			"func add(a, b int) int {",
			"	return a + b",
			"}",
			"add(x, 2)",
			`"str"`,
			":nosuch",
			":time",
			":quit",
			"fmt.Println(\"not reached\")",
		}, "\n") + "\n"
		r := wy(t, t.TempDir(), in, flags...)
		if r.code != 0 {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		for _, want := range []string{"wy - Go", "wy> ", ".... ", "42", `"str"`, "timing on"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
			}
		}
		if strings.Contains(r.stdout, "not reached") {
			t.Errorf(":quit did not stop the session:\n%s", r.stdout)
		}
		if !strings.Contains(r.stderr, "unknown command :nosuch") {
			t.Errorf("stderr lacks unknown command error: %s", r.stderr)
		}
	})
}

func TestREPLLoadAndReset(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "defs.go"), []byte("y := 7\n"), 0o644)
	in := ":load defs.go\ny * 6\n:reset\ny\n:help\n"
	r := wy(t, dir, in, "-tree")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"42", "session reset", ":quit"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	// After :reset, y is gone.
	if !strings.Contains(r.stderr, "undefined: y") {
		t.Errorf("stderr lacks undefined y: %s", r.stderr)
	}
}

func TestSplitComma(t *testing.T) {
	for in, want := range map[string][]string{
		"":                     nil,
		"linux":                {"linux"},
		"linux,darwin":         {"linux", "darwin"},
		",linux,,windows,":     {"linux", "windows"},
		"linux,darwin,windows": {"linux", "darwin", "windows"},
	} {
		if got := splitComma(in); !slices.Equal(got, want) {
			t.Errorf("splitComma(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormat(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want string
	}{
		{"a\"b", `"a\"b"`},
		{'x', `120 ('x')`},
		{errors.New("bad"), "error: bad"},
		{nil, "nil"},
		{42, "42"},
		{[]int{1, 2}, "[1 2]"},
	} {
		if got := format(tc.v); got != tc.want {
			t.Errorf("format(%#v) = %q, want %q", tc.v, got, tc.want)
		}
	}
}

func TestTrimStack(t *testing.T) {
	stack := `goroutine 1 [running]:
runtime/debug.Stack()
	/go/src/runtime/debug/stack.go:26 +0x5e
main.f()
	cell3:2 +0x10
main.g()
	cell1:5 +0x20
wy/interp.run()
	/wy/interp/session.go:100 +0x30
`
	want := "  main.f()\n    cell3:2 +0x10\n  main.g()\n    cell1:5 +0x20\n"
	if got := trimStack(stack); got != want {
		t.Fatalf("trimStack:\n%s\nwant:\n%s", got, want)
	}
	if got := trimStack("goroutine 1 [running]:\nmain.main()\n\t/x.go:1\n"); got != "" {
		t.Fatalf("trimStack without user frames = %q", got)
	}
}

func TestExtractCmd(t *testing.T) {
	dir := t.TempDir()
	root, _ := filepath.Abs("../..")
	r := wy(t, root, "", "extract", "-o", dir, "-pkg", "syms", "-goos", "linux,darwin", "container/ring")
	if r.code != 0 || !strings.Contains(r.stderr, "container/ring:") {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "zsym_container_ring.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "package syms") || !strings.Contains(string(data), "ring.New") {
		t.Fatalf("unexpected output:\n%s", data)
	}
	if r := wy(t, root, "", "extract"); r.code != 1 || !strings.Contains(r.stderr, "usage: wy extract") {
		t.Fatalf("extract without packages: exit %d, stderr %s", r.code, r.stderr)
	}
}
