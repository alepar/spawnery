package authsvc

import (
	"math/big"
	"sync"
	"testing"
	"time"

	"spawnery/internal/authsvc/store"
	"spawnery/internal/pki"
)

func TestNodeCRLConcurrentPublishersCannotDowngradeSink(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, "prod.spawnery.internal")
	st := store.NewTestStore(t)
	var mu sync.Mutex
	var published []*big.Int
	svc := New(root.Cert, issuer, WithClock(func() time.Time { return now }), WithNodeRevocationStore(st, func(pem []byte) error {
		list, err := pki.ParseCRLPEM(pem)
		if err != nil {
			return err
		}
		mu.Lock()
		published = append(published, new(big.Int).Set(list.Number))
		mu.Unlock()
		return nil
	}))
	first, _ := svc.IssueSelfHostedNode("node-a", "acct", now.Add(time.Hour))
	second, _ := svc.IssueSelfHostedNode("node-b", "acct", now.Add(time.Hour))
	firstCommitted := make(chan struct{})
	releaseFirst := make(chan struct{})
	svc.nodeCRLCommitted = func(number *big.Int) {
		if number.Cmp(big.NewInt(1)) == 0 {
			close(firstCommitted)
			<-releaseFirst
		}
	}
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- svc.RevokeNodeCertificate(t.Context(), "node-a", issuer.Cert.SerialNumber, first.Cert.SerialNumber, "lost")
	}()
	<-firstCommitted
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- svc.RevokeNodeCertificate(t.Context(), "node-b", issuer.Cert.SerialNumber, second.Cert.SerialNumber, "lost")
	}()
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(published) != 2 || published[0].Cmp(big.NewInt(2)) != 0 || published[1].Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("publication order = %v, want [2 2]", published)
	}
}
