package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"

	"spawnery/internal/secrets/seal"
)

func decodeBase64(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// Also try URL encoding
		b, err = base64.URLEncoding.DecodeString(s)
	}
	return b, err
}

func encodeBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// m8TrustedDeviceWarning is the verbatim M8 warning from the owner-sealed spec
// §3, displayed before any operation where the user enters a recovery phrase.
// It must never be suppressed or shortened.
const m8TrustedDeviceWarning = `SECURITY WARNING: You are about to enter your BIP-39 recovery phrase.
This phrase is the master key for all your sealed secrets. Only enter it on a
device you personally control and trust. In a shared, hotel, or observed
environment, cancel and use an enrolled device instead.
`

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

// recoverResult holds the output of a successful recovery.
type recoverResult struct {
	// FreshDevice is the newly-generated device whose keyfile was written.
	FreshDevice *seal.Device
	// NewRecoveryMnemonic is the replacement BIP-39 phrase (old one is now
	// retired in the local device set).
	NewRecoveryMnemonic string
}

// recoverDevice performs the full local recovery flow (spec §4 MVP [WM12]):
//
//  1. Re-derive the recovery virtual device from the entered mnemonic.
//  2. Load and verify the local device set — confirm the recovery key is enrolled.
//  3. Generate a fresh device (new X25519 + signing keypairs from a new mnemonic).
//  4. Add the fresh device to the local device set (signed by the recovery key).
//  5. Generate a NEW recovery mnemonic and add it to the local device set
//     (signed by the recovery key).
//  6. Remove the OLD recovery virtual device (signed by the fresh device, which
//     is now enrolled).
//  7. Write the fresh device's 0600 keyfile.
//
// Re-sealing existing DEKs to the updated device set requires CP/network access
// and is deferred — the local chain mutations happen here so the device set
// reflects the actual state (the removal is recorded; re-sealing follows
// separately when network access is available).
func recoverDevice(dir, mnemonic string, force bool) (*recoverResult, error) {
	if !force {
		if _, statErr := os.Stat(keyfilePath(dir)); statErr == nil {
			return nil, fmt.Errorf("keyfile already exists at %s (use --force to overwrite)", keyfilePath(dir))
		}
	}

	// 1. Derive the recovery key from the entered mnemonic.
	recoveryDev, err := seal.RecoveryDevice(mnemonic)
	if err != nil {
		return nil, err
	}

	// 2. Load and verify the local device set.
	dsf, err := loadDeviceSet(dir)
	if err != nil {
		return nil, err
	}
	members, err := seal.VerifyDeviceSet(dsf.Log, dsf.Root)
	if err != nil {
		return nil, fmt.Errorf("chain verification failed: %w", err)
	}

	// Confirm the recovery key is actually enrolled.
	recoveryRef := recoveryDev.Ref()
	found := false
	for _, m := range members {
		if bytes.Equal(m.SignPub, recoveryRef.SignPub) {
			found = true
			break
		}
	}
	if !found {
		return nil, errors.New("the entered mnemonic does not match any enrolled recovery device")
	}

	// 3. Generate a fresh device for this machine.
	freshMnemonic, err := seal.NewMnemonic()
	if err != nil {
		return nil, err
	}
	freshDev, err := seal.DeviceFromMnemonic(freshMnemonic, "")
	if err != nil {
		return nil, err
	}

	// 4. Add the fresh device to the local chain, signed by the recovery key.
	if err := dsf.Log.AddDeviceLabeled(freshDev.Ref(), recoveryDev, "recovered-device"); err != nil {
		return nil, fmt.Errorf("add fresh device to chain: %w", err)
	}

	// 5. Generate a new recovery phrase and add it, signed by the recovery key.
	newRecoveryMnemonic, err := seal.NewMnemonic()
	if err != nil {
		return nil, err
	}
	newRecoveryDev, err := seal.RecoveryDevice(newRecoveryMnemonic)
	if err != nil {
		return nil, err
	}
	if err := dsf.Log.AddDeviceLabeled(newRecoveryDev.Ref(), recoveryDev, "recovery"); err != nil {
		return nil, fmt.Errorf("add new recovery device to chain: %w", err)
	}

	// 6. Remove the OLD recovery virtual device, signed by the fresh device
	//    (which is now a member after step 4).
	if err := dsf.Log.RemoveDevice(recoveryRef.X25519Pub, freshDev); err != nil {
		return nil, fmt.Errorf("remove old recovery device from chain: %w", err)
	}

	// 7. Persist.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	if err := freshDev.WriteKeyfile(keyfilePath(dir)); err != nil {
		return nil, fmt.Errorf("write keyfile: %w", err)
	}
	if err := writeDeviceSet(dir, dsf); err != nil {
		return nil, err
	}
	return &recoverResult{FreshDevice: freshDev, NewRecoveryMnemonic: newRecoveryMnemonic}, nil
}

// ---- approve ----

// approveDevice processes an enrollment payload from a new device (the
// enrollee), appends a member-signed add-entry to the local device set, and
// prints the approval response (OwnerRoot + head) for the enrollee to paste.
//
// The caller is responsible for verifying the SAS out-of-band before running
// this command. The SAS must be computed from:
//   sha256(encodeFields("sas/v1", genesis_hash, head_hash, new_x25519_pub, new_sign_pub))
//
// The approval response returns the OwnerRoot + head so the enrollee can pin
// them (spec §2 [WM5]: never TOFU from the AS).
func approveDevice(dir, payloadJSON, deviceName string) (string, error) {
	// Parse the enrollment payload.
	var payload struct {
		X25519Pub string `json:"x25519Pub"` // base64
		SignPub   string `json:"signPub"`   // base64
		DeviceName string `json:"deviceName"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return "", fmt.Errorf("parse enrollment payload: %w", err)
	}

	x25519Pub, err := decodeBase64(payload.X25519Pub)
	if err != nil {
		return "", fmt.Errorf("decode x25519_pub: %w", err)
	}
	signPub, err := decodeBase64(payload.SignPub)
	if err != nil {
		return "", fmt.Errorf("decode sign_pub: %w", err)
	}

	newDeviceRef := seal.DeviceRef{X25519Pub: x25519Pub, SignPub: signPub}
	name := deviceName
	if name == "" {
		name = payload.DeviceName
	}

	// Load and verify the current chain.
	dsf, err := loadDeviceSet(dir)
	if err != nil {
		return "", err
	}
	if _, err := seal.VerifyDeviceSet(dsf.Log, dsf.Root); err != nil {
		return "", fmt.Errorf("chain verification failed: %w", err)
	}

	// Load the approver's device key.
	approver, err := loadDevice(dir)
	if err != nil {
		return "", err
	}

	// Append the add-entry signed by this device.
	if err := dsf.Log.AddDeviceLabeled(newDeviceRef, approver, name); err != nil {
		return "", fmt.Errorf("add device to chain: %w", err)
	}
	if err := writeDeviceSet(dir, dsf); err != nil {
		return "", err
	}

	headHash, err := dsf.Log.HeadHash()
	if err != nil {
		return "", err
	}
	headVersion := dsf.Log.HeadVersion()

	// Format the approval response.
	resp, err := json.MarshalIndent(map[string]any{
		"ownerRoot": map[string]string{
			"device1_sign_pub":  encodeBase64(dsf.Root.Device1SignPub),
			"recovery_sign_pub": encodeBase64(dsf.Root.RecoverySignPub),
		},
		"headHash":    encodeBase64(headHash),
		"headVersion": headVersion,
	}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(resp), nil
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
	if dsf.Log != nil {
		version = dsf.Log.HeadVersion()
	}
	if err != nil {
		fmt.Fprintf(&b, "chain: INVALID — %v\n", err)
		// Still show the (untrusted) claimed membership of the head for diagnosis.
		if dsf.Log != nil {
			members = dsf.Log.HeadDevices()
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
		Usage: "manage this device's owner-sealed-secrets keys",
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
		Usage: "recover from a BIP-39 recovery mnemonic, enroll a fresh device, rotate the recovery code",
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
			// Surface the M8 trusted-device warning before any mnemonic processing.
			fmt.Fprint(c.Writer, m8TrustedDeviceWarning)
			mnemonic := strings.TrimSpace(c.String("mnemonic"))
			result, err := recoverDevice(dir, mnemonic, c.Bool("force"))
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			fmt.Fprintf(c.Writer, "fresh device key written: %s (0600)\n", keyfilePath(dir))
			fmt.Fprintf(c.Writer, "device set updated:       %s\n", deviceSetPath(dir))
			fmt.Fprint(c.Writer, formatKeyShow(result.FreshDevice))
			fmt.Fprintf(c.Writer, "\nYour NEW BIP-39 recovery code (24 words):\n\n    %s\n\n", result.NewRecoveryMnemonic)
			fmt.Fprintln(c.Writer, recoveryLossWarning)
			fmt.Fprintln(c.Writer, "NOTE: re-sealing existing secrets to the updated device set requires")
			fmt.Fprintln(c.Writer, "CP/network access and must be performed separately (spawnctl key reseal).")
			return nil
		},
	}
}

func keyDeviceSetCmd() *cli.Command {
	return &cli.Command{
		Name:  "device-set",
		Usage: "inspect and manage the local device set",
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
			keyDeviceSetApproveCmd(),
		},
	}
}

// keyDeviceSetApproveCmd implements `spawnctl key device-set approve`.
//
// Takes an enrollment payload JSON (from the web or another CLI) and
// appends a member-signed add-entry to the local device set, then
// prints the approval response (OwnerRoot + head) for the enrollee.
//
// The SAS must be verified by the human operator before running this
// command (spec §2 [WM4]).
func keyDeviceSetApproveCmd() *cli.Command {
	return &cli.Command{
		Name:  "approve",
		Usage: "approve a device enrollment from a JSON payload; print the approval response",
		Description: `Processes an enrollment payload from a new device (web or another spawnctl).
Appends a member-signed add-entry to the local device set and prints the
approval response (OwnerRoot + head) for the enrollee to paste into their UI.

IMPORTANT: verify the SAS code out-of-band BEFORE running this command.
The SAS is computed from (genesis_hash || head_hash || new_device_pubkeys)
and must match the code displayed by the enrolling device.`,
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.StringFlag{
				Name:  "payload",
				Usage: "enrollment payload JSON (from the new device's link/QR); reads stdin if omitted",
			},
			&cli.StringFlag{
				Name:  "device-name",
				Usage: "override the device name from the payload",
			},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}

			payloadJSON := strings.TrimSpace(c.String("payload"))
			if payloadJSON == "" {
				// Read from stdin
				var sb strings.Builder
				buf := make([]byte, 4096)
				for {
					n, readErr := os.Stdin.Read(buf)
					if n > 0 {
						sb.Write(buf[:n])
					}
					if readErr != nil {
						break
					}
				}
				payloadJSON = strings.TrimSpace(sb.String())
			}
			if payloadJSON == "" {
				return cli.Exit("no enrollment payload provided (use --payload or pipe JSON to stdin)", 1)
			}

			resp, err := approveDevice(dir, payloadJSON, c.String("device-name"))
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			fmt.Fprintln(c.Writer, "Device enrolled. Send this approval response to the enrollee:")
			fmt.Fprintln(c.Writer)
			fmt.Fprintln(c.Writer, resp)
			return nil
		},
	}
}
