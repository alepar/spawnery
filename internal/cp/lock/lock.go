// Package lock provides a per-key mutex so the CP can serialize all operations on one spawn id
// (the {claim -> node command -> await} critical section) without a global lock.
package lock

import "sync"

type Keyed struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func New() *Keyed { return &Keyed{m: map[string]*sync.Mutex{}} }

// Lock acquires the mutex for key and returns its unlock func. Note: per-key mutexes are not
// reclaimed (bounded by the number of distinct spawn ids — acceptable for the demo).
func (k *Keyed) Lock(key string) func() {
	k.mu.Lock()
	m, ok := k.m[key]
	if !ok {
		m = &sync.Mutex{}
		k.m[key] = m
	}
	k.mu.Unlock()
	m.Lock()
	return m.Unlock
}
