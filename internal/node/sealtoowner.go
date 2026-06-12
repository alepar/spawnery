package node

import (
	"context"
	"log"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/secrets/seal"
)

// sealJournalKey handles a CP SealJournalKeyToOwnerRequest: reads the node-local
// repo password for each journaled mount of the spawn, seals it to the owner's
// device set, and replies with a SealJournalKeyToOwnerResponse on the Attach
// stream. Generation fencing is done by the caller (handle). Runs on its own
// goroutine (async like setModel/suspendSpawn) so a slow crypto op never stalls
// the single per-connection Receive loop.
func (a *attacher) sealJournalKey(ctx context.Context, req *nodev1.SealJournalKeyToOwnerRequest) {
	reply := func(entries []*nodev1.SealedJournalKey, errMsg string) {
		_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_SealJournalKeyResult{
			SealJournalKeyResult: &nodev1.SealJournalKeyToOwnerResponse{
				SpawnId:   req.SpawnId,
				RequestId: req.RequestId,
				Entries:   entries,
				Error:     errMsg,
			},
		}})
	}

	// Convert raw bytes to X25519PubKey slice.
	ownerDevices := make([]seal.X25519PubKey, len(req.OwnerDevicePubkeys))
	for i, pk := range req.OwnerDevicePubkeys {
		ownerDevices[i] = seal.X25519PubKey(pk)
	}

	sealed, err := a.mgr.SealJournalMounts(req.SpawnId, req.OwnerId, req.Generation, ownerDevices)
	if err != nil {
		log.Printf("node: sealJournalKey spawn=%s: %v", req.SpawnId, err)
		reply(nil, err.Error())
		return
	}

	entries := make([]*nodev1.SealedJournalKey, 0, len(sealed))
	for _, sm := range sealed {
		entries = append(entries, &nodev1.SealedJournalKey{
			Mount:      sm.Mount,
			Ciphertext: sm.Ciphertext,
		})
	}
	reply(entries, "")
}
