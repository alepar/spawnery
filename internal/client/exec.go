package client

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/execstream"
	"spawnery/internal/intent"
)

type execSessionStream interface {
	Send(*cpv1.Frame) error
	Receive() (*cpv1.Frame, error)
	CloseRequest() error
	CloseResponse() error
}

// Exec runs argv in a spawn through an authenticated, node-verified CP Session attachment.
func (c *Client) Exec(ctx context.Context, spawnID string, argv []string, stdout, stderr io.Writer) (int, error) {
	env, req, _, sessionID, err := buildExecOpenIntent(ctx, c.rpc, c.nodeCredentials, c.targetTrust, spawnID, argv)
	if err != nil {
		return 1, err
	}
	stream := c.rpc.Session(ctx)
	return runExecSession(ctx, stream, &cpv1.Frame{
		SpawnId: spawnID, SessionId: sessionID, ExecRequest: req, SessionAuth: env,
	}, stdout, stderr)
}

func buildExecOpenIntent(ctx context.Context, rpc sessionTargetClient, source NodeCredentialSource, trust TargetTrust, spawnID string, argv []string) (*authv1.AuthEnvelope, *authv1.ExecRequest, uint64, string, error) {
	if spawnID == "" || len(argv) == 0 || argv[0] == "" {
		return nil, nil, 0, "", errors.New("exec open requires spawn id and non-empty argv")
	}
	prepared, err := prepareNodeAuthorization(ctx, source, trust)
	if err != nil {
		return nil, nil, 0, "", err
	}
	response, err := rpc.GetSpawnNodeKey(ctx, connect.NewRequest(&cpv1.GetSpawnNodeKeyRequest{SpawnId: spawnID}))
	if err != nil {
		return nil, nil, 0, "", fmt.Errorf("exec open %s: GetSpawnNodeKey: %w", spawnID, err)
	}
	if _, err := verifyResolvedTarget(response.Msg.GetNodeCertChain(), response.Msg.GetTargetNodeId(), response.Msg.GetTargetNodeClass(), response.Msg.GetTargetNodeAccountId(), trust); err != nil {
		return nil, nil, 0, "", fmt.Errorf("exec open %s: verify target: %w", spawnID, err)
	}
	credentials, err := prepared.NodeCredentials(ctx)
	if err != nil {
		return nil, nil, 0, "", fmt.Errorf("exec open %s: node credentials: %w", spawnID, err)
	}
	if credentials.AccessToken == "" || credentials.Signer == nil {
		return nil, nil, 0, "", errors.New("exec open received incomplete node credentials")
	}
	req := &authv1.ExecRequest{Argv: append([]string(nil), argv...)}
	sessionID, err := randomExecID("exec-")
	if err != nil {
		return nil, nil, 0, "", fmt.Errorf("exec open %s: generate session id: %w", spawnID, err)
	}
	jti, err := randomExecID("")
	if err != nil {
		return nil, nil, 0, "", fmt.Errorf("exec open %s: generate jti: %w", spawnID, err)
	}
	body := &authv1.IntentBody{
		Jti: jti, IssuedAt: time.Now().Unix(), Op: string(intent.OpExecOpen), SpawnId: spawnID,
		Generation: response.Msg.GetGeneration(), TargetNodeId: response.Msg.GetTargetNodeId(),
		SessionId: sessionID, ExecRequest: &authv1.ExecRequest{Argv: append([]string(nil), req.GetArgv()...)},
	}
	signed, err := buildSignedIntent(intent.OpExecOpen, body, credentials.Signer)
	if err != nil {
		return nil, nil, 0, "", fmt.Errorf("exec open %s: sign: %w", spawnID, err)
	}
	return &authv1.AuthEnvelope{AccessToken: credentials.AccessToken, Intent: signed}, req, response.Msg.GetGeneration(), sessionID, nil
}

func randomExecID(prefix string) (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%x", prefix, id), nil
}

func runExecSession(_ context.Context, stream execSessionStream, bind *cpv1.Frame, stdout, stderr io.Writer) (int, error) {
	if stream == nil || bind == nil || bind.GetSpawnId() == "" || bind.GetSessionId() == "" || bind.GetExecRequest() == nil || bind.GetSessionAuth() == nil {
		return 1, errors.New("exec session requires complete bind frame")
	}
	if err := stream.Send(bind); err != nil {
		return 1, fmt.Errorf("exec session bind: %w", err)
	}
	reader, writer := io.Pipe()
	receiveDone := make(chan struct{})
	go func() {
		defer close(receiveDone)
		defer writer.Close()
		for {
			frame, err := stream.Receive()
			if err != nil {
				if errors.Is(err, io.EOF) {
					_ = writer.Close()
				} else {
					_ = writer.CloseWithError(err)
				}
				return
			}
			if _, err := writer.Write(frame.GetData()); err != nil {
				return
			}
		}
	}()
	code, err := execstream.Demux(reader, stdout, stderr)
	_ = reader.Close()
	_ = stream.CloseRequest()
	_ = stream.CloseResponse()
	<-receiveDone
	return code, err
}
