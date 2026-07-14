package node

import (
	"context"
	"io"
	"sync"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/execstream"
)

type execAttachment struct {
	attachmentID string
	cancel       context.CancelFunc
}

type execRunner interface {
	ExecStream(context.Context, string, []string, io.Writer, io.Writer) (int, error)
}

type execRunnerFunc func(context.Context, string, []string, io.Writer, io.Writer) (int, error)

func (f execRunnerFunc) ExecStream(ctx context.Context, spawnID string, argv []string, stdout, stderr io.Writer) (int, error) {
	return f(ctx, spawnID, argv, stdout, stderr)
}

type execRelay struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func newExecRelay(parent context.Context, runner execRunner, spawnID string, argv []string, send func([]byte) error, onClose func()) *execRelay {
	ctx, cancel := context.WithCancel(parent)
	r := &execRelay{cancel: cancel, done: make(chan struct{})}
	argv = append([]string(nil), argv...)
	go func() {
		defer onClose()
		defer close(r.done)
		defer cancel()
		mux := execstream.NewMuxer(relayWriter{send: send}, nil)
		code, err := runner.ExecStream(ctx, spawnID, argv, mux.Writer(execstream.Stdout), mux.Writer(execstream.Stderr))
		if err != nil {
			_ = mux.WriteError(err.Error())
			code = 1
		}
		_ = mux.WriteExit(code)
	}()
	return r
}

func (r *execRelay) Cancel() {
	if r == nil {
		return
	}
	r.once.Do(r.cancel)
}

func (r *execRelay) Done() <-chan struct{} {
	if r == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return r.done
}

type relayWriter struct{ send func([]byte) error }

func (w relayWriter) Write(p []byte) (int, error) {
	if err := w.send(append([]byte(nil), p...)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (a *attacher) attachExec(parent context.Context, open *nodev1.SessionOpen, key sessionAuthKey) bool {
	if a.execRunner == nil || open == nil || open.GetExecRequest() == nil || len(open.GetExecRequest().GetArgv()) == 0 {
		return false
	}
	ctx, cancel := context.WithCancel(parent)
	entry := execAttachment{attachmentID: open.GetAttachmentId(), cancel: cancel}
	a.mu.Lock()
	if a.execAttachments == nil {
		a.execAttachments = make(map[sessionAuthKey]execAttachment)
	}
	old := a.execAttachments[key]
	a.execAttachments[key] = entry
	a.mu.Unlock()
	if old.cancel != nil {
		old.cancel()
	}

	newExecRelay(ctx, a.execRunner, open.GetSpawnId(), open.GetExecRequest().GetArgv(),
		a.frameSenderFor(open.GetSpawnId(), key.sessionID, key.clientID),
		func() { a.finishExec(key, open.GetGeneration(), open.GetAttachmentId()) },
	)
	return true
}

func (a *attacher) finishExec(key sessionAuthKey, generation uint64, attachmentID string) {
	a.mu.Lock()
	current, ok := a.execAttachments[key]
	if ok && current.attachmentID == attachmentID {
		delete(a.execAttachments, key)
	} else {
		ok = false
	}
	a.mu.Unlock()
	if !ok {
		return
	}
	current.cancel()
	if a.auths != nil {
		a.auths.removeIfAttachment(key, attachmentID)
	}
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_SessionAuthClosed{SessionAuthClosed: &nodev1.SessionAuthClosed{
		SpawnId: key.spawnID, Generation: generation, SessionId: key.sessionID, ClientId: key.clientID,
		AttachmentId: attachmentID, Reason: "exec complete",
	}}})
}

func (a *attacher) cancelExec(key sessionAuthKey) {
	a.mu.Lock()
	entry := a.execAttachments[key]
	a.mu.Unlock()
	if entry.cancel != nil {
		entry.cancel()
	}
}
