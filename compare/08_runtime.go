//go:build ignore

package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

func where() string {
	_, file, line, _ := runtime.Caller(1)
	return fmt.Sprintf("%s:%d", filepath.Base(file), line)
}

func name() string {
	pc, _, _, _ := runtime.Caller(0)
	f := runtime.FuncForPC(pc).Name()
	return f[strings.LastIndex(f, ".")+1:]
}

func main() {
	fmt.Println(where(), name())
}
