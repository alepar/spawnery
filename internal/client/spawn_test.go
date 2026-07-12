package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
)

// fakeSpawnClient is a scripted spawnClient: each ListSpawns call pops the next canned summary
// list off a queue (so WaitActive tests can script a starting->active or starting->error
// sequence), and records the requests made to the other RPCs.
type fakeSpawnClient struct {
	mu sync.Mutex

	listQueue [][]*cpv1.SpawnSummary // ListSpawns pops one entry per call; last entry repeats

	resumeErr    error
	suspendErr   error
	setModelResp *cpv1.SetSpawnModelResponse
	setModelErr  error
	deleteErr    error
	stopErr      error

	gotResume    *cpv1.ResumeSpawnRequest
	gotCreate    *cpv1.CreateSpawnRequest
	gotSuspend   *cpv1.SuspendSpawnRequest
	gotSetModel  *cpv1.SetSpawnModelRequest
	gotDelete    *cpv1.DeleteSpawnRequest
	gotStop      *cpv1.StopSpawnRequest
	listCalls    int
	pendingReady bool
}

func (f *fakeSpawnClient) GetPendingIntent(_ context.Context, _ *connect.Request[cpv1.GetPendingIntentRequest]) (*connect.Response[cpv1.GetPendingIntentResponse], error) {
	return connect.NewResponse(&cpv1.GetPendingIntentResponse{Ready: f.pendingReady}), nil
}

func (f *fakeSpawnClient) SubmitIntent(_ context.Context, _ *connect.Request[cpv1.SubmitIntentRequest]) (*connect.Response[cpv1.SubmitIntentResponse], error) {
	return connect.NewResponse(&cpv1.SubmitIntentResponse{}), nil
}

func (f *fakeSpawnClient) CreateSpawn(_ context.Context, req *connect.Request[cpv1.CreateSpawnRequest]) (*connect.Response[cpv1.CreateSpawnResponse], error) {
	f.gotCreate = req.Msg
	return connect.NewResponse(&cpv1.CreateSpawnResponse{SpawnId: "sp-created"}), nil
}

func (f *fakeSpawnClient) ListSpawns(_ context.Context, _ *connect.Request[cpv1.ListSpawnsRequest]) (*connect.Response[cpv1.ListSpawnsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := f.listCalls
	if idx >= len(f.listQueue) {
		idx = len(f.listQueue) - 1
	}
	f.listCalls++
	return connect.NewResponse(&cpv1.ListSpawnsResponse{Spawns: f.listQueue[idx]}), nil
}

func (f *fakeSpawnClient) ResumeSpawn(_ context.Context, req *connect.Request[cpv1.ResumeSpawnRequest]) (*connect.Response[cpv1.ResumeSpawnResponse], error) {
	f.gotResume = req.Msg
	if f.resumeErr != nil {
		return nil, f.resumeErr
	}
	return connect.NewResponse(&cpv1.ResumeSpawnResponse{}), nil
}

func (f *fakeSpawnClient) SuspendSpawn(_ context.Context, req *connect.Request[cpv1.SuspendSpawnRequest]) (*connect.Response[cpv1.SuspendSpawnResponse], error) {
	f.gotSuspend = req.Msg
	if f.suspendErr != nil {
		return nil, f.suspendErr
	}
	return connect.NewResponse(&cpv1.SuspendSpawnResponse{}), nil
}

func (f *fakeSpawnClient) SetSpawnModel(_ context.Context, req *connect.Request[cpv1.SetSpawnModelRequest]) (*connect.Response[cpv1.SetSpawnModelResponse], error) {
	f.gotSetModel = req.Msg
	if f.setModelErr != nil {
		return nil, f.setModelErr
	}
	return connect.NewResponse(f.setModelResp), nil
}

func (f *fakeSpawnClient) DeleteSpawn(_ context.Context, req *connect.Request[cpv1.DeleteSpawnRequest]) (*connect.Response[cpv1.DeleteSpawnResponse], error) {
	f.gotDelete = req.Msg
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return connect.NewResponse(&cpv1.DeleteSpawnResponse{}), nil
}

func (f *fakeSpawnClient) StopSpawn(_ context.Context, req *connect.Request[cpv1.StopSpawnRequest]) (*connect.Response[cpv1.StopSpawnResponse], error) {
	f.gotStop = req.Msg
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	return connect.NewResponse(&cpv1.StopSpawnResponse{}), nil
}

func TestWaitActiveReturnsGenerationOnActive(t *testing.T) {
	f := &fakeSpawnClient{listQueue: [][]*cpv1.SpawnSummary{
		{{SpawnId: "sp-1", Status: cpv1.SpawnStatus_SPAWN_STATUS_STARTING}},
		{{SpawnId: "sp-1", Status: cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE, Generation: 7}},
	}}
	var polled []cpv1.SpawnStatus
	gen, err := waitActive(context.Background(), f, "sp-1", func(s *cpv1.SpawnSummary) {
		polled = append(polled, s.Status)
	})
	if err != nil {
		t.Fatalf("waitActive: %v", err)
	}
	if gen != 7 {
		t.Fatalf("generation = %d, want 7", gen)
	}
	if len(polled) != 2 {
		t.Fatalf("onPoll called %d times, want 2", len(polled))
	}
}

func TestWaitActiveReturnsTerminalErrorOnError(t *testing.T) {
	f := &fakeSpawnClient{listQueue: [][]*cpv1.SpawnSummary{
		{{SpawnId: "sp-1", Status: cpv1.SpawnStatus_SPAWN_STATUS_ERROR, ErrorStep: "pull-image"}},
	}}
	_, err := waitActive(context.Background(), f, "sp-1", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var termErr *TerminalError
	if !errors.As(err, &termErr) {
		t.Fatalf("expected *TerminalError, got %T: %v", err, err)
	}
	if termErr.Summary.GetErrorStep() != "pull-image" {
		t.Fatalf("Summary.ErrorStep = %q, want pull-image", termErr.Summary.GetErrorStep())
	}
}

func TestWaitActiveNilOnPollIsSafe(t *testing.T) {
	f := &fakeSpawnClient{listQueue: [][]*cpv1.SpawnSummary{
		{{SpawnId: "sp-1", Status: cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE, Generation: 1}},
	}}
	if _, err := waitActive(context.Background(), f, "sp-1", nil); err != nil {
		t.Fatalf("waitActive with nil onPoll: %v", err)
	}
}

func TestListReturnsSpawns(t *testing.T) {
	summaries := []*cpv1.SpawnSummary{{SpawnId: "sp-1"}, {SpawnId: "sp-2"}}
	got, err := listSpawns(context.Background(), &fakeSpawnClient{listQueue: [][]*cpv1.SpawnSummary{summaries}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d spawns, want 2", len(got))
	}
}

func TestStatusFindsSpawn(t *testing.T) {
	summaries := []*cpv1.SpawnSummary{{SpawnId: "sp-1"}, {SpawnId: "sp-2"}}
	got, err := statusSpawn(context.Background(), &fakeSpawnClient{listQueue: [][]*cpv1.SpawnSummary{summaries}}, "sp-2")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.GetSpawnId() != "sp-2" {
		t.Fatalf("Status returned %+v, want sp-2", got)
	}
}

func TestStatusNotFound(t *testing.T) {
	_, err := statusSpawn(context.Background(), &fakeSpawnClient{listQueue: [][]*cpv1.SpawnSummary{{{SpawnId: "sp-1"}}}}, "sp-missing")
	if !errors.Is(err, ErrSpawnNotFound) {
		t.Fatalf("err = %v, want ErrSpawnNotFound", err)
	}
}

func TestSetModel(t *testing.T) {
	f := &fakeSpawnClient{setModelResp: &cpv1.SetSpawnModelResponse{Model: "anthropic/claude-3.5-sonnet", Applied: true}}
	model, applied, err := setModel(context.Background(), f, "sp-1", "anthropic/claude-3.5-sonnet")
	if err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if model != "anthropic/claude-3.5-sonnet" || !applied {
		t.Fatalf("SetModel = (%q, %v)", model, applied)
	}
	if f.gotSetModel == nil || f.gotSetModel.SpawnId != "sp-1" || f.gotSetModel.Model != "anthropic/claude-3.5-sonnet" {
		t.Fatalf("SetSpawnModel req = %+v", f.gotSetModel)
	}
}

func TestResumeSignsAndCallsResumeSpawn(t *testing.T) {
	f := &fakeSpawnClient{pendingReady: true, listQueue: [][]*cpv1.SpawnSummary{{}}}
	if err := resumeSpawn(context.Background(), f, "sp-1", nil); err != nil {
		t.Fatalf("resumeSpawn: %v", err)
	}
	if f.gotResume == nil || f.gotResume.SpawnId != "sp-1" {
		t.Fatalf("ResumeSpawn req = %+v", f.gotResume)
	}
}

func TestResumePropagatesRPCError(t *testing.T) {
	f := &fakeSpawnClient{pendingReady: true, resumeErr: fmt.Errorf("no capacity")}
	err := resumeSpawn(context.Background(), f, "sp-1", nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestResumePreflightFailureMakesNoCPCall(t *testing.T) {
	f := &fakeSpawnClient{}
	source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) { return NodeCredentials{}, errors.New("login required") })
	err := resumeSpawnAuthorized(context.Background(), f, source, TargetTrust{}, "sp-1", nil)
	if err == nil || f.gotResume != nil {
		t.Fatalf("error = %v, ResumeSpawn = %+v", err, f.gotResume)
	}
}

func TestCreatePreflightFailureMakesNoCPCall(t *testing.T) {
	f := &fakeSpawnClient{}
	source := nodeCredentialSourceFunc(func(context.Context) (NodeCredentials, error) { return NodeCredentials{}, errors.New("login required") })
	if _, _, err := createSpawnAuthorized(context.Background(), f, source, TargetTrust{}, &cpv1.CreateSpawnRequest{}); err == nil {
		t.Fatal("create accepted missing credentials")
	}
	if f.gotCreate != nil {
		t.Fatal("CreateSpawn called before credential preflight")
	}
}

func TestSuspendCallsSuspendSpawn(t *testing.T) {
	f := &fakeSpawnClient{}
	if err := suspendSpawn(context.Background(), f, "sp-1"); err != nil {
		t.Fatalf("suspendSpawn: %v", err)
	}
	if f.gotSuspend == nil || f.gotSuspend.SpawnId != "sp-1" {
		t.Fatalf("SuspendSpawn req = %+v", f.gotSuspend)
	}
}

func TestSuspendPropagatesRPCError(t *testing.T) {
	f := &fakeSpawnClient{suspendErr: fmt.Errorf("no capacity")}
	if err := suspendSpawn(context.Background(), f, "sp-1"); err == nil {
		t.Fatal("expected error")
	}
}

func TestDelete(t *testing.T) {
	f := &fakeSpawnClient{}
	if err := deleteSpawn(context.Background(), f, "sp-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if f.gotDelete == nil || f.gotDelete.SpawnId != "sp-1" {
		t.Fatalf("DeleteSpawn req = %+v", f.gotDelete)
	}
}

func TestStop(t *testing.T) {
	f := &fakeSpawnClient{}
	if err := stopSpawn(context.Background(), f, "sp-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if f.gotStop == nil || f.gotStop.SpawnId != "sp-1" {
		t.Fatalf("StopSpawn req = %+v", f.gotStop)
	}
}

// TestSessionStreamRoundTrips exercises the pipe/writer wiring in isolation from a real
// connect.BidiStreamForClient: a scripted recv func feeds one frame then blocks (simulated by
// returning an error after it), and send captures the outgoing frame.
func TestSessionStreamRoundTrips(t *testing.T) {
	recvFrames := []*cpv1.Frame{{Data: []byte("hello")}}
	recvIdx := 0
	recv := func() (*cpv1.Frame, error) {
		if recvIdx < len(recvFrames) {
			f := recvFrames[recvIdx]
			recvIdx++
			return f, nil
		}
		return nil, errors.New("eof")
	}
	var sent []*cpv1.Frame
	var mu sync.Mutex
	send := func(f *cpv1.Frame) error {
		mu.Lock()
		sent = append(sent, f)
		mu.Unlock()
		return nil
	}

	pr, pw := sessionStream(recv, send, "sp-1")

	buf := make([]byte, 5)
	n, err := pr.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("read %q, want hello", buf[:n])
	}

	if _, err := pw.Write([]byte("world")); err != nil {
		t.Fatalf("write: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || sent[0].SpawnId != "sp-1" || string(sent[0].Data) != "world" {
		t.Fatalf("sent = %+v, want one frame {sp-1, world}", sent)
	}
}
