//go:build ignore

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"text/template"
	"time"
)

type Item struct {
	Name  string
	Price float64
	Tags  []string
}

func (i Item) Label() string { return strings.ToUpper(i.Name) }

type upper struct{ r io.Reader }

func (u upper) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	copy(p, bytes.ToUpper(p[:n]))
	return n, err
}

type greeter struct{ greeting string }

func (g *greeter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Greeter", "yes")
	fmt.Fprintf(w, "%s, %s!", g.greeting, r.URL.Query().Get("name"))
}

var ErrEmpty = errors.New("empty input")

type wrapErr struct {
	op  string
	err error
}

func (w *wrapErr) Error() string { return w.op + ": " + w.err.Error() }
func (w *wrapErr) Unwrap() error { return w.err }

func parse(s string) error {
	if s == "" {
		return &wrapErr{"parse", ErrEmpty}
	}
	return nil
}

func main() {
	items := []Item{{"tea", 3.5, []string{"hot"}}, {"cake", 4, nil}, {"water", 1, []string{"cold", "free"}}}
	sort.Slice(items, func(i, j int) bool { return items[i].Price < items[j].Price })
	t2 := template.Must(template.New("t").Funcs(template.FuncMap{"join": strings.Join}).Parse(`{{range .}}{{.Name}}({{printf "%.2f" .Price}}){{if .Tags}} {{join .Tags "+"}}{{end}};{{end}}`))
	if err := t2.Execute(os.Stdout, items); err != nil {
		fmt.Println("template error:", err)
	}
	fmt.Println()

	sc := bufio.NewScanner(upper{strings.NewReader("one two\nthree")})
	sc.Split(bufio.ScanWords)
	var words []string
	for sc.Scan() {
		words = append(words, sc.Text())
	}
	fmt.Println(words)
	data, _ := io.ReadAll(io.LimitReader(upper{strings.NewReader("abcdef")}, 4))
	fmt.Println(string(data))

	srv := httptest.NewServer(&greeter{"Hello"})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "?name=gopher")
	if err != nil {
		fmt.Println("http error:", err)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println(resp.StatusCode, resp.Header.Get("X-Greeter"), string(body))

	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("/items/{name}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "item ", r.PathValue("name"))
	})
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/items/tea", nil))
	fmt.Println(rec.Body.String())

	err = parse("")
	var we *wrapErr
	fmt.Println(err, errors.Is(err, ErrEmpty), errors.As(err, &we), we.op, errors.Unwrap(err) == ErrEmpty)
	joined := errors.Join(parse(""), errors.New("other"))
	fmt.Println(errors.Is(joined, ErrEmpty), strings.Count(joined.Error(), "\n"))
	wrapped := fmt.Errorf("ctx: %w", err)
	fmt.Println(wrapped, errors.Is(wrapped, ErrEmpty))

	d := 90 * time.Minute
	tm := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	fmt.Println(d, tm.Add(d).Format(time.Kitchen), tm.Weekday(), d.Hours())
	var sb strings.Builder
	w := bufio.NewWriter(&sb)
	fmt.Fprintf(w, "%05.1f|%-4s|%x", 3.14159, "ab", []byte("hi"))
	w.Flush()
	fmt.Println(sb.String())
}
