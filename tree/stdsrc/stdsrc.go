// Package stdsrc embeds the source of standard packages that consist of
// generic code, which the tree-walking interpreter runs from source
// (generic functions cannot be called through reflection).
package stdsrc

import (
	"embed"
	"io/fs"
	"path"
	"strings"
)

//go:embed cmp slices maps iter overlay
var files embed.FS

// Overlay returns source files adding generic declarations to a compiled
// package (whose generic functions cannot be called by reflection), or nil.
func Overlay(importPath string) map[string][]byte {
	return Package("overlay/" + importPath)
}

// Package returns the source files of an embedded package, or nil.
func Package(importPath string) map[string][]byte {
	entries, err := fs.ReadDir(files, importPath)
	if err != nil {
		return nil
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".src") {
			continue
		}
		data, err := files.ReadFile(path.Join(importPath, e.Name()))
		if err == nil {
			out[strings.TrimSuffix(e.Name(), ".src")+".go"] = data
		}
	}
	return out
}
