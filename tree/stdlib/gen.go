//go:build ignore

// gen regenerates the standard-library symbol tables:
//
//	go generate github.com/darkweak/wy/tree/stdlib
//
// Symbols are intersected across linux, darwin and windows so the tables
// compile everywhere.
package main

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/darkweak/wy/internal/extract"
	"github.com/darkweak/wy/internal/gocmd"
)

// Packages that are not extracted: internal or platform-specific ones, and
// the generic packages the interpreter runs from source (see tree/stdsrc).
var skip = map[string]bool{
	"syscall": true, "unsafe": true, "plugin": true, "log/syslog": true,
	"runtime/cgo": true, "runtime/race": true, "runtime/msan": true, "runtime/asan": true,
	"cmp": true, "slices": true, "maps": true, "iter": true, "unique": true, "weak": true,
	"testing/synctest": true, "structs": true,
}

func main() {
	out, err := gocmd.Run(context.Background(), "", []string{"GOFLAGS=", "GOWORK=off"}, "list", "std")
	if err != nil {
		log.Fatal(err)
	}
	var pkgs []string
	for _, p := range strings.Fields(string(out)) {
		if skip[p] || strings.Contains(p, "internal") || strings.HasPrefix(p, "vendor/") ||
			strings.HasPrefix(p, "cmd/") || strings.HasPrefix(p, "syscall/") || strings.HasPrefix(p, "simd") {
			continue
		}
		pkgs = append(pkgs, p)
	}
	err = extract.Run(context.Background(), extract.Options{
		Packages: pkgs,
		OutDir:   ".",
		PkgName:  "stdlib",
		GOOS:     []string{"linux", "darwin", "windows"},
		Log:      os.Stderr,
	})
	if err != nil {
		log.Fatal(err)
	}
}
