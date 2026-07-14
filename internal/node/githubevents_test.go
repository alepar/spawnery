package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	nodev1 "spawnery/gen/node/v1"
)

// fakeEvents is a stand-in for the sidecar's control listener: it serves BOTH the /control/github push
// endpoint and the /control/github/events long-poll (sp-2tx8.9.1's contract).
type fakeEvents struct {
	mu       sync.Mutex
	bodies   []pushBody  // /control/github pushes
	eventCh  chan string // fed a value => the next poll returns 200 {"event":<v>}; empty => 204
	eventGet atomic.Int64
	auths    []string
	hold     time.Duration // how long a poll with no pending event blocks before its 204
	srv      *httptest.Server
}

func newFakeEvents(t *testing.T) *fakeEvents {
	t.Helper()
	f := &fakeEvents{eventCh: make(chan string, 8), hold: 10 * time.Millisecond}
	mux := http.NewServeMux()
	mux.HandleFunc("/control/github", func(w http.ResponseWriter, r *http.Request) {
		var body pushBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"applied":true}`))
	})
	mux.HandleFunc("/control/github/events", func(w http.ResponseWriter, r *http.Request) {
		f.eventGet.Add(1)
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		hold := f.hold
		f.mu.Unlock()
		select {
		case ev := <-f.eventCh:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"event":"` + ev + `"}`))
		case <-time.After(hold):
			w.WriteHeader(http.StatusNoContent) // the sidecar's bounded-timeout 204
		case <-r.Context().Done():
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEvents) controlURL() string { return f.srv.URL + "/control/model" }
func (f *fakeEvents) reject()            { f.eventCh <- "token_rejected" }
func (f *fakeEvents) polls() int64       { return f.eventGet.Load() }
func (f *fakeEvents) pushes() []pushBody {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pushBody(nil), f.bodies...)
}

// seqMintClient hands out a DIFFERENT token on every mint (ghs_1, ghs_2, …) so a test can prove the
// node pushed a token it re-minted rather than the one the sidecar just rejected. err, when set, is
// returned instead.
type seqMintClient struct {
	mu  sync.Mutex
	n   int
	err error
}

func (m *seqMintClient) MintGitHubAccessToken(_ context.Context, _ *connect.Request[authv1.MintGitHubAccessTokenRequest]) (*connect.Response[authv1.MintGitHubAccessTokenResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	m.n++
	return connect.NewResponse(&authv1.MintGitHubAccessTokenResponse{
		AccessToken:         fmt.Sprintf("ghs_%d", m.n),
		AccessExpiresAtUnix: time.Now().Add(8 * time.Hour).Unix(),
	}), nil
}

func (m *seqMintClient) mints() int { m.mu.Lock(); defer m.mu.Unlock(); return m.n }

// newWatchTestServer wires a githubControlServer with a linked spawn "s1", a real (untimed) events
// client and fast timings.
func newWatchTestServer(t *testing.T, mint GitHubMintClient) *githubControlServer {
	t.Helper()
	s := newGitHubControlServer(newGitHubRefresher(mint), caStore{dir: t.TempDir()})
	s.doer = &http.Client{Timeout: 2 * time.Second}
	s.eventsDoer = &http.Client{} // no client-side timeout: the poll ctx bounds it
	s.pushBackoffBase = time.Millisecond
	s.pushBackoffMax = 2 * time.Millisecond
	s.pushFallbackWindow = 50 * time.Millisecond
	s.eventsBackoffBase = time.Millisecond
	s.eventsBackoffMax = 2 * time.Millisecond
	s.eventsPollTimeout = 2 * time.Second
	s.rejectCooldown = 0
	s.refresher.Note(githubRefreshEntry{SpawnID: "s1", SecretID: "sec-1"})
	return s
}

func TestPollEventsOnceReturnsRejection(t *testing.T) {
	f := newFakeEvents(t)
	s := newWatchTestServer(t, &seqMintClient{})
	f.reject()
	got, err := s.pollEventsOnce(context.Background(), f.controlURL(), "tok")
	if err != nil {
		t.Fatalf("pollEventsOnce: %v", err)
	}
	if !got {
		t.Fatal("poll did not report the rejection the sidecar returned")
	}
	f.mu.Lock()
	auth := f.auths[0]
	f.mu.Unlock()
	if auth != "Bearer tok" {
		t.Fatalf("poll Authorization = %q, want %q", auth, "Bearer tok")
	}
}

func TestPollEventsOnce204IsNotARejection(t *testing.T) {
	f := newFakeEvents(t)
	s := newWatchTestServer(t, &seqMintClient{})
	got, err := s.pollEventsOnce(context.Background(), f.controlURL(), "tok")
	if err != nil {
		t.Fatalf("pollEventsOnce: %v", err)
	}
	if got {
		t.Fatal("a 204 (bounded-timeout) must not be read as a token rejection")
	}
}

func TestPollEventsOnceNon2xxIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "github proxy not enabled", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	s := newWatchTestServer(t, &seqMintClient{})
	if _, err := s.pollEventsOnce(context.Background(), srv.URL+"/control/model", "tok"); err == nil {
		t.Fatal("a 503 from the sidecar must be an error (it drives the re-dial backoff)")
	}
}

func TestPushWithRetryForceReMints(t *testing.T) {
	f := newFakeEvents(t)
	mint := &seqMintClient{}
	s := newWatchTestServer(t, mint)
	t.Cleanup(func() { s.Stop("s1") }) // PushCredentials below starts the events watch; stop it with the test.

	// A first, unforced push: mints ghs_1 and caches it.
	if err := s.PushCredentials(context.Background(), "s1", f.controlURL(), "tok"); err != nil {
		t.Fatalf("PushCredentials: %v", err)
	}
	// Forced: must NOT reuse the cached ghs_1 — the whole point is that GitHub rejected it.
	if got := s.pushWithRetry(context.Background(), "s1", f.controlURL(), "tok", true); got != pushOK {
		t.Fatalf("forced pushWithRetry = %v, want pushOK", got)
	}
	pushed := f.pushes()
	if len(pushed) != 2 {
		t.Fatalf("sidecar received %d pushes, want 2", len(pushed))
	}
	if pushed[0].Token != "ghs_1" || pushed[1].Token != "ghs_2" {
		t.Fatalf("pushed tokens = %q,%q; want ghs_1 then a RE-MINTED ghs_2", pushed[0].Token, pushed[1].Token)
	}
	if mint.mints() != 2 {
		t.Fatalf("mints = %d, want 2 (the forced push must bypass the token cache AND the mint rate floor)", mint.mints())
	}
}

func TestPushWithRetryForceRelinkRequired(t *testing.T) {
	f := newFakeEvents(t)
	mint := &seqMintClient{err: connect.NewError(connect.CodeFailedPrecondition, errors.New("link revoked"))}
	s := newWatchTestServer(t, mint)
	var got []nodev1.GitHubCredentialStatus
	var mu sync.Mutex
	s.SetStatusReporter(func(_ string, st nodev1.GitHubCredentialStatus) {
		mu.Lock()
		got = append(got, st)
		mu.Unlock()
	})

	if out := s.pushWithRetry(context.Background(), "s1", f.controlURL(), "tok", true); out != pushRelinkRequired {
		t.Fatalf("pushWithRetry on a revoked link = %v, want pushRelinkRequired", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_RELINK_REQUIRED {
		t.Fatalf("reported %v, want a single RELINK_REQUIRED", got)
	}
}

// The headline behaviour of this bead: a token GitHub rejects mid-life is detected on the long-poll,
// re-minted (forced), and re-pushed — without waiting ~8h for the next scheduled rotation.
func TestWatchRejectionForcesReMintAndRePush(t *testing.T) {
	f := newFakeEvents(t)
	mint := &seqMintClient{}
	s := newWatchTestServer(t, mint)
	t.Cleanup(func() { s.Stop("s1") })

	// Create-path push: delivers ghs_1 AND starts the watch (only for a github-linked spawn).
	if err := s.PushCredentials(context.Background(), "s1", f.controlURL(), "tok"); err != nil {
		t.Fatalf("PushCredentials: %v", err)
	}
	waitFor(t, "the node never dialled /control/github/events", func() bool { return f.polls() >= 1 })

	f.reject() // the MITM proxy saw a 401/403 from GitHub

	waitFor(t, "the rejection did not produce a re-minted, re-pushed token", func() bool {
		p := f.pushes()
		return len(p) >= 2 && p[len(p)-1].Token == "ghs_2"
	})

	// And the watch keeps running: it re-dials after handling the rejection.
	before := f.polls()
	waitFor(t, "the watcher did not re-dial after a rejection", func() bool { return f.polls() > before })
}

func TestWatchIsOnlyStartedForGitHubSpawns(t *testing.T) {
	f := newFakeEvents(t)
	s := newWatchTestServer(t, &seqMintClient{})
	s.refresher.Forget("s1") // no link => PushCredentials is a no-op
	t.Cleanup(func() { s.Stop("s2") })

	if err := s.PushCredentials(context.Background(), "s2", f.controlURL(), "tok"); err != nil {
		t.Fatalf("PushCredentials: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if f.polls() != 0 {
		t.Fatalf("the node held a long-poll for a spawn with NO github mount (%d polls)", f.polls())
	}
	s.mu.Lock()
	n := len(s.watches)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("watches = %d, want 0", n)
	}
}

func TestWatchIsIdempotent(t *testing.T) {
	f := newFakeEvents(t)
	s := newWatchTestServer(t, &seqMintClient{})
	t.Cleanup(func() { s.Stop("s1") })

	s.startEventsWatch("s1", f.controlURL(), "tok")
	s.startEventsWatch("s1", f.controlURL(), "tok") // rotation push, re-adopt push, …
	s.mu.Lock()
	n := len(s.watches)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("watches = %d, want exactly 1 long-poll per spawn", n)
	}
}

// Re-adopt (SE3): a restarted node must not silently lose rejection detection. readopt.go calls
// PushAsync, whose success path re-establishes the watch.
func TestPushAsyncReEstablishesTheWatchOnReAdopt(t *testing.T) {
	f := newFakeEvents(t)
	s := newWatchTestServer(t, &seqMintClient{})
	s.lookup = func(string) (string, string, bool) { return f.controlURL(), "tok", true }
	t.Cleanup(func() { s.Stop("s1") })

	s.PushAsync(context.Background(), "s1")
	waitFor(t, "re-adopt did not re-establish the rejection long-poll", func() bool { return f.polls() >= 1 })
}

func TestStopEndsTheWatch(t *testing.T) {
	f := newFakeEvents(t)
	s := newWatchTestServer(t, &seqMintClient{})
	if err := s.PushCredentials(context.Background(), "s1", f.controlURL(), "tok"); err != nil {
		t.Fatalf("PushCredentials: %v", err)
	}
	waitFor(t, "watch never started", func() bool { return f.polls() >= 1 })

	s.Stop("s1")
	s.mu.Lock()
	n := len(s.watches)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("watches = %d after Stop, want 0", n)
	}
	time.Sleep(30 * time.Millisecond)
	before := f.polls()
	time.Sleep(60 * time.Millisecond)
	if f.polls() != before {
		t.Fatal("the long-poll kept re-dialling after the spawn stopped")
	}
}

// A revoked link does not heal: report RELINK_REQUIRED once and stop watching (bead: "do not
// retry-loop on a revoked link").
func TestWatchStopsOnRelinkRequired(t *testing.T) {
	f := newFakeEvents(t)
	mint := &seqMintClient{}
	s := newWatchTestServer(t, mint)
	t.Cleanup(func() { s.Stop("s1") })
	var mu sync.Mutex
	var got []nodev1.GitHubCredentialStatus
	s.SetStatusReporter(func(_ string, st nodev1.GitHubCredentialStatus) {
		mu.Lock()
		got = append(got, st)
		mu.Unlock()
	})

	if err := s.PushCredentials(context.Background(), "s1", f.controlURL(), "tok"); err != nil {
		t.Fatalf("PushCredentials: %v", err)
	}
	waitFor(t, "watch never started", func() bool { return f.polls() >= 1 })

	mint.mu.Lock()
	mint.err = connect.NewError(connect.CodeNotFound, errors.New("link gone"))
	mint.mu.Unlock()
	f.reject()

	waitFor(t, "a revoked link was never reported as RELINK_REQUIRED", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, st := range got {
			if st == nodev1.GitHubCredentialStatus_GITHUB_CREDENTIAL_STATUS_RELINK_REQUIRED {
				return true
			}
		}
		return false
	})

	waitFor(t, "the watcher kept re-dialling after a revoked link", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.watches) == 0
	})
}
