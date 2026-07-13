package client

import (
	"bytes"
	"context"
	"io"
	"testing"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/execstream"
	"spawnery/internal/intent"
	"spawnery/internal/pki"
)

func TestBuildExecOpenIntentUsesVerifiedTargetAndExactRequest(t *testing.T) {
	fx := issueProdNodeClass(t, "node-1", "alice", pki.ClassSelfHosted)
	source, trust := testSessionAuthorization(t, fx)
	rpc := &fakeSessionTargetClient{response: &cpv1.GetSpawnNodeKeyResponse{
		Generation: 7, TargetNodeId: "node-1", TargetNodeClass: pki.ClassSelfHosted,
		TargetNodeAccountId: "alice", NodeCertChain: fx.chainPEM,
	}}
	argv := []string{"sh", "-lc", "printf exact"}
	env, req, generation, sessionID, err := buildExecOpenIntent(context.Background(), rpc, source, trust, "sp-1", argv)
	if err != nil {
		t.Fatal(err)
	}
	if generation != 7 || sessionID == "" || !proto.Equal(req, &authv1.ExecRequest{Argv: argv}) {
		t.Fatalf("resolved exec = generation %d session %q request %+v", generation, sessionID, req)
	}
	if env.GetAccessToken() != "node-token" || env.GetIntent().GetDomain() != intent.DomainExecOpen {
		t.Fatalf("envelope = %+v", env)
	}
	var body authv1.IntentBody
	if err := proto.Unmarshal(env.GetIntent().GetBody(), &body); err != nil {
		t.Fatal(err)
	}
	if body.GetOp() != string(intent.OpExecOpen) || body.GetSpawnId() != "sp-1" || body.GetGeneration() != 7 ||
		body.GetTargetNodeId() != "node-1" || body.GetSessionId() != sessionID || !proto.Equal(body.GetExecRequest(), req) {
		t.Fatalf("exec intent body = %+v", &body)
	}
	argv[2] = "mutated"
	if req.GetArgv()[2] != "printf exact" {
		t.Fatalf("request retained caller argv alias: %+v", req)
	}
}

func TestBuildExecOpenIntentRejectsMissingCredentialsBeforeTargetLookup(t *testing.T) {
	rpc := &fakeSessionTargetClient{}
	if _, _, _, _, err := buildExecOpenIntent(context.Background(), rpc, nil, TargetTrust{}, "sp-1", []string{"true"}); err == nil {
		t.Fatal("missing credentials accepted")
	}
	if rpc.calls != 0 {
		t.Fatalf("GetSpawnNodeKey called %d times before custody preflight", rpc.calls)
	}
}

type fakeExecStream struct {
	sent   []*cpv1.Frame
	frames []*cpv1.Frame
	closed bool
}

func (s *fakeExecStream) Send(frame *cpv1.Frame) error {
	s.sent = append(s.sent, proto.Clone(frame).(*cpv1.Frame))
	return nil
}

func (s *fakeExecStream) Receive() (*cpv1.Frame, error) {
	if len(s.frames) == 0 {
		return nil, io.EOF
	}
	f := s.frames[0]
	s.frames = s.frames[1:]
	return f, nil
}

func (s *fakeExecStream) CloseRequest() error { s.closed = true; return nil }

func TestRunExecSessionStreamsAndReturnsExactExitCode(t *testing.T) {
	var wire bytes.Buffer
	_ = execstream.WriteFrame(&wire, execstream.Stdout, []byte("out\n"))
	_ = execstream.WriteFrame(&wire, execstream.Stderr, []byte("err\n"))
	_ = execstream.WriteExit(&wire, 37)
	b := wire.Bytes()
	stream := &fakeExecStream{frames: []*cpv1.Frame{{Data: bytes.Clone(b[:7])}, {Data: bytes.Clone(b[7:])}}}
	bind := &cpv1.Frame{
		SpawnId: "sp-1", SessionId: "exec-1", ExecRequest: &authv1.ExecRequest{Argv: []string{"exit", "37"}},
		SessionAuth: &authv1.AuthEnvelope{AccessToken: "node-token", Intent: &authv1.SignedIntent{}},
	}
	var stdout, stderr bytes.Buffer
	code, err := runExecSession(context.Background(), stream, bind, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 37 || stdout.String() != "out\n" || stderr.String() != "err\n" {
		t.Fatalf("exec = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	if len(stream.sent) != 1 || !proto.Equal(stream.sent[0], bind) || !stream.closed {
		t.Fatalf("stream bind/close = sent %+v closed %t", stream.sent, stream.closed)
	}
}
