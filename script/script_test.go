package script

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runOut(t *testing.T, src, name string, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code, err := Run(context.Background(), []byte(src), name, args, Options{Stdout: &out, Stderr: &out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out.String(), code
}

func TestSnippet(t *testing.T) {
	t.Setenv("WY_CACHE", t.TempDir())
	src := `#!/usr/bin/env wy
type pair[K comparable, V any] struct{ k K; v V }
func best[K comparable](m map[K]int) (p pair[K, int]) {
	for k, v := range m { if v > p.v { p = pair[K, int]{k, v} } }
	return
}
counts := map[string]int{}
for _, w := range strings.Fields("a b a c a b") { counts[w]++ }
fmt.Println(best(counts), os.Args[1:])
if len(os.Args) > 2 { os.Exit(3) }
`
	out, code := runOut(t, src, "snip.go", "x")
	if out != "{a 3} [x]\n" || code != 0 {
		t.Fatalf("got %q (exit %d)", out, code)
	}
	// Second run hits the binary cache.
	start := time.Now()
	out, code = runOut(t, src, "snip.go", "x", "y")
	if code != 3 || !strings.HasPrefix(out, "{a 3} [x y]") {
		t.Fatalf("got %q (exit %d)", out, code)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("cached run took %v", d)
	}
}

func TestErrorPositions(t *testing.T) {
	t.Setenv("WY_CACHE", t.TempDir())
	_, err := Build(context.Background(), []byte("x := 1\n\nfmt.Println(x, nope)\n"), "pos.go", Options{})
	if err == nil || !strings.Contains(err.Error(), "pos.go:3:16: undefined: nope") {
		t.Fatalf("want error at pos.go:3:16, got %v", err)
	}
}

func TestFullFileGetsImports(t *testing.T) {
	t.Setenv("WY_CACHE", t.TempDir())
	src := "package main\n\nfunc main() {\n\tfmt.Println(strings.Repeat(\"ab\", 2))\n}\n"
	out, _ := runOut(t, src, "full.go")
	if out != "abab\n" {
		t.Fatalf("got %q", out)
	}
}

func TestModuleScript(t *testing.T) {
	t.Setenv("WY_CACHE", t.TempDir())
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/proj\n\ngo 1.22\n"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "lib"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "lib", "lib.go"), []byte("package lib\n\nfunc Name() string { return \"proj-lib\" }\n"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "tools"), 0o755))
	file := filepath.Join(dir, "tools", "task.go")
	src := "import \"example.com/proj/lib\"\nfmt.Println(lib.Name())\n"
	must(os.WriteFile(file, []byte(src), 0o644))
	out, _ := runOut(t, src, file)
	if out != "proj-lib\n" {
		t.Fatalf("got %q", out)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".wy") {
			t.Fatalf("module directory was modified: %s", e.Name())
		}
	}
}
