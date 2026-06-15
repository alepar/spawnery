//go:build fork_spike_e2e

package spawnlet

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/acp"
	"spawnery/internal/runtime"
	"spawnery/internal/storage/journal"
)

const (
	forkSpikeJSONPath     = "/tmp/spawnery-fork-spike-e.json"
	forkSpikeDTS5NotePath = "/tmp/spawnery-fork-spike-e-sp-dts5-note.md"
	forkSpikeSpecNotePath = "/tmp/spawnery-fork-spike-e-spec-note.md"

	repTreeFiles = 19761
	repTreeBytes = int64(164855808)
)

type forkFreezeResult struct {
	Kind                     string        `json:"kind"`
	Samples                  int           `json:"samples,omitempty"`
	DatasetFiles             int           `json:"dataset_files,omitempty"`
	DatasetBytes             int64         `json:"dataset_bytes,omitempty"`
	WarmSnapshot             time.Duration `json:"warm_snapshot,omitempty"`
	UnderPauseP50            time.Duration `json:"under_pause_p50,omitempty"`
	UnderPauseP95            time.Duration `json:"under_pause_p95,omitempty"`
	UnderPauseMax            time.Duration `json:"under_pause_max,omitempty"`
	RootfsCommitP95          time.Duration `json:"rootfs_commit_p95,omitempty"`
	TotalFreezeP95           time.Duration `json:"total_freeze_p95,omitempty"`
	CaptureStoppedSource     bool          `json:"capture_stopped_source,omitempty"`
	TurnOutcome              string        `json:"turn_outcome,omitempty"`
	TurnMarkerCount          int           `json:"turn_marker_count,omitempty"`
	CurrentTurnTruncated     bool          `json:"current_turn_truncated,omitempty"`
	SourceRecovered          bool          `json:"source_recovered,omitempty"`
	TurnBoundaryGateRequired bool          `json:"turn_boundary_gate_required"`
	DecisionReason           string        `json:"decision_reason"`
}

type spikeTelemetry struct {
	continuous chan time.Duration
	final      chan time.Duration
}

func newSpikeTelemetry() *spikeTelemetry {
	return &spikeTelemetry{
		continuous: make(chan time.Duration, 128),
		final:      make(chan time.Duration, 128),
	}
}

func (s *spikeTelemetry) SnapshotDone(_ string, _ string, _ uint64, kind journal.SnapshotKind, scan time.Duration, _ journal.ManifestID, err error) {
	if err != nil {
		return
	}
	switch kind {
	case journal.SnapshotContinuous:
		select {
		case s.continuous <- scan:
		default:
		}
	case journal.SnapshotFinal:
		select {
		case s.final <- scan:
		default:
		}
	}
}

func (s *spikeTelemetry) drain() {
	drainDurationChan(s.continuous)
	drainDurationChan(s.final)
}

func drainDurationChan(ch chan time.Duration) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

type forkFreezeSpikeHarness struct {
	t       *testing.T
	root    string
	mgr     *Manager
	jm      *journal.Manager
	tele    *spikeTelemetry
	appDir  string
	model   string
	sel     AgentSelection
	useSel  bool
	spawnID string
	spawn   *Spawn
	server  *httptest.Server
	client  spawnv1connect.SpawnServiceClient

	cleanups []func()
}

func TestForkFreezeSpikeMeasuresWarmFinalAndCommit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	h := newForkFreezeSpikeHarness(t, ctx, false)
	defer h.cleanup()

	result := h.measureFreeze(ctx, 5)
	if result.Samples != 5 {
		t.Fatalf("samples = %d, want 5", result.Samples)
	}
	if result.WarmSnapshot <= 0 || result.UnderPauseP95 <= 0 || result.RootfsCommitP95 <= 0 {
		t.Fatalf("invalid durations: %+v", result)
	}
	h.writeReports(result)
}

func TestForkFreezeSpikeMidTurnSourceBehavior(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	h := newForkFreezeSpikeHarness(t, ctx, true)
	defer h.cleanup()

	result := h.measureMidTurn(ctx)
	h.writeReports(result)
	if !result.SourceRecovered {
		t.Fatalf("source did not recover after mid-turn pause: %+v", result)
	}
}

func newForkFreezeSpikeHarness(t *testing.T, ctx context.Context, withLLM bool) *forkFreezeSpikeHarness {
	t.Helper()

	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable (need Docker for fork_spike_e2e): %v", err)
	}
	if err := rt.Ping(ctx); err != nil {
		t.Fatalf("docker not pingable (need Docker for fork_spike_e2e): %v", err)
	}

	s3Endpoint := os.Getenv("GARAGE_S3_ENDPOINT")
	adminEndpoint := os.Getenv("GARAGE_ADMIN_ENDPOINT")
	adminToken := os.Getenv("GARAGE_ADMIN_TOKEN")
	if s3Endpoint == "" || adminEndpoint == "" || adminToken == "" {
		t.Fatalf("GARAGE_S3_ENDPOINT, GARAGE_ADMIN_ENDPOINT, and GARAGE_ADMIN_TOKEN are required; run `just garage`, then source deploy/garage/dev-creds.env")
	}
	region := os.Getenv("GARAGE_REGION")
	if region == "" {
		region = "garage"
	}

	root := t.TempDir()
	tele := newSpikeTelemetry()
	spawnID := "fork-spike-" + newID()

	admin, err := journal.NewGarageAdmin(adminEndpoint, adminToken, &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("NewGarageAdmin: %v", err)
	}
	keyMgr, err := journal.NewGenerationKeyManager(journal.GenerationKeyConfig{
		Admin:        admin,
		S3Endpoint:   s3Endpoint,
		Region:       region,
		DisableTLS:   true,
		BucketPrefix: "spawnery-spawn-",
	})
	if err != nil {
		t.Fatalf("NewGenerationKeyManager: %v", err)
	}
	backend, err := keyMgr.BackendFor(ctx, spawnID, 1)
	if err != nil {
		t.Fatalf("generation BackendFor: %v", err)
	}
	keyfile := filepath.Join(root, "node.key")
	if err := journal.GenerateNodeKeyfile(keyfile); err != nil {
		t.Fatalf("GenerateNodeKeyfile: %v", err)
	}
	custody, err := journal.NewNodeLocalCustody(keyfile, filepath.Join(root, "seals"))
	if err != nil {
		t.Fatalf("NewNodeLocalCustody: %v", err)
	}
	jm, err := journal.NewManager(journal.Config{
		RepoRoot:  filepath.Join(root, "repos"),
		Backend:   backend,
		Custody:   custody,
		Telemetry: tele,
	})
	if err != nil {
		t.Fatalf("journal.NewManager: %v", err)
	}

	agentImage := "spawnery/stubagent:dev"
	model := "fork-spike-stub"
	openRouterKey := "unused"
	var selection AgentSelection
	if withLLM {
		openRouterKey = os.Getenv("OPENROUTER_API_KEY")
		if openRouterKey == "" {
			t.Fatal("OPENROUTER_API_KEY is required for the mid-turn Hermes spike")
		}
		agentImage = "spawnery/agent:dev"
		model = "openai/gpt-4.1-mini"
		selection = AgentSelection{RunnableID: "hermes-acp"}
	}

	mgr := NewManager(rt, ManagerConfig{
		AgentImage:    agentImage,
		SidecarImage:  "spawnery/sidecar:dev",
		OpenRouterKey: openRouterKey,
		DataRoot:      filepath.Join(root, "data"),
	})
	mgr.SetJournal(jm, filepath.Join(root, "journal-state"))

	appDir := writeForkSpikeApp(t, root)
	var sp *Spawn
	if withLLM {
		sp, err = mgr.CreateWithSelection(ctx, spawnID, appDir, model, "", "", 1, selection)
	} else {
		sp, err = mgr.Create(ctx, spawnID, appDir, model, "", "", 1)
	}
	if err != nil {
		t.Fatalf("create spike spawn: %v", err)
	}

	for _, w := range mgr.takeWatchers(sp) {
		w.Stop()
	}

	h := &forkFreezeSpikeHarness{
		t:       t,
		root:    root,
		mgr:     mgr,
		jm:      jm,
		tele:    tele,
		appDir:  appDir,
		model:   model,
		sel:     selection,
		useSel:  withLLM,
		spawnID: spawnID,
		spawn:   sp,
	}

	h.cleanups = append(h.cleanups, func() {
		cctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if _, live := h.mgr.Store().Get(h.spawnID); live {
			_ = h.mgr.Stop(cctx, h.spawnID)
		}
		_ = h.removeDeltaTag(cctx)
		_ = h.jm.Close(cctx, h.spawnID)
	})

	if withLLM {
		mux := http.NewServeMux()
		mux.Handle(spawnv1connect.NewSpawnServiceHandler(NewServer(mgr)))
		server := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
		server.Start()
		h.server = server
		h.cleanups = append(h.cleanups, func() { h.server.Close() })

		hc := &http.Client{
			Transport: &http2.Transport{
				AllowHTTP: true,
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				},
			},
		}
		h.client = spawnv1connect.NewSpawnServiceClient(hc, server.URL, connect.WithGRPC())
	}

	return h
}

func (h *forkFreezeSpikeHarness) cleanup() {
	for i := len(h.cleanups) - 1; i >= 0; i-- {
		h.cleanups[i]()
	}
}

func (h *forkFreezeSpikeHarness) waitForACP(ctx context.Context) {
	h.t.Helper()

	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		sp, ok := h.mgr.Store().Get(h.spawnID)
		if !ok {
			lastErr = fmt.Errorf("spawn %s not live in store", h.spawnID)
		} else {
			att, err := h.mgr.Attach(ctx, sp)
			if err == nil {
				_ = att.Close()
				return
			}
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	h.t.Fatalf("ACP attach never became ready for %s: %v", h.spawnID, lastErr)
}

func (h *forkFreezeSpikeHarness) recreateSpawn(ctx context.Context) {
	h.t.Helper()

	if _, live := h.mgr.Store().Get(h.spawnID); live {
		if err := h.mgr.Stop(ctx, h.spawnID); err != nil {
			h.t.Fatalf("stop before recreate: %v", err)
		}
	}

	var (
		sp  *Spawn
		err error
	)
	if h.useSel {
		sp, err = h.mgr.CreateWithSelection(ctx, h.spawnID, h.appDir, h.model, "", "", 1, h.sel)
	} else {
		sp, err = h.mgr.Create(ctx, h.spawnID, h.appDir, h.model, "", "", 1)
	}
	if err != nil {
		h.t.Fatalf("recreate spike spawn: %v", err)
	}
	for _, w := range h.mgr.takeWatchers(sp) {
		w.Stop()
	}
	h.spawn = sp
}

func (h *forkFreezeSpikeHarness) measureFreeze(ctx context.Context, samples int) forkFreezeResult {
	h.t.Helper()

	hostDir := h.mustJournalHostDir()
	warmSamples := make([]time.Duration, 0, samples)
	underPauseSamples := make([]time.Duration, 0, samples)
	commitSamples := make([]time.Duration, 0, samples)
	totalSamples := make([]time.Duration, 0, samples)
	captureStoppedSource := false
	datasetFiles := 0
	datasetBytes := int64(0)

	for sample := 1; sample <= samples; sample++ {
		if sample == 1 {
			datasetFiles, datasetBytes = populateRepresentativeTree(h.t, hostDir)
		} else {
			rewriteModules(h.t, hostDir, 0, 49, sample)
		}

		warm := h.requestWarmSnapshot(ctx)
		if err := seedRootfsSessionChurn(ctx, h.spawn.AgentID); err != nil {
			h.t.Fatalf("seedRootfsSessionChurn sample %d: %v", sample, err)
		}
		rewriteModules(h.t, hostDir, 50, 79, sample)
		writeSizedFile(h.t, filepath.Join(hostDir, "node_modules", "pkg-119", fmt.Sprintf("post-warm-delta-%02d.js", sample)), 0o644, 8192)

		underPause, finalScan, commitDur, totalFreeze, stoppedSource := h.pauseSnapshotCommitUnpause(ctx)
		captureStoppedSource = captureStoppedSource || stoppedSource
		h.t.Logf("sample %d freeze: warm=%s final_scan=%s under_pause=%s commit=%s total=%s", sample, warm, finalScan, underPause, commitDur, totalFreeze)

		warmSamples = append(warmSamples, warm)
		underPauseSamples = append(underPauseSamples, underPause)
		commitSamples = append(commitSamples, commitDur)
		totalSamples = append(totalSamples, totalFreeze)

		if err := h.removeDeltaTag(ctx); err != nil {
			h.t.Fatalf("remove delta tag after sample %d: %v", sample, err)
		}
		if stoppedSource && sample < samples {
			h.recreateSpawn(ctx)
		}
	}

	result := forkFreezeResult{
		Kind:                 "freeze",
		Samples:              samples,
		DatasetFiles:         datasetFiles,
		DatasetBytes:         datasetBytes,
		WarmSnapshot:         percentileDuration(warmSamples, 0.95),
		UnderPauseP50:        percentileDuration(underPauseSamples, 0.50),
		UnderPauseP95:        percentileDuration(underPauseSamples, 0.95),
		UnderPauseMax:        maxDuration(underPauseSamples),
		RootfsCommitP95:      percentileDuration(commitSamples, 0.95),
		TotalFreezeP95:       percentileDuration(totalSamples, 0.95),
		CaptureStoppedSource: captureStoppedSource,
	}
	finalizeDecision(&result)
	h.logJSON("freeze", result)
	return result
}

func (h *forkFreezeSpikeHarness) measureMidTurn(ctx context.Context) forkFreezeResult {
	h.t.Helper()

	if h.client == nil {
		h.t.Fatal("mid-turn harness requires an h2c SpawnService client")
	}
	h.waitForACP(ctx)

	hostDir := h.mustJournalHostDir()
	datasetFiles, datasetBytes := populateRepresentativeTree(h.t, hostDir)
	warm := h.requestWarmSnapshot(ctx)
	rewriteModules(h.t, hostDir, 50, 79, 1)
	writeSizedFile(h.t, filepath.Join(hostDir, "node_modules", "pkg-119", "post-warm-delta-mid-turn.js"), 0o644, 8192)
	if err := seedRootfsSessionChurn(ctx, h.spawn.AgentID); err != nil {
		h.t.Fatalf("seedRootfsSessionChurn mid-turn: %v", err)
	}

	firstChunk := make(chan struct{})
	var firstChunkOnce sync.Once
	type promptOutcome struct {
		text string
		err  error
	}
	promptDone := make(chan promptOutcome, 1)
	promptCtx, cancelPrompt := context.WithTimeout(ctx, 4*time.Minute)
	defer cancelPrompt()
	go func() {
		text, err := h.runPrompt(promptCtx, "Write 120 numbered lines. Each line must contain the phrase fork-spike-mid-turn and a unique number.", func(string) {
			firstChunkOnce.Do(func() { close(firstChunk) })
		})
		promptDone <- promptOutcome{text: text, err: err}
	}()

	select {
	case <-firstChunk:
	case outcome := <-promptDone:
		h.t.Fatalf("first prompt ended before a streamed chunk arrived; output=%q err=%v", outcome.text, outcome.err)
	case <-time.After(90 * time.Second):
		h.t.Fatal("timed out waiting for the first streamed chunk from the Hermes prompt")
	}

	underPause, finalScan, commitDur, totalFreeze, captureStoppedSource := h.pauseSnapshotCommitUnpause(ctx)
	h.t.Logf("mid-turn freeze: warm=%s final_scan=%s under_pause=%s commit=%s total=%s", warm, finalScan, underPause, commitDur, totalFreeze)
	if err := h.removeDeltaTag(ctx); err != nil {
		h.t.Fatalf("remove delta tag after mid-turn capture: %v", err)
	}

	turnOutcome := "timed_out_after_unpause"
	turnMarkerCount := 0
	currentTurnTruncated := false
	select {
	case outcome := <-promptDone:
		turnMarkerCount = strings.Count(outcome.text, "fork-spike-mid-turn")
		if outcome.err != nil {
			turnOutcome = "errored"
		} else {
			turnOutcome = "completed"
			currentTurnTruncated = turnMarkerCount < 120
		}
	case <-time.After(90 * time.Second):
		cancelPrompt()
		select {
		case <-promptDone:
		case <-time.After(5 * time.Second):
		}
	}

	recoveredText, recoveredErr := h.runPrompt(ctx, "Reply with exactly RECOVERED after the fork-spike pause.", nil)
	sourceRecovered := recoveredErr == nil && strings.Contains(recoveredText, "RECOVERED")

	result := forkFreezeResult{
		Kind:                 "mid-turn",
		Samples:              1,
		DatasetFiles:         datasetFiles,
		DatasetBytes:         datasetBytes,
		WarmSnapshot:         warm,
		UnderPauseP50:        underPause,
		UnderPauseP95:        underPause,
		UnderPauseMax:        underPause,
		RootfsCommitP95:      commitDur,
		TotalFreezeP95:       totalFreeze,
		CaptureStoppedSource: captureStoppedSource,
		TurnOutcome:          turnOutcome,
		TurnMarkerCount:      turnMarkerCount,
		CurrentTurnTruncated: currentTurnTruncated,
		SourceRecovered:      sourceRecovered,
	}
	finalizeDecision(&result)
	h.logJSON("mid-turn", result)
	return result
}

func (h *forkFreezeSpikeHarness) pauseSnapshotCommitUnpause(ctx context.Context) (time.Duration, time.Duration, time.Duration, time.Duration, bool) {
	h.t.Helper()

	h.tele.drain()
	startTotal := time.Now()
	startSnapshot := time.Now()
	result, err := h.mgr.SnapshotForSuspend(ctx, h.spawnID, nil)
	snapshotDur := time.Since(startSnapshot)
	if err != nil {
		h.t.Fatalf("SnapshotForSuspend: %v", err)
	}
	if result.MountMarkers["work"] == "" {
		h.t.Fatalf("SnapshotForSuspend returned empty work marker: %+v", result)
	}

	finalScan := waitForDuration(h.t, h.tele.final, 3*time.Minute, "final snapshot telemetry")
	handle := &runtime.PodHandle{
		SpawnID:      h.spawn.ID,
		AgentID:      h.spawn.AgentID,
		SidecarID:    h.spawn.SidecarID,
		BaseImageRef: h.spawn.LaunchImageRef,
	}

	paused := true
	defer func() {
		if !paused {
			return
		}
		if err := h.mgr.pod.Unpause(context.Background(), handle); err != nil && !strings.Contains(err.Error(), "is not paused") {
			h.t.Fatalf("Unpause after failed capture: %v", err)
		}
	}()

	startCommit := time.Now()
	ref, err := h.mgr.pod.CaptureDelta(ctx, handle)
	commitDur := time.Since(startCommit)
	if err != nil {
		h.t.Fatalf("CaptureDelta: %v", err)
	}
	if ref == "" {
		h.t.Fatal("CaptureDelta returned an empty image reference")
	}
	captureStoppedSource := false
	if err := h.mgr.pod.Unpause(ctx, handle); err != nil {
		if strings.Contains(err.Error(), "is not paused") {
			captureStoppedSource = true
		} else {
			h.t.Fatalf("Unpause: %v", err)
		}
	}
	paused = false

	total := time.Since(startTotal)
	return snapshotDur, finalScan, commitDur, total, captureStoppedSource
}

func (h *forkFreezeSpikeHarness) requestWarmSnapshot(ctx context.Context) time.Duration {
	h.t.Helper()

	h.tele.drain()
	h.jm.RequestSnapshot(ctx, h.spawnID, 1, h.mustJournalMount())
	return waitForDuration(h.t, h.tele.continuous, 3*time.Minute, "warm snapshot telemetry")
}

func (h *forkFreezeSpikeHarness) runPrompt(ctx context.Context, prompt string, onChunk func(string)) (string, error) {
	h.t.Helper()

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream := h.client.Session(streamCtx)
	pr, pw := io.Pipe()
	go func() {
		for {
			frame, err := stream.Receive()
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(frame.Data); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
	}()

	client := acp.NewClient(pr, &forkSpikeStreamWriter{stream: stream, spawnID: h.spawnID})
	if err := client.Initialize(); err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}
	if err := client.NewSession("/app"); err != nil {
		return "", fmt.Errorf("session/new: %w", err)
	}

	var out strings.Builder
	if err := client.Prompt(prompt, func(chunk string) {
		out.WriteString(chunk)
		if onChunk != nil {
			onChunk(chunk)
		}
	}); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

func (h *forkFreezeSpikeHarness) writeReports(result forkFreezeResult) {
	h.t.Helper()

	combined := loadExistingResult(h.t)
	mergeResult(&combined, result)
	finalizeDecision(&combined)

	js, err := json.MarshalIndent(combined, "", "  ")
	if err != nil {
		h.t.Fatalf("marshal report json: %v", err)
	}
	if err := os.WriteFile(forkSpikeJSONPath, js, 0o644); err != nil {
		h.t.Fatalf("write %s: %v", forkSpikeJSONPath, err)
	}

	dts5Note := fmt.Sprintf("### 2026-06-15 Spike E\n\nRepresentative dataset (deterministic actuals): `%d` files / `%d` bytes.\n\n```json\n%s\n```\n\nSpike E SLO: source freeze target is p95 <= 30s and max <= 45s for representative node_modules data; measured p95/max are recorded above. MVP may accept a current-turn error only when the next source turn recovers cleanly after unpause.\n\nGate decision: `turn_boundary_gate_required=%t` because %s.\n", combined.DatasetFiles, combined.DatasetBytes, string(js), combined.TurnBoundaryGateRequired, combined.DecisionReason)
	if err := os.WriteFile(forkSpikeDTS5NotePath, []byte(dts5Note), 0o644); err != nil {
		h.t.Fatalf("write %s: %v", forkSpikeDTS5NotePath, err)
	}

	specNote := fmt.Sprintf("- **2026-06-15 (Spike E):** Deterministic representative journal tree measured `%d` files / `%d` bytes (the plan's earlier count/byte placeholders were inconsistent with the listed files). Warm pre-snapshot p95 was `%s`; under-pause final snapshot p95/max were `%s` / `%s`; rootfs commit p95 was `%s`; total freeze p95 was `%s`. Mid-turn source behavior: current turn `%s` (marker count `%d`, truncated `%t`); follow-up prompt recovery `%t`. Gate decision: `turn_boundary_gate_required=%t` because %s.\n", combined.DatasetFiles, combined.DatasetBytes, durationString(combined.WarmSnapshot), durationString(combined.UnderPauseP95), durationString(combined.UnderPauseMax), durationString(combined.RootfsCommitP95), durationString(combined.TotalFreezeP95), turnOutcomeString(combined.TurnOutcome), combined.TurnMarkerCount, combined.CurrentTurnTruncated, combined.SourceRecovered, combined.TurnBoundaryGateRequired, combined.DecisionReason)
	if err := os.WriteFile(forkSpikeSpecNotePath, []byte(specNote), 0o644); err != nil {
		h.t.Fatalf("write %s: %v", forkSpikeSpecNotePath, err)
	}
}

func (h *forkFreezeSpikeHarness) mustJournalMount() journal.Mount {
	h.t.Helper()
	if len(h.spawn.JournalMounts) != 1 {
		h.t.Fatalf("expected exactly one journal mount, got %d", len(h.spawn.JournalMounts))
	}
	return h.spawn.JournalMounts[0]
}

func (h *forkFreezeSpikeHarness) mustJournalHostDir() string {
	h.t.Helper()
	return h.mustJournalMount().HostDir
}

func (h *forkFreezeSpikeHarness) removeDeltaTag(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "image", "rm", "-f", runtime.DeltaTag(h.spawnID))
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "No such image") {
			return nil
		}
		return fmt.Errorf("docker image rm -f %s: %w (%s)", runtime.DeltaTag(h.spawnID), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (h *forkFreezeSpikeHarness) logJSON(label string, result forkFreezeResult) {
	h.t.Helper()
	b, err := json.Marshal(result)
	if err != nil {
		h.t.Fatalf("marshal %s result: %v", label, err)
	}
	h.t.Log(string(b))
}

func populateRepresentativeTree(t *testing.T, root string) (files int, bytes int64) {
	t.Helper()

	for pkg := 0; pkg < 120; pkg++ {
		for file := 0; file < 160; file++ {
			rel := filepath.Join("node_modules", fmt.Sprintf("pkg-%03d", pkg), fmt.Sprintf("file-%03d.js", file))
			writeSizedFile(t, filepath.Join(root, rel), 0o644, 8192)
			files++
			bytes += 8192
		}
	}
	for tool := 0; tool < 80; tool++ {
		rel := filepath.Join("node_modules", ".bin", fmt.Sprintf("tool-%03d", tool))
		writeSizedFile(t, filepath.Join(root, rel), 0o755, 2048)
		files++
		bytes += 2048
	}
	for mod := 0; mod < 400; mod++ {
		rel := filepath.Join("src", fmt.Sprintf("module-%03d.ts", mod))
		writeSizedFile(t, filepath.Join(root, rel), 0o644, 4096)
		files++
		bytes += 4096
	}
	for blob := 0; blob < 80; blob++ {
		rel := filepath.Join("assets", fmt.Sprintf("blob-%03d.dat", blob))
		writeSizedFile(t, filepath.Join(root, rel), 0o644, 65536)
		files++
		bytes += 65536
	}
	writeSizedFile(t, filepath.Join(root, "package-lock.json"), 0o644, 524288)
	files++
	bytes += 524288

	if files != repTreeFiles || bytes != repTreeBytes {
		t.Fatalf("representative tree math = %d files / %d bytes, want %d files / %d bytes", files, bytes, repTreeFiles, repTreeBytes)
	}
	return files, bytes
}

func rewriteModules(t *testing.T, root string, start, end, sample int) {
	t.Helper()
	for idx := start; idx <= end; idx++ {
		path := filepath.Join(root, "src", fmt.Sprintf("module-%03d.ts", idx))
		writeSizedFileWithLabel(t, path, 0o644, 4096, fmt.Sprintf("sample-%02d-module-%03d", sample, idx))
	}
}

func writeForkSpikeApp(t *testing.T, root string) string {
	t.Helper()

	appDir := filepath.Join(root, "app")
	for _, rel := range []string{"seed", "work"} {
		if err := os.MkdirAll(filepath.Join(appDir, rel), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
	}
	manifest := `apiVersion: spawnery/v1
kind: App
id: spawnery/fork-spike
title: Fork Spike
agents: { support: [any], requiresAcp: [prompt] }
model: { recommendedDefault: openai/gpt-4.1-mini }
storage:
  mounts:
    - name: work
      path: work
      seed: seed
      durability: node-local
visibility: open
`
	if err := os.WriteFile(filepath.Join(appDir, "spawneryapp.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return appDir
}

func seedRootfsSessionChurn(ctx context.Context, agentID string) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", agentID, "sh", "-lc", `mkdir -p /root/.codex /root/.claude/projects/fork-spike && i=1; while [ "$i" -le 600 ]; do printf "{\"turn\":%s,\"payload\":\"fork-spike-session-line-%s\"}\n" "$i" "$i" >> /root/.codex/history.jsonl; printf "{\"turn\":%s,\"payload\":\"fork-spike-claude-line-%s\"}\n" "$i" "$i" >> /root/.claude/projects/fork-spike/session.jsonl; i=$((i+1)); done; sync`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker exec rootfs churn: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeSizedFile(t *testing.T, path string, mode os.FileMode, size int) {
	t.Helper()
	writeSizedFileWithLabel(t, path, mode, size, filepath.ToSlash(path))
}

func writeSizedFileWithLabel(t *testing.T, path string, mode os.FileMode, size int, label string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	data := deterministicBytes(label, size)
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func deterministicBytes(label string, size int) []byte {
	pattern := []byte(label + "\n")
	if len(pattern) == 0 {
		pattern = []byte("x")
	}
	out := make([]byte, size)
	for offset := 0; offset < size; offset += len(pattern) {
		copy(out[offset:], pattern)
	}
	return out
}

func waitForDuration(t *testing.T, ch chan time.Duration, timeout time.Duration, label string) time.Duration {
	t.Helper()
	select {
	case dur := <-ch:
		return dur
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", label)
		return 0
	}
}

func percentileDuration(ds []time.Duration, pct float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), ds...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(len(cp))*pct + 0.999999999)
	if idx <= 0 {
		idx = 1
	}
	if idx > len(cp) {
		idx = len(cp)
	}
	return cp[idx-1]
}

func maxDuration(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	max := ds[0]
	for _, d := range ds[1:] {
		if d > max {
			max = d
		}
	}
	return max
}

func loadExistingResult(t *testing.T) forkFreezeResult {
	t.Helper()
	var out forkFreezeResult
	b, err := os.ReadFile(forkSpikeJSONPath)
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		t.Fatalf("read %s: %v", forkSpikeJSONPath, err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", forkSpikeJSONPath, err)
	}
	return out
}

func mergeResult(dst *forkFreezeResult, src forkFreezeResult) {
	switch {
	case dst.Kind == "":
		dst.Kind = src.Kind
	case src.Kind == "":
	case dst.Kind != src.Kind:
		dst.Kind = "combined"
	}
	if src.Samples > 0 {
		dst.Samples = src.Samples
	}
	if src.DatasetFiles > 0 {
		dst.DatasetFiles = src.DatasetFiles
	}
	if src.DatasetBytes > 0 {
		dst.DatasetBytes = src.DatasetBytes
	}
	if src.WarmSnapshot > 0 {
		dst.WarmSnapshot = src.WarmSnapshot
	}
	if src.UnderPauseP50 > 0 {
		dst.UnderPauseP50 = src.UnderPauseP50
	}
	if src.UnderPauseP95 > 0 {
		dst.UnderPauseP95 = src.UnderPauseP95
	}
	if src.UnderPauseMax > 0 {
		dst.UnderPauseMax = src.UnderPauseMax
	}
	if src.RootfsCommitP95 > 0 {
		dst.RootfsCommitP95 = src.RootfsCommitP95
	}
	if src.TotalFreezeP95 > 0 {
		dst.TotalFreezeP95 = src.TotalFreezeP95
	}
	if src.CaptureStoppedSource {
		dst.CaptureStoppedSource = true
	}
	if src.TurnOutcome != "" || src.Kind == "mid-turn" {
		dst.TurnOutcome = src.TurnOutcome
		dst.TurnMarkerCount = src.TurnMarkerCount
		dst.CurrentTurnTruncated = src.CurrentTurnTruncated
		dst.SourceRecovered = src.SourceRecovered
	}
}

func finalizeDecision(result *forkFreezeResult) {
	reasons := make([]string, 0, 4)
	gate := false

	if result.TotalFreezeP95 == 0 || result.UnderPauseMax == 0 {
		gate = true
		reasons = append(reasons, "freeze measurements incomplete")
	} else {
		if result.TotalFreezeP95 > 30*time.Second {
			gate = true
			reasons = append(reasons, fmt.Sprintf("total_freeze_p95=%s exceeds 30s", result.TotalFreezeP95))
		}
		if result.UnderPauseMax > 45*time.Second {
			gate = true
			reasons = append(reasons, fmt.Sprintf("under_pause_max=%s exceeds 45s", result.UnderPauseMax))
		}
	}

	if result.TurnOutcome == "" {
		gate = true
		reasons = append(reasons, "mid-turn source behavior incomplete")
	} else {
		if result.CaptureStoppedSource {
			gate = true
			reasons = append(reasons, "current Docker CaptureDelta stops the source container before any unpause can preserve it")
		}
		if result.TurnOutcome == "timed_out_after_unpause" {
			gate = true
			reasons = append(reasons, "current turn timed out after unpause")
		}
		if result.CurrentTurnTruncated {
			gate = true
			reasons = append(reasons, fmt.Sprintf("current turn returned success with only %d fork-spike markers", result.TurnMarkerCount))
		}
		if !result.SourceRecovered {
			gate = true
			reasons = append(reasons, "follow-up source prompt did not recover cleanly")
		}
	}

	if !gate {
		reasons = append(reasons, "freeze p95/max stayed within target and the follow-up source turn recovered cleanly")
	}
	result.TurnBoundaryGateRequired = gate
	result.DecisionReason = strings.Join(reasons, "; ")
}

func durationString(d time.Duration) string {
	if d == 0 {
		return "n/a"
	}
	return d.String()
}

func turnOutcomeString(outcome string) string {
	if outcome == "" {
		return "not measured"
	}
	return outcome
}

type forkSpikeStreamWriter struct {
	stream  *connect.BidiStreamForClient[spawnv1.Frame, spawnv1.Frame]
	spawnID string
}

func (w *forkSpikeStreamWriter) Write(b []byte) (int, error) {
	if err := w.stream.Send(&spawnv1.Frame{SpawnId: w.spawnID, Data: b}); err != nil {
		return 0, err
	}
	return len(b), nil
}
