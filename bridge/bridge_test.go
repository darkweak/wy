package bridge

import (
	"sync"
	"testing"
)

func TestPutTake(t *testing.T) {
	k1 := Put(42)
	k2 := Put("x")
	if k1 == k2 {
		t.Fatalf("keys collide: %q", k1)
	}
	if v := Take(k2); v != "x" {
		t.Fatalf("Take(%q) = %v, want x", k2, v)
	}
	if v := Take(k1); v != 42 {
		t.Fatalf("Take(%q) = %v, want 42", k1, v)
	}
	// A value can be taken only once.
	if v := Take(k1); v != nil {
		t.Fatalf("second Take(%q) = %v, want nil", k1, v)
	}
	if v := Take("missing"); v != nil {
		t.Fatalf("Take(missing) = %v, want nil", v)
	}
}

func TestPutTakeConcurrent(t *testing.T) {
	const n = 1000
	keys := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i] = Put(i)
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, k := range keys {
		if seen[k] {
			t.Fatalf("duplicate key %q", k)
		}
		seen[k] = true
		if v := Take(k); v != i {
			t.Fatalf("Take(%q) = %v, want %d", k, v, i)
		}
	}
}

func TestValues(t *testing.T) {
	if v := Values(); len(v) != 0 {
		t.Fatalf("Values() = %v", v)
	}
	v := Values(1, "a", nil)
	if len(v) != 3 || v[0] != 1 || v[1] != "a" || v[2] != nil {
		t.Fatalf("Values(1, a, nil) = %v", v)
	}
}
