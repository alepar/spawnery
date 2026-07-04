# Reconcile notes — full prod-mode stack validated on `blacky` (2026-07-03)

The whole stack was brought up live in a VM off the golden image; all six services active:
`spawnery-authsvc` (:8090), `spawnery-cp` (:8080 + node-mTLS :8081, **node-auth mode=enforced**),
`spawnery-node` (**registered over enforced mTLS**, class=cloud), `caddy` (:443 TLS), postgresql,
containerd. AS `/healthz` OK. These are the fixes that got there — **fold into `provision.sh`**.

## Findings (in the order hit)

1. **`virt-install` defaulted to `qemu:///session`** → couldn't see the system-mode network. Fixed
   in `build-base.sh` (`--connect "$LIBVIRT_URI"`).
2. **PKI must come from `spawnery-ca dev`, not the openssl fallback.** `spawnery-ca dev <dir>` writes
   `root.pem`/`root-key.pem`, `self-hosted-intermediate.*`, `cloud-intermediate.*`, `cp-server.*`,
   `node/`, `node-cloud/`, `session-key.pem`, `session-pub.pem` — exactly what the AS/CP/node env
   references. The openssl fallback lacks intermediates + node identities. **→ install the binaries
   in `provision.sh` BEFORE `gen-pki`** so `spawnery-ca` is on PATH; sign the Caddy `*.e2e.test`
   wildcard with `root.pem`/`root-key.pem` (single host-trust anchor).
3. **Caddy runs as user `caddy`** → the wildcard key must be readable: `chmod 644 wildcard.{crt,key}`.
4. **`SPAWNERY_ENV=prod` resolves `cp.prod.yaml`'s `${sops:store.dsn}` regardless of env override** →
   patch `/etc/spawnery/config/cp.prod.yaml` to a literal DSN (no `${sops:}`).
5. **Postgres store** — the `pgx` driver was imported only under `//go:build pgtest`, so prod binaries
   failed `store open: sql: unknown driver "pgx"`. **FIXED in master (`23cce42`): pgx registered
   unguarded in `internal/cp/store/open.go`**, so postgres works in prod builds. Harness uses
   **postgres** (cp.prod.yaml store.driver=postgres + a literal DSN, `${sops:}` patched out).
   TWO postgres provisioning steps required: (a) `postgresql-setup --initdb`; **(b) set
   `pg_hba.conf` local TCP (127.0.0.1/32, ::1/128) to `scram-sha-256`** (default is `ident`, which
   ignores the password → `FATAL: Ident authentication failed`), and `ALTER USER spawnery PASSWORD
   'spawnery'` under `password_encryption=scram-sha-256`, then `systemctl reload postgresql`.
   Validated: CP comes up on real Postgres in prod mode with the fixed binary.
6. **`AS_GITHUB_TOKEN_ENC_KEY` must decode to exactly 32 bytes** (AES-256) — use a 32-char key
   base64'd. `fake_github=true` is NOT dev-gated, so prod + fake-GitHub is valid.
7. **CP seeds demo apps from relative `examples/secret-app`** (opened relative to CWD). systemd runs
   from `/`. **→ install `examples/` at `/opt/spawnery/examples` + `WorkingDirectory=/opt/spawnery`**
   drop-in for `spawnery-cp.service`.
8. **Per-run vars** (host/IP-specific — `roll.sh`/`up.sh` render `@@HOST@@`/`@@IP@@` at boot, restart):
   `AS_PUBLIC_URL`, `AS_REDIRECT_URIS`, `AS_GITHUB_LINK_REDIRECT_URI`, `AS_GITHUB_POST_REDEEM_REDIRECT`,
   `CP_ALLOWED_ORIGINS` = `https://<host>`; `NODE_ADVERTISE_IP` = `<ip>`;
   `AS_FAKE_GITHUB_BASE_URL` = `http://<host>:9099`.

The captured working env is in `env/common.env` + `env/profile.fake.env` (with the `@@HOST@@`/`@@IP@@`
placeholders). `provision.sh` should install those verbatim (minus the per-run render).

## Still open (before the acceptance suite runs green)

- **Agent/sidecar images not baked** (docker-in-distrobox perms) → build on the host, or add the
  distrobox user to gid 985, then `make images` + import into `k8s.io`. Needed for spawn *creation*.
- **Web content 404** — the golden's web bundle is a placeholder; `roll.sh` must build it per-run with
  `VITE_CP_ORIGIN=https://<host>` and deploy to `/var/www/spawnery`.
- **`roll.sh` node-list readiness gate** — verify the real CP node-list surface (node registration is
  visible in the CP journal: `msg="node connected" id=node-1`).
