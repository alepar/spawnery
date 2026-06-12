package main

// intent_test.go covers the AM1 client-side validation in pollAndSign [AM1]:
// the client must reject a PendingIntent that does not match the originating request.

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
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
