package subkey

import (
	"fmt"
	"time"

	"spawnery/internal/secrets/seal"
)

// SealTransferKeyForNode verifies the CP-relayed target node key material
// against the caller's pinned root and seals a freshly generated transfer key
// to that verified node.
func SealTransferKeyForNode(transferKey []byte, leafPEM, chainPEM, rootPEM []byte, sk SignedSubKey, expect Expectation, revoked RevocationChecker, aad seal.InFlightAAD, now time.Time) (*seal.NodeSealed, seal.InFlightAAD, error) {
	if len(rootPEM) == 0 {
		return nil, seal.InFlightAAD{}, fmt.Errorf("subkey: pinned root PEM is required for fork transfer sealing")
	}
	hpkePub, id, err := VerifyNodeForSealing(leafPEM, chainPEM, rootPEM, sk, expect, revoked, now)
	if err != nil {
		return nil, seal.InFlightAAD{}, err
	}
	aad.NodeID = id.NodeID
	aad.NotAfter = sk.NotAfter
	sealed, err := seal.SealPlaintextToNode(transferKey, hpkePub, aad)
	if err != nil {
		return nil, seal.InFlightAAD{}, err
	}
	return sealed, aad, nil
}
