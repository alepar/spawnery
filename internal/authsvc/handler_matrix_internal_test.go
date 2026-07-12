package authsvc

import (
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"spawnery/gen/auth/v1/authv1connect"
	"spawnery/internal/mtls"
	"spawnery/internal/pki"
)

func TestInternalHandlerCompletePrincipalRouteMatrix(t *testing.T) {
	now := time.Now().UTC()
	const trustDomain = "prod.spawnery.internal"
	root, _ := pki.NewRootCA("root")
	serviceIssuer, _ := root.NewIntermediate(pki.IssuerService, trustDomain)
	cloudIssuer, _ := root.NewIntermediate(pki.IssuerCloudNode, trustDomain)
	selfIssuer, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, trustDomain)
	cp, _ := serviceIssuer.IssueService(pki.RoleCP, "cp-1", trustDomain, nil, nil, now.Add(time.Hour))
	authsvcPeer, _ := serviceIssuer.IssueService(pki.RoleAuthService, "as-2", trustDomain, nil, nil, now.Add(time.Hour))
	cloud, _ := cloudIssuer.IssueNode("cloud-1", "system", pki.RoleCloud, trustDomain, now.Add(time.Hour))
	selfHosted, _ := selfIssuer.IssueNode("self-1", "acct-1", pki.RoleSelfHosted, trustDomain, now.Add(time.Hour))

	verifier, err := mtls.NewPeerVerifier(mtls.PeerVerifierOptions{
		Root: root.Cert, TrustDomain: trustDomain, CurrentTime: func() time.Time { return now },
		IsRevoked: func(*big.Int, *big.Int) bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	count := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls[name]++
			w.WriteHeader(http.StatusNoContent)
		})
	}
	handler := mtls.PrincipalMiddleware(verifier, internalHandler(DefaultInternalPolicy(), internalRouteHandlers{
		enroll: count("enroll"), credentialMint: count("mint"),
		revocations: count("revocations"), githubLink: count("link"),
	}))

	type route struct{ method, path, counter string }
	enroll := route{http.MethodPost, "/enroll", "enroll"}
	mint := route{http.MethodPost, authv1connect.AuthServiceMintGitHubAccessTokenProcedure, "mint"}
	revocations := route{http.MethodGet, "/revocations", "revocations"}
	link := route{http.MethodPost, "/internal/github/link-status", "link"}
	unknown := route{http.MethodPost, "/internal/unknown", ""}

	tests := []struct {
		name  string
		peer  *pki.Leaf
		route route
		want  int
	}{
		{"anonymous enroll", nil, enroll, http.StatusNoContent},
		{"anonymous mint denied", nil, mint, http.StatusForbidden},
		{"anonymous revocations denied", nil, revocations, http.StatusForbidden},
		{"anonymous link denied", nil, link, http.StatusForbidden},
		{"anonymous unknown denied", nil, unknown, http.StatusForbidden},
		{"cloud mint", cloud, mint, http.StatusNoContent},
		{"cloud enroll denied", cloud, enroll, http.StatusForbidden},
		{"cloud CP revocations denied", cloud, revocations, http.StatusForbidden},
		{"cloud link denied", cloud, link, http.StatusForbidden},
		{"cloud unknown denied", cloud, unknown, http.StatusForbidden},
		{"self-hosted mint", selfHosted, mint, http.StatusNoContent},
		{"self-hosted enroll denied", selfHosted, enroll, http.StatusForbidden},
		{"self-hosted CP revocations denied", selfHosted, revocations, http.StatusForbidden},
		{"self-hosted link denied", selfHosted, link, http.StatusForbidden},
		{"self-hosted unknown denied", selfHosted, unknown, http.StatusForbidden},
		{"CP revocations", cp, revocations, http.StatusNoContent},
		{"CP link", cp, link, http.StatusNoContent},
		{"CP enroll denied", cp, enroll, http.StatusForbidden},
		{"CP mint denied", cp, mint, http.StatusForbidden},
		{"CP unknown denied", cp, unknown, http.StatusForbidden},
		{"authsvc enroll denied", authsvcPeer, enroll, http.StatusForbidden},
		{"authsvc mint denied", authsvcPeer, mint, http.StatusForbidden},
		{"authsvc revocations denied", authsvcPeer, revocations, http.StatusForbidden},
		{"authsvc link denied", authsvcPeer, link, http.StatusForbidden},
		{"authsvc unknown denied", authsvcPeer, unknown, http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := 0
			for _, value := range calls {
				before += value
			}
			req := httptest.NewRequest(test.route.method, test.route.path, nil)
			req.TLS = &tls.ConnectionState{HandshakeComplete: true, Version: tls.VersionTLS13}
			if test.peer != nil {
				req.TLS.PeerCertificates = append([]*x509.Certificate{test.peer.Cert}, test.peer.Chain...)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.want {
				t.Fatalf("status=%d want=%d", rec.Code, test.want)
			}
			after := 0
			for _, value := range calls {
				after += value
			}
			if test.want == http.StatusNoContent {
				if after != before+1 || calls[test.route.counter] == 0 {
					t.Fatalf("allowed route calls before=%d after=%d counters=%v", before, after, calls)
				}
			} else if after != before {
				t.Fatalf("denial reached handler: before=%d after=%d counters=%v", before, after, calls)
			}
		})
	}
}
