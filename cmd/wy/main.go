// Command wy runs Go code on the fly: programs, snippets and an
// interactive session, all compiled by the real Go toolchain.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/darkweak/wy/internal/gocmd"
	"github.com/darkweak/wy/script"
)

const usage = `wy runs Go code on the fly, compiled by the real Go toolchain.

Usage:
  wy                          start an interactive session
  wy FILE [args...]           run a Go program or snippet (usable in #! lines)
  wy run FILE|DIR [args...]   same, explicitly; DIR runs a main package
  wy -e 'CODE' [args...]      run code given on the command line
  wy - [args...]              run code read from standard input
  wy build [-o OUT] FILE      compile a program or snippet to an executable
  wy gen FILE                 print the Go source generated for a snippet
  wy cache [dir|clean]        show or clear the build cache
  wy extract [-o DIR] [-pkg NAME] PKG...
                               generate symbol tables so the tree interpreter
                               can use compiled packages (like yaegi extract)
  wy version

Add -tree to any of these to use the tree-walking interpreter instead of
the compiler: it starts instantly and needs no Go toolchain, at the cost of
slower execution and reflection seeing interpreted types structurally.

Snippets may mix imports, declarations and statements without a package
clause or func main; missing standard-library imports are added for you.

Flags:
  -tree       interpret without compiling (tree-walking interpreter)
  -x          print the go commands being run (interactive mode)
  -fresh      ignore the binary cache and rebuild
  -standalone build outside any enclosing Go module

Environment: WY_GO (go command), WY_CACHE (cache dir), WY_ROOT (wy source).
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "wy:", strings.TrimSpace(err.Error()))
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("wy", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	expr := fs.String("e", "", "code to run")
	verbose := fs.Bool("x", false, "print go commands")
	fresh := fs.Bool("fresh", false, "rebuild, ignoring the cache")
	standalone := fs.Bool("standalone", false, "build outside any enclosing module")
	useTree := fs.Bool("tree", false, "interpret with the tree-walking interpreter")
	// Flags stop at the first non-flag argument, so a script's own flags
	// are passed through untouched.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	opts := script.Options{Rebuild: *fresh, Standalone: *standalone}
	rest := fs.Args()
	ctx := context.Background()
	treeMode = *useTree

	if *expr != "" {
		return runSource(ctx, []byte(*expr), "-e", "wy", rest, opts)
	}
	if len(rest) == 0 {
		return repl(*verbose, *useTree)
	}
	switch rest[0] {
	case "run":
		if len(rest) < 2 {
			return errors.New("run: missing file or directory")
		}
		return runPath(ctx, rest[1], rest[2:], opts)
	case "build":
		return buildCmd(ctx, rest[1:], opts)
	case "gen":
		if len(rest) != 2 {
			return errors.New("usage: wy gen FILE")
		}
		src, err := os.ReadFile(rest[1])
		if err != nil {
			return err
		}
		p, err := script.Prepare(string(src), rest[1])
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(p.Code)
		return err
	case "extract":
		return extractCmd(ctx, rest[1:])
	case "cache":
		if len(rest) > 1 && rest[1] == "clean" {
			return script.CleanCache()
		}
		fmt.Println(gocmd.CacheDir())
		return nil
	case "version":
		v, _ := gocmd.Version()
		fmt.Printf("wy %s (built with %s, using %s) %s/%s\n", gocmd.WyVersion, runtime.Version(), v, runtime.GOOS, runtime.GOARCH)
		if runtime.GOOS == "darwin" {
			fmt.Println("macOS: plugins are linked with -ldflags=-w (workaround for golang.org/issue/81793)")
		}
		return nil
	case "help":
		fmt.Print(usage)
		return nil
	case "-":
		src, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		return runSource(ctx, src, "stdin", "wy", rest[1:], opts)
	}
	return runPath(ctx, rest[0], rest[1:], opts)
}

// treeMode selects the tree-walking interpreter.
var treeMode bool

func runPath(ctx context.Context, path string, args []string, opts script.Options) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.IsDir() && treeMode {
		return runTreeDir(ctx, path, args)
	}
	if fi.IsDir() {
		bin, err := script.BuildDir(ctx, path, opts)
		if err != nil {
			return err
		}
		return execProgram(bin, filepath.Base(path), args)
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return runSource(ctx, src, path, path, args, opts)
}

func runSource(ctx context.Context, src []byte, name, argv0 string, args []string, opts script.Options) error {
	if treeMode {
		return runTree(ctx, src, name, args)
	}
	bin, err := script.Build(ctx, src, name, opts)
	if err != nil {
		return err
	}
	return execProgram(bin, argv0, args)
}

func buildCmd(ctx context.Context, args []string, opts script.Options) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	out := fs.String("o", "", "output file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: wy build [-o OUT] FILE")
	}
	file := fs.Arg(0)
	src, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	bin, err := script.Build(ctx, src, file, opts)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = strings.TrimSuffix(filepath.Base(file), ".go")
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	return os.WriteFile(*out, data, 0o755)
}
