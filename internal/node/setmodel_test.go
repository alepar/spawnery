package node

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/spawnlet"
)

// stubDoer is an injectable httpDoer that records the request it received and returns a canned response.
type stubDoer struct {
	mu      sync.Mutex
	calls   int
	gotReq  *http.Request
	gotBody string
	status  int   // status code to return
	err     error // if set, Do returns this error (network failure)
}

func (d *stubDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.gotReq = req
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		d.gotBody = string(b)
	}
	if d.err != nil {
		return nil, d.err
	}
	code := d.status
	if code == 0 {
		code = 200
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
}

func stubDoerOK() *stubDoer { return &stubDoer{status: 200} }

// lastSetModelResult returns the most recent SetModelResult the attacher sent (nil if none).
func lastSetModelResult(f *fakeCPStream) *nodev1.SetModelResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if r := f.sent[i].GetSetModelResult(); r != nil {
			return r
		}
	}
	return nil
}

func putSpawn(mgr *spawnlet.Manager, id string, gen uint64, token, url string) {
	mgr.Store().Put(&spawnlet.Spawn{ID: id, Generation: gen, ControlToken: token, ControlURL: url})
}

// Happy path: SetModel for the current generation POSTs to the spawn's ControlURL with the right bearer
// token and JSON body, and replies ok=true echoing request_id.
func TestSetModelPostsAndAcks(t *testing.T) {
	mgr := newGooseManager(t, &scriptedPodBackend{})
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	doer := stubDoerOK()
	a.ctrlHTTP = doer
	putSpawn(mgr, "sp1", 5, "tok-abc", "http://10.0.0.5:8081/control/model")

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_SetModel{SetModel: &nodev1.SetModel{
		SpawnId: "sp1", Generation: 5, Model: "anthropic/claude-x", RequestId: "req-1",
	}}})

	// handle dispatches setModel on its own goroutine (avoids head-of-line blocking the Receive loop),
	// so wait for the reply to land before asserting. The reply is sent only after the POST completes,
	// so observing it means doer.gotReq/gotBody are fully populated (synchronized via the fake stream's mu).
	waitFor(t, "SetModelResult reply", func() bool { return lastSetModelResult(fs) != nil })

	if doer.calls != 1 {
		t.Fatalf("POST calls = %d, want 1", doer.calls)
	}
	if got := doer.gotReq.Method; got != http.MethodPost {
		t.Fatalf("method = %s, want POST", got)
	}
	if got := doer.gotReq.URL.String(); got != "http://10.0.0.5:8081/control/model" {
		t.Fatalf("url = %s", got)
	}
	if got := doer.gotReq.Header.Get("Authorization"); got != "Bearer tok-abc" {
		t.Fatalf("authorization = %q, want %q", got, "Bearer tok-abc")
	}
	if got := doer.gotReq.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want %q", got, "application/json")
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(doer.gotBody), &body); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, doer.gotBody)
	}
	if body.Model != "anthropic/claude-x" {
		t.Fatalf("body model = %q, want %q", body.Model, "anthropic/claude-x")
	}
	res := lastSetModelResult(fs)
	if res == nil || !res.Ok {
		t.Fatalf("result = %+v, want ok=true", res)
	}
	if res.SpawnId != "sp1" || res.RequestId != "req-1" {
		t.Fatalf("result spawn/req = %q/%q, want sp1/req-1", res.SpawnId, res.RequestId)
	}
}

// Non-2xx from the sidecar -> ok=false with detail, request_id still echoed.
func TestSetModelNon200Fails(t *testing.T) {
	mgr := newGooseManager(t, &scriptedPodBackend{})
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	a.ctrlHTTP = &stubDoer{status: 503}
	putSpawn(mgr, "sp1", 1, "tok", "http://10.0.0.5:8081/control/model")

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_SetModel{SetModel: &nodev1.SetModel{
		SpawnId: "sp1", Generation: 1, Model: "m", RequestId: "req-2",
	}}})

	waitFor(t, "SetModelResult reply", func() bool { return lastSetModelResult(fs) != nil })
	res := lastSetModelResult(fs)
	if res == nil || res.Ok {
		t.Fatalf("result = %+v, want ok=false", res)
	}
	if res.RequestId != "req-2" {
		t.Fatalf("request_id = %q, want req-2", res.RequestId)
	}
	if res.Detail == "" {
		t.Fatal("ok=false must carry a detail")
	}
}

// Stale generation (gen < live) -> dropped: no POST, no reply (matches StopSpawn fence convention).
func TestSetModelStaleGenerationDropped(t *testing.T) {
	mgr := newGooseManager(t, &scriptedPodBackend{})
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	doer := stubDoerOK()
	a.ctrlHTTP = doer
	putSpawn(mgr, "sp1", 5, "tok", "http://10.0.0.5:8081/control/model")

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_SetModel{SetModel: &nodev1.SetModel{
		SpawnId: "sp1", Generation: 4, Model: "m", RequestId: "req-3",
	}}})

	if doer.calls != 0 {
		t.Fatalf("stale SetModel must not POST, got %d calls", doer.calls)
	}
	if res := lastSetModelResult(fs); res != nil {
		t.Fatalf("stale SetModel must not reply, got %+v", res)
	}
}

// Unknown spawn -> ok=false with detail, no POST, request_id echoed.
func TestSetModelUnknownSpawnFails(t *testing.T) {
	mgr := newGooseManager(t, &scriptedPodBackend{})
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	doer := stubDoerOK()
	a.ctrlHTTP = doer

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_SetModel{SetModel: &nodev1.SetModel{
		SpawnId: "ghost", Generation: 0, Model: "m", RequestId: "req-4",
	}}})

	waitFor(t, "SetModelResult reply", func() bool { return lastSetModelResult(fs) != nil })
	if doer.calls != 0 {
		t.Fatalf("unknown spawn must not POST, got %d calls", doer.calls)
	}
	res := lastSetModelResult(fs)
	if res == nil || res.Ok {
		t.Fatalf("result = %+v, want ok=false", res)
	}
	if res.RequestId != "req-4" || res.SpawnId != "ghost" {
		t.Fatalf("result req/spawn = %q/%q, want req-4/ghost", res.RequestId, res.SpawnId)
	}
}

// Empty ControlURL (pod has no IP) -> ok=false with detail, no POST, request_id echoed.
func TestSetModelEmptyControlURLFails(t *testing.T) {
	mgr := newGooseManager(t, &scriptedPodBackend{})
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	doer := stubDoerOK()
	a.ctrlHTTP = doer
	putSpawn(mgr, "sp1", 1, "tok", "") // no control URL

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_SetModel{SetModel: &nodev1.SetModel{
		SpawnId: "sp1", Generation: 1, Model: "m", RequestId: "req-5",
	}}})

	waitFor(t, "SetModelResult reply", func() bool { return lastSetModelResult(fs) != nil })
	if doer.calls != 0 {
		t.Fatalf("empty ControlURL must not POST, got %d calls", doer.calls)
	}
	res := lastSetModelResult(fs)
	if res == nil || res.Ok {
		t.Fatalf("result = %+v, want ok=false", res)
	}
	if res.RequestId != "req-5" {
		t.Fatalf("request_id = %q, want req-5", res.RequestId)
	}
}

// Transport error from the sidecar -> ok=false with detail, request_id echoed.
func TestSetModelTransportErrorFails(t *testing.T) {
	mgr := newGooseManager(t, &scriptedPodBackend{})
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	a.ctrlHTTP = &stubDoer{err: io.ErrUnexpectedEOF}
	putSpawn(mgr, "sp1", 1, "tok", "http://10.0.0.5:8081/control/model")

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_SetModel{SetModel: &nodev1.SetModel{
		SpawnId: "sp1", Generation: 1, Model: "m", RequestId: "req-6",
	}}})

	waitFor(t, "SetModelResult reply", func() bool { return lastSetModelResult(fs) != nil })
	res := lastSetModelResult(fs)
	if res == nil || res.Ok {
		t.Fatalf("result = %+v, want ok=false", res)
	}
	if res.RequestId != "req-6" || res.Detail == "" {
		t.Fatalf("result = %+v, want req-6 + non-empty detail", res)
	}
}
