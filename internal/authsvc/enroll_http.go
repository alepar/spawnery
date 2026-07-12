package authsvc

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/net/http2"

	"spawnery/internal/mtls"
	"spawnery/internal/pki"
)

// enrollReq/enrollResp are the /enroll wire shapes (PEM as strings).
type enrollReq struct {
	Token  string `json:"token"`
	NodeID string `json:"node_id"`
	CSRPEM string `json:"csr_pem"`
}

type enrollResp struct {
	CertPEM  string `json:"cert_pem"`
	ChainPEM string `json:"chain_pem"`
}

// enrollHandler redeems a node's enrollment token + CSR and returns the issued cert + chain.
func (s *Service) enrollHandler(w http.ResponseWriter, r *http.Request) {
	var req enrollReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	csrDER, err := pki.ParseCSRPEM([]byte(req.CSRPEM))
	if err != nil {
		http.Error(w, "bad csr", http.StatusBadRequest)
		return
	}
	certPEM, chainPEM, err := s.Enroll(req.Token, csrDER, req.NodeID)
	if err != nil {
		// Token validity AND fingerprint-binding failures both collapse to 401 — the redeemer learns
		// only "rejected", never which check failed.
		if errors.Is(err, ErrBadEnrollToken) || errors.Is(err, ErrTokenFingerprintMismatch) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Error(w, "enrollment failed", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enrollResp{CertPEM: string(certPEM), ChainPEM: string(chainPEM)})
}

// EnrollResult is what a node keeps after enrolling: its issued leaf + chain (PEM) and the private key
// it generated locally (PEM).
type EnrollResult struct {
	CertPEM  []byte
	ChainPEM []byte
	KeyPEM   []byte
}

type EnrollTransport struct {
	Root        *x509.Certificate
	TrustDomain string
	ServerName  string
	IsRevoked   pki.CertificateRevocationChecker
}

// RunEnrollWithKey redeems an enrollment token using a PRE-EXISTING node key — the fingerprint-bound
// flow. The node generates and persists its key, announces pki.PublicKeyFingerprint(key.Public()) to the
// owner (who mints a token bound to it via IssueBoundEnrollmentToken over the pinned AS connection), then
// redeems here with that exact key so the AS's CSR-fingerprint check passes. The private key never
// leaves the node; only the CSR (proving possession) is sent.
func RunEnrollWithKey(ctx context.Context, asURL, token, nodeID string, key *ecdsa.PrivateKey, transport EnrollTransport) (*EnrollResult, error) {
	tlsConfig, err := mtls.ClientConfig(mtls.ClientOptions{
		Root: transport.Root, TrustDomain: transport.TrustDomain, ServerName: transport.ServerName,
		ExpectedServiceRole: pki.RoleAuthService, CurrentTime: time.Now, IsRevoked: transport.IsRevoked,
	})
	if err != nil {
		return nil, fmt.Errorf("enroll: authenticate auth service: %w", err)
	}
	client := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsConfig}}
	csrDER, err := pki.NodeCSRForKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM, err := pki.MarshalKeyPEM(key)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(enrollReq{Token: token, NodeID: nodeID, CSRPEM: string(pki.MarshalCSRPEM(csrDER))})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, asURL+"/enroll", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("enroll: status %d", resp.StatusCode)
	}
	var er enrollResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&er); err != nil {
		return nil, err
	}
	return &EnrollResult{CertPEM: []byte(er.CertPEM), ChainPEM: []byte(er.ChainPEM), KeyPEM: keyPEM}, nil
}
