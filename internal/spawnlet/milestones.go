package spawnlet

// Milestone key constants for provisioning step reporting (sp-m859.3).
const (
	MilestoneAuthorize       = "authorize"
	MilestoneMintCredentials = "mint-credentials"
	MilestonePrepareMounts   = "prepare-mounts"
	MilestoneRestoreSnapshot = "restore-snapshot"
	MilestoneCreatePod       = "create-pod"
	MilestoneSetupNetwork    = "setup-network"
	MilestonePullImage       = "pull-image"
	MilestoneStartAgent      = "start-agent"
	MilestoneAwaitReady      = "await-ready"
)

// Milestone is a single provisioning step (key + human label).
type Milestone struct{ Key, Label string }

// ProvisionFlags describes which optional milestones apply to a given spawn.
type ProvisionFlags struct {
	MintCredentials bool
	RestoreSnapshot bool
	SetupNetwork    bool
	AwaitReady      bool
}

// fullCatalog is the ordered list of all possible milestones. Order matches the
// typical code execution sequence; individual milestones may be skipped via their
// include predicate. See the "Catalog-order vs code-order skew" note in the spec:
// cross-node rootfs restore-snapshot emits after setup-network, and a sidecar-probe
// failure attributes to start-agent — both are documented limitations.
var fullCatalog = []struct {
	Milestone
	include func(ProvisionFlags) bool
}{
	{Milestone{MilestoneAuthorize, "Authorize"}, func(ProvisionFlags) bool { return true }},
	{Milestone{MilestoneMintCredentials, "Mint credentials"}, func(f ProvisionFlags) bool { return f.MintCredentials }},
	{Milestone{MilestonePrepareMounts, "Prepare mounts"}, func(ProvisionFlags) bool { return true }},
	{Milestone{MilestoneRestoreSnapshot, "Restore snapshot"}, func(f ProvisionFlags) bool { return f.RestoreSnapshot }},
	{Milestone{MilestoneCreatePod, "Create pod"}, func(ProvisionFlags) bool { return true }},
	{Milestone{MilestoneSetupNetwork, "Set up network"}, func(f ProvisionFlags) bool { return f.SetupNetwork }},
	{Milestone{MilestonePullImage, "Pull image"}, func(ProvisionFlags) bool { return true }},
	{Milestone{MilestoneStartAgent, "Start agent"}, func(ProvisionFlags) bool { return true }},
	{Milestone{MilestoneAwaitReady, "Await agent ready"}, func(f ProvisionFlags) bool { return f.AwaitReady }},
}

// ApplicableMilestones returns the ordered slice of milestones that apply for flags.
func ApplicableMilestones(f ProvisionFlags) []Milestone {
	out := make([]Milestone, 0, len(fullCatalog))
	for _, c := range fullCatalog {
		if c.include(f) {
			out = append(out, c.Milestone)
		}
	}
	return out
}

// FindMilestone returns the 1-based index, Milestone, and ok=true for the given key
// within steps. Returns 0, Milestone{}, false if not found.
func FindMilestone(steps []Milestone, key string) (index int, m Milestone, ok bool) {
	for i, s := range steps {
		if s.Key == key {
			return i + 1, s, true
		}
	}
	return 0, Milestone{}, false
}
