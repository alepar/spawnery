package lock

import (
	"sync"
	"testing"
)

func TestKeyedSerializesSameKey(t *testing.T) {
	k := New()
	var n int
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := k.Lock("sp1")
			defer unlock()
			n++ // protected by the per-key lock; race detector would flag if unprotected
		}()
	}
	wg.Wait()
	if n != 100 {
		t.Fatalf("n=%d want 100", n)
	}
}

func TestKeyedDifferentKeysDontBlock(t *testing.T) {
	k := New()
	u1 := k.Lock("a")
	u2 := k.Lock("b") // must not deadlock on a different key
	u2()
	u1()
}
