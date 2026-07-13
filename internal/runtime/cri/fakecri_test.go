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

// fakeContainer is one container in the fake's sandbox, with the state ListContainers reports.
type fakeContainer struct {
	id        string
	sandboxID string
	name      string // Metadata.Name ("sidecar"/"agent")
	labels    map[string]string
	state     runtimeapi.ContainerState
	createdAt int64
	envs      []*runtimeapi.KeyValue // -> ContainerStatus(Verbose) Info["info"].config.envs
}

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
	failStop      bool              // inject a StopContainer failure
	sandboxLabels map[string]string // labels from the last RunPodSandbox (for ListPodSandbox)
	removed       bool              // sandbox removed (ListPodSandbox returns empty)

	// image presence: images already pulled (ImageStatus returns non-nil).
	present map[string]bool
	// digests maps image name -> RepoDigests (for ResolveImageDigest tests).
	digests map[string][]string

	// recorded calls.
	createdNames      []string // container Metadata.Name in CreateContainer order
	created           []*runtimeapi.ContainerConfig
	createSandbox     []string // PodSandboxId per CreateContainer
	started           []string // StartContainer ids
	stopped           []string // StopContainer ids
	removedContainers []string // RemoveContainer ids
	pulled            []string // PullImage images
	stopSandbox       []string
	removeSandbox     []string
	nextID            int

	containers          []*fakeContainer
	failSandboxStatus   bool // inject a PodSandboxStatus failure (a transient CRI blip)
	failListCtrs        bool // inject a ListContainers failure
	failContainerStatus bool // inject a ContainerStatus failure
	clock               int64
}

// tick returns an incrementing logical clock value, used to give fakeContainers a distinct CreatedAt.
// Caller holds f.mu.
func (f *fakeCRI) tick() int64 {
	f.clock++
	return f.clock
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
	var kept []*fakeContainer
	for _, c := range f.containers {
		if c.sandboxID != req.PodSandboxId {
			kept = append(kept, c)
		}
	}
	f.containers = kept
	f.mu.Unlock()
	return &runtimeapi.RemovePodSandboxResponse{}, nil
}

func (f *fakeCRI) PodSandboxStatus(_ context.Context, req *runtimeapi.PodSandboxStatusRequest) (*runtimeapi.PodSandboxStatusResponse, error) {
	f.mu.Lock()
	fail := f.failSandboxStatus
	f.mu.Unlock()
	if fail {
		return nil, fmt.Errorf("injected sandbox status failure")
	}
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
	f.containers = append(f.containers, &fakeContainer{
		id:        id,
		sandboxID: req.PodSandboxId,
		name:      req.Config.GetMetadata().GetName(),
		labels:    req.Config.GetLabels(),
		state:     runtimeapi.ContainerState_CONTAINER_CREATED,
		createdAt: f.tick(),
		envs:      req.GetConfig().GetEnvs(),
	})
	return &runtimeapi.CreateContainerResponse{ContainerId: id}, nil
}

// ContainerStatus reports the container's state and, when Verbose is requested, an Info["info"] blob
// shaped like containerd's verbose ContainerStatus: {"config":{"envs":[{"key":..,"value":..}, ...]}}.
// envFromContainerInfo (backend.go) is what parses this shape back out.
func (f *fakeCRI) ContainerStatus(_ context.Context, req *runtimeapi.ContainerStatusRequest) (*runtimeapi.ContainerStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failContainerStatus {
		return nil, fmt.Errorf("injected container status failure")
	}
	var c *fakeContainer
	for _, cc := range f.containers {
		if cc.id == req.GetContainerId() {
			c = cc
			break
		}
	}
	if c == nil {
		return nil, fmt.Errorf("container %q not found", req.GetContainerId())
	}
	resp := &runtimeapi.ContainerStatusResponse{
		Status: &runtimeapi.ContainerStatus{Id: c.id, State: c.state},
	}
	if req.GetVerbose() {
		type kv struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		envs := make([]kv, 0, len(c.envs))
		for _, e := range c.envs {
			envs = append(envs, kv{Key: e.GetKey(), Value: e.GetValue()})
		}
		info, _ := json.Marshal(struct {
			Config struct {
				Envs []kv `json:"envs"`
			} `json:"config"`
		}{Config: struct {
			Envs []kv `json:"envs"`
		}{Envs: envs}})
		resp.Info = map[string]string{"info": string(info)}
	}
	return resp, nil
}

func (f *fakeCRI) StartContainer(_ context.Context, req *runtimeapi.StartContainerRequest) (*runtimeapi.StartContainerResponse, error) {
	f.mu.Lock()
	f.started = append(f.started, req.ContainerId)
	for _, c := range f.containers {
		if c.id == req.ContainerId {
			c.state = runtimeapi.ContainerState_CONTAINER_RUNNING
			break
		}
	}
	f.mu.Unlock()
	return &runtimeapi.StartContainerResponse{}, nil
}

func (f *fakeCRI) StopContainer(_ context.Context, req *runtimeapi.StopContainerRequest) (*runtimeapi.StopContainerResponse, error) {
	f.mu.Lock()
	fail := f.failStop
	f.stopped = append(f.stopped, req.ContainerId)
	if !fail {
		for _, c := range f.containers {
			if c.id == req.ContainerId {
				c.state = runtimeapi.ContainerState_CONTAINER_EXITED
				break
			}
		}
	}
	f.mu.Unlock()
	if fail {
		return nil, fmt.Errorf("injected stop failure")
	}
	return &runtimeapi.StopContainerResponse{}, nil
}

func (f *fakeCRI) RemoveContainer(_ context.Context, req *runtimeapi.RemoveContainerRequest) (*runtimeapi.RemoveContainerResponse, error) {
	f.mu.Lock()
	f.removedContainers = append(f.removedContainers, req.ContainerId)
	var kept []*fakeContainer
	for _, c := range f.containers {
		if c.id != req.ContainerId {
			kept = append(kept, c)
		}
	}
	f.containers = kept
	f.mu.Unlock()
	return &runtimeapi.RemoveContainerResponse{}, nil
}

// ListContainers honours the PodSandboxId and LabelSelector filter fields the backend uses.
func (f *fakeCRI) ListContainers(_ context.Context, req *runtimeapi.ListContainersRequest) (*runtimeapi.ListContainersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failListCtrs {
		return nil, fmt.Errorf("injected list containers failure")
	}
	var out []*runtimeapi.Container
	for _, c := range f.containers {
		if sb := req.GetFilter().GetPodSandboxId(); sb != "" && c.sandboxID != sb {
			continue
		}
		match := true
		for k, v := range req.GetFilter().GetLabelSelector() {
			if c.labels[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		out = append(out, &runtimeapi.Container{
			Id:           c.id,
			PodSandboxId: c.sandboxID,
			Metadata:     &runtimeapi.ContainerMetadata{Name: c.name},
			Labels:       c.labels,
			State:        c.state,
			CreatedAt:    c.createdAt,
		})
	}
	return &runtimeapi.ListContainersResponse{Containers: out}, nil
}

// addContainer injects a container that the backend did not create — used to model a pod created by an
// OLDER node binary (no spawnery.role label) and a crashed predecessor agent that containerd kept.
func (f *fakeCRI) addContainer(sandboxID, name string, labels map[string]string, state runtimeapi.ContainerState) string {
	return f.addContainerWithEnv(sandboxID, name, labels, state, nil)
}

// addContainerWithEnv is addContainer plus the container's recorded env, for ContainerStatus(Verbose)
// round-trip tests.
func (f *fakeCRI) addContainerWithEnv(sandboxID, name string, labels map[string]string, state runtimeapi.ContainerState, envs []*runtimeapi.KeyValue) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextContainerID()
	f.containers = append(f.containers, &fakeContainer{
		id: id, sandboxID: sandboxID, name: name, labels: labels, state: state, createdAt: f.tick(), envs: envs,
	})
	return id
}

func (f *fakeCRI) ImageStatus(_ context.Context, req *runtimeapi.ImageStatusRequest) (*runtimeapi.ImageStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := req.Image.GetImage()
	if f.present[name] {
		img := &runtimeapi.Image{Id: name}
		if ds := f.digests[name]; len(ds) > 0 {
			img.RepoDigests = ds
		}
		return &runtimeapi.ImageStatusResponse{Image: img}, nil
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
		digests: map[string][]string{},
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
