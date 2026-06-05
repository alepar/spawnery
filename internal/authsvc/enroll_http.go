package authsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

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
		if errors.Is(err, ErrBadEnrollToken) {
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

// RunEnroll performs node-side enrollment against an AS: it generates a keypair + CSR locally (the key
// never leaves), redeems the token at asURL/enroll, and returns the issued cert/chain plus the key.
func RunEnroll(ctx context.Context, asURL, token, nodeID string) (*EnrollResult, error) {
	csrDER, key, err := pki.NewNodeCSR()
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
	resp, err := http.DefaultClient.Do(req)
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
