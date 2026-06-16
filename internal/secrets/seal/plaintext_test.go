package seal

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestSealPlaintextToNodeRoundTripAndAADMismatch(t *testing.T) {
	pub, priv, err := NodeKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	aad := InFlightAAD{
		SpawnID:    "sp-fork",
		Generation: 1,
		NodeID:     "node-2",
		NotAfter:   time.Now().Add(time.Hour),
		DeliveryID: "ts-1",
	}
	sealed, err := SealPlaintextToNode([]byte("transfer-key"), pub, aad)
	if err != nil {
		t.Fatalf("SealPlaintextToNode: %v", err)
	}
	opened, err := OpenFromOwner(sealed, priv, aad, time.Now())
	if err != nil {
		t.Fatalf("OpenFromOwner: %v", err)
	}
	if !bytes.Equal(opened, []byte("transfer-key")) {
		t.Fatalf("opened = %q", opened)
	}
	wrong := aad
	wrong.SpawnID = "sp-other"
	if _, err := OpenFromOwner(sealed, priv, wrong, time.Now()); !errors.Is(err, ErrAADMismatch) {
		t.Fatalf("wrong AAD err = %v, want ErrAADMismatch", err)
	}
}
