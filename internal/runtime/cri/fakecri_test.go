package cri

import (
	"context"
	"encoding/json"
	"fmt"
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
	sandboxID     string
	podIP         string
	infoPid       int               // -> Info["info"] {"pid":...}
	failCreate    bool              // inject a CreateContainer failure (exercises the cleanup path)
	sandboxLabels map[string]string // labels from the last RunPodSandbox (for ListPodSandbox)
	removed       bool              // sandbox removed (ListPodSandbox returns empty)

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

func (f *fakeCRI) nextContainerID() string {
	f.nextID++
	return fmt.Sprintf("ctr-%d", f.nextID)
}

func (f *fakeCRI) RunPodSandbox(_ context.Context, req *runtimeapi.RunPodSandboxRequest) (*runtimeapi.RunPodSandboxResponse, error) {
	f.mu.Lock()
	f.sandboxLabels = req.GetConfig().GetLabels()
	f.removed = false
	f.mu.Unlock()
	return &runtimeapi.RunPodSandboxResponse{PodSandboxId: f.sandboxID}, nil
}

// ListPodSandbox returns the (single) sandbox if present and matching the label selector.
func (f *fakeCRI) ListPodSandbox(_ context.Context, req *runtimeapi.ListPodSandboxRequest) (*runtimeapi.ListPodSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removed || f.sandboxLabels == nil {
		return &runtimeapi.ListPodSandboxResponse{}, nil
	}
	for k, v := range req.GetFilter().GetLabelSelector() {
		if f.sandboxLabels[k] != v {
			return &runtimeapi.ListPodSandboxResponse{}, nil
		}
	}
	return &runtimeapi.ListPodSandboxResponse{Items: []*runtimeapi.PodSandbox{
		{Id: f.sandboxID, Labels: f.sandboxLabels},
	}}, nil
}

func (f *fakeCRI) StopPodSandbox(_ context.Context, req *runtimeapi.StopPodSandboxRequest) (*runtimeapi.StopPodSandboxResponse, error) {
	f.mu.Lock()
	f.stopSandbox = append(f.stopSandbox, req.PodSandboxId)
	f.mu.Unlock()
	return &runtimeapi.StopPodSandboxResponse{}, nil
}

func (f *fakeCRI) RemovePodSandbox(_ context.Context, req *runtimeapi.RemovePodSandboxRequest) (*runtimeapi.RemovePodSandboxResponse, error) {
	f.mu.Lock()
	f.removeSandbox = append(f.removeSandbox, req.PodSandboxId)
	f.removed = true
	f.mu.Unlock()
	return &runtimeapi.RemovePodSandboxResponse{}, nil
}

func (f *fakeCRI) PodSandboxStatus(_ context.Context, req *runtimeapi.PodSandboxStatusRequest) (*runtimeapi.PodSandboxStatusResponse, error) {
	info, _ := json.Marshal(struct {
		Pid int `json:"pid"`
	}{Pid: f.infoPid})
	return &runtimeapi.PodSandboxStatusResponse{
		Status: &runtimeapi.PodSandboxStatus{Id: req.PodSandboxId, Network: &runtimeapi.PodSandboxNetworkStatus{Ip: f.podIP}},
		Info:   map[string]string{"info": string(info)},
	}, nil
}

func (f *fakeCRI) CreateContainer(_ context.Context, req *runtimeapi.CreateContainerRequest) (*runtimeapi.CreateContainerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate {
		return nil, fmt.Errorf("injected create failure")
	}
	id := f.nextContainerID()
	f.created = append(f.created, req.Config)
	f.createdNames = append(f.createdNames, req.Config.GetMetadata().GetName())
	f.createSandbox = append(f.createSandbox, req.PodSandboxId)
	return &runtimeapi.CreateContainerResponse{ContainerId: id}, nil
}

func (f *fakeCRI) StartContainer(_ context.Context, req *runtimeapi.StartContainerRequest) (*runtimeapi.StartContainerResponse, error) {
	f.mu.Lock()
	f.started = append(f.started, req.ContainerId)
	f.mu.Unlock()
	return &runtimeapi.StartContainerResponse{}, nil
}

func (f *fakeCRI) StopContainer(_ context.Context, req *runtimeapi.StopContainerRequest) (*runtimeapi.StopContainerResponse, error) {
	f.mu.Lock()
	f.stopped = append(f.stopped, req.ContainerId)
	f.mu.Unlock()
	return &runtimeapi.StopContainerResponse{}, nil
}

func (f *fakeCRI) ImageStatus(_ context.Context, req *runtimeapi.ImageStatusRequest) (*runtimeapi.ImageStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.present[req.Image.GetImage()] {
		return &runtimeapi.ImageStatusResponse{Image: &runtimeapi.Image{Id: req.Image.GetImage()}}, nil
	}
	return &runtimeapi.ImageStatusResponse{}, nil // not present
}

func (f *fakeCRI) PullImage(_ context.Context, req *runtimeapi.PullImageRequest) (*runtimeapi.PullImageResponse, error) {
	f.mu.Lock()
	f.pulled = append(f.pulled, req.Image.GetImage())
	f.present[req.Image.GetImage()] = true
	f.mu.Unlock()
	return &runtimeapi.PullImageResponse{ImageRef: req.Image.GetImage()}, nil
}

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
