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
}

// FakeRuntime records calls for unit tests.
type FakeRuntime struct {
	Started []ContainerSpec
	Stopped map[string]bool
	n       int
}

func NewFake() *FakeRuntime { return &FakeRuntime{Stopped: map[string]bool{}} }

func (f *FakeRuntime) Ping(_ context.Context) error { return nil }

func (f *FakeRuntime) StartContainer(_ context.Context, s ContainerSpec) (string, error) {
	f.n++
	id := fmt.Sprintf("fake-%d", f.n)
	f.Started = append(f.Started, s)
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
