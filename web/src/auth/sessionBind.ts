/**
 * sessionBind — build the /ws/session bind frame, including the session-open SignedIntent the
 * node's enforced IntentVerifier requires.
 *
 * Without a session-open intent the node NACKs `MISSING_INTENT` and never attaches the client, so
 * the agent/terminal panels come back blank (spawnery bug sp-rxvb). This mirrors cmd/spawnctl's
 * runCP bindFrame.SessionAuth: sign spawnId + the live episode `generation` + sessionId. spawnctl's
 * own terminal sidesteps the CP by attaching via mosh direct to the node; the web goes through the
 * CP /ws/session bridge, which is the only path that drives the CP-side session-open.
 */

import { getAccessToken, authEnabled, useSessionStore } from "@/auth/session";
import { getOrCreateSessionKey } from "@/auth/keypair";
import { buildSessionOpenSignedIntentB64 } from "@/auth/intent";
import { listSpawns } from "@/api/spawnlet";

export interface SessionBindFrame {
  spawnId: string;
  sessionId: string;
  clientId: string;
  token: string;
  cursor: number;
  /** base64(proto.Marshal(SignedIntent)) — present only when auth is enabled. */
  signedIntent?: string;
}

/**
 * buildSessionBindFrame returns the JSON bind frame for a /ws/session socket. When auth is enabled
 * it signs an OpSessionOpen intent over the spawn's current live generation. The generation must be
 * current at (re)connect time — a stale one NACKs CORRESPONDENCE and the socket retries — so it is
 * fetched here rather than threaded through props.
 */
export async function buildSessionBindFrame(
  spawnId: string,
  sessionId: string,
  clientId: string,
  cursor: number,
): Promise<SessionBindFrame> {
  const frame: SessionBindFrame = { spawnId, sessionId, clientId, token: getAccessToken(), cursor };
  // Dev without auth (authEnabled()=false): the node accepts the open without an intent.
  if (!authEnabled()) return frame;
  try {
    const generation = (await listSpawns()).find((s) => s.spawnId === spawnId)?.generation ?? 0n;
    const kp = await getOrCreateSessionKey(useSessionStore.getState().keyStore);
    frame.signedIntent = await buildSessionOpenSignedIntentB64(
      spawnId,
      sessionId || "0",
      generation,
      kp.privateKey,
      kp.publicKey,
    );
  } catch (e) {
    // Best-effort: surface but don't block — a bind without an intent NACKs and the socket retries.
    console.error("session bind: session-open intent signing failed:", e);
  }
  return frame;
}
