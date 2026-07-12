package mtls

import (
	"context"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
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

func TestConnectionRegistryRejectsInvalidPeer(t *testing.T) {
	registry := NewConnectionRegistry()
	var calls atomic.Int32
	registry.Register(PeerCertificate{}, func() { calls.Add(1) })
	if calls.Load() != 1 {
		t.Fatal("invalid peer was retained instead of cancelled fail closed")
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
