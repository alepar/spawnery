package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/urfave/cli/v3"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/internal/secrets/journalkey"
	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
)

// `spawnctl move <spawn-id> <target>` drives the data-only local<->cloud migration (sp-u53.5.3). It
// orchestrates the owner-side leg of the journal-key travel that the CP cannot do (the CP holds no
// key): fetch the owner-sealed ciphertext, drive MigrateSpawn (suspend source -> resume on target),
// then unseal locally + reseal to the target node's sub-key + deliver, so the journaled mounts restore
// on the target. <target> is a node id, or the literal "cloud" for the cloud class.

// targetCloud is the reserved <target> token that selects the cloud node class (vs a node id).
const targetCloud = "cloud"

// genDeliveryID mints the one-time delivery nonce bound into the in-flight AAD. A package var so tests
// can pin it; production uses a fresh UUID per delivery (replay defence, owner-sealed-secrets §3).
var genDeliveryID = func() string { return uuid.NewString() }

// moveClient is the subset of the cp.v1 client `spawnctl move` drives — narrowed to an interface so
// the orchestration is unit-testable with a fake.
type moveClient interface {
	GetJournalKeyCiphertext(context.Context, *connect.Request[cpv1.GetJournalKeyCiphertextRequest]) (*connect.Response[cpv1.GetJournalKeyCiphertextResponse], error)
	MigrateSpawn(context.Context, *connect.Request[cpv1.MigrateSpawnRequest]) (*connect.Response[cpv1.MigrateSpawnResponse], error)
	GetSpawnNodeKey(context.Context, *connect.Request[cpv1.GetSpawnNodeKeyRequest]) (*connect.Response[cpv1.GetSpawnNodeKeyResponse], error)
	DeliverSecrets(context.Context, *connect.Request[cpv1.DeliverSecretsRequest]) (*connect.Response[cpv1.DeliverSecretsResponse], error)
}

var _ moveClient = (cpv1connect.SpawnServiceClient)(nil)

// migrateTarget maps a <target> token onto a MigrateSpawnRequest's node/class fields.
func migrateTarget(spawnID, target string) *cpv1.MigrateSpawnRequest {
	if target == targetCloud {
		return &cpv1.MigrateSpawnRequest{SpawnId: spawnID, TargetClass: targetCloud}
	}
	return &cpv1.MigrateSpawnRequest{SpawnId: spawnID, TargetNodeId: target}
}

// runMove is the testable orchestration of `spawnctl move`. ic is the A4 intent client used to
// sign the migration intent (nil skips the intent flow for legacy/test CPs). dev is the local
// owner device (its private X25519 half opens the owner-sealed envelopes). On any step failure
// it returns a clear, data-safe message: the CP leaves the spawn in a defined state (resumed on
// the source's data, back to suspended on a failed target), so the user's data is never lost.
func runMove(ctx context.Context, client moveClient, ic intentClient, dev *seal.Device, spawnID, target string, out io.Writer, now time.Time) error {
	fmt.Fprintf(out, "move %s -> %s\n", spawnID, target)

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

	// 2) Drive the migration: suspend on the source, resume with a placement override on the target.
	// A4 two-phase sign-after-resolve [AC1][AM12]: launch pollAndSign concurrently so it can submit
	// the signed intent while MigrateSpawn blocks at the CP waiting for it.
	fmt.Fprintln(out, "  migrating (suspend source -> resume on target)...")
	var mr *connect.Response[cpv1.MigrateSpawnResponse]
	if ic != nil {
		// A4 two-phase sign-after-resolve [AC1][AM12]: MigrateSpawn is a blocking RPC so we
		// can use provisionWithIntent for retry-once on retryable NACK codes (e.g. STALE).
		// For an explicit node target, validate the CP's resolved target_node_id [AM1].
		// For "cloud", the CP selects the node — leave TargetNodeID empty (no validation).
		var migrateTargetNodeID string
		if target != targetCloud {
			migrateTargetNodeID = target
		}
		migrateErr := provisionWithIntent(ctx, ic, spawnID, intentParams{TargetNodeID: migrateTargetNodeID}, func(rpcCtx context.Context) error {
			var rpcErr error
			mr, rpcErr = client.MigrateSpawn(rpcCtx, connect.NewRequest(migrateTarget(spawnID, target)))
			return rpcErr
		})
		if migrateErr != nil {
			return fmt.Errorf("migrate: %w (your data is safe — resume on the source)", migrateErr)
		}
	} else {
		var migrateErr error
		mr, migrateErr = client.MigrateSpawn(ctx, connect.NewRequest(migrateTarget(spawnID, target)))
		if migrateErr != nil {
			return fmt.Errorf("migrate: %w (your data is safe — resume on the source)", migrateErr)
		}
	}
	fmt.Fprintf(out, "  resumed on node %s\n", mr.Msg.NodeId)
	if len(entries) == 0 {
		fmt.Fprintln(out, "  done.")
		return nil
	}

	// 3) Fetch the TARGET node's verified key material so we can reseal the journal key to it.
	nk, err := client.GetSpawnNodeKey(ctx, connect.NewRequest(&cpv1.GetSpawnNodeKeyRequest{SpawnId: spawnID}))
	if err != nil {
		return fmt.Errorf("fetch target node key: %w (migrated, but journal key not yet delivered — retry the move)", err)
	}
	var sk subkey.SignedSubKey
	if err := json.Unmarshal(nk.Msg.SignedSubkey, &sk); err != nil {
		return fmt.Errorf("parse target sub-key: %w", err)
	}

	// 4) For each mount: unseal the owner envelope locally + reseal to the target node's HPKE sub-key,
	//    binding the in-flight AAD (spawn, generation, node, sub-key expiry, one-time delivery id).
	fmt.Fprintf(out, "  resealing %d journal key(s) to node %s...\n", len(entries), sk.NodeID)
	secrets := make([]*cpv1.SealedSecret, 0, len(entries))
	for _, e := range entries {
		sealedJSON, rerr := resealJournalKey(e.Ciphertext, dev, sk, nk.Msg.NodeCertChain, spawnID, nk.Msg.Generation, now)
		if rerr != nil {
			return fmt.Errorf("reseal journal key for mount %q: %w", e.Mount, rerr)
		}
		secrets = append(secrets, &cpv1.SealedSecret{
			SecretId:   journalkey.SecretID(e.Mount),
			TargetPath: journalkey.SecretID(e.Mount),
			Sealed:     sealedJSON,
		})
	}

	// 5) Deliver the resealed ciphertext; the CP relays it to the target, which restores the journal.
	if _, err := client.DeliverSecrets(ctx, connect.NewRequest(&cpv1.DeliverSecretsRequest{SpawnId: spawnID, Secrets: secrets})); err != nil {
		return fmt.Errorf("deliver journal key: %w (migrated, but delivery failed — retry the move)", err)
	}
	fmt.Fprintln(out, "  journal key delivered — target is restoring the journaled mounts.")
	fmt.Fprintln(out, "  done.")
	return nil
}

// resealJournalKey unseals an owner-sealed envelope with the device key and re-seals the recovered
// journal password to the target node's HPKE sub-key under the in-flight AAD, returning the JSON
// seal.NodeSealed the CP relays. When the CP relayed a node cert chain (enforced/prod mode), the
// chain+sub-key are NOT yet PKI-verified here — full verification (pinned root + SAN/tenancy +
// revocation, via subkey.SealForNode) lands with the production delivery wiring; in dev/insecure mode
// the chain is empty and the relayed sub-key's HPKE pubkey is used directly.
func resealJournalKey(ciphertext []byte, dev *seal.Device, sk subkey.SignedSubKey, certChain []byte, spawnID string, generation uint64, now time.Time) ([]byte, error) {
	var env seal.Envelope
	if err := json.Unmarshal(ciphertext, &env); err != nil {
		return nil, fmt.Errorf("ciphertext is not a valid owner-sealed envelope: %w", err)
	}
	_ = certChain // production PKI verification hook (see doc comment)
	aad := seal.InFlightAAD{
		SpawnID:    spawnID,
		Generation: generation,
		NodeID:     sk.NodeID,
		NotAfter:   sk.NotAfter,
		Version:    generation,
		DeliveryID: genDeliveryID(),
	}
	sealed, err := journalkey.ResealForNode(&env, dev.X25519Priv, sk.HPKEPub, aad)
	if err != nil {
		return nil, err
	}
	_ = now
	return json.Marshal(sealed)
}

// moveCmd wires `spawnctl move <spawn-id> <target>` to the control plane.
func moveCmd() *cli.Command {
	return &cli.Command{
		Name:      "move",
		Usage:     "migrate a spawn to another node or the cloud (suspend here, resume there)",
		ArgsUsage: "<spawn-id> <target|cloud>",
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.StringFlag{Name: "cp", Value: "http://127.0.0.1:8080", Usage: "control-plane address"},
			&cli.StringFlag{Name: "token", Value: "dev-token", Usage: "dev auth token"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 2 {
				return cli.Exit("usage: spawnctl move <spawn-id> <target|cloud>", 2)
			}
			spawnID := c.Args().Get(0)
			target := strings.TrimSpace(c.Args().Get(1))
			if target == "" {
				return cli.Exit("a target node id (or \"cloud\") is required", 2)
			}
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			dev, err := loadDevice(dir)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			src := buildTokenSource(dir, c.String("token"), h2cClient())
			client := cpv1connect.NewSpawnServiceClient(h2cClient(), c.String("cp"),
				connect.WithGRPC(), connect.WithInterceptors(tokenSourceInterceptor(src)))
			// Pass client as both moveClient and intentClient — cpv1connect.SpawnServiceClient
			// satisfies both interfaces.
			if err := runMove(ctx, client, client, dev, spawnID, target, c.Writer, time.Now()); err != nil {
				return cli.Exit("move failed: "+err.Error(), 1)
			}
			return nil
		},
	}
}
