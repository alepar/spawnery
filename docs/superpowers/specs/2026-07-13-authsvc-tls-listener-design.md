# AuthService TLS Listener (node mTLS)

- **Bead:** `sp-astls` — blocks `sp-wwtc.5` (the agent-side MITM assertion) and `sp-2tx8.3.8`
- **Status:** draft
- **Mode:** one-shot (Mode B)

## 1. Problem

**AuthService can authenticate a node by mTLS, but cannot serve TLS — so that code path is dead in the
shipped binary.**

- `internal/authsvc/node_identity.go:38` — `nodeIdentityMiddleware` establishes a node's identity by reading
  `r.TLS.PeerCertificates` and verifying the chain against the root. For `r.TLS` to be populated, **AS itself
  must terminate TLS**; a TLS-terminating proxy in front would leave `r.TLS` nil and the identity unset.
- `cmd/authsvc/main.go:179` — the binary calls plain `srv.ListenAndServe()`. There is no `ListenAndServeTLS`,
  no cert/key config, and no client-CA config anywhere in `cmd/authsvc`, `config/`, or `deploy/`.

So the node→AS mint lane (`nodeGitHubMint`'s enforced branch, `cmd/spawnlet/main.go:547`) builds an **mTLS
client against a service that cannot speak TLS**. The middleware is tested — but only against a TLS server the
test stands up itself (`internal/node/github_refresh_e2e_test.go:136`, `ClientAuth: tls.VerifyClientCertIfGiven`).
The test proves the *middleware*; nothing proves the *binary*.

The immediate consequence is that the e2e VM lane cannot run the mint lane, so the node's GitHub control
server is never constructed (`internal/node/attach.go:173` — `if cfg.GitHubMint != nil`), so the per-spawn
MITM CA, the agent's CA bundle, and the agent's `HTTP_PROXY` are all absent, so **the sidecar's GitHub MITM
proxy has zero end-to-end coverage**. But the deeper consequence is the production one: as shipped, a node
cannot present an identity AS can read.

## 2. Main challenges

**AS serves two very different populations on the same mux.** Browsers and CLIs hit the OAuth and device-flow
routes with **no client certificate**. Nodes hit the mint/enroll/fanout routes **with** one. A naive
`RequireAndVerifyClientCert` would break every human caller.

Second, this is the security-critical binary. The failure mode of getting it wrong is not a red test — it is
an auth service that accepts an identity it should not.

## 3. Key decisions

Add an **optional** TLS listener to `cmd/authsvc`: when a cert and key are configured, serve TLS with
`ClientAuth: VerifyClientCertIfGiven` against a configured client-CA pool; otherwise keep today's plain
`ListenAndServe` byte-for-byte. Additive, inert by default, and it makes the existing (already-reviewed)
`nodeIdentityMiddleware` reachable for the first time.

## 4. Decision points

### 4.1 The client-auth mode

**Chosen: `tls.VerifyClientCertIfGiven`.**

A client certificate is **optional**, but if one is presented it **must** chain to the configured client CA or
the handshake fails. This is the only mode that fits AS's actual traffic:

- browser / CLI (OAuth, device flow, refresh): presents no cert → connects anonymously → unchanged behaviour.
- node (mint, enroll, fanout, revocation): presents its cert → verified → `nodeIdentityMiddleware` extracts the
  identity exactly as it does today.

It is also precisely what the existing e2e test already asserts against
(`github_refresh_e2e_test.go:136`), so the binary and the test agree on the contract.

*Considered and rejected:* `RequireAndVerifyClientCert` — it would reject every browser and CLI caller, i.e.
break OAuth and the device flow outright. *Considered and rejected:* a TLS-terminating reverse proxy in front
of AS — it cannot work: `nodeIdentityMiddleware` reads `r.TLS`, which a proxied plain-HTTP request never has.
This is the crux of why the capability has to live in the binary.

**Non-negotiable:** *anonymous is not authenticated.* `VerifyClientCertIfGiven` lets an un-certed client
connect; it must never let one reach a node-identity-gated route. Authorisation stays where it is — the routes
that need a node identity must continue to reject a request that has none. This design widens **who can
connect**, never **who is trusted**.

### 4.2 Configuration, and the default

**Chosen: `AS_TLS_CERT` + `AS_TLS_KEY` + `AS_CLIENT_CA`, all optional. With cert+key unset, behaviour is
byte-for-byte what ships today (plain `ListenAndServe`).**

- `AS_TLS_CERT` / `AS_TLS_KEY` — the server identity. If exactly one is set, that is a **fatal config error**,
  not a silent downgrade to plaintext: a half-configured TLS listener that quietly serves HTTP is precisely the
  failure you never want in an auth service.
- `AS_CLIENT_CA` — the pool that node client certs are verified against. If TLS is on and this is unset, no
  client cert can ever verify, so node identity is impossible; **warn loudly** at startup rather than failing,
  since a TLS-only-no-clients deployment is legitimate.

`AS_ROOT_CA_PEM` already exists and feeds `nodeIdentityMiddleware`'s chain verification. `AS_CLIENT_CA` is the
*handshake*-level pool. They will usually be the same file, and the config should say so — but they are
different layers and conflating them in code would be wrong.

### 4.3 What this does NOT change

- `nodeIdentityMiddleware` — untouched. It already does the right thing; it has simply never been reachable.
- `AS_DEV_RELAX_NODE_AUTH` — untouched, and must remain unset in every enforced/prod recipe. This work makes
  the *real* path available, which weakens the argument for ever using the relaxed one.
- The mux, the routes, and every authorisation check — untouched.

## 5. Acceptance criteria

- With `AS_TLS_CERT`/`AS_TLS_KEY` **unset**, the binary's behaviour is unchanged (plain HTTP). Proven by the
  existing AS tests staying green untouched.
- With them set, AS serves TLS, and a node presenting a valid client cert gets a **verified identity** through
  the real binary — not a test-constructed server.
- A client presenting **no** cert still completes OAuth/device flows (browsers are not broken).
- A client presenting an **invalid/untrusted** cert **fails the handshake** (not "connects anonymously").
- **Anonymous callers are still rejected from node-identity-gated routes** — a test must assert this, because
  it is the one way `VerifyClientCertIfGiven` could be turned into a privilege escalation.
- Setting exactly one of cert/key is a startup error, not a silent plaintext downgrade.
- The e2e VM lane can then run the enforced mint lane, which switches on the node's GitHub control server and
  gives the sidecar MITM proxy its first end-to-end coverage.

## 6. Out of scope

Changing how node identity is verified once extracted. Changing any authorisation rule. Redis/DB TLS.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the
assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
