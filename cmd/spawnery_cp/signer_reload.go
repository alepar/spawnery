package main

import (
	"context"
	"log"
	"time"

	"spawnery/internal/authsvc/token"
)

const defaultSignerRevocationReloadInterval = 5 * time.Second
const signerRevocationShutdownBound = 2 * time.Second

type signerRevocationReloader struct {
	interval time.Duration
	now      func() time.Time
	onError  func(error)
	load     func(context.Context) error
}

func newSignerRevocationReloader(store *token.SignerRevocationStore, path string) *signerRevocationReloader {
	r := &signerRevocationReloader{
		interval: defaultSignerRevocationReloadInterval, now: time.Now,
		onError: func(err error) { log.Printf("cp: signer revocation statement reload failed: %v", err) },
	}
	r.load = func(ctx context.Context) error { return store.LoadAndApplyContext(ctx, path, r.now()) }
	return r
}

func (r *signerRevocationReloader) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.load(ctx); err != nil && ctx.Err() == nil {
				r.onError(err)
			}
		}
	}
}

func stopSignerRevocationReloader(cancel context.CancelFunc, done <-chan struct{}, bound time.Duration) bool {
	cancel()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
