package client

// spawn.go: spawn lifecycle + query cores, extracted from cmd/spawnctl/{main,list,status,setmodel,
// resume}.go. Every method wraps an RPC and returns values/errors — no log.Fatalf, no fmt/stdout;
// CLI-side rendering (provisionFailure's "✗ failed at …", table printing, etc.) stays in spawnctl.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/internal/intent"
)

// spawnClient is the narrow RPC surface spawn.go's free functions need — narrowed to an interface
// so the orchestration is unit-testable with fakes. cpv1connect.SpawnServiceClient satisfies it.
type spawnClient interface {
	intentClient
	CreateSpawn(context.Context, *connect.Request[cpv1.CreateSpawnRequest]) (*connect.Response[cpv1.CreateSpawnResponse], error)
	ListSpawns(context.Context, *connect.Request[cpv1.ListSpawnsRequest]) (*connect.Response[cpv1.ListSpawnsResponse], error)
	ResumeSpawn(context.Context, *connect.Request[cpv1.ResumeSpawnRequest]) (*connect.Response[cpv1.ResumeSpawnResponse], error)
	SuspendSpawn(context.Context, *connect.Request[cpv1.SuspendSpawnRequest]) (*connect.Response[cpv1.SuspendSpawnResponse], error)
	SetSpawnModel(context.Context, *connect.Request[cpv1.SetSpawnModelRequest]) (*connect.Response[cpv1.SetSpawnModelResponse], error)
	DeleteSpawn(context.Context, *connect.Request[cpv1.DeleteSpawnRequest]) (*connect.Response[cpv1.DeleteSpawnResponse], error)
	StopSpawn(context.Context, *connect.Request[cpv1.StopSpawnRequest]) (*connect.Response[cpv1.StopSpawnResponse], error)
}

var _ spawnClient = (cpv1connect.SpawnServiceClient)(nil)

// TerminalError reports that WaitActive observed the spawn reach a terminal (non-ACTIVE, no
// further transition) status. Summary carries the raw SpawnSummary so a caller can render the
// failure however it likes (spawnctl's provisionFailure renders "✗ failed at <step>: <detail>").
type TerminalError struct{ Summary *cpv1.SpawnSummary }

func (e *TerminalError) Error() string {
	return fmt.Sprintf("spawn %s reached terminal status %s", e.Summary.GetSpawnId(), e.Summary.GetStatus())
}

// ErrSpawnNotFound is returned by Status when no spawn with the given id is in the caller's list.
var ErrSpawnNotFound = errors.New("client: spawn not found")

// CreateSpawn creates a spawn via the control plane and returns its id. CreateSpawn is async: the
// spawn returns in 'starting' and provisions on the node — call WaitActive to block until it is
// ready (and, if the CP requires it, run SignProvision concurrently to sign the create intent).
func (c *Client) CreateSpawn(ctx context.Context, req *cpv1.CreateSpawnRequest) (string, error) {
	return createSpawnAuthorized(ctx, c.rpc, c.nodeCredentials, c.targetTrust, req)
}

func createSpawnAuthorized(ctx context.Context, rpc spawnClient, credentials NodeCredentialSource, trust TargetTrust, req *cpv1.CreateSpawnRequest) (string, error) {
	if _, err := prepareNodeAuthorization(ctx, credentials, trust); err != nil {
		return "", err
	}
	return createSpawn(ctx, rpc, req)
}

func createSpawn(ctx context.Context, rpc spawnClient, req *cpv1.CreateSpawnRequest) (string, error) {
	resp, err := rpc.CreateSpawn(ctx, connect.NewRequest(req))
	if err != nil {
		return "", fmt.Errorf("createSpawn: %w", err)
	}
	return resp.Msg.SpawnId, nil
}

// WaitActive polls ListSpawns until the spawn reaches ACTIVE (router-bound), failing fast on a
// terminal status (*TerminalError). onPoll (nil-safe) is called with the spawn's current summary
// each time the poll observes it, so callers can render progress (e.g. spawnctl's
// nextProgressLine dedup). Returns the spawn's live episode generation, for use in
// BuildSessionOpenIntent.
func (c *Client) WaitActive(ctx context.Context, id string, onPoll func(*cpv1.SpawnSummary)) (uint64, error) {
	return waitActive(ctx, c.rpc, id, onPoll)
}

func waitActive(ctx context.Context, rpc spawnClient, id string, onPoll func(*cpv1.SpawnSummary)) (uint64, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		ls, lsErr := rpc.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
		if lsErr != nil {
			return 0, fmt.Errorf("listSpawns: %w", lsErr)
		}
		for _, sp := range ls.Msg.Spawns {
			if sp.SpawnId != id {
				continue
			}
			if onPoll != nil {
				onPoll(sp)
			}
			switch sp.Status {
			case cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE:
				return sp.Generation, nil
			case cpv1.SpawnStatus_SPAWN_STATUS_ERROR, cpv1.SpawnStatus_SPAWN_STATUS_DELETED,
				cpv1.SpawnStatus_SPAWN_STATUS_UNREACHABLE:
				return 0, &TerminalError{Summary: sp}
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("spawn %s did not reach ACTIVE within 60s", id)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Resume resumes a suspended spawn in place. ResumeSpawn blocks at the CP awaiting the client's
// signed intent (A4 two-phase sign-after-resolve [AC1][AM12]); Resume drives pollAndSign
// concurrently via provisionWithIntent and retries once on a retryable NACK.
func (c *Client) Resume(ctx context.Context, id string) error {
	return resumeSpawnAuthorized(ctx, c.rpc, c.nodeCredentials, c.targetTrust, id, c.warn)
}

func resumeSpawn(ctx context.Context, rpc spawnClient, id string, _ func(error)) error {
	_, err := rpc.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: id}))
	return err
}

func resumeSpawnAuthorized(ctx context.Context, rpc spawnClient, credentials NodeCredentialSource, trust TargetTrust, id string, warn func(error)) error {
	prepared, err := prepareNodeAuthorization(ctx, credentials, trust)
	if err != nil {
		return err
	}
	return provisionWithIntent(ctx, rpc, prepared, trust, id, IntentParams{Op: intent.OpResumeSpawn}, func(rpcCtx context.Context) error {
		_, err := rpc.ResumeSpawn(rpcCtx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: id}))
		return err
	}, warn)
}

// Suspend suspends a running spawn in place: the CP snapshots its mounts to the journal and tears
// down the pod. Unlike Resume, SuspendSpawn needs no signed intent — it is a plain RPC call.
func (c *Client) Suspend(ctx context.Context, id string) error {
	return suspendSpawn(ctx, c.rpc, id)
}

func suspendSpawn(ctx context.Context, rpc spawnClient, id string) error {
	if _, err := rpc.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: id})); err != nil {
		return fmt.Errorf("suspend: %w", err)
	}
	return nil
}

// SetModel sets the inference model of a running spawn: persists it on the CP and (best-effort)
// switches the live agent mid-session via the sidecar override. applied reports whether the
// change reached the live agent.
func (c *Client) SetModel(ctx context.Context, id, model string) (activeModel string, applied bool, err error) {
	return setModel(ctx, c.rpc, id, model)
}

func setModel(ctx context.Context, rpc spawnClient, id, model string) (activeModel string, applied bool, err error) {
	resp, err := rpc.SetSpawnModel(ctx, connect.NewRequest(&cpv1.SetSpawnModelRequest{SpawnId: id, Model: model}))
	if err != nil {
		return "", false, fmt.Errorf("setModel: %w", err)
	}
	return resp.Msg.GetModel(), resp.Msg.GetApplied(), nil
}

// List fetches the caller's spawns from the CP (the token source scopes them to the owner).
func (c *Client) List(ctx context.Context) ([]*cpv1.SpawnSummary, error) {
	return listSpawns(ctx, c.rpc)
}

func listSpawns(ctx context.Context, rpc spawnClient) ([]*cpv1.SpawnSummary, error) {
	resp, err := rpc.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	return resp.Msg.GetSpawns(), nil
}

// Status returns the summary for a single spawn, or ErrSpawnNotFound if it is not in the caller's
// list.
func (c *Client) Status(ctx context.Context, id string) (*cpv1.SpawnSummary, error) {
	return statusSpawn(ctx, c.rpc, id)
}

func statusSpawn(ctx context.Context, rpc spawnClient, id string) (*cpv1.SpawnSummary, error) {
	sums, err := listSpawns(ctx, rpc)
	if err != nil {
		return nil, err
	}
	for _, s := range sums {
		if s.GetSpawnId() == id {
			return s, nil
		}
	}
	return nil, ErrSpawnNotFound
}

// Delete permanently deletes a spawn.
func (c *Client) Delete(ctx context.Context, id string) error {
	return deleteSpawn(ctx, c.rpc, id)
}

func deleteSpawn(ctx context.Context, rpc spawnClient, id string) error {
	if _, err := rpc.DeleteSpawn(ctx, connect.NewRequest(&cpv1.DeleteSpawnRequest{SpawnId: id})); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

// Stop stops a running spawn.
func (c *Client) Stop(ctx context.Context, id string) error {
	return stopSpawn(ctx, c.rpc, id)
}

func stopSpawn(ctx context.Context, rpc spawnClient, id string) error {
	if _, err := rpc.StopSpawn(ctx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: id})); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	return nil
}

// Session opens the frame-protocol bidi stream to the control plane. Pair with SessionStream to
// adapt it to io.Reader/io.Writer, and send a bind frame (spawn id + BuildSessionOpenIntent) first.
func (c *Client) Session(ctx context.Context) *connect.BidiStreamForClient[cpv1.Frame, cpv1.Frame] {
	return c.rpc.Session(ctx)
}

// SessionStream adapts an open Session stream to io.Reader/io.Writer: a goroutine receives frames
// and writes their Data into the returned reader; writes to the returned writer are sent as
// {SpawnId: spawnID, Data: b} frames.
func (c *Client) SessionStream(stream *connect.BidiStreamForClient[cpv1.Frame, cpv1.Frame], spawnID string) (io.Reader, io.Writer) {
	return sessionStream(stream.Receive, stream.Send, spawnID)
}

// sessionStream is the pipe/writer wiring underneath SessionStream, parameterized on plain
// recv/send funcs so it is unit-testable without a real connect.BidiStreamForClient.
func sessionStream(recv func() (*cpv1.Frame, error), send func(*cpv1.Frame) error, spawnID string) (io.Reader, io.Writer) {
	pr, pw := io.Pipe()
	go func() {
		for {
			f, err := recv()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, werr := pw.Write(f.Data); werr != nil {
				return
			}
		}
	}()
	sendW := writerFunc(func(b []byte) (int, error) {
		if err := send(&cpv1.Frame{SpawnId: spawnID, Data: b}); err != nil {
			return 0, err
		}
		return len(b), nil
	})
	return pr, sendW
}
