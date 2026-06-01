//go:build acp_e2e

package runtime_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"spawnery/internal/runtime"
)

func TestAttachACPRoundtrip(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "acpadapter")
	if out, err := exec.Command("go", "build", "-o", bin, "../../deploy/agent/acpadapter").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "cat") // default ACP_SOCKET=@spawnlet-acp
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	defer cmd.Process.Kill()
	time.Sleep(300 * time.Millisecond) // let it bind

	att, err := runtime.AttachACP(context.Background(), "/proc/self/ns/net")
	if err != nil {
		t.Fatalf("AttachACP: %v", err)
	}
	defer att.Close()

	if _, err := io.WriteString(att.Stdin, "secret-word\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("secret-word\n"))
	if _, err := io.ReadFull(att.Stdout, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "secret-word\n" {
		t.Fatalf("got %q", buf)
	}
}
