package spawnlet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoPodReachableControlEndpoint is the adversarial guard for sp-2tx8.9: the node used to run a
// per-spawn INBOUND control listener (/control/gettoken, /control/spawnca) that every pod on the CNI
// bridge could reach, plus a UNIX-socket lane bind-mounted straight into the sidecar. The inversion
// deleted it — the node now PUSHES to the sidecar and never listens. Prove that, on BOTH lanes:
//
//	no SIDECAR_GETTOKEN_* in the sidecar env  (nothing tells a pod where to dial)
//	no /run/spawnery/control mount            (no socket handed into the pod)
//	no per-spawn control dir on disk          (nothing to bind a socket in)
//
// If a future change re-introduces any of the three, this fails.
func TestNoPodReachableControlEndpoint(t *testing.T) {
	for _, usernsMode := range []string{"remap", "off", "native"} {
		t.Run(usernsMode, func(t *testing.T) {
			fb := fakeBackend(t)
			mock := &mockGitHubControlServer{}
			overrideSidecarReadyProbe(t, nil)
			dataRoot := t.TempDir()
			m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
				AgentImage:   "a",
				SidecarImage: "s",
				DataRoot:     dataRoot,
				UsernsMode:   usernsMode,
			})
			m.SetGitHubControlServer(mock)

			if _, err := m.Create(context.Background(), "sp-adv", writeApp(t), "model", "", "", 0); err != nil {
				t.Fatalf("Create: %v", err)
			}

			spec := fb.PodSpec("sp-adv")
			for _, e := range spec.SidecarEnv {
				if strings.HasPrefix(e, "SIDECAR_GETTOKEN") {
					t.Fatalf("sidecar env carries %q — the node's inbound control listener is back "+
						"(sp-2tx8.9 deleted it; the node PUSHES to the sidecar now)", e)
				}
			}
			for _, mn := range spec.SidecarMounts {
				if strings.HasPrefix(mn.ContainerPath, "/run/spawnery/control") {
					t.Fatalf("sidecar mount %q — the GetToken UDS lane is back", mn.ContainerPath)
				}
			}
			if _, err := os.Stat(filepath.Join(dataRoot, "control")); !os.IsNotExist(err) {
				t.Fatalf("control dir under DataRoot exists (stat err = %v); the per-spawn control dir "+
					"is back", err)
			}

			// The push plane, by contrast, MUST still run: the sidecar gets its CA + token delivered.
			if got := len(mock.pushCalls()); got != 1 {
				t.Fatalf("PushCredentials called %d times, want 1 (deleting the listener must not "+
					"delete the delivery)", got)
			}
		})
	}
}
