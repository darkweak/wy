//go:build ignore

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

type NotFound struct{ Key string }

func (e *NotFound) Error() string        { return e.Key + " not found" }
func (e *NotFound) Is(target error) bool { return target == fs.ErrNotExist }

func find(k string) error { return fmt.Errorf("find: %w", &NotFound{k}) }

func main() {
	err := find("cfg")
	var nf *NotFound
	fmt.Println(errors.As(err, &nf), nf.Key, errors.Is(err, fs.ErrNotExist))
	_, err = os.Open("/definitely/missing")
	var pe *fs.PathError
	fmt.Println(errors.As(err, &pe), pe.Op)
	joined := errors.Join(err, find("x"))
	fmt.Println(errors.As(joined, &nf), nf.Key)
}
