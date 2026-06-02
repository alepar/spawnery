package cri

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// fakeCRI is an in-process CRI RuntimeService + ImageService for hermetic tests.
type fakeCRI struct {
	runtimeapi.UnimplementedRuntimeServiceServer
	runtimeapi.UnimplementedImageServiceServer

	mu sync.Mutex

	// readiness reported by Status.
	runtimeReady bool
	networkReady bool

	// canned StartPod responses.
	sandboxID string
	podIP     string
	infoPid   int // -> Info["info"] {"pid":...}

	// image presence: images already pulled (ImageStatus returns non-nil).
	present map[string]bool

	// recorded calls.
	createdNames  []string // container Metadata.Name in CreateContainer order
	created       []*runtimeapi.ContainerConfig
	createSandbox []string // PodSandboxId per CreateContainer
	started       []string // StartContainer ids
	stopped       []string // StopContainer ids
	pulled        []string // PullImage images
	stopSandbox   []string
	removeSandbox []string
	nextID        int
}

func (f *fakeCRI) Status(_ context.Context, _ *runtimeapi.StatusRequest) (*runtimeapi.StatusResponse, error) {
	f.mu.Lock()
	rr, nr := f.runtimeReady, f.networkReady
	f.mu.Unlock()
	return &runtimeapi.StatusResponse{Status: &runtimeapi.RuntimeStatus{Conditions: []*runtimeapi.RuntimeCondition{
		{Type: "RuntimeReady", Status: rr},
		{Type: "NetworkReady", Status: nr},
	}}}, nil
}

func (f *fakeCRI) setNetworkReady(v bool) { f.mu.Lock(); f.networkReady = v; f.mu.Unlock() }

// newFakeCRI starts the fake over bufconn and returns a connected *Client + the fake for assertions.
func newFakeCRI(t *testing.T) (*Client, *fakeCRI) {
	t.Helper()
	f := &fakeCRI{
		runtimeReady: true, networkReady: true,
		sandboxID: "sandbox-1", podIP: "10.244.0.7", infoPid: 4242,
		present: map[string]bool{},
	}
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	runtimeapi.RegisterRuntimeServiceServer(s, f)
	runtimeapi.RegisterImageServiceServer(s, f)
	go func() { _ = s.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(); s.Stop() })
	return NewClient(conn), f
}
