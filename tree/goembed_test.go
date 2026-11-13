package tree

import (
	"context"
	"strings"
	"testing"
)

func TestGoEmbed(t *testing.T) {
	gopath := t.TempDir()
	writeFiles(t, gopath, map[string]string{
		"src/example.com/assets/assets.go": `package assets

import _ "embed"

//go:embed data/blob.bin
var Blob []byte

var (
	//go:embed data/name.txt
	Name string
	Other = "plain"
)
`,
		"src/example.com/assets/data/blob.bin": "\x00\x01\xff",
		"src/example.com/assets/data/name.txt": "souin\n",
	})
	in := New(Options{GoPath: gopath})
	vals, err := in.Eval(context.Background(), `import "example.com/assets"
string(assets.Blob) + "|" + assets.Name + "|" + assets.Other`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "\x00\x01\xff|souin\n|plain"; vals[0] != want {
		t.Fatalf("got %q, want %q", vals[0], want)
	}
}

func TestGoEmbedFSUnsupported(t *testing.T) {
	gopath := t.TempDir()
	writeFiles(t, gopath, map[string]string{
		"src/example.com/fs/fs.go":      "package fs\n\nimport \"embed\"\n\n//go:embed data\nvar Data embed.FS\n",
		"src/example.com/fs/data/a.txt": "a",
	})
	in := New(Options{GoPath: gopath})
	_, err := in.Eval(context.Background(), `import "example.com/fs"`)
	if err == nil || !strings.Contains(err.Error(), "embed.FS is not supported") {
		t.Fatalf("err = %v, want embed.FS is not supported", err)
	}
}

// A forbidden import deep in a diamond-shaped graph must be reported once per
// package: re-checking it from every importer is exponential in the depth, and
// nesting every error of every dependency makes the message grow as fast.
func TestFailedImportCached(t *testing.T) {
	const depth = 24
	files := map[string]string{
		"src/example.com/g/leaf/leaf.go": "package leaf\n\nimport \"unsafe\"\n\nvar A, B = unsafe.Sizeof(0), unsafe.Sizeof(1)\n",
	}
	below := []string{"example.com/g/leaf"}
	for i := depth; i > 0; i-- {
		var level []string
		for _, side := range []string{"a", "b"} {
			name := "p" + strings.Repeat("x", i) + side
			src := "package " + name + "\n\nimport (\n"
			for j, imp := range below {
				src += "\td" + string(rune('a'+j)) + " \"" + imp + "\"\n"
			}
			src += ")\n\nvar _, _ = da.A, da.B\n"
			if len(below) > 1 {
				src += "var _, _ = db.A, db.B\n"
			}
			files["src/example.com/g/"+name+"/"+name+".go"] = src
			level = append(level, "example.com/g/"+name)
		}
		below = level
	}
	gopath := t.TempDir()
	writeFiles(t, gopath, files)
	in := New(Options{GoPath: gopath, Forbid: []string{"unsafe"}})
	_, err := in.Eval(context.Background(), `import "`+below[0]+`"`)
	if err == nil || !strings.Contains(err.Error(), "is not allowed") {
		t.Fatalf("err = %v, want a forbidden import", err)
	}
	if n := len(err.Error()); n > 64<<10 {
		t.Fatalf("error is %d bytes long", n)
	}
	if _, ok := in.failed["example.com/g/leaf"]; !ok {
		t.Fatal("failed leaf import is not cached")
	}
}
