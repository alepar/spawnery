package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestParsePrincipal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   string
		want Principal
	}{
		{
			name: "control plane service",
			id:   "spiffe://prod.spawnery.internal/service/cp/cp-a",
			want: Principal{TrustDomain: "prod.spawnery.internal", Kind: "service", Role: "cp", InstanceID: "cp-a"},
		},
		{
			name: "auth service",
			id:   "spiffe://prod.spawnery.internal/service/authsvc/as-a",
			want: Principal{TrustDomain: "prod.spawnery.internal", Kind: "service", Role: "authsvc", InstanceID: "as-a"},
		},
		{
			name: "cloud node",
			id:   "spiffe://prod.spawnery.internal/node/cloud/spawnery-system/n1",
			want: Principal{TrustDomain: "prod.spawnery.internal", Kind: "node", Role: "cloud", AccountID: "spawnery-system", NodeID: "n1"},
		},
		{
			name: "self-hosted node",
			id:   "spiffe://prod.spawnery.internal/node/self-hosted/acct-1/n2",
			want: Principal{TrustDomain: "prod.spawnery.internal", Kind: "node", Role: "self-hosted", AccountID: "acct-1", NodeID: "n2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := url.Parse(tt.id)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParsePrincipal(id, "prod.spawnery.internal")
			if err != nil {
				t.Fatalf("ParsePrincipal: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("principal = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParsePrincipalRejectsNonCanonicalIDs(t *testing.T) {
	t.Parallel()

	tests := []string{
		"spiffe://prod.spawnery.internal/service/cp/",
		"spiffe://prod.spawnery.internal/service/cp/cp%2Da",
		"spiffe://prod.spawnery.internal/service/cp/cp-a?x=1",
		"spiffe://prod.spawnery.internal/service/cp/cp-a#frag",
		"spiffe://user@prod.spawnery.internal/service/cp/cp-a",
		"spiffe://other.spawnery.internal/service/cp/cp-a",
		"spiffe://prod.spawnery.internal/service/cp/cp-a/extra",
		"spiffe://prod.spawnery.internal/service/other/x",
		"spiffe://prod.spawnery.internal/node/cloud/acct/node/extra",
		"spiffe://prod.spawnery.internal/node/self-hosted/acct/node:bad",
		"https://prod.spawnery.internal/service/cp/cp-a",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			id, err := url.Parse(raw)
			if err != nil {
				return
			}
			if _, err := ParsePrincipal(id, "prod.spawnery.internal"); err == nil {
				t.Fatalf("ParsePrincipal(%q) succeeded", raw)
			}
		})
	}
}

func TestPrincipalIDConstructors(t *testing.T) {
	t.Parallel()

	service, err := ServiceID("prod.spawnery.internal", "cp", "cp-a")
	if err != nil {
		t.Fatalf("ServiceID: %v", err)
	}
	if got, want := service.String(), "spiffe://prod.spawnery.internal/service/cp/cp-a"; got != want {
		t.Fatalf("ServiceID = %q, want %q", got, want)
	}
	node, err := NodeID("prod.spawnery.internal", "self-hosted", "acct-1", "n2")
	if err != nil {
		t.Fatalf("NodeID: %v", err)
	}
	if got, want := node.String(), "spiffe://prod.spawnery.internal/node/self-hosted/acct-1/n2"; got != want {
		t.Fatalf("NodeID = %q, want %q", got, want)
	}
	if _, err := ServiceID("prod.spawnery.internal", "other", "x"); err == nil {
		t.Fatal("ServiceID accepted unknown role")
	}
	if _, err := NodeID("prod.spawnery.internal", "cloud", "", "n"); err == nil {
		t.Fatal("NodeID accepted an empty account")
	}
}

func TestVerifyPrincipalValidIdentities(t *testing.T) {
	t.Parallel()

	now := time.Now()
	root, _ := NewRootCA("root")
	tests := []struct {
		name       string
		issuerRole IssuerRole
		issue      func(*CA) (*Leaf, error)
		want       Principal
	}{
		{
			name:       "cp",
			issuerRole: IssuerService,
			issue: func(ca *CA) (*Leaf, error) {
				return ca.IssueService(RoleCP, "cp-a", "prod.spawnery.internal", []string{"cp.internal"}, nil, now.Add(time.Hour))
			},
			want: Principal{TrustDomain: "prod.spawnery.internal", Kind: KindService, Role: RoleCP, InstanceID: "cp-a"},
		},
		{
			name:       "authsvc",
			issuerRole: IssuerService,
			issue: func(ca *CA) (*Leaf, error) {
				return ca.IssueService(RoleAuthService, "as-a", "prod.spawnery.internal", nil, nil, now.Add(time.Hour))
			},
			want: Principal{TrustDomain: "prod.spawnery.internal", Kind: KindService, Role: RoleAuthService, InstanceID: "as-a"},
		},
		{
			name:       "cloud node",
			issuerRole: IssuerCloudNode,
			issue: func(ca *CA) (*Leaf, error) {
				return ca.IssueNode("n1", "spawnery-system", RoleCloud, "prod.spawnery.internal", now.Add(time.Hour))
			},
			want: Principal{TrustDomain: "prod.spawnery.internal", Kind: KindNode, Role: RoleCloud, AccountID: "spawnery-system", NodeID: "n1"},
		},
		{
			name:       "self-hosted node",
			issuerRole: IssuerSelfHostedNode,
			issue: func(ca *CA) (*Leaf, error) {
				return ca.IssueNode("n2", "acct-1", RoleSelfHosted, "prod.spawnery.internal", now.Add(time.Hour))
			},
			want: Principal{TrustDomain: "prod.spawnery.internal", Kind: KindNode, Role: RoleSelfHosted, AccountID: "acct-1", NodeID: "n2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer, err := root.NewIntermediate(tt.issuerRole, "prod.spawnery.internal")
			if err != nil {
				t.Fatal(err)
			}
			leaf, err := tt.issue(issuer)
			if err != nil {
				t.Fatal(err)
			}
			got, err := VerifyPrincipal(leaf.Cert, leaf.Chain, verifyOptions(root.Cert, now))
			if err != nil {
				t.Fatalf("VerifyPrincipal: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("principal = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestVerifyPrincipalRejectsInvalidLeaf(t *testing.T) {
	now := time.Now()
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerSelfHostedNode, "prod.spawnery.internal")
	material, _ := issuer.IssueNode("n1", "acct-1", RoleSelfHosted, "prod.spawnery.internal", now.Add(time.Hour))
	otherRoot, _ := NewRootCA("other")
	otherIssuer, _ := otherRoot.NewIntermediate(IssuerSelfHostedNode, "prod.spawnery.internal")
	wrongRootLeaf, _ := otherIssuer.IssueNode("n1", "acct-1", RoleSelfHosted, "prod.spawnery.internal", now.Add(time.Hour))

	tests := []struct {
		name  string
		leaf  *x509.Certificate
		chain []*x509.Certificate
		opts  VerifyOptions
	}{
		{name: "wrong root", leaf: wrongRootLeaf.Cert, chain: wrongRootLeaf.Chain, opts: verifyOptions(root.Cert, now)},
		{name: "wrong trust domain", leaf: material.Cert, chain: material.Chain, opts: VerifyOptions{Root: root.Cert, TrustDomain: "staging.spawnery.internal", CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}},
		{name: "zero URI SANs", leaf: resignLeaf(t, issuer, material.Cert, func(c *x509.Certificate) { c.URIs = nil }), chain: material.Chain, opts: verifyOptions(root.Cert, now)},
		{name: "multiple URI SANs", leaf: resignLeaf(t, issuer, material.Cert, func(c *x509.Certificate) { c.URIs = append(c.URIs, c.URIs[0]) }), chain: material.Chain, opts: verifyOptions(root.Cert, now)},
		{name: "CA leaf", leaf: resignLeaf(t, issuer, material.Cert, func(c *x509.Certificate) { c.IsCA = true }), chain: material.Chain, opts: verifyOptions(root.Cert, now)},
		{name: "wrong key usage", leaf: resignLeaf(t, issuer, material.Cert, func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageKeyEncipherment }), chain: material.Chain, opts: verifyOptions(root.Cert, now)},
		{name: "wrong EKU", leaf: resignLeaf(t, issuer, material.Cert, func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} }), chain: material.Chain, opts: verifyOptions(root.Cert, now)},
		{name: "expired", leaf: resignLeaf(t, issuer, material.Cert, func(c *x509.Certificate) { c.NotBefore = now.Add(-2 * time.Hour); c.NotAfter = now.Add(-time.Hour) }), chain: material.Chain, opts: verifyOptions(root.Cert, now)},
		{name: "malformed path", leaf: resignLeaf(t, issuer, material.Cert, func(c *x509.Certificate) {
			c.URIs = []*url.URL{{Scheme: "spiffe", Host: "prod.spawnery.internal", Path: "/node/self-hosted/acct"}}
		}), chain: material.Chain, opts: verifyOptions(root.Cert, now)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := VerifyPrincipal(tt.leaf, tt.chain, tt.opts); err == nil {
				t.Fatal("invalid leaf accepted")
			}
		})
	}
}

func TestVerifyPrincipalRejectsIssuerPathMismatch(t *testing.T) {
	now := time.Now()
	root, _ := NewRootCA("root")
	tests := []struct {
		name       string
		issuerRole IssuerRole
		leafRole   string
		service    bool
	}{
		{name: "service issuer to cloud", issuerRole: IssuerService, leafRole: RoleCloud},
		{name: "cloud issuer to service", issuerRole: IssuerCloudNode, leafRole: RoleCP, service: true},
		{name: "cloud issuer to self-hosted", issuerRole: IssuerCloudNode, leafRole: RoleSelfHosted},
		{name: "self-hosted issuer to service", issuerRole: IssuerSelfHostedNode, leafRole: RoleAuthService, service: true},
		{name: "self-hosted issuer to cloud", issuerRole: IssuerSelfHostedNode, leafRole: RoleCloud},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer, _ := root.NewIntermediate(tt.issuerRole, "prod.spawnery.internal")
			var leaf *Leaf
			if tt.service {
				leaf, _ = issuer.IssueService(tt.leafRole, "instance", "prod.spawnery.internal", nil, nil, now.Add(time.Hour))
			} else {
				leaf, _ = issuer.IssueNode("node", "account", tt.leafRole, "prod.spawnery.internal", now.Add(time.Hour))
			}
			if _, err := VerifyPrincipal(leaf.Cert, leaf.Chain, verifyOptions(root.Cert, now)); err == nil {
				t.Fatal("issuer/path mismatch accepted")
			}
		})
	}
}

func TestVerifyPrincipalRejectsMissingOrUnknownIssuerRole(t *testing.T) {
	now := time.Now()
	root, _ := NewRootCA("root")
	unknown, _ := asn1.Marshal("unknown-issuer")
	for _, tt := range []struct {
		name       string
		extensions []pkix.Extension
	}{
		{name: "missing"},
		{name: "unknown", extensions: []pkix.Extension{{Id: issuerRoleOID, Value: unknown}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issuer := testIntermediate(t, root, tt.extensions)
			leaf, err := issuer.IssueNode("n", "a", RoleSelfHosted, "prod.spawnery.internal", now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyPrincipal(leaf.Cert, leaf.Chain, verifyOptions(root.Cert, now)); err == nil {
				t.Fatal("invalid issuer role accepted")
			}
		})
	}
}

func TestVerifyPrincipalRejectsRevokedLeaf(t *testing.T) {
	now := time.Now()
	root, _ := NewRootCA("root")
	issuer, _ := root.NewIntermediate(IssuerCloudNode, "prod.spawnery.internal")
	leaf, _ := issuer.IssueNode("n", "spawnery-system", RoleCloud, "prod.spawnery.internal", now.Add(time.Hour))
	opts := verifyOptions(root.Cert, now)
	opts.IsRevoked = func(gotIssuer, gotSerial *big.Int) bool {
		return gotIssuer.Cmp(issuer.Cert.SerialNumber) == 0 && gotSerial.Cmp(leaf.Cert.SerialNumber) == 0
	}
	if _, err := VerifyPrincipal(leaf.Cert, leaf.Chain, opts); err == nil {
		t.Fatal("revoked leaf accepted")
	}
}

func verifyOptions(root *x509.Certificate, now time.Time) VerifyOptions {
	return VerifyOptions{
		Root:        root,
		TrustDomain: "prod.spawnery.internal",
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
}

func resignLeaf(t *testing.T, issuer *CA, source *x509.Certificate, mutate func(*x509.Certificate)) *x509.Certificate {
	t.Helper()
	template := *source
	template.SerialNumber, _ = newSerial()
	template.URIs = append([]*url.URL(nil), source.URIs...)
	template.ExtKeyUsage = append([]x509.ExtKeyUsage(nil), source.ExtKeyUsage...)
	mutate(&template)
	der, err := x509.CreateCertificate(rand.Reader, &template, issuer.Cert, source.PublicKey, issuer.Key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func testIntermediate(t *testing.T, root *CA, extensions []pkix.Extension) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := newSerial()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "test intermediate"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		ExtraExtensions:       extensions,
	}
	return mustFinishCA(t, template, root, key)
}

func mustFinishCA(t *testing.T, template *x509.Certificate, root *CA, key *ecdsa.PrivateKey) *CA {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, root.Cert, key.Public(), root.Key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &CA{Cert: cert, Key: key}
}
