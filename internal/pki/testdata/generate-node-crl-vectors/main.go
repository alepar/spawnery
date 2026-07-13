// Command generate-node-crl-vectors writes deterministic, validly signed browser CRL fixtures.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"spawnery/internal/pki"
)

var (
	oidECDSAWithSHA256 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidAKI             = asn1.ObjectIdentifier{2, 5, 29, 35}
	oidCRLNumber       = asn1.ObjectIdentifier{2, 5, 29, 20}
	oidDeltaCRL        = asn1.ObjectIdentifier{2, 5, 29, 27}
	oidIssuingDP       = asn1.ObjectIdentifier{2, 5, 29, 28}
	oidUnknown         = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 55555, 1}
	oidSKID            = asn1.ObjectIdentifier{2, 5, 29, 14}
)

const (
	trustDomain = "prod.spawnery.internal"
	outputPath  = "web/src/auth/testdata/node-crl-vectors.json"
)

type bundle struct {
	Class     string `json:"class"`
	IssuerPEM string `json:"issuerPEM"`
	CRLPEM    string `json:"crlPEM"`
}

type scenario struct {
	ChainPEM    string   `json:"chainPEM"`
	Bundles     []bundle `json:"bundles"`
	TrustDomain string   `json:"trustDomain,omitempty"`
	Error       string   `json:"error,omitempty"`
}

type vectors struct {
	Now       string              `json:"now"`
	RootPEM   string              `json:"rootPEM"`
	ChainPEM  string              `json:"chainPEM"`
	Valid     []bundle            `json:"validBundles"`
	Accepted  map[string]scenario `json:"acceptedScenarios"`
	Scenarios map[string]scenario `json:"scenarios"`
}

type fixedReader struct {
	seed    []byte
	counter uint64
	buffer  []byte
}

func (r *fixedReader) Read(p []byte) (int, error) {
	// crypto/internal/randutil may issue a nondeterministic one-byte probe. Do not
	// let that probe shift the deterministic stream used by the actual signature.
	if len(p) == 1 {
		p[0] = 0
		return 1, nil
	}
	for len(r.buffer) < len(p) {
		h := sha256.New()
		h.Write(r.seed)
		h.Write(new(big.Int).SetUint64(r.counter).FillBytes(make([]byte, 8)))
		r.counter++
		r.buffer = append(r.buffer, h.Sum(nil)...)
	}
	copy(p, r.buffer[:len(p)])
	r.buffer = r.buffer[len(p):]
	return len(p), nil
}

func entropy(label string) *fixedReader { return &fixedReader{seed: []byte(label)} }

func key(label string) *ecdsa.PrivateKey {
	sum := sha256.Sum256([]byte("spawnery/browser-crl-vector/" + label))
	n := new(big.Int).Sub(elliptic.P256().Params().N, big.NewInt(1))
	d := new(big.Int).SetBytes(sum[:])
	d.Mod(d, n)
	d.Add(d, big.NewInt(1))
	x, y := elliptic.P256().ScalarBaseMult(d.Bytes())
	return &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, D: d}
}

func serial(n int64) *big.Int { return big.NewInt(n) }

func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

func mustOID(raw string) x509.OID {
	oid, err := x509.ParseOID(raw)
	if err != nil {
		panic(err)
	}
	return oid
}

func cert(label string, tmpl, parent *x509.Certificate, pub *ecdsa.PublicKey, signer *ecdsa.PrivateKey) *x509.Certificate {
	der, err := x509.CreateCertificate(entropy("cert/"+label), tmpl, parent, pub, signer)
	if err != nil {
		panic(err)
	}
	out, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	return out
}

func makeRoot(now time.Time) (*x509.Certificate, *ecdsa.PrivateKey) {
	k := key("root")
	t := &x509.Certificate{
		SerialNumber: serial(1), Subject: pkix.Name{CommonName: "Spawnery Browser CRL Root"},
		NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true, IsCA: true,
	}
	return cert("root", t, t, &k.PublicKey, k), k
}

type issuerOptions struct {
	pathLen     int
	pathLenZero bool
	keyUsage    x509.KeyUsage
	policy      x509.OID
	uri         string
	emails      []string
	emptySKID   bool
	notBefore   time.Time
	notAfter    time.Time
}

func makeIssuer(label string, now time.Time, root *x509.Certificate, rootKey *ecdsa.PrivateKey, opts issuerOptions) (*x509.Certificate, *ecdsa.PrivateKey) {
	k := key("issuer/" + label)
	t := &x509.Certificate{
		SerialNumber: serial(100 + int64(len(label))), Subject: pkix.Name{CommonName: "Spawnery " + label + " Intermediate"},
		NotBefore: opts.notBefore, NotAfter: opts.notAfter,
		KeyUsage: opts.keyUsage, BasicConstraintsValid: true, IsCA: true,
		MaxPathLen: opts.pathLen, MaxPathLenZero: opts.pathLenZero,
		URIs: []*url.URL{mustURL(opts.uri)}, EmailAddresses: opts.emails, Policies: []x509.OID{opts.policy},
	}
	if opts.emptySKID {
		empty, err := asn1.Marshal([]byte{})
		if err != nil {
			panic(err)
		}
		t.ExtraExtensions = []pkix.Extension{{Id: oidSKID, Value: empty}}
	}
	return cert("issuer/"+label, t, root, &k.PublicKey, rootKey), k
}

func makeLeaf(label string, now time.Time, issuer *x509.Certificate, issuerKey *ecdsa.PrivateKey) *x509.Certificate {
	k := key("leaf/" + label)
	t := &x509.Certificate{
		SerialNumber: serial(9001), Subject: pkix.Name{CommonName: "node-1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{mustURL("spiffe://" + trustDomain + "/node/self-hosted/acct-1/node-1")},
	}
	return cert("leaf/"+label, t, issuer, &k.PublicKey, issuerKey)
}

func pemCert(cert *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
}

func pemCRL(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der}))
}

func akiExtension(id []byte) pkix.Extension {
	value, err := asn1.Marshal(struct {
		ID []byte `asn1:"tag:0,optional"`
	}{ID: id})
	if err != nil {
		panic(err)
	}
	return pkix.Extension{Id: oidAKI, Value: value}
}

func numberExtension(n *big.Int) pkix.Extension {
	value, err := asn1.Marshal(n)
	if err != nil {
		panic(err)
	}
	return pkix.Extension{Id: oidCRLNumber, Value: value}
}

func alternateCommonNameEncoding(rawSubject []byte) []byte {
	out := bytes.Clone(rawSubject)
	commonNamePrintableString := []byte{0x06, 0x03, 0x55, 0x04, 0x03, 0x13}
	index := bytes.Index(out, commonNamePrintableString)
	if index < 0 {
		panic("issuer common name is not encoded as PrintableString")
	}
	out[index+len(commonNamePrintableString)-1] = 0x0c // UTF8String, same text and length.
	return out
}

type tbsCRL struct {
	Raw        asn1.RawContent
	Version    int `asn1:"optional,default:0"`
	Signature  pkix.AlgorithmIdentifier
	Issuer     asn1.RawValue
	ThisUpdate time.Time
	NextUpdate time.Time                 `asn1:"optional"`
	Revoked    []pkix.RevokedCertificate `asn1:"optional"`
	Extensions []pkix.Extension          `asn1:"tag:0,optional,explicit"`
}

type signedCRL struct {
	TBS       tbsCRL
	Algorithm pkix.AlgorithmIdentifier
	Signature asn1.BitString
}

func customCRL(label string, issuer *x509.Certificate, signer *ecdsa.PrivateKey, thisUpdate, nextUpdate time.Time, revoked []pkix.RevokedCertificate, extensions []pkix.Extension) []byte {
	return customCRLWithRawIssuer(label, issuer.RawSubject, signer, thisUpdate, nextUpdate, revoked, extensions)
}

func customCRLWithRawIssuer(label string, rawIssuer []byte, signer *ecdsa.PrivateKey, thisUpdate, nextUpdate time.Time, revoked []pkix.RevokedCertificate, extensions []pkix.Extension) []byte {
	alg := pkix.AlgorithmIdentifier{Algorithm: oidECDSAWithSHA256}
	tbs := tbsCRL{
		Version: 1, Signature: alg, Issuer: asn1.RawValue{FullBytes: rawIssuer},
		ThisUpdate: thisUpdate.UTC(), NextUpdate: nextUpdate.UTC(), Revoked: revoked, Extensions: extensions,
	}
	tbsDER, err := asn1.Marshal(tbs)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(tbsDER)
	sig, err := ecdsa.SignASN1(entropy("crl/"+label), signer, digest[:])
	if err != nil {
		panic(err)
	}
	tbs.Raw = tbsDER
	der, err := asn1.Marshal(signedCRL{TBS: tbs, Algorithm: alg, Signature: asn1.BitString{Bytes: sig, BitLength: len(sig) * 8}})
	if err != nil {
		panic(err)
	}
	return der
}

func standardCRL(label string, issuer *x509.Certificate, signer *ecdsa.PrivateKey, number *big.Int, thisUpdate, nextUpdate time.Time, entries []x509.RevocationListEntry, extras ...pkix.Extension) []byte {
	der, err := x509.CreateRevocationList(entropy("standard-crl/"+label), &x509.RevocationList{
		Number: number, ThisUpdate: thisUpdate, NextUpdate: nextUpdate,
		RevokedCertificateEntries: entries, ExtraExtensions: extras,
	}, issuer, signer)
	if err != nil {
		panic(err)
	}
	return der
}

func main() {
	// Go 1.26 ignores signer readers by default. Fixtures need reproducible
	// signatures, so opt this standalone generator into its supplied readers.
	if err := os.Setenv("GODEBUG", "cryptocustomrand=1"); err != nil {
		panic(err)
	}
	check := flag.Bool("check", false, "fail if the committed vector differs")
	flag.Parse()
	now := time.Date(2030, 1, 15, 12, 0, 0, 0, time.UTC)
	root, rootKey := makeRoot(now)
	validOpts := issuerOptions{
		pathLen: 0, pathLenZero: true, keyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		policy: pki.SelfHostedNodeIssuerPolicyOID, uri: "spiffe://" + trustDomain,
		notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(365 * 24 * time.Hour),
	}
	issuer, issuerKey := makeIssuer("self-hosted-node-issuer", now, root, rootKey, validOpts)
	cloudOpts := validOpts
	cloudOpts.policy = pki.CloudNodeIssuerPolicyOID
	cloudIssuer, cloudKey := makeIssuer("cloud-node-issuer", now, root, rootKey, cloudOpts)
	leaf := makeLeaf("valid", now, issuer, issuerKey)
	chain := pemCert(leaf) + pemCert(issuer)
	largeRevokedSerial := new(big.Int).Lsh(big.NewInt(1), 200)
	current := standardCRL("current", issuer, issuerKey, big.NewInt(1), now.Add(-time.Hour), now.Add(time.Hour),
		[]x509.RevocationListEntry{{SerialNumber: largeRevokedSerial, RevocationTime: now.Add(-2 * time.Hour)}})
	parsedCurrent, err := x509.ParseRevocationList(current)
	if err != nil || pki.VerifyCRL(parsedCurrent, issuer, now) != nil {
		panic("Go verifier rejected canonical browser CRL fixture")
	}
	validBundle := bundle{Class: "self-hosted", IssuerPEM: pemCert(issuer), CRLPEM: pemCRL(current)}
	cloudCRL := standardCRL("cloud-current", cloudIssuer, cloudKey, big.NewInt(1), now.Add(-time.Hour), now.Add(time.Hour), nil)
	cloudBundle := bundle{Class: "cloud", IssuerPEM: pemCert(cloudIssuer), CRLPEM: pemCRL(cloudCRL)}

	v := vectors{Now: now.Format(time.RFC3339), RootPEM: pemCert(root), ChainPEM: chain,
		Valid: []bundle{cloudBundle, validBundle}, Accepted: map[string]scenario{}, Scenarios: map[string]scenario{}}
	extraSANOpts := validOpts
	extraSANOpts.emails = []string{"crl-operator@spawnery.internal"}
	extraSANIssuer, extraSANKey := makeIssuer("issuer-extra-email-san", now, root, rootKey, extraSANOpts)
	extraSANLeaf := makeLeaf("issuer-extra-email-san", now, extraSANIssuer, extraSANKey)
	extraSANCRL := standardCRL("issuer-extra-email-san", extraSANIssuer, extraSANKey, big.NewInt(1), now.Add(-time.Hour), now.Add(time.Hour), nil)
	parsedExtraSANCRL, err := x509.ParseRevocationList(extraSANCRL)
	if err != nil || pki.VerifyCRL(parsedExtraSANCRL, extraSANIssuer, now) != nil {
		panic("Go verifier rejected issuer with an allowed non-URI SAN")
	}
	v.Accepted["issuer-extra-email-san"] = scenario{
		ChainPEM: pemCert(extraSANLeaf) + pemCert(extraSANIssuer),
		Bundles: []bundle{cloudBundle, {
			Class: "self-hosted", IssuerPEM: pemCert(extraSANIssuer), CRLPEM: pemCRL(extraSANCRL),
		}},
	}
	addCRL := func(name, want string, der []byte) {
		list, err := x509.ParseRevocationList(der)
		if err == nil && pki.VerifyCRL(list, issuer, now) == nil {
			panic(fmt.Errorf("Go verifier accepted negative %s", name))
		}
		bad := validBundle
		bad.CRLPEM = pemCRL(der)
		v.Scenarios[name] = scenario{ChainPEM: chain, Bundles: []bundle{cloudBundle, bad}, Error: want}
	}
	addIssuer := func(name, want string, opts issuerOptions) {
		badIssuer, badKey := makeIssuer(name, now, root, rootKey, opts)
		badLeaf := makeLeaf(name, now, badIssuer, badKey)
		badCRL := customCRL(name, badIssuer, badKey, now.Add(-time.Hour), now.Add(time.Hour), nil,
			[]pkix.Extension{akiExtension(badIssuer.SubjectKeyId), numberExtension(big.NewInt(1))})
		list, err := x509.ParseRevocationList(badCRL)
		if err != nil {
			panic(err)
		}
		// VerifyCRL derives its trust domain from the issuer. The browser receives the
		// configured domain explicitly, so only that layer can reject this mismatch.
		if err := pki.VerifyCRL(list, badIssuer, now); err == nil && name != "bad-trust-domain" {
			panic(fmt.Errorf("Go verifier accepted invalid issuer %s", name))
		}
		b := bundle{Class: "self-hosted", IssuerPEM: pemCert(badIssuer), CRLPEM: pemCRL(badCRL)}
		v.Scenarios[name] = scenario{ChainPEM: pemCert(badLeaf) + pemCert(badIssuer), Bundles: []bundle{cloudBundle, b}, Error: want}
	}

	validExts := []pkix.Extension{akiExtension(issuer.SubjectKeyId), numberExtension(big.NewInt(1))}
	addCRL("delta", "CRL extension", standardCRL("delta", issuer, issuerKey, big.NewInt(2), now.Add(-time.Hour), now.Add(time.Hour), nil,
		pkix.Extension{Id: oidDeltaCRL, Value: []byte{2, 1, 1}}))
	addCRL("indirect", "CRL extension", standardCRL("indirect", issuer, issuerKey, big.NewInt(3), now.Add(-time.Hour), now.Add(time.Hour), nil,
		pkix.Extension{Id: oidIssuingDP, Critical: true, Value: []byte{0x30, 0x03, 0x84, 0x01, 0xff}}))
	addCRL("unknown-extension", "CRL extension", standardCRL("unknown", issuer, issuerKey, big.NewInt(4), now.Add(-time.Hour), now.Add(time.Hour), nil,
		pkix.Extension{Id: oidUnknown, Value: []byte{5, 0}}))
	criticalAKI := akiExtension(issuer.SubjectKeyId)
	criticalAKI.Critical = true
	addCRL("critical-extension", "critical CRL extension", customCRL("critical", issuer, issuerKey, now.Add(-time.Hour), now.Add(time.Hour), nil,
		[]pkix.Extension{criticalAKI, numberExtension(big.NewInt(12))}))
	addCRL("missing-aki", "required extensions", customCRL("missing-aki", issuer, issuerKey, now.Add(-time.Hour), now.Add(time.Hour), nil, []pkix.Extension{validExts[1]}))
	addCRL("aki-mismatch", "authority key identifier", customCRL("aki-mismatch", issuer, issuerKey, now.Add(-time.Hour), now.Add(time.Hour), nil,
		[]pkix.Extension{akiExtension(bytes.Repeat([]byte{0x42}, len(issuer.SubjectKeyId))), validExts[1]}))
	addCRL("missing-number", "required extensions", customCRL("missing-number", issuer, issuerKey, now.Add(-time.Hour), now.Add(time.Hour), nil, []pkix.Extension{validExts[0]}))
	addCRL("zero-number", "CRL number", customCRL("zero-number", issuer, issuerKey, now.Add(-time.Hour), now.Add(time.Hour), nil,
		[]pkix.Extension{validExts[0], numberExtension(big.NewInt(0))}))
	oversized := new(big.Int).Lsh(big.NewInt(1), 159)
	addCRL("oversized-number", "CRL number", customCRL("oversized-number", issuer, issuerKey, now.Add(-time.Hour), now.Add(time.Hour), nil,
		[]pkix.Extension{validExts[0], numberExtension(oversized)}))
	addCRL("before-issuer-validity", "issuer validity", standardCRL("before-issuer", issuer, issuerKey, big.NewInt(5), validOpts.notBefore.Add(-time.Hour), now.Add(time.Hour), nil))
	addCRL("after-issuer-validity", "issuer validity", standardCRL("after-issuer", issuer, issuerKey, big.NewInt(6), now.Add(-time.Hour), validOpts.notAfter.Add(time.Hour), nil))
	addCRL("entry-extension", "entry extensions", standardCRL("entry-extension", issuer, issuerKey, big.NewInt(7), now.Add(-time.Hour), now.Add(time.Hour),
		[]x509.RevocationListEntry{{SerialNumber: big.NewInt(77), RevocationTime: now.Add(-2 * time.Hour), ReasonCode: 1}}))
	addCRL("duplicate-serial", "duplicate revoked", standardCRL("duplicate", issuer, issuerKey, big.NewInt(8), now.Add(-time.Hour), now.Add(time.Hour),
		[]x509.RevocationListEntry{{SerialNumber: big.NewInt(78), RevocationTime: now.Add(-2 * time.Hour)}, {SerialNumber: big.NewInt(78), RevocationTime: now.Add(-3 * time.Hour)}}))
	addCRL("nonpositive-serial", "serial must be positive", standardCRL("negative", issuer, issuerKey, big.NewInt(9), now.Add(-time.Hour), now.Add(time.Hour),
		[]x509.RevocationListEntry{{SerialNumber: big.NewInt(-1), RevocationTime: now.Add(-2 * time.Hour)}}))
	addCRL("future-revocation", "invalid revocation time", standardCRL("future-revocation", issuer, issuerKey, big.NewInt(10), now.Add(-time.Hour), now.Add(time.Hour),
		[]x509.RevocationListEntry{{SerialNumber: big.NewInt(79), RevocationTime: now}}))
	addCRL("missing-next-update", "update window", customCRL("missing-next-update", issuer, issuerKey, now.Add(-time.Hour), time.Time{}, nil, validExts))
	addCRL("invalid-update-window", "update window", customCRL("invalid-update-window", issuer, issuerKey, now.Add(-time.Hour), now.Add(-2*time.Hour), nil, validExts))
	addCRL("future-crl", "not yet valid", standardCRL("future-crl", issuer, issuerKey, big.NewInt(13), now.Add(time.Minute), now.Add(time.Hour), nil))
	addCRL("expired-crl", "expired", standardCRL("expired-crl", issuer, issuerKey, big.NewInt(14), now.Add(-2*time.Hour), now, nil))
	addCRL("wrong-raw-issuer", "issuer", customCRLWithRawIssuer("wrong-raw-issuer", root.RawSubject, issuerKey, now.Add(-time.Hour), now.Add(time.Hour), nil, validExts))
	addCRL("equivalent-rdn-different-der", "issuer", customCRLWithRawIssuer("equivalent-rdn-different-der",
		alternateCommonNameEncoding(issuer.RawSubject), issuerKey, now.Add(-time.Hour), now.Add(time.Hour), nil, validExts))
	addCRL("bad-signature", "signature", customCRL("bad-signature", issuer, rootKey, now.Add(-time.Hour), now.Add(time.Hour), nil, validExts))
	revoked := standardCRL("revoked", issuer, issuerKey, big.NewInt(11), now.Add(-time.Hour), now.Add(time.Hour),
		[]x509.RevocationListEntry{{SerialNumber: leaf.SerialNumber, RevocationTime: now.Add(-2 * time.Hour)}})
	badRevoked := validBundle
	badRevoked.CRLPEM = pemCRL(revoked)
	v.Scenarios["revoked"] = scenario{ChainPEM: chain, Bundles: []bundle{cloudBundle, badRevoked}, Error: "revoked"}

	delegating := validOpts
	delegating.pathLen, delegating.pathLenZero = 1, false
	addIssuer("delegating-issuer", "non-delegating", delegating)
	badUsage := validOpts
	badUsage.keyUsage = x509.KeyUsageCertSign
	addIssuer("bad-key-usage", "key usage", badUsage)
	badPolicy := validOpts
	badPolicy.policy = mustOID("1.3.6.1.4.1.55555.2")
	addIssuer("bad-role", "issuer role", badPolicy)
	badURI := validOpts
	badURI.uri = "spiffe://other.spawnery.internal"
	addIssuer("bad-trust-domain", "trust domain", badURI)
	uppercaseTrustDomain := "PROD.spawnery.internal"
	uppercaseURI := validOpts
	uppercaseURI.uri = "spiffe://" + uppercaseTrustDomain
	addIssuer("uppercase-configured-trust-domain", "trust domain", uppercaseURI)
	uppercaseScenario := v.Scenarios["uppercase-configured-trust-domain"]
	uppercaseScenario.TrustDomain = uppercaseTrustDomain
	v.Scenarios["uppercase-configured-trust-domain"] = uppercaseScenario
	emptySKID := validOpts
	emptySKID.emptySKID = true
	addIssuer("missing-skid", "invalid identity", emptySKID)

	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	out = append(out, '\n')
	if *check {
		committed, err := os.ReadFile(outputPath)
		if err != nil {
			panic(err)
		}
		if !bytes.Equal(committed, out) {
			panic("browser CRL vectors are stale; run generator without --check")
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(outputPath, out, 0o644); err != nil {
		panic(err)
	}
}
