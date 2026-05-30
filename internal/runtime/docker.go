package runtime

import (
	"context"
	"io"
	"log"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type Docker struct{ cli *client.Client }

func NewDocker() (*Docker, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Docker{cli: cli}, nil
}

func (d *Docker) Ping(ctx context.Context) error {
	_, err := d.cli.Ping(ctx)
	return err
}

func (d *Docker) StartContainer(ctx context.Context, s ContainerSpec) (string, error) {
	cfg := &container.Config{
		Image:       s.Image,
		Cmd:         s.Cmd,
		Env:         s.Env,
		OpenStdin:   s.AttachStdio,
		StdinOnce:   false,
		AttachStdin: s.AttachStdio,
		Tty:         false,
	}
	host := &container.HostConfig{}
	if s.NetnsOf != "" {
		host.NetworkMode = container.NetworkMode("container:" + s.NetnsOf)
	}
	for _, m := range s.Mounts {
		host.Binds = append(host.Binds, bind(m))
	}
	created, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, "")
	if err != nil {
		return "", err
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = d.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
		return "", err
	}
	return created.ID, nil
}

type logWriter struct{ prefix string }

func (l logWriter) Write(p []byte) (int, error) { log.Printf("%s%s", l.prefix, p); return len(p), nil }

func bind(m Mount) string {
	b := m.HostPath + ":" + m.ContainerPath
	if m.ReadOnly {
		b += ":ro"
	}
	return b
}

func (d *Docker) Attach(ctx context.Context, id string) (*AttachedStream, error) {
	hijack, err := d.cli.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true, Stdin: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return nil, err
	}
	// Demux multiplexed stdout into a pipe (non-TTY attach is framed).
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, logWriter{prefix: "[agent-stderr] "}, hijack.Reader)
		pw.CloseWithError(err)
	}()
	return &AttachedStream{
		Stdin:  hijack.Conn,
		Stdout: pr,
		Close:  func() error { hijack.Close(); pr.CloseWithError(io.ErrClosedPipe); return nil },
	}, nil
}

func (d *Docker) StopContainer(ctx context.Context, id string) error {
	ctx = context.WithoutCancel(ctx)
	to := 0
	_ = d.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &to})
	err := d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
	if err != nil && client.IsErrNotFound(err) {
		return nil
	}
	return err
}
