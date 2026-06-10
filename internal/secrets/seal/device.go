package seal

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"

	bip39 "github.com/tyler-smith/go-bip39"
)

// signCurve is the ECDSA signing curve for device-set authorization. P-256 is
// used (not Ed25519) because X25519 cannot sign and Ed25519-in-WebCrypto is not
// broadly reliable until ~2027 (§1, roast M4).
var signCurve = elliptic.P256()

// HKDF info labels deriving the two device sub-keys from one BIP-39 seed. The
// labels domain-separate the sealing key from the signing key so neither can be
// reconstructed from the other.
const (
	hkdfInfoX25519 = "spawnery/device/x25519/v1"
	hkdfInfoP256   = "spawnery/device/ecdsa-p256/v1"
)

// Device holds one device's two keypairs (§1): an X25519 keypair for HPKE
// sealing/unsealing and an ECDSA P-256 keypair for device-set authorization.
// Both derive deterministically from a single BIP-39 seed; the recovery-code
// "virtual device" is just a Device built from the recovery mnemonic.
type Device struct {
	X25519Priv []byte // raw 32-byte X25519 scalar
	X25519Pub  []byte // raw 32-byte X25519 public key
	Sign       *ecdsa.PrivateKey
}

// SignPub returns the device's ECDSA P-256 public key.
func (d *Device) SignPub() *ecdsa.PublicKey { return &d.Sign.PublicKey }

// X25519PubKey returns the HPKE recipient public key for this device.
func (d *Device) X25519PubKey() X25519PubKey { return append(X25519PubKey(nil), d.X25519Pub...) }

// Ref returns the public DeviceRef (the form registered in the device set).
func (d *Device) Ref() DeviceRef {
	return DeviceRef{
		X25519Pub: append([]byte(nil), d.X25519Pub...),
		SignPub:   marshalSignPub(d.SignPub()),
	}
}

// NewMnemonic returns a fresh 24-word (256-bit entropy) BIP-39 mnemonic, used
// for both device seeds and the recovery code.
func NewMnemonic() (string, error) {
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return "", fmt.Errorf("seal: new entropy: %w", err)
	}
	m, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return "", fmt.Errorf("seal: new mnemonic: %w", err)
	}
	return m, nil
}

// DeviceFromMnemonic deterministically derives a Device from a BIP-39 mnemonic
// (and optional passphrase). The same mnemonic always yields the same keypairs
// — the basis of recovery without server escrow.
func DeviceFromMnemonic(mnemonic, passphrase string) (*Device, error) {
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, errors.New("seal: invalid BIP-39 mnemonic")
	}
	seed := bip39.NewSeed(mnemonic, passphrase)
	return DeviceFromSeed(seed)
}

// RecoveryDevice derives the always-enrolled recovery "virtual device" from the
// BIP-39 recovery code (§1).
func RecoveryDevice(recoveryMnemonic string) (*Device, error) {
	return DeviceFromMnemonic(recoveryMnemonic, "")
}

// DeviceFromSeed deterministically derives both keypairs from a raw seed
// (typically the 64-byte BIP-39 seed).
func DeviceFromSeed(seed []byte) (*Device, error) {
	if len(seed) == 0 {
		return nil, errors.New("seal: empty seed")
	}

	// X25519: derive a 32-byte sub-seed, then the HPKE DeriveKeyPair.
	xSeed, err := hkdf.Expand(sha256.New, seed, hkdfInfoX25519, x25519KeySize)
	if err != nil {
		return nil, fmt.Errorf("seal: derive x25519 seed: %w", err)
	}
	pk, sk := kemScheme.DeriveKeyPair(xSeed)
	xPub, err := pk.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("seal: marshal x25519 pub: %w", err)
	}
	xPriv, err := sk.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("seal: marshal x25519 priv: %w", err)
	}

	// ECDSA P-256: derive the scalar deterministically. Expand to 48 bytes
	// (> 256 + 64 bits) then reduce into [1, N-1] to keep the modular bias
	// below 2^-64 (FIPS 186-4 B.4.1 style).
	signPriv, err := deriveP256(seed)
	if err != nil {
		return nil, err
	}

	return &Device{X25519Priv: xPriv, X25519Pub: xPub, Sign: signPriv}, nil
}

func deriveP256(seed []byte) (*ecdsa.PrivateKey, error) {
	buf, err := hkdf.Expand(sha256.New, seed, hkdfInfoP256, 48)
	if err != nil {
		return nil, fmt.Errorf("seal: derive p256 seed: %w", err)
	}
	n1 := new(big.Int).Sub(signCurve.Params().N, big.NewInt(1))
	d := new(big.Int).SetBytes(buf)
	d.Mod(d, n1)
	d.Add(d, big.NewInt(1)) // map into [1, N-1]

	priv := new(ecdsa.PrivateKey)
	priv.Curve = signCurve
	priv.D = d
	priv.PublicKey.Curve = signCurve
	priv.PublicKey.X, priv.PublicKey.Y = signCurve.ScalarBaseMult(d.Bytes())
	return priv, nil
}

// ---- keyfile format (CLI, 0600) ----

type keyfile struct {
	Version    int    `json:"version"`
	X25519Priv string `json:"x25519_priv"` // base64 raw 32 bytes
	X25519Pub  string `json:"x25519_pub"`  // base64 raw 32 bytes
	SignD      string `json:"sign_d"`      // base64 P-256 scalar
}

// MarshalKeyfile serializes the device's private key material to the 0600
// keyfile JSON format used by the CLI.
func (d *Device) MarshalKeyfile() ([]byte, error) {
	kf := keyfile{
		Version:    1,
		X25519Priv: base64.StdEncoding.EncodeToString(d.X25519Priv),
		X25519Pub:  base64.StdEncoding.EncodeToString(d.X25519Pub),
		SignD:      base64.StdEncoding.EncodeToString(d.Sign.D.Bytes()),
	}
	return json.Marshal(kf)
}

// ParseKeyfile reconstructs a Device from keyfile JSON, recomputing the public
// points from the stored private material.
func ParseKeyfile(b []byte) (*Device, error) {
	var kf keyfile
	if err := json.Unmarshal(b, &kf); err != nil {
		return nil, fmt.Errorf("seal: parse keyfile: %w", err)
	}
	if kf.Version != 1 {
		return nil, fmt.Errorf("seal: unsupported keyfile version %d", kf.Version)
	}
	xPriv, err := base64.StdEncoding.DecodeString(kf.X25519Priv)
	if err != nil {
		return nil, fmt.Errorf("seal: decode x25519 priv: %w", err)
	}
	xPub, err := base64.StdEncoding.DecodeString(kf.X25519Pub)
	if err != nil {
		return nil, fmt.Errorf("seal: decode x25519 pub: %w", err)
	}
	if len(xPriv) != x25519KeySize || len(xPub) != x25519KeySize {
		return nil, ErrBadKeySize
	}
	dRaw, err := base64.StdEncoding.DecodeString(kf.SignD)
	if err != nil {
		return nil, fmt.Errorf("seal: decode sign scalar: %w", err)
	}
	priv := new(ecdsa.PrivateKey)
	priv.Curve = signCurve
	priv.D = new(big.Int).SetBytes(dRaw)
	if priv.D.Sign() == 0 || priv.D.Cmp(signCurve.Params().N) >= 0 {
		return nil, errors.New("seal: signing scalar out of range")
	}
	priv.PublicKey.Curve = signCurve
	priv.PublicKey.X, priv.PublicKey.Y = signCurve.ScalarBaseMult(priv.D.Bytes())
	return &Device{X25519Priv: xPriv, X25519Pub: xPub, Sign: priv}, nil
}

// WriteKeyfile writes the device keyfile to path with 0600 permissions.
func (d *Device) WriteKeyfile(path string) error {
	b, err := d.MarshalKeyfile()
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// LoadKeyfile reads and parses a device keyfile from path.
func LoadKeyfile(path string) (*Device, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseKeyfile(b)
}

// ---- signing-key public encoding ----

// marshalSignPub encodes a P-256 public key in SEC1 uncompressed form (65
// bytes).
func marshalSignPub(pub *ecdsa.PublicKey) []byte {
	return elliptic.Marshal(pub.Curve, pub.X, pub.Y)
}

// parseSignPub decodes a SEC1 uncompressed P-256 public key.
func parseSignPub(b []byte) (*ecdsa.PublicKey, error) {
	x, y := elliptic.Unmarshal(signCurve, b)
	if x == nil {
		return nil, errors.New("seal: malformed signing pubkey")
	}
	return &ecdsa.PublicKey{Curve: signCurve, X: x, Y: y}, nil
}
