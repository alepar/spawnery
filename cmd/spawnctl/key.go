package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"

	"spawnery/internal/secrets/seal"
)

// Local owner device-key lifecycle for spawnctl (sp-2ckv.2, CLI-first). These
// commands manage the owner's per-device keypairs and the hash-chained device
// set entirely on the local filesystem — no CP/network/proto. They wrap the
// pure-crypto primitives in internal/secrets/seal (device derivation, keyfile
// marshal, device-set genesis + verify). See
// docs/superpowers/specs/2026-06-10-owner-sealed-secrets-design.md §1 + §4.
//
// The device-set add/remove mutations, node delivery, and AS registration are a
// later wave (phase ② of the spec) and require CP relay; the commands here are
// deliberately scoped to local key custody only.

const (
	// keyfileName is this device's 0600 private keyfile under the config dir.
	keyfileName = "device.key"
	// deviceSetName is the local device-set log + pinned owner root.
	deviceSetName = "device-set.json"

	// recoveryLossWarning is the mandatory user-facing copy shown at the key
	// ceremony (spec §4): the owner alone holds the means of recovery.
	recoveryLossWarning = "WARNING: without this recovery code and your devices, suspended spawn " +
		"contents cannot be recovered by anyone, including Spawnery. Write it down and store it " +
		"somewhere safe and offline. It is shown ONCE and is never stored on this machine."
)

// deviceSetFile is the on-disk device-set: the append-only log plus the pinned
// owner root (device₁ + recovery signing pubkeys) the chain verifies against.
// Persisting the root separately makes VerifyDeviceSet a real pin rather than a
// self-referential check of the genesis it is validating.
type deviceSetFile struct {
	Root seal.OwnerRoot
	Log  *seal.DeviceSetLog
}

// ownerRootJSON is the JSON shadow of seal.OwnerRoot (whose exported []byte
// fields carry no JSON tags). deviceSetFile round-trips through it.
type ownerRootJSON struct {
	Device1SignPub  []byte `json:"device1_sign_pub"`
	RecoverySignPub []byte `json:"recovery_sign_pub"`
}

func (f deviceSetFile) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Root ownerRootJSON      `json:"root"`
		Log  *seal.DeviceSetLog `json:"log"`
	}{
		Root: ownerRootJSON{f.Root.Device1SignPub, f.Root.RecoverySignPub},
		Log:  f.Log,
	})
}

func (f *deviceSetFile) UnmarshalJSON(b []byte) error {
	var tmp struct {
		Root ownerRootJSON      `json:"root"`
		Log  *seal.DeviceSetLog `json:"log"`
	}
	if err := json.Unmarshal(b, &tmp); err != nil {
		return err
	}
	f.Root = seal.OwnerRoot{Device1SignPub: tmp.Root.Device1SignPub, RecoverySignPub: tmp.Root.RecoverySignPub}
	f.Log = tmp.Log
	return nil
}

// defaultConfigDir resolves spawnctl's config dir (default ~/.config/spawnctl).
func defaultConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config dir: %w", err)
	}
	return filepath.Join(base, "spawnctl"), nil
}

func keyfilePath(dir string) string   { return filepath.Join(dir, keyfileName) }
func deviceSetPath(dir string) string { return filepath.Join(dir, deviceSetName) }

// deviceFingerprint is a short, human-comparable identity for a device: hex
// SHA-256 over its two public keys, grouped for readability. Same bytes → same
// fingerprint, so two operators can confirm a device out-of-band.
func deviceFingerprint(ref seal.DeviceRef) string {
	h := sha256.New()
	h.Write(ref.X25519Pub)
	h.Write(ref.SignPub)
	sum := h.Sum(nil)
	hexs := fmt.Sprintf("%x", sum[:16]) // 128 bits is plenty for a display fingerprint
	var b strings.Builder
	for i := 0; i < len(hexs); i += 4 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hexs[i : i+4])
	}
	return "SP-" + b.String()
}

// ---- init ----

// initDeviceSet runs the first-device ceremony locally: it derives device₁ and
// the recovery virtual device from fresh BIP-39 mnemonics, writes device₁'s
// 0600 keyfile, and writes the genesis device-set (device₁ + recovery,
// co-signed). It returns the RECOVERY mnemonic — the one secret the caller must
// surface to the user and which is never stored on disk.
func initDeviceSet(dir string, force bool) (recoveryMnemonic string, err error) {
	if !force {
		if _, statErr := os.Stat(keyfilePath(dir)); statErr == nil {
			return "", fmt.Errorf("keyfile already exists at %s (use --force to overwrite)", keyfilePath(dir))
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create config dir: %w", err)
	}

	dev1Mnemonic, err := seal.NewMnemonic()
	if err != nil {
		return "", err
	}
	device1, err := seal.DeviceFromMnemonic(dev1Mnemonic, "")
	if err != nil {
		return "", err
	}

	recoveryMnemonic, err = seal.NewMnemonic()
	if err != nil {
		return "", err
	}
	recovery, err := seal.RecoveryDevice(recoveryMnemonic)
	if err != nil {
		return "", err
	}

	log, err := seal.NewGenesis(device1, recovery)
	if err != nil {
		return "", err
	}

	if err := device1.WriteKeyfile(keyfilePath(dir)); err != nil {
		return "", fmt.Errorf("write keyfile: %w", err)
	}
	dsf := &deviceSetFile{
		Root: seal.OwnerRoot{
			Device1SignPub:  device1.Ref().SignPub,
			RecoverySignPub: recovery.Ref().SignPub,
		},
		Log: log,
	}
	if err := writeDeviceSet(dir, dsf); err != nil {
		return "", err
	}
	return recoveryMnemonic, nil
}

func writeDeviceSet(dir string, dsf *deviceSetFile) error {
	b, err := json.MarshalIndent(dsf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal device set: %w", err)
	}
	if err := os.WriteFile(deviceSetPath(dir), b, 0o600); err != nil {
		return fmt.Errorf("write device set: %w", err)
	}
	return nil
}

// ---- load helpers ----

func loadDevice(dir string) (*seal.Device, error) {
	d, err := seal.LoadKeyfile(keyfilePath(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no device keyfile at %s — run `spawnctl key init` first", keyfilePath(dir))
		}
		return nil, err
	}
	return d, nil
}

func loadDeviceSet(dir string) (*deviceSetFile, error) {
	b, err := os.ReadFile(deviceSetPath(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no device set at %s — run `spawnctl key init` first", deviceSetPath(dir))
		}
		return nil, err
	}
	var dsf deviceSetFile
	if err := json.Unmarshal(b, &dsf); err != nil {
		return nil, fmt.Errorf("parse device set: %w", err)
	}
	return &dsf, nil
}

// ---- recover ----

// recoverDevice re-derives this device from a BIP-39 recovery mnemonic and
// writes its 0600 keyfile (spec §4: the recovery virtual device). This is a
// pure-local re-derivation; enrolling a fresh device and re-sealing DEKs to it
// is a later (CP-wired) wave.
func recoverDevice(dir, mnemonic string, force bool) (*seal.Device, error) {
	if !force {
		if _, statErr := os.Stat(keyfilePath(dir)); statErr == nil {
			return nil, fmt.Errorf("keyfile already exists at %s (use --force to overwrite)", keyfilePath(dir))
		}
	}
	dev, err := seal.RecoveryDevice(mnemonic)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	if err := dev.WriteKeyfile(keyfilePath(dir)); err != nil {
		return nil, fmt.Errorf("write keyfile: %w", err)
	}
	return dev, nil
}

// ---- rendering (pure, testable) ----

// formatKeyShow renders a device's PUBLIC identity — no private material.
func formatKeyShow(d *seal.Device) string {
	ref := d.Ref()
	var b strings.Builder
	fmt.Fprintf(&b, "fingerprint:       %s\n", deviceFingerprint(ref))
	fmt.Fprintf(&b, "x25519 (HPKE) pub: %x\n", ref.X25519Pub)
	fmt.Fprintf(&b, "p-256 (sign)  pub: %x\n", ref.SignPub)
	return b.String()
}

// formatInitResult renders the post-ceremony output: where the keys landed plus
// the recovery mnemonic and the mandatory loss-warning copy.
func formatInitResult(dir, recoveryMnemonic string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "device key written: %s (0600)\n", keyfilePath(dir))
	fmt.Fprintf(&b, "device set written: %s\n", deviceSetPath(dir))
	b.WriteString("\nYour BIP-39 recovery code (24 words):\n\n")
	b.WriteString("    " + recoveryMnemonic + "\n\n")
	b.WriteString(recoveryLossWarning + "\n")
	return b.String()
}

// formatDeviceSetShow resolves and verifies the device set, rendering the
// membership with fingerprints and a valid/invalid chain verdict.
func formatDeviceSetShow(dsf *deviceSetFile) string {
	var b strings.Builder
	members, err := seal.VerifyDeviceSet(dsf.Log, dsf.Root)
	version := uint64(0)
	if dsf.Log != nil && len(dsf.Log.Entries) > 0 {
		version = dsf.Log.Entries[len(dsf.Log.Entries)-1].Version
	}
	if err != nil {
		fmt.Fprintf(&b, "chain: INVALID — %v\n", err)
		// Still show the (untrusted) claimed membership of the head for diagnosis.
		if dsf.Log != nil && len(dsf.Log.Entries) > 0 {
			members = dsf.Log.Entries[len(dsf.Log.Entries)-1].Devices
		}
	} else {
		fmt.Fprintf(&b, "chain: VALID (version %d, %d entr%s)\n", version, len(dsf.Log.Entries), plural(len(dsf.Log.Entries)))
	}
	fmt.Fprintf(&b, "members (%d):\n", len(members))
	for i, m := range members {
		fmt.Fprintf(&b, "  [%d] %s\n", i+1, deviceFingerprint(m))
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// ---- cli wiring ----

func configDirFlag() *cli.StringFlag {
	return &cli.StringFlag{Name: "config-dir", Usage: "config dir (default ~/.config/spawnctl)"}
}

// resolveDir returns the --config-dir override or the platform default.
func resolveDir(c *cli.Command) (string, error) {
	if d := c.String("config-dir"); d != "" {
		return d, nil
	}
	return defaultConfigDir()
}

// keyCmd is the local owner device-key lifecycle command group.
func keyCmd() *cli.Command {
	return &cli.Command{
		Name:  "key",
		Usage: "manage this device's owner-sealed-secrets keys (local only)",
		Commands: []*cli.Command{
			keyInitCmd(),
			keyShowCmd(),
			keyRecoverCmd(),
			keyDeviceSetCmd(),
		},
	}
}

func keyInitCmd() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "generate this device's keys + device-set genesis; print the recovery code",
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.BoolFlag{Name: "force", Usage: "overwrite an existing keyfile"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			recovery, err := initDeviceSet(dir, c.Bool("force"))
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			fmt.Fprint(c.Writer, formatInitResult(dir, recovery))
			return nil
		},
	}
}

func keyShowCmd() *cli.Command {
	return &cli.Command{
		Name:  "show",
		Usage: "print this device's public identity + fingerprint",
		Flags: []cli.Flag{configDirFlag()},
		Action: func(_ context.Context, c *cli.Command) error {
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			d, err := loadDevice(dir)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			fmt.Fprint(c.Writer, formatKeyShow(d))
			return nil
		},
	}
}

func keyRecoverCmd() *cli.Command {
	return &cli.Command{
		Name:  "recover",
		Usage: "re-derive this device from a BIP-39 recovery mnemonic",
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.StringFlag{Name: "mnemonic", Usage: "the 24-word BIP-39 recovery code", Required: true},
			&cli.BoolFlag{Name: "force", Usage: "overwrite an existing keyfile"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			mnemonic := strings.TrimSpace(c.String("mnemonic"))
			dev, err := recoverDevice(dir, mnemonic, c.Bool("force"))
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			fmt.Fprintf(c.Writer, "device key recovered: %s (0600)\n\n", keyfilePath(dir))
			fmt.Fprint(c.Writer, formatKeyShow(dev))
			return nil
		},
	}
}

func keyDeviceSetCmd() *cli.Command {
	return &cli.Command{
		Name:  "device-set",
		Usage: "inspect the local device set",
		Commands: []*cli.Command{
			{
				Name:  "show",
				Usage: "print the resolved device set + verify the chain",
				Flags: []cli.Flag{configDirFlag()},
				Action: func(_ context.Context, c *cli.Command) error {
					dir, err := resolveDir(c)
					if err != nil {
						return cli.Exit(err.Error(), 1)
					}
					dsf, err := loadDeviceSet(dir)
					if err != nil {
						return cli.Exit(err.Error(), 1)
					}
					fmt.Fprint(c.Writer, formatDeviceSetShow(dsf))
					return nil
				},
			},
		},
	}
}
