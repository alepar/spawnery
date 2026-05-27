# Spawnery E6 — Web Client (Design)

**Bead:** `sp-95v`
**Status:** Draft v1 (interview complete; pending user review)
**Date:** 2026-05-27
**Depends on:** [E0](2026-05-26-spawnery-e0-contracts-design.md),
[E1](2026-05-27-spawnery-e1-runtime-core-design.md), [E2](2026-05-27-spawnery-e2-model-layer-design.md)

The browser app: catalog → spawn → chat, acting as the client end of the per-session E2E channel.
Third piece of the zork vertical slice (with E1 + E2).

---

## 1. Client architecture

- **SPA, no BFF.** The SPA talks the **CP HTTP/OpenAPI** for auth, catalog, and spawn management,
  and establishes the **E2E session channel to the node** (via the CP relay) for live sessions.
  A BFF can't sit in the E2E path, and the CP is the OAuth authority, so no separate backend.
- Embeds an **ACP client** (renders `session/update` streaming, tool-calls, prompts) and
  **WebCrypto** (ECDH key agreement + AEAD for the session channel; decrypting vault-stored secrets).
- **Ephemeral per-session client keypair** (forward secrecy); the CP **session token** authenticates
  the owner. No persistent device key in MVP.

---

## 2. The per-session E2E channel (client side)

Canonical mechanism in **[E0 §10](2026-05-26-spawnery-e0-contracts-design.md)**. The browser:
1. `POST /spawns/{id}/session` → gets the **session token** + the **node pubkey** + rendezvous endpoint.
2. Generates an **ephemeral keypair**; opens a WebSocket to the **CP rendezvous**.
3. Runs the **authenticated key agreement** (node static + client ephemeral) → **session symmetric key**;
   presents the session token so the node authorizes the owner.
4. **AEAD-encrypts all non-metadata** (ACP frames, secrets) over that WS; the CP relays **opaque
   ciphertext**; the **node decrypts + forwards to the agent over loopback**.
- **Metadata** (spawn id, routing) stays plaintext to the CP by design.

---

## 3. Catalog → spawn wizard → chat

- **Browse/search** the catalog (CP catalog API; open Apps public, private gated by entitlement).
- **Spawn wizard:** pick App version, **choose agent** (agent-agnostic; filtered by manifest
  `agents.*`), **choose model** (managed | BYO; filtered by `model.requires`), choose **storage**
  (E3), confirm. Client calls **`POST /spawns`**; repo init + `spawn.yml` write is CP/node-side (E3).
- **Chat:** open the session (§2), stream the ACP session as chat; render tool-calls/activity.
  **Resume** an idle spawn = new session (cold start; agent reloads the thread from `/data`).
- **Spawn management:** list/open/delete spawns; clean-exit deletes the CP pointer (data stays in
  the repo); export/re-spawn is just the data repo (E0 portability).

---

## 4. Secrets in the browser

- The **vault passphrase** (E4) unlocks the user's BYO-key ciphertext (CP stores ciphertext only).
- The decrypted BYO key is sent to the node **over the per-session E2E channel** (§2) at session
  start; the node injects it into the sidecar (E2 §2). Passphrase/key never leave the client except
  as session-encrypted bytes. Passphrase UX/storage detail is **E4**.

---

## 5. Mobile

- **MVP = responsive web / PWA** (one codebase; web + mobile are identical clients per E0 §10).
- **Native mobile apps → post-MVP.**

---

## 6. Deferred (post-MVP)
Native mobile apps · LAN-direct + P2P data paths (E0) · out-of-band node-key pinning UX · offline/PWA
caching of the catalog · rich session history search UI.

---

## Appendix — E6 decision log

| # | Decision | Choice |
|---|---|---|
| E6.1 | Content privacy | All non-metadata E2E via a per-session symmetric key (node-static + client-ephemeral key agreement), CP relays opaque ciphertext; metadata plaintext. Node terminates + forwards over loopback. Supersedes E0 §10 bridge-cert / E2 §2 per-secret seal. |
| E6.2 | Client architecture (asserted) | SPA, no BFF; talks CP HTTP + establishes E2E channel to node; embeds ACP client + WebCrypto; ephemeral per-session client key |
| E6.3 | Mobile (asserted) | Responsive web / PWA for MVP; native post-MVP |
