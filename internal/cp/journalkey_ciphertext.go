package cp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/journalkeys"
	"spawnery/internal/secrets/journalkey"
	"spawnery/internal/secrets/seal"
)

// Owner-sealed journal-key ciphertext custody, CP side (sp-u53.5.3; the RPCs sp-u53.5.4 deferred to
// this slice). The CP holds ONLY opaque owner-sealed Envelope bytes per (spawn, mount) — it never has a
// key to open them. GetJournalKeyCiphertext serves them to a verified owner client (which unseals +
// reseals to a target node on migration); PutJournalKeyCiphertext stores them on the node-local ->
// owner-sealed upgrade. Both are owner-only + ownership-checked.

// GetJournalKeyCiphertext returns the owner-sealed journal-key ciphertext for each of the spawn's
// mounts that has one stored. Owner-only. The owner client unseals these locally and re-seals to the
// target node's sub-key during MigrateSpawn.
func (s *Server) GetJournalKeyCiphertext(ctx context.Context, req *connect.Request[cpv1.GetJournalKeyCiphertextRequest]) (*connect.Response[cpv1.GetJournalKeyCiphertextResponse], error) {
	if err := s.ownSpawn(ctx, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	mounts, err := s.st.Spawns().GetMounts(ctx, req.Msg.SpawnId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	var entries []*cpv1.JournalKeyCiphertext
	for _, m := range mounts {
		ct, gerr := s.journalKeys.Get(ctx, req.Msg.SpawnId, journalkey.SecretID(m.Name))
		if errors.Is(gerr, journalkeys.ErrNotFound) {
			continue // this mount isn't owner-sealed (node-local/ephemeral) — skip it
		}
		if gerr != nil {
			return nil, connect.NewError(connect.CodeInternal, gerr)
		}
		entries = append(entries, &cpv1.JournalKeyCiphertext{Mount: m.Name, Ciphertext: ct})
	}
	return connect.NewResponse(&cpv1.GetJournalKeyCiphertextResponse{Entries: entries}), nil
}

// PutJournalKeyCiphertext stores the owner-sealed journal-key ciphertext for one or more of the
// spawn's mounts (the node-local -> owner-sealed upgrade). Owner-only. As a fail-closed-if-known guard
// it verifies each uploaded Envelope is openable by the owner's CURRENTLY-enrolled device set (so the
// CP never custodies ciphertext the owner could not later open) — skipped only when the owner has no
// devices enrolled in the registry yet (lazy enrollment).
func (s *Server) PutJournalKeyCiphertext(ctx context.Context, req *connect.Request[cpv1.PutJournalKeyCiphertextRequest]) (*connect.Response[cpv1.PutJournalKeyCiphertextResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	if err := s.ownSpawn(ctx, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	if len(req.Msg.Entries) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("no ciphertext entries to store"))
	}
	devices, devErr := s.ownerDevices.Devices(ctx, owner)
	checkOpenable := devErr == nil && len(devices) > 0 // skip the guard for an un-enrolled owner
	for _, e := range req.Msg.Entries {
		if e.Mount == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("ciphertext entry has empty mount"))
		}
		if len(e.Ciphertext) == 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("ciphertext entry for mount %q is empty", e.Mount))
		}
		if checkOpenable {
			if err := assertOpenableByOwner(e.Ciphertext, devices); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("mount %q: %w", e.Mount, err))
			}
		}
		if perr := s.journalKeys.Put(ctx, req.Msg.SpawnId, journalkey.SecretID(e.Mount), e.Ciphertext); perr != nil {
			return nil, connect.NewError(connect.CodeInternal, perr)
		}
	}
	return connect.NewResponse(&cpv1.PutJournalKeyCiphertextResponse{}), nil
}

// assertOpenableByOwner checks that the owner-sealed Envelope has at least one recipient stanza sealed
// to a currently-enrolled owner device — the invariant that the owner can still open it. It inspects
// only the public recipient HINTS (the CP holds no key and never unseals); a mismatch means the
// ciphertext was sealed to none of the owner's live devices and would be unrecoverable.
func assertOpenableByOwner(ciphertext []byte, devices []seal.X25519PubKey) error {
	var env seal.Envelope
	if err := json.Unmarshal(ciphertext, &env); err != nil {
		return fmt.Errorf("ciphertext is not a valid owner-sealed envelope: %w", err)
	}
	for _, r := range env.Recipients {
		for _, d := range devices {
			if bytes.Equal(r.Recipient, d) {
				return nil
			}
		}
	}
	return fmt.Errorf("envelope is not sealed to any of your enrolled devices")
}
