import { exportSpkiDer, signP1363 } from "../keys/crypto.js";

/** Persistent login-bound signer supplied by the host application. */
export interface SessionSigner {
  publicSPKIDER(): Promise<Uint8Array>;
  signP1363(domain: string, exactBody: Uint8Array): Promise<Uint8Array>;
}

/** WebCrypto adapter for an existing keypair. It never creates or replaces keys. */
export class WebCryptoSessionSigner implements SessionSigner {
  constructor(
    private readonly privateKey: CryptoKey,
    private readonly publicKey: CryptoKey,
  ) {}

  publicSPKIDER(): Promise<Uint8Array> {
    return exportSpkiDer(this.publicKey);
  }

  async signP1363(domain: string, exactBody: Uint8Array): Promise<Uint8Array> {
    const domainBytes = new TextEncoder().encode(domain);
    const message = new Uint8Array(domainBytes.length + exactBody.length);
    message.set(domainBytes);
    message.set(exactBody, domainBytes.length);
    const signature = await signP1363(this.privateKey, message);
    if (signature.length !== 64) throw new Error("session signer: expected P1363 signature");
    return signature;
  }
}
