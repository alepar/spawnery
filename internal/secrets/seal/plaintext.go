package seal

import (
	"crypto/rand"
	"fmt"
)

// SealPlaintextToNode seals an arbitrary plaintext directly to a verified node
// HPKE public key under the in-flight AAD. The caller is responsible for
// verifying the node key before calling this helper.
func SealPlaintextToNode(payload []byte, nodeHPKEPub []byte, aad InFlightAAD) (*NodeSealed, error) {
	pk, err := parsePub(nodeHPKEPub)
	if err != nil {
		return nil, err
	}
	sender, err := suite.NewSender(pk, []byte(infoInFlight))
	if err != nil {
		return nil, fmt.Errorf("seal: new node sender: %w", err)
	}
	enc, sealer, err := sender.Setup(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("seal: node hpke setup: %w", err)
	}
	ct, err := sealer.Seal(payload, aad.bytes())
	if err != nil {
		return nil, fmt.Errorf("seal: node hpke seal: %w", err)
	}
	return &NodeSealed{Enc: enc, CT: ct}, nil
}
