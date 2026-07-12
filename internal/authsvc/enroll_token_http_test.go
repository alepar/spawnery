package authsvc_test

import (
	"bytes"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/mtls"
	"spawnery/internal/pki"
)

func enrollmentTokenService(t *testing.T) (*authsvc.Service, string, string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	root, err := pki.NewRootCA("enrollment root")
	if err != nil {
		t.Fatal(err)
	}
	intermediate, err := root.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := authsvc.NewDevelopmentSigningCredential(root, "dev", now)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := token.NewVerifier(root.Cert, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	mintSession := func(audience string) string {
		payload, err := proto.Marshal(&authv1.SessionTokenBody{
			KeyId: hex.EncodeToString(signer.KeyID[:]), AccountId: "acct-owner", Audience: audience,
			IssuedAt: now.Unix(), ExpiresAt: now.Add(15 * time.Minute).Unix(),
		})
		if err != nil {
			t.Fatal(err)
		}
		session, err := signer.Sign(token.ArtifactTypeSession, payload)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	svc := authsvc.New(root.Cert, intermediate,
		authsvc.WithEnrollmentTokenIssuance(authsvc.EnrollmentSessionAccount(verifier, func() time.Time { return now })))
	return svc, mintSession(token.AudienceCP), mintSession("node")
}

func issueEnrollmentToken(t *testing.T, handler http.Handler, bearer, fingerprint string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"class": pki.ClassSelfHosted, "fingerprint": fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/enrollment-tokens", bytes.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestPublicEnrollmentTokenIssuanceRequiresAuthenticatedOwner(t *testing.T) {
	svc, ownerSession, nodeSession := enrollmentTokenService(t)
	_, fingerprint, _ := boundKey(t)

	anonymous := issueEnrollmentToken(t, svc.PublicHandler(), "", fingerprint)
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous issuance status = %d, want 401", anonymous.Code)
	}
	nodeAudience := issueEnrollmentToken(t, svc.PublicHandler(), nodeSession, fingerprint)
	if nodeAudience.Code != http.StatusUnauthorized {
		t.Fatalf("aud=node issuance status = %d, want 401", nodeAudience.Code)
	}

	authenticated := issueEnrollmentToken(t, svc.PublicHandler(), ownerSession, fingerprint)
	if authenticated.Code != http.StatusOK {
		t.Fatalf("authenticated issuance status = %d body=%s, want 200", authenticated.Code, authenticated.Body.String())
	}
	var response struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(authenticated.Body.Bytes(), &response); err != nil || response.Token == "" {
		t.Fatalf("decode enrollment token: token=%q err=%v", response.Token, err)
	}
}

func TestInternalEnrollmentBootstrapsOnlyWithPubliclyIssuedBoundToken(t *testing.T) {
	svc, ownerSession, _ := enrollmentTokenService(t)
	_, fingerprint, boundCSR := boundKey(t)
	_, _, substitutedCSR := boundKey(t)

	issued := issueEnrollmentToken(t, svc.PublicHandler(), ownerSession, fingerprint)
	if issued.Code != http.StatusOK {
		t.Fatalf("issue bound token: %d %s", issued.Code, issued.Body.String())
	}
	var tokenResponse struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &tokenResponse); err != nil {
		t.Fatal(err)
	}

	internal := svc.InternalHandler(mtls.Policy{"anonymous": {"authsvc.enroll": {}}})
	redeem := func(csr []byte) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{
			"token": tokenResponse.Token, "node_id": "node-1", "csr_pem": string(pki.MarshalCSRPEM(csr)),
		})
		rec := httptest.NewRecorder()
		internal.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/enroll", bytes.NewReader(body)))
		return rec
	}

	if rec := redeem(substitutedCSR); rec.Code != http.StatusUnauthorized {
		t.Fatalf("substituted bootstrap status = %d, want 401", rec.Code)
	}
	bound := redeem(boundCSR)
	if bound.Code != http.StatusOK {
		t.Fatalf("bound bootstrap status = %d body=%s, want 200", bound.Code, bound.Body.String())
	}
	var enrolled struct {
		CertPEM  string `json:"cert_pem"`
		ChainPEM string `json:"chain_pem"`
	}
	if err := json.Unmarshal(bound.Body.Bytes(), &enrolled); err != nil {
		t.Fatal(err)
	}
	leaf, err := pki.ParseCertPEM([]byte(enrolled.CertPEM))
	if err != nil {
		t.Fatal(err)
	}
	chain, err := pki.ParseCertPEM([]byte(enrolled.ChainPEM))
	if err != nil {
		t.Fatal(err)
	}
	root, err := pki.ParseCertPEM(svc.RootCAPEM())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := pki.Verify(leaf, []*x509.Certificate{chain}, root, pki.DefaultTrustDomain, time.Now(), allowNoCertificateRevocations)
	if err != nil {
		t.Fatal(err)
	}
	if identity.AccountID != "acct-owner" {
		t.Fatalf("enrolled account = %q, want authenticated account acct-owner", identity.AccountID)
	}
}
