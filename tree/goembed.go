package tree

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// applyEmbeds initializes the string and []byte variables of f declared with
// a //go:embed directive from the files of dir they name, as the compiler
// does. embed.FS variables are not supported.
func applyEmbeds(dir string, f *ast.File) error {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			doc := vs.Doc
			if doc == nil && len(gd.Specs) == 1 {
				doc = gd.Doc
			}
			patterns := embedPatterns(doc)
			if len(patterns) == 0 {
				continue
			}
			if len(vs.Names) != 1 || len(vs.Values) != 0 || vs.Type == nil {
				return fmt.Errorf("%s: go:embed only applies to a single variable without initializer", vs.Names[0].Name)
			}
			if sel, ok := vs.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "FS" {
				return fmt.Errorf("%s: go:embed into embed.FS is not supported", vs.Names[0].Name)
			}
			if len(patterns) != 1 {
				return fmt.Errorf("%s: go:embed of a string or []byte takes a single file", vs.Names[0].Name)
			}
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(patterns[0])))
			if err != nil {
				return fmt.Errorf("%s: go:embed: %w", vs.Names[0].Name, err)
			}
			lit := &ast.BasicLit{ValuePos: vs.Names[0].Pos(), Kind: token.STRING, Value: strconv.Quote(string(data))}
			vs.Values = []ast.Expr{&ast.CallExpr{Fun: vs.Type, Lparen: vs.Names[0].Pos(), Args: []ast.Expr{lit}}}
		}
	}
	return nil
}

func embedPatterns(doc *ast.CommentGroup) []string {
	if doc == nil {
		return nil
	}
	var patterns []string
	for _, c := range doc.List {
		if rest, ok := strings.CutPrefix(c.Text, "//go:embed "); ok {
			patterns = append(patterns, strings.Fields(rest)...)
		}
	}
	return patterns
}
