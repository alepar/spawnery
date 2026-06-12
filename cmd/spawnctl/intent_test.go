package main

// intent_test.go covers the AM1 client-side validation in pollAndSign [AM1] and the
// provisionWithIntent NACK-retry orchestration [AC1].

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/intent"
)

// fakeIntentClient returns a ready PendingIntent immediately on GetPendingIntent.
// SubmitIntent always succeeds (we test that pollAndSign rejects before submitting).
type fakeIntentClient struct {
	pending  *cpv1.PendingIntent
	submitted bool
}

func (f *fakeIntentClient) GetPendingIntent(_ context.Context, _ *connect.Request[cpv1.GetPendingIntentRequest]) (*connect.Response[cpv1.GetPendingIntentResponse], error) {
	return connect.NewResponse(&cpv1.GetPendingIntentResponse{Ready: true, Pending: f.pending}), nil
}

func (f *fakeIntentClient) SubmitIntent(_ context.Context, _ *connect.Request[cpv1.SubmitIntentRequest]) (*connect.Response[cpv1.SubmitIntentResponse], error) {
	f.submitted = true
	return connect.NewResponse(&cpv1.SubmitIntentResponse{}), nil
}

// TestAM1AppRefSubstitutionRejected: CP echoes a different app_ref than the user requested.
func TestAM1AppRefSubstitutionRejected(t *testing.T) {
	ic := &fakeIntentClient{pending: &cpv1.PendingIntent{
		Op:      "create-spawn",
		SpawnId: "sp-1",
		AppRef:  "evil-app/ref",
		Model:   "claude-3",
	}}
	err := pollAndSign(context.Background(), ic, "sp-1", intentParams{AppRef: "myapp/ref", Model: "claude-3"})
	if err == nil {
		t.Fatal("expected AM1 rejection but got nil error")
	}
	if !strings.Contains(err.Error(), "AM1") || !strings.Contains(err.Error(), "app_ref") {
		t.Fatalf("expected AM1 app_ref error, got: %v", err)
	}
	if ic.submitted {
		t.Fatal("must not submit intent after AM1 rejection")
	}
}

// TestAM1ModelSubstitutionRejected: CP echoes a different model than the user requested.
func TestAM1ModelSubstitutionRejected(t *testing.T) {
	ic := &fakeIntentClient{pending: &cpv1.PendingIntent{
		Op:      "create-spawn",
		SpawnId: "sp-1",
		AppRef:  "myapp/ref",
		Model:   "gpt-5-malicious",
	}}
	err := pollAndSign(context.Background(), ic, "sp-1", intentParams{AppRef: "myapp/ref", Model: "claude-3"})
	if err == nil {
		t.Fatal("expected AM1 rejection but got nil error")
	}
	if !strings.Contains(err.Error(), "AM1") || !strings.Contains(err.Error(), "model") {
		t.Fatalf("expected AM1 model error, got: %v", err)
	}
	if ic.submitted {
		t.Fatal("must not submit intent after AM1 rejection")
	}
}

// TestAM1TargetNodeSubstitutionRejected: CP echoes a different target_node_id than requested.
func TestAM1TargetNodeSubstitutionRejected(t *testing.T) {
	ic := &fakeIntentClient{pending: &cpv1.PendingIntent{
		Op:           "migrate-spawn",
		SpawnId:      "sp-1",
		TargetNodeId: "node-attacker",
	}}
	err := pollAndSign(context.Background(), ic, "sp-1", intentParams{TargetNodeID: "node-requested"})
	if err == nil {
		t.Fatal("expected AM1 rejection but got nil error")
	}
	if !strings.Contains(err.Error(), "AM1") || !strings.Contains(err.Error(), "target_node_id") {
		t.Fatalf("expected AM1 target_node_id error, got: %v", err)
	}
	if ic.submitted {
		t.Fatal("must not submit intent after AM1 rejection")
	}
}

// TestAM1CloudTargetSkipsNodeValidation: when TargetNodeID is empty (cloud placement),
// any target_node_id from the CP is accepted — the CP selects the actual node.
func TestAM1CloudTargetSkipsNodeValidation(t *testing.T) {
	ic := &fakeIntentClient{pending: &cpv1.PendingIntent{
		Op:           "migrate-spawn",
		SpawnId:      "sp-1",
		TargetNodeId: "node-cp-selected",
	}}
	// No TargetNodeID in params -> cloud placement, no validation.
	err := pollAndSign(context.Background(), ic, "sp-1", intentParams{})
	// pollAndSign may still fail (e.g. crypto), but NOT with an AM1 error.
	if err != nil && strings.Contains(err.Error(), "AM1") {
		t.Fatalf("cloud target should not produce AM1 error, got: %v", err)
	}
}

// TestAM1MatchingParamsAccepted: when CP echoes exactly the requested params, no AM1 error.
func TestAM1MatchingParamsAccepted(t *testing.T) {
	ic := &fakeIntentClient{pending: &cpv1.PendingIntent{
		Op:      "create-spawn",
		SpawnId: "sp-1",
		AppRef:  "myapp/ref",
		Model:   "claude-3",
	}}
	err := pollAndSign(context.Background(), ic, "sp-1", intentParams{AppRef: "myapp/ref", Model: "claude-3"})
	// No AM1 error; may fail for other reasons (build intent etc.) but not AM1.
	if err != nil && strings.Contains(err.Error(), "AM1") {
		t.Fatalf("matching params should not produce AM1 error, got: %v", err)
	}
}

// ---- provisionWithIntent tests [AC1] ------------------------------------------------

// capturingIntentClient records each SubmitIntent call (the SpawnId + the JTI from the
// signed body) so tests can assert distinct JTIs across retry rounds.
type capturingIntentClient struct {
	mu       sync.Mutex
	jtisSeen []string
	pending  *cpv1.PendingIntent
}

func (c *capturingIntentClient) GetPendingIntent(_ context.Context, _ *connect.Request[cpv1.GetPendingIntentRequest]) (*connect.Response[cpv1.GetPendingIntentResponse], error) {
	return connect.NewResponse(&cpv1.GetPendingIntentResponse{Ready: true, Pending: c.pending}), nil
}

func (c *capturingIntentClient) SubmitIntent(_ context.Context, req *connect.Request[cpv1.SubmitIntentRequest]) (*connect.Response[cpv1.SubmitIntentResponse], error) {
	jti := ""
	if si := req.Msg.Intent; si != nil {
		if body, err := intent.ParseBody(si.Body); err == nil {
			jti = body.Jti
		}
	}
	c.mu.Lock()
	c.jtisSeen = append(c.jtisSeen, jti)
	c.mu.Unlock()
	return connect.NewResponse(&cpv1.SubmitIntentResponse{}), nil
}

func (c *capturingIntentClient) submitCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.jtisSeen)
}

// waitSubmit blocks until the capturingIntentClient has received at least n SubmitIntent
// calls, or until the deadline. Returns the actual count when the condition is met or the
// deadline expires.
func waitSubmit(t *testing.T, ic *capturingIntentClient, n int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := ic.submitCount(); got >= n {
			return got
		}
		if time.Now().After(deadline) {
			return ic.submitCount()
		}
		time.Sleep(time.Millisecond)
	}
}

// TestProvisionWithIntentHappyPath: doRPC blocks until pollAndSign submits (mimicking the
// CP blocking on pendingIntents.await), then succeeds. SubmitIntent must be called once.
func TestProvisionWithIntentHappyPath(t *testing.T) {
	ic := &capturingIntentClient{pending: &cpv1.PendingIntent{
		Op:      "create-spawn",
		SpawnId: "sp-happy",
	}}
	var called int32
	// Mimic real CP behavior: doRPC blocks until pollAndSign calls SubmitIntent, then
	// returns. This is the invariant that makes provisionWithIntent correct in production.
	doRPC := func(_ context.Context) error {
		waitSubmit(t, ic, 1)
		atomic.AddInt32(&called, 1)
		return nil
	}
	if err := provisionWithIntent(context.Background(), ic, "sp-happy", intentParams{}, doRPC); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if atomic.LoadInt32(&called) != 1 {
		t.Fatalf("doRPC called %d times, want 1", called)
	}
	if got := ic.submitCount(); got != 1 {
		t.Fatalf("SubmitIntent called %d times, want 1", got)
	}
}

// TestProvisionWithIntentRetryableNACKRetries: doRPC returns a STALE NACK on the first
// call and succeeds on the second. provisionWithIntent retries exactly once; the two
// SubmitIntent calls must carry distinct JTIs (fresh key + jti per pollAndSign call).
func TestProvisionWithIntentRetryableNACKRetries(t *testing.T) {
	ic := &capturingIntentClient{pending: &cpv1.PendingIntent{
		Op:      "migrate-spawn",
		SpawnId: "sp-retry",
	}}
	var callCount int32
	doRPC := func(_ context.Context) error {
		n := atomic.AddInt32(&callCount, 1)
		// Block until the n-th SubmitIntent arrives (simulates CP blocking on await).
		waitSubmit(t, ic, int(n))
		if n == 1 {
			// First call: return a retryable NACK (STALE).
			return connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("%s: intent is 35s old (max 90s+30s); node time: 1770000000", intent.NACKStale))
		}
		return nil // second call succeeds
	}
	if err := provisionWithIntent(context.Background(), ic, "sp-retry", intentParams{}, doRPC); err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if atomic.LoadInt32(&callCount) != 2 {
		t.Fatalf("doRPC called %d times, want 2 (first NACK + retry)", callCount)
	}
	// Both rounds must have called SubmitIntent with distinct JTIs.
	if got := ic.submitCount(); got != 2 {
		t.Fatalf("SubmitIntent called %d times, want 2", got)
	}
	ic.mu.Lock()
	j0, j1 := ic.jtisSeen[0], ic.jtisSeen[1]
	ic.mu.Unlock()
	if j0 == "" || j1 == "" || j0 == j1 {
		t.Fatalf("JTIs must be non-empty and distinct across retry rounds: %q, %q", j0, j1)
	}
}

// TestProvisionWithIntentNonRetryableNACKFails: a CORRESPONDENCE NACK is non-retryable;
// provisionWithIntent returns the error without a second attempt.
func TestProvisionWithIntentNonRetryableNACKFails(t *testing.T) {
	ic := &capturingIntentClient{pending: &cpv1.PendingIntent{
		Op:      "migrate-spawn",
		SpawnId: "sp-nonretry",
	}}
	var callCount int32
	doRPC := func(_ context.Context) error {
		n := atomic.AddInt32(&callCount, 1)
		waitSubmit(t, ic, int(n))
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s: image: intent=\"img-a\" exec=\"img-b\"", intent.NACKCorrespondence))
	}
	err := provisionWithIntent(context.Background(), ic, "sp-nonretry", intentParams{}, doRPC)
	if err == nil {
		t.Fatal("expected error for non-retryable NACK, got nil")
	}
	if atomic.LoadInt32(&callCount) != 1 {
		t.Fatalf("doRPC called %d times for non-retryable NACK, want 1 (no retry)", callCount)
	}
	var connErr *connect.Error
	if !errors.As(err, &connErr) || connErr.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got: %v", err)
	}
}

