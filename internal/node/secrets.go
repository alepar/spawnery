package node

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/githubcred"
	"spawnery/internal/secrets/journalkey"
	"spawnery/internal/secrets/seal"
)

// Owner-sealed secret delivery, node side (sp-2ckv.4, design §3/§6). The node holds the HPKE sub-key
// private halves (cfg.SubKeys, a subkey.Node); the CP relays owner-sealed ciphertext it cannot read.
// On SecretDelivery the node trial-Opens each ciphertext across its retained sub-keys (enforcing AAD
// equality + the notAfter clock check), then writes the plaintext into the spawn's tmpfs secrets dir
// at the declared path (0600). Plaintext exists only in this process's memory and that tmpfs.

type secretDeliveryReplayKey struct {
	spawnID    string
	generation uint64
	secretID   string
}

type secretDeliveryReplayState struct {
	highWater uint64
	seen      map[string]struct{}
	busy      bool
	pruned    bool
	cond      *sync.Cond
}

type secretDeliveryReplay struct {
	mu         sync.Mutex
	deliveries map[secretDeliveryReplayKey]*secretDeliveryReplayState
}

func newSecretDeliveryReplay() *secretDeliveryReplay {
	return &secretDeliveryReplay{deliveries: map[secretDeliveryReplayKey]*secretDeliveryReplayState{}}
}

func (r *secretDeliveryReplay) begin(spawnID string, generation uint64, secretID string, version uint64, deliveryID string) (func(), func(), error) {
	if version == 0 {
		return nil, nil, fmt.Errorf("missing delivery version")
	}
	if deliveryID == "" {
		return nil, nil, fmt.Errorf("missing delivery id")
	}

	key := secretDeliveryReplayKey{spawnID: spawnID, generation: generation, secretID: secretID}
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.deliveries[key]
	if st == nil {
		st = &secretDeliveryReplayState{seen: map[string]struct{}{}}
		st.cond = sync.NewCond(&r.mu)
		r.deliveries[key] = st
	}
	for st.busy && !st.pruned {
		st.cond.Wait()
	}
	if st.pruned {
		return nil, nil, fmt.Errorf("delivery generation pruned for spawn=%s generation=%d secret=%s", spawnID, generation, secretID)
	}
	if version < st.highWater {
		return nil, nil, fmt.Errorf("stale delivery version %d below accepted high-water %d", version, st.highWater)
	}
	if _, ok := st.seen[deliveryID]; ok {
		return nil, nil, fmt.Errorf("duplicate delivery id %q", deliveryID)
	}
	st.seen[deliveryID] = struct{}{}
	st.busy = true

	done := false
	commit := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if done {
			return
		}
		done = true
		if version > st.highWater {
			st.highWater = version
		}
		st.busy = false
		st.cond.Broadcast()
		if st.pruned {
			delete(r.deliveries, key)
		}
	}
	rollback := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if done {
			return
		}
		done = true
		delete(st.seen, deliveryID)
		st.busy = false
		st.cond.Broadcast()
		if st.pruned {
			delete(r.deliveries, key)
		}
	}
	return commit, rollback, nil
}

func (r *secretDeliveryReplay) checkBeforeConsume(spawnID string, generation uint64, secretID string, version uint64) error {
	key := secretDeliveryReplayKey{spawnID: spawnID, generation: generation, secretID: secretID}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.deliveries[key]
	if st == nil {
		return fmt.Errorf("delivery replay lease missing")
	}
	if st.pruned {
		return fmt.Errorf("delivery generation pruned for spawn=%s generation=%d secret=%s", spawnID, generation, secretID)
	}
	if version < st.highWater {
		return fmt.Errorf("stale delivery version %d below accepted high-water %d", version, st.highWater)
	}
	return nil
}

func (r *secretDeliveryReplay) pruneSpawnOlderThan(spawnID string, generation uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, st := range r.deliveries {
		if key.spawnID == spawnID && key.generation < generation {
			if st.busy {
				st.pruned = true
				st.cond.Broadcast()
				continue
			}
			delete(r.deliveries, key)
		}
	}
}

type startupSecretRoute struct {
	target   nodev1.ArtifactTarget
	destPath string
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func (a *attacher) openDeliveredSecret(spawnID string, generation uint64, sec *nodev1.SealedSecret, now time.Time) ([]byte, func(), func(), error) {
	if sec == nil {
		return nil, nil, nil, fmt.Errorf("nil sealed secret")
	}
	var sealed seal.NodeSealed
	if err := json.Unmarshal(sec.GetSealed(), &sealed); err != nil {
		return nil, nil, nil, fmt.Errorf("malformed sealed payload: %w", err)
	}
	base := seal.InFlightAAD{
		SpawnID:    spawnID,
		Generation: generation,
		Version:    sec.GetVersion(),
		DeliveryID: sec.GetDeliveryId(),
	}
	commit, rollback, err := a.secretReplay.begin(spawnID, generation, sec.GetSecretId(), sec.GetVersion(), sec.GetDeliveryId())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("replay rejected: %w", err)
	}
	a.subkeysMu.Lock()
	pt, err := a.cfg.SubKeys.OpenDelivered(&sealed, base, now)
	a.subkeysMu.Unlock()
	if err != nil {
		rollback()
		return nil, nil, nil, fmt.Errorf("unseal failed: %w", err)
	}
	if err := a.secretReplay.checkBeforeConsume(spawnID, generation, sec.GetSecretId(), sec.GetVersion()); err != nil {
		zeroBytes(pt)
		rollback()
		return nil, nil, nil, fmt.Errorf("replay rejected before consume: %w", err)
	}
	return pt, commit, rollback, nil
}

// publishSubKey returns the current SignedSubKey JSON to advertise on Register, rotating it first if it
// is past half-life (or absent). Returns nil when the node has no sub-key holder (insecure/dev mode) —
// then no sub-key is published and SecretDelivery is rejected. Concurrency-safe (subkeysMu).
func (a *attacher) publishSubKey(now time.Time) []byte {
	a.subkeysMu.Lock()
	defer a.subkeysMu.Unlock()
	return a.currentSubKeyBytesLocked(now)
}

// rotatedSubKey is publishSubKey for the heartbeat path: it rotates if needed but returns the bytes
// only when the published sub-key actually CHANGED since the last publish (so a steady-state heartbeat
// carries no sub-key). Concurrency-safe (subkeysMu).
func (a *attacher) rotatedSubKey(now time.Time) []byte {
	a.subkeysMu.Lock()
	defer a.subkeysMu.Unlock()
	b := a.currentSubKeyBytesLocked(now)
	if b == nil {
		return nil
	}
	id := a.lastSubKeyID
	cur, ok := a.cfg.SubKeys.Current(now)
	if !ok {
		return nil
	}
	if cur.KeyID() == id {
		return nil // unchanged since last publish — don't re-send
	}
	return b
}

// currentSubKeyBytesLocked rotates-at-half-life and returns the current SignedSubKey JSON, recording
// its KeyID as the last published one. Caller holds subkeysMu.
func (a *attacher) currentSubKeyBytesLocked(now time.Time) []byte {
	if a.cfg.SubKeys == nil {
		return nil
	}
	if a.cfg.SubKeys.NeedsRotation(now) {
		if _, err := a.cfg.SubKeys.Rotate(now); err != nil {
			log.Printf("subkey: rotate: %v", err)
			return nil
		}
	}
	cur, ok := a.cfg.SubKeys.Current(now)
	if !ok {
		return nil
	}
	b, err := json.Marshal(cur)
	if err != nil {
		log.Printf("subkey: marshal current: %v", err)
		return nil
	}
	a.lastSubKeyID = cur.KeyID()
	return b
}

// handleSecretDelivery unseals each owner-sealed secret in a SecretDelivery and writes the plaintext to
// the spawn's tmpfs secrets dir at its target path. Generation fencing is done by the caller (handle).
// Runs on its own goroutine. A node with no sub-key holder (insecure/dev) drops the delivery.
func (a *attacher) handleSecretDelivery(sd *nodev1.SecretDelivery) {
	if a.cfg.SubKeys == nil {
		log.Printf("secret-delivery for %s dropped: node has no HPKE sub-key holder", sd.SpawnId)
		return
	}
	if a.secretReplay == nil {
		log.Printf("secret-delivery for %s dropped: node has no replay guard", sd.SpawnId)
		return
	}
	a.secretReplay.pruneSpawnOlderThan(sd.SpawnId, sd.Generation)
	now := time.Now()
	for _, sec := range sd.Secrets {
		pt, commit, rollback, err := a.openDeliveredSecret(sd.SpawnId, sd.Generation, sec, now)
		if err != nil {
			log.Printf("secret-delivery %s/%s: %v", sd.SpawnId, sec.GetSecretId(), err)
			continue
		}

		// Journal-key deliveries (transient-tier §4, sp-u53.5.4) carry the per-spawn
		// Kopia repo password, NOT a tmpfs secret: route the plaintext into the
		// journaler's owner-sealed custody so a cross-node resume can open the repo
		// before journal.Restore. secret_id namespaces these (journalkey.Prefix).
		if journalkey.IsJournalKey(sec.SecretId) {
			derr := a.mgr.DeliverJournalKey(sd.SpawnId, sd.Generation, string(pt))
			zeroBytes(pt)
			if derr != nil {
				rollback()
				log.Printf("secret-delivery %s/%s: journal key inject failed: %v", sd.SpawnId, sec.SecretId, derr)
				continue
			}
			commit()
			log.Printf("secret-delivery %s: injected journal key %q (gen %d)", sd.SpawnId, sec.SecretId, sd.Generation)
			continue
		}

		if sec.GetType() == nodev1.SecretType_SECRET_TYPE_GITHUB_TOKEN {
			mounts, merr := a.nodeMountBindings(sd.SpawnId)
			if merr == nil {
				var handled bool
				handled, merr = a.consumeGitHubSecret(sd.SpawnId, sec, pt, mounts)
				if merr == nil && !handled {
					merr = fmt.Errorf("github secret %q has no supported usage", sec.GetSecretId())
				}
			}
			zeroBytes(pt)
			if merr != nil {
				rollback()
				log.Printf("secret-delivery %s/%s: github render failed: %v", sd.SpawnId, sec.SecretId, merr)
				continue
			}
			commit()
			log.Printf("secret-delivery %s: rendered github secret %q (gen %d)", sd.SpawnId, sec.SecretId, sd.Generation)
			a.noteGitHubRefresh(sd.SpawnId, sd.Generation, sec, mounts)
			continue
		}

		path, werr := a.mgr.InjectSecret(sd.SpawnId, sec.TargetPath, pt)
		// Zero the plaintext copy we hold once written (defense-in-depth, §6 — not a hard guarantee under
		// Go's GC, but cheap and removes the obvious lingering buffer).
		zeroBytes(pt)
		if werr != nil {
			rollback()
			log.Printf("secret-delivery %s/%s: write %q: %v", sd.SpawnId, sec.SecretId, sec.TargetPath, werr)
			continue
		}
		commit()
		log.Printf("secret-delivery %s: injected secret %q -> %s", sd.SpawnId, sec.SecretId, path)
	}
}

func (a *attacher) consumeStartupSecrets(ctx context.Context, spawnID string, generation uint64, secrets []*nodev1.SealedSecret, mounts []*nodev1.MountBinding, routes map[string]startupSecretRoute, inject func(string, []byte) (string, error), controlURL, controlToken string) error {
	if len(secrets) == 0 {
		return nil
	}
	seenSecretIDs := make(map[string]struct{}, len(secrets))
	for _, sec := range secrets {
		secretID := sec.GetSecretId()
		if _, ok := seenSecretIDs[secretID]; ok {
			return fmt.Errorf("startup secret delivery for %s: duplicate secret_id %q", spawnID, secretID)
		}
		seenSecretIDs[secretID] = struct{}{}
	}
	if a.cfg.SubKeys == nil {
		return fmt.Errorf("startup secret delivery for %s: node has no HPKE sub-key holder", spawnID)
	}
	if a.secretReplay == nil {
		return fmt.Errorf("startup secret delivery for %s: node has no replay guard", spawnID)
	}
	if inject == nil {
		return fmt.Errorf("startup secret delivery for %s: no secret injector", spawnID)
	}
	a.secretReplay.pruneSpawnOlderThan(spawnID, generation)
	now := time.Now()

	type openedSecret struct {
		sec      *nodev1.SealedSecret
		pt       []byte
		commit   func()
		rollback func()
	}
	opened := make([]openedSecret, 0, len(secrets))
	rollbackAll := func() {
		for i := range opened {
			zeroBytes(opened[i].pt)
			opened[i].pt = nil
		}
		for i := len(opened) - 1; i >= 0; i-- {
			opened[i].rollback()
		}
	}
	for _, sec := range secrets {
		pt, commit, rollback, err := a.openDeliveredSecret(spawnID, generation, sec, now)
		if err != nil {
			rollbackAll()
			return fmt.Errorf("startup secret %s/%s: %w", spawnID, sec.GetSecretId(), err)
		}
		opened = append(opened, openedSecret{sec: sec, pt: pt, commit: commit, rollback: rollback})
	}

	for i := range opened {
		consumeErr := a.consumeStartupSecret(ctx, spawnID, opened[i].sec, opened[i].pt, mounts, routes, inject, controlURL, controlToken)
		zeroBytes(opened[i].pt)
		opened[i].pt = nil
		if consumeErr != nil {
			rollbackAll()
			return fmt.Errorf("startup secret %s/%s: %w", spawnID, opened[i].sec.GetSecretId(), consumeErr)
		}
	}
	for _, opened := range opened {
		opened.commit()
	}
	return nil
}

func (a *attacher) consumeStartupGitHubSecrets(ctx context.Context, spawnID string, generation uint64, secrets []*nodev1.SealedSecret, mounts []*nodev1.MountBinding) (map[string]struct{}, error) {
	githubSecrets := make([]*nodev1.SealedSecret, 0)
	for _, sec := range secrets {
		if sec.GetType() == nodev1.SecretType_SECRET_TYPE_GITHUB_TOKEN {
			githubSecrets = append(githubSecrets, sec)
		}
	}
	if len(githubSecrets) == 0 {
		return nil, nil
	}
	consumed := make(map[string]struct{}, len(githubSecrets))
	inject := func(target string, plaintext []byte) (string, error) {
		return "", fmt.Errorf("github startup secret unexpectedly routed to generic target %q", target)
	}
	if err := a.consumeStartupSecrets(ctx, spawnID, generation, githubSecrets, mounts, nil, inject, "", ""); err != nil {
		_ = a.mgr.RemoveGitHubNodeCredentials(spawnID)
		a.mgr.CleanupSpawnTransient(spawnID)
		return nil, err
	}
	for _, sec := range githubSecrets {
		consumed[sec.GetSecretId()] = struct{}{}
		a.noteGitHubRefresh(spawnID, generation, sec, mounts)
	}
	return consumed, nil
}

func filterConsumedStartupSecrets(secrets []*nodev1.SealedSecret, consumed map[string]struct{}) []*nodev1.SealedSecret {
	if len(consumed) == 0 {
		return secrets
	}
	out := make([]*nodev1.SealedSecret, 0, len(secrets))
	for _, sec := range secrets {
		if _, ok := consumed[sec.GetSecretId()]; ok {
			continue
		}
		out = append(out, sec)
	}
	return out
}

func (a *attacher) consumeStartupSecret(ctx context.Context, spawnID string, sec *nodev1.SealedSecret, plaintext []byte, mounts []*nodev1.MountBinding, routes map[string]startupSecretRoute, inject func(string, []byte) (string, error), controlURL, controlToken string) error {
	if sec.GetType() == nodev1.SecretType_SECRET_TYPE_GITHUB_TOKEN {
		handled, err := a.consumeGitHubSecret(spawnID, sec, plaintext, mounts)
		if handled || err != nil {
			return err
		}
	}
	if route, ok := routes[sec.GetSecretId()]; ok {
		switch route.target {
		case nodev1.ArtifactTarget_ARTIFACT_TARGET_SIDECAR:
			return postSidecarCredentials(ctx, a.ctrlHTTP, controlURL, controlToken, plaintext, "")
		case nodev1.ArtifactTarget_ARTIFACT_TARGET_AGENT, nodev1.ArtifactTarget_ARTIFACT_TARGET_UNSPECIFIED:
			target := route.destPath
			if target == "" {
				target = sec.GetTargetPath()
			}
			if target == "" {
				return fmt.Errorf("agent route has no dest_path or target_path")
			}
			_, err := inject(target, plaintext)
			return err
		default:
			return fmt.Errorf("unsupported artifact target %s", route.target)
		}
	}
	if sec.GetTargetPath() == "" {
		return fmt.Errorf("no sensitive artifact route and no legacy target_path")
	}
	_, err := inject(sec.GetTargetPath(), plaintext)
	return err
}

func (a *attacher) consumeGitHubSecret(spawnID string, sec *nodev1.SealedSecret, plaintext []byte, mounts []*nodev1.MountBinding) (bool, error) {
	handled := false
	if hasSecretUsage(sec, nodev1.SecretUsage_SECRET_USAGE_NODE_STORAGE) {
		owner, repo, mountName, err := githubRepoMountForSecret(sec, mounts)
		if err != nil {
			return true, err
		}
		meta := sec.GetGithubToken()
		_, err = a.mgr.RenderGitHubNodeCredential(spawnID, mountName, githubcred.RenderRequest{
			Host:        meta.GetHost(),
			Owner:       owner,
			Repo:        repo,
			Login:       meta.GetLogin(),
			AccessToken: string(plaintext),
		})
		if err != nil {
			return true, err
		}
		handled = true
	}
	if hasSecretUsage(sec, nodev1.SecretUsage_SECRET_USAGE_AGENT_RENDER) {
		owner, repo, _, err := githubRepoMountForSecret(sec, mounts)
		if err != nil {
			return true, err
		}
		render := sec.GetRender()
		meta := sec.GetGithubToken()
		_, err = a.mgr.RenderGitHubAgentCredential(spawnID, githubcred.RenderRequest{
			Host:                 meta.GetHost(),
			Owner:                owner,
			Repo:                 repo,
			Login:                meta.GetLogin(),
			AccessToken:          string(plaintext),
			TargetDir:            render.GetTargetPath(),
			GHConfigDir:          render.GetGhConfigDir(),
			HostsPath:            render.GetHostsPath(),
			GitConfigPath:        render.GetGitConfigPath(),
			CredentialHelperPath: render.GetCredentialHelperPath(),
		})
		return true, err
	}
	if handled {
		return true, nil
	}
	return false, nil
}

// noteGitHubRefresh records the delivered GitHub link for proactive refresh scheduling (design §16.4).
// It extracts the link reference (secret_id/version/delivery_id) and audit repository_id from the
// matching mount; it never handles the token plaintext. nil-safe (refresher disabled in dev).
func (a *attacher) noteGitHubRefresh(spawnID string, generation uint64, sec *nodev1.SealedSecret, mounts []*nodev1.MountBinding) {
	if a.githubRefresh == nil || sec.GetSecretId() == "" {
		return
	}
	repositoryID := ""
	if _, _, mountName, err := githubRepoMountForSecret(sec, mounts); err == nil {
		for _, m := range mounts {
			if m.GetName() == mountName {
				repositoryID = m.GetRepositoryId()
				break
			}
		}
	}
	a.githubRefresh.Note(githubRefreshEntry{
		SpawnID:      spawnID,
		Generation:   generation,
		SecretID:     sec.GetSecretId(),
		Version:      sec.GetVersion(),
		DeliveryID:   sec.GetDeliveryId(),
		RepositoryID: repositoryID,
	})
}

func (a *attacher) nodeMountBindings(spawnID string) ([]*nodev1.MountBinding, error) {
	bindings, ok := a.mgr.MountBindings(spawnID)
	if !ok {
		return nil, fmt.Errorf("unknown spawn %s", spawnID)
	}
	out := make([]*nodev1.MountBinding, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, &nodev1.MountBinding{
			Name:               binding.Name,
			BackendUri:         binding.BackendURI,
			CredentialSecretId: binding.CredentialSecretID,
			CreateIfMissing:    binding.CreateIfMissing,
			RepositoryId:       binding.RepositoryID,
		})
	}
	return out, nil
}

func hasSecretUsage(sec *nodev1.SealedSecret, want nodev1.SecretUsage) bool {
	for _, usage := range sec.GetUsages() {
		if usage == want {
			return true
		}
	}
	return false
}

func githubRepoMountForSecret(sec *nodev1.SealedSecret, mounts []*nodev1.MountBinding) (string, string, string, error) {
	wanted := map[string]struct{}{}
	for _, name := range sec.GetMountNames() {
		wanted[name] = struct{}{}
	}
	for _, mount := range mounts {
		if len(wanted) > 0 {
			if _, ok := wanted[mount.GetName()]; !ok {
				continue
			}
		}
		owner, repo, ok := parseGitHubBackendURI(mount.GetBackendUri())
		if ok {
			if sec.GetSecretId() != "" && mount.GetCredentialSecretId() != "" && mount.GetCredentialSecretId() != sec.GetSecretId() {
				continue
			}
			return owner, repo, mount.GetName(), nil
		}
	}
	return "", "", "", fmt.Errorf("github secret %q has no bound github mount", sec.GetSecretId())
}

func parseGitHubBackendURI(uri string) (string, string, bool) {
	rest, ok := strings.CutPrefix(uri, "github:")
	if !ok {
		return "", "", false
	}
	rest = strings.TrimPrefix(rest, "//")
	rest = strings.Trim(rest, "/")
	owner, repo, ok := strings.Cut(rest, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	repo = strings.TrimSuffix(repo, ".git")
	if repo == "" {
		return "", "", false
	}
	return owner, repo, true
}
