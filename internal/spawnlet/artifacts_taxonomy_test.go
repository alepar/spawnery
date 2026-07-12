package spawnlet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sp-mwco.4.2: error taxonomy on the parsed S3 error Code (never the bare HTTP status), the
// re-presign seam, and the no-leak (presigned URL redaction) tests.
//
// Phase-0 spike (internal/cp/skillstore/presign_expiry_spike_test.go, live dev Garage) recorded the
// exact status+Code+Message this taxonomy is built from (see
// docs/superpowers/specs/2026-07-12-skill-delivery-hardening-design.md Post-Implementation Notes):
//
//	expired presign   -> HTTP 400 InvalidRequest  "Bad request: Date is too old"
//	tampered signature -> HTTP 403 AccessDenied    "Forbidden: Invalid signature"
//	missing key        -> HTTP 404 NoSuchKey       "Key not found"

// s3ErrorXMLBody renders a minimal S3-compatible <Error> XML body, matching what Garage returns.
func s3ErrorXMLBody(code, msg string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, msg)
}

// --- Classification: fetchObjectTar (real defaultFetcher, one HTTP request) -> *FetchError ---

// classifyOverHTTP starts an httptest server returning status+body exactly once, calls
// ArtifactStager.fetchObjectTar directly (bypassing fetchWithRetry's represign seam, which would
// convert an Expired classification into Terminal when no hook is supplied) so Expired/Terminal are
// observable as classifyS3Error produced them, and asserts the handler saw exactly one request.
func classifyOverHTTP(t *testing.T, status int, body string) *FetchError {
	t.Helper()
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	defer srv.Close()

	st := ArtifactStager{Root: t.TempDir()}
	art := Artifact{PresignedURL: srv.URL + "/obj"}
	_, err := st.fetchObjectTar(context.Background(), art)
	if err == nil {
		t.Fatalf("expected an error for status %d body %q", status, body)
	}
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FetchError, got %T: %v", err, err)
	}
	if got := atomic.LoadInt32(&reqs); got != 1 {
		t.Fatalf("handler invoked %d times, want exactly 1", got)
	}
	return fe
}

func TestClassifyS3Error_ExpiredPresign_400InvalidRequest(t *testing.T) {
	fe := classifyOverHTTP(t, http.StatusBadRequest, s3ErrorXMLBody("InvalidRequest", "Bad request: Date is too old"))
	if !fe.Expired {
		t.Fatalf("want Expired=true, got %+v", fe)
	}
	if fe.Terminal {
		t.Fatalf("expiry must not be Terminal (it is recoverable via re-presign), got Terminal=true: %+v", fe)
	}
	if !strings.Contains(strings.ToLower(fe.Message()), "expired") {
		t.Fatalf("message %q does not mention expiry", fe.Message())
	}
}

func TestClassifyS3Error_ExpiredPresign_403AccessDeniedExpiryWording(t *testing.T) {
	fe := classifyOverHTTP(t, http.StatusForbidden, s3ErrorXMLBody("AccessDenied", "Request has expired"))
	if !fe.Expired {
		t.Fatalf("want Expired=true, got %+v", fe)
	}
	if fe.Terminal {
		t.Fatalf("want Terminal=false for an expiry-worded AccessDenied, got %+v", fe)
	}
}

func TestClassifyS3Error_SignatureFault_403AccessDeniedNoExpiryWording(t *testing.T) {
	// Exact spike wording for a tampered signature.
	fe := classifyOverHTTP(t, http.StatusForbidden, s3ErrorXMLBody("AccessDenied", "Forbidden: Invalid signature"))
	if fe.Expired {
		t.Fatalf("a non-expiry AccessDenied must not be classified Expired: %+v", fe)
	}
	if !fe.Terminal {
		t.Fatalf("a signature/config fault must be Terminal (retrying will not help): %+v", fe)
	}
	msg := fe.Message()
	if !strings.Contains(msg, "clock skew") {
		t.Fatalf("message %q does not name the likely cause (clock skew)", msg)
	}
	if !strings.Contains(msg, "403") {
		t.Fatalf("message %q does not name the HTTP status", msg)
	}
}

func TestClassifyS3Error_SignatureDoesNotMatch_Terminal(t *testing.T) {
	fe := classifyOverHTTP(t, http.StatusForbidden, s3ErrorXMLBody("SignatureDoesNotMatch", "The request signature we calculated does not match"))
	if fe.Expired {
		t.Fatalf("SignatureDoesNotMatch must not be classified Expired: %+v", fe)
	}
	if !fe.Terminal {
		t.Fatalf("SignatureDoesNotMatch must be Terminal: %+v", fe)
	}
}

func TestClassifyS3Error_403GarbageBody_StatusFallbackTerminal(t *testing.T) {
	fe := classifyOverHTTP(t, http.StatusForbidden, "not xml at all")
	if fe.Expired {
		t.Fatalf("an unparseable 403 body must not be classified Expired: %+v", fe)
	}
	if !fe.Terminal {
		t.Fatalf("an unparseable 403 must fall back to Terminal by status: %+v", fe)
	}
}

func TestClassifyS3Error_403EmptyBody_StatusFallbackTerminal(t *testing.T) {
	fe := classifyOverHTTP(t, http.StatusForbidden, "")
	if fe.Expired || !fe.Terminal {
		t.Fatalf("empty-body 403 must be Terminal, not Expired: %+v", fe)
	}
}

func TestClassifyS3Error_404WithNoSuchKey_Terminal(t *testing.T) {
	fe := classifyOverHTTP(t, http.StatusNotFound, s3ErrorXMLBody("NoSuchKey", "Key not found"))
	if !fe.Terminal {
		t.Fatalf("404/NoSuchKey must be Terminal: %+v", fe)
	}
	if !strings.Contains(fe.Message(), "missing") {
		t.Fatalf("message %q does not say the object is missing", fe.Message())
	}
}

func TestClassifyS3Error_404NoBody_Terminal(t *testing.T) {
	fe := classifyOverHTTP(t, http.StatusNotFound, "")
	if !fe.Terminal {
		t.Fatalf("bare 404 must be Terminal: %+v", fe)
	}
	if !strings.Contains(fe.Message(), "missing") {
		t.Fatalf("message %q does not say the object is missing", fe.Message())
	}
}

// --- Request-count / retry semantics via Materialize (the retry loop, not just classification) ---

// terminalOverMaterialize starts an httptest server always returning status+body, runs it through
// Materialize (with no represign hook), and asserts the handler saw exactly one request — proving a
// Terminal classification short-circuits the retry loop instead of burning the retry budget.
func terminalOverMaterialize(t *testing.T, status int, body string) error {
	t.Helper()
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	defer srv.Close()

	st := ArtifactStager{Root: t.TempDir(), retryBase: time.Millisecond}
	sec := SecretInjector{Root: t.TempDir()}
	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", PresignedURL: srv.URL + "/obj", Sha256: "ignored"}

	err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := atomic.LoadInt32(&reqs); got != 1 {
		t.Fatalf("GET count = %d, want exactly 1 (Terminal must not burn the retry budget)", got)
	}
	return err
}

func TestFetchWithRetry_403AccessDenied_NoRetryBudgetBurned(t *testing.T) {
	err := terminalOverMaterialize(t, http.StatusForbidden, s3ErrorXMLBody("AccessDenied", "Forbidden: Invalid signature"))
	var fe *FetchError
	if !errors.As(err, &fe) || !fe.Terminal {
		t.Fatalf("expected a Terminal *FetchError, got %v", err)
	}
}

func TestFetchWithRetry_403SignatureDoesNotMatch_NoRetryBudgetBurned(t *testing.T) {
	terminalOverMaterialize(t, http.StatusForbidden, s3ErrorXMLBody("SignatureDoesNotMatch", "bad sig"))
}

// --- Re-presign seam ---

func TestFetchWithRetry_RepresignOnExpiry_Succeeds(t *testing.T) {
	var reqs int32
	files := map[string]struct {
		mode os.FileMode
		body string
	}{"f": {0o644, "ok"}}
	payload, hexSum := tarZstBytes(t, files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&reqs, 1)
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(s3ErrorXMLBody("InvalidRequest", "Bad request: Date is too old")))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	const objectKey = "skills/abc.tar.zst"
	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", ObjectKey: objectKey, PresignedURL: srv.URL + "/obj?X-Amz-Signature=old", Sha256: hexSum}

	var hookCalls int32
	represign := func(ctx context.Context, keys []string) (map[string]string, error) {
		atomic.AddInt32(&hookCalls, 1)
		if len(keys) != 1 || keys[0] != objectKey {
			t.Fatalf("represign called with unexpected keys: %v", keys)
		}
		return map[string]string{objectKey: srv.URL + "/obj?X-Amz-Signature=new"}, nil
	}

	st := ArtifactStager{Root: t.TempDir(), retryBase: time.Millisecond}
	sec := SecretInjector{Root: t.TempDir()}
	if err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil, represign); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if got := atomic.LoadInt32(&hookCalls); got != 1 {
		t.Fatalf("represign hook called %d times, want exactly 1", got)
	}
	if got := atomic.LoadInt32(&reqs); got != 2 {
		t.Fatalf("GET count = %d, want exactly 2 (expired attempt + retried-with-fresh-url attempt)", got)
	}
	staged, err := os.ReadFile(st.DirFor("sp1") + "/payloads/a/f")
	if err != nil || string(staged) != "ok" {
		t.Fatalf("artifact not staged after re-presign: content=%q err=%v", staged, err)
	}
}

func TestFetchWithRetry_RepresignStillExpired_TerminalNoInfiniteLoop(t *testing.T) {
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(s3ErrorXMLBody("InvalidRequest", "Bad request: Date is too old")))
	}))
	defer srv.Close()

	const objectKey = "skills/abc.tar.zst"
	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", ObjectKey: objectKey, PresignedURL: srv.URL + "/obj", Sha256: "ignored"}

	var hookCalls int32
	represign := func(ctx context.Context, keys []string) (map[string]string, error) {
		atomic.AddInt32(&hookCalls, 1)
		return map[string]string{objectKey: srv.URL + "/obj"}, nil // still expired
	}

	st := ArtifactStager{Root: t.TempDir(), retryBase: time.Millisecond}
	sec := SecretInjector{Root: t.TempDir()}
	err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil, represign)
	if err == nil {
		t.Fatal("expected a terminal error (no infinite re-presign loop)")
	}
	var fe *FetchError
	if !errors.As(err, &fe) || !fe.Terminal {
		t.Fatalf("expected a Terminal *FetchError, got %v", err)
	}
	if got := atomic.LoadInt32(&hookCalls); got != 1 {
		t.Fatalf("represign hook called %d times, want exactly 1 (one re-presign round per artifact)", got)
	}
	if got := atomic.LoadInt32(&reqs); got != 2 {
		t.Fatalf("GET count = %d, want exactly 2 (initial expired + one post-represign attempt, no loop)", got)
	}
}

func TestFetchWithRetry_ExpiredNilHook_Terminal(t *testing.T) {
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(s3ErrorXMLBody("InvalidRequest", "Bad request: Date is too old")))
	}))
	defer srv.Close()

	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", ObjectKey: "skills/abc.tar.zst", PresignedURL: srv.URL + "/obj", Sha256: "ignored"}

	st := ArtifactStager{Root: t.TempDir(), retryBase: time.Millisecond}
	sec := SecretInjector{Root: t.TempDir()}
	err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil, nil)
	if err == nil {
		t.Fatal("expected a terminal error")
	}
	var fe *FetchError
	if !errors.As(err, &fe) || !fe.Terminal {
		t.Fatalf("expected a Terminal *FetchError, got %v", err)
	}
	msg := fe.Message()
	if !strings.Contains(msg, "presigned URL expired") {
		t.Fatalf("message %q does not say the presign expired", msg)
	}
	if !strings.Contains(msg, "re-presign unavailable") {
		t.Fatalf("message %q does not say re-presign is unavailable", msg)
	}
	if got := atomic.LoadInt32(&reqs); got != 1 {
		t.Fatalf("GET count = %d, want exactly 1 (no hook -> no retry, no budget burn)", got)
	}
}

func TestFetchWithRetry_ExpiredHookError_Terminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(s3ErrorXMLBody("InvalidRequest", "Bad request: Date is too old")))
	}))
	defer srv.Close()

	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", ObjectKey: "skills/abc.tar.zst", PresignedURL: srv.URL + "/obj", Sha256: "ignored"}
	represign := func(ctx context.Context, keys []string) (map[string]string, error) {
		return nil, errors.New("attach stream unavailable")
	}

	st := ArtifactStager{Root: t.TempDir(), retryBase: time.Millisecond}
	sec := SecretInjector{Root: t.TempDir()}
	err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil, represign)
	var fe *FetchError
	if !errors.As(err, &fe) || !fe.Terminal {
		t.Fatalf("expected a Terminal *FetchError, got %v", err)
	}
	if strings.Contains(err.Error(), "attach stream unavailable") {
		t.Fatalf("represign hook's raw error must not be echoed into the persisted message: %v", err)
	}
}

func TestFetchWithRetry_ExpiredHookMissingKey_Terminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(s3ErrorXMLBody("InvalidRequest", "Bad request: Date is too old")))
	}))
	defer srv.Close()

	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", ObjectKey: "skills/abc.tar.zst", PresignedURL: srv.URL + "/obj", Sha256: "ignored"}
	represign := func(ctx context.Context, keys []string) (map[string]string, error) {
		return map[string]string{}, nil // map missing the requested key
	}

	st := ArtifactStager{Root: t.TempDir(), retryBase: time.Millisecond}
	sec := SecretInjector{Root: t.TempDir()}
	err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil, represign)
	var fe *FetchError
	if !errors.As(err, &fe) || !fe.Terminal {
		t.Fatalf("expected a Terminal *FetchError, got %v", err)
	}
}

func TestFetchWithRetry_RepresignThenRetryBudgetStillApplies(t *testing.T) {
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&reqs, 1)
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(s3ErrorXMLBody("InvalidRequest", "Bad request: Date is too old")))
			return
		}
		w.WriteHeader(http.StatusInternalServerError) // every post-represign attempt fails retryably
	}))
	defer srv.Close()

	const objectKey = "skills/abc.tar.zst"
	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", ObjectKey: objectKey, PresignedURL: srv.URL + "/obj", Sha256: "ignored"}
	represign := func(ctx context.Context, keys []string) (map[string]string, error) {
		return map[string]string{objectKey: srv.URL + "/obj2"}, nil
	}

	st := ArtifactStager{Root: t.TempDir(), retryBase: time.Millisecond}
	sec := SecretInjector{Root: t.TempDir()}
	err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil, represign)
	if err == nil {
		t.Fatal("expected retry exhaustion after the re-presigned URL keeps 500ing")
	}
	want := int32(1 + retryAttempts) // 1 expired attempt (free) + the full retry budget post-represign
	if got := atomic.LoadInt32(&reqs); got != want {
		t.Fatalf("GET count = %d, want exactly %d (re-presign must not grant a fresh retry budget)", got, want)
	}
}

// --- No-leak: the presigned URL (and its X-Amz-Signature) must never reach Materialize's error ---

const leakySignatureQuery = "X-Amz-Signature=DEADBEEFCAFE&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE"

func assertNoLeak(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "X-Amz-Signature") || strings.Contains(msg, "DEADBEEFCAFE") || strings.Contains(msg, leakySignatureQuery) {
		t.Fatalf("error leaks the presigned URL query: %v", err)
	}
}

func TestMaterialize_NoLeak_TransportError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	presigned := fmt.Sprintf("http://%s/skills/x.tar.zst?%s", addr, leakySignatureQuery)
	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", PresignedURL: presigned, Sha256: "ignored"}

	st := ArtifactStager{Root: t.TempDir(), retryBase: time.Millisecond}
	sec := SecretInjector{Root: t.TempDir()}
	err = st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil, nil)
	assertNoLeak(t, err)
	if !strings.Contains(err.Error(), "Garage unreachable") {
		t.Fatalf("error does not name Garage unreachable: %v", err)
	}
}

func TestMaterialize_NoLeak_Terminal403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(s3ErrorXMLBody("AccessDenied", "Forbidden: Invalid signature")))
	}))
	defer srv.Close()

	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", PresignedURL: srv.URL + "/skills/x.tar.zst?" + leakySignatureQuery, Sha256: "ignored"}
	st := ArtifactStager{Root: t.TempDir(), retryBase: time.Millisecond}
	sec := SecretInjector{Root: t.TempDir()}
	err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil, nil)
	assertNoLeak(t, err)
}

func TestMaterialize_NoLeak_Terminal404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", PresignedURL: srv.URL + "/skills/x.tar.zst?" + leakySignatureQuery, Sha256: "ignored"}
	st := ArtifactStager{Root: t.TempDir(), retryBase: time.Millisecond}
	sec := SecretInjector{Root: t.TempDir()}
	err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil, nil)
	assertNoLeak(t, err)
}

func TestMaterialize_NoLeak_BadPresignedURLParseFailure(t *testing.T) {
	// A control character makes http.NewRequestWithContext's internal url.Parse fail; the resulting
	// *url.Error embeds the full URL (query and all) unless redacted at the source. This is the LIVE
	// BUG the roast flagged: this path is Terminal and previously returned that raw error unmodified.
	presigned := "http://example.invalid/skills/x.tar.zst?" + leakySignatureQuery + "\x01"
	art := Artifact{ID: "a", ContentType: ArtifactTar, DestPath: "payloads/a", PresignedURL: presigned, Sha256: "ignored"}

	st := ArtifactStager{Root: t.TempDir(), retryBase: time.Millisecond}
	sec := SecretInjector{Root: t.TempDir()}
	err := st.Materialize(context.Background(), "sp1", []Artifact{art}, sec, nil, nil)
	assertNoLeak(t, err)
	var fe *FetchError
	if !errors.As(err, &fe) || !fe.Terminal {
		t.Fatalf("expected a Terminal *FetchError, got %v", err)
	}
}

// --- redactURLErr / SafeErrorMessage ---

func TestRedactURLErr_DropsQueryFromURLError(t *testing.T) {
	orig := &url.Error{Op: "Get", URL: "https://garage.example/skills/x.tar.zst?" + leakySignatureQuery, Err: errors.New("boom")}
	got := redactURLErr(orig)
	if got.Error() == orig.Error() {
		t.Fatalf("redactURLErr did not change the error")
	}
	if strings.Contains(got.Error(), "DEADBEEFCAFE") {
		t.Fatalf("redacted error still leaks the signature: %v", got)
	}
	if !strings.Contains(got.Error(), "garage.example") {
		t.Fatalf("redacted error lost the host, which is useful for diagnosis: %v", got)
	}
}

func TestRedactURLErr_PassesThroughNonURLError(t *testing.T) {
	orig := errors.New("plain error")
	if got := redactURLErr(orig); got != orig {
		t.Fatalf("expected a non-*url.Error to pass through unchanged, got %v", got)
	}
	if redactURLErr(nil) != nil {
		t.Fatalf("expected nil to pass through as nil")
	}
}

func TestSafeErrorMessage_ExtractsFetchErrorMsgOnly(t *testing.T) {
	fe := terminalFetch("skill object fetch denied (HTTP 403, AccessDenied): likely clock skew",
		&url.Error{Op: "Get", URL: "https://garage.example/x?" + leakySignatureQuery, Err: errors.New("boom")})
	wrapped := fmt.Errorf("artifacts: stage %q: %w", "skill-id", fe)
	got := SafeErrorMessage(wrapped)
	if got != fe.Message() {
		t.Fatalf("SafeErrorMessage = %q, want the bare FetchError message %q", got, fe.Message())
	}
	if strings.Contains(got, "DEADBEEFCAFE") || strings.Contains(got, "garage.example") {
		t.Fatalf("SafeErrorMessage leaked the wrapped url.Error: %q", got)
	}
}

func TestSafeErrorMessage_FallsBackForNonFetchError(t *testing.T) {
	err := errors.New("plain error, no FetchError in the chain")
	if got := SafeErrorMessage(err); got != err.Error() {
		t.Fatalf("SafeErrorMessage = %q, want %q", got, err.Error())
	}
}
