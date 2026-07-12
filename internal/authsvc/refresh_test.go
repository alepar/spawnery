package authsvc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
)

// seedFamily inserts a refresh family row for testing. Returns raw token + the inserted row.
func seedFamily(t *testing.T, st store.Store, accountID string, spkiDER []byte, now time.Time) (rawToken string, famID string) {
	t.Helper()
	rawToken = randOpaque()
	famID = "fam-" + accountID
	row := store.RefreshSession{
		TokenHash:         sha256Hex(rawToken),
		AccountID:         accountID,
		FamilyID:          famID,
		ClientKind:        store.ClientWeb,
		SessionPubkeySPKI: spkiDER,
		CPAccessTokenID:   "cp-" + accountID,
		NodeAccessTokenID: "node-" + accountID,
		CreatedAt:         now.Unix(),
		LastUsedAt:        now.Unix(),
		ExpiresAt:         now.Add(30 * 24 * time.Hour).Unix(),
		FamilyCreatedAt:   now.Unix(),
	}
	if err := st.RefreshSessions().Insert(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	return rawToken, famID
}

func TestRefreshSupersedeFailureExposesNoTupleAndRetrySucceeds(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	faults := &storeFaults{failSupersede: true}
	idp, st, _ := newTestIdP(t, fake, now, func(cfg *IdPConfig) {
		cfg.Store = &failingStore{Store: cfg.Store, faults: faults}
	})
	sessKey, spkiDER := newTestP256(t)
	seedUser(t, st, "acct-refresh-atomic", 74001, now)
	rawToken, _ := seedFamily(t, st, "acct-refresh-atomic", spkiDER, now)

	request := func() *httptest.ResponseRecorder {
		proof := buildPoP(t, sessKey, rawToken, now.Unix(), make([]byte, 16))
		req := httptest.NewRequest(http.MethodPost, "/refresh", nil)
		req.AddCookie(&http.Cookie{Name: "refresh_token", Value: rawToken})
		req.Header.Set("X-PoP-Timestamp", strconv.FormatInt(proof.Timestamp, 10))
		req.Header.Set("X-PoP-Nonce", base64.RawURLEncoding.EncodeToString(proof.Nonce))
		req.Header.Set("X-PoP-Sig", base64.RawURLEncoding.EncodeToString(proof.Sig))
		rec := httptest.NewRecorder()
		idp.serveRefresh(rec, req)
		return rec
	}
	rec := request()
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "cp_access_token") || strings.Contains(rec.Body.String(), "node_access_token") {
		t.Fatalf("failed refresh status/body = %d %s", rec.Code, rec.Body.String())
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "refresh_token" && cookie.Value != "" {
			t.Fatalf("failed refresh exposed successor cookie %q", cookie.Value)
		}
	}
	predecessor, err := st.RefreshSessions().Get(context.Background(), sha256Hex(rawToken))
	if err != nil || predecessor.SupersededBy != "" {
		t.Fatalf("predecessor after failed refresh = %+v, err=%v", predecessor, err)
	}

	faults.failSupersede = false
	rec = request()
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cp_access_token") || !strings.Contains(rec.Body.String(), "node_access_token") {
		t.Fatalf("refresh retry status/body = %d %s", rec.Code, rec.Body.String())
	}
}

// seedUser creates a user in the store.
func seedUser(t *testing.T, st store.Store, accountID string, sub int64, now time.Time) store.User {
	t.Helper()
	u := store.User{AccountID: accountID, GithubSub: sub, Handle: "h", Status: store.UserActive, CreatedAt: now.Unix()}
	if err := st.Users().Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestRefreshHappyPath(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	idp, st, _ := newTestIdP(t, fake, now)
	sessKey, spkiDER := newTestP256(t)
	seedUser(t, st, "acct-1", 1, now)
	rawToken, _ := seedFamily(t, st, "acct-1", spkiDER, now)

	nonce := make([]byte, 16)
	proof := buildPoP(t, sessKey, rawToken, now.Unix(), nonce)
	cpAccess, nodeAccess, newRefresh, err := idp.handleRefresh(context.Background(), rawToken, proof, now)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if cpAccess == "" || nodeAccess == "" || newRefresh == "" {
		t.Fatal("empty paired access or refresh token")
	}
	// Old token should now be superseded.
	old, _ := st.RefreshSessions().Get(context.Background(), sha256Hex(rawToken))
	if old.SupersededBy == "" {
		t.Fatal("predecessor not superseded")
	}
}

// TestRefreshGrace: two concurrent goroutines both present the SAME token and must both get
// the SAME cached successor pair [AM3].
func TestRefreshGrace(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	idp, st, _ := newTestIdP(t, fake, now)
	sessKey, spkiDER := newTestP256(t)
	seedUser(t, st, "acct-1", 1, now)
	rawToken, _ := seedFamily(t, st, "acct-1", spkiDER, now)

	type result struct {
		cpAccess, nodeAccess, refresh string
		err                           error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			nonce := make([]byte, 16)
			proof := buildPoP(t, sessKey, rawToken, now.Unix(), nonce)
			cp, node, r, err := idp.handleRefresh(context.Background(), rawToken, proof, now)
			results[i] = result{cp, node, r, err}
		}()
	}
	wg.Wait()

	// Both must succeed (one rotates, the other gets the grace replay).
	for i, res := range results {
		if res.err != nil {
			t.Fatalf("result[%d]: %v", i, res.err)
		}
	}
	if results[0].cpAccess != results[1].cpAccess || results[0].nodeAccess != results[1].nodeAccess || results[0].refresh != results[1].refresh {
		t.Fatalf("concurrent refresh returned different successors: [0]=%+v [1]=%+v", results[0], results[1])
	}
}

// TestRefreshLostResponseRetry: present the superseded token again within grace → same cached pair.
func TestRefreshLostResponseRetry(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	idp, st, _ := newTestIdP(t, fake, now)
	sessKey, spkiDER := newTestP256(t)
	seedUser(t, st, "acct-1", 1, now)
	rawToken, _ := seedFamily(t, st, "acct-1", spkiDER, now)

	nonce := make([]byte, 16)
	// First refresh.
	proof1 := buildPoP(t, sessKey, rawToken, now.Unix(), nonce)
	cp1, node1, refresh1, err := idp.handleRefresh(context.Background(), rawToken, proof1, now)
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	// Simulate "lost response" — present the same OLD token within grace (30s later).
	replayTime := now.Add(30 * time.Second)
	idp.now = func() time.Time { return replayTime }
	proof2 := buildPoP(t, sessKey, rawToken, replayTime.Unix(), nonce)
	cp2, node2, refresh2, err := idp.handleRefresh(context.Background(), rawToken, proof2, replayTime)
	if err != nil {
		t.Fatalf("retry within grace: %v", err)
	}
	if cp1 != cp2 || node1 != node2 || refresh1 != refresh2 {
		t.Fatalf("grace retry returned different tuple: first=%q/%q/%q retry=%q/%q/%q", cp1, node1, refresh1, cp2, node2, refresh2)
	}
	// New refresh token should still work (was not consumed by the retry).
	idp.now = func() time.Time { return replayTime }
	proof3 := buildPoP(t, sessKey, refresh1, replayTime.Unix(), nonce)
	_, _, _, err = idp.handleRefresh(context.Background(), refresh1, proof3, replayTime)
	if err != nil {
		t.Fatalf("using successor after grace retry: %v", err)
	}
	_ = st
}

// TestRefreshReuseOutsideGrace: present superseded token after the 45s grace → family revoked.
func TestRefreshReuseOutsideGrace(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	idp, st, _ := newTestIdP(t, fake, now)
	sessKey, spkiDER := newTestP256(t)
	seedUser(t, st, "acct-1", 1, now)
	rawToken, _ := seedFamily(t, st, "acct-1", spkiDER, now)

	nonce := make([]byte, 16)
	proof := buildPoP(t, sessKey, rawToken, now.Unix(), nonce)
	_, _, _, err := idp.handleRefresh(context.Background(), rawToken, proof, now)
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	// Reuse the OLD token 46s later (outside grace).
	staleTime := now.Add(46 * time.Second)
	idp.cfg.Now = func() time.Time { return staleTime }
	proof2 := buildPoP(t, sessKey, rawToken, staleTime.Unix(), nonce)
	_, _, _, err = idp.handleRefresh(context.Background(), rawToken, proof2, staleTime)
	if !errors.Is(err, ErrFamilyRevoked) {
		t.Fatalf("want ErrFamilyRevoked after grace, got %v", err)
	}
	row, err := st.RefreshSessions().Get(context.Background(), sha256Hex(rawToken))
	if err != nil || !row.Revoked {
		t.Fatalf("reused family was not durably revoked: row=%+v err=%v", row, err)
	}
	successor, err := st.RefreshSessions().Get(context.Background(), row.SupersededBy)
	if err != nil || !successor.Revoked {
		t.Fatalf("successor was not durably revoked: row=%+v err=%v", successor, err)
	}
	events, err := st.Revocations().Since(context.Background(), 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("reuse revocation events = %+v, err=%v", events, err)
	}
	var ids []string
	if err := json.Unmarshal([]byte(events[0].TokenIDs), &ids); err != nil {
		t.Fatal(err)
	}
	want := []string{row.CPAccessTokenID, row.NodeAccessTokenID, successor.CPAccessTokenID, successor.NodeAccessTokenID}
	sort.Strings(ids)
	sort.Strings(want)
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("reuse revocation token ids = %v, want %v", ids, want)
	}
}

// TestRefreshPoPRequired: missing PoP headers → refused [AM5].
func TestRefreshPoPRequired(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	idp, st, _ := newTestIdP(t, fake, now)
	_, spkiDER := newTestP256(t)
	seedUser(t, st, "acct-1", 1, now)
	rawToken, _ := seedFamily(t, st, "acct-1", spkiDER, now)

	// Empty PoP — no sig.
	emptyProof := PoPProof{Timestamp: now.Unix(), Nonce: make([]byte, 16)}
	_, _, _, err := idp.handleRefresh(context.Background(), rawToken, emptyProof, now)
	if err == nil {
		t.Fatal("expected error with missing PoP sig")
	}
}

// TestRefreshFamilyMaxAge: present a token whose family is >90d old → refused [AM6].
func TestRefreshFamilyMaxAge(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	idp, st, _ := newTestIdP(t, fake, now)
	sessKey, spkiDER := newTestP256(t)
	seedUser(t, st, "acct-1", 1, now)

	// Insert a row with family_created_at in the distant past.
	rawToken := randOpaque()
	oldFamilyTime := now.Add(-91 * 24 * time.Hour)
	row := store.RefreshSession{
		TokenHash:         sha256Hex(rawToken),
		AccountID:         "acct-1",
		FamilyID:          "old-fam",
		ClientKind:        store.ClientWeb,
		SessionPubkeySPKI: spkiDER,
		CPAccessTokenID:   "cp-old",
		NodeAccessTokenID: "node-old",
		CreatedAt:         oldFamilyTime.Unix(),
		LastUsedAt:        now.Unix(),
		ExpiresAt:         now.Add(30 * 24 * time.Hour).Unix(),
		FamilyCreatedAt:   oldFamilyTime.Unix(),
	}
	if err := st.RefreshSessions().Insert(context.Background(), row); err != nil {
		t.Fatal(err)
	}

	nonce := make([]byte, 16)
	proof := buildPoP(t, sessKey, rawToken, now.Unix(), nonce)
	_, _, _, err := idp.handleRefresh(context.Background(), rawToken, proof, now)
	// ErrFamilyRevoked is also acceptable (max age triggers revoke); any non-nil error passes.
	if err == nil {
		t.Fatal("expected error for 91d family, got nil")
	}
}
