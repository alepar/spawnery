package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	nodev1 "spawnery/gen/node/v1"
)

// pushBody is the /control/github wire contract (sp-2tx8.9.1, internal/sidecar/override.go).
type pushBody struct {
	CACertPEM      string `json:"ca_cert_pem"`
	CAKeyPEM       string `json:"ca_key_pem"`
	Token          string `json:"token"`
	TokenExpiresAt int64  `json:"token_expires_at"`
}

// fakeSidecar is a stand-in for the sidecar's control listener. It records every /control/github
// POST and answers with the status in `code` (200 by default).
type fakeSidecar struct {
	mu     sync.Mutex
	bodies []pushBody
	auths  []string
	paths  []string
	code   int

	srv *httptest.Server
}

func newFakeSidecar(t *testing.T) *fakeSidecar {
	t.Helper()
	f := &fakeSidecar{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body pushBody
		_ = json.Unmarshal(b, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.paths = append(f.paths, r.URL.Path)
		code := f.code
		f.mu.Unlock()
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"applied":true}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// controlURL mimics spawnlet.Spawn.ControlURL: the sidecar's /control/model URL.
func (f *fakeSidecar) controlURL() string { return f.srv.URL + "/control/model" }

func (f *fakeSidecar) pushes() []pushBody {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pushBody(nil), f.bodies...)
}

func (f *fakeSidecar) setCode(c int) { f.mu.Lock(); f.code = c; f.mu.Unlock() }

// newPushTestServer wires a githubControlServer with a linked spawn ("s1"), a fake AS mint client,
// a real http.Client doer and short push timings.
func newPushTestServer(t *testing.T, mint *fakeMintClient) *githubControlServer {
	t.Helper()
	r := newGitHubRefresher(mint)
	s := newGitHubControlServer(r, caStore{dir: t.TempDir()})
	s.doer = &http.Client{Timeout: 2 * time.Second}
	s.pushBackoffBase = time.Millisecond
	s.pushBackoffMax = 2 * time.Millisecond
	s.pushFallbackWindow = 30 * time.Millisecond
	return s
}

func TestPushCredentialsDeliversCAAndToken(t *testing.T) {
	sc := newFakeSidecar(t)
	mint := &fakeMintClient{resp: &authv1.MintGitHubAccessTokenResponse{
		AccessToken: "ghs_live", AccessExpiresAtUnix: 1_900_000_000,
	}}
	s := newPushTestServer(t, mint)
	s.refresher.Note(githubRefreshEntry{SpawnID: "s1", SecretID: "sec-1"})

	if err := s.PushCredentials(context.Background(), "s1", sc.controlURL(), "bearer-tok"); err != nil {
		t.Fatalf("PushCredentials: %v", err)
	}

	got := sc.pushes()
	if len(got) != 1 {
		t.Fatalf("sidecar received %d pushes, want 1", len(got))
	}
	if got[0].Token != "ghs_live" || got[0].TokenExpiresAt != 1_900_000_000 {
		t.Fatalf("pushed token = (%q,%d), want (ghs_live,1900000000)", got[0].Token, got[0].TokenExpiresAt)
	}
	if !strings.Contains(got[0].CACertPEM, "BEGIN CERTIFICATE") {
		t.Fatalf("pushed ca_cert_pem is not a PEM certificate: %q", got[0].CACertPEM)
	}
	if !strings.Contains(got[0].CAKeyPEM, "PRIVATE KEY") {
		t.Fatalf("pushed ca_key_pem is not a PEM private key: %q", got[0].CAKeyPEM)
	}
	// The pushed CA MUST be the same one rendered into the agent's trust bundle.
	certPEM, err := s.SpawnCACert("s1")
	if err != nil {
		t.Fatalf("SpawnCACert: %v", err)
	}
	if got[0].CACertPEM != string(certPEM) {
		t.Fatal("pushed CA differs from the CA handed to renderGitProxy — the agent would not trust the MITM leaf")
	}
	sc.mu.Lock()
	path, auth := sc.paths[0], sc.auths[0]
	sc.mu.Unlock()
	if path != "/control/github" {
		t.Fatalf("push path = %q, want /control/github", path)
	}
	if auth != "Bearer bearer-tok" {
		t.Fatalf("push Authorization = %q, want Bearer bearer-tok", auth)
	}
}

func TestPushCredentialsNoGitHubLinkIsANoOp(t *testing.T) {
	sc := newFakeSidecar(t)
	s := newPushTestServer(t, &fakeMintClient{})
	// No Note => no link for "s1".
	if err := s.PushCredentials(context.Background(), "s1", sc.controlURL(), "tok"); err != nil {
		t.Fatalf("a spawn with no github link must be a no-op, got error: %v", err)
	}
	if n := len(sc.pushes()); n != 0 {
		t.Fatalf("pushed %d times for an unlinked spawn, want 0", n)
	}
}

func TestPushCredentialsSidecarErrorIsFatal(t *testing.T) {
	sc := newFakeSidecar(t)
	sc.setCode(http.StatusBadRequest)
	mint := &fakeMintClient{resp: &authv1.MintGitHubAccessTokenResponse{AccessToken: "ghs_live"}}
	s := newPushTestServer(t, mint)
	s.refresher.Note(githubRefreshEntry{SpawnID: "s1", SecretID: "sec-1"})

	err := s.PushCredentials(context.Background(), "s1", sc.controlURL(), "tok")
	if err == nil {
		t.Fatal("a 400 from the sidecar must be an error (the create path is fail-closed)")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("error should name the status code, got %v", err)
	}
}

func TestPushAsyncRetriesThenReportsStale(t *testing.T) {
	sc := newFakeSidecar(t)
	sc.setCode(http.StatusInternalServerError)
	mint := &fakeMintClient{resp: &authv1.MintGitHubAccessTokenResponse{AccessToken: "ghs_live"}}
	s := newPushTestServer(t, mint)
	s.refresher.Note(githubRefreshEntry{SpawnID: "s1", SecretID: "sec-1"})
	s.lookup = func(string) (string, string, bool) { return sc.controlURL(), "tok", true }

	reported := make(chan nodev1.GitHubCredentialStatus, 8)
	s.SetStatusReporter(func(_ string, st nodev1.GitHubCredentialStatus) { reported <- st })

	s.PushAsync(context.Background(), "s1")

	select {
	case st := <-reported:
		if st != nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_STALE {
			t.Fatalf("reported %v, want STALE", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an undeliverable push never reported STALE")
	}
	if n := len(sc.pushes()); n < 2 {
		t.Fatalf("push was attempted %d times; want a bounded RETRY (>=2)", n)
	}
}

func TestPushAsyncRelinkRequired(t *testing.T) {
	sc := newFakeSidecar(t)
	mint := &fakeMintClient{err: connect.NewError(connect.CodeFailedPrecondition, errors.New("link revoked"))}
	s := newPushTestServer(t, mint)
	s.refresher.Note(githubRefreshEntry{SpawnID: "s1", SecretID: "sec-1"})
	s.lookup = func(string) (string, string, bool) { return sc.controlURL(), "tok", true }

	reported := make(chan nodev1.GitHubCredentialStatus, 8)
	s.SetStatusReporter(func(_ string, st nodev1.GitHubCredentialStatus) { reported <- st })

	s.PushAsync(context.Background(), "s1")

	select {
	case st := <-reported:
		if st != nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_RELINK_REQUIRED {
			t.Fatalf("reported %v, want RELINK_REQUIRED", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a revoked link never reported RELINK_REQUIRED")
	}
	if n := len(sc.pushes()); n != 0 {
		t.Fatalf("a revoked link must not be pushed or retry-looped; got %d pushes", n)
	}
}

func TestPushAsyncSuccessReportsOK(t *testing.T) {
	sc := newFakeSidecar(t)
	mint := &fakeMintClient{resp: &authv1.MintGitHubAccessTokenResponse{AccessToken: "ghs_live"}}
	s := newPushTestServer(t, mint)
	s.refresher.Note(githubRefreshEntry{SpawnID: "s1", SecretID: "sec-1"})
	s.lookup = func(string) (string, string, bool) { return sc.controlURL(), "tok", true }

	reported := make(chan nodev1.GitHubCredentialStatus, 8)
	s.SetStatusReporter(func(_ string, st nodev1.GitHubCredentialStatus) { reported <- st })

	s.PushAsync(context.Background(), "s1")

	select {
	case st := <-reported:
		if st != nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_OK {
			t.Fatalf("reported %v, want OK", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a successful push never reported OK")
	}
}

// A condition computed while the CP was disconnected must not be lost: installing a reporter on the
// next connection re-emits every sticky non-OK status.
func TestSetStatusReporterReplaysStickyConditions(t *testing.T) {
	s := newPushTestServer(t, &fakeMintClient{})
	s.report("s1", nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_STALE) // no reporter installed

	var got []nodev1.GitHubCredentialStatus
	s.SetStatusReporter(func(_ string, st nodev1.GitHubCredentialStatus) { got = append(got, st) })
	if len(got) != 1 || got[0] != nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_STALE {
		t.Fatalf("sticky STALE was not replayed to the new reporter, got %v", got)
	}
}

// Stop is spawn death: it must cancel an in-flight push loop and forget the spawn's condition.
func TestStopCancelsInFlightPush(t *testing.T) {
	sc := newFakeSidecar(t)
	sc.setCode(http.StatusInternalServerError)
	mint := &fakeMintClient{resp: &authv1.MintGitHubAccessTokenResponse{AccessToken: "ghs_live"}}
	s := newPushTestServer(t, mint)
	s.pushFallbackWindow = time.Hour // would retry for an hour if Stop did not cancel it
	s.refresher.Note(githubRefreshEntry{SpawnID: "s1", SecretID: "sec-1"})
	s.lookup = func(string) (string, string, bool) { return sc.controlURL(), "tok", true }

	s.PushAsync(context.Background(), "s1")
	s.Stop("s1")

	// The loop must stop attempting; sample twice and require the count to settle.
	time.Sleep(50 * time.Millisecond)
	n1 := len(sc.pushes())
	time.Sleep(100 * time.Millisecond)
	if n2 := len(sc.pushes()); n2 != n1 {
		t.Fatalf("push loop kept running after Stop: %d -> %d attempts", n1, n2)
	}
}

// TestRotationPushesTheRotatedToken: a successful proactive refresh must deliver the freshly rotated
// token into the sidecar (spec §3.1, "on rotation": the existing githubRefresher schedule).
func TestRotationPushesTheRotatedToken(t *testing.T) {
	sc := newFakeSidecar(t)
	mint := &fakeMintClient{resp: &authv1.MintGitHubAccessTokenResponse{
		AccessToken: "ghs_rotated", AccessExpiresAtUnix: 1_900_000_000,
	}}
	s := newPushTestServer(t, mint)
	s.lookup = func(string) (string, string, bool) { return sc.controlURL(), "tok", true }

	pushed := make(chan struct{}, 4)
	s.SetStatusReporter(func(_ string, _ nodev1.GitHubCredentialStatus) { pushed <- struct{}{} })

	r := s.refresher
	r.onRotate = s.PushAsync
	base := time.Unix(1_800_000_000, 0)
	r.now = func() time.Time { return base }
	r.Note(githubRefreshEntry{SpawnID: "s1", SecretID: "sec-1", Version: 2, DeliveryID: "d-2"})

	// Drive one proactive refresh: the entry is due once the receipt-relative interval has elapsed.
	r.Tick(context.Background(), base.Add(defaultRefreshInterval+time.Minute))

	select {
	case <-pushed:
	case <-time.After(5 * time.Second):
		t.Fatal("a successful proactive refresh did not push the rotated token")
	}
	got := sc.pushes()
	if len(got) == 0 || got[0].Token != "ghs_rotated" {
		t.Fatalf("sidecar got %+v; want the rotated token ghs_rotated", got)
	}
	if got[0].TokenExpiresAt != 1_900_000_000 {
		t.Fatalf("pushed expiry = %d, want the mint response's 1900000000", got[0].TokenExpiresAt)
	}
	// The rotated token must be CACHED, not re-minted: exactly one mint call for the whole rotation.
	if n := len(mint.calls()); n != 1 {
		t.Fatalf("mint was called %d times for one rotation; want 1 (the pushed token must come from the refresh mint)", n)
	}
}

// The out-of-band credential condition report must NOT carry a lifecycle phase: the CP persists the
// condition on any status message, but an ACTIVE phase would re-fire spawn_create telemetry and clear
// provisioning state. A spawn with a stale token is still ACTIVE (spec §4.1: a condition, not a phase).
func TestReportGitHubCredentialStatusIsPhaseless(t *testing.T) {
	a := &attacher{}
	fs := &fakeCPStream{}
	a.stream = fs

	a.reportGitHubCredentialStatus("s1", nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_STALE)

	msgs := fs.sent
	if len(msgs) != 1 {
		t.Fatalf("sent %d messages, want 1", len(msgs))
	}
	st := msgs[0].GetStatus()
	if st == nil {
		t.Fatalf("message is not a SpawnStatus: %+v", msgs[0])
	}
	if st.GetSpawnId() != "s1" {
		t.Fatalf("spawn id = %q, want s1", st.GetSpawnId())
	}
	if st.GetGithubCredentialStatus() != nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_STALE {
		t.Fatalf("github_credential_status = %v, want STALE", st.GetGithubCredentialStatus())
	}
	if st.GetPhase() != nodev1.SpawnPhase_SPAWN_PHASE_UNSPECIFIED {
		t.Fatalf("phase = %v, want SPAWN_PHASE_UNSPECIFIED (a condition, not a phase)", st.GetPhase())
	}
}
