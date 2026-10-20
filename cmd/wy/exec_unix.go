//go:build unix

package main

import (
	"os"
	"syscall"
)

// execProgram replaces the wy process with the compiled program, so
// signals, exit codes and stdio behave exactly as if it ran directly.
func execProgram(bin, argv0 string, args []string) error {
	return syscall.Exec(bin, append([]string{argv0}, args...), os.Environ())
}
