package authsvc

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
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
	s.nodeCRLMutationMu.Lock()
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
	s.nodeCRLMutationMu.Unlock()
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
	s.nodeCRLMutationMu.Lock()
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
	s.nodeCRLMutationMu.Unlock()
	if err != nil {
		return err
	}
	if s.nodeCRLCommitted != nil {
		s.nodeCRLCommitted(new(big.Int).Set(committedNumber))
	}
	return s.publishCurrentNodeCRL(ctx)
}

// EnsureCurrentNodeCRL creates the initial empty self-hosted CRL, renews a committed CRL before
// expiry, or republishes a still-current checkpoint. Issuance and the full revoked set are committed
// atomically; publication rereads that durable outbox and never publishes an older checkpoint.
func (s *Service) EnsureCurrentNodeCRL(ctx context.Context, renewBefore time.Duration) error {
	if s == nil || s.intermediate == nil || s.intermediate.Cert == nil || s.nodeRevocationStore == nil || s.nodeCRLSink == nil {
		return errors.New("authsvc: node certificate revocation is not configured")
	}
	if renewBefore < 0 || renewBefore >= nodeCRLValidity {
		return fmt.Errorf("authsvc: node CRL renew-before must be in [0, %s)", nodeCRLValidity)
	}
	now := s.now().UTC().Truncate(time.Second)
	issuerHex := s.intermediate.Cert.SerialNumber.Text(16)
	var committedNumber *big.Int
	s.nodeCRLMutationMu.Lock()
	err := s.nodeRevocationStore.WithTx(ctx, func(tx store.Store) error {
		repo := tx.NodeRevocations()
		current, err := repo.GetCRL(ctx, issuerHex)
		if errors.Is(err, store.ErrNotFound) {
			committedNumber, err = s.issueNodeCRL(ctx, repo, issuerHex, now)
			return err
		}
		if err != nil {
			return err
		}
		list, err := pki.ParseCRLPEM(current.PEM)
		if err != nil {
			return fmt.Errorf("authsvc: parse persisted node CRL: %w", err)
		}
		if err := list.CheckSignatureFrom(s.intermediate.Cert); err != nil {
			return fmt.Errorf("authsvc: verify persisted node CRL signature: %w", err)
		}
		if list.Number == nil || list.Number.Sign() <= 0 || list.Number.String() != current.Number {
			return errors.New("authsvc: persisted node CRL number does not match its checkpoint")
		}
		if !list.NextUpdate.After(now.Add(renewBefore)) {
			committedNumber, err = s.issueNodeCRL(ctx, repo, issuerHex, now)
			return err
		}
		committedNumber = new(big.Int).Set(list.Number)
		return nil
	})
	s.nodeCRLMutationMu.Unlock()
	if err != nil {
		return err
	}
	if s.nodeCRLCommitted != nil {
		s.nodeCRLCommitted(new(big.Int).Set(committedNumber))
	}
	return s.publishCurrentNodeCRL(ctx)
}

// RunNodeCRLRenewal maintains the self-hosted CRL until ctx is cancelled. Failures are reported and
// retried on the next bounded interval; the last durable checkpoint remains the publication source.
func (s *Service) RunNodeCRLRenewal(ctx context.Context, interval, renewBefore time.Duration, report func(error)) {
	if interval <= 0 {
		if report != nil {
			report(errors.New("authsvc: node CRL renewal interval must be positive"))
		}
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.EnsureCurrentNodeCRL(ctx, renewBefore); err != nil && ctx.Err() == nil && report != nil {
				report(err)
			}
		}
	}
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
	return s.EnsureCurrentNodeCRL(ctx, 0)
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
