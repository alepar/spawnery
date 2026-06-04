// Package runtime is the spawnlet's container-orchestration boundary.
package runtime

import (
	"context"
	"fmt"
	"io"
)

type Mount struct {
	HostPath, ContainerPath string
	ReadOnly                bool
}

type ContainerSpec struct {
	Image       string
	Cmd         []string
	Env         []string
	Mounts      []Mount
	NetnsOf     string // if set, join this container's network namespace
	AttachStdio bool   // attach stdin+stdout (for the agent)
	MemoryBytes int64  // 0 = unlimited
	NanoCPUs    int64  // 0 = unlimited; 1 CPU = 1_000_000_000
	PidsLimit   int64  // 0 = unlimited
	Runtime        string // "" = Docker default; e.g. "runsc"
	DropAllCaps    bool
	ReadonlyRootfs bool
	Labels         map[string]string // container labels (spawnery.managed/spawn-id/generation/node-id/role)
}

// ContainerSummary is the minimal view ListByLabel returns: the container id + its labels.
type ContainerSummary struct {
	ID     string
	Labels map[string]string
}

// AttachedStream is the agent's bidirectional stdio (demuxed stdout).
type AttachedStream struct {
	// Stdin: do not Close independently — closing it tears down the whole attach; use Close() for teardown.
	Stdin  io.WriteCloser
	Stdout io.Reader
	Close  func() error
}

type ContainerRuntime interface {
	Ping(ctx context.Context) error
	StartContainer(ctx context.Context, s ContainerSpec) (id string, err error)
	Attach(ctx context.Context, id string) (*AttachedStream, error)
	StopContainer(ctx context.Context, id string) error
	ContainerPID(ctx context.Context, id string) (int, error)
	ContainerIP(ctx context.Context, id string) (string, error)
	// ListByLabel returns all containers (any state) carrying label key=value, with their labels.
	ListByLabel(ctx context.Context, key, value string) ([]ContainerSummary, error)
}

// FakeRuntime records calls for unit tests.
type FakeRuntime struct {
	Started []ContainerSpec
	Stopped map[string]bool
	byID    map[string]ContainerSpec // id -> spec (for ListByLabel)
	n       int
}

func NewFake() *FakeRuntime { return &FakeRuntime{Stopped: map[string]bool{}, byID: map[string]ContainerSpec{}} }

func (f *FakeRuntime) Ping(_ context.Context) error { return nil }

func (f *FakeRuntime) StartContainer(_ context.Context, s ContainerSpec) (string, error) {
	f.n++
	id := fmt.Sprintf("fake-%d", f.n)
	f.Started = append(f.Started, s)
	f.byID[id] = s
	return id, nil
}
func (f *FakeRuntime) Attach(_ context.Context, id string) (*AttachedStream, error) {
	pr, pw := io.Pipe()
	return &AttachedStream{Stdin: pw, Stdout: pr, Close: func() error { return pw.Close() }}, nil
}
func (f *FakeRuntime) StopContainer(_ context.Context, id string) error {
	f.Stopped[id] = true
	return nil
}
func (f *FakeRuntime) ContainerPID(_ context.Context, id string) (int, error) { return 4242, nil }
func (f *FakeRuntime) ContainerIP(_ context.Context, id string) (string, error) {
	return "172.17.0.99", nil
}
func (f *FakeRuntime) ListByLabel(_ context.Context, key, value string) ([]ContainerSummary, error) {
	var out []ContainerSummary
	for id, s := range f.byID {
		if f.Stopped[id] {
			continue
		}
		if s.Labels[key] == value {
			out = append(out, ContainerSummary{ID: id, Labels: s.Labels})
		}
	}
	return out, nil
}
