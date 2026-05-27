# Spawnery E2 — Model Layer (Design)

**Bead:** `sp-21b`
**Status:** Draft v1 (interview complete; pending user review)
**Date:** 2026-05-27
**Depends on:** [E0 contracts](2026-05-26-spawnery-e0-contracts-design.md),
[E1 runtime core](2026-05-27-spawnery-e1-runtime-core-design.md)

The model sidecar, the central gateway (managed), the BYO sealed-key path, and inference routing.

> Cross-epic: GPU-burst routing *trigger* → **E1 §5**; node enrollment/identity → **E1 §6**;
> usage-based billing/tiers → **E10**; conversation-content audit → **E8 / E0 §9**.

---

## 1. Topology (translation/routing split)

```
            ┌─ BYO ─────────────────────────────────────────▶ provider (direct)
 agent ──▶ model sidecar (OpenAI-compatible, translation lib)
            └─ managed ─▶ central gateway ─▶ local DeepSeek (home)  | cloud provider (burst)
                          (translation + key custody + metering + routing)
```

- **Sidecar** (per spawn, in the pod): exposes an **OpenAI-compatible** endpoint on loopback;
  embeds the shared **translation library**.
  - **BYO:** sidecar translates and **calls the provider directly** — never touches Spawnery's
    central path.
  - **Managed:** sidecar forwards (OpenAI-compatible) to the **central gateway**.
- **Central gateway** (cloud-only service): translation + **managed-key custody** + **metering** +
  **routing**. Both sidecar and gateway speak OpenAI-compatible and share the same translation lib.

---

## 2. BYO key — sealed-to-node, CP-relayed

- Each **node autogenerates a keypair at startup**; enrolls its **public key with the CP** (part
  of authenticated node enrollment, E1 §6).
- **CP vends the node's public key to the client.** The client **seals the BYO key to the node
  pubkey**; the sealed blob is **relayed (opaque) through the CP** to the node; the **node kubelet
  unseals and injects** the key into the sidecar (BYO mode).
- Client↔node always via the **CP relay** (handles self-hosted-node NAT).
- **Guarantee:** the **CP never sees plaintext** (only relays sealed bytes + vends pubkeys).
  Self-hosted node → nothing leaves your hardware. Spawnery-operated cloud node → the node
  transiently unseals in memory and content is audited for abuse (disclosed).
- **Trust:** client trusts the CP-vended node key; a self-hoster can pin their node key
  out-of-band (out-of-band verification → post-MVP). Generalizes to any client-supplied secret.

---

## 3. Managed inference (central gateway)

- **Managed keys live only in the central gateway** — never in spawn containers. The **sidecar
  authenticates** to the gateway with a **CP-issued, spawn-scoped, short-lived token**.
- **Local DeepSeek v4 Flash** runs behind an **OpenAI-compatible inference server** (e.g. vLLM-class)
  on the home machine; the gateway treats it as one provider endpoint.
- **Routing (per the E1 GPU-burst signal):** managed inference → **local DeepSeek (home)** while
  home GPU has headroom; **→ cloud provider** when home GPU is overloaded. Container placement (CPU)
  and inference routing (GPU) are independent (E1 §5).

---

## 4. Metering & free-tier enforcement

- The **gateway emits usage events** to the CP per managed call: `{spawnId, owner, provider, model,
  tokensIn, tokensOut, ts}`.
- The gateway **enforces free-tier caps in real time** (blocks managed calls past the cap).
  Subscription tiers, premium-model gating, BYO-tier rules, and creator rev-share are **E10**.
- **BYO usage is not metered/billed** by Spawnery (it's the user's key/provider account); BYO on
  Spawnery-operated infra is still **audited** for abuse at the sidecar (E0 §9).

---

## 5. MVP provider scope

- **Managed local DeepSeek first** — enough for the zork vertical slice + free tier.
- **BYO via OpenAI-compatible providers** through the shared translation lib; major providers
  (OpenAI/Anthropic/Google) added incrementally. New providers are cheap to add behind the lib.
- **Model catalog + capability matching** (offer set; filter by the App `model.requires` contract)
  is surfaced via the catalog — detailed in **E5**; E2 supplies the capability metadata per model.

---

## 6. Deferred (post-MVP)
Out-of-band node-key pinning/verification · subscription tiers / billing / rev-share (E10) ·
premium-model gating · broad provider matrix · embeddings/vector endpoints (system design §11).

---

## Appendix — E2 decision log

| # | Decision | Choice |
|---|---|---|
| E2.1 | Translation/routing | Split: sidecar translates + direct-calls provider for BYO; managed → central gateway (translation + key + metering + routing) |
| E2.2 | BYO key delivery | Sealed to the **node** pubkey, relayed opaque via CP, node unseals + injects; CP never sees plaintext |
| E2.3 | Managed keys (asserted) | Central gateway only; sidecar auths with CP-issued spawn-scoped short-lived token |
| E2.4 | Local DeepSeek (asserted) | Behind an OpenAI-compatible server; a provider endpoint to the gateway |
| E2.5 | Routing (asserted) | Managed → local DeepSeek (home) else cloud, per E1 GPU-burst signal |
| E2.6 | Metering (asserted) | Gateway emits usage events to CP + enforces free-tier caps; tiers/billing → E10 |
| E2.7 | MVP providers (asserted) | Managed local DeepSeek first; BYO via OpenAI-compatible through shared lib |
