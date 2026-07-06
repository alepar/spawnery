package main

import "testing"

func TestForkTargetMapping(t *testing.T) {
	if r, err := forkTarget("sp1", "", "", ""); err != nil || r.TargetNodeId != "" || r.TargetClass != "" {
		t.Fatalf("default target = %+v err=%v", r, err)
	}
	if r, err := forkTarget("sp1", "node-b", "", "Trial"); err != nil || r.TargetNodeId != "node-b" || r.Name != "Trial" {
		t.Fatalf("node target = %+v err=%v", r, err)
	}
	if r, err := forkTarget("sp1", "", "cloud", ""); err != nil || r.TargetClass != "cloud" || r.TargetNodeId != "" {
		t.Fatalf("class target = %+v err=%v", r, err)
	}
	if _, err := forkTarget("sp1", "node-b", "cloud", ""); err == nil {
		t.Fatal("node and class together must be rejected")
	}
}
