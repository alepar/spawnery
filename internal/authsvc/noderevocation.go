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
	var published []byte
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
			published = append([]byte(nil), current.PEM...)
			return nil
		}
		rows, err := repo.ListByIssuer(ctx, issuerHex)
		if err != nil {
			return err
		}
		number := big.NewInt(1)
		if current, err := repo.GetCRL(ctx, issuerHex); err == nil {
			if _, ok := number.SetString(current.Number, 10); !ok || number.Sign() <= 0 {
				return errors.New("authsvc: invalid persisted node CRL number")
			}
			number.Add(number, big.NewInt(1))
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		entries := make([]x509.RevocationListEntry, 0, len(rows))
		for _, row := range rows {
			serial, ok := new(big.Int).SetString(row.LeafSerial, 16)
			if !ok || serial.Sign() <= 0 {
				return fmt.Errorf("authsvc: invalid stored leaf serial for node %s", row.NodeID)
			}
			entries = append(entries, x509.RevocationListEntry{SerialNumber: serial, RevocationTime: time.Unix(row.RevokedAt, 0).UTC()})
		}
		nextUpdate := now.Add(nodeCRLValidity)
		if nextUpdate.After(s.intermediate.Cert.NotAfter) {
			nextUpdate = s.intermediate.Cert.NotAfter
		}
		list, err := s.intermediate.CreateCRL(number, entries, now, nextUpdate)
		if err != nil {
			return fmt.Errorf("authsvc: issue node CRL: %w", err)
		}
		published = pki.MarshalCRLPEM(list)
		return repo.PutCRL(ctx, store.NodeRevocationCRL{IssuerSerial: issuerHex, Number: number.String(), PEM: published})
	})
	if err != nil {
		return err
	}
	if err := s.nodeCRLSink(published); err != nil {
		return fmt.Errorf("authsvc: publish node CRL: %w", err)
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
