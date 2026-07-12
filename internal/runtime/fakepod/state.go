// Package fakepod is a behaviour-faithful, in-memory implementation of runtime.PodBackend for
// hermetic tests. It models three things the ad-hoc fakes it replaces did not: per-container
// lifecycle state machines (illegal transitions FAIL), an image/layer chain, and the CONTENT of the
// agent partitioned into rootfs and mount views — which is what makes capture-time consistency (and
// a torn suspend snapshot) observable without Docker.
//
// It is a non-test package because internal/spawnlet's tests, internal/node's tests and the
// cross-lane contract suite all import it. No production code imports it.
//
// NOTE: this is NOT runtime.NewFake()/runtime.FakeRuntime — that is a fake of the older,
// per-container runtime.ContainerRuntime interface. Two fakes of two interfaces; the package
// boundary keeps them apart.
package fakepod

import "fmt"

// State is a container's lifecycle state.
type State string

const (
	StateAbsent  State = "absent"
	StateCreated State = "created"
	StateRunning State = "running"
	StatePaused  State = "paused"
	StateStopped State = "stopped"
	StateRemoved State = "removed"
)

// legalEdges is the transition table: legalEdges[from][to]. Anything not in it is an ERROR, not a
// no-op — a fake that cheerfully accepts CaptureDelta on a removed container cannot catch a fork
// that removed its own source.
var legalEdges = map[State]map[State]bool{
	StateAbsent:  {StateCreated: true},
	StateCreated: {StateRunning: true, StateRemoved: true},
	StateRunning: {StatePaused: true, StateStopped: true},
	StatePaused:  {StateRunning: true, StateStopped: true},
	StateStopped: {StateRemoved: true},
	StateRemoved: {},
}

// container is one container in a pod: the sandbox, the sidecar or the agent.
// NOTE: keep this struct free of fields nothing reads — golangci's `unused` (staticcheck U1000)
// flags unused unexported struct fields, and the repo's bar is 0 issues. The image and the labels
// live on the pod (see pod.podSpec / pod.agentSpec / pod.labels), not here.
type container struct {
	id    string
	role  string // "sandbox" | "sidecar" | "agent"
	state State
}

// transition moves c to want, or returns an error naming the illegal edge.
func (c *container) transition(want State) error {
	if !legalEdges[c.state][want] {
		return fmt.Errorf("fakepod: illegal transition for %s container %q: %s -> %s",
			c.role, c.id, c.state, want)
	}
	c.state = want
	return nil
}

// requireState returns an error unless c is in one of want.
func (c *container) requireState(op string, want ...State) error {
	for _, w := range want {
		if c.state == w {
			return nil
		}
	}
	return fmt.Errorf("fakepod: %s: %s container %q is %s (want one of %v)", op, c.role, c.id, c.state, want)
}
