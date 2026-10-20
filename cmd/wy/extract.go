package main

import (
	"context"
	"errors"
	"flag"
	"os"

	"github.com/darkweak/wy/internal/extract"
)

// extractCmd generates native symbol tables for packages, so that programs
// run with -tree (or the tree package) can call them. Import the generated
// package for its side effects in the host program.
func extractCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	out := fs.String("o", "wysyms", "output directory")
	pkg := fs.String("pkg", "wysyms", "package name of the generated files")
	goos := fs.String("goos", "", "comma-separated platforms whose common API is extracted (default: current)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: wy extract [-o DIR] [-pkg NAME] PKG...")
	}
	var platforms []string
	if *goos != "" {
		for _, g := range splitComma(*goos) {
			platforms = append(platforms, g)
		}
	}
	return extract.Run(ctx, extract.Options{
		Packages: fs.Args(),
		OutDir:   *out,
		PkgName:  *pkg,
		GOOS:     platforms,
		Dir:      ".",
		Log:      os.Stderr,
	})
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}
