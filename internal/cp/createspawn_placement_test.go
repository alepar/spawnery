package cp

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
)

func TestCreateSpawnImageAwarePlacement(t *testing.T) {
	s, reg, _ := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")

	// Catalog (for validation) + a node that advertises the image (for placement).
	s.upsertAgentCatalog(context.Background(), []string{"img:1"}, []string{"goose"})
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1, Images: []string{"img:1"}})

	go func() {
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "secret-app", Model: "m", Image: "img:1", RunnableId: "goose-acp",
	}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	waitActive(t, s, resp.Msg.SpawnId)

	st := sender.firstStart()
	if st == nil {
		t.Fatal("no StartSpawn sent")
	}
	if st.GetImage() != "img:1" || st.GetRunnableId() != "goose-acp" || st.GetMode() != "acp" {
		t.Fatalf("StartSpawn missing selection: image=%q runnable=%q mode=%q",
			st.GetImage(), st.GetRunnableId(), st.GetMode())
	}
}
