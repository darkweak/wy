// Package stdlib registers the standard library with the tree-walking
// interpreter's native registry. Import it for its side effects.
package stdlib

//go:generate go run gen.go
//go:generate go run gen_families.go
