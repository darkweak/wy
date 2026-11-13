//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package tree

import (
	"reflect"
	"syscall"
)

func init() {
	// golang.org/x/sys/unix jumps to these in assembly.
	for name, fn := range map[string]any{
		"Syscall":     syscall.Syscall,
		"Syscall6":    syscall.Syscall6,
		"RawSyscall":  syscall.RawSyscall,
		"RawSyscall6": syscall.RawSyscall6,
	} {
		fv := reflect.ValueOf(fn)
		hostImpls["golang.org/x/sys/unix."+name] = func(args []reflect.Value, res []reflect.Type) []reflect.Value {
			out := fv.Call(args)
			return results(res, out[0].Interface(), out[1].Interface(), out[2].Interface())
		}
	}
	hostImpls["golang.org/x/sys/unix.SyscallNoError"] = func(args []reflect.Value, res []reflect.Type) []reflect.Value {
		r1, r2, _ := syscall.Syscall(uintptr(args[0].Uint()), uintptr(args[1].Uint()), uintptr(args[2].Uint()), uintptr(args[3].Uint()))
		return results(res, r1, r2)
	}
	hostImpls["golang.org/x/sys/unix.RawSyscallNoError"] = func(args []reflect.Value, res []reflect.Type) []reflect.Value {
		r1, r2, _ := syscall.RawSyscall(uintptr(args[0].Uint()), uintptr(args[1].Uint()), uintptr(args[2].Uint()), uintptr(args[3].Uint()))
		return results(res, r1, r2)
	}
	hostImpls["golang.org/x/sys/unix.gettimeofday"] = func(args []reflect.Value, res []reflect.Type) []reflect.Value {
		var tv syscall.Timeval
		err := syscall.Gettimeofday(&tv)
		if err == nil {
			p := args[0].Elem()
			p.FieldByName("Sec").SetInt(int64(tv.Sec))
			p.FieldByName("Usec").SetInt(int64(tv.Usec))
			return results(res)
		}
		return results(res, err)
	}
	// Unexported syscall functions pulled with //go:linkname.
	hostImpls["syscall.prlimit"] = func(args []reflect.Value, res []reflect.Type) []reflect.Value {
		_, _, e := syscall.RawSyscall6(syscall.SYS_PRLIMIT64, uintptr(args[0].Int()), uintptr(args[1].Int()),
			args[2].Pointer(), args[3].Pointer(), 0, 0)
		return results(res, errnoErr(e))
	}
	hostImpls["syscall.getsockopt"] = func(args []reflect.Value, res []reflect.Type) []reflect.Value {
		_, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, uintptr(args[0].Int()), uintptr(args[1].Int()),
			uintptr(args[2].Int()), args[3].Pointer(), args[4].Pointer(), 0)
		return results(res, errnoErr(e))
	}
	hostImpls["syscall.setsockopt"] = func(args []reflect.Value, res []reflect.Type) []reflect.Value {
		_, _, e := syscall.Syscall6(syscall.SYS_SETSOCKOPT, uintptr(args[0].Int()), uintptr(args[1].Int()),
			uintptr(args[2].Int()), args[3].Pointer(), uintptr(args[4].Uint()), 0)
		return results(res, errnoErr(e))
	}
}

func errnoErr(e syscall.Errno) error {
	if e == 0 {
		return nil
	}
	return e
}
