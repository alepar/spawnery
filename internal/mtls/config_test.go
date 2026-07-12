package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"spawnery/internal/pki"
)

const testTrustDomain = "prod.spawnery.internal"

type tlsFixture struct {
	now        time.Time
	root       *pki.CA
	serviceCA  *pki.CA
	cloudCA    *pki.CA
	selfHostCA *pki.CA
	cp         *pki.Leaf
	authsvc    *pki.Leaf
	cloud      *pki.Leaf
	selfHosted *pki.Leaf
}

func newTLSFixture(t *testing.T) *tlsFixture {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	root, err := pki.NewRootCA("test root")
	if err != nil {
		t.Fatal(err)
	}
	serviceCA, err := root.NewIntermediate(pki.IssuerService, testTrustDomain)
	if err != nil {
		t.Fatal(err)
	}
	cloudCA, err := root.NewIntermediate(pki.IssuerCloudNode, testTrustDomain)
	if err != nil {
		t.Fatal(err)
	}
	selfHostCA, err := root.NewIntermediate(pki.IssuerSelfHostedNode, testTrustDomain)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := serviceCA.IssueService(pki.RoleCP, "cp-1", testTrustDomain, []string{"cp.internal"}, nil, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	authsvc, err := serviceCA.IssueService(pki.RoleAuthService, "as-1", testTrustDomain, []string{"as.internal"}, nil, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	cloud, err := cloudCA.IssueNode("cloud-1", "spawnery-system", pki.RoleCloud, testTrustDomain, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	selfHosted, err := selfHostCA.IssueNode("node-1", "acct-1", pki.RoleSelfHosted, testTrustDomain, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return &tlsFixture{
		now:        now,
		root:       root,
		serviceCA:  serviceCA,
		cloudCA:    cloudCA,
		selfHostCA: selfHostCA,
		cp:         cp,
		authsvc:    authsvc,
		cloud:      cloud,
		selfHosted: selfHosted,
	}
}

func TestClientTLSAcceptsNamedExpectedServiceAndCallerCertificate(t *testing.T) {
	t.Parallel()
	f := newTLSFixture(t)
	clientConfig, err := ClientConfig(ClientOptions{
		Root:                f.root.Cert,
		TrustDomain:         testTrustDomain,
		Identity:            tlsCertificate(t, f.selfHosted),
		ServerName:          "cp.internal",
		ExpectedServiceRole: pki.RoleCP,
		CurrentTime:         func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	serverConfig := standardServerConfig(t, f.root.Cert, f.cp)

	clientErr, serverErr := handshake(clientConfig, serverConfig)
	if clientErr != nil || serverErr != nil {
		t.Fatalf("handshake errors: client=%v server=%v", clientErr, serverErr)
	}
}

func TestClientTLSRejectsMissingServerName(t *testing.T) {
	t.Parallel()
	f := newTLSFixture(t)
	_, err := ClientConfig(ClientOptions{
		Root:                f.root.Cert,
		TrustDomain:         testTrustDomain,
		Identity:            tlsCertificate(t, f.selfHosted),
		ExpectedServiceRole: pki.RoleCP,
	})
	if err == nil || !strings.Contains(err.Error(), "server name") {
		t.Fatalf("ClientConfig error = %v, want server-name error", err)
	}
}

func TestClientTLSRejectsInvalidPeer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		configure func(*testing.T, *tlsFixture, *ClientOptions) (*pki.Leaf, *x509.Certificate)
	}{
		{
			name: "wrong DNS SAN",
			configure: func(_ *testing.T, f *tlsFixture, opts *ClientOptions) (*pki.Leaf, *x509.Certificate) {
				opts.ServerName = "wrong.internal"
				return f.cp, f.root.Cert
			},
		},
		{
			name: "wrong root",
			configure: func(t *testing.T, f *tlsFixture, _ *ClientOptions) (*pki.Leaf, *x509.Certificate) {
				other := newTLSFixture(t)
				return other.cp, f.root.Cert
			},
		},
		{
			name: "wrong trust domain",
			configure: func(_ *testing.T, f *tlsFixture, opts *ClientOptions) (*pki.Leaf, *x509.Certificate) {
				opts.TrustDomain = "staging.spawnery.internal"
				return f.cp, f.root.Cert
			},
		},
		{
			name: "node as service",
			configure: func(_ *testing.T, f *tlsFixture, _ *ClientOptions) (*pki.Leaf, *x509.Certificate) {
				return f.cloud, f.root.Cert
			},
		},
		{
			name: "wrong service role",
			configure: func(_ *testing.T, f *tlsFixture, _ *ClientOptions) (*pki.Leaf, *x509.Certificate) {
				return f.authsvc, f.root.Cert
			},
		},
		{
			name: "expired",
			configure: func(_ *testing.T, f *tlsFixture, opts *ClientOptions) (*pki.Leaf, *x509.Certificate) {
				opts.CurrentTime = func() time.Time { return f.now.Add(2 * time.Hour) }
				return f.cp, f.root.Cert
			},
		},
		{
			name: "revoked",
			configure: func(_ *testing.T, f *tlsFixture, opts *ClientOptions) (*pki.Leaf, *x509.Certificate) {
				opts.IsRevoked = func(_, serial *big.Int) bool { return serial.Cmp(f.cp.Cert.SerialNumber) == 0 }
				return f.cp, f.root.Cert
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newTLSFixture(t)
			opts := ClientOptions{
				Root:                f.root.Cert,
				TrustDomain:         testTrustDomain,
				Identity:            tlsCertificate(t, f.selfHosted),
				ServerName:          "cp.internal",
				ExpectedServiceRole: pki.RoleCP,
				CurrentTime:         func() time.Time { return f.now },
			}
			serverIdentity, serverRoot := tt.configure(t, f, &opts)
			clientConfig, err := ClientConfig(opts)
			if err != nil {
				t.Fatalf("ClientConfig: %v", err)
			}
			serverConfig := standardServerConfig(t, serverRoot, serverIdentity)

			clientErr, serverErr := handshake(clientConfig, serverConfig)
			if clientErr == nil && serverErr == nil {
				t.Fatal("invalid peer completed the TLS handshake")
			}
		})
	}
}

func TestClientTLSRejectsMissingCallerCertificate(t *testing.T) {
	t.Parallel()
	f := newTLSFixture(t)
	clientConfig, err := ClientConfig(ClientOptions{
		Root:                f.root.Cert,
		TrustDomain:         testTrustDomain,
		ServerName:          "cp.internal",
		ExpectedServiceRole: pki.RoleCP,
		CurrentTime:         func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}

	clientErr, serverErr := handshake(clientConfig, standardServerConfig(t, f.root.Cert, f.cp))
	if clientErr == nil && serverErr == nil {
		t.Fatal("client without a certificate completed the TLS handshake")
	}
}

func standardServerConfig(t *testing.T, root *x509.Certificate, identity *pki.Leaf) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(root)
	return &tls.Config{
		Certificates: []tls.Certificate{tlsCertificate(t, identity)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}
}

func tlsCertificate(t *testing.T, identity *pki.Leaf) tls.Certificate {
	t.Helper()
	certificate, err := identity.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func handshake(clientConfig, serverConfig *tls.Config) (clientErr, serverErr error) {
	clientConn, serverConn := net.Pipe()
	clientTLS := tls.Client(clientConn, clientConfig)
	serverTLS := tls.Server(serverConn, serverConfig)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serverTLS.Handshake()
	}()
	clientErr = clientTLS.Handshake()
	_ = clientTLS.Close()
	serverErr = <-serverDone
	_ = serverTLS.Close()
	return clientErr, serverErr
}
