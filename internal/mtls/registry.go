package mtls

import (
	"context"
	"math/big"
	"sync"
)

type PeerCertificate struct {
	IssuerSerial *big.Int
	LeafSerial   *big.Int
}

type connectionKey struct {
	issuer string
	leaf   string
}

type ConnectionRegistry struct {
	mu          sync.Mutex
	nextID      uint64
	connections map[connectionKey]map[uint64]context.CancelFunc
}

func NewConnectionRegistry() *ConnectionRegistry {
	return &ConnectionRegistry{connections: make(map[connectionKey]map[uint64]context.CancelFunc)}
}

func (r *ConnectionRegistry) Register(peer PeerCertificate, cancel context.CancelFunc) func() {
	if cancel == nil {
		return func() {}
	}
	if r == nil || peer.IssuerSerial == nil || peer.LeafSerial == nil || peer.IssuerSerial.Sign() <= 0 || peer.LeafSerial.Sign() <= 0 {
		cancel()
		return func() {}
	}
	key := connectionKey{issuer: peer.IssuerSerial.Text(16), leaf: peer.LeafSerial.Text(16)}
	r.mu.Lock()
	if r.connections == nil {
		r.connections = make(map[connectionKey]map[uint64]context.CancelFunc)
	}
	r.nextID++
	id := r.nextID
	callbacks := r.connections[key]
	if callbacks == nil {
		callbacks = make(map[uint64]context.CancelFunc)
		r.connections[key] = callbacks
	}
	callbacks[id] = cancel
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		callbacks := r.connections[key]
		delete(callbacks, id)
		if len(callbacks) == 0 {
			delete(r.connections, key)
		}
		r.mu.Unlock()
	}
}

func (r *ConnectionRegistry) Revoke(issuerSerial *big.Int, serials []*big.Int) {
	if r == nil || issuerSerial == nil || issuerSerial.Sign() <= 0 {
		return
	}
	issuer := issuerSerial.Text(16)
	var callbacks []context.CancelFunc
	r.mu.Lock()
	for _, serial := range serials {
		if serial == nil || serial.Sign() <= 0 {
			continue
		}
		key := connectionKey{issuer: issuer, leaf: serial.Text(16)}
		for _, cancel := range r.connections[key] {
			callbacks = append(callbacks, cancel)
		}
		delete(r.connections, key)
	}
	r.mu.Unlock()
	for _, cancel := range callbacks {
		cancelSafely(cancel)
	}
}

func cancelSafely(cancel context.CancelFunc) {
	defer func() { _ = recover() }()
	cancel()
}
