package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"time"

	"golang.org/x/net/http2"

	"spawnery/internal/mtls"
	"spawnery/internal/pki"
)

func failClosedCertificateRevocations(*big.Int, *big.Int) bool { return true }

func loadCertificateRevocations(cfg ASInternalTLS, now func() time.Time) (*pki.RevocationState, error) {
	issuerPaths := splitCSV(cfg.RevocationIssuers)
	crlPaths := splitCSV(cfg.RevocationCRLs)
	if cfg.RevocationState == "" || len(issuerPaths) == 0 || len(issuerPaths) != len(crlPaths) {
		return nil, errors.New("authsvc: certificate revocation state, issuers, and one CRL per issuer are required")
	}
	rootRaw, err := os.ReadFile(cfg.RootCA)
	if err != nil {
		return nil, fmt.Errorf("authsvc: read revocation root %s: %w", cfg.RootCA, err)
	}
	root, err := pki.ParseCertPEM(rootRaw)
	if err != nil {
		return nil, fmt.Errorf("authsvc: parse revocation root %s: %w", cfg.RootCA, err)
	}
	issuers := make([]*x509.Certificate, 0, len(issuerPaths))
	for _, path := range issuerPaths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("authsvc: read revocation issuer %s: %w", path, err)
		}
		issuer, err := pki.ParseCertPEM(raw)
		if err != nil {
			return nil, fmt.Errorf("authsvc: parse revocation issuer %s: %w", path, err)
		}
		if err := issuer.CheckSignatureFrom(root); err != nil {
			return nil, fmt.Errorf("authsvc: revocation issuer %s is outside the configured root: %w", path, err)
		}
		if len(issuer.URIs) != 1 || issuer.URIs[0].Host != cfg.TrustDomain {
			return nil, fmt.Errorf("authsvc: revocation issuer %s has wrong trust domain", path)
		}
		issuers = append(issuers, issuer)
	}
	state, err := pki.OpenRevocationState(cfg.RevocationState, issuers, now)
	if err != nil {
		return nil, fmt.Errorf("authsvc: open certificate revocation state: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = state.Close()
		}
	}()
	if err := applyCertificateCRLs(state, crlPaths); err != nil {
		return nil, err
	}
	for _, issuer := range issuers {
		if _, ok := state.HighestNumber(issuer.SerialNumber); !ok {
			return nil, fmt.Errorf("authsvc: no current CRL for issuer %s", issuer.Subject.String())
		}
	}
	closeOnError = false
	return state, nil
}

func applyCertificateCRLs(state *pki.RevocationState, paths []string) error {
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("authsvc: read certificate CRL %s: %w", path, err)
		}
		if err := state.ApplyPEM(raw); err != nil {
			return fmt.Errorf("authsvc: apply certificate CRL %s: %w", path, err)
		}
	}
	return nil
}

func refreshCertificateCRLs(ctx context.Context, state *pki.RevocationState, paths []string, interval time.Duration) {
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
			if err := applyCertificateCRLs(state, paths); err != nil {
				// IsRevoked remains fail-closed when the last accepted CRL becomes stale.
				log.Printf("authsvc: certificate revocation refresh failed: %v", err)
				continue
			}
		}
	}
}

func loadInternalTLSConfig(cfg ASInternalTLS, state *pki.RevocationState) (*tls.Config, *mtls.PeerVerifier, *x509.Certificate, tls.Certificate, error) {
	rootRaw, err := os.ReadFile(cfg.RootCA)
	if err != nil {
		return nil, nil, nil, tls.Certificate{}, fmt.Errorf("authsvc: read internal root: %w", err)
	}
	root, err := pki.ParseCertPEM(rootRaw)
	if err != nil {
		return nil, nil, nil, tls.Certificate{}, fmt.Errorf("authsvc: parse internal root: %w", err)
	}
	certRaw, err := os.ReadFile(cfg.Cert)
	if err != nil {
		return nil, nil, nil, tls.Certificate{}, fmt.Errorf("authsvc: read internal certificate: %w", err)
	}
	chainRaw, err := os.ReadFile(cfg.Chain)
	if err != nil {
		return nil, nil, nil, tls.Certificate{}, fmt.Errorf("authsvc: read internal chain: %w", err)
	}
	keyRaw, err := os.ReadFile(cfg.Key)
	if err != nil {
		return nil, nil, nil, tls.Certificate{}, fmt.Errorf("authsvc: read internal key: %w", err)
	}
	identity, err := tls.X509KeyPair(append(certRaw, chainRaw...), keyRaw)
	if err != nil {
		return nil, nil, nil, tls.Certificate{}, fmt.Errorf("authsvc: load internal identity: %w", err)
	}
	if err := validateInternalServerName(identity, cfg.ServerName); err != nil {
		return nil, nil, nil, tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(identity.Certificate[0])
	if err != nil {
		return nil, nil, nil, tls.Certificate{}, fmt.Errorf("authsvc: parse internal identity leaf: %w", err)
	}
	intermediates := make([]*x509.Certificate, 0, len(identity.Certificate)-1)
	for _, raw := range identity.Certificate[1:] {
		cert, parseErr := x509.ParseCertificate(raw)
		if parseErr != nil {
			return nil, nil, nil, tls.Certificate{}, fmt.Errorf("authsvc: parse internal identity chain: %w", parseErr)
		}
		intermediates = append(intermediates, cert)
	}
	principal, err := pki.VerifyPrincipal(leaf, intermediates, pki.VerifyOptions{
		Root: root, TrustDomain: cfg.TrustDomain, CurrentTime: time.Now(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsRevoked: state.IsRevoked,
	})
	if err != nil {
		return nil, nil, nil, tls.Certificate{}, fmt.Errorf("authsvc: verify internal identity: %w", err)
	}
	if principal.Kind != pki.KindService || principal.Role != pki.RoleAuthService {
		return nil, nil, nil, tls.Certificate{}, fmt.Errorf("authsvc: internal identity is %s/%s, want service/authsvc", principal.Kind, principal.Role)
	}
	verifier, err := mtls.NewPeerVerifier(mtls.PeerVerifierOptions{
		Root: root, TrustDomain: cfg.TrustDomain, CurrentTime: time.Now, IsRevoked: state.IsRevoked,
	})
	if err != nil {
		return nil, nil, nil, tls.Certificate{}, err
	}
	tlsConfig, err := mtls.ServerConfig(mtls.ServerOptions{
		Verifier: verifier, Identity: identity, ClientMode: mtls.VerifyClientCertificateIfGiven,
	})
	if err != nil {
		return nil, nil, nil, tls.Certificate{}, err
	}
	return tlsConfig, verifier, root, identity, nil
}

func validateInternalServerName(identity tls.Certificate, serverName string) error {
	if serverName == "" {
		return errors.New("authsvc: internal server name is required")
	}
	if len(identity.Certificate) == 0 {
		return errors.New("authsvc: internal identity leaf is required")
	}
	leaf, err := x509.ParseCertificate(identity.Certificate[0])
	if err != nil {
		return fmt.Errorf("authsvc: parse internal identity leaf: %w", err)
	}
	if err := leaf.VerifyHostname(serverName); err != nil {
		return fmt.Errorf("authsvc: internal identity does not serve %q: %w", serverName, err)
	}
	return nil
}

func newInternalClient(root *x509.Certificate, identity tls.Certificate, trustDomain, serverName, expectedRole string, state *pki.RevocationState) (*http.Client, error) {
	tlsConfig, err := mtls.ClientConfig(mtls.ClientOptions{
		Root: root, TrustDomain: trustDomain, Identity: identity, ServerName: serverName,
		ExpectedServiceRole: expectedRole, CurrentTime: time.Now, IsRevoked: state.IsRevoked,
	})
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsConfig}}, nil
}

func newInternalHTTPServer(addr string, handler http.Handler, tlsConfig *tls.Config) (*http.Server, error) {
	if tlsConfig == nil {
		return nil, errors.New("authsvc: internal TLS configuration is required")
	}
	server := &http.Server{Addr: addr, Handler: handler, TLSConfig: tlsConfig}
	if err := http2.ConfigureServer(server, &http2.Server{}); err != nil {
		return nil, fmt.Errorf("authsvc: configure internal HTTP/2: %w", err)
	}
	return server, nil
}
