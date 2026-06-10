package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

func TestCreateSpawnAutogeneratesNameWithCollisionCounter(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sp1 := createActive(t, s, reg, &cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"})
	if sp1.Name != "secret-app" {
		t.Fatalf("first spawn name=%q want %q", sp1.Name, "secret-app")
	}
	sp2 := createActive(t, s, reg, &cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"})
	if sp2.Name != "secret-app 2" {
		t.Fatalf("second spawn name=%q want %q", sp2.Name, "secret-app 2")
	}
}

func TestCreateSpawnUsesExplicitName(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sp := createActive(t, s, reg, &cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m", Name: "  My Spawn  "})
	if sp.Name != "My Spawn" {
		t.Fatalf("explicit name=%q want %q (trimmed)", sp.Name, "My Spawn")
	}
}

func TestListSpawnsReturnsName(t *testing.T) {
	s, reg, _ := newTestServer(t)
	createActive(t, s, reg, &cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"})
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Spawns) != 1 || resp.Msg.Spawns[0].Name != "secret-app" {
		t.Fatalf("ListSpawns name not populated: %+v", resp.Msg.Spawns)
	}
}

func TestNextSpawnName(t *testing.T) {
	cases := []struct {
		base  string
		taken []string
		want  string
	}{
		{"Wiki", nil, "Wiki"},
		{"Wiki", []string{"Wiki"}, "Wiki 2"},
		{"Wiki", []string{"Wiki", "Wiki 2"}, "Wiki 3"},
		{"Wiki", []string{"Wiki", "Wiki 3"}, "Wiki 2"}, // fills the first gap
		{"", nil, "spawn"},
		{"", []string{"spawn"}, "spawn 2"},
	}
	for _, c := range cases {
		taken := map[string]bool{}
		for _, n := range c.taken {
			taken[n] = true
		}
		if got := nextSpawnName(c.base, taken); got != c.want {
			t.Errorf("nextSpawnName(%q, %v) = %q, want %q", c.base, c.taken, got, c.want)
		}
	}
}

func TestListSpawnsReturnsModelApplied(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sp := createActive(t, s, reg, &cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"})
	ctx := auth.WithOwner(context.Background(), "alice")

	// A fresh active spawn is applied from birth.
	resp, err := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Spawns) != 1 || !resp.Msg.Spawns[0].ModelApplied || resp.Msg.Spawns[0].Model != "m" {
		t.Fatalf("want model=m applied=true, got %+v", resp.Msg.Spawns)
	}

	// SetModel marks it unapplied; ListSpawns must surface that.
	if err := s.st.Spawns().SetModel(ctx, sp.ID, "m2"); err != nil {
		t.Fatal(err)
	}
	resp, err = s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Spawns[0].ModelApplied || resp.Msg.Spawns[0].Model != "m2" {
		t.Fatalf("want model=m2 applied=false, got %+v", resp.Msg.Spawns[0])
	}
}
