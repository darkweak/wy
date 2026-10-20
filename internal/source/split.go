// Package source turns loose Go snippets into compilable Go files: it splits
// input into top-level units, classifies them, and finds missing imports.
package source

import (
	"fmt"
	"go/ast"
	"go/scanner"
	"go/token"
	"sort"
	"strings"
)

// Kind classifies a top-level unit of a snippet.
type Kind int

const (
	Stmt    Kind = iota // an ordinary statement
	Import              // import declaration
	Decl                // type, const, func or method declaration
	Var                 // var declaration
	Package             // package clause: the input is a complete file
)

func (k Kind) String() string {
	return [...]string{"stmt", "import", "decl", "var", "package"}[k]
}

// Unit is one top-level piece of a snippet.
type Unit struct {
	Kind      Kind
	Text      string
	Line, Col int // 1-based position of Text in the original input
	Name      string
	File      string // file name for positions; set for code replayed from earlier inputs
}

// StripShebang blanks a leading "#!" line while keeping line numbers intact.
func StripShebang(src string) string {
	if strings.HasPrefix(src, "#!") {
		if i := strings.IndexByte(src, '\n'); i >= 0 {
			return "//" + src[2:]
		}
		return ""
	}
	return src
}

type tok struct {
	t   token.Token
	lit string
	off int
}

// Split breaks src into top-level units. It never fails on syntax that the
// parser will later reject; it only fails on lexical errors.
func Split(src string) ([]Unit, error) {
	fset := token.NewFileSet()
	file := fset.AddFile("input", -1, len(src))
	var errs scanner.ErrorList
	var s scanner.Scanner
	s.Init(file, []byte(src), func(pos token.Position, msg string) { errs.Add(pos, msg) }, 0)

	var units []Unit
	var cur []tok
	depth := 0
	// Semicolons inside "for a; b; c {", "if x := f(); x {" and
	// "switch v := g(); v {" headers do not end a unit. headers holds the
	// depths at which such a header is open.
	var headers []int
	flush := func(end int) {
		if len(cur) == 0 {
			return
		}
		start := cur[0].off
		text := strings.TrimRight(src[start:end], " \t\r\n;")
		p := file.Position(file.Pos(start))
		units = append(units, Unit{Kind: classify(cur), Text: text, Line: p.Line, Col: p.Column, Name: unitName(cur)})
		cur = nil
	}
	for {
		pos, t, lit := s.Scan()
		if t == token.EOF {
			flush(len(src))
			break
		}
		off := file.Offset(pos)
		switch t {
		case token.FOR, token.IF, token.SWITCH:
			headers = append(headers, depth)
		case token.LBRACE:
			if n := len(headers); n > 0 && headers[n-1] == depth {
				headers = headers[:n-1]
			}
			depth++
		case token.LPAREN, token.LBRACK:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth--
		case token.SEMICOLON:
			if n := len(headers); n > 0 && headers[n-1] == depth && lit == ";" {
				break
			}
			if depth == 0 {
				end := off
				if lit == ";" {
					end = off + 1
				}
				if end > len(src) {
					end = len(src)
				}
				flush(end)
				continue
			}
		}
		cur = append(cur, tok{t, lit, off})
	}
	if len(errs) > 0 {
		errs.Sort()
		return nil, errs.Err()
	}
	return units, nil
}

func classify(ts []tok) Kind {
	switch ts[0].t {
	case token.PACKAGE:
		return Package
	case token.IMPORT:
		return Import
	case token.TYPE, token.CONST:
		return Decl
	case token.VAR:
		return Var
	case token.FUNC:
		if len(ts) > 1 && ts[1].t == token.IDENT {
			return Decl
		}
		if len(ts) > 1 && ts[1].t == token.LPAREN {
			// func (recv T) Name(...) is a method; func(...) {...}(...) is a statement.
			d := 0
			for i := 1; i < len(ts); i++ {
				switch ts[i].t {
				case token.LPAREN:
					d++
				case token.RPAREN:
					d--
				}
				if d == 0 {
					// A method has a name followed by its parameters;
					// a literal's result type is followed by { or more type.
					if i+2 < len(ts) && ts[i+1].t == token.IDENT && ts[i+2].t == token.LPAREN {
						return Decl
					}
					return Stmt
				}
			}
		}
	}
	return Stmt
}

func unitName(ts []tok) string {
	if len(ts) > 1 && ts[1].t == token.IDENT {
		return ts[1].lit
	}
	return ""
}

// Incomplete reports whether src looks like the beginning of a longer input
// (unbalanced brackets, an unterminated raw string, a trailing operator).
// The REPL uses it to decide whether to read a continuation line.
func Incomplete(src string) bool {
	fset := token.NewFileSet()
	file := fset.AddFile("input", -1, len(src))
	unterminated := false
	var s scanner.Scanner
	s.Init(file, []byte(src), func(_ token.Position, msg string) {
		if strings.Contains(msg, "not terminated") {
			unterminated = true
		}
	}, 0)
	depth := 0
	last := token.ILLEGAL
	for {
		_, t, lit := s.Scan()
		if t == token.EOF {
			break
		}
		switch t {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth--
		}
		if t == token.SEMICOLON && lit == "\n" {
			continue
		}
		last = t
	}
	if unterminated || depth > 0 {
		return true
	}
	switch {
	case last == token.COMMA, last == token.PERIOD, last == token.COLON && depth > 0:
		return true
	case last.IsOperator() && last != token.RPAREN && last != token.RBRACK && last != token.RBRACE &&
		last != token.INC && last != token.DEC && last != token.SEMICOLON && last != token.COLON:
		return true
	}
	return false
}

// UnresolvedQualifiers returns the names X used as X.Sel in f that are not
// declared anywhere in the file: candidates for automatic imports. The file
// must have been parsed with object resolution enabled (the default).
func UnresolvedQualifiers(f *ast.File) []string {
	unresolved := map[*ast.Ident]bool{}
	for _, id := range f.Unresolved {
		unresolved[id] = true
	}
	seen := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && unresolved[id] {
				seen[id.Name] = true
			}
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// LineDirective returns a //line comment placing the following text at
// file:line:col. It must start at column 1, hence the leading newline.
func LineDirective(file string, line, col int) string {
	return fmt.Sprintf("\n//line %s:%d:%d\n", file, line, col)
}

// GuessName guesses the package name for an import path the way wymports
// does: the last element, skipping major-version suffixes like /v2 and
// trimming ".v3"-style suffixes and a "go-" prefix.
func GuessName(path string) string {
	elems := strings.Split(path, "/")
	name := elems[len(elems)-1]
	if len(elems) > 1 && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		name = elems[len(elems)-2]
	}
	if i := strings.IndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	name = strings.TrimPrefix(name, "go-")
	return strings.ReplaceAll(name, "-", "_")
}

// IsExprList reports whether text is a bare list of two or more expressions
// ("a, b"), which a REPL echoes as multiple values.
func IsExprList(text string) bool {
	fset := token.NewFileSet()
	file := fset.AddFile("input", -1, len(text))
	var s scanner.Scanner
	s.Init(file, []byte(text), nil, 0)
	depth, commas, first := 0, 0, true
	for {
		_, t, lit := s.Scan()
		if t == token.EOF || (t == token.SEMICOLON && lit == "\n") {
			break
		}
		if first && t.IsKeyword() && t != token.FUNC && t != token.STRUCT && t != token.MAP && t != token.CHAN && t != token.INTERFACE {
			return false
		}
		first = false
		switch t {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth--
		case token.COMMA:
			if depth == 0 {
				commas++
			}
		case token.ASSIGN, token.DEFINE, token.ADD_ASSIGN, token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN,
			token.REM_ASSIGN, token.AND_ASSIGN, token.OR_ASSIGN, token.XOR_ASSIGN, token.SHL_ASSIGN, token.SHR_ASSIGN,
			token.AND_NOT_ASSIGN, token.COLON, token.SEMICOLON:
			if depth == 0 {
				return false
			}
		}
	}
	return commas > 0
}
