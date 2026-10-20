// Package bridge is linked into both the host process and every compiled
// interpreter input. Because plugins share already-loaded packages with the
// host, this package's state is a single instance visible to both sides,
// which is how values travel between host code and interpreted code.
package bridge

import (
	"fmt"
	"sync"
)

var (
	mu    sync.Mutex
	slots = map[string]any{}
	seq   uint64
)

// Marker exists so the interpreter can discover this package's import path.
type Marker struct{}

// Put stores v and returns a fresh key for it.
func Put(v any) string {
	mu.Lock()
	defer mu.Unlock()
	seq++
	k := fmt.Sprintf("v%d", seq)
	slots[k] = v
	return k
}

// Take removes and returns the value stored under key.
func Take(key string) any {
	mu.Lock()
	defer mu.Unlock()
	v := slots[key]
	delete(slots, key)
	return v
}

// Values packs results (including all results of a multi-value call) for
// the interpreter's echo of a trailing expression.
func Values(v ...any) []any { return v }
