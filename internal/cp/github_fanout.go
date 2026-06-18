package cp

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/authsvc"
	"spawnery/internal/cp/store"
)

type githubLinkIndex struct {
	mu       sync.Mutex
	bySecret map[string]map[string]githubLinkRecord
}

type githubLinkRecord struct {
	secretTemplates []*cpv1.SealedSecret
}

func newGitHubLinkIndex() *githubLinkIndex {
	return &githubLinkIndex{bySecret: map[string]map[string]githubLinkRecord{}}
}

func (i *githubLinkIndex) noteNodeSecrets(spawnID string, secrets []*nodev1.SealedSecret) {
	bySecret := map[string][]*cpv1.SealedSecret{}
	for _, sec := range secrets {
		if sec.GetType() != nodev1.SecretType_SECRET_TYPE_GITHUB_TOKEN || sec.GetSecretId() == "" {
			continue
		}
		bySecret[sec.GetSecretId()] = append(bySecret[sec.GetSecretId()], nodeGitHubSecretTemplate(sec))
	}
	for secretID, templates := range bySecret {
		i.note(spawnID, secretID, templates...)
	}
}

func (i *githubLinkIndex) noteCPSecrets(spawnID string, secrets []*cpv1.SealedSecret) {
	bySecret := map[string][]*cpv1.SealedSecret{}
	for _, sec := range secrets {
		if sec.GetType() != cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN || sec.GetSecretId() == "" {
			continue
		}
		bySecret[sec.GetSecretId()] = append(bySecret[sec.GetSecretId()], cpGitHubSecretTemplate(sec))
	}
	for secretID, templates := range bySecret {
		i.note(spawnID, secretID, templates...)
	}
}

func (i *githubLinkIndex) note(spawnID, secretID string, templates ...*cpv1.SealedSecret) {
	i.mu.Lock()
	defer i.mu.Unlock()
	spawns := i.bySecret[secretID]
	if spawns == nil {
		spawns = map[string]githubLinkRecord{}
		i.bySecret[secretID] = spawns
	}
	rec := spawns[spawnID]
	if len(templates) > 0 {
		rec.secretTemplates = cloneCPSecrets(templates)
	}
	spawns[spawnID] = rec
}

func (i *githubLinkIndex) has(secretID, spawnID string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	_, ok := i.bySecret[secretID][spawnID]
	return ok
}

func (i *githubLinkIndex) spawns(secretID string) []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	spawns := i.bySecret[secretID]
	out := make([]string, 0, len(spawns))
	for spawnID := range spawns {
		out = append(out, spawnID)
	}
	return out
}

func (i *githubLinkIndex) templates(secretID, spawnID string) []*cpv1.SealedSecret {
	i.mu.Lock()
	defer i.mu.Unlock()
	return cloneCPSecrets(i.bySecret[secretID][spawnID].secretTemplates)
}

type GitHubLinkTarget struct {
	SpawnID         string
	NodeID          string
	Generation      uint64
	SignedSubkey    []byte
	NodeCertChain   []byte
	SecretTemplates []*cpv1.SealedSecret
}

type GitHubSealedAccessTokenDelivery struct {
	SpawnID    string
	Generation uint64
	Secrets    []*cpv1.SealedSecret
}

func (s *Server) authorizeGitHubMint(ctx context.Context, req authsvc.GitHubMintAuthorization) error {
	nodeID, generation, err := s.liveNode(ctx, req.SpawnID)
	if err != nil {
		return err
	}
	if nodeID != req.NodeID || generation != req.Generation {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("node does not host spawn generation"))
	}
	if !s.githubLinks.has(req.SecretID, req.SpawnID) {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("spawn is not indexed for github link"))
	}
	return nil
}

func (s *Server) githubLinkTargets(ctx context.Context, secretID string) ([]GitHubLinkTarget, error) {
	if secretID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("secret_id is required"))
	}
	spawnIDs := s.githubLinks.spawns(secretID)
	targets := make([]GitHubLinkTarget, 0, len(spawnIDs))
	for _, spawnID := range spawnIDs {
		nodeID, generation, err := s.liveNode(ctx, spawnID)
		if err != nil {
			if connect.CodeOf(err) == connect.CodeFailedPrecondition {
				continue
			}
			return nil, err
		}
		// A disconnected hosting node re-syncs the current token on reconnect (spec §16.4 step 4);
		// it must not abort the fanout for siblings or for the requesting node.
		if n, ok := s.reg.Get(nodeID); !ok || n.Sender == nil {
			log.Printf("githubLinkTargets %s: skipping disconnected hosting node %q for spawn %q (will re-sync on reconnect)", secretID, nodeID, spawnID)
			continue
		}
		entry, ok := s.nodeKeys.get(nodeID)
		if !ok || len(entry.subkey) == 0 {
			log.Printf("githubLinkTargets %s: skipping hosting node %q for spawn %q with no published github fanout subkey", secretID, nodeID, spawnID)
			continue
		}
		targets = append(targets, GitHubLinkTarget{
			SpawnID:         spawnID,
			NodeID:          nodeID,
			Generation:      generation,
			SignedSubkey:    append([]byte(nil), entry.subkey...),
			NodeCertChain:   append([]byte(nil), entry.certChain...),
			SecretTemplates: s.githubLinks.templates(secretID, spawnID),
		})
	}
	return targets, nil
}

func (s *Server) fanoutGitHubSealedAccessToken(ctx context.Context, secretID string, deliveries []GitHubSealedAccessTokenDelivery) error {
	if secretID == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("secret_id is required"))
	}
	bySpawn := make(map[string]GitHubSealedAccessTokenDelivery, len(deliveries))
	for _, d := range deliveries {
		bySpawn[d.SpawnID] = d
	}
	for _, spawnID := range s.githubLinks.spawns(secretID) {
		nodeID, generation, err := s.liveNode(ctx, spawnID)
		if err != nil {
			if connect.CodeOf(err) == connect.CodeFailedPrecondition {
				continue
			}
			return err
		}
		// Best-effort fanout: a per-spawn problem (disconnected node, missing/stale/empty
		// delivery, transient send failure) must not deny the requesting node its token.
		// Such spawns re-sync the current token on reconnect/resume (spec §16.4 step 4).
		n, ok := s.reg.Get(nodeID)
		if !ok || n.Sender == nil {
			log.Printf("fanoutGitHubSealedAccessToken %s: skipping disconnected hosting node %q for spawn %q (will re-sync on reconnect)", secretID, nodeID, spawnID)
			continue
		}
		d, ok := bySpawn[spawnID]
		if !ok {
			log.Printf("fanoutGitHubSealedAccessToken %s: no sealed delivery for spawn %q (hosting node skipped during sealing); will re-sync on reconnect", secretID, spawnID)
			continue
		}
		if d.Generation != generation {
			log.Printf("fanoutGitHubSealedAccessToken %s: stale delivery generation for spawn %q (have %d, live %d); skipping", secretID, spawnID, d.Generation, generation)
			continue
		}
		if len(d.Secrets) == 0 {
			log.Printf("fanoutGitHubSealedAccessToken %s: empty sealed delivery for spawn %q; skipping", secretID, spawnID)
			continue
		}
		if err := n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_SecretDelivery{SecretDelivery: &nodev1.SecretDelivery{
			SpawnId:    spawnID,
			Generation: generation,
			Secrets:    sealedSecretsToNode(d.Secrets),
		}}}); err != nil {
			log.Printf("fanoutGitHubSealedAccessToken %s: send to hosting node %q for spawn %q failed: %v; will re-sync on reconnect", secretID, nodeID, spawnID, err)
			continue
		}
	}
	return nil
}

// spawnHasGitHubMintMount reports whether any mount is a github mount carrying a CP-derived gh:
// mint link-ref (Approach 2 — minted at provision by the hosting node).
func spawnHasGitHubMintMount(mounts []store.Mount) bool {
	for _, m := range mounts {
		if strings.HasPrefix(m.BackendURI, "github:") && isGitHubMintLinkRef(m.CredentialSecretID) {
			return true
		}
	}
	return false
}

// seedGitHubMintLinks pre-seeds the in-memory github-link index for every gh: mint link-ref mount
// so authorizeGitHubMint admits the hosting node's at-provision JIT mint, which races INSIDE the
// blocking Provision/StartSpawn window (the node mints before acking ACTIVE). The entry needs no
// SealedSecret template — has(secretID, spawnID) is all authorizeGitHubMint consults. Proactive-refresh
// fanout (which needs templates) is out of scope for the live-dev demo (D3); a token never transits
// the CP.
func (s *Server) seedGitHubMintLinks(spawnID string, mounts []store.Mount) {
	for _, m := range mounts {
		if strings.HasPrefix(m.BackendURI, "github:") && isGitHubMintLinkRef(m.CredentialSecretID) {
			s.githubLinks.note(spawnID, strings.TrimSpace(m.CredentialSecretID))
		}
	}
}

// prepareGitHubMintProvision makes a github-mint spawn mintable at provision: it seeds the link
// index AND pre-binds the live container to the already-picked target node (Adopt), both BEFORE
// StartSpawn is sent. The pre-bind is required because authorizeGitHubMint also calls liveNode,
// but the gen-N container's node_id is "" until SetActive (which runs AFTER the blocking Provision).
// No-op when the spawn has no gh: mint mount. Called only from the intentEnabled provision branches
// (the dev-github lane — the only lane with a node->AS mint channel and a pre-Provision target node).
func (s *Server) prepareGitHubMintProvision(ctx context.Context, spawnID string, gen uint64, targetNodeID string, mounts []store.Mount) error {
	if !spawnHasGitHubMintMount(mounts) {
		return nil
	}
	s.seedGitHubMintLinks(spawnID, mounts)
	if err := s.st.Spawns().Adopt(ctx, spawnID, targetNodeID, int64(gen)); err != nil {
		return fmt.Errorf("pre-bind node %q for github mint: %w", targetNodeID, err)
	}
	return nil
}

func (s *Server) AuthorizeGitHubMint(ctx context.Context, req *connect.Request[cpv1.AuthorizeGitHubMintRequest]) (*connect.Response[cpv1.AuthorizeGitHubMintResponse], error) {
	msg := req.Msg
	if msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("request is required"))
	}
	if err := s.authorizeGitHubMint(ctx, authsvc.GitHubMintAuthorization{
		NodeID:       msg.GetNodeId(),
		SpawnID:      msg.GetSpawnId(),
		Generation:   msg.GetGeneration(),
		SecretID:     msg.GetSecretId(),
		Version:      msg.GetVersion(),
		DeliveryID:   msg.GetDeliveryId(),
		RepositoryID: msg.GetRepositoryId(),
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.AuthorizeGitHubMintResponse{}), nil
}

func (s *Server) GetGitHubLinkTargets(ctx context.Context, req *connect.Request[cpv1.GetGitHubLinkTargetsRequest]) (*connect.Response[cpv1.GetGitHubLinkTargetsResponse], error) {
	if req.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("request is required"))
	}
	targets, err := s.githubLinkTargets(ctx, req.Msg.GetSecretId())
	if err != nil {
		return nil, err
	}
	out := make([]*cpv1.GitHubLinkTarget, 0, len(targets))
	for _, target := range targets {
		out = append(out, &cpv1.GitHubLinkTarget{
			SpawnId:         target.SpawnID,
			NodeId:          target.NodeID,
			Generation:      target.Generation,
			SignedSubkey:    target.SignedSubkey,
			NodeCertChain:   target.NodeCertChain,
			SecretTemplates: cloneCPSecrets(target.SecretTemplates),
		})
	}
	return connect.NewResponse(&cpv1.GetGitHubLinkTargetsResponse{Targets: out}), nil
}

func nodeGitHubSecretTemplate(sec *nodev1.SealedSecret) *cpv1.SealedSecret {
	if sec == nil {
		return nil
	}
	usages := make([]cpv1.SecretUsage, len(sec.GetUsages()))
	for i, usage := range sec.GetUsages() {
		usages[i] = cpv1.SecretUsage(usage)
	}
	out := &cpv1.SealedSecret{
		TargetPath: sec.GetTargetPath(),
		SecretId:   sec.GetSecretId(),
		Type:       cpv1.SecretType(sec.GetType()),
		Version:    sec.GetVersion(),
		DeliveryId: sec.GetDeliveryId(),
		Usages:     usages,
		MountNames: append([]string(nil), sec.GetMountNames()...),
	}
	if sec.GetRender() != nil {
		out.Render = &cpv1.SecretRenderSpec{
			Profile:              sec.GetRender().GetProfile(),
			TargetPath:           sec.GetRender().GetTargetPath(),
			GhConfigDir:          sec.GetRender().GetGhConfigDir(),
			HostsPath:            sec.GetRender().GetHostsPath(),
			GitConfigPath:        sec.GetRender().GetGitConfigPath(),
			CredentialHelperPath: sec.GetRender().GetCredentialHelperPath(),
		}
	}
	if sec.GetGithubToken() != nil {
		out.GithubToken = &cpv1.GitHubTokenClearMetadata{
			Host:                 sec.GetGithubToken().GetHost(),
			Login:                sec.GetGithubToken().GetLogin(),
			GithubUserId:         sec.GetGithubToken().GetGithubUserId(),
			RefreshExpiresAtUnix: sec.GetGithubToken().GetRefreshExpiresAtUnix(),
			AppClientId:          sec.GetGithubToken().GetAppClientId(),
			AccessExpiresAtUnix:  sec.GetGithubToken().GetAccessExpiresAtUnix(),
		}
	}
	return out
}

func cpGitHubSecretTemplate(sec *cpv1.SealedSecret) *cpv1.SealedSecret {
	if sec == nil {
		return nil
	}
	out := &cpv1.SealedSecret{
		TargetPath: sec.GetTargetPath(),
		SecretId:   sec.GetSecretId(),
		Type:       sec.GetType(),
		Version:    sec.GetVersion(),
		DeliveryId: sec.GetDeliveryId(),
		Usages:     append([]cpv1.SecretUsage(nil), sec.GetUsages()...),
		MountNames: append([]string(nil), sec.GetMountNames()...),
	}
	if sec.GetRender() != nil {
		out.Render = &cpv1.SecretRenderSpec{
			Profile:              sec.GetRender().GetProfile(),
			TargetPath:           sec.GetRender().GetTargetPath(),
			GhConfigDir:          sec.GetRender().GetGhConfigDir(),
			HostsPath:            sec.GetRender().GetHostsPath(),
			GitConfigPath:        sec.GetRender().GetGitConfigPath(),
			CredentialHelperPath: sec.GetRender().GetCredentialHelperPath(),
		}
	}
	if sec.GetGithubToken() != nil {
		out.GithubToken = &cpv1.GitHubTokenClearMetadata{
			Host:                 sec.GetGithubToken().GetHost(),
			Login:                sec.GetGithubToken().GetLogin(),
			GithubUserId:         sec.GetGithubToken().GetGithubUserId(),
			RefreshExpiresAtUnix: sec.GetGithubToken().GetRefreshExpiresAtUnix(),
			AppClientId:          sec.GetGithubToken().GetAppClientId(),
			AccessExpiresAtUnix:  sec.GetGithubToken().GetAccessExpiresAtUnix(),
		}
	}
	return out
}

func cloneCPSecrets(in []*cpv1.SealedSecret) []*cpv1.SealedSecret {
	if len(in) == 0 {
		return nil
	}
	out := make([]*cpv1.SealedSecret, 0, len(in))
	for _, sec := range in {
		tmpl := cpGitHubSecretTemplate(sec)
		if tmpl != nil {
			out = append(out, tmpl)
		}
	}
	return out
}

func (s *Server) FanoutGitHubSealedAccessToken(ctx context.Context, req *connect.Request[cpv1.FanoutGitHubSealedAccessTokenRequest]) (*connect.Response[cpv1.FanoutGitHubSealedAccessTokenResponse], error) {
	msg := req.Msg
	if msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("request is required"))
	}
	deliveries := make([]GitHubSealedAccessTokenDelivery, 0, len(msg.GetDeliveries()))
	for _, delivery := range msg.GetDeliveries() {
		deliveries = append(deliveries, GitHubSealedAccessTokenDelivery{
			SpawnID:    delivery.GetSpawnId(),
			Generation: delivery.GetGeneration(),
			Secrets:    delivery.GetSecrets(),
		})
	}
	if err := s.fanoutGitHubSealedAccessToken(ctx, msg.GetSecretId(), deliveries); err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.FanoutGitHubSealedAccessTokenResponse{}), nil
}
