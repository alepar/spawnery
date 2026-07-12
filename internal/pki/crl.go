package pki

import (
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const crlPEMType = "X509 CRL"

// CreateCRL creates a signed, numbered CRL for a Spawnery role-bearing intermediate.
func (ca *CA) CreateCRL(number *big.Int, revoked []x509.RevocationListEntry, now, nextUpdate time.Time) (*x509.RevocationList, error) {
	if ca == nil || ca.Cert == nil || ca.Key == nil {
		return nil, errors.New("pki: invalid CRL authority")
	}
	if err := validateCRLIssuer(ca.Cert); err != nil {
		return nil, err
	}
	if number == nil || number.Sign() <= 0 {
		return nil, errors.New("pki: CRL number must be positive")
	}
	if now.IsZero() || nextUpdate.IsZero() || !nextUpdate.After(now) {
		return nil, errors.New("pki: invalid CRL update window")
	}
	if now.Before(ca.Cert.NotBefore) || nextUpdate.After(ca.Cert.NotAfter) {
		return nil, errors.New("pki: CRL update window exceeds issuer validity")
	}
	if err := validateRevocationEntries(revoked, now); err != nil {
		return nil, err
	}
	template := &x509.RevocationList{
		Number:                    new(big.Int).Set(number),
		ThisUpdate:                now,
		NextUpdate:                nextUpdate,
		RevokedCertificateEntries: append([]x509.RevocationListEntry(nil), revoked...),
	}
	der, err := x509.CreateRevocationList(rand.Reader, template, ca.Cert, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("pki: create CRL: %w", err)
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		return nil, fmt.Errorf("pki: parse created CRL: %w", err)
	}
	return list, nil
}

// VerifyCRL verifies the signature, issuer identity, profile, number, validity, and entries of list.
func VerifyCRL(list *x509.RevocationList, issuer *x509.Certificate, now time.Time) error {
	if list == nil || len(list.Raw) == 0 || issuer == nil || now.IsZero() {
		return errors.New("pki: invalid CRL verification input")
	}
	if err := validateCRLIssuer(issuer); err != nil {
		return err
	}
	if !bytes.Equal(list.RawIssuer, issuer.RawSubject) {
		return errors.New("pki: CRL issuer does not match certificate subject")
	}
	if len(issuer.SubjectKeyId) == 0 || !bytes.Equal(list.AuthorityKeyId, issuer.SubjectKeyId) {
		return errors.New("pki: CRL authority key identifier does not match issuer")
	}
	if err := list.CheckSignatureFrom(issuer); err != nil {
		return fmt.Errorf("pki: verify CRL signature: %w", err)
	}
	if list.Number == nil || list.Number.Sign() <= 0 {
		return errors.New("pki: CRL number must be positive")
	}
	if list.ThisUpdate.IsZero() || list.NextUpdate.IsZero() || !list.NextUpdate.After(list.ThisUpdate) {
		return errors.New("pki: invalid CRL update window")
	}
	if list.ThisUpdate.Before(issuer.NotBefore) || list.NextUpdate.After(issuer.NotAfter) {
		return errors.New("pki: CRL update window exceeds issuer validity")
	}
	if list.ThisUpdate.After(now) {
		return errors.New("pki: CRL is not yet valid")
	}
	if !list.NextUpdate.After(now) {
		return errors.New("pki: CRL is expired")
	}
	if err := validateRevocationEntries(list.RevokedCertificateEntries, list.ThisUpdate); err != nil {
		return err
	}
	return nil
}

// MarshalCRLPEM encodes list as one X509 CRL PEM block.
func MarshalCRLPEM(list *x509.RevocationList) []byte {
	if list == nil || len(list.Raw) == 0 {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: crlPEMType, Bytes: list.Raw})
}

// ParseCRLPEM parses exactly one X509 CRL PEM block.
func ParseCRLPEM(data []byte) (*x509.RevocationList, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != crlPEMType || len(block.Headers) != 0 {
		return nil, errors.New("pki: no canonical X509 CRL PEM block")
	}
	if strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("pki: trailing data after X509 CRL PEM block")
	}
	list, err := x509.ParseRevocationList(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse CRL: %w", err)
	}
	return list, nil
}

func validateCRLIssuer(issuer *x509.Certificate) error {
	if issuer == nil || len(issuer.Raw) == 0 || !issuer.IsCA || !issuer.BasicConstraintsValid || !issuer.MaxPathLenZero || issuer.MaxPathLen != 0 {
		return errors.New("pki: CRL issuer must be a non-delegating intermediate CA")
	}
	if issuer.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign {
		return errors.New("pki: CRL issuer has invalid key usage")
	}
	if issuer.SerialNumber == nil || issuer.SerialNumber.Sign() <= 0 || len(issuer.SubjectKeyId) == 0 {
		return errors.New("pki: CRL issuer has invalid identity")
	}
	if _, err := IssuerRoleFromCertificate(issuer); err != nil {
		return fmt.Errorf("pki: invalid CRL issuer role: %w", err)
	}
	if len(issuer.URIs) != 1 || issuer.URIs[0] == nil {
		return errors.New("pki: CRL issuer must contain one trust-domain URI SAN")
	}
	trustDomain := issuer.URIs[0].Host
	if err := validateTrustDomain(trustDomain); err != nil {
		return fmt.Errorf("pki: invalid CRL issuer trust domain: %w", err)
	}
	if err := validateIntermediateSPIFFEID(issuer, trustDomain); err != nil {
		return err
	}
	return nil
}

func validateRevocationEntries(entries []x509.RevocationListEntry, thisUpdate time.Time) error {
	serials := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.SerialNumber == nil || entry.SerialNumber.Sign() <= 0 {
			return errors.New("pki: revoked certificate serial must be positive")
		}
		serial := entry.SerialNumber.Text(16)
		if _, duplicate := serials[serial]; duplicate {
			return fmt.Errorf("pki: duplicate revoked certificate serial %s", serial)
		}
		serials[serial] = struct{}{}
		if entry.RevocationTime.IsZero() || entry.RevocationTime.After(thisUpdate) {
			return fmt.Errorf("pki: invalid revocation time for serial %s", serial)
		}
	}
	return nil
}
