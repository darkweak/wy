// Package typescope edits go/types scopes (removing names, snapshots), which
// incremental type checking of interactive inputs needs.
package typescope

import (
	"fmt"
	"go/types"
	"reflect"
	"unsafe"
)

// All cells are type-checked in one shared *types.Package so that go/types
// treats them as a single package in every respect (it compares packages
// by pointer in places, e.g. for unkeyed struct literals). Redefinitions and
// rollbacks of failed inputs need names removed from the package scope,
// which go/types offers no API for; these helpers reach the scope's element
// map directly. The map has been a field named "elems" since go/types
// appeared; scopeElems fails loudly if that ever changes.

func scopeElems(s *types.Scope) (reflect.Value, error) {
	f := reflect.ValueOf(s).Elem().FieldByName("elems")
	if !f.IsValid() || f.Kind() != reflect.Map {
		return reflect.Value{}, fmt.Errorf("interp: unsupported go/types version (types.Scope layout changed)")
	}
	m := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
	if m.IsNil() {
		m.Set(reflect.MakeMap(f.Type()))
	}
	return m, nil
}

// scopeDelete removes name from s.
func Delete(s *types.Scope, name string) error {
	m, err := scopeElems(s)
	if err != nil {
		return err
	}
	m.SetMapIndex(reflect.ValueOf(name), reflect.Value{})
	return nil
}

// scopeSnapshot captures the scope's contents; the returned function
// restores them.
func Snapshot(s *types.Scope) (restore func(), err error) {
	m, err := scopeElems(s)
	if err != nil {
		return nil, err
	}
	saved := reflect.MakeMapWithSize(m.Type(), m.Len())
	iter := m.MapRange()
	for iter.Next() {
		saved.SetMapIndex(iter.Key(), iter.Value())
	}
	return func() {
		m.Clear()
		iter := saved.MapRange()
		for iter.Next() {
			m.SetMapIndex(iter.Key(), iter.Value())
		}
	}, nil
}
