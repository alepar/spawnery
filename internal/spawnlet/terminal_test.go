package spawnlet

import (
	"reflect"
	"testing"
)

func TestAttachCommandWithSession(t *testing.T) {
	got := attachCommand("", "", "ses_abc")
	want := []string{"tmux", "new-session", "-A", "-s", "opencode", "opencode", "attach", "http://127.0.0.1:4096", "-s", "ses_abc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attachCommand:\n got %v\n want %v", got, want)
	}
	// tmux -A is what makes a second attach reattach instead of duplicating.
	if got[2] != "-A" {
		t.Fatalf("expected tmux -A (attach-or-create), got %q", got[2])
	}
}

func TestAttachCommandFallsBackToContinue(t *testing.T) {
	got := attachCommand("http://x:1", "sess", "")
	want := []string{"tmux", "new-session", "-A", "-s", "sess", "opencode", "attach", "http://x:1", "-c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attachCommand (no oc session):\n got %v\n want %v", got, want)
	}
}

func TestExecArgvDockerAndCRI(t *testing.T) {
	inner := []string{"tmux", "ls"}
	docker := execArgv([]string{"docker", "exec", "-it"}, "agent123", inner)
	if !reflect.DeepEqual(docker, []string{"docker", "exec", "-it", "agent123", "tmux", "ls"}) {
		t.Fatalf("docker exec argv wrong: %v", docker)
	}
	cri := execArgv([]string{"crictl", "exec", "-it"}, "cri456", inner)
	if !reflect.DeepEqual(cri, []string{"crictl", "exec", "-it", "cri456", "tmux", "ls"}) {
		t.Fatalf("crictl exec argv wrong: %v", cri)
	}
}

func TestParseMoshConnect(t *testing.T) {
	port, key, err := parseMoshConnect("MOSH CONNECT 60001 7MusRvLzjBJknfj9jli2aw\n\nmosh-server (mosh 1.4.0)")
	if err != nil {
		t.Fatal(err)
	}
	if port != 60001 || key != "7MusRvLzjBJknfj9jli2aw" {
		t.Fatalf("parsed port=%d key=%q", port, key)
	}
	if _, _, err := parseMoshConnect("garbage output"); err == nil {
		t.Fatal("expected error on missing MOSH CONNECT line")
	}
}

func TestMoshServerArgs(t *testing.T) {
	with := moshServerArgs("10.0.0.1", []string{"docker", "exec", "-it", "id", "tmux"})
	want := []string{"new", "-i", "10.0.0.1", "--", "docker", "exec", "-it", "id", "tmux"}
	if !reflect.DeepEqual(with, want) {
		t.Fatalf("moshServerArgs with ip:\n got %v\n want %v", with, want)
	}
	without := moshServerArgs("", []string{"x"})
	if !reflect.DeepEqual(without, []string{"new", "--", "x"}) {
		t.Fatalf("moshServerArgs no ip: %v", without)
	}
}
