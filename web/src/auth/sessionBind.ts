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

import { WebCryptoSessionSigner, fromBase64 } from "@spawnery/client";
import { getAccessToken, getNodeAccessToken, authEnabled, useSessionStore } from "@/auth/session";
import { buildSessionOpenSignedIntentB64, requireSessionSigningKeys } from "@/auth/intent";
import { verifyResolvedTarget } from "@/auth/target";
import { unary } from "@/api/connect";
import type { VerifiedSessionAuthorization } from "./sessionReauth";

export interface SessionBindFrame {
  spawnId: string;
  sessionId: string;
  clientId: string;
  token: string;
  nodeAccessToken: string;
  cursor: number;
  /** base64(proto.Marshal(SignedIntent)) — present only when auth is enabled. */
  signedIntent?: string;
  /** Browser-only verified tuple. Callers remove it before serializing the bind frame. */
  authorization?: VerifiedSessionAuthorization;
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
  attachmentSequence = 1,
): Promise<SessionBindFrame> {
  const frame: SessionBindFrame = {
    spawnId,
    sessionId,
    clientId,
    token: getAccessToken(),
    nodeAccessToken: getNodeAccessToken(),
    cursor,
  };
  // Dev without auth (authEnabled()=false): the node accepts the open without an intent.
  if (!authEnabled()) return frame;
  const metadata = await unary<{
    nodeCertChain?: string;
    generation?: string | number;
    targetNodeId?: string;
    targetNodeClass?: string;
    targetNodeAccountId?: string;
  }>("GetSpawnNodeKey", { spawnId });
  const generation = BigInt(metadata.generation ?? 0);
  const target = {
    nodeCertChain: metadata.nodeCertChain ? fromBase64(metadata.nodeCertChain) : new Uint8Array(),
    targetNodeId: metadata.targetNodeId ?? "",
    targetNodeClass: metadata.targetNodeClass ?? "",
    targetNodeAccountId: metadata.targetNodeAccountId ?? "",
  };
  if (generation <= 0n || target.nodeCertChain.length === 0 || !target.targetNodeId ||
      !target.targetNodeClass || !target.targetNodeAccountId || !frame.token || !frame.nodeAccessToken) {
    throw new Error("session bind: incomplete authorization metadata");
  }
  const accountId = useSessionStore.getState().account?.accountId ?? "";
  await verifyResolvedTarget(target, accountId);
  const keys = await requireSessionSigningKeys();
  frame.signedIntent = await buildSessionOpenSignedIntentB64(
    spawnId,
    sessionId || "0",
    generation,
    target.targetNodeId,
    new WebCryptoSessionSigner(keys.privateKey, keys.publicKey),
  );
  frame.authorization = {
    spawnId,
    generation,
    targetNodeId: target.targetNodeId,
    sessionId: sessionId || "0",
    clientId,
    attachmentSequence,
  };
  return frame;
}
