package runtime

import (
	"context"
	"testing"
)

func TestFakeRecordsStartAndStop(t *testing.T) {
	f := NewFake()
	id, err := f.StartContainer(context.Background(), ContainerSpec{Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Started[0].Image; got != "img" {
		t.Fatalf("image not recorded: %q", got)
	}
	if err := f.StopContainer(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !f.Stopped[id] {
		t.Fatalf("stop not recorded for %s", id)
	}
}
