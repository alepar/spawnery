// Package runtime is the spawnlet's container-orchestration boundary.
package runtime

import (
	"context"
	"fmt"
	"io"
)

// ImageInfo is the subset of image metadata consumed by the delta-capture path.
// Layers is used to guard against the moby#47065 zero-layer-commit class of bugs.
type ImageInfo struct {
	ID          string
	RepoDigests []string
	Layers      int // len(RootFS.Layers)
}

// CapPolicy controls the Linux capability set applied to a container.
// Zero value is CapDefaultSet so unset container specs (sidecar, preflight smoke)
// keep today's behavior: no CapDrop, engine default capability set.
type CapPolicy int

const (
	// CapDefaultSet leaves the engine's default capability set in place — used when
	// the daemon's userns-remap or a sentry (runsc/native) shields the host.
	CapDefaultSet CapPolicy = iota
	// CapDropAll issues CapDrop=ALL — the degraded / legacy behavior used when no
	// kernel user-namespace isolation is present (USERNS_MODE=off or degraded fallback).
	CapDropAll
)

// CapPolicyForUsernsMode returns the CapPolicy implied by the node's USERNS_MODE string.
// "remap" and "native" both provide kernel isolation (userns-remap or runsc sentry) and
// allow the agent's default capability set. Any other value ("off", "", unknown) falls
// back to CapDropAll so an unprotected node always applies the safe default.
func CapPolicyForUsernsMode(mode string) CapPolicy {
	switch mode {
	case "remap", "native":
		return CapDefaultSet
	default:
		return CapDropAll
	}
}

// ACPPort is the TCP port the in-pod acpadapter listens on for the node's ACP
// connection. Both lanes use TCP: the runc/shared-netns lane dials the pod bridge
// IP, the runsc/CRI lane dials the pod IP via the CNI bridge. (gVisor isolates the
// in-sandbox abstract-UDS namespace, and the opencode adapter has no stdio ACP
// channel, so stdio attach is gone.)
const ACPPort = 7000

type Mount struct {
	HostPath, ContainerPath string
	ReadOnly                bool
}

type ContainerSpec struct {
	Image       string
	Cmd         []string
	Env         []string
	Mounts      []Mount
	NetnsOf     string            // if set, join this container's network namespace
	AttachStdio bool              // attach stdin+stdout (for the agent)
	MemoryBytes int64             // 0 = unlimited
	NanoCPUs    int64             // 0 = unlimited; 1 CPU = 1_000_000_000
	PidsLimit   int64             // 0 = unlimited
	Runtime     string            // "" = Docker default; e.g. "runsc"
	CapPolicy   CapPolicy         // zero = CapDefaultSet (engine default capability set)
	CapAdd      []string          // capabilities to ADD — rejected by the Docker backend (§7 floor-defeat guard)
	Labels      map[string]string // container labels (spawnery.managed/spawn-id/generation/node-id/role)
}

// ContainerSummary is the minimal view ListByLabel returns: the container id + its labels.
type ContainerSummary struct {
	ID     string
	Labels map[string]string
}

// AttachedStream is the agent's bidirectional stdio (demuxed stdout).
type AttachedStream struct {
	// Stdin: do not Close independently — closing it tears down the whole attach; use Close() for teardown.
	Stdin  io.WriteCloser
	Stdout io.Reader
	Close  func() error
}

type ContainerRuntime interface {
	Ping(ctx context.Context) error
	StartContainer(ctx context.Context, s ContainerSpec) (id string, err error)
	Attach(ctx context.Context, id string) (*AttachedStream, error)
	StopContainer(ctx context.Context, id string) error
	ContainerPID(ctx context.Context, id string) (int, error)
	ContainerIP(ctx context.Context, id string) (string, error)
	// ListByLabel returns all containers (any state) carrying label key=value, with their labels.
	ListByLabel(ctx context.Context, key, value string) ([]ContainerSummary, error)

	// CommitContainer stops the container (without removing it) then commits its writable
	// layer to a new image tagged ref. Used by the Docker delta-capture path (spec §2).
	CommitContainer(ctx context.Context, containerID, ref string) (imageID string, err error)
	// InspectImage returns image metadata for ref. exists=false (nil err) when the image is
	// not present locally (equivalent to a docker inspect "not found").
	InspectImage(ctx context.Context, ref string) (info ImageInfo, exists bool, err error)
	// RemoveImage removes the image tagged ref. A not-found image is not an error (idempotent).
	RemoveImage(ctx context.Context, ref string) error
	// ExportTopLayer writes ONLY ref's top (writable/delta) layer as an uncompressed tar — not
	// the whole image. Delta-only migration (sp-ei4.1.14): docker save/load cannot ship a layer
	// against a base already on the target (moby#18723), so the layer is shipped alone and
	// reassembled via AssembleOnBase; uncompressed so the Kopia journal's CDC dedup collapses
	// successive deltas. Preserves .wh. whiteouts / modes / xattrs / uids byte-for-byte.
	ExportTopLayer(ctx context.Context, ref string, w io.Writer) error
	// AssembleOnBase reads baseRef from the runtime, appends the single delta layer tar, and
	// writes the result back as newTag — reconstructing base+delta on a target that already has
	// the pinned base (sp-ei4.1.14). baseRef must be present locally.
	AssembleOnBase(ctx context.Context, baseRef, newTag string, layer io.Reader) error
	// PauseContainer pauses all processes in the container (SIGSTOP / cgroup freeze).
	// Used by the suspend gate to quiesce agent writes before the final snapshot (spec §3).
	PauseContainer(ctx context.Context, id string) error
	// UnpauseContainer resumes a previously-paused container.
	UnpauseContainer(ctx context.Context, id string) error
}

// FakeRuntime records calls for unit tests.
type FakeRuntime struct {
	Started []ContainerSpec
	Stopped map[string]bool
	byID    map[string]ContainerSpec // id -> spec (for ListByLabel)
	n       int

	// Delta-capture fields (docker-lane image ops, spec §2–4).
	// Committed is an ordered log of CommitContainer calls.
	Committed []struct{ ContainerID, Ref string }
	// Images is the seeded + committed image store, keyed by ref.
	// Tests seed this to control InspectImage responses.
	Images map[string]ImageInfo
	// Removed is an ordered log of RemoveImage calls.
	Removed []string
	// ImageArchives are fake archive payloads keyed by image ref. Tests use these
	// to assert ExportImage streams the exact deterministic delta tag.
	ImageArchives map[string][]byte
	// ExportedImages is an ordered log of ExportImage refs.
	ExportedImages []string
	// ImportedImages is an ordered log of refs loaded by ImportImage.
	ImportedImages []string
	// CommitLayers overrides the layer count of a committed image (0 = base+1 default).
	// Set to a value ≤ base layers to trigger the moby#47065 guard in tests.
	CommitLayers int

	// Paused is an ordered log of PauseContainer calls (container ids).
	Paused []string
	// Unpaused is an ordered log of UnpauseContainer calls (container ids).
	Unpaused []string
}

func NewFake() *FakeRuntime {
	return &FakeRuntime{
		Stopped:       map[string]bool{},
		byID:          map[string]ContainerSpec{},
		Images:        map[string]ImageInfo{},
		ImageArchives: map[string][]byte{},
	}
}

func (f *FakeRuntime) Ping(_ context.Context) error { return nil }

func (f *FakeRuntime) StartContainer(_ context.Context, s ContainerSpec) (string, error) {
	f.n++
	id := fmt.Sprintf("fake-%d", f.n)
	f.Started = append(f.Started, s)
	f.byID[id] = s
	return id, nil
}
func (f *FakeRuntime) Attach(_ context.Context, id string) (*AttachedStream, error) {
	pr, pw := io.Pipe()
	return &AttachedStream{Stdin: pw, Stdout: pr, Close: func() error { return pw.Close() }}, nil
}
func (f *FakeRuntime) StopContainer(_ context.Context, id string) error {
	f.Stopped[id] = true
	return nil
}
func (f *FakeRuntime) ContainerPID(_ context.Context, id string) (int, error) { return 4242, nil }
func (f *FakeRuntime) ContainerIP(_ context.Context, id string) (string, error) {
	return "172.17.0.99", nil
}
func (f *FakeRuntime) ListByLabel(_ context.Context, key, value string) ([]ContainerSummary, error) {
	var out []ContainerSummary
	for id, s := range f.byID {
		if f.Stopped[id] {
			continue
		}
		if s.Labels[key] == value {
			out = append(out, ContainerSummary{ID: id, Labels: s.Labels})
		}
	}
	return out, nil
}

// CommitContainer records the call and synthesises a committed ImageInfo into Images[ref].
// The committed image's layer count is: CommitLayers if > 0, else (base layers)+1.
// "base layers" is derived from the seeded Images entry whose ref was the last-started container's
// image — tests that need precise layer counts should seed Images[ref] for the base before calling.
func (f *FakeRuntime) CommitContainer(_ context.Context, containerID, ref string) (string, error) {
	f.Committed = append(f.Committed, struct{ ContainerID, Ref string }{ContainerID: containerID, Ref: ref})
	// Derive the base layer count from the seeded image that matches the last-started container.
	baseLayers := 0
	for _, cs := range f.Started {
		if bi, ok := f.Images[cs.Image]; ok {
			if bi.Layers > baseLayers {
				baseLayers = bi.Layers
			}
		}
	}
	layers := baseLayers + 1
	if f.CommitLayers > 0 {
		layers = f.CommitLayers
	}
	id := fmt.Sprintf("sha256:committed-%s", ref)
	f.Images[ref] = ImageInfo{ID: id, Layers: layers}
	return id, nil
}

func (f *FakeRuntime) InspectImage(_ context.Context, ref string) (ImageInfo, bool, error) {
	info, ok := f.Images[ref]
	return info, ok, nil
}

func (f *FakeRuntime) RemoveImage(_ context.Context, ref string) error {
	f.Removed = append(f.Removed, ref)
	delete(f.Images, ref)
	return nil
}

func (f *FakeRuntime) ExportTopLayer(_ context.Context, ref string, w io.Writer) error {
	f.ExportedImages = append(f.ExportedImages, ref)
	payload, ok := f.ImageArchives[ref]
	if !ok {
		if _, ok := f.Images[ref]; !ok {
			return fmt.Errorf("image %q not found", ref)
		}
		payload = []byte(ref + "\n")
	}
	_, err := w.Write(payload)
	return err
}

func (f *FakeRuntime) AssembleOnBase(_ context.Context, baseRef, newTag string, layer io.Reader) error {
	if _, err := io.Copy(io.Discard, layer); err != nil {
		return err
	}
	if baseRef != "" {
		if _, ok := f.Images[baseRef]; !ok {
			return fmt.Errorf("base image %q not found", baseRef)
		}
	}
	f.ImportedImages = append(f.ImportedImages, newTag)
	f.Images[newTag] = ImageInfo{ID: "sha256:assembled-" + newTag, Layers: 1}
	return nil
}

func (f *FakeRuntime) PauseContainer(_ context.Context, id string) error {
	f.Paused = append(f.Paused, id)
	return nil
}

func (f *FakeRuntime) UnpauseContainer(_ context.Context, id string) error {
	f.Unpaused = append(f.Unpaused, id)
	return nil
}
