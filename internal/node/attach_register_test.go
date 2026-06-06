package node

import "testing"

func TestRegisterMessageIncludesBinaries(t *testing.T) {
	cfg := Config{
		NodeID: "n1", MaxSpawns: 2, AgentImage: "ghcr.io/acme/goose:1",
		AgentBinaries: []string{"goose", "opencode"}, NodeClass: "byo", NodeOwner: "alice",
	}
	r := registerMessage(cfg, nil)
	if r.NodeId != "n1" || len(r.AgentImages) != 1 || r.AgentImages[0] != "ghcr.io/acme/goose:1" {
		t.Fatalf("register basics wrong: %+v", r)
	}
	if r.MaxSpawns != 2 || r.NodeClass != "byo" || r.NodeOwner != "alice" {
		t.Fatalf("register basics wrong: %+v", r)
	}
	if len(r.Binaries) != 2 || r.Binaries[0] != "goose" || r.Binaries[1] != "opencode" {
		t.Fatalf("binaries not threaded: %v", r.Binaries)
	}
}
