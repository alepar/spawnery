/**
 * Key ceremony state machine (Phase 4, [M14][WM11][WM10]).
 *
 * The lazy ceremony triggers on the first secret-bearing action (spec §2:
 * "Lazy ceremony — not at signup"). It:
 *   1. Generates this device's non-extractable X25519 + ECDSA-P256 keypairs.
 *   2. Generates a BIP-39 recovery phrase (the "virtual device").
 *   3. Co-signs the genesis device-set entry (device1 + recovery).
 *   4. Displays the phrase + mandatory loss-disclosure copy + phrase re-entry
 *      confirmation step ([WM11]).
 *   5. Calls navigator.storage.persist() and surfaces the result ([WM11]).
 *   6. Performs a ceremony-time round-trip check: re-derives recovery pubkeys
 *      from the confirmed phrase, asserts they equal the enrolled recovery
 *      DeviceRef before publishing ([WM10]).
 *   7. Publishes the genesis entry atomically to the AS.
 *   8. Stores the device keys in IndexedDB and resumes the parked action.
 *
 * Abandoning mid-ceremony persists nothing anywhere ([M14]).
 */

import { generateDeviceKeys, deriveDeviceKeysFromMnemonic, exportDeviceRef, storeDeviceKeys } from "./device";
import { generateMnemonic } from "./bip39";
import { buildGenesisEntry, appendEntry, type StoredEntry, type OwnerRoot } from "./deviceset";
import { toBase64 } from "./encoding";

export type CeremonyStep =
  | "idle"
  | "generating"
  | "show-phrase"
  | "confirm-phrase"
  | "persist-prompt"
  | "round-trip-check"
  | "publishing"
  | "done"
  | "error";

export interface CeremonyState {
  step: CeremonyStep;
  recoveryPhrase?: string;
  persistGranted?: boolean;
  error?: string;
  genesisEntry?: StoredEntry;
  ownerRoot?: OwnerRoot;
}

/**
 * Mandatory loss-disclosure copy (spec §4, verbatim from owner-sealed-secrets-design.md §4).
 * Must be displayed at every ceremony.
 */
export const LOSS_DISCLOSURE = `Without this recovery code and your devices, suspended spawn contents \
cannot be recovered by anyone, including Spawnery. Write it down and store it somewhere safe and offline. \
It is shown ONCE and is never stored on any server.`;

/**
 * Browser data clearing warning — surfaced alongside the loss disclosure ([WM11]).
 * Names Safari's 7-day ITP eviction explicitly.
 */
export const BROWSER_EVICTION_WARNING = `Your device key is stored in this browser's secure storage. \
Clearing browser data, using private/incognito mode, or Safari's 7-day inactivity eviction \
will delete the key. Enroll a second device before the first seal to avoid loss.`;

/**
 * CeremonyContext holds all ceremony-time key material. It is created at the
 * start and discarded (zeroed) once the ceremony completes or is abandoned.
 */
export interface CeremonyContext {
  deviceKeys: Awaited<ReturnType<typeof generateDeviceKeys>>;
  recoveryKeys: Awaited<ReturnType<typeof generateDeviceKeys>>;
  recoveryPhrase: string;
  genesisEntry: StoredEntry;
  ownerRoot: OwnerRoot;
}

/**
 * startCeremony generates the device and recovery keypairs, builds the
 * (unsigned) genesis entry, and returns the ceremony context.
 *
 * Does NOT persist anything — call completeCeremony after the user confirms
 * the phrase.
 */
export async function startCeremony(opts: {
  deviceName?: string;
  recoveryName?: string;
}): Promise<CeremonyContext> {
  const deviceKeys = await generateDeviceKeys();
  const recoveryPhrase = await generateMnemonic();
  const recoveryKeys = await deriveDeviceKeysFromMnemonic(recoveryPhrase);

  const deviceRef = await exportDeviceRef(deviceKeys);
  const recoveryRef = await exportDeviceRef(recoveryKeys);

  const device1Ref = {
    x25519_pub: toBase64(deviceRef.x25519Pub),
    sign_pub: toBase64(deviceRef.signPub),
  };
  const recoveryRefDS = {
    x25519_pub: toBase64(recoveryRef.x25519Pub),
    sign_pub: toBase64(recoveryRef.signPub),
  };

  const genesisEntry = await buildGenesisEntry(
    device1Ref,
    recoveryRefDS,
    opts.deviceName ?? "device1",
    opts.recoveryName ?? "recovery",
    deviceKeys.ecdsaPrivate,
    recoveryKeys.ecdsaPrivate,
  );

  const ownerRoot: OwnerRoot = {
    device1_sign_pub: device1Ref.sign_pub,
    recovery_sign_pub: recoveryRefDS.sign_pub,
  };

  return { deviceKeys, recoveryKeys, recoveryPhrase, genesisEntry, ownerRoot };
}

/**
 * ceremonyRoundTripCheck re-derives the recovery pubkeys from the confirmed
 * phrase and asserts they equal the enrolled recovery DeviceRef ([WM10]).
 *
 * Throws if the pubkeys do not match — the ceremony must not proceed.
 */
export async function ceremonyRoundTripCheck(
  ctx: CeremonyContext,
  confirmedPhrase: string,
): Promise<void> {
  const rederived = await deriveDeviceKeysFromMnemonic(confirmedPhrase);
  const rdRef = await exportDeviceRef(rederived);
  const rdX25519B64 = toBase64(rdRef.x25519Pub);
  const rdSignB64 = toBase64(rdRef.signPub);

  const [_, genesisRecovery] = genesisDevices(ctx.genesisEntry);
  if (rdX25519B64 !== genesisRecovery.x25519_pub || rdSignB64 !== genesisRecovery.sign_pub) {
    throw new Error(
      "ceremony: re-derived recovery keys do not match the enrolled DeviceRef — phrase may have been mis-entered",
    );
  }
}

/**
 * completeCeremony persists device keys to IndexedDB, calls navigator.storage.persist(),
 * and publishes the genesis entry to the AS. Returns the persist result.
 *
 * This is the point of no return — only call after phrase confirmation and
 * round-trip check pass.
 */
export async function completeCeremony(
  ctx: CeremonyContext,
  asUrl: string,
  bearerToken: string,
): Promise<{ persistGranted: boolean }> {
  // Publish genesis to AS first (atomic — no local state persisted until success)
  await appendEntry(asUrl, bearerToken, ctx.genesisEntry);

  // Now persist keys to IndexedDB + call navigator.storage.persist()
  const { persistGranted } = await storeDeviceKeys(ctx.deviceKeys);
  return { persistGranted };
}

// ── Helpers ───────────────────────────────────────────────────────────────────

function genesisDevices(
  entry: StoredEntry,
): [{ x25519_pub: string; sign_pub: string }, { x25519_pub: string; sign_pub: string }] {
  const bodyBytes = new Uint8Array(
    atob(entry.body).split("").map((c) => c.charCodeAt(0)),
  );
  const body = JSON.parse(new TextDecoder().decode(bodyBytes));
  if (!Array.isArray(body.devices) || body.devices.length < 2) {
    throw new Error("ceremony: genesis has fewer than 2 devices");
  }
  return [body.devices[0], body.devices[1]];
}
