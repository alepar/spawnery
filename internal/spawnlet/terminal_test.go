package spawnlet

import (
	"reflect"
	"testing"
)

func TestSessionTitle(t *testing.T) {
	cases := []struct{ name, app, want string }{
		{"Secret App 2", "Secret", "Secret App 2 (Secret)"},
		{"Secret App 2", "", "Secret App 2"},
		{"", "Secret", "Secret"},
		{"", "", ""},
		{"  Trimmed  ", "  App  ", "Trimmed (App)"},
	}
	for _, c := range cases {
		if got := sessionTitle(c.name, c.app); got != c.want {
			t.Errorf("sessionTitle(%q,%q)=%q want %q", c.name, c.app, got, c.want)
		}
	}
}

func TestAttachCommandRunsLauncher(t *testing.T) {
	// The in-container command is the baked launcher, which owns TERM + `opencode attach -s <id>`.
	got := attachCommand()
	if !reflect.DeepEqual(got, []string{"spawn-tui"}) {
		t.Fatalf("attachCommand = %v, want [spawn-tui]", got)
	}
}

func TestExecArgvWrapsLauncher(t *testing.T) {
	got := execArgv([]string{"docker", "exec", "-it"}, "agent123", attachCommand())
	want := []string{"docker", "exec", "-it", "agent123", "spawn-tui"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("execArgv = %v, want %v", got, want)
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

func TestTerminalInnerCmd(t *testing.T) {
	// tmux spawn → attach to the in-container tmux session
	if got := terminalInnerCmd(&Spawn{Mode: "tmux"}); len(got) != 4 ||
		got[0] != "tmux" || got[1] != "attach" || got[2] != "-t" || got[3] != "spawn" {
		t.Fatalf("tmux inner cmd = %v, want [tmux attach -t spawn]", got)
	}
	// acp spawn → launch nori with the baked "spawnery" custom agent (-> acpdial -> acpmux),
	// joining the shared goose session as the web (sp-9xr.12.2).
	wantACP := []string{"nori", "-a", "spawnery",
		"--skip-welcome", "--skip-trust-directory", "--dangerously-bypass-approvals-and-sandbox"}
	if got := terminalInnerCmd(&Spawn{Mode: "acp"}); !reflect.DeepEqual(got, wantACP) {
		t.Fatalf("acp inner cmd = %v, want %v", got, wantACP)
	}
	// served/opencode (and legacy "") → the opencode TUI launcher
	for _, mode := range []string{"served", ""} {
		if got := terminalInnerCmd(&Spawn{Mode: mode}); len(got) != 1 || got[0] != "spawn-tui" {
			t.Fatalf("mode %q inner cmd = %v, want [spawn-tui]", mode, got)
		}
	}
}
