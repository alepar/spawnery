package pki

import (
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"
)

func TestCRLCreateVerifyAndPEMRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, err := NewRootCA("root")
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []IssuerRole{IssuerService, IssuerCloudNode, IssuerSelfHostedNode} {
		t.Run(string(role), func(t *testing.T) {
			issuer, err := root.NewIntermediate(role, "prod.spawnery.internal")
			if err != nil {
				t.Fatal(err)
			}
			serial := big.NewInt(42)
			list, err := issuer.CreateCRL(big.NewInt(7), []x509.RevocationListEntry{{
				SerialNumber:   serial,
				RevocationTime: now.Add(-time.Minute),
			}}, now, now.Add(time.Hour))
			if err != nil {
				t.Fatalf("CreateCRL: %v", err)
			}
			if err := VerifyCRL(list, issuer.Cert, now); err != nil {
				t.Fatalf("VerifyCRL: %v", err)
			}
			if list.Number.Cmp(big.NewInt(7)) != 0 || !list.ThisUpdate.Equal(now) || !list.NextUpdate.Equal(now.Add(time.Hour)) {
				t.Fatalf("CRL bounds = number %v, this %v, next %v", list.Number, list.ThisUpdate, list.NextUpdate)
			}
			if len(list.RevokedCertificateEntries) != 1 || list.RevokedCertificateEntries[0].SerialNumber.Cmp(serial) != 0 {
				t.Fatalf("revoked entries = %+v", list.RevokedCertificateEntries)
			}
			encoded := MarshalCRLPEM(list)
			parsed, err := ParseCRLPEM(encoded)
			if err != nil {
				t.Fatalf("ParseCRLPEM: %v", err)
			}
			if !bytes.Equal(parsed.Raw, list.Raw) {
				t.Fatal("PEM round trip changed CRL DER")
			}
			if err := VerifyCRL(parsed, issuer.Cert, now); err != nil {
				t.Fatalf("VerifyCRL after PEM round trip: %v", err)
			}
		})
	}
}

func TestCRLRejectsInvalidAuthorityNumberAndWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	other, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	valid := mustCreateRawCRL(t, issuer, big.NewInt(1), now.Add(-time.Minute), now.Add(time.Hour), nil)
	missingNumber := *valid
	missingNumber.Number = nil

	tests := []struct {
		name   string
		list   *x509.RevocationList
		issuer *x509.Certificate
		at     time.Time
	}{
		{name: "root presented as issuer", list: mustCreateRawCRL(t, root, big.NewInt(1), now.Add(-time.Minute), now.Add(time.Hour), nil), issuer: root.Cert, at: now},
		{name: "wrong intermediate", list: valid, issuer: other.Cert, at: now},
		{name: "missing number", list: &missingNumber, issuer: issuer.Cert, at: now},
		{name: "zero number", list: mustCreateRawCRL(t, issuer, big.NewInt(0), now.Add(-time.Minute), now.Add(time.Hour), nil), issuer: issuer.Cert, at: now},
		{name: "negative number", list: mustCreateRawCRL(t, issuer, big.NewInt(-1), now.Add(-time.Minute), now.Add(time.Hour), nil), issuer: issuer.Cert, at: now},
		{name: "expired", list: mustCreateRawCRL(t, issuer, big.NewInt(2), now.Add(-time.Hour), now, nil), issuer: issuer.Cert, at: now},
		{name: "not yet valid", list: mustCreateRawCRL(t, issuer, big.NewInt(2), now.Add(time.Minute), now.Add(time.Hour), nil), issuer: issuer.Cert, at: now},
		{name: "before issuer validity", list: mustCreateRawCRL(t, issuer, big.NewInt(2), issuer.Cert.NotBefore.Add(-time.Minute), now.Add(time.Hour), nil), issuer: issuer.Cert, at: now},
		{name: "after issuer validity", list: mustCreateRawCRL(t, issuer, big.NewInt(2), now.Add(-time.Minute), issuer.Cert.NotAfter.Add(time.Minute), nil), issuer: issuer.Cert, at: now},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := VerifyCRL(tt.list, tt.issuer, tt.at); err == nil {
				t.Fatal("invalid CRL accepted")
			}
		})
	}

	badCA := *issuer.Cert
	badCA.KeyUsage &^= x509.KeyUsageCRLSign
	if err := VerifyCRL(valid, &badCA, now); err == nil {
		t.Fatal("issuer without CRLSign accepted")
	}
	if _, err := (&CA{Cert: root.Cert, Key: root.Key}).CreateCRL(big.NewInt(1), nil, now, now.Add(time.Hour)); err == nil {
		t.Fatal("root created a role-issuer CRL")
	}
	if _, err := issuer.CreateCRL(big.NewInt(0), nil, now, now.Add(time.Hour)); err == nil {
		t.Fatal("zero CRL number accepted for creation")
	}
	if _, err := issuer.CreateCRL(big.NewInt(1), nil, now, now); err == nil {
		t.Fatal("invalid CRL update window accepted for creation")
	}
	if _, err := (&CA{Cert: issuer.Cert, Key: other.Key}).CreateCRL(big.NewInt(1), nil, now, now.Add(time.Hour)); err == nil {
		t.Fatal("issuer certificate paired with wrong private key created a CRL")
	}
}

func TestCRLNumberIsLimitedToTwentyOctets(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 159), big.NewInt(1))
	if _, err := issuer.CreateCRL(max, nil, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("20-octet CRL number rejected: %v", err)
	}
	tooLarge := new(big.Int).Lsh(big.NewInt(1), 159)
	if _, err := issuer.CreateCRL(tooLarge, nil, now, now.Add(time.Hour)); err == nil {
		t.Fatal("21-octet CRL number created")
	}
	list := mustCreateRawCRL(t, issuer, max, now, now.Add(time.Hour), nil)
	oversized := *list
	oversized.Number = tooLarge
	if err := VerifyCRL(&oversized, issuer.Cert, now); err == nil {
		t.Fatal("21-octet CRL number verified")
	}
}

func TestCRLRejectsInvalidEntriesAndPEM(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerCloudNode, "prod.spawnery.internal")
	for _, entries := range [][]x509.RevocationListEntry{
		{{SerialNumber: nil, RevocationTime: now}},
		{{SerialNumber: big.NewInt(0), RevocationTime: now}},
		{{SerialNumber: big.NewInt(1), RevocationTime: now}, {SerialNumber: big.NewInt(1), RevocationTime: now}},
		{{SerialNumber: big.NewInt(1), RevocationTime: now.Add(time.Minute)}},
	} {
		if _, err := issuer.CreateCRL(big.NewInt(1), entries, now, now.Add(time.Hour)); err == nil {
			t.Fatalf("invalid entries accepted: %+v", entries)
		}
	}

	list, err := issuer.CreateCRL(big.NewInt(1), nil, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	good := MarshalCRLPEM(list)
	for name, data := range map[string][]byte{
		"garbage":             []byte("not pem"),
		"wrong type":          pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: list.Raw}),
		"leading bytes":       append([]byte("junk\n"), good...),
		"leading whitespace":  append([]byte("\n"), good...),
		"trailing bytes":      append(append([]byte(nil), good...), []byte("junk")...),
		"trailing whitespace": append(append([]byte(nil), good...), '\n'),
		"CRLF":                bytes.ReplaceAll(good, []byte("\n"), []byte("\r\n")),
		"alternate wrapping":  nonCanonicalCRLPEM(list.Raw),
		"second block":        append(append([]byte(nil), good...), good...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCRLPEM(data); err == nil {
				t.Fatal("invalid CRL PEM accepted")
			}
		})
	}
}

func TestCRLPEMSizeLimitBoundary(t *testing.T) {
	if _, err := ParseCRLPEM(bytes.Repeat([]byte{'x'}, maxCRLSize)); errors.Is(err, ErrCRLTooLarge) {
		t.Fatal("CRL at size boundary reported too large")
	}
	if _, err := ParseCRLPEM(bytes.Repeat([]byte{'x'}, maxCRLSize+1)); !errors.Is(err, ErrCRLTooLarge) {
		t.Fatalf("oversized CRL error = %v", err)
	}
}

func TestVerifyCRLRejectsOversizedRawObject(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	list, err := issuer.CreateCRL(big.NewInt(1), nil, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	oversized := *list
	oversized.Raw = bytes.Repeat([]byte{'x'}, maxCRLSize+1)
	if err := VerifyCRL(&oversized, issuer.Cert, now); !errors.Is(err, ErrCRLTooLarge) {
		t.Fatalf("oversized parsed CRL error = %v", err)
	}
}

func nonCanonicalCRLPEM(der []byte) []byte {
	encoded := base64.StdEncoding.EncodeToString(der)
	return []byte("-----BEGIN X509 CRL-----\n" + encoded + "\n-----END X509 CRL-----\n")
}

func TestCRLRejectsUnsupportedListAndEntryExtensions(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	valid := mustCreateRawCRL(t, issuer, big.NewInt(1), now, now.Add(time.Hour), nil)
	if len(valid.Extensions) != 2 {
		t.Fatalf("standard CRL extension count = %d, want 2", len(valid.Extensions))
	}
	for _, tt := range []struct {
		name     string
		oid      asn1.ObjectIdentifier
		critical bool
		value    []byte
	}{
		{name: "issuing distribution point", oid: []int{2, 5, 29, 28}, critical: true, value: []byte{0x30, 0x00}},
		{name: "indirect CRL", oid: []int{2, 5, 29, 28}, critical: true, value: []byte{0x30, 0x03, 0x84, 0x01, 0xff}},
		{name: "delta CRL indicator", oid: []int{2, 5, 29, 27}, critical: true, value: []byte{0x02, 0x01, 0x01}},
		{name: "unknown critical", oid: []int{1, 2, 3, 4}, critical: true, value: []byte{0x05, 0x00}},
		{name: "unknown noncritical", oid: []int{1, 2, 3, 5}, value: []byte{0x05, 0x00}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			list := mustCreateRawCRLWithExtensions(t, issuer, now, []pkix.Extension{{
				Id: tt.oid, Critical: tt.critical, Value: tt.value,
			}}, nil)
			if err := VerifyCRL(list, issuer.Cert, now); err == nil {
				t.Fatal("unsupported CRL extension accepted")
			}
		})
	}

	for name, entry := range map[string]x509.RevocationListEntry{
		"reason code": {
			SerialNumber: big.NewInt(1), RevocationTime: now.Add(-time.Minute), ReasonCode: 1,
		},
		"unknown entry extension": {
			SerialNumber: big.NewInt(1), RevocationTime: now.Add(-time.Minute),
			ExtraExtensions: []pkix.Extension{{Id: []int{1, 2, 3, 6}, Value: []byte{0x05, 0x00}}},
		},
	} {
		t.Run(name+" create", func(t *testing.T) {
			if _, err := issuer.CreateCRL(big.NewInt(1), []x509.RevocationListEntry{entry}, now, now.Add(time.Hour)); err == nil {
				t.Fatal("unsupported entry extension created")
			}
		})
		t.Run(name+" verify", func(t *testing.T) {
			list := mustCreateRawCRLWithExtensions(t, issuer, now, nil, []x509.RevocationListEntry{entry})
			if err := VerifyCRL(list, issuer.Cert, now); err == nil {
				t.Fatal("unsupported entry extension verified")
			}
		})
	}
}

func mustCreateRawCRL(t *testing.T, issuer *CA, number *big.Int, thisUpdate, nextUpdate time.Time, entries []x509.RevocationListEntry) *x509.RevocationList {
	t.Helper()
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number: number, ThisUpdate: thisUpdate, NextUpdate: nextUpdate, RevokedCertificateEntries: entries,
	}, issuer.Cert, issuer.Key)
	if err != nil {
		t.Fatalf("create raw CRL: %v", err)
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatalf("parse raw CRL: %v", err)
	}
	return list
}

func mustCreateRawCRLWithExtensions(t *testing.T, issuer *CA, now time.Time, extensions []pkix.Extension, entries []x509.RevocationListEntry) *x509.RevocationList {
	t.Helper()
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number: big.NewInt(1), ThisUpdate: now, NextUpdate: now.Add(time.Hour),
		RevokedCertificateEntries: entries, ExtraExtensions: extensions,
	}, issuer.Cert, issuer.Key)
	if err != nil {
		t.Fatalf("create extended raw CRL: %v", err)
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatalf("parse extended raw CRL: %v", err)
	}
	return list
}
