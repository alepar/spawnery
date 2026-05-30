//go:build e2e

package runtime

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"
)

// requireDocker fails the test loudly when Docker is unavailable. Under the e2e
// build tag we never skip: a broken Docker environment must surface as a test
// failure, not a silent pass.
func requireDocker(t *testing.T) {
	t.Helper()
	r, err := NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Ping(ctx); err != nil {
		t.Fatalf("docker not pingable: %v", err)
	}
}

func TestDockerRunAndAttachEcho(t *testing.T) {
	requireDocker(t)
	r, err := NewDocker()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := r.StartContainer(ctx, ContainerSpec{
		Image: "alpine:3", Cmd: []string{"sh", "-c", "read x; echo got:$x"},
		AttachStdio: true,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer r.StopContainer(ctx, id)

	att, err := r.Attach(ctx, id)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer att.Close()
	att.Stdin.Write([]byte("hello\n"))

	sc := bufio.NewScanner(att.Stdout)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "got:hello") {
			return
		}
	}
	t.Fatal("did not see echoed output")
}
