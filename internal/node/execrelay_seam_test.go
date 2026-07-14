package node

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/execstream"
	"spawnery/internal/intent"
	"spawnery/internal/runtime/fakepod"
)

type seamExecRunner struct {
	mu       sync.Mutex
	calls    int
	argv     []string
	wait     bool
	started  chan struct{}
	canceled chan struct{}
}

func (r *seamExecRunner) ExecStream(ctx context.Context, _ string, argv []string, stdout, stderr io.Writer) (int, error) {
	r.mu.Lock()
	r.calls++
	r.argv = append([]string(nil), argv...)
	r.mu.Unlock()
	if r.started != nil {
		close(r.started)
	}
	if r.wait {
		<-ctx.Done()
		close(r.canceled)
		return 1, ctx.Err()
	}
	_, _ = io.WriteString(stdout, "out\n")
	_, _ = io.WriteString(stderr, "err\n")
	return 17, nil
}

func (r *seamExecRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func execFramesFor(fs *fakeCPStream, sessionID string) []byte {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var wire []byte
	for _, msg := range fs.sent {
		if frame := msg.GetFrame(); frame != nil && frame.GetSessionId() == sessionID {
			wire = append(wire, frame.GetData()...)
		}
	}
	return wire
}

func startExecSeamSpawn(t *testing.T, runner execRunner) (*attacher, *fakeCPStream, testArtifactSigner, *token.Verifier, *ecdsa.PrivateKey, time.Time) {
	t.Helper()
	now := time.Unix(1_770_000_000, 0)
	asPriv, artifacts := genASKey(t)
	key := genECDSA(t)
	appDir := writeNodeApp(t)
	startBody := &authv1.IntentBody{
		Jti: "exec-start", IssuedAt: now.Unix(), SpawnId: "sp-exec-seam", Op: string(intent.OpCreateSpawn), AppRef: appDir, Model: "m",
	}
	fs := &fakeCPStream{}
	mgr := newGooseManager(t, fakeBackend(t, fakepod.WithAttachScript(scriptGoose)))
	a := newEnforcedAttacher(t, mgr, fs, artifacts, now)
	a.auths = newSessionAuthRegistryWithClock(func() time.Time { return now }, func(delay time.Duration, callback func()) sessionAuthTimer {
		return time.AfterFunc(delay, callback)
	})
	a.execRunner = runner
	a.startSpawn(context.Background(), &nodev1.StartSpawn{
		SpawnId: "sp-exec-seam", AppRef: appDir, Model: "m", AssertedOwner: "alice",
		Auth: buildIntentEnvelope(t, asPriv, artifacts, key, "alice", now, startBody, intent.OpCreateSpawn), IntentOp: string(intent.OpCreateSpawn),
	})
	t.Cleanup(func() { a.stopSpawn(context.Background(), "sp-exec-seam") })
	return a, fs, asPriv, artifacts, key, now
}

func execOpen(t *testing.T, asPriv testArtifactSigner, artifacts *token.Verifier, key *ecdsa.PrivateKey, now time.Time, sessionID, jti string, signedReq, relayedReq *authv1.ExecRequest) *nodev1.SessionOpen {
	t.Helper()
	body := &authv1.IntentBody{
		Jti: jti, IssuedAt: now.Unix(), SpawnId: "sp-exec-seam", SessionId: sessionID,
		Op: string(intent.OpExecOpen), ExecRequest: proto.Clone(signedReq).(*authv1.ExecRequest),
	}
	return &nodev1.SessionOpen{
		SpawnId: "sp-exec-seam", SessionId: sessionID, ClientId: "client-1", AssertedOwner: "alice",
		AttachmentId: "attachment-1", AttachmentSequence: 1, ExecRequest: relayedReq,
		Auth: buildIntentEnvelope(t, asPriv, artifacts, key, "alice", now, body, intent.OpExecOpen),
	}
}

func TestExecOpenRunsVerifiedRequestAndClosesAfterExit(t *testing.T) {
	runner := &seamExecRunner{}
	a, fs, asPriv, artifacts, key, now := startExecSeamSpawn(t, runner)
	req := &authv1.ExecRequest{Argv: []string{"sh", "-lc", "exit 17"}}
	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: execOpen(t, asPriv, artifacts, key, now, "exec-good", "exec-good", req, req)}})
	waitFor(t, "exec authorization close", func() bool {
		closed := fs.lastSessionAuthClosed()
		return closed != nil && closed.GetSessionId() == "exec-good"
	})
	if runner.callCount() != 1 || !strings.Contains(strings.Join(runner.argv, " "), "exit 17") {
		t.Fatalf("runner calls=%d argv=%v", runner.callCount(), runner.argv)
	}
	var stdout, stderr bytes.Buffer
	code, err := execstream.Demux(bytes.NewReader(execFramesFor(fs, "exec-good")), &stdout, &stderr)
	if err != nil || code != 17 || stdout.String() != "out\n" || stderr.String() != "err\n" {
		t.Fatalf("demux code=%d stdout=%q stderr=%q err=%v", code, stdout.String(), stderr.String(), err)
	}
}

func TestExecOpenRejectsSubstitutedRequestBeforeRunner(t *testing.T) {
	runner := &seamExecRunner{}
	a, fs, asPriv, artifacts, key, now := startExecSeamSpawn(t, runner)
	signed := &authv1.ExecRequest{Argv: []string{"echo", "safe"}}
	relayed := &authv1.ExecRequest{Argv: []string{"echo", "evil"}}
	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: execOpen(t, asPriv, artifacts, key, now, "exec-bad", "exec-bad", signed, relayed)}})
	closed := fs.lastSessionAuthClosed()
	if closed == nil || !strings.Contains(closed.GetReason(), string(NACKCorrespondence)) {
		t.Fatalf("SessionAuthClosed = %+v", closed)
	}
	if runner.callCount() != 0 {
		t.Fatalf("substituted exec reached runner %d times", runner.callCount())
	}
}

func TestExecSessionCloseCancelsAddressedRunner(t *testing.T) {
	runner := &seamExecRunner{wait: true, started: make(chan struct{}), canceled: make(chan struct{})}
	a, _, asPriv, artifacts, key, now := startExecSeamSpawn(t, runner)
	req := &authv1.ExecRequest{Argv: []string{"sleep", "forever"}}
	open := execOpen(t, asPriv, artifacts, key, now, "exec-cancel", "exec-cancel", req, req)
	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: open}})
	<-runner.started
	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_Close{Close: &nodev1.SessionClose{
		SpawnId: open.GetSpawnId(), SessionId: open.GetSessionId(), ClientId: open.GetClientId(), AttachmentId: open.GetAttachmentId(),
	}}})
	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("SessionClose did not cancel exec runner")
	}
}
