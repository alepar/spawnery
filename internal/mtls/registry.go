package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net"
	"net/http"
	"sync"

	"spawnery/internal/pki"
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

func ConnectionRegistryMiddleware(registry *ConnectionRegistry, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, ok := VerifiedPeerFromContext(r.Context())
		if registry == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !ok {
			if _, authenticated := PrincipalFromContext(r.Context()); authenticated {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		release := registry.Register(peer, cancel)
		defer release()
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func SubscribeConnectionRegistry(state *pki.RevocationState, registry *ConnectionRegistry) func() {
	if state == nil || registry == nil {
		return func() {}
	}
	return state.SubscribeRevocations(func(update pki.RevocationUpdate) {
		registry.Revoke(update.IssuerSerial, update.NewlyRevoked)
	})
}

func DialTLSContext(config *tls.Config, registry *ConnectionRegistry) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if config == nil || registry == nil {
			return nil, errors.New("mtls: TLS connection registry is required")
		}
		connection, err := (&tls.Dialer{Config: config}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		tlsConnection, ok := connection.(*tls.Conn)
		if !ok {
			_ = connection.Close()
			return nil, errors.New("mtls: dial did not return a TLS connection")
		}
		state := tlsConnection.ConnectionState()
		leaf, issuer, err := verifiedLeafAndIssuer(state)
		if err != nil {
			_ = connection.Close()
			return nil, err
		}
		release := registry.Register(PeerCertificate{IssuerSerial: issuer.SerialNumber, LeafSerial: leaf.SerialNumber}, func() { _ = tlsConnection.Close() })
		return &registeredTLSConnection{Conn: tlsConnection, release: release}, nil
	}
}

func DialTLSContextHTTP2(config *tls.Config, registry *ConnectionRegistry) func(context.Context, string, string, *tls.Config) (net.Conn, error) {
	dial := DialTLSContext(config, registry)
	return func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
		return dial(ctx, network, address)
	}
}

func verifiedLeafAndIssuer(state tls.ConnectionState) (*x509.Certificate, *x509.Certificate, error) {
	if !state.HandshakeComplete || len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) < 3 {
		return nil, nil, errors.New("mtls: peer has no verified leaf and intermediate")
	}
	chain := state.VerifiedChains[0]
	return chain[0], chain[1], nil
}

type registeredTLSConnection struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *registeredTLSConnection) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}
