package opencode

import (
	"context"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	f := NewFake("/app")
	defer f.Close()
	if err := New(f.URL).Health(); err != nil {
		t.Fatalf("health: %v", err)
	}
}

func TestDiscoverOrCreateCreatesThenReuses(t *testing.T) {
	f := NewFake("/app")
	defer f.Close()
	c := New(f.URL)

	id1, err := c.DiscoverOrCreateSession("/app", "t")
	if err != nil || id1 == "" {
		t.Fatalf("create: id=%q err=%v", id1, err)
	}
	id2, err := c.DiscoverOrCreateSession("/app", "t")
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Fatalf("expected reuse of %q, got new %q", id1, id2)
	}
}

func TestDiscoverScopesByDirectory(t *testing.T) {
	f := NewFake("/other") // server roots sessions at /other
	defer f.Close()
	c := New(f.URL)
	// seed one session under /other
	if _, err := c.CreateSession("seed"); err != nil {
		t.Fatal(err)
	}
	// asking for /app must NOT reuse the /other session — it creates a new one.
	id, err := c.DiscoverOrCreateSession("/app", "t")
	if err != nil {
		t.Fatal(err)
	}
	// the fake roots all its sessions at /other, so the "created" one also has
	// dir /other; the point is discover did not early-return on the seed.
	ss, _ := c.ListSessions()
	if len(ss) != 2 {
		t.Fatalf("expected a new session to be created (2 total), got %d", len(ss))
	}
	_ = id
}

func TestEventsStreamsPromptSequence(t *testing.T) {
	f := NewFake("/app")
	defer f.Close()
	c := New(f.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got := make(chan RawEvent, 16)
	go func() { _ = c.Events(ctx, func(e RawEvent) { got <- e }) }()

	// wait for the subscriber to register, then trigger a prompt.
	time.Sleep(100 * time.Millisecond)
	if err := c.PromptAsync("ses_fake1", "hello", ""); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	sawDelta := false
	deadline := time.After(2 * time.Second)
	for !sawDelta {
		select {
		case e := <-got:
			if e.Type == "message.part.delta" {
				sawDelta = true
			}
		case <-deadline:
			t.Fatal("never received message.part.delta from prompt script")
		}
	}
}

func TestListCommands(t *testing.T) {
	f := NewFake("/app")
	defer f.Close()
	c := New(f.URL)

	// Empty by default -> an agent with no commands returns an empty list (graceful absence).
	cmds, err := c.ListCommands()
	if err != nil {
		t.Fatalf("list commands: %v", err)
	}
	if len(cmds) != 0 {
		t.Fatalf("expected no commands by default, got %+v", cmds)
	}

	f.SetCommands([]Command{
		{Name: "init", Description: "guided setup", Hints: []string{"$ARGUMENTS"}, Source: "command"},
		{Name: "review", Description: "review changes", Source: "command"},
	})
	cmds, err = c.ListCommands()
	if err != nil {
		t.Fatalf("list commands: %v", err)
	}
	if len(cmds) != 2 || cmds[0].Name != "init" || cmds[0].Description != "guided setup" ||
		len(cmds[0].Hints) != 1 || cmds[0].Hints[0] != "$ARGUMENTS" || cmds[1].Name != "review" {
		t.Fatalf("bad command list: %+v", cmds)
	}
}

func TestAbortRecorded(t *testing.T) {
	f := NewFake("/app")
	defer f.Close()
	if err := New(f.URL).Abort("ses_fake1"); err != nil {
		t.Fatal(err)
	}
	if len(f.Aborts()) != 1 || f.Aborts()[0] != "ses_fake1" {
		t.Fatalf("abort not recorded: %+v", f.Aborts())
	}
}
