# Auth Service (AS) — Deployment Guide

**Spec:** `docs/superpowers/specs/2026-06-11-auth-identity-design.md` §6 + adversarial review [AM13]

## Overview

The AS is the identity root of trust. It runs as a dedicated process (container) separate from the
CP. Its data is sqlite tier-0: small, critical, continuously replicated. It holds:

- The self-hosted PKI intermediate cert + key (enrollment/node certs)
- The session-signing Ed25519 key (access tokens, revocation feed)
- The user store + refresh-session families + device grants (sqlite)

## Tier-0 SQLite: Durability Contract [AM13]

**RPO target: ≤ 5 minutes** (the authsvc sqlite database must be replicated with max 5-minute lag).

### Recommended: litestream continuous replication

[Litestream](https://litestream.io) replicates SQLite WAL frames to S3/GCS/Azure in near-real-time
(typically 1–10s lag). Run litestream alongside authsvc in the same pod/container:

```yaml
# docker-compose (or equivalent) example
authsvc:
  image: spawnery/authsvc:latest
  environment:
    AS_DB_DSN: "file:/data/authsvc/identity.db?_pragma=foreign_keys(1)"
    # ... other env vars
  volumes:
    - authsvc_data:/data/authsvc

litestream:
  image: litestream/litestream:latest
  command: replicate
  volumes:
    - authsvc_data:/data/authsvc
    - ./litestream.yml:/etc/litestream.yml
  environment:
    LITESTREAM_ACCESS_KEY_ID: "..."
    LITESTREAM_SECRET_ACCESS_KEY: "..."
```

```yaml
# litestream.yml
dbs:
  - path: /data/authsvc/identity.db
    replicas:
      - url: s3://your-bucket/authsvc/identity.db
        retention: 72h
        sync-interval: 5s   # WAL flush; effective RPO ≈ 5s under normal conditions
```

### Backup snapshots

In addition to WAL streaming, schedule daily snapshots:
```sh
litestream snapshot -replica s3 /data/authsvc/identity.db
```

### Restore procedure (rehearse quarterly)

```sh
# 1. Stop authsvc.
# 2. Restore from litestream.
litestream restore -o /data/authsvc/identity.db \
  -timestamp "2024-01-01T12:00:00Z" \  # or omit for latest
  s3://your-bucket/authsvc/identity.db

# 3. Verify the DB is healthy.
sqlite3 /data/authsvc/identity.db "PRAGMA integrity_check;"

# 4. Restart authsvc.
```

**Recovery matrix:**

| Scenario | Action |
|---|---|
| DB lost, key intact | Restore from litestream replica; no key ceremony needed |
| Key lost, DB intact | Emergency key rotation (see below); re-issue via PKI chain |
| Both lost | Restore DB + emergency key rotation; users must re-login |
| Restore behind WM6 chain head | Session families from before the restore gap expire naturally; no manual cleanup |

## Signing Key Custody [AM13]

The session-signing Ed25519 key is **the most sensitive secret the AS holds**. Its compromise lets
an attacker mint arbitrary session tokens valid at every CP and node until rotation completes.

### Key file permissions

```sh
chmod 600 /etc/spawnery/as/session-key.pem
chown authsvc:authsvc /etc/spawnery/as/session-key.pem
```

The key file must be owned by the authsvc process user and readable only by that user.

### Key generation (one-time setup)

```sh
# Generate a new Ed25519 key (PKCS#8 PEM):
openssl genpkey -algorithm ed25519 -out /etc/spawnery/as/session-key.pem
chmod 600 /etc/spawnery/as/session-key.pem

# The AS derives key_id automatically from the key material.
```

### Offline escrow

Store a copy of the session signing key in offline escrow:
- Encrypted with age/GPG (operator key ceremony, similar to root CA ceremony)
- Stored offline (not in the same cloud region as production)
- Access requires 2-of-N operator approval (recommended)

### Key rotation (routine) [AM4]

1. Generate a new key (`AS_SESSION_KEY_NEXT_PEM`) and deploy to all CP/node verifiers (config push).
2. Wait for the overlap window (≥ token TTL = 15 min recommended, 24h for safety).
3. Move `AS_SESSION_KEY_NEXT_PEM` → `AS_SESSION_KEY_PEM`; restart authsvc.
4. Monitor for verification failures; retire old key from verifier configs after all old tokens expire.
5. Update offline escrow with the new key.

### Emergency key rotation (compromise) [AM4]

If the session signing key is compromised:
1. Generate a replacement key immediately.
2. Deploy the replacement signed via the enrollment-pinned PKI chain (the CP accepts it on
   the PKI trust path, bypassing the compromised session key).
3. Revoke all active refresh families (triggers AS→CP revocation events; severs all sessions).
4. Update offline escrow.

## Re-Binding After DB Restore — Non-Goal [AM13]

Binding a fresh `accountId` to GitHub credentials after a DB restore is explicitly **not
supported** in A1. Users whose records were lost in the gap must re-register (if registration
is open) with a new account_id. The restore-vs-chain-head note above covers the family expiry
path; no admin tool is provided for manual re-binding.

## Environment Variables

See `cmd/authsvc/main.go` file header for the complete environment variable reference.

Key variables for production:

| Variable | Required | Notes |
|---|---|---|
| `AS_DB_DSN` | Yes | SQLite file path with WAL enabled |
| `AS_SESSION_KEY_PEM` | Yes | Path to session signing key (0600) |
| `GITHUB_CLIENT_ID` | Yes | GitHub App client_id |
| `GITHUB_CLIENT_SECRET` | Yes | GitHub App client_secret |
| `AS_GITHUB_REDIRECT_URI` | Yes | AS callback URL registered at GitHub App |
| `AS_SPA_ORIGINS` | Yes | SPA origin for credentialed CORS [AM2] |
| `AS_REDIRECT_URIS` | Yes | Registered client redirect_uri allowlist |
| `REGISTRATION_ENABLED` | No | Default: true; set false to close new registrations |

## CORS & Same-Registrable-Domain Mandate [AM2]

The AS and SPA **must** share a registrable domain not on the PSL. Example:
- AS: `auth.spawnery.dev`
- SPA: `app.spawnery.dev`

Cross-*site* placement (e.g. AS on a different TLD) breaks silent refresh under Safari ITP and
Firefox TCP (a perpetual-login loop). The `/refresh` cookie is `SameSite=Strict` and is only
sent by the browser to the AS origin.

## GitHub App Requirements

Register a GitHub App (not OAuth App) for the confidential-client leg [AM9]:
- Callback URL: `{AS_GITHUB_REDIRECT_URI}` (e.g., `https://auth.spawnery.dev/oauth/callback`)
- Required permission: `read:user`
- The GitHub App's client_secret is the load-bearing credential (not PKCE alone)
- Record: GitHub user-to-server tokens expire (~8h); the AS discards GitHub tokens after `GET /user`
  (not relevant to our session management)
