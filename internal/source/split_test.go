package source

import (
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	src := `import "fmt"
type T struct{ a int }
func (t T) M() int { return t.a }
func f() {}
var v = 1
const c = 2
x := func() int { return 1 }()
func() { fmt.Println("lit") }()
func(x int) string { return "" }(1)
for i := 0; i < 3; i++ { fmt.Println(i) }
if a := 1; a > 0 { x++ } else { x-- }
a := 1; b := 2`
	units, err := Split(src)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, u := range units {
		kinds = append(kinds, u.Kind.String())
	}
	want := "import decl decl decl var decl stmt stmt stmt stmt stmt stmt stmt"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("kinds:\n got %s\nwant %s", got, want)
	}
	if u := units[len(units)-1]; u.Text != "b := 2" || u.Line != 12 || u.Col != 9 {
		t.Fatalf("last unit = %+v", u)
	}
}

func TestIncomplete(t *testing.T) {
	for src, want := range map[string]bool{
		"x := 1":                 false,
		"func f() {":             true,
		"s := `multi":            true,
		"x := 1 +":               true,
		"f(a,":                   true,
		"type T struct{ a int }": false,
		"for {\n}":               false,
		"x++":                    false,
		"m := map[string]int{\n": true,
	} {
		if got := Incomplete(src); got != want {
			t.Errorf("Incomplete(%q) = %v, want %v", src, got, want)
		}
	}
}

func TestIsExprList(t *testing.T) {
	for src, want := range map[string]bool{
		"a, b":              true,
		"f(a, b)":           false,
		"a, b = b, a":       false,
		"x":                 false,
		"len(s), cap(s)":    true,
		"return a, b":       false,
		"map[int]int{1: 2}": false,
	} {
		if got := IsExprList(src); got != want {
			t.Errorf("IsExprList(%q) = %v, want %v", src, got, want)
		}
	}
}

func TestGuessName(t *testing.T) {
	for path, want := range map[string]string{
		"fmt":                        "fmt",
		"math/rand/v2":               "rand",
		"gopkg.in/yaml.v3":           "yaml",
		"github.com/mattn/go-isatty": "isatty",
	} {
		if got := GuessName(path); got != want {
			t.Errorf("GuessName(%q) = %q, want %q", path, got, want)
		}
	}
}
