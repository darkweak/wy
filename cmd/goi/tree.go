package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"wy/tree"
)

// runTree interprets a program or snippet and exits like a compiled
// program would (status 2 on an unrecovered panic).
func runTree(ctx context.Context, src []byte, name string, args []string) error {
	dir := "."
	if name != "-e" && name != "stdin" {
		dir = filepath.Dir(name)
	}
	in := tree.New(tree.Options{ModuleDir: dir})
	return treeExit(in.RunSource(ctx, src, name, args))
}

func runTreeDir(ctx context.Context, dir string, args []string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	files := map[string][]byte{}
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			data, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				return err
			}
			files[filepath.Join(dir, n)] = data
		}
	}
	in := tree.New(tree.Options{ModuleDir: dir})
	os.Args = append([]string{filepath.Base(dir)}, args...)
	return treeExit(in.RunFiles(ctx, files))
}

func treeExit(err error) error {
	var pe *tree.PanicError
	if errors.As(err, &pe) {
		fmt.Fprintf(os.Stderr, "panic: %v\n", pe.Value)
		os.Exit(2)
	}
	return err
}
