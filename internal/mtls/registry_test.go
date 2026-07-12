package mtls

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"spawnery/internal/pki"
)

func TestConnectionRegistryRevokesAllMatchingConnectionsOnlyOnce(t *testing.T) {
	registry := NewConnectionRegistry()
	issuer := big.NewInt(10)
	otherIssuer := big.NewInt(11)
	serial := big.NewInt(20)
	sibling := big.NewInt(21)
	var matchingA, matchingB, released, siblingCalls, otherIssuerCalls atomic.Int32

	registry.Register(PeerCertificate{IssuerSerial: issuer, LeafSerial: serial}, func() { matchingA.Add(1) })
	registry.Register(PeerCertificate{IssuerSerial: issuer, LeafSerial: serial}, func() { matchingB.Add(1) })
	release := registry.Register(PeerCertificate{IssuerSerial: issuer, LeafSerial: serial}, func() { released.Add(1) })
	registry.Register(PeerCertificate{IssuerSerial: issuer, LeafSerial: sibling}, func() { siblingCalls.Add(1) })
	registry.Register(PeerCertificate{IssuerSerial: otherIssuer, LeafSerial: serial}, func() { otherIssuerCalls.Add(1) })
	release()
	release()

	registry.Revoke(issuer, []*big.Int{serial})
	registry.Revoke(issuer, []*big.Int{serial})
	if matchingA.Load() != 1 || matchingB.Load() != 1 {
		t.Fatalf("matching callbacks = %d, %d", matchingA.Load(), matchingB.Load())
	}
	if released.Load() != 0 || siblingCalls.Load() != 0 || otherIssuerCalls.Load() != 0 {
		t.Fatalf("unrelated callbacks fired: released=%d sibling=%d other=%d", released.Load(), siblingCalls.Load(), otherIssuerCalls.Load())
	}
}

func TestConnectionRegistryMiddlewareCancelsRevokedLiveStream(t *testing.T) {
	registry := NewConnectionRegistry()
	issuer := big.NewInt(10)
	leaf := big.NewInt(20)
	started := make(chan struct{})
	done := make(chan struct{})
	handler := ConnectionRegistryMiddleware(registry, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(done)
	}))
	req := httptest.NewRequest(http.MethodPost, "https://internal/stream", nil)
	req = req.WithContext(context.WithValue(req.Context(), verifiedPeerContextKey{}, PeerCertificate{IssuerSerial: issuer, LeafSerial: leaf}))
	go handler.ServeHTTP(httptest.NewRecorder(), req)
	<-started
	registry.Revoke(issuer, []*big.Int{leaf})
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("revoked live request was not cancelled")
	}
}

func TestConnectionRegistryMiddlewareAllowsAnonymousButRejectsUnboundPrincipal(t *testing.T) {
	registry := NewConnectionRegistry()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := ConnectionRegistryMiddleware(registry, next)
	anonymous := httptest.NewRequest(http.MethodPost, "https://internal/enroll", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, anonymous)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("anonymous status = %d", recorder.Code)
	}
	authenticated := anonymous.WithContext(context.WithValue(anonymous.Context(), principalContextKey{}, pki.Principal{Kind: pki.KindNode, Role: pki.RoleSelfHosted}))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticated)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unbound authenticated status = %d", recorder.Code)
	}
}

func TestConnectionRegistryRejectsInvalidPeer(t *testing.T) {
	registry := NewConnectionRegistry()
	var calls atomic.Int32
	registry.Register(PeerCertificate{}, func() { calls.Add(1) })
	if calls.Load() != 1 {
		t.Fatal("invalid peer was retained instead of cancelled fail closed")
	}
}

func TestConnectionRegistryRejectsRegistrationAfterRevokeTombstone(t *testing.T) {
	registry := NewConnectionRegistry()
	issuer := big.NewInt(10)
	revoked := big.NewInt(20)
	sibling := big.NewInt(21)
	registry.Revoke(issuer, []*big.Int{revoked})

	revokedContext, cancelRevoked := context.WithCancel(t.Context())
	releaseRevoked := registry.Register(PeerCertificate{IssuerSerial: issuer, LeafSerial: revoked}, cancelRevoked)
	releaseRevoked()
	select {
	case <-revokedContext.Done():
	default:
		t.Fatal("registration after revoke was not cancelled by tombstone")
	}

	siblingContext, cancelSibling := context.WithCancel(t.Context())
	releaseSibling := registry.Register(PeerCertificate{IssuerSerial: issuer, LeafSerial: sibling}, cancelSibling)
	t.Cleanup(releaseSibling)
	select {
	case <-siblingContext.Done():
		t.Fatal("revoked tombstone cancelled sibling")
	default:
	}
}

func TestConnectionRegistryMiddlewareRevocationBetweenVerifyAndRegister(t *testing.T) {
	registry := NewConnectionRegistry()
	peer := PeerCertificate{IssuerSerial: big.NewInt(10), LeafSerial: big.NewInt(20)}
	registry.Revoke(peer.IssuerSerial, []*big.Int{peer.LeafSerial})
	handlerReached := make(chan error, 1)
	handler := ConnectionRegistryMiddleware(registry, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		handlerReached <- r.Context().Err()
	}))
	req := httptest.NewRequest(http.MethodPost, "https://internal/stream", nil)
	req = req.WithContext(context.WithValue(req.Context(), verifiedPeerContextKey{}, peer))
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if err := <-handlerReached; !errors.Is(err, context.Canceled) {
		t.Fatalf("handler context after verify/register gap = %v", err)
	}
}

func TestConnectionRegistryConcurrentRegisterReleaseAndRevoke(t *testing.T) {
	registry := NewConnectionRegistry()
	issuer := big.NewInt(100)
	serial := big.NewInt(200)
	const workers = 32
	const iterations = 200
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				ctx, cancel := context.WithCancel(t.Context())
				release := registry.Register(PeerCertificate{IssuerSerial: issuer, LeafSerial: serial}, cancel)
				select {
				case <-ctx.Done():
				default:
					release()
				}
			}
		}()
	}
	for range iterations {
		registry.Revoke(issuer, []*big.Int{serial})
	}
	wg.Wait()
	registry.Revoke(issuer, []*big.Int{serial})
}
