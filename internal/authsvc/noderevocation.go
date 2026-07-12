package authsvc

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"spawnery/internal/authsvc/store"
	"spawnery/internal/pki"
)

const nodeCRLValidity = 24 * time.Hour

func (s *Service) RevokeNodeCertificate(ctx context.Context, nodeID string, issuerSerial, leafSerial *big.Int, reason string) error {
	if s == nil || s.intermediate == nil || s.intermediate.Cert == nil || s.nodeRevocationStore == nil || s.nodeCRLSink == nil {
		return errors.New("authsvc: node certificate revocation is not configured")
	}
	if nodeID == "" || issuerSerial == nil || leafSerial == nil || issuerSerial.Sign() <= 0 || leafSerial.Sign() <= 0 {
		return errors.New("authsvc: invalid node certificate revocation")
	}
	if issuerSerial.Cmp(s.intermediate.Cert.SerialNumber) != 0 {
		return errors.New("authsvc: node certificate was not issued by the self-hosted authority")
	}
	now := s.now().UTC().Truncate(time.Second)
	issuerHex := issuerSerial.Text(16)
	var committedNumber *big.Int
	err := s.nodeRevocationStore.WithTx(ctx, func(tx store.Store) error {
		repo := tx.NodeRevocations()
		inserted, err := repo.Revoke(ctx, store.NodeRevocation{
			NodeID: nodeID, IssuerSerial: issuerHex, LeafSerial: leafSerial.Text(16), Reason: reason, RevokedAt: now.Unix(),
		})
		if err != nil {
			return err
		}
		if !inserted {
			current, err := repo.GetCRL(ctx, issuerHex)
			if err != nil {
				return err
			}
			committedNumber, _ = new(big.Int).SetString(current.Number, 10)
			if committedNumber == nil || committedNumber.Sign() <= 0 {
				return errors.New("authsvc: invalid persisted node CRL number")
			}
			return nil
		}
		committedNumber, err = s.issueNodeCRL(ctx, repo, issuerHex, now)
		return err
	})
	if err != nil {
		return err
	}
	if s.nodeCRLCommitted != nil {
		s.nodeCRLCommitted(new(big.Int).Set(committedNumber))
	}
	return s.publishCurrentNodeCRL(ctx)
}

// ReconcileLegacyNodeRevocations binds every legacy node-id-only row to an operator-supplied leaf,
// verifies that leaf under this service's self-hosted issuer, and commits the resulting CRL.
func (s *Service) ReconcileLegacyNodeRevocations(ctx context.Context, certificates map[string]*x509.Certificate) error {
	if s == nil || s.intermediate == nil || s.intermediate.Cert == nil || s.nodeRevocationStore == nil || s.nodeCRLSink == nil {
		return errors.New("authsvc: node certificate revocation is not configured")
	}
	legacy, err := s.nodeRevocationStore.NodeRevocations().ListLegacy(ctx)
	if err != nil {
		return err
	}
	if len(legacy) != len(certificates) {
		return fmt.Errorf("authsvc: legacy node revocation reconciliation requires %d exact certificate mappings, got %d", len(legacy), len(certificates))
	}
	if len(legacy) == 0 {
		return s.publishCurrentNodeCRL(ctx)
	}
	now := s.now().UTC().Truncate(time.Second)
	verified := make(map[string]*x509.Certificate, len(certificates))
	for _, row := range legacy {
		leaf := certificates[row.NodeID]
		if leaf == nil {
			return fmt.Errorf("authsvc: legacy node revocation %s has no certificate mapping", row.NodeID)
		}
		validationTime := now
		if validationTime.Before(leaf.NotBefore) || validationTime.After(leaf.NotAfter) {
			validationTime = leaf.NotBefore.Add(time.Second)
		}
		principal, err := pki.VerifyPrincipal(leaf, []*x509.Certificate{s.intermediate.Cert}, pki.VerifyOptions{
			Root: s.root, TrustDomain: s.trustDomain, CurrentTime: validationTime,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			IsRevoked: func(*big.Int, *big.Int) bool { return false },
		})
		if err != nil {
			return fmt.Errorf("authsvc: legacy node revocation %s certificate mapping is invalid: %w", row.NodeID, err)
		}
		if principal.Kind != pki.KindNode || principal.Role != pki.RoleSelfHosted || principal.NodeID != row.NodeID {
			return fmt.Errorf("authsvc: legacy node revocation %s certificate mapping has principal %s/%s/%s", row.NodeID, principal.Kind, principal.Role, principal.NodeID)
		}
		verified[row.NodeID] = leaf
	}
	issuerHex := s.intermediate.Cert.SerialNumber.Text(16)
	var committedNumber *big.Int
	err = s.nodeRevocationStore.WithTx(ctx, func(tx store.Store) error {
		repo := tx.NodeRevocations()
		currentLegacy, err := repo.ListLegacy(ctx)
		if err != nil {
			return err
		}
		if len(currentLegacy) != len(legacy) {
			return errors.New("authsvc: legacy node revocations changed during reconciliation")
		}
		for _, row := range currentLegacy {
			leaf := verified[row.NodeID]
			if leaf == nil {
				return fmt.Errorf("authsvc: legacy node revocation %s changed during reconciliation", row.NodeID)
			}
			if err := repo.ReconcileLegacy(ctx, row.NodeID, issuerHex, leaf.SerialNumber.Text(16)); err != nil {
				return err
			}
		}
		committedNumber, err = s.issueNodeCRL(ctx, repo, issuerHex, now)
		return err
	})
	if err != nil {
		return err
	}
	if s.nodeCRLCommitted != nil {
		s.nodeCRLCommitted(new(big.Int).Set(committedNumber))
	}
	return s.publishCurrentNodeCRL(ctx)
}

func (s *Service) issueNodeCRL(ctx context.Context, repo store.NodeRevocationRepo, issuerHex string, now time.Time) (*big.Int, error) {
	rows, err := repo.ListByIssuer(ctx, issuerHex)
	if err != nil {
		return nil, err
	}
	number := big.NewInt(1)
	if current, err := repo.GetCRL(ctx, issuerHex); err == nil {
		if _, ok := number.SetString(current.Number, 10); !ok || number.Sign() <= 0 {
			return nil, errors.New("authsvc: invalid persisted node CRL number")
		}
		number.Add(number, big.NewInt(1))
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	entries := make([]x509.RevocationListEntry, 0, len(rows))
	for _, row := range rows {
		serial, ok := new(big.Int).SetString(row.LeafSerial, 16)
		if !ok || serial.Sign() <= 0 {
			return nil, fmt.Errorf("authsvc: invalid stored leaf serial for node %s", row.NodeID)
		}
		entries = append(entries, x509.RevocationListEntry{SerialNumber: serial, RevocationTime: time.Unix(row.RevokedAt, 0).UTC()})
	}
	nextUpdate := now.Add(nodeCRLValidity)
	if nextUpdate.After(s.intermediate.Cert.NotAfter) {
		nextUpdate = s.intermediate.Cert.NotAfter
	}
	list, err := s.intermediate.CreateCRL(number, entries, now, nextUpdate)
	if err != nil {
		return nil, fmt.Errorf("authsvc: issue node CRL: %w", err)
	}
	if err := repo.PutCRL(ctx, store.NodeRevocationCRL{IssuerSerial: issuerHex, Number: number.String(), PEM: pki.MarshalCRLPEM(list)}); err != nil {
		return nil, err
	}
	return new(big.Int).Set(number), nil
}

// RecoverNodeCRLPublication republishes the latest committed self-hosted CRL without advancing its
// number. Startup calls this before listeners so a prior sink failure cannot leave stale deployment
// material indefinitely.
func (s *Service) RecoverNodeCRLPublication(ctx context.Context) error {
	if s == nil || s.intermediate == nil || s.intermediate.Cert == nil || s.nodeRevocationStore == nil || s.nodeCRLSink == nil {
		return errors.New("authsvc: node certificate revocation is not configured")
	}
	return s.publishCurrentNodeCRL(ctx)
}

func (s *Service) publishCurrentNodeCRL(ctx context.Context) error {
	s.nodeCRLPublishMu.Lock()
	defer s.nodeCRLPublishMu.Unlock()
	current, err := s.nodeRevocationStore.NodeRevocations().GetCRL(ctx, s.intermediate.Cert.SerialNumber.Text(16))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.nodeCRLSink(append([]byte(nil), current.PEM...)); err != nil {
		return fmt.Errorf("authsvc: publish node CRL %s: %w", current.Number, err)
	}
	return nil
}

func (s *Service) serveNodeRevocations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	rows, err := s.nodeRevocations.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "node revocations unavailable")
		return
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.NodeID)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		RevokedNodeIDs []string `json:"revoked_node_ids"`
		GeneratedAt    int64    `json:"generated_at"`
	}{
		RevokedNodeIDs: ids,
		GeneratedAt:    s.now().Unix(),
	})
}
