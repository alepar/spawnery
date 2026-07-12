import {
  WebCryptoSessionSigner,
  buildSessionReauthSignedIntentB64,
  exportSpkiDer,
  sessionKeyHash,
} from "@spawnery/client";
import { requireSessionSigningKeys } from "./intent";
import { useSessionStore } from "./session";
import { validateAccessTokenPair } from "./token";

/** Verified session-open tuple plus the browser's current socket attachment incarnation. */
export interface VerifiedSessionAuthorization {
  spawnId: string;
  generation: bigint;
  targetNodeId: string;
  sessionId: string;
  clientId: string;
  attachmentSequence: number;
}

export interface NodeReauthControl {
  type: "nodeReauth";
  nodeAccessToken: string;
  signedIntent: string;
}

/** Build the exact browser-to-CP nodeReauth control for the current atomic token pair. */
export async function buildNodeReauthControl(
  authorization: VerifiedSessionAuthorization,
  nodeAccessToken: string,
): Promise<NodeReauthControl> {
  if (!authorization.spawnId || authorization.generation <= 0n || !authorization.targetNodeId ||
      !authorization.sessionId || !authorization.clientId || authorization.attachmentSequence <= 0) {
    throw new Error("session reauth: invalid attachment authorization context");
  }
  const session = useSessionStore.getState();
  if (!nodeAccessToken || nodeAccessToken !== session.nodeAccessToken) {
    throw new Error("session reauth: stale node access token");
  }
  try {
    const keys = await requireSessionSigningKeys();
    const localHash = await sessionKeyHash(await exportSpkiDer(keys.publicKey));
    const validated = validateAccessTokenPair({
      cpAccessToken: session.cpAccessToken,
      nodeAccessToken,
    }, localHash);
    const signedIntent = await buildSessionReauthSignedIntentB64({
      spawnId: authorization.spawnId,
      generation: authorization.generation,
      targetNodeId: authorization.targetNodeId,
      sessionId: authorization.sessionId,
      newTokenId: validated.node.tokenId,
    }, new WebCryptoSessionSigner(keys.privateKey, keys.publicKey));
    return { type: "nodeReauth", nodeAccessToken, signedIntent };
  } catch (error) {
    if (useSessionStore.getState().status !== "key-lost") {
      await useSessionStore.getState().recoverKeyLoss();
    }
    throw error;
  }
}
