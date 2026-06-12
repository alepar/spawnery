package authsvc_test

// Device-set HTTP endpoint tests (POST /devices/append, GET /devices).
// Uses real seal.StoredEntry objects built from live mnemonics so the HTTP
// handler exercises the full encode→decode→hash→CAS path.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/pki"
	"spawnery/internal/secrets/seal"
)

// --- helpers ------------------------------------------------------------------

// newSealDevice generates a fresh seal.Device from a new mnemonic.
func newSealDevice(t *testing.T) *seal.Device {
	t.Helper()
	m, err := seal.NewMnemonic()
	if err != nil {
		t.Fatalf("NewMnemonic: %v", err)
	}
	d, err := seal.DeviceFromMnemonic(m, "")
	if err != nil {
		t.Fatalf("DeviceFromMnemonic: %v", err)
	}
	return d
}

// encodeEntry marshals a StoredEntry to JSON and then base64-encodes it (the wire format for POST /devices/append).
func encodeEntry(t *testing.T, e seal.StoredEntry) string {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal StoredEntry: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// newDeviceSetSvc creates a minimal authsvc.Service wired with a fresh store and WithDeviceSet,
// using the given accountID for all requests.
func newDeviceSetSvc(t *testing.T, accountID, spaOrigin string) (*authsvc.Service, store.Store) {
	t.Helper()
	root, err := pki.NewRootCA("Test Root")
	if err != nil {
		t.Fatal(err)
	}
	inter, err := root.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewTestStore(t)
	svc := authsvc.New(root.Cert, inter,
		authsvc.WithDeviceSet(
			st.DeviceSets(),
			spaOrigin,
			authsvc.FixedAccountFromRequest(accountID),
		),
	)
	return svc, st
}

// post sends a POST /devices/append with the base64-encoded entry body.
func post(t *testing.T, srv *httptest.Server, entryB64, origin, bearerToken string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"entry": entryB64})
	req, _ := http.NewRequest("POST", srv.URL+"/devices/append", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /devices/append: %v", err)
	}
	return resp
}

// getDevices sends GET /devices with optional origin and bearer.
func getDevices(t *testing.T, srv *httptest.Server, origin, bearerToken string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/devices", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /devices: %v", err)
	}
	return resp
}

// --- tests --------------------------------------------------------------------

func TestDeviceSetAppendAndFetchHappyPath(t *testing.T) {
	svc, _ := newDeviceSetSvc(t, "acct-1", "")
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	d1 := newSealDevice(t)
	rec := newSealDevice(t)
	log, err := seal.NewGenesis(d1, rec)
	if err != nil {
		t.Fatalf("NewGenesis: %v", err)
	}

	genesisB64 := encodeEntry(t, log.Entries[0])
	resp := post(t, srv, genesisB64, "", "token-unused-fixed-account")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("append genesis: want 200, got %d: %s", resp.StatusCode, body)
	}

	var appendOut struct {
		Version uint64 `json:"version"`
		Head    string `json:"head"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &appendOut); err != nil {
		t.Fatalf("decode append response: %v (%s)", err, body)
	}
	if appendOut.Version != 1 {
		t.Fatalf("want version=1, got %d", appendOut.Version)
	}
	if appendOut.Head == "" {
		t.Fatal("empty head in append response")
	}

	// GET /devices should return one entry.
	gr := getDevices(t, srv, "", "")
	defer gr.Body.Close()
	if gr.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(gr.Body)
		t.Fatalf("GET /devices: want 200, got %d: %s", gr.StatusCode, body)
	}
	var listOut struct {
		Entries []string `json:"entries"`
		Head    string   `json:"head"`
		Version uint64   `json:"version"`
	}
	body2, _ := io.ReadAll(gr.Body)
	if err := json.Unmarshal(body2, &listOut); err != nil {
		t.Fatalf("decode list response: %v (%s)", err, body2)
	}
	if len(listOut.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(listOut.Entries))
	}
	if listOut.Version != 1 {
		t.Fatalf("want version=1 in list, got %d", listOut.Version)
	}
	if listOut.Head != appendOut.Head {
		t.Fatalf("head mismatch between append and list")
	}
}

func TestDeviceSetConflict409(t *testing.T) {
	svc, _ := newDeviceSetSvc(t, "acct-1", "")
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	d1 := newSealDevice(t)
	rec := newSealDevice(t)
	log, _ := seal.NewGenesis(d1, rec)

	genesisB64 := encodeEntry(t, log.Entries[0])

	// First append succeeds.
	resp1 := post(t, srv, genesisB64, "", "")
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first append: want 200, got %d", resp1.StatusCode)
	}

	// Second append of the same entry (same version=1) must conflict.
	resp2 := post(t, srv, genesisB64, "", "")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("second append: want 409, got %d: %s", resp2.StatusCode, body)
	}

	var conflictOut struct {
		Error   string `json:"error"`
		Head    string `json:"head"`
		Version uint64 `json:"version"`
	}
	body, _ := io.ReadAll(resp2.Body)
	if err := json.Unmarshal(body, &conflictOut); err != nil {
		t.Fatalf("decode 409 body: %v (%s)", err, body)
	}
	if conflictOut.Error != "conflict" {
		t.Fatalf("want error=conflict, got %q", conflictOut.Error)
	}
	if conflictOut.Version != 1 {
		t.Fatalf("409 should report current version=1, got %d", conflictOut.Version)
	}
	if conflictOut.Head == "" {
		t.Fatal("409 should include current head hash")
	}
}

func TestDeviceSetAccountIsolationHTTP(t *testing.T) {
	root, err := pki.NewRootCA("Test Root")
	if err != nil {
		t.Fatal(err)
	}
	inter, err := root.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewTestStore(t)

	// Build a service whose account is determined by an "X-Account-ID" header.
	accountFromReq := func(r *http.Request) (string, bool) {
		id := r.Header.Get("X-Account-ID")
		return id, id != ""
	}
	svc := authsvc.New(root.Cert, inter,
		authsvc.WithDeviceSet(st.DeviceSets(), "", authsvc.AccountFromRequest(accountFromReq)),
	)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	sendAs := func(t *testing.T, accountID, entryB64 string) *http.Response {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"entry": entryB64})
		req, _ := http.NewRequest("POST", srv.URL+"/devices/append", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Account-ID", accountID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /devices/append: %v", err)
		}
		return resp
	}
	listAs := func(t *testing.T, accountID string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+"/devices", nil)
		req.Header.Set("X-Account-ID", accountID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /devices: %v", err)
		}
		return resp
	}

	// Genesis for account A.
	d1A := newSealDevice(t)
	recA := newSealDevice(t)
	logA, _ := seal.NewGenesis(d1A, recA)
	rA := sendAs(t, "acct-A", encodeEntry(t, logA.Entries[0]))
	rA.Body.Close()
	if rA.StatusCode != http.StatusOK {
		t.Fatalf("acct-A genesis: want 200, got %d", rA.StatusCode)
	}

	// Genesis for account B.
	d1B := newSealDevice(t)
	recB := newSealDevice(t)
	logB, _ := seal.NewGenesis(d1B, recB)
	rB := sendAs(t, "acct-B", encodeEntry(t, logB.Entries[0]))
	rB.Body.Close()
	if rB.StatusCode != http.StatusOK {
		t.Fatalf("acct-B genesis: want 200, got %d", rB.StatusCode)
	}

	// A's list should have 1 entry; B's should have 1 entry.
	checkCount := func(accountID string, want int) {
		t.Helper()
		gr := listAs(t, accountID)
		defer gr.Body.Close()
		var out struct {
			Entries []string `json:"entries"`
		}
		body, _ := io.ReadAll(gr.Body)
		_ = json.Unmarshal(body, &out)
		if len(out.Entries) != want {
			t.Fatalf("%s: want %d entries, got %d", accountID, want, len(out.Entries))
		}
	}
	checkCount("acct-A", 1)
	checkCount("acct-B", 1)

	// A trying version=1 again must 409 (B's genesis must not count for A).
	rA2 := sendAs(t, "acct-A", encodeEntry(t, logA.Entries[0]))
	rA2.Body.Close()
	if rA2.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate A genesis: want 409, got %d", rA2.StatusCode)
	}
}

func TestDeviceSetCORSOriginPinning(t *testing.T) {
	svc, _ := newDeviceSetSvc(t, "acct-1", "https://app.example.com")
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	d1 := newSealDevice(t)
	rec := newSealDevice(t)
	log, _ := seal.NewGenesis(d1, rec)
	genesisB64 := encodeEntry(t, log.Entries[0])

	// Correct origin: should succeed.
	resp := post(t, srv, genesisB64, "https://app.example.com", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct origin: want 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("missing ACAO header for correct origin")
	}

	// Wrong origin: should 403.
	d1b := newSealDevice(t)
	recb := newSealDevice(t)
	logb, _ := seal.NewGenesis(d1b, recb)
	resp2 := post(t, srv, encodeEntry(t, logb.Entries[0]), "https://evil.example.com", "")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong origin: want 403, got %d", resp2.StatusCode)
	}

	// No origin (CLI/curl style): should pass through.
	d1c := newSealDevice(t)
	recc := newSealDevice(t)
	logc, _ := seal.NewGenesis(d1c, recc)
	// Need a different account to avoid conflict — use a new server.
	svc2, _ := newDeviceSetSvc(t, "acct-noorigin", "https://app.example.com")
	srv2 := httptest.NewServer(svc2.Handler())
	defer srv2.Close()
	resp3 := post(t, srv2, encodeEntry(t, logc.Entries[0]), "" /*no origin*/, "")
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("no origin: want 200, got %d", resp3.StatusCode)
	}
}

func TestDeviceSetCORSPreflight(t *testing.T) {
	svc, _ := newDeviceSetSvc(t, "acct-1", "https://app.example.com")
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("OPTIONS", srv.URL+"/devices/append", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight: want 204, got %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: Authorization must be in allowed headers, got %q", resp.Header.Get("Access-Control-Allow-Headers"))
	}
}

func TestDeviceSetUnauthRejected(t *testing.T) {
	root, err := pki.NewRootCA("Test Root")
	if err != nil {
		t.Fatal(err)
	}
	inter, err := root.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewTestStore(t)

	// accountFromReq always returns false.
	svc := authsvc.New(root.Cert, inter,
		authsvc.WithDeviceSet(st.DeviceSets(), "", func(_ *http.Request) (string, bool) {
			return "", false
		}),
	)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	d1 := newSealDevice(t)
	rec := newSealDevice(t)
	log, _ := seal.NewGenesis(d1, rec)
	resp := post(t, srv, encodeEntry(t, log.Entries[0]), "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth POST: want 401, got %d", resp.StatusCode)
	}

	gr := getDevices(t, srv, "", "")
	gr.Body.Close()
	if gr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth GET: want 401, got %d", gr.StatusCode)
	}
}

func TestDeviceSetGetEmptyAccount(t *testing.T) {
	svc, _ := newDeviceSetSvc(t, "acct-empty", "")
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	gr := getDevices(t, srv, "", "")
	defer gr.Body.Close()
	if gr.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(gr.Body)
		t.Fatalf("GET /devices empty: want 200, got %d: %s", gr.StatusCode, body)
	}
	var out struct {
		Entries []string `json:"entries"`
		Version uint64   `json:"version"`
	}
	body, _ := io.ReadAll(gr.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(out.Entries) != 0 {
		t.Fatalf("want 0 entries, got %d", len(out.Entries))
	}
	if out.Version != 0 {
		t.Fatalf("want version=0, got %d", out.Version)
	}
}
