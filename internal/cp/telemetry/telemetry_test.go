package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJSONLSinkWritesOneLinePerEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	s, err := NewJSONLSink(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := time.Unix(1700000000, 0).UTC()
	for _, k := range []string{"spawn_create", "session_start", "session_end"} {
		if err := s.Emit(Event{Kind: k, Owner: "alice", AppID: "secret-app", Tier: "reviewed", Storage: "managed", NodeID: "n1", SpawnID: "sp1", Timestamp: ts}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d: %q", len(lines), raw)
	}
	var ev Event
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != "spawn_create" || ev.Owner != "alice" || ev.AppID != "secret-app" {
		t.Fatalf("bad event: %+v", ev)
	}
}

func TestNopSinkIsNoOp(t *testing.T) {
	var s Sink = NopSink{}
	if err := s.Emit(Event{Kind: "spawn_create"}); err != nil {
		t.Fatal(err)
	}
}
