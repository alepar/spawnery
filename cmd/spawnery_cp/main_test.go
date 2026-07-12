package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	configfiles "spawnery/config"
	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/gen/node/v1/nodev1connect"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/config"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/nodeauth"
	"spawnery/internal/mtls"
	"spawnery/internal/pki"
)

type publicAuthTestHandler struct {
	cpv1connect.UnimplementedSpawnServiceHandler
	listCalls      int
	authorizeCalls int
}

func (h *publicAuthTestHandler) ListSpawns(context.Context, *connect.Request[cpv1.ListSpawnsRequest]) (*connect.Response[cpv1.ListSpawnsResponse], error) {
	h.listCalls++
	return connect.NewResponse(&cpv1.ListSpawnsResponse{}), nil
}

func (h *publicAuthTestHandler) AuthorizeGitHubMint(context.Context, *connect.Request[cpv1.AuthorizeGitHubMintRequest]) (*connect.Response[cpv1.AuthorizeGitHubMintResponse], error) {
	h.authorizeCalls++
	return connect.NewResponse(&cpv1.AuthorizeGitHubMintResponse{}), nil
}

type bearerClientInterceptor struct{ token string }

func (i bearerClientInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+i.token)
		return next(ctx, req)
	}
}
func (bearerClientInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (bearerClientInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func TestCPPublicHandlerPreservesBearerAndRejectsInternalProcedures(t *testing.T) {
	h := &publicAuthTestHandler{}
	verifier := auth.NewVerifier(auth.VerifierConfig{DevMode: true, DevTokens: map[string]string{"user-token": "acct"}})
	_, handler := cpv1connect.NewSpawnServiceHandler(h, connect.WithInterceptors(publicAuthInterceptor(verifier)))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	client := cpv1connect.NewSpawnServiceClient(ts.Client(), ts.URL, connect.WithInterceptors(bearerClientInterceptor{token: "user-token"}))
	if _, err := client.ListSpawns(t.Context(), connect.NewRequest(&cpv1.ListSpawnsRequest{})); err != nil {
		t.Fatalf("public bearer request: %v", err)
	}
	if h.listCalls != 1 {
		t.Fatalf("ListSpawns calls=%d", h.listCalls)
	}
	if _, err := client.AuthorizeGitHubMint(t.Context(), connect.NewRequest(&cpv1.AuthorizeGitHubMintRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("public internal procedure code=%v err=%v", connect.CodeOf(err), err)
	}
	if h.authorizeCalls != 0 {
		t.Fatal("public internal procedure reached handler")
	}
}

func TestCPInternalHandlerPrincipalRouteMatrix(t *testing.T) {
	const td = "prod.spawnery.internal"
	root, err := pki.NewRootCA("route matrix root")
	if err != nil {
		t.Fatal(err)
	}
	serviceIssuer, _ := root.NewIntermediate(pki.IssuerService, td)
	nodeIssuer, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, td)
	authsvc, _ := serviceIssuer.IssueService(pki.RoleAuthService, "as-1", td, nil, nil, time.Now().Add(time.Hour))
	cpService, _ := serviceIssuer.IssueService(pki.RoleCP, "cp-2", td, nil, nil, time.Now().Add(time.Hour))
	node, _ := nodeIssuer.IssueNode("node-1", "acct-1", pki.RoleSelfHosted, td, time.Now().Add(time.Hour))
	verifier, err := mtls.NewPeerVerifier(mtls.PeerVerifierOptions{Root: root.Cert, TrustDomain: td, CurrentTime: time.Now, IsRevoked: func(*big.Int, *big.Int) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}

	spawnCalls, nodeCalls := 0, 0
	spawnHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { spawnCalls++; w.WriteHeader(http.StatusNoContent) })
	nodeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeCalls++
		if principal, ok := nodeauth.IdentityFromContext(r.Context()); !ok || principal.NodeID != "node-1" {
			t.Errorf("node identity missing: %+v %v", principal, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h := buildInternalHandler(verifier, spawnHandler, nodeHandler)
	chain := func(leaf *pki.Leaf) *tls.ConnectionState {
		return &tls.ConnectionState{HandshakeComplete: true, Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{leaf.Cert, leaf.Chain[0]}}
	}
	tests := []struct {
		name       string
		path       string
		peer       *pki.Leaf
		want       int
		spawnDelta int
		nodeDelta  int
	}{
		{"authsvc authorize", cpv1connect.SpawnServiceAuthorizeGitHubMintProcedure, authsvc, http.StatusNoContent, 1, 0},
		{"authsvc rotation", cpv1connect.SpawnServiceSignalGitHubTokenRotatedProcedure, authsvc, http.StatusNoContent, 1, 0},
		{"authsvc node denied", nodev1connect.NodeServiceAttachProcedure, authsvc, http.StatusForbidden, 0, 0},
		{"node attach", nodev1connect.NodeServiceAttachProcedure, node, http.StatusNoContent, 0, 1},
		{"node service denied", cpv1connect.SpawnServiceAuthorizeGitHubMintProcedure, node, http.StatusForbidden, 0, 0},
		{"cp denied", cpv1connect.SpawnServiceAuthorizeGitHubMintProcedure, cpService, http.StatusForbidden, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beforeSpawn, beforeNode := spawnCalls, nodeCalls
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			req.TLS = chain(tt.peer)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want || spawnCalls-beforeSpawn != tt.spawnDelta || nodeCalls-beforeNode != tt.nodeDelta {
				t.Fatalf("code=%d calls=%d/%d", rec.Code, spawnCalls-beforeSpawn, nodeCalls-beforeNode)
			}
		})
	}
}

func TestCPInternalTLSServerRequiresClientSVIDAndNegotiatesHTTP2(t *testing.T) {
	cfg, root, serviceIssuer, nodeIssuer := writeCPInternalFixture(t)
	runtime, err := loadInternalRuntime(cfg, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.revocations.Close() })
	server, err := buildInternalTLSServer("", runtime.tlsConfig, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(tls.NewListener(listener, runtime.tlsConfig)) }()

	node, err := nodeIssuer.IssueNode("node-1", "acct-1", pki.RoleSelfHosted, cfg.Internal.TrustDomain, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	nodeIdentity, _ := node.TLSCertificate()
	clientTLS, err := mtls.ClientConfig(mtls.ClientOptions{
		Root: root.Cert, TrustDomain: cfg.Internal.TrustDomain, Identity: nodeIdentity,
		ServerName: "cp.internal", ExpectedServiceRole: pki.RoleCP, CurrentTime: time.Now,
		IsRevoked: runtime.revocations.IsRevoked,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS, ForceAttemptHTTP2: true}}
	resp, err := client.Get("https://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.ProtoMajor != 2 {
		t.Fatalf("status=%d protocol=%s", resp.StatusCode, resp.Proto)
	}
	if runtime.tlsConfig.ClientAuth != tls.RequireAnyClientCert || len(runtime.tlsConfig.Certificates) != 1 {
		t.Fatalf("unexpected server TLS policy: auth=%v certificates=%d", runtime.tlsConfig.ClientAuth, len(runtime.tlsConfig.Certificates))
	}
	leaf, err := x509.ParseCertificate(runtime.tlsConfig.Certificates[0].Certificate[0])
	if err != nil || len(leaf.URIs) != 1 || leaf.URIs[0].Path != "/service/cp/cp-1" {
		t.Fatalf("server identity=%v err=%v", leaf, err)
	}

	noIdentityTLS, err := mtls.ClientConfig(mtls.ClientOptions{
		Root: root.Cert, TrustDomain: cfg.Internal.TrustDomain, ServerName: "cp.internal",
		ExpectedServiceRole: pki.RoleCP, CurrentTime: time.Now, IsRevoked: runtime.revocations.IsRevoked,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&http.Client{Transport: &http.Transport{TLSClientConfig: noIdentityTLS}}).Get("https://" + listener.Addr().String())
	if err == nil {
		t.Fatal("internal listener accepted a client without an SVID")
	}
	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if serveErr := <-done; serveErr != nil && serveErr != http.ErrServerClosed {
		t.Fatal(serveErr)
	}
	_ = serviceIssuer
}

func TestCPInternalRuntimeRefreshesSignedCRLsAndFailsClosedAtStartup(t *testing.T) {
	cfg, _, serviceIssuer, _ := writeCPInternalFixture(t)
	runtime, err := loadInternalRuntime(cfg, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	leafRaw, _ := os.ReadFile(cfg.Internal.Cert)
	leaf, _ := pki.ParseCertPEM(leafRaw)
	now := time.Now()
	updated, err := serviceIssuer.CreateCRL(big.NewInt(2), []x509.RevocationListEntry{{SerialNumber: leaf.SerialNumber, RevocationTime: now}}, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Internal.RevocationCRLs[0], pki.MarshalCRLPEM(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		refreshCertificateRevocations(ctx, runtime.revocations, cfg.Internal.RevocationCRLs, time.Millisecond)
	}()
	deadline := time.Now().Add(time.Second)
	for !runtime.revocations.IsRevoked(serviceIssuer.Cert.SerialNumber, leaf.SerialNumber) {
		if time.Now().After(deadline) {
			t.Fatal("refreshed CRL was not published")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if err := runtime.revocations.Close(); err != nil {
		t.Fatal(err)
	}

	missing := cfg
	missing.Internal.RevocationState = filepath.Join(filepath.Dir(cfg.Internal.RevocationState), "missing-state.json")
	missing.Internal.RevocationCRLs = []string{filepath.Join(filepath.Dir(cfg.Internal.RevocationState), "missing.crl")}
	if _, err := loadInternalRuntime(missing, time.Now); err == nil {
		t.Fatal("startup accepted missing certificate CRL material")
	}

	otherRoot, _ := pki.NewRootCA("different artifact root")
	otherRootPath := filepath.Join(filepath.Dir(cfg.Internal.RootCA), "other-root.pem")
	if err := os.WriteFile(otherRootPath, pki.MarshalCertPEM(otherRoot.Cert), 0o600); err != nil {
		t.Fatal(err)
	}
	mismatch := cfg
	mismatch.Auth.RootCA = otherRootPath
	mismatch.Internal.RevocationState = filepath.Join(filepath.Dir(cfg.Internal.RevocationState), "mismatch-state.json")
	if _, err := loadInternalRuntime(mismatch, time.Now); err == nil || !strings.Contains(err.Error(), "share the environment root") {
		t.Fatalf("separate artifact/transport roots accepted: %v", err)
	}
}

func TestCPInternalClientPresentsCPAndRequiresAuthServiceRole(t *testing.T) {
	cfg, root, serviceIssuer, _ := writeCPInternalFixture(t)
	runtime, err := loadInternalRuntime(cfg, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.revocations.Close() })
	serve := func(role string, reached *bool) *httptest.Server {
		leaf, issueErr := serviceIssuer.IssueService(role, "peer-1", cfg.Internal.TrustDomain, []string{cfg.Internal.ServerName}, nil, time.Now().Add(time.Hour))
		if issueErr != nil {
			t.Fatal(issueErr)
		}
		identity, _ := leaf.TLSCertificate()
		peerVerifier, verifyErr := mtls.NewPeerVerifier(mtls.PeerVerifierOptions{Root: root.Cert, TrustDomain: cfg.Internal.TrustDomain, CurrentTime: time.Now, IsRevoked: runtime.revocations.IsRevoked})
		if verifyErr != nil {
			t.Fatal(verifyErr)
		}
		serverTLS, configErr := mtls.ServerConfig(mtls.ServerOptions{Verifier: peerVerifier, Identity: identity, ClientMode: mtls.RequireClientCertificate})
		if configErr != nil {
			t.Fatal(configErr)
		}
		ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*reached = true
			if len(r.TLS.PeerCertificates) == 0 || len(r.TLS.PeerCertificates[0].URIs) != 1 || r.TLS.PeerCertificates[0].URIs[0].Path != "/service/cp/cp-1" {
				t.Errorf("CP client identity was not presented: %+v", r.TLS.PeerCertificates)
			}
			if r.Header.Get("Authorization") != "" || r.Header.Get("X-Spawnery-AS-"+"Secret") != "" {
				t.Error("CP internal client sent a retired service credential")
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		ts.TLS = serverTLS
		ts.StartTLS()
		t.Cleanup(ts.Close)
		return ts
	}

	var authsvcReached bool
	authsvc := serve(pki.RoleAuthService, &authsvcReached)
	resp, err := runtime.client.Get(authsvc.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || !authsvcReached {
		t.Fatalf("status=%d reached=%v", resp.StatusCode, authsvcReached)
	}

	var wrongRoleReached bool
	wrongRole := serve(pki.RoleCP, &wrongRoleReached)
	if _, err := runtime.client.Get(wrongRole.URL); err == nil {
		t.Fatal("CP client accepted a DNS-valid server with the CP role")
	}
	if wrongRoleReached {
		t.Fatal("wrong-role AS server reached its handler")
	}
}

func writeCPInternalFixture(t *testing.T) (CP, *pki.CA, *pki.CA, *pki.CA) {
	t.Helper()
	const td = "prod.spawnery.internal"
	now := time.Now()
	root, _ := pki.NewRootCA("internal fixture root")
	serviceIssuer, _ := root.NewIntermediate(pki.IssuerService, td)
	nodeIssuer, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, td)
	cpLeaf, _ := serviceIssuer.IssueService(pki.RoleCP, "cp-1", td, []string{"cp.internal"}, nil, now.Add(time.Hour))
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	keyPEM, _ := pki.MarshalKeyPEM(cpLeaf.Key)
	serviceCRL, _ := serviceIssuer.CreateCRL(big.NewInt(1), nil, now.Add(-time.Minute), now.Add(time.Hour))
	nodeCRL, _ := nodeIssuer.CreateCRL(big.NewInt(1), nil, now.Add(-time.Minute), now.Add(time.Hour))
	rootPath := write("root.pem", pki.MarshalCertPEM(root.Cert))
	var cfg CP
	cfg.Auth.RootCA = rootPath
	cfg.Internal.Listen = "127.0.0.1:0"
	cfg.Internal.TrustDomain = td
	cfg.Internal.RootCA = rootPath
	cfg.Internal.Cert = write("cp.pem", pki.MarshalCertPEM(cpLeaf.Cert))
	cfg.Internal.Chain = write("service-issuer.pem", pki.MarshalCertPEM(serviceIssuer.Cert))
	cfg.Internal.Key = write("cp-key.pem", keyPEM)
	cfg.Internal.ServerName = "authsvc.internal"
	cfg.Internal.RevocationState = filepath.Join(dir, "certificate-revocations.json")
	cfg.Internal.RevocationIssuers = []string{cfg.Internal.Chain, write("node-issuer.pem", pki.MarshalCertPEM(nodeIssuer.Cert))}
	cfg.Internal.RevocationCRLs = []string{write("service.crl", pki.MarshalCRLPEM(serviceCRL)), write("node.crl", pki.MarshalCRLPEM(nodeCRL))}
	return cfg, root, serviceIssuer, nodeIssuer
}

func loadCPTest(t *testing.T, env string, getenv map[string]string, sets ...string) (*CP, error) {
	t.Helper()
	return config.Load[CP]("cp", config.Options{
		Args:       []string{"--env=" + env},
		Getenv:     func(k string) (string, bool) { v, ok := getenv[k]; return v, ok },
		Embedded:   configfiles.FS,
		SecretsFS:  configfiles.FS,
		EnvAliases: cpEnvAliases,
		Sets:       sets,
	})
}

func TestCPConfig_Defaults(t *testing.T) {
	cfg, err := loadCPTest(t, "dev", nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.Store.Driver != "sqlite" || string(cfg.Store.DSN) != sqliteDefaultDSN {
		t.Errorf("Store = %s/%s", cfg.Store.Driver, string(cfg.Store.DSN))
	}
	if !cfg.DevMode() {
		t.Error("expected dev mode by default")
	}
	if cfg.MaxSpawnsPerOwner != 5 {
		t.Errorf("MaxSpawnsPerOwner = %d, want 5", cfg.MaxSpawnsPerOwner)
	}
	if cfg.Evaluator.IdleDetached != 15*time.Minute || cfg.Evaluator.IdleAttached != 60*time.Minute {
		t.Errorf("evaluator idle defaults = %s/%s", cfg.Evaluator.IdleDetached, cfg.Evaluator.IdleAttached)
	}
	if cfg.Auth.RevocationPollInterval != 30*time.Second {
		t.Errorf("revocation_poll_interval = %s, want 30s", cfg.Auth.RevocationPollInterval)
	}
	if cfg.Internal.Listen != "" {
		t.Errorf("internal.listen = %q, want disabled in base dev config", cfg.Internal.Listen)
	}
	if cfg.Internal.InsecureDevNodeOnPublic {
		t.Fatal("insecure public node route must default off")
	}
}

func TestCPConfig_InsecureDevNodeOnPublicRequiresDevLoopback(t *testing.T) {
	if _, err := loadCPTest(t, "dev", nil, "internal.insecure_dev_node_on_public=true"); err != nil {
		t.Fatalf("explicit loopback dev lane: %v", err)
	}
	for _, listen := range []string{"0.0.0.0:8080", "[::]:8080", "192.168.1.20:8080", "localhost:8080", "cp.internal:8080"} {
		_, err := loadCPTest(t, "dev", nil, "internal.insecure_dev_node_on_public=true", "listen="+listen)
		if err == nil {
			t.Fatalf("listen %q accepted", listen)
		}
	}
	if _, err := loadCPTest(t, "dev", nil, "internal.insecure_dev_node_on_public=true", "auth.mode=prod"); err == nil || !strings.Contains(err.Error(), "auth.mode=dev") {
		t.Fatalf("production insecure node route: %v", err)
	}
}

func TestMountInsecureDevNodeRouteDefaultsAbsent(t *testing.T) {
	called := false
	node := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	for _, test := range []struct {
		name    string
		enabled bool
		want    int
	}{
		{name: "default absent", enabled: false, want: http.StatusNotFound},
		{name: "explicit enabled", enabled: true, want: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			called = false
			mux := http.NewServeMux()
			mountInsecureDevNodeRoute(mux, test.enabled, nodev1connect.NodeServiceAttachProcedure, node)
			req := httptest.NewRequest(http.MethodPost, nodev1connect.NodeServiceAttachProcedure, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != test.want || called != test.enabled {
				t.Fatalf("code=%d called=%v", rec.Code, called)
			}
		})
	}
}

func TestCPConfig_EnvAliasOverride(t *testing.T) {
	cfg, err := loadCPTest(t, "dev", map[string]string{
		"CP_LISTEN":               "0.0.0.0:9000",
		"CP_MAX_SPAWNS_PER_OWNER": "9",
		"EVALUATOR_IDLE_DETACHED": "5m",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "0.0.0.0:9000" {
		t.Errorf("Listen = %q (env alias should win over file)", cfg.Listen)
	}
	if cfg.MaxSpawnsPerOwner != 9 {
		t.Errorf("MaxSpawnsPerOwner = %d, want 9 (string env coerced to int)", cfg.MaxSpawnsPerOwner)
	}
	if cfg.Evaluator.IdleDetached != 5*time.Minute {
		t.Errorf("IdleDetached = %s, want 5m", cfg.Evaluator.IdleDetached)
	}
}

func TestCPConfig_SetOverride(t *testing.T) {
	cfg, err := loadCPTest(t, "dev", nil, "allowed_origins=https://app.example.test")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.AllowedOrigins != "https://app.example.test" {
		t.Errorf("allowed_origins = %q, want override", cfg.AllowedOrigins)
	}
}

func TestCPConfig_ProdModeRequiresRootArtifactTrust(t *testing.T) {
	tests := []struct {
		name string
		sets []string
		want string
	}{
		{name: "all missing", sets: []string{"auth.mode=prod"}, want: "auth.environment"},
		{name: "root missing", sets: []string{"auth.mode=prod", "auth.environment=prod"}, want: "auth.root_ca"},
		{name: "state missing", sets: []string{"auth.mode=prod", "auth.environment=prod", "auth.root_ca=/root.pem"}, want: "auth.signer_revocation_state"},
		{name: "legacy raw key does not count", sets: []string{"auth.mode=prod", "auth.as_session_" + "pubkeys=/as.pub"}, want: "auth.environment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadCPTest(t, "dev", nil, tt.sets...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %s validation error, got %v", tt.want, err)
			}
		})
	}

	if _, err := loadCPTest(t, "dev", nil,
		"auth.mode=prod",
		"auth.environment=prod",
		"auth.root_ca=/root.pem",
		"auth.signer_revocation_state=/state/revocations.json",
		"internal.listen=127.0.0.1:8081",
		"internal.trust_domain=prod.spawnery.internal",
		"internal.root_ca=/root.pem",
		"internal.cert=/cp.pem",
		"internal.chain=/service-issuer.pem",
		"internal.key=/cp-key.pem",
		"internal.revocation_state=/state/certificates.json",
		"internal.revocation_issuers=/service-issuer.pem",
		"internal.revocation_crls=/service.crl",
	); err != nil {
		t.Fatalf("valid production config: %v", err)
	}
}

func TestCPConfig_ProdModeRequiresInternalMTLS(t *testing.T) {
	base := []string{
		"auth.mode=prod",
		"auth.environment=prod",
		"auth.root_ca=/root.pem",
		"auth.signer_revocation_state=/state/signer.json",
	}
	required := []struct {
		set  string
		want string
	}{
		{"internal.listen=127.0.0.1:8081", "internal.listen"},
		{"internal.trust_domain=prod.spawnery.internal", "internal.trust_domain"},
		{"internal.root_ca=/root.pem", "internal.root_ca"},
		{"internal.cert=/cp.pem", "internal.cert"},
		{"internal.chain=/service-issuer.pem", "internal.chain"},
		{"internal.key=/cp-key.pem", "internal.key"},
		{"internal.revocation_state=/state/certificates.json", "internal.revocation_state"},
		{"internal.revocation_issuers=/service-issuer.pem", "internal.revocation_issuers"},
		{"internal.revocation_crls=/service.crl", "internal.revocation_crls"},
	}
	sets := append([]string(nil), base...)
	for _, step := range required {
		_, err := loadCPTest(t, "dev", nil, sets...)
		if err == nil || !strings.Contains(err.Error(), step.want) {
			t.Fatalf("before %s: got %v", step.want, err)
		}
		sets = append(sets, step.set)
	}
	if _, err := loadCPTest(t, "dev", nil, sets...); err != nil {
		t.Fatalf("complete internal mTLS config: %v", err)
	}
}

func TestCPConfig_ASInternalURLRequiresServerName(t *testing.T) {
	for _, key := range []string{"auth.as_url", "auth.as_revocation_url"} {
		_, err := loadCPTest(t, "dev", nil, key+"=https://authsvc.internal")
		if err == nil || !strings.Contains(err.Error(), "internal.server_name") {
			t.Fatalf("%s without server name: %v", key, err)
		}
	}
}

func TestCPConfig_LegacyServiceSecretAliasesAreRejected(t *testing.T) {
	for _, name := range []string{"CP_AS_" + "RPC_SECRET", "CP_AS_" + "CP_SECRET"} {
		if _, ok := cpEnvAliases[name]; ok {
			t.Fatalf("legacy alias %s is still represented", name)
		}
	}
}

func TestCPConfig_ArtifactTrustEnvAliases(t *testing.T) {
	cfg, err := loadCPTest(t, "dev", map[string]string{
		"CP_AUTH_ENVIRONMENT":                 "prod",
		"CP_AUTH_ROOT_CA":                     "/root.pem",
		"CP_AUTH_SIGNER_REVOCATION_STATEMENT": "/statement",
		"CP_AUTH_SIGNER_REVOCATION_STATE":     "/state",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Environment != "prod" || cfg.Auth.RootCA != "/root.pem" ||
		cfg.Auth.SignerRevocationStatement != "/statement" || cfg.Auth.SignerRevocationState != "/state" {
		t.Fatalf("artifact trust aliases were not applied: %+v", cfg.Auth)
	}
}

func TestLoadArtifactVerifierGenerationZeroAndStoreLifetime(t *testing.T) {
	root, err := pki.NewRootCA("CP artifact test root")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "root.pem")
	statePath := filepath.Join(dir, "state", "signer-revocations.json")
	if err := os.WriteFile(rootPath, pki.MarshalCertPEM(root.Cert), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := CP{}
	cfg.Auth.Environment = "prod"
	cfg.Auth.RootCA = rootPath
	cfg.Auth.SignerRevocationState = statePath
	verifier, store, err := loadArtifactVerifier(cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if verifier == nil || store == nil || store.Generation() != 0 {
		t.Fatalf("verifier=%v store=%v", verifier, store)
	}
	if _, _, err := loadArtifactVerifier(cfg, time.Now()); err == nil {
		t.Fatal("second store owner should be rejected")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, reopened, err := loadArtifactVerifier(cfg, time.Now())
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadArtifactVerifierRejectsMalformedRoot(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "root.pem")
	if err := os.WriteFile(rootPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := CP{}
	cfg.Auth.Environment = "prod"
	cfg.Auth.RootCA = rootPath
	cfg.Auth.SignerRevocationState = filepath.Join(dir, "state")
	if _, _, err := loadArtifactVerifier(cfg, time.Now()); err == nil || !strings.Contains(err.Error(), "parse root") {
		t.Fatalf("expected malformed root rejection, got %v", err)
	}
}

func TestParseSingleRootCertificateRejectsMultipleCertificates(t *testing.T) {
	root, err := pki.NewRootCA("root")
	if err != nil {
		t.Fatal(err)
	}
	pem := pki.MarshalCertPEM(root.Cert)
	if _, err := parseSingleRootCertificate(append(append([]byte(nil), pem...), pem...)); err == nil {
		t.Fatal("accepted multiple root certificates")
	}
}

func TestLoadArtifactVerifierRejectsWrongEnvironmentAndRollbackStatement(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	root, err := pki.NewRootCA("root")
	if err != nil {
		t.Fatal(err)
	}
	intermediateKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "auth signing"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true,
		Policies: []x509.OID{pki.AuthSigningIntermediatePolicyOID},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, root.Cert, &intermediateKey.PublicKey, root.Key)
	if err != nil {
		t.Fatal(err)
	}
	intermediate, _ := x509.ParseCertificate(der)
	sign := func(environment string, generation uint64) string {
		payload, err := proto.Marshal(&authv1.SignerRevocationStatement{Environment: environment, Generation: generation, IssuedAt: now.Unix()})
		if err != nil {
			t.Fatal(err)
		}
		wire, err := token.SignSignerRevocationStatement(intermediate, intermediateKey, payload)
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	dir := t.TempDir()
	rootPath, statePath, statementPath := filepath.Join(dir, "root.pem"), filepath.Join(dir, "state", "revocations.json"), filepath.Join(dir, "statement")
	if err := os.WriteFile(rootPath, pki.MarshalCertPEM(root.Cert), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := CP{}
	cfg.Auth.Environment, cfg.Auth.RootCA, cfg.Auth.SignerRevocationState, cfg.Auth.SignerRevocationStatement = "prod", rootPath, statePath, statementPath
	if err := os.WriteFile(statementPath, []byte(sign("staging", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadArtifactVerifier(cfg, now); err == nil {
		t.Fatal("accepted wrong-environment statement")
	}

	if err := os.WriteFile(statementPath, []byte(sign("prod", 2)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, store, err := loadArtifactVerifier(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statementPath, []byte(sign("prod", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadArtifactVerifier(cfg, now); err == nil {
		t.Fatal("accepted deployed statement rollback")
	}
}

func TestCPConfig_PostgresRequiresDSN(t *testing.T) {
	_, err := loadCPTest(t, "dev", nil, "store.driver=postgres") // dsn still the sqlite default
	if err == nil || !strings.Contains(err.Error(), "store.dsn") {
		t.Fatalf("expected store.dsn validation error, got %v", err)
	}
}

func TestCPConfig_InvalidEnumIsFatal(t *testing.T) {
	if _, err := loadCPTest(t, "dev", nil, "auth.mode=bogus"); err == nil {
		t.Fatal("expected validation error for auth.mode=bogus")
	}
}
