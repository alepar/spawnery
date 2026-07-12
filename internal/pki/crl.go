package pki

import (
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

const (
	crlPEMType = "X509 CRL"
	maxCRLSize = 4 << 20
)

var ErrCRLTooLarge = errors.New("pki: CRL exceeds size limit")

var (
	authorityKeyIdentifierOID = asn1.ObjectIdentifier{2, 5, 29, 35}
	crlNumberOID              = asn1.ObjectIdentifier{2, 5, 29, 20}
)

// CreateCRL creates a signed, numbered CRL for a Spawnery role-bearing intermediate.
func (ca *CA) CreateCRL(number *big.Int, revoked []x509.RevocationListEntry, now, nextUpdate time.Time) (*x509.RevocationList, error) {
	if ca == nil || ca.Cert == nil || ca.Key == nil {
		return nil, errors.New("pki: invalid CRL authority")
	}
	if err := validateCRLIssuer(ca.Cert); err != nil {
		return nil, err
	}
	if !validCRLNumber(number) {
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
	if err := VerifyCRL(list, ca.Cert, now); err != nil {
		return nil, fmt.Errorf("pki: verify created CRL: %w", err)
	}
	return list, nil
}

// VerifyCRL verifies the signature, issuer identity, profile, number, validity, and entries of list.
func VerifyCRL(list *x509.RevocationList, issuer *x509.Certificate, now time.Time) error {
	if list == nil || len(list.Raw) == 0 || issuer == nil || now.IsZero() {
		return errors.New("pki: invalid CRL verification input")
	}
	if len(list.Raw) > maxCRLSize {
		return ErrCRLTooLarge
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
	if err := validateCRLExtensions(list); err != nil {
		return err
	}
	if !validCRLNumber(list.Number) {
		return errors.New("pki: CRL number must be positive and at most 20 DER octets")
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

func validCRLNumber(number *big.Int) bool {
	if number == nil || number.Sign() <= 0 {
		return false
	}
	encoded := number.Bytes()
	return len(encoded) < 20 || len(encoded) == 20 && encoded[0]&0x80 == 0
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
	if len(data) > maxCRLSize {
		return nil, ErrCRLTooLarge
	}
	if !bytes.HasPrefix(data, []byte("-----BEGIN "+crlPEMType+"-----")) {
		return nil, errors.New("pki: no canonical X509 CRL PEM block")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != crlPEMType || len(block.Headers) != 0 {
		return nil, errors.New("pki: no canonical X509 CRL PEM block")
	}
	if len(rest) != 0 {
		return nil, errors.New("pki: trailing data after X509 CRL PEM block")
	}
	list, err := x509.ParseRevocationList(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse CRL: %w", err)
	}
	if !bytes.Equal(data, MarshalCRLPEM(list)) {
		return nil, errors.New("pki: non-canonical X509 CRL PEM encoding")
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
		if entry.ReasonCode != 0 || len(entry.Extensions) != 0 || len(entry.ExtraExtensions) != 0 {
			return errors.New("pki: CRL entry extensions are unsupported")
		}
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

func validateCRLExtensions(list *x509.RevocationList) error {
	if len(list.Extensions) != 2 {
		return fmt.Errorf("pki: CRL extension count is %d, want 2", len(list.Extensions))
	}
	seenAuthorityKeyID := false
	seenNumber := false
	for _, extension := range list.Extensions {
		if extension.Critical {
			return fmt.Errorf("pki: critical CRL extension %s is unsupported", extension.Id.String())
		}
		switch {
		case extension.Id.Equal(authorityKeyIdentifierOID):
			if seenAuthorityKeyID {
				return errors.New("pki: duplicate CRL authority key identifier")
			}
			seenAuthorityKeyID = true
		case extension.Id.Equal(crlNumberOID):
			if seenNumber {
				return errors.New("pki: duplicate CRL number")
			}
			seenNumber = true
		default:
			return fmt.Errorf("pki: unsupported CRL extension %s", extension.Id.String())
		}
	}
	if !seenAuthorityKeyID || !seenNumber {
		return errors.New("pki: CRL lacks required extensions")
	}
	return nil
}
