//go:build e2e

// The Docker arm of the PodBackend contract (sp-2tx8.1.3). Runs the same RunContract table the fake
// runs, against the real DockerPodBackend on a real daemon — that is what makes the fake's fidelity
// enforced rather than assumed.
//
// Needs: a reachable Docker daemon + the fixture images (`make images` → spawnery/stubagent:dev,
// spawnery/sidecar:dev). Per project convention the `e2e` tag IS the opt-in, so a missing dep is a
// FAILURE, never a skip.
//
//	make images && go test -tags e2e -count=1 -timeout 20m -run TestPodBackendContract_Docker ./internal/runtime/
//
// LANE DIVERGENCES recorded here (see the SE1 spec §4.3):
//   - Docker's AgentSpec.Cmd maps to container Config.Cmd (overrides CMD, KEEPS ENTRYPOINT), while the
//     CRI lane's maps to Command (overrides ENTRYPOINT). This arm drives Cmd=nil, so both lanes fall
//     through to the image's own entrypoint and the divergence is not exercised by the contract.
//   - Docker's Stop stops but does not remove; stopped pods therefore linger in ListManaged. The
//     contract does not assert their disappearance; this Env force-removes them in cleanup.
//   - Docker's ListManaged reports SidecarID+AgentID and no SandboxID (CRI reports only SandboxID).
package runtime_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"spawnery/internal/runtime"
	"spawnery/internal/runtime/podbackendtest"
)

const (
	dockerAgentImage   = "spawnery/stubagent:dev"
	dockerSidecarImage = "spawnery/sidecar:dev"
	// dockerRootfsFile is in the agent's writable layer and under no mount, so a delta capture
	// commits it.
	dockerRootfsFile = "/contract-marker"
)

// dockerRuntime builds the ContainerRuntime, failing loudly when Docker is not usable.
func dockerRuntime(t *testing.T) *runtime.Docker {
	t.Helper()
	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable: %v (the e2e tag requires a reachable Docker daemon)", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.Ping(ctx); err != nil {
		t.Fatalf("docker not pingable: %v (the e2e tag requires a reachable Docker daemon)", err)
	}
	return rt
}

// requireImage fails when a fixture image is missing — with the command that builds it.
func requireImage(ctx context.Context, t *testing.T, rt *runtime.Docker, ref string) {
	t.Helper()
	_, ok, err := rt.InspectImage(ctx, ref)
	if err != nil {
		t.Fatalf("inspect %s: %v", ref, err)
	}
	if !ok {
		t.Fatalf("fixture image %s is not present — build it with `make images`", ref)
	}
}

// uniqueNodeID gives each Factory call its own node id, so the cleanup can find exactly the
// containers this case created (label spawnery.node-id).
func uniqueNodeID(t *testing.T) string {
	t.Helper()
	return "node-ct-" + randomHex(t)
}

// randomHex returns 12 random hex digits (6 bytes), suitable for a Docker tag component.
func randomHex(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// mobyClient is the raw daemon client the hooks need: runtime.ContainerRuntime has no exec and no
// arbitrary-entrypoint run, and the contract's Write/Exec/ReadArtifact hooks need both.
func mobyClient(t *testing.T) *dockerclient.Client {
	t.Helper()
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker sdk client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// removeNodeContainers force-removes every container this case created (matched by the node-id label).
// Docker's Stop only stops, so without this the daemon accumulates a stopped pod per contract case.
func removeNodeContainers(t *testing.T, nodeID string) {
	t.Helper()
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Logf("cleanup: docker sdk client: %v", err)
		return
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cs, err := cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", runtime.LabelNodeID+"="+nodeID)),
	})
	if err != nil {
		t.Logf("cleanup: list containers: %v", err)
		return
	}
	for _, c := range cs {
		if err := cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			t.Logf("cleanup: remove %s: %v", c.ID, err)
		}
	}
}

// execInContainer runs argv in a RUNNING container and fails on a non-zero exit. It errors when the
// container is stopped, removed, or PAUSED — which is precisely what the contract's Write and Exec
// hooks are specified to do (a paused container cannot run a process; `docker exec` says so).
func execInContainer(ctx context.Context, cli *dockerclient.Client, containerID string, argv []string) error {
	ex, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          argv,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("exec create %v: %w", argv, err)
	}
	att, err := cli.ContainerExecAttach(ctx, ex.ID, container.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("exec attach %v: %w", argv, err)
	}
	defer att.Close()

	var out, errOut bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &errOut, att.Reader); err != nil {
		return fmt.Errorf("exec read %v: %w", argv, err)
	}
	insp, err := cli.ContainerExecInspect(ctx, ex.ID)
	if err != nil {
		return fmt.Errorf("exec inspect %v: %w", argv, err)
	}
	if insp.ExitCode != 0 {
		return fmt.Errorf("exec %v exited %d: %s", argv, insp.ExitCode, strings.TrimSpace(errOut.String()))
	}
	return nil
}

// shellQuote single-quotes s for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readFileFromImage runs `cat file` in a throwaway container built FROM ref (entrypoint overridden,
// because the delta image inherits the agent image's entrypoint) and returns the file's bytes.
func readFileFromImage(ctx context.Context, cli *dockerclient.Client, ref, file string) ([]byte, error) {
	created, err := cli.ContainerCreate(ctx, &container.Config{
		Image:      ref,
		Entrypoint: []string{"/bin/cat"},
		Cmd:        []string{file},
	}, &container.HostConfig{}, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("create reader container from %s: %w", ref, err)
	}
	defer func() {
		_ = cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
	}()
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("start reader container from %s: %w", ref, err)
	}
	statusC, errC := cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errC:
		return nil, fmt.Errorf("wait reader container: %w", err)
	case st := <-statusC:
		if st.StatusCode != 0 {
			return nil, fmt.Errorf("cat %s in %s exited %d (file absent from the artifact?)", file, ref, st.StatusCode)
		}
	}
	logs, err := cli.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return nil, fmt.Errorf("logs of reader container: %w", err)
	}
	defer func() { _ = logs.Close() }()
	var out, errOut bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &errOut, logs); err != nil {
		return nil, fmt.Errorf("demux reader logs: %w", err)
	}
	return out.Bytes(), nil
}

// armedBackend wraps the real DockerPodBackend and, while armed, swaps the handle's pinned base ref for
// a DEEPER image (base + 1 committed layer) before delegating the capture. The shipped guard is
// `committed.Layers <= InspectImage(h.BaseImageRef).Layers`, so a commit that can add at most one layer
// over the true base necessarily lands <= the deeper ref's count and the guard MUST fire. This models
// moby#47065 (a commit that adds no layer relative to the pinned base) without asking the daemon to
// reproduce the bug on cue.
type armedBackend struct {
	runtime.PodBackend
	deepRef string
	armed   bool
}

func (a *armedBackend) baseFor(h *runtime.PodHandle) (restore func()) {
	if !a.armed {
		return func() {}
	}
	orig := h.BaseImageRef
	h.BaseImageRef = a.deepRef
	return func() { h.BaseImageRef = orig }
}

func (a *armedBackend) CaptureDelta(ctx context.Context, h *runtime.PodHandle) (string, error) {
	defer a.baseFor(h)()
	return a.PodBackend.CaptureDelta(ctx, h)
}

func (a *armedBackend) CaptureDeltaAs(ctx context.Context, h *runtime.PodHandle, target string) (string, error) {
	defer a.baseFor(h)()
	return a.PodBackend.CaptureDeltaAs(ctx, h, target)
}

// buildDeepImage commits a throwaway container (base + one written file) to a unique tag and asserts it
// has exactly one layer more than the base. Returns the ref; registers its removal with t.Cleanup.
func buildDeepImage(ctx context.Context, t *testing.T, rt *runtime.Docker, cli *dockerclient.Client, base string) string {
	t.Helper()
	deep := "spawnery/contract-deep:" + randomHex(t)
	created, err := cli.ContainerCreate(ctx, &container.Config{
		Image:      base,
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{"echo deep > /contract-deep-layer"},
	}, &container.HostConfig{}, nil, nil, "")
	if err != nil {
		t.Fatalf("create deep-image container: %v", err)
	}
	defer func() {
		_ = cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
	}()
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start deep-image container: %v", err)
	}
	statusC, errC := cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errC:
		t.Fatalf("wait deep-image container: %v", err)
	case st := <-statusC:
		if st.StatusCode != 0 {
			t.Fatalf("deep-image container exited %d", st.StatusCode)
		}
	}
	if _, err := rt.CommitContainer(ctx, created.ID, deep); err != nil {
		t.Fatalf("commit deep image: %v", err)
	}
	t.Cleanup(func() { _ = rt.RemoveImage(context.Background(), deep) })

	bi, ok, err := rt.InspectImage(ctx, base)
	if err != nil || !ok {
		t.Fatalf("inspect base %s: ok=%v err=%v", base, ok, err)
	}
	di, ok, err := rt.InspectImage(ctx, deep)
	if err != nil || !ok {
		t.Fatalf("inspect deep %s: ok=%v err=%v", deep, ok, err)
	}
	if di.Layers != bi.Layers+1 {
		t.Fatalf("deep image %s has %d layers, want base+1 = %d — the zero-layer arm would not trip the guard",
			deep, di.Layers, bi.Layers+1)
	}
	return deep
}

// dockerFactory builds a fresh Env per contract case.
func dockerFactory(t *testing.T) *podbackendtest.Env {
	t.Helper()
	ctx := context.Background()
	rt := dockerRuntime(t)
	requireImage(ctx, t, rt, dockerAgentImage)
	requireImage(ctx, t, rt, dockerSidecarImage)

	nodeID := uniqueNodeID(t)
	backend := runtime.NewDockerPodBackend(rt, "", dockerAgentImage)
	t.Cleanup(func() { removeNodeContainers(t, nodeID) })

	cli := mobyClient(t)
	armed := &armedBackend{PodBackend: backend}

	return &podbackendtest.Env{
		Backend:    armed,
		NodeID:     nodeID,
		BaseImage:  dockerAgentImage,
		RootfsFile: dockerRootfsFile,
		PodSpec: func(spawnID string, labels map[string]string) runtime.PodSpec {
			return runtime.PodSpec{
				ID:           spawnID,
				SidecarImage: dockerSidecarImage,
				// The sidecar proxy exits without a key; it makes no upstream call in this suite.
				SidecarEnv: []string{"OPENROUTER_API_KEY=unused-by-the-contract"},
				Labels:     labels,
			}
		},
		AgentSpec: func(_, imageRef string, labels map[string]string) runtime.AgentSpec {
			// Cmd nil => the stubagent image's own entrypoint (launcher → acpmux on tcp/7000).
			return runtime.AgentSpec{Image: imageRef, Labels: labels}
		},
		Write: func(ctx context.Context, h *runtime.PodHandle, file string, data []byte) error {
			return execInContainer(ctx, cli, h.AgentID, []string{
				"/bin/sh", "-c", "printf %s " + shellQuote(string(data)) + " > " + shellQuote(file),
			})
		},
		ReadArtifact: func(ctx context.Context, ref, file string) ([]byte, error) {
			return readFileFromImage(ctx, cli, ref, file)
		},
		Exec: func(ctx context.Context, h *runtime.PodHandle, argv []string) error {
			return execInContainer(ctx, cli, h.AgentID, argv)
		},
		ArmZeroLayerCapture: func() func() {
			armed.deepRef = buildDeepImage(context.Background(), t, rt, cli, dockerAgentImage)
			armed.armed = true
			return func() { armed.armed = false }
		},
		// No exceptions: the Docker lane supports the whole contract. If one of these turns out to be
		// unsupportable, register it HERE with a reason — do not weaken the contract case.
		Exceptions: nil,
	}
}

func TestPodBackendContract_Docker(t *testing.T) {
	podbackendtest.RunContract(t, dockerFactory)
}
