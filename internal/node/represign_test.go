package node

import (
	"context"
	"strings"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/runtime/fakepod"
)

// newRepresignTestAttacher returns an attacher wired for represignFunc tests: newAttacher's
// baseline plus an initialized represigns map (newAttacher doesn't set one — it's only needed by
// tests that exercise the re-presign seam).
func newRepresignTestAttacher(t *testing.T, fs *fakeCPStream) *attacher {
	t.Helper()
	be := fakeBackend(t, fakepod.WithAttachScript(scriptGoose))
	a := newAttacher(newGooseManager(t, be), fs)
	a.represigns = newRepresignPending()
	return a
}

// lastRepresignRequest returns the most recent RepresignArtifactsRequest sent (nil if none).
func lastRepresignRequest(f *fakeCPStream) *nodev1.RepresignArtifactsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if r := f.sent[i].GetRepresignArtifactsRequest(); r != nil {
			return r
		}
	}
	return nil
}

// allRepresignRequests returns every RepresignArtifactsRequest sent, in send order.
func allRepresignRequests(f *fakeCPStream) []*nodev1.RepresignArtifactsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*nodev1.RepresignArtifactsRequest
	for _, m := range f.sent {
		if r := m.GetRepresignArtifactsRequest(); r != nil {
			out = append(out, r)
		}
	}
	return out
}

func represignResponse(requestID, spawnID, key, url, errMsg string) *nodev1.CPMessage {
	resp := &nodev1.RepresignArtifactsResponse{RequestId: requestID, SpawnId: spawnID, Error: errMsg}
	if key != "" {
		resp.Objects = []*nodev1.PresignedObject{{ObjectKey: key, PresignedUrl: url}}
	}
	return &nodev1.CPMessage{Msg: &nodev1.CPMessage_RepresignArtifactsResponse{RepresignArtifactsResponse: resp}}
}

// TestRepresignFunc_SendsRequestAndResolvesResponse: represignFunc sends a
// RepresignArtifactsRequest carrying spawn_id/generation/keys, and the demux (a.handle) routes the
// matching response back to the waiting call.
func TestRepresignFunc_SendsRequestAndResolvesResponse(t *testing.T) {
	fs := &fakeCPStream{}
	a := newRepresignTestAttacher(t, fs)

	fn := a.represignFunc("sp1", 7)
	done := make(chan struct{})
	var got map[string]string
	var gotErr error
	go func() {
		got, gotErr = fn(context.Background(), []string{"skills/aaa.tar.zst", "skills/bbb.tar.zst"})
		close(done)
	}()

	var req *nodev1.RepresignArtifactsRequest
	waitFor(t, "RepresignArtifactsRequest sent", func() bool {
		req = lastRepresignRequest(fs)
		return req != nil
	})
	if req.SpawnId != "sp1" || req.Generation != 7 {
		t.Fatalf("request spawn/gen = %s/%d, want sp1/7", req.SpawnId, req.Generation)
	}
	if len(req.ObjectKeys) != 2 || req.ObjectKeys[0] != "skills/aaa.tar.zst" || req.ObjectKeys[1] != "skills/bbb.tar.zst" {
		t.Fatalf("object_keys = %v", req.ObjectKeys)
	}
	if req.RequestId == "" {
		t.Fatal("request_id must be non-empty")
	}

	resp := &nodev1.RepresignArtifactsResponse{
		RequestId: req.RequestId, SpawnId: "sp1",
		Objects: []*nodev1.PresignedObject{
			{ObjectKey: "skills/aaa.tar.zst", PresignedUrl: "https://x/aaa"},
			{ObjectKey: "skills/bbb.tar.zst", PresignedUrl: "https://x/bbb"},
		},
	}
	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_RepresignArtifactsResponse{RepresignArtifactsResponse: resp}})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for represignFunc to return")
	}
	if gotErr != nil {
		t.Fatalf("err: %v", gotErr)
	}
	if got["skills/aaa.tar.zst"] != "https://x/aaa" || got["skills/bbb.tar.zst"] != "https://x/bbb" {
		t.Fatalf("got %+v", got)
	}
}

// TestRepresignFunc_ConcurrentRequestsResolveOutOfOrder: two concurrent represignFunc calls each
// get their own response even when the CP answers out of send order.
func TestRepresignFunc_ConcurrentRequestsResolveOutOfOrder(t *testing.T) {
	fs := &fakeCPStream{}
	a := newRepresignTestAttacher(t, fs)

	fn := a.represignFunc("sp1", 1)
	done1, done2 := make(chan struct{}), make(chan struct{})
	var got1, got2 map[string]string
	var err1, err2 error
	go func() { got1, err1 = fn(context.Background(), []string{"k1"}); close(done1) }()
	go func() { got2, err2 = fn(context.Background(), []string{"k2"}); close(done2) }()

	var reqs []*nodev1.RepresignArtifactsRequest
	waitFor(t, "two requests sent", func() bool {
		reqs = allRepresignRequests(fs)
		return len(reqs) == 2
	})
	var reqK1, reqK2 *nodev1.RepresignArtifactsRequest
	for _, r := range reqs {
		switch r.ObjectKeys[0] {
		case "k1":
			reqK1 = r
		case "k2":
			reqK2 = r
		}
	}
	if reqK1 == nil || reqK2 == nil {
		t.Fatalf("expected one request per key, got %+v", reqs)
	}

	// Respond OUT OF ORDER: k2's response arrives before k1's.
	a.handle(context.Background(), represignResponse(reqK2.RequestId, "sp1", "k2", "https://x/k2", ""))
	a.handle(context.Background(), represignResponse(reqK1.RequestId, "sp1", "k1", "https://x/k1", ""))

	for _, ch := range []chan struct{}{done1, done2} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for a represignFunc call to return")
		}
	}
	if err1 != nil || err2 != nil {
		t.Fatalf("err1=%v err2=%v", err1, err2)
	}
	if got1["k1"] != "https://x/k1" {
		t.Fatalf("got1 = %+v", got1)
	}
	if got2["k2"] != "https://x/k2" {
		t.Fatalf("got2 = %+v", got2)
	}
}

// TestRepresignPending_UnknownRequestIDDropped: a response naming an unrecognized request_id must
// not panic and must not resolve a DIFFERENT (still-pending) waiter.
func TestRepresignPending_UnknownRequestIDDropped(t *testing.T) {
	fs := &fakeCPStream{}
	a := newRepresignTestAttacher(t, fs)
	a.represignRequestTimeout = 5 * time.Second // long enough that a bogus early-resolve would be caught

	fn := a.represignFunc("sp1", 1)
	done := make(chan struct{})
	var gotErr error
	go func() { _, gotErr = fn(context.Background(), []string{"k1"}); close(done) }()

	waitFor(t, "request sent", func() bool { return lastRepresignRequest(fs) != nil })

	// An unknown request_id must be dropped without panicking and without touching the real waiter.
	a.handle(context.Background(), represignResponse("bogus-request-id", "sp1", "k1", "https://x/k1", ""))

	select {
	case <-done:
		t.Fatal("represignFunc returned before its own response arrived — the unknown request_id response must have been (incorrectly) matched")
	case <-time.After(80 * time.Millisecond):
	}

	req := lastRepresignRequest(fs)
	a.handle(context.Background(), represignResponse(req.RequestId, "sp1", "k1", "https://x/k1", ""))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for represignFunc to return after its real response")
	}
	if gotErr != nil {
		t.Fatalf("err: %v", gotErr)
	}
}

// TestRepresignFunc_CPRefusalReturnsError: a response with Error set is surfaced as an error.
func TestRepresignFunc_CPRefusalReturnsError(t *testing.T) {
	fs := &fakeCPStream{}
	a := newRepresignTestAttacher(t, fs)

	fn := a.represignFunc("sp1", 1)
	done := make(chan struct{})
	var gotErr error
	go func() { _, gotErr = fn(context.Background(), []string{"k1"}); close(done) }()

	req := waitForRepresignRequest(t, fs)
	a.handle(context.Background(), represignResponse(req.RequestId, "sp1", "", "", "object key not bound to this spawn"))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "object key not bound to this spawn") {
		t.Fatalf("err = %v, want it to mention the CP's refusal reason", gotErr)
	}
}

// TestRepresignFunc_SpawnIDMismatchReturnsError: a response for a different spawn_id (a
// cross-connection mixup, defensively rejected) is treated as an error, not a silent success.
func TestRepresignFunc_SpawnIDMismatchReturnsError(t *testing.T) {
	fs := &fakeCPStream{}
	a := newRepresignTestAttacher(t, fs)

	fn := a.represignFunc("sp1", 1)
	done := make(chan struct{})
	var gotErr error
	go func() { _, gotErr = fn(context.Background(), []string{"k1"}); close(done) }()

	req := waitForRepresignRequest(t, fs)
	a.handle(context.Background(), represignResponse(req.RequestId, "sp-DIFFERENT", "k1", "https://x/k1", ""))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "sp-DIFFERENT") {
		t.Fatalf("err = %v, want it to mention the spawn_id mismatch", gotErr)
	}
}

// TestRepresignPending_FailAllUnblocksWaiterImmediately: failAll (simulating a CP disconnect)
// unblocks an in-flight represignFunc call right away, without waiting for represignRequestTimeout.
func TestRepresignPending_FailAllUnblocksWaiterImmediately(t *testing.T) {
	fs := &fakeCPStream{}
	a := newRepresignTestAttacher(t, fs)
	a.represignRequestTimeout = 5 * time.Second // long — proves failAll (not the timeout) unblocked it

	fn := a.represignFunc("sp1", 1)
	start := time.Now()
	done := make(chan struct{})
	var gotErr error
	go func() { _, gotErr = fn(context.Background(), []string{"k1"}); close(done) }()

	waitForRepresignRequest(t, fs)
	a.represigns.failAll()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("failAll did not unblock the waiter")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %s to unblock — failAll should be near-instant", elapsed)
	}
	if gotErr == nil {
		t.Fatal("want an error after failAll (CP connection lost)")
	}
}

// TestRepresignFunc_TimesOutWithoutAResponse: represignFunc gives up after its (shrunk, per-test)
// timeout when the CP never answers.
func TestRepresignFunc_TimesOutWithoutAResponse(t *testing.T) {
	fs := &fakeCPStream{}
	a := newRepresignTestAttacher(t, fs)
	a.represignRequestTimeout = 30 * time.Millisecond

	start := time.Now()
	_, err := a.represignFunc("sp1", 1)(context.Background(), []string{"k1"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want a timeout error")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("took %s, want ~30ms (represignRequestTimeout)", elapsed)
	}
}

func waitForRepresignRequest(t *testing.T, fs *fakeCPStream) *nodev1.RepresignArtifactsRequest {
	t.Helper()
	var req *nodev1.RepresignArtifactsRequest
	waitFor(t, "RepresignArtifactsRequest sent", func() bool {
		req = lastRepresignRequest(fs)
		return req != nil
	})
	return req
}
