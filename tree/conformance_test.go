package tree

import (
	"bytes"
	"context"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// runInterp interprets a program file and returns what it printed to stdout.
func runInterp(t *testing.T, file string) (string, error) {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	var buf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { io.Copy(&buf, r); wg.Done() }()
	in := New(Options{})
	runErr := in.RunFiles(context.Background(), map[string][]byte{filepath.Base(file): src})
	w.Close()
	os.Stdout = old
	wg.Wait()
	return buf.String(), runErr
}

// knownDiff lists programs whose output differs for a documented reason.
var knownDiff = map[string]string{
	"05_reflect_methods": "reflection sees interpreted named types as their underlying types (Go cannot create named types at run time)",
}

// TestConformance runs every program in testdata both with `go run` and
// with the interpreter, and requires identical output.
func TestConformance(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.go")
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".go")
		t.Run(name, func(t *testing.T) {
			if why, ok := knownDiff[name]; ok {
				t.Skip(why)
			}
			want, err := osexec.Command("go", "run", f).CombinedOutput()
			if err != nil {
				t.Fatalf("go run: %v\n%s", err, want)
			}
			got, err := runInterp(t, f)
			if err != nil {
				t.Fatalf("interpreter error: %v\noutput so far:\n%s", err, got)
			}
			if got != string(want) {
				t.Fatalf("output differs\n--- go run\n%s\n--- interpreter\n%s", want, got)
			}
		})
	}
}
