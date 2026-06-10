# Dev Garage — transient-tier journal object store

Single-node [Garage](https://garagehq.deuxfleurs.fr/) for Spawnery's transient storage tier
(Kopia journal). Design:
[`docs/superpowers/specs/2026-06-10-transient-tier-kopia-journal-design.md`](../../docs/superpowers/specs/2026-06-10-transient-tier-kopia-journal-design.md)
(§1, T.6 — self-hosted S3-class sink; bucket-per-spawn + per-bucket key, §3).

**DEV ONLY.** `garage.toml` ships fixed, well-known `rpc_secret` / `admin_token` so the stack
works out of the box. Regenerate them for any non-local use.

## Bring up / tear down

```bash
just garage        # docker compose up -d  +  bootstrap.sh (layout + dev bucket/key)
just garage-down   # docker compose down -v  (also drops the data volumes)
```

Ports (bound to 127.0.0.1 via host networking):

| Port | Use |
|---|---|
| 3900 | S3 API — the Kopia `S3Backend` endpoint |
| 3901 | RPC (intra-cluster) |
| 3903 | Admin API — bucket/key bootstrap |

## Bucket / key bootstrap (Garage admin API)

Garage has no IAM/prefix policies, so isolation is **bucket-per-spawn + per-bucket access key**
(design §3, roast m3/M1). Both are minted over the **admin API** (`Authorization: Bearer <admin_token>`):

1. **Create an access key** — `POST /v1/key` `{"name": "..."}` → `{accessKeyId, secretAccessKey}`.
2. **Create a bucket** — `POST /v1/bucket` `{"globalAlias": "..."}` → `{id}`.
3. **Grant the key on the bucket** — `POST /v1/bucket/allow`
   `{"bucketId": "...", "accessKeyId": "...", "permissions": {"read": true, "write": true, "owner": true}}`.

A one-time **cluster layout** must be applied before any bucket/key ops (even single-node) —
`bootstrap.sh` does this (`garage layout assign` + `apply`) and then mints a default
`spawnery-journal` bucket + key, printing the S3 env the journaler consumes.

The `garage_e2e` test (`internal/storage/journal/blob_s3_e2e_test.go`) mints its **own** fresh
bucket+key per run via the same admin API, mirroring the per-spawn isolation.

## Run the live round-trip test

```bash
just garage   # prints the env below
GARAGE_S3_ENDPOINT=127.0.0.1:3900 \
GARAGE_ADMIN_ENDPOINT=http://127.0.0.1:3903 \
GARAGE_ADMIN_TOKEN=$(grep -E '^admin_token' deploy/garage/garage.toml | cut -d'"' -f2) \
go test -tags garage_e2e -run TestS3BackendRoundTripGarage -v ./internal/storage/journal/
```

Without `GARAGE_S3_ENDPOINT` the test **skips** — the hermetic `go test ./...` stays green and
never touches Garage.
