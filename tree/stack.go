package tree

import (
	"reflect"
	"runtime"
	"sync"
	"unsafe"
)

// Interpreted call stacks for runtime.Caller and runtime.FuncForPC. A
// program counter for interpreted code is the address of a pcRecord; it
// cannot collide with real code addresses. FuncForPC returns a
// *runtime.Func that points at the record, and the methods of
// runtime.Func are intercepted for such pointers.

type pcRecord struct {
	name, file string
	line       int
}

var pcRecords sync.Map // uintptr -> *pcRecord

func (in *Interp) runtimeCaller(fr *frame, skip int) []reflect.Value {
	f := fr
	for i := 0; i < skip && f != nil; i++ {
		f = f.caller
	}
	if f == nil || f.fn == nil || !f.pos.IsValid() {
		return []reflect.Value{reflect.ValueOf(uintptr(0)), reflect.ValueOf(""), reflect.ValueOf(0), reflect.ValueOf(false)}
	}
	p := in.fset.Position(f.pos)
	rec := &pcRecord{name: f.fn.name, file: p.Filename, line: p.Line}
	pc := uintptr(unsafe.Pointer(rec))
	pcRecords.Store(pc, rec)
	return []reflect.Value{reflect.ValueOf(pc), reflect.ValueOf(p.Filename), reflect.ValueOf(p.Line), reflect.ValueOf(true)}
}

func funcForPC(pc uintptr) *runtime.Func {
	if r, ok := pcRecords.Load(pc); ok {
		return (*runtime.Func)(unsafe.Pointer(r.(*pcRecord)))
	}
	return runtime.FuncForPC(pc)
}

// funcMethod implements the methods of a *runtime.Func built by funcForPC.
func funcMethod(f *runtime.Func, name string, args []reflect.Value) ([]reflect.Value, bool) {
	r, ok := pcRecords.Load(uintptr(unsafe.Pointer(f)))
	if !ok {
		return nil, false
	}
	rec := r.(*pcRecord)
	switch name {
	case "Name":
		return []reflect.Value{reflect.ValueOf(rec.name)}, true
	case "Entry":
		return []reflect.Value{reflect.ValueOf(uintptr(unsafe.Pointer(rec)))}, true
	case "FileLine":
		return []reflect.Value{reflect.ValueOf(rec.file), reflect.ValueOf(rec.line)}, true
	}
	return nil, false
}

var runtimeFuncPtr = reflect.TypeFor[*runtime.Func]()
