package main

import (
	"context"
	"log"
	"time"

	"spawnery/internal/authsvc/token"
)

const defaultSignerRevocationReloadInterval = 5 * time.Second

type signerRevocationReloader struct {
	store    *token.SignerRevocationStore
	path     string
	interval time.Duration
	now      func() time.Time
	onError  func(error)
}

func newSignerRevocationReloader(store *token.SignerRevocationStore, path string) *signerRevocationReloader {
	return &signerRevocationReloader{
		store: store, path: path, interval: defaultSignerRevocationReloadInterval, now: time.Now,
		onError: func(err error) { log.Printf("cp: signer revocation statement reload failed: %v", err) },
	}
}

func (r *signerRevocationReloader) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.store.LoadAndApply(r.path, r.now()); err != nil {
				r.onError(err)
			}
		}
	}
}
