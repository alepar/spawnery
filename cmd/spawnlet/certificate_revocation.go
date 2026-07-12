package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"spawnery/internal/pki"
)

type nodeCertificateRevocations struct {
	state *pki.RevocationState
	crls  []string
}

func loadNodeCertificateRevocations(cfg *Spawnlet, now func() time.Time) (*nodeCertificateRevocations, error) {
	if cfg.Node.AuthMode != "enforced" {
		return nil, nil
	}
	issuerPaths := splitPathList(cfg.Node.CertificateRevocationIssuers)
	crlPaths := splitPathList(cfg.Node.CertificateRevocationCRLs)
	if cfg.Node.CertificateRevocationState == "" || len(issuerPaths) == 0 || len(issuerPaths) != len(crlPaths) {
		return nil, errors.New("node certificate revocation state, issuers, and one CRL per issuer are required")
	}
	issuers := make([]*x509.Certificate, 0, len(issuerPaths))
	for _, path := range issuerPaths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read certificate revocation issuer %s: %w", path, err)
		}
		issuer, err := pki.ParseCertPEM(raw)
		if err != nil {
			return nil, fmt.Errorf("parse certificate revocation issuer %s: %w", path, err)
		}
		issuers = append(issuers, issuer)
	}
	state, err := pki.OpenRevocationState(cfg.Node.CertificateRevocationState, issuers, now)
	if err != nil {
		return nil, fmt.Errorf("open certificate revocation state: %w", err)
	}
	runtime := &nodeCertificateRevocations{state: state, crls: crlPaths}
	if err := runtime.refresh(); err != nil {
		_ = state.Close()
		return nil, err
	}
	for _, issuer := range issuers {
		if _, ok := state.HighestNumber(issuer.SerialNumber); !ok {
			_ = state.Close()
			return nil, fmt.Errorf("no current CRL for issuer %s", issuer.Subject.String())
		}
	}
	return runtime, nil
}

func (r *nodeCertificateRevocations) refresh() error {
	for _, path := range r.crls {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read certificate CRL %s: %w", path, err)
		}
		if err := r.state.ApplyPEM(raw); err != nil {
			return fmt.Errorf("apply certificate CRL %s: %w", path, err)
		}
	}
	return nil
}

func (r *nodeCertificateRevocations) watch(ctx context.Context, interval time.Duration) {
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
			if err := r.refresh(); err != nil {
				log.Printf("node certificate revocation refresh failed: %v", err)
			}
		}
	}
}

func splitPathList(value string) []string {
	var paths []string
	for _, part := range strings.Split(value, ",") {
		if path := strings.TrimSpace(part); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}
