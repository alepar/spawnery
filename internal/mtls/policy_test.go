package mtls

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"spawnery/internal/pki"
)

const (
	opNodeCP  = "/spawnery.node.v1.NodeService/Heartbeat"
	opCPAS    = "/spawnery.auth.v1.AuthService/RevocationFeed"
	opNodeAS  = "/internal/node/credentials/refresh"
	opASCP    = "/internal/cp/github/mint"
	opEnroll  = "/enroll"
	opUnknown = "/internal/unknown"
)

func testPolicy() Policy {
	return Policy{
		"anonymous":        {opEnroll: {}},
		"service:cp":       {opCPAS: {}},
		"service:authsvc":  {opASCP: {}},
		"node:cloud":       {opNodeCP: {}, opNodeAS: {}},
		"node:self-hosted": {opNodeCP: {}, opNodeAS: {}},
	}
}

func TestPolicyEnforcesExactRoleOperationMatrix(t *testing.T) {
	t.Parallel()
	principals := []struct {
		name      string
		principal *pki.Principal
		roleKey   string
	}{
		{name: "anonymous", roleKey: "anonymous"},
		{name: "CP", roleKey: "service:cp", principal: &pki.Principal{TrustDomain: testTrustDomain, Kind: pki.KindService, Role: pki.RoleCP, InstanceID: "cp-1"}},
		{name: "authsvc", roleKey: "service:authsvc", principal: &pki.Principal{TrustDomain: testTrustDomain, Kind: pki.KindService, Role: pki.RoleAuthService, InstanceID: "as-1"}},
		{name: "cloud", roleKey: "node:cloud", principal: &pki.Principal{TrustDomain: testTrustDomain, Kind: pki.KindNode, Role: pki.RoleCloud, AccountID: "spawnery-system", NodeID: "cloud-1"}},
		{name: "self-hosted", roleKey: "node:self-hosted", principal: &pki.Principal{TrustDomain: testTrustDomain, Kind: pki.KindNode, Role: pki.RoleSelfHosted, AccountID: "acct-1", NodeID: "node-1"}},
	}
	operations := []string{opNodeCP, opCPAS, opNodeAS, opASCP, opEnroll, opUnknown}
	policy := testPolicy()
	for _, caller := range principals {
		caller := caller
		for _, operation := range operations {
			operation := operation
			t.Run(caller.name+" "+operation, func(t *testing.T) {
				t.Parallel()
				_, wantAllowed := policy[caller.roleKey][operation]
				err := policy.Authorize(operation, caller.principal)
				if wantAllowed && err != nil {
					t.Fatalf("Authorize denied registered operation: %v", err)
				}
				if !wantAllowed && err == nil {
					t.Fatal("Authorize allowed unregistered role-operation pair")
				}
			})
		}
	}
}

func TestPolicyRejectsUnknownPrincipalAndNonExactOperation(t *testing.T) {
	t.Parallel()
	policy := testPolicy()
	unknown := &pki.Principal{TrustDomain: testTrustDomain, Kind: pki.KindService, Role: "unknown", InstanceID: "x"}
	if err := policy.Authorize(opCPAS, unknown); err == nil {
		t.Fatal("unknown principal role authorized")
	}
	cloud := &pki.Principal{TrustDomain: testTrustDomain, Kind: pki.KindNode, Role: pki.RoleCloud, AccountID: "spawnery-system", NodeID: "cloud-1"}
	if err := policy.Authorize(opNodeCP+"/extra", cloud); err == nil {
		t.Fatal("operation prefix match authorized")
	}
}

func TestPolicyHTTPMiddlewareUsesVerifiedContextAndFailsClosed(t *testing.T) {
	t.Parallel()
	f := newTLSFixture(t)
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	})
	handler := PrincipalMiddleware(
		f.root.Cert,
		testTrustDomain,
		testPolicy().HTTPMiddleware(func(r *http.Request) string { return r.URL.Path }, next),
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, requestWithPeer(opNodeCP, f.cloud))
	if rec.Code != http.StatusNoContent || !reached {
		t.Fatalf("allowed request: status=%d reached=%v", rec.Code, reached)
	}

	reached = false
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, requestWithPeer(opCPAS, f.cloud))
	if rec.Code != http.StatusForbidden || reached {
		t.Fatalf("denied request: status=%d reached=%v", rec.Code, reached)
	}

	reached = false
	rec = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, opEnroll, nil)
	request.TLS = &tls.ConnectionState{HandshakeComplete: true}
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusNoContent || !reached {
		t.Fatalf("anonymous enrollment: status=%d reached=%v", rec.Code, reached)
	}
}
