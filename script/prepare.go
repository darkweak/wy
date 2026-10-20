// Package script runs Go programs and loose Go snippets on the fly. Code is
// compiled by the real Go toolchain, so every language feature, the whole
// standard library and any module dependency work exactly as in `go build`.
// Compiled binaries are cached by content, so repeated runs start instantly.
package script

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"

	"github.com/darkweak/wy/internal/source"
)

// Prepared is a snippet turned into a complete main package.
type Prepared struct {
	Code    []byte   // compilable Go source for package main
	Imports []string // every import path used
	NonStd  []string // import paths outside the standard library
}

// Prepare converts src into a complete Go program. src may be a full file
// (with a package clause) or a snippet: any mix of imports, declarations and
// statements. Statements are placed in main, declarations at package level,
// and missing standard-library imports are added. filename is used in
// positions so compiler errors point at the user's own lines.
func Prepare(src, filename string) (*Prepared, error) {
	return PrepareWith(src, filename, nil)
}

// PrepareWith is Prepare with an explicit package-name index for automatic
// imports (name -> import path). With a nil index the standard library is
// listed with the go command.
func PrepareWith(src, filename string, index map[string]string) (*Prepared, error) {
	src = source.StripShebang(src)
	units, err := source.Split(src)
	if err != nil {
		return nil, err
	}
	for _, u := range units {
		if u.Kind == source.Package {
			return prepareFile(src, filename, index)
		}
	}
	return prepareSnippet(units, filename, index)
}

func prepareFile(src, filename string, index map[string]string) (*Prepared, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	if f.Name.Name != "main" {
		return nil, fmt.Errorf("%s: package %s is not runnable (expected package main)", filename, f.Name.Name)
	}
	missing, err := missingImports(f, index)
	if err != nil {
		return nil, err
	}
	code := src
	if len(missing) > 0 {
		// Insert right after the package name, on the same line, so that
		// every original line keeps its number.
		off := fset.Position(f.Name.End()).Offset
		var b strings.Builder
		for _, p := range missing {
			b.WriteString("; import " + strconv.Quote(p))
		}
		code = src[:off] + b.String() + src[off:]
	}
	// Report the user's file name in panics, stack traces and runtime.Caller.
	code = "//line " + filename + ":1\n" + code
	return finish(code, f, missing, index), nil
}

func prepareSnippet(units []source.Unit, filename string, index map[string]string) (*Prepared, error) {
	var imports, decls, stmts []source.Unit
	hasMain := false
	for _, u := range units {
		switch u.Kind {
		case source.Import:
			imports = append(imports, u)
		case source.Decl, source.Var:
			decls = append(decls, u)
			if u.Name == "main" && strings.HasPrefix(u.Text, "func") {
				hasMain = true
			}
		default:
			stmts = append(stmts, u)
		}
	}
	if hasMain && len(stmts) > 0 {
		s := stmts[0]
		return nil, fmt.Errorf("%s:%d:%d: top-level statements cannot be combined with func main", filename, s.Line, s.Col)
	}
	compose := func(extra []string) string {
		var b strings.Builder
		b.WriteString("package main\n")
		for _, p := range extra {
			b.WriteString("import " + strconv.Quote(p) + "\n")
		}
		for _, u := range imports {
			b.WriteString(source.LineDirective(filename, u.Line, u.Col) + u.Text + "\n")
		}
		for _, u := range decls {
			b.WriteString(source.LineDirective(filename, u.Line, u.Col) + u.Text + "\n")
		}
		if !hasMain {
			b.WriteString("\nfunc main() {")
			for _, u := range stmts {
				b.WriteString(source.LineDirective(filename, u.Line, u.Col) + u.Text + "\n")
			}
			b.WriteString("}\n")
		}
		return b.String()
	}
	code := compose(nil)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, code, 0)
	if err != nil {
		return nil, err
	}
	missing, err := missingImports(f, index)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		code = compose(missing)
	}
	return finish(code, f, missing, index), nil
}

// missingImports resolves qualifiers that refer to no declaration or import
// in f against the standard library.
func missingImports(f *ast.File, index map[string]string) ([]string, error) {
	quals := source.UnresolvedQualifiers(f)
	if len(quals) == 0 {
		return nil, nil
	}
	imported := map[string]bool{}
	for _, is := range f.Imports {
		p, _ := strconv.Unquote(is.Path.Value)
		name := source.GuessName(p)
		if is.Name != nil {
			name = is.Name.Name
		}
		imported[name] = true
	}
	std := index
	if std == nil {
		var err error
		if std, _, err = source.Std(); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, q := range quals {
		if imported[q] {
			continue // imported under its real name; the compiler will judge
		}
		if p, ok := std[q]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func finish(code string, f *ast.File, extra []string, index map[string]string) *Prepared {
	set := map[string]bool{}
	for _, is := range f.Imports {
		p, _ := strconv.Unquote(is.Path.Value)
		set[p] = true
	}
	for _, p := range extra {
		set[p] = true
	}
	pr := &Prepared{Code: []byte(code)}
	for p := range set {
		pr.Imports = append(pr.Imports, p)
		std := false
		if index != nil {
			std = !strings.Contains(strings.SplitN(p, "/", 2)[0], ".")
		} else {
			std = source.IsStd(p)
		}
		if !std {
			pr.NonStd = append(pr.NonStd, p)
		}
	}
	sort.Strings(pr.Imports)
	sort.Strings(pr.NonStd)
	return pr
}
