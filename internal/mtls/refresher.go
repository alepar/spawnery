package mtls

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"spawnery/internal/pki"
)

const MaxCRLResponseSize = 6 << 20

var (
	ErrCRLFetch            = errors.New("mtls: fetch CRL")
	ErrCRLStatus           = errors.New("mtls: unexpected CRL response status")
	ErrCRLResponseTooLarge = errors.New("mtls: CRL response exceeds size limit")
)

type CRLSource struct {
	Issuer *x509.Certificate
	URL    string
}

type CRLRefresher struct {
	client   *http.Client
	sources  []CRLSource
	state    *pki.RevocationState
	interval time.Duration
}

func NewCRLRefresher(client *http.Client, sources []CRLSource, state *pki.RevocationState, interval time.Duration) *CRLRefresher {
	if client == nil {
		client = http.DefaultClient
	}
	return &CRLRefresher{
		client:   client,
		sources:  append([]CRLSource(nil), sources...),
		state:    state,
		interval: interval,
	}
}

func (r *CRLRefresher) Refresh(ctx context.Context) error {
	if r == nil || r.state == nil || r.client == nil || len(r.sources) == 0 {
		return errors.New("mtls: invalid CRL refresher configuration")
	}
	var failures []error
	for _, source := range r.sources {
		if source.Issuer == nil || source.URL == "" {
			failures = append(failures, errors.New("mtls: invalid CRL source"))
			continue
		}
		if err := r.refreshSource(ctx, source); err != nil {
			failures = append(failures, fmt.Errorf("issuer %s from %s: %w", source.Issuer.SerialNumber.Text(16), source.URL, err))
		}
	}
	return errors.Join(failures...)
}

func (r *CRLRefresher) refreshSource(ctx context.Context, source CRLSource) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCRLFetch, err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCRLFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s", ErrCRLStatus, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxCRLResponseSize+1))
	if err != nil {
		return fmt.Errorf("%w: read response: %v", ErrCRLFetch, err)
	}
	if len(body) > MaxCRLResponseSize {
		return ErrCRLResponseTooLarge
	}
	if err := r.state.ApplyPEM(body); err != nil {
		return fmt.Errorf("apply CRL: %w", err)
	}
	return nil
}

func (r *CRLRefresher) Run(ctx context.Context) {
	if r == nil {
		return
	}
	interval := r.interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Refresh(ctx); err != nil {
				log.Printf("mtls: CRL refresh failed: %v", err)
			}
		}
	}
}
