package client

// keytravel.go: Migrate + Fork + owner-sealed journal-key travel, extracted from
// cmd/spawnctl/{move,fork}.go. The orchestration cores (migrateSpawn/forkSpawn) take the narrow
// moveClient/forkClient interfaces as parameters (rather than being *Client methods) so they stay
// unit-testable with narrow fakes exactly as they were in spawnctl; Migrate/Fork are thin
// *Client wrappers passing c.rpc + c.warn.
//
// Progress lines (the key-travel step narration) are written to an injected io.Writer — nil
// defaults to io.Discard. This package references no os.Stdout/os package; the CLI passes
// os.Stdout. CLI-only header/footer prints ("move %s -> %s", "fork %s", "done.") stay in
// spawnctl (T3); this file keeps only the mid-flight step lines that already lived in the
// extracted helpers.

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/internal/clientverify"
	"spawnery/internal/intent"
	"spawnery/internal/pki"
	"spawnery/internal/secrets/journalkey"
	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
)

// targetCloud is the reserved <target> token that selects the cloud node class (vs a node id).
const targetCloud = "cloud"

// genDeliveryID mints the one-time delivery nonce bound into the in-flight AAD. A package var so
// tests can pin it; production uses a fresh UUID per delivery (replay defence, owner-sealed-secrets §3).
var genDeliveryID = func() string { return uuid.NewString() }

// ownerSealedDeliveryClient is the subset of the cp.v1 client used to fetch + deliver owner-sealed
// journal keys during a migrate or fork.
type ownerSealedDeliveryClient interface {
	GetJournalKeyCiphertext(context.Context, *connect.Request[cpv1.GetJournalKeyCiphertextRequest]) (*connect.Response[cpv1.GetJournalKeyCiphertextResponse], error)
	GetSpawnNodeKey(context.Context, *connect.Request[cpv1.GetSpawnNodeKeyRequest]) (*connect.Response[cpv1.GetSpawnNodeKeyResponse], error)
	DeliverSecrets(context.Context, *connect.Request[cpv1.DeliverSecretsRequest]) (*connect.Response[cpv1.DeliverSecretsResponse], error)
}

// moveClient is the subset of the cp.v1 client Migrate drives — narrowed to an interface so the
// orchestration is unit-testable with a fake.
type moveClient interface {
	ownerSealedDeliveryClient
	intentClient
	MigrateSpawn(context.Context, *connect.Request[cpv1.MigrateSpawnRequest]) (*connect.Response[cpv1.MigrateSpawnResponse], error)
}

var _ moveClient = (cpv1connect.SpawnServiceClient)(nil)

// forkClient is the subset of the cp.v1 client Fork drives.
type forkClient interface {
	ownerSealedDeliveryClient
	intentClient
	ForkSpawn(context.Context, *connect.Request[cpv1.ForkSpawnRequest]) (*connect.Response[cpv1.ForkSpawnResponse], error)
}

var _ forkClient = (cpv1connect.SpawnServiceClient)(nil)

// MoveOptions carries the caller-resolved (CLI-concern) inputs needed for owner-sealed key-travel
// node verification. Loading these from auth.json/env/flags is a CLI concern and stays in
// spawnctl; Migrate/Fork take a ready MoveOptions.
type MoveOptions struct {
	AccountID                   string
	TrustDomain                 string
	RootPEM                     []byte
	CertificateRevocations      pki.CertificateRevocationChecker
	CloseCertificateRevocations func() error
}

// migrateTarget maps a <target> token onto a MigrateSpawnRequest's node/class fields.
func migrateTarget(spawnID, target string) *cpv1.MigrateSpawnRequest {
	if target == targetCloud {
		return &cpv1.MigrateSpawnRequest{SpawnId: spawnID, TargetClass: targetCloud}
	}
	return &cpv1.MigrateSpawnRequest{SpawnId: spawnID, TargetNodeId: target}
}

// Migrate drives the data-only local<->cloud migration (sp-u53.5.3): fetch the owner-sealed
// journal-key ciphertext, drive MigrateSpawn (suspend source -> resume on target, signing the A4
// migration intent via provisionWithIntent), then unseal locally + reseal to the target node's
// sub-key + deliver, so the journaled mounts restore on the target. target is a node id, or the
// literal "cloud" for the cloud class. dev is the local owner device (its private X25519 half
// opens the owner-sealed envelopes). On any step failure it returns a clear, data-safe message:
// the CP leaves the spawn in a defined state (resumed on the source's data, back to suspended on
// a failed target), so the user's data is never lost.
func (c *Client) Migrate(ctx context.Context, dev *seal.Device, spawnID, target string, out io.Writer, now time.Time, opts MoveOptions) error {
	return migrateSpawnAuthorized(ctx, c.rpc, c.nodeCredentials, c.targetTrust, dev, spawnID, target, out, now, opts, c.warn, true)
}

func migrateSpawn(ctx context.Context, client moveClient, dev *seal.Device, spawnID, target string, out io.Writer, now time.Time, opts MoveOptions, warn func(error)) error {
	return migrateSpawnAuthorized(ctx, client, nil, TargetTrust{}, dev, spawnID, target, out, now, opts, warn, false)
}

func migrateSpawnAuthorized(ctx context.Context, client moveClient, credentials NodeCredentialSource, trust TargetTrust, dev *seal.Device, spawnID, target string, out io.Writer, now time.Time, opts MoveOptions, warn func(error), authorize bool) error {
	if authorize {
		prepared, err := prepareNodeAuthorization(ctx, credentials, trust)
		if err != nil {
			return err
		}
		credentials = prepared
	}
	if out == nil {
		out = io.Discard
	}

	// 1) Fetch the owner-sealed journal-key ciphertext for the spawn's mounts (CP holds ciphertext only).
	fmt.Fprintln(out, "  fetching owner-sealed journal-key ciphertext...")
	jk, err := client.GetJournalKeyCiphertext(ctx, connect.NewRequest(&cpv1.GetJournalKeyCiphertextRequest{SpawnId: spawnID}))
	if err != nil {
		return fmt.Errorf("fetch journal-key ciphertext: %w (no change — your data is safe)", err)
	}
	entries := jk.Msg.Entries
	if len(entries) == 0 {
		fmt.Fprintln(out, "  note: no owner-sealed journal keys for this spawn — its mounts will not restore on the target")
	}

	// 2) Drive the migration: suspend on the source, resume with a placement override on the
	// target. A4 two-phase sign-after-resolve [AC1][AM12]: provisionWithIntent runs pollAndSign
	// concurrently so it can submit the signed intent while MigrateSpawn blocks at the CP waiting
	// for it, and retries once on a retryable NACK (e.g. STALE). For an explicit node target,
	// validate the CP's resolved target_node_id [AM1]; for "cloud" the CP selects the node, so
	// leave TargetNodeID empty (no validation).
	fmt.Fprintln(out, "  migrating (suspend source -> resume on target)...")
	params := IntentParams{Op: intent.OpMigrateSpawn}
	if target != targetCloud {
		params.TargetNodeID = target
	} else {
		params.TargetClass = pki.ClassCloud
	}
	var mr *connect.Response[cpv1.MigrateSpawnResponse]
	doMigrate := func(rpcCtx context.Context) error {
		var rpcErr error
		mr, rpcErr = client.MigrateSpawn(rpcCtx, connect.NewRequest(migrateTarget(spawnID, target)))
		return rpcErr
	}
	var migrateErr error
	if authorize {
		migrateErr = provisionWithIntent(ctx, client, credentials, trust, spawnID, params, doMigrate, warn)
	} else {
		migrateErr = doMigrate(ctx)
	}
	if migrateErr != nil {
		return fmt.Errorf("migrate: %w (your data is safe — resume on the source)", migrateErr)
	}
	fmt.Fprintf(out, "  resumed on node %s\n", mr.Msg.NodeId)
	if len(entries) == 0 {
		return nil
	}

	if _, err := deliverOwnerSealedJournalKeys(ctx, client, dev, spawnID, mr.Msg.NodeId, target, out, now, opts); err != nil {
		return fmt.Errorf("%w (migrated, but journal key not yet delivered — retry the move)", err)
	}
	return nil
}

func deliverOwnerSealedJournalKeys(ctx context.Context, client ownerSealedDeliveryClient, dev *seal.Device, spawnID, expectedNodeID, target string, out io.Writer, now time.Time, opts MoveOptions) (int, error) {
	jk, err := client.GetJournalKeyCiphertext(ctx, connect.NewRequest(&cpv1.GetJournalKeyCiphertextRequest{SpawnId: spawnID}))
	if err != nil {
		return 0, fmt.Errorf("fetch journal-key ciphertext: %w", err)
	}
	entries := jk.Msg.Entries
	if len(entries) == 0 {
		return 0, nil
	}

	nk, err := client.GetSpawnNodeKey(ctx, connect.NewRequest(&cpv1.GetSpawnNodeKeyRequest{SpawnId: spawnID}))
	if err != nil {
		return 0, fmt.Errorf("fetch target node key: %w", err)
	}
	var sk subkey.SignedSubKey
	if err := json.Unmarshal(nk.Msg.SignedSubkey, &sk); err != nil {
		return 0, fmt.Errorf("parse target sub-key: %w", err)
	}
	if expectedNodeID == "" {
		expectedNodeID = sk.NodeID
	}
	var expect subkey.Expectation
	if len(nk.Msg.NodeCertChain) != 0 {
		if opts.CertificateRevocations == nil {
			return 0, errors.New("production node verification requires certificate revocation state")
		}
		expect, err = moveExpectation(target, opts.AccountID, opts.TrustDomain)
		if err != nil {
			return 0, err
		}
	}

	fmt.Fprintf(out, "  resealing %d journal key(s) to node %s...\n", len(entries), sk.NodeID)
	secrets := make([]*cpv1.SealedSecret, 0, len(entries))
	for _, e := range entries {
		version := nk.Msg.Generation
		deliveryID := genDeliveryID()
		sealedJSON, rerr := resealJournalKey(e.Ciphertext, dev, sk, nk.Msg.NodeCertChain, opts.RootPEM, expect, opts.CertificateRevocations, expectedNodeID, spawnID, nk.Msg.Generation, version, deliveryID, now)
		if rerr != nil {
			return 0, fmt.Errorf("reseal journal key for mount %q: %w", e.Mount, rerr)
		}
		secrets = append(secrets, &cpv1.SealedSecret{
			SecretId:   journalkey.SecretID(e.Mount),
			TargetPath: journalkey.SecretID(e.Mount),
			Sealed:     sealedJSON,
			Version:    version,
			DeliveryId: deliveryID,
		})
	}

	if _, err := client.DeliverSecrets(ctx, connect.NewRequest(&cpv1.DeliverSecretsRequest{SpawnId: spawnID, Secrets: secrets})); err != nil {
		return 0, fmt.Errorf("deliver journal key: %w", err)
	}
	fmt.Fprintln(out, "  journal key delivered — target is restoring the journaled mounts.")
	return len(secrets), nil
}

// resealJournalKey unseals an owner-sealed envelope with the device key and re-seals the recovered
// journal password to the target node's HPKE sub-key under the in-flight AAD, returning the JSON
// seal.NodeSealed the CP relays. When the CP relayed a node cert chain (enforced/prod mode), the
// chain+sub-key are PKI-verified, revocation-checked, and compared against the CP-resolved node id;
// in dev/insecure mode the chain is empty and the relayed sub-key's HPKE pubkey is used directly.
func resealJournalKey(ciphertext []byte, dev *seal.Device, sk subkey.SignedSubKey, certChain []byte, rootPEM []byte, expect subkey.Expectation, certificateRevocations pki.CertificateRevocationChecker, expectedNodeID string, spawnID string, generation uint64, version uint64, deliveryID string, now time.Time) ([]byte, error) {
	var env seal.Envelope
	if err := json.Unmarshal(ciphertext, &env); err != nil {
		return nil, fmt.Errorf("ciphertext is not a valid owner-sealed envelope: %w", err)
	}
	aad := seal.InFlightAAD{
		SpawnID:    spawnID,
		Generation: generation,
		Version:    version,
		DeliveryID: deliveryID,
	}
	if len(certChain) == 0 {
		if len(rootPEM) != 0 {
			return nil, errors.New("production node verification requires target node cert chain")
		}
		aad.NodeID = sk.NodeID
		aad.NotAfter = sk.NotAfter
		sealed, err := journalkey.ResealForNode(&env, dev.X25519Priv, sk.HPKEPub, aad)
		if err != nil {
			return nil, err
		}
		return json.Marshal(sealed)
	}
	if len(rootPEM) == 0 {
		return nil, errors.New("production node verification requires --root-ca")
	}
	leafPEM, chainPEM, err := splitLeafChainPEM(certChain)
	if err != nil {
		return nil, err
	}
	hpkePub, id, err := subkey.VerifyNodeForSealing(leafPEM, chainPEM, rootPEM, sk, expect, certificateRevocations, now)
	if err != nil {
		return nil, err
	}
	if id.NodeID != expectedNodeID {
		return nil, fmt.Errorf("verified node %q does not match resolved node %q", id.NodeID, expectedNodeID)
	}
	aad.NodeID = id.NodeID
	aad.NotAfter = sk.NotAfter
	sealed, err := seal.ReSealToNode(&env, dev.X25519Priv, hpkePub, aad)
	if err != nil {
		return nil, err
	}
	return json.Marshal(sealed)
}

func moveExpectation(target, accountID, trustDomain string) (subkey.Expectation, error) {
	if trustDomain == "" {
		return clientverify.Expectation{}, errors.New("production node verification requires a trust domain")
	}
	if target == targetCloud {
		return clientverify.Expectation{TrustDomain: trustDomain, Tenancy: "cloud"}, nil
	}
	if strings.TrimSpace(accountID) == "" {
		return clientverify.Expectation{}, errors.New("production self-hosted node verification requires a logged-in account")
	}
	return clientverify.Expectation{TrustDomain: trustDomain, Tenancy: "self-hosted", AccountID: accountID}, nil
}

func splitLeafChainPEM(certChain []byte) (leafPEM, chainPEM []byte, err error) {
	block, rest := pem.Decode(certChain)
	if block == nil {
		return nil, nil, errors.New("target node cert chain is not valid PEM")
	}
	return pem.EncodeToMemory(block), bytes.TrimSpace(rest), nil
}

// ForkResult carries the outcome of a Fork call.
type ForkResult struct {
	ForkSpawnID   string
	NodeID        string
	TransferSetID string
	Delivered     int
}

// Fork forks an active spawn to the same node, a node, or a node class (req.TargetNodeId /
// req.TargetClass / neither). ForkSpawn blocks at the CP awaiting the client's SignedIntent for
// the fork-spawn op (the CP registers the pending intent under the SOURCE spawn id), so Fork
// signs concurrently via the source-keyed pending intent — kicking it off after the await would
// deadlock. The signed child id must then match ForkSpawnResponse before delivery continues.
// Unlike Migrate, this does NOT use provisionWithIntent: its retry-once-on-NACK would issue a
// second ForkSpawn and create a duplicate fork. A single attempt is correct here; a STALE NACK
// just surfaces for the caller to retry manually.
func (c *Client) Fork(ctx context.Context, dev *seal.Device, req *cpv1.ForkSpawnRequest, out io.Writer, now time.Time, opts MoveOptions) (ForkResult, error) {
	return forkSpawnAuthorized(ctx, c.rpc, c.nodeCredentials, c.targetTrust, dev, req, out, now, opts)
}

func forkSpawn(ctx context.Context, client forkClient, dev *seal.Device, req *cpv1.ForkSpawnRequest, out io.Writer, now time.Time, opts MoveOptions, _ func(error)) (ForkResult, error) {
	resp, err := client.ForkSpawn(ctx, connect.NewRequest(req))
	if err != nil {
		return ForkResult{}, fmt.Errorf("fork: %w", err)
	}
	return finishFork(ctx, client, dev, req, resp, out, now, opts)
}

func forkSpawnAuthorized(ctx context.Context, client forkClient, credentials NodeCredentialSource, trust TargetTrust, dev *seal.Device, req *cpv1.ForkSpawnRequest, out io.Writer, now time.Time, opts MoveOptions) (ForkResult, error) {
	prepared, err := prepareNodeAuthorization(ctx, credentials, trust)
	if err != nil {
		return ForkResult{}, err
	}
	credentials = prepared
	if out == nil {
		out = io.Discard
	}
	// The CP registers the fork's pending intent under the source spawn id; poll/submit are keyed by it.
	sourceID := strings.TrimSpace(req.SpawnId)

	operationCtx, cancelOperation := context.WithCancel(ctx)
	defer cancelOperation()
	params := IntentParams{Op: intent.OpForkSpawn, TargetNodeID: req.GetTargetNodeId(), TargetClass: req.GetTargetClass()}
	type forkAuthorization struct {
		spawnID string
		err     error
	}
	signCh := make(chan forkAuthorization, 1)
	go func() {
		spawnID, err := pollAndSignFork(operationCtx, client, credentials, trust, sourceID, params)
		signCh <- forkAuthorization{spawnID: spawnID, err: err}
	}()
	type forkResponse struct {
		resp *connect.Response[cpv1.ForkSpawnResponse]
		err  error
	}
	rpcCh := make(chan forkResponse, 1)
	go func() {
		resp, err := client.ForkSpawn(operationCtx, connect.NewRequest(req))
		rpcCh <- forkResponse{resp: resp, err: err}
	}()
	var resp *connect.Response[cpv1.ForkSpawnResponse]
	var authorizedForkID string
	authDone, rpcDone := false, false
	var firstErr error
	ctxDone := ctx.Done()
	for !authDone || !rpcDone {
		select {
		case result := <-signCh:
			if result.err != nil && firstErr == nil {
				firstErr = fmt.Errorf("fork %s authorization: %w", sourceID, result.err)
				cancelOperation()
			}
			authorizedForkID = result.spawnID
			authDone = true
			signCh = nil
		case result := <-rpcCh:
			if result.err != nil && firstErr == nil {
				firstErr = fmt.Errorf("fork: %w", result.err)
				cancelOperation()
			}
			resp = result.resp
			rpcDone = true
			rpcCh = nil
		case <-ctxDone:
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			cancelOperation()
			ctxDone = nil
		}
	}
	cancelOperation()
	if firstErr != nil {
		return ForkResult{}, firstErr
	}
	if resp == nil || resp.Msg == nil {
		return ForkResult{}, errors.New("fork: empty response")
	}
	if resp.Msg.GetForkSpawnId() != authorizedForkID {
		return ForkResult{}, fmt.Errorf("fork: response fork_spawn_id %q does not match authorized %q", resp.Msg.GetForkSpawnId(), authorizedForkID)
	}
	return finishFork(ctx, client, dev, req, resp, out, now, opts)
}

func finishFork(ctx context.Context, client forkClient, dev *seal.Device, req *cpv1.ForkSpawnRequest, resp *connect.Response[cpv1.ForkSpawnResponse], out io.Writer, now time.Time, opts MoveOptions) (ForkResult, error) {
	if out == nil {
		out = io.Discard
	}
	forkID := resp.Msg.ForkSpawnId
	fmt.Fprintf(out, "  fork %s active on node %s\n", forkID, resp.Msg.NodeId)
	if resp.Msg.TransferSetId != "" {
		fmt.Fprintf(out, "  transfer set %s\n", resp.Msg.TransferSetId)
	}

	verifyTarget := req.TargetNodeId
	if verifyTarget == "" {
		verifyTarget = req.TargetClass
	}
	delivered, err := deliverOwnerSealedJournalKeys(ctx, client, dev, forkID, resp.Msg.NodeId, verifyTarget, out, now, opts)
	if err != nil {
		return ForkResult{}, fmt.Errorf("fork created as %s; delivery pending: %w", forkID, err)
	}
	return ForkResult{
		ForkSpawnID:   forkID,
		NodeID:        resp.Msg.NodeId,
		TransferSetID: resp.Msg.TransferSetId,
		Delivered:     delivered,
	}, nil
}
