package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"time"

	"wy/internal/source"
	"wy/interp"
	"wy/tree"
)

const replHelp = `Enter Go code: declarations, statements or expressions, in any mix.
Variables, types, functions and imports persist between inputs; a trailing
expression (or "a, b" list) is printed. Standard-library imports are automatic.
Multi-line input continues while brackets are open.

Commands:
  :vars          list what the session defines
  :src [N]       show the Go code generated for input N (default: last)
  :load FILE     evaluate a file (its package clause is ignored)
  :time          toggle timing of each input
  :reset         start a fresh session
  :help          this help
  :quit          exit (or Ctrl-D)

Ctrl-C clears the current input, or abandons a running one (it keeps
running in the background: goroutines cannot be killed).
`

func repl(verbose, useTree bool) error {
	newEval := func() (evaluator, error) {
		if useTree {
			return &treeEval{tree.New(tree.Options{ModuleDir: "."})}, nil
		}
		opts := interp.Options{}
		if verbose {
			opts.Log = os.Stderr
		}
		s, err := interp.New(opts)
		if err != nil {
			return nil, err
		}
		return &compiledEval{s}, nil
	}
	s, err := newEval()
	if err != nil {
		return err
	}
	defer func() { s.close() }()

	mode := "compiled on the fly"
	if useTree {
		mode = "interpreted (tree-walking)"
	}
	fmt.Printf("wy - Go %s, %s. :help for help.\n", strings.TrimPrefix(runtime.Version(), "go"), mode)

	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)

	timing := false
	var buf strings.Builder
	prompt := func() {
		if buf.Len() == 0 {
			fmt.Print("wy> ")
		} else {
			fmt.Print(".... ")
		}
	}
	prompt()
	for {
		var line string
		var ok bool
		select {
		case line, ok = <-lines:
			if !ok {
				fmt.Println()
				return nil
			}
		case <-interrupts:
			buf.Reset()
			fmt.Println("^C")
			prompt()
			continue
		}

		if buf.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), ":") {
			quit, err := command(&s, newEval, strings.TrimSpace(line), &timing)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			if quit {
				return nil
			}
			prompt()
			continue
		}
		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(line)
		src := buf.String()
		if strings.TrimSpace(src) == "" {
			buf.Reset()
			prompt()
			continue
		}
		if source.Incomplete(src) {
			prompt()
			continue
		}
		buf.Reset()
		evalAndPrint(s, src, interrupts, timing)
		prompt()
	}
}

func evalAndPrint(s evaluator, src string, interrupts chan os.Signal, timing bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-interrupts:
			cancel()
		case <-ctx.Done():
		}
	}()
	start := time.Now()
	vals, err := s.Eval(ctx, src)
	elapsed := time.Since(start)
	for _, w := range s.notes(src) {
		fmt.Fprintln(os.Stderr, "note:", w)
	}
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(os.Stderr, "interrupted (the code keeps running in the background)")
	case err != nil:
		var pe *interp.PanicError
		var tpe *tree.PanicError
		switch {
		case errors.As(err, &pe):
			fmt.Fprintf(os.Stderr, "panic: %v\n%s", pe.Value, trimStack(pe.Stack))
		case errors.As(err, &tpe):
			fmt.Fprintf(os.Stderr, "panic: %v\n", tpe.Value)
		default:
			fmt.Fprintln(os.Stderr, err)
		}
	case len(vals) > 0:
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = format(v)
		}
		fmt.Println(strings.Join(parts, ", "))
	}
	if timing {
		fmt.Fprintf(os.Stderr, "(%v)\n", elapsed.Round(time.Millisecond))
	}
}

// format prints a value the way a Go programmer would write it when that is
// unambiguous (quoted strings), and with %v otherwise.
func format(v any) string {
	switch v := v.(type) {
	case string:
		return strconv.Quote(v)
	case rune:
		return fmt.Sprintf("%d (%q)", v, v)
	case error:
		return "error: " + v.Error()
	case nil:
		return "nil"
	}
	return fmt.Sprintf("%v", v)
}

// trimStack keeps the part of a panic stack that belongs to user code.
func trimStack(stack string) string {
	var out []string
	lines := strings.Split(stack, "\n")
	for i := 0; i+1 < len(lines); i++ {
		// Generated code carries //line directives, so frames of user code
		// are reported at "cellN:line".
		if strings.HasPrefix(strings.TrimSpace(lines[i+1]), "cell") {
			out = append(out, "  "+strings.TrimSpace(lines[i]), "    "+strings.TrimSpace(lines[i+1]))
			i++
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

func command(s *evaluator, newEval func() (evaluator, error), line string, timing *bool) (quit bool, err error) {
	fields := strings.Fields(line)
	switch fields[0] {
	case ":q", ":quit", ":exit":
		return true, nil
	case ":help", ":h", ":?":
		fmt.Print(replHelp)
	case ":time":
		*timing = !*timing
		fmt.Println("timing", map[bool]string{true: "on", false: "off"}[*timing])
	case ":vars", ":v":
		for _, l := range (*s).vars() {
			fmt.Println(l)
		}
	case ":src":
		n := 0
		if len(fields) > 1 {
			if n, err = strconv.Atoi(fields[1]); err != nil {
				return false, fmt.Errorf("no input %s", fields[1])
			}
		}
		src, err := (*s).source(n)
		if err != nil {
			return false, err
		}
		fmt.Print(src)
	case ":load":
		if len(fields) != 2 {
			return false, errors.New("usage: :load FILE")
		}
		data, err := os.ReadFile(fields[1])
		if err != nil {
			return false, err
		}
		evalAndPrint(*s, string(data), make(chan os.Signal), *timing)
	case ":reset":
		ns, err := newEval()
		if err != nil {
			return false, err
		}
		(*s).close()
		*s = ns
		fmt.Println("session reset")
	default:
		return false, fmt.Errorf("unknown command %s (:help for help)", fields[0])
	}
	return false, nil
}

// evaluator is the REPL's view of an engine.
type evaluator interface {
	Eval(ctx context.Context, src string) ([]any, error)
	notes(src string) []string
	vars() []string
	source(n int) (string, error)
	close()
}

type compiledEval struct{ s *interp.Session }

func (e *compiledEval) Eval(ctx context.Context, src string) ([]any, error) {
	return e.s.Eval(ctx, src)
}
func (e *compiledEval) close() { e.s.Close() }
func (e *compiledEval) notes(src string) []string {
	cells := e.s.Cells()
	if len(cells) == 0 || cells[len(cells)-1].Source != src {
		return nil
	}
	return cells[len(cells)-1].Warnings
}
func (e *compiledEval) vars() []string {
	var out []string
	for _, sym := range e.s.Symbols() {
		out = append(out, fmt.Sprintf("  %-5s %-16s %s", sym.Kind, sym.Name, sym.Type))
	}
	return out
}
func (e *compiledEval) source(n int) (string, error) {
	cells := e.s.Cells()
	if len(cells) == 0 {
		return "", errors.New("no inputs yet")
	}
	if n == 0 {
		n = len(cells)
	}
	if n < 1 || n > len(cells) {
		return "", fmt.Errorf("no input %d", n)
	}
	return cells[n-1].Generated, nil
}

type treeEval struct{ in *tree.Interp }

func (e *treeEval) Eval(ctx context.Context, src string) ([]any, error) { return e.in.Eval(ctx, src) }
func (e *treeEval) close()                                              {}
func (e *treeEval) notes(string) []string                               { return e.in.Notes() }
func (e *treeEval) vars() []string {
	var out []string
	for _, sym := range e.in.Symbols() {
		out = append(out, fmt.Sprintf("  %-5s %-16s %s", sym.Kind, sym.Name, sym.Type))
	}
	return out
}
func (e *treeEval) source(int) (string, error) {
	return "", errors.New("the tree-walking interpreter runs inputs directly; there is no generated code")
}
