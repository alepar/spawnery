package runtime

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

type Docker struct{ cli *client.Client }

func NewDocker() (*Docker, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Docker{cli: cli}, nil
}

func (d *Docker) Ping(ctx context.Context) error {
	_, err := d.cli.Ping(ctx)
	return err
}

func (d *Docker) StartContainer(ctx context.Context, s ContainerSpec) (string, error) {
	if err := assertNoAddedCaps(s); err != nil {
		return "", err
	}
	cfg := &container.Config{
		Image:       s.Image,
		Cmd:         s.Cmd,
		Env:         s.Env,
		Labels:      s.Labels,
		OpenStdin:   s.AttachStdio,
		StdinOnce:   false,
		AttachStdin: s.AttachStdio,
		Tty:         false,
	}
	host := buildHostConfig(s)
	created, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, "")
	if err != nil {
		return "", err
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = d.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
		return "", err
	}
	return created.ID, nil
}

type logWriter struct{ prefix string }

func (l logWriter) Write(p []byte) (int, error) { log.Printf("%s%s", l.prefix, p); return len(p), nil }

func buildHostConfig(s ContainerSpec) *container.HostConfig {
	host := &container.HostConfig{}
	if s.NetnsOf != "" {
		host.NetworkMode = container.NetworkMode("container:" + s.NetnsOf)
	}
	for _, m := range s.Mounts {
		host.Binds = append(host.Binds, bind(m))
	}
	if s.MemoryBytes > 0 {
		host.Resources.Memory = s.MemoryBytes
	}
	if s.NanoCPUs > 0 {
		host.Resources.NanoCPUs = s.NanoCPUs
	}
	if s.PidsLimit > 0 {
		p := s.PidsLimit
		host.Resources.PidsLimit = &p
	}
	if s.Runtime != "" {
		host.Runtime = s.Runtime
	}
	switch s.CapPolicy {
	case CapDropAll:
		host.CapDrop = []string{"ALL"}
	default: // CapDefaultSet: no CapDrop/CapAdd — engine default capability set
	}
	return host
}

func bind(m Mount) string {
	b := m.HostPath + ":" + m.ContainerPath
	if m.ReadOnly {
		b += ":ro"
	}
	return b
}

// assertNoAddedCaps rejects any ContainerSpec that requests added capabilities.
// Granting extra caps (e.g. CAP_NET_ADMIN) in the agent container lets the agent flush the
// egress floor in the sidecar-owned shared network namespace — a direct floor-defeat (spec §7).
// This is always wrong: the agent path never sets CapAdd; the assertion is a defensive guard.
func assertNoAddedCaps(s ContainerSpec) error {
	if len(s.CapAdd) > 0 {
		return fmt.Errorf("capability add rejected: agent containers must not receive extra capabilities "+
			"(got %v) — granting CAP_NET_ADMIN or similar lets the agent flush the egress floor in the "+
			"shared sidecar netns (spec §7 floor-defeat guard)", s.CapAdd)
	}
	return nil
}

// UsernsRemap probes the Docker daemon to determine whether it is running with
// userns-remap enabled and, if so, parses the remap base UID from the daemon's
// data-root directory suffix (e.g. "/var/lib/docker/700000.700000" → 700000).
//
// Returns (base, true, nil) when userns-remap is active and the base UID is parsed.
// Returns (0, false, nil) when userns-remap is not active (degraded: caller falls back to off).
// Returns a non-nil error when the probe itself fails or the base UID cannot be parsed.
func (d *Docker) UsernsRemap(ctx context.Context) (base uint32, active bool, err error) {
	info, err := d.cli.Info(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("docker info: %w", err)
	}
	if !hasUsernsSecurityOption(info) {
		return 0, false, nil
	}
	b, ok := parseRemapBase(info.DockerRootDir)
	if !ok {
		return 0, true, fmt.Errorf("userns-remap is active but remap base UID could not be parsed from DockerRootDir %q", info.DockerRootDir)
	}
	return b, true, nil
}

// hasUsernsSecurityOption reports whether the daemon's SecurityOptions list contains
// a "name=userns" entry, which signals that userns-remap is active.
func hasUsernsSecurityOption(info system.Info) bool {
	for _, opt := range info.SecurityOptions {
		if strings.Contains(opt, "name=userns") {
			return true
		}
	}
	return false
}

// parseRemapBase extracts the remap base UID from a Docker data-root path whose
// last path segment is "<uid>.<gid>" (e.g. "/var/lib/docker/700000.700000" → 700000).
// Returns (0, false) if the path does not match the expected format.
func parseRemapBase(rootDir string) (uint32, bool) {
	base := filepath.Base(rootDir)
	uid, _, ok := strings.Cut(base, ".")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(uid, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

func (d *Docker) Attach(ctx context.Context, id string) (*AttachedStream, error) {
	hijack, err := d.cli.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true, Stdin: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return nil, err
	}
	// Demux multiplexed stdout into a pipe (non-TTY attach is framed).
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, logWriter{prefix: "[agent-stderr] "}, hijack.Reader)
		pw.CloseWithError(err)
	}()
	return &AttachedStream{
		Stdin:  hijack.Conn,
		Stdout: pr,
		Close:  func() error { hijack.Close(); pr.CloseWithError(io.ErrClosedPipe); return nil },
	}, nil
}

func (d *Docker) ContainerPID(ctx context.Context, id string) (int, error) {
	j, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return 0, err
	}
	if j.State == nil || j.State.Pid == 0 {
		return 0, fmt.Errorf("container %s has no pid (not running)", id)
	}
	return j.State.Pid, nil
}

func (d *Docker) ContainerIP(ctx context.Context, id string) (string, error) {
	j, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return "", err
	}
	ip := ""
	if j.NetworkSettings != nil {
		// Docker <=28 exposes the bridge IP on the legacy top-level field; Docker 29+ drops it and
		// only reports per-network endpoints, so fall back to the Networks map (prefer the default
		// "bridge" network, then any attached network with an IP).
		ip = j.NetworkSettings.DefaultNetworkSettings.IPAddress
		if ip == "" {
			if ep := j.NetworkSettings.Networks["bridge"]; ep != nil {
				ip = ep.IPAddress
			}
		}
		if ip == "" {
			for _, ep := range j.NetworkSettings.Networks {
				if ep != nil && ep.IPAddress != "" {
					ip = ep.IPAddress
					break
				}
			}
		}
	}
	if ip == "" {
		return "", fmt.Errorf("container %s has no bridge IP", id)
	}
	return ip, nil
}

func (d *Docker) ListByLabel(ctx context.Context, key, value string) ([]ContainerSummary, error) {
	cs, err := d.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", key+"="+value)),
	})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerSummary, 0, len(cs))
	for _, c := range cs {
		out = append(out, ContainerSummary{ID: c.ID, Labels: c.Labels})
	}
	return out, nil
}

func (d *Docker) StopContainer(ctx context.Context, id string) error {
	ctx = context.WithoutCancel(ctx)
	to := 0
	_ = d.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &to})
	err := d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
	if err != nil && client.IsErrNotFound(err) {
		return nil
	}
	return err
}

// CommitContainer stops the container (without removing it) and commits its writable layer to a
// new image tagged ref. Used by the Docker delta-capture path (spec §2). The caller is responsible
// for removing the container afterward via the normal Stop path.
func (d *Docker) CommitContainer(ctx context.Context, containerID, ref string) (string, error) {
	to := 0
	_ = d.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &to})
	resp, err := d.cli.ContainerCommit(ctx, containerID, container.CommitOptions{Reference: ref, Pause: false})
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// InspectImage returns image metadata for ref. Returns (ImageInfo{}, false, nil) when the image
// is not present locally (equivalent to "docker inspect not found").
func (d *Docker) InspectImage(ctx context.Context, ref string) (ImageInfo, bool, error) {
	info, err := d.cli.ImageInspect(ctx, ref)
	if client.IsErrNotFound(err) {
		return ImageInfo{}, false, nil
	}
	if err != nil {
		return ImageInfo{}, false, err
	}
	return ImageInfo{ID: info.ID, RepoDigests: info.RepoDigests, Layers: len(info.RootFS.Layers)}, true, nil
}

// RemoveImage removes the image tagged ref. A not-found image is silently ignored (idempotent).
func (d *Docker) RemoveImage(ctx context.Context, ref string) error {
	_, err := d.cli.ImageRemove(ctx, ref, image.RemoveOptions{Force: true})
	if client.IsErrNotFound(err) {
		return nil
	}
	return err
}

// ExportTopLayer streams ref's top (delta) layer as an uncompressed tar via go-containerregistry
// (sp-ei4.1.14). docker save/load can't ship a single layer against a base already on the target
// (moby#18723), so we read the image from the daemon, take only its last layer, and stream the
// raw tar — preserving .wh. whiteouts / modes / xattrs / uids and feeding Kopia uncompressed for
// CDC dedup. crane uses its own env-based daemon client (DOCKER_HOST), independent of d.cli.
func (d *Docker) ExportTopLayer(ctx context.Context, ref string, w io.Writer) error {
	r, err := name.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("parse ref %s: %w", ref, err)
	}
	img, err := daemon.Image(r, daemon.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("read image %s from daemon: %w", ref, err)
	}
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("list layers of %s: %w", ref, err)
	}
	if len(layers) == 0 {
		return fmt.Errorf("image %s has no layers", ref)
	}
	rc, err := layers[len(layers)-1].Uncompressed()
	if err != nil {
		return fmt.Errorf("open top layer of %s: %w", ref, err)
	}
	defer rc.Close()
	if _, err := io.Copy(w, rc); err != nil {
		return fmt.Errorf("stream top layer of %s: %w", ref, err)
	}
	return nil
}

// AssembleOnBase reconstructs base+delta on a target that already has the pinned base: read base
// from the daemon, append the shipped delta layer tar (byte-for-byte, preserving whiteouts/uids),
// and write the result back as newTag (sp-ei4.1.14). daemon.Write side-steps the partial-load
// limitation entirely. The layer is buffered to a temp file because tarball.LayerFromFile needs a
// re-openable source.
func (d *Docker) AssembleOnBase(ctx context.Context, baseRef, newTag string, layer io.Reader) error {
	bref, err := name.ParseReference(baseRef)
	if err != nil {
		return fmt.Errorf("parse base ref %s: %w", baseRef, err)
	}
	base, err := daemon.Image(bref, daemon.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("read base %s from daemon: %w", baseRef, err)
	}
	tmp, err := os.CreateTemp("", "spawnery-delta-*.tar")
	if err != nil {
		return fmt.Errorf("temp delta layer file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, layer); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("buffer delta layer: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp delta layer: %w", err)
	}
	dl, err := tarball.LayerFromFile(tmp.Name())
	if err != nil {
		return fmt.Errorf("read delta layer: %w", err)
	}
	out, err := mutate.AppendLayers(base, dl)
	if err != nil {
		return fmt.Errorf("append delta to base %s: %w", baseRef, err)
	}
	tref, err := name.NewTag(newTag)
	if err != nil {
		return fmt.Errorf("parse new tag %s: %w", newTag, err)
	}
	if _, err := daemon.Write(tref, out, daemon.WithContext(ctx)); err != nil {
		return fmt.Errorf("write assembled image %s: %w", newTag, err)
	}
	return nil
}

func (d *Docker) PauseContainer(ctx context.Context, id string) error {
	return d.cli.ContainerPause(ctx, id)
}

func (d *Docker) UnpauseContainer(ctx context.Context, id string) error {
	return d.cli.ContainerUnpause(ctx, id)
}

var _ ContainerRuntime = (*Docker)(nil)
