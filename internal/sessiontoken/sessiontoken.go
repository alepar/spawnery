// Package sessiontoken is the AS-signed, offline-verifiable session token (sp-ova design §7a). The Auth
// Service signs it; nodes verify it against the AS's published Ed25519 public key — so a compromised CP
// (which holds no AS key) cannot forge session authority. Format: base64url(claimsJSON).base64url(sig).
package sessiontoken

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Claims authorize a client session to a spawn's agent at the node.
type Claims struct {
	SpawnID string
	Owner   string
	Node    string
	Exp     time.Time
}

type wire struct {
	SpawnID string `json:"spawn_id"`
	Owner   string `json:"owner"`
	Node    string `json:"node"`
	Exp     int64  `json:"exp"` // unix seconds
}

var b64 = base64.RawURLEncoding

// Sign produces a token signed by the AS session key.
func Sign(c Claims, priv ed25519.PrivateKey) (string, error) {
	payload, err := json.Marshal(wire{SpawnID: c.SpawnID, Owner: c.Owner, Node: c.Node, Exp: c.Exp.Unix()})
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(priv, payload)
	return b64.EncodeToString(payload) + "." + b64.EncodeToString(sig), nil
}

// Verify checks the signature against pub and that the token has not expired at now, returning the
// claims. It returns an error on any tampering, wrong key, or expiry.
func Verify(token string, pub ed25519.PublicKey, now time.Time) (Claims, error) {
	p, s, ok := strings.Cut(token, ".")
	if !ok {
		return Claims{}, errors.New("sessiontoken: malformed token")
	}
	payload, err := b64.DecodeString(p)
	if err != nil {
		return Claims{}, fmt.Errorf("sessiontoken: bad payload: %w", err)
	}
	sig, err := b64.DecodeString(s)
	if err != nil {
		return Claims{}, fmt.Errorf("sessiontoken: bad signature: %w", err)
	}
	if !ed25519.Verify(pub, payload, sig) {
		return Claims{}, errors.New("sessiontoken: signature does not verify")
	}
	var w wire
	if err := json.Unmarshal(payload, &w); err != nil {
		return Claims{}, fmt.Errorf("sessiontoken: bad claims: %w", err)
	}
	c := Claims{SpawnID: w.SpawnID, Owner: w.Owner, Node: w.Node, Exp: time.Unix(w.Exp, 0)}
	if now.After(c.Exp) {
		return Claims{}, errors.New("sessiontoken: expired")
	}
	return c, nil
}
