package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"spawnery/internal/mtls"
	"spawnery/internal/pki"
)

type nodeCertificateRevocations struct {
	state       *pki.RevocationState
	connections *mtls.ConnectionRegistry
	unsubscribe func()
	refresher   *mtls.CRLRefresher
}

func loadNodeCertificateRevocations(cfg *Spawnlet, now func() time.Time) (*nodeCertificateRevocations, error) {
	if cfg.Node.AuthMode != "enforced" {
		return nil, nil
	}
	issuerPaths := splitPathList(cfg.Node.CertificateRevocationIssuers)
	crlPaths := splitPathList(cfg.Node.CertificateRevocationCRLs)
	crlURLs := splitPathList(cfg.Node.CertificateRevocationURLs)
	if cfg.Node.CertificateRevocationState == "" || len(issuerPaths) == 0 {
		return nil, errors.New("node certificate revocation state and issuers are required")
	}
	rootPath := cfg.Node.RootCA
	if rootPath == "" {
		rootPath = filepath.Join(cfg.Node.IDDir, "root.pem")
	}
	rootRaw, err := os.ReadFile(rootPath)
	if err != nil {
		return nil, fmt.Errorf("read certificate revocation root %s: %w", rootPath, err)
	}
	root, err := pki.ParseCertPEM(rootRaw)
	if err != nil {
		return nil, fmt.Errorf("parse certificate revocation root %s: %w", rootPath, err)
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
		if err := issuer.CheckSignatureFrom(root); err != nil {
			return nil, fmt.Errorf("certificate revocation issuer %s is outside the pinned root: %w", path, err)
		}
		if len(issuer.URIs) != 1 || issuer.URIs[0].Host != cfg.Node.TrustDomain {
			return nil, fmt.Errorf("certificate revocation issuer %s has wrong trust domain", path)
		}
		issuers = append(issuers, issuer)
	}
	sources, err := mtls.BuildCRLSources(issuers, crlPaths, crlURLs)
	if err != nil {
		return nil, err
	}
	state, err := pki.OpenRevocationState(cfg.Node.CertificateRevocationState, issuers, now)
	if err != nil {
		return nil, fmt.Errorf("open certificate revocation state: %w", err)
	}
	connections := mtls.NewConnectionRegistry()
	refresher := mtls.NewCRLRefresher(http.DefaultClient, sources, state, cfg.Node.CertificateRevocationRefresh)
	runtime := &nodeCertificateRevocations{state: state, connections: connections, unsubscribe: mtls.SubscribeConnectionRegistry(state, connections), refresher: refresher}
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
	return r.refresher.Refresh(context.Background())
}

func (r *nodeCertificateRevocations) watch(ctx context.Context, interval time.Duration) {
	r.refresher.RunEvery(ctx, interval)
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
