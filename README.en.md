[简体中文](README.md) ｜ [English](README.en.md)

# objbox

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A minimal self-hosted S3-compatible object store: **one account, one AK/SK pair, one isolated space**.
A single binary, no database, no middleware.

## Why build this

Platform-style projects such as MinIO, Garage and SeaweedFS offer full clustering, erasure coding,
replication and lifecycle management — at the cost of running a fleet of services, a pile of config,
and a non-trivial operational surface.

objbox fills only a thin layer of that space: **accounts + AK/SK + isolation**. It reuses the standard
library `net/http` and [gofakes3](https://github.com/johannesboyne/gofakes3) for routing/XML, performs
SigV4 verification itself, keeps metadata in an embedded bbolt database, and stores object bytes
directly on disk. It fits the "one machine, a few accounts, use rclone/aws cli/mc as a drive" case;
it does not aim to replace platform-style systems.

- Language: Go; HTTP via the standard library `net/http`
- S3 protocol layer: gofakes3 (routing/XML only, no auth)
- Auth: SigV4 header signing + presigned URLs, verified by recomputing with `aws-sdk-go-v2/aws/signer/v4`
- Metadata: bbolt (pure-Go embedded KV); object bytes go straight to disk

> All endpoints in this document use the placeholder `https://s3.example.com`; replace it with your own.

## Features

- **Account isolation**: each account has its own root directory; cross-account access and
  "bucket does not exist" return byte-for-byte identical 403 responses.
- **Automatic bucket creation**: one account per app, and creating an account creates its bucket;
  clients only need AK/SK — set Bucket to anything, or to the account name.
- **AK/SK auth**: standard SigV4 `Authorization` header plus presigned URLs for GET / PUT.
- **Read-only and disabled**: an account can be read-only (writes rejected) or disabled (all requests
  rejected), effective immediately.
- **Hot reload**: `accounts.json` is reloaded automatically; a parse failure keeps the previous table
  instead of emptying it.
- **S3 subset**: bucket/object CRUD, prefix and paged listing, batch delete, same-account CopyObject.
- **Presigned URLs**: up to 7 days, `Range` supported.
- **Multipart upload**: streamed to disk, concurrent parts, expired uploads cleaned on startup.
- **Quota**: per-account write byte limit, exceeding it returns `403 QuotaExceeded`.
- **Admin page**: embedded React page, with `AUTH_MODE=builtin` (self-managed password) or `sso`
  (trusts the gateway).
- **Single binary**: frontend build output is embedded via `//go:embed`; deploy one file plus one data dir.

## Quick start

### Build from source

```bash
make build                 # builds web/ first, then go build, producing ./objbox
./objbox serve -addr 127.0.0.1:18930 -data /var/lib/objbox
```

Account management (`accounts.json` mode 0600, contains the plaintext SK):

```bash
./objbox account add demo -note "example"    # name must match [a-z0-9][a-z0-9-]{0,31}
./objbox account list
./objbox account quota demo 1073741824       # 1GiB, 0 means unlimited
./objbox account rotate demo
./objbox account disable demo
./objbox account enable demo
./objbox account remove demo
```

Generate a presigned download link:

```bash
./objbox presign -account demo -bucket my-bucket -key path/to/file \
  -method GET -expires 3600 -endpoint https://s3.example.com
```

Open `http://127.0.0.1:18930/` in a browser for the admin page; on first start an admin password is
generated into `<data>/admin-password.txt` (0600).

### Docker

```bash
make docker                # docker build -t objbox:latest .
make docker-run            # foreground, port 18930, mounts ./data:/data
make image-size            # print image size
```

Or use Compose (see `docker-compose.yml`):

```bash
docker compose up -d
```

> **The data volume must be persisted**: the account table, metadata DB and object bytes all live under
> `/data`; recreating the container loses nothing only if `/data` is persisted. Compose mounts
> `./data` at `/data`.

## Supported S3 operations

Service level: `ListBuckets`

Bucket level: `CreateBucket` / `DeleteBucket` (rejects non-empty) / `HeadBucket` /
`ListObjects` (V1 and V2, with prefix, delimiter, paging) /
`DeleteObjects` (batch delete, up to 1000 keys) / `ListMultipartUploads`

Object level: `PutObject` / `GetObject` (single `Range`) / `HeadObject` / `DeleteObject` /
`CopyObject` (same account, `x-amz-copy-source`)

Multipart upload: `CreateMultipartUpload` / `UploadPart` / `ListParts` /
`CompleteMultipartUpload` / `AbortMultipartUpload`

Auth: SigV4 `Authorization` header; presigned URLs (GET / PUT, up to 7 days)

Quota: checked before writes when `quotaBytes > 0`, exceeding returns `403 QuotaExceeded`

**Explicitly not supported**: versioning (versioning/versions), object lock, lifecycle, bucket/object
ACL, bucket policy, event notifications, cross-region replication, STS temporary credentials, S3 Select,
plus public buckets / anonymous access, Virtual-Hosted-Style, server-side encryption, and non-STANDARD
storage classes. See [`docs/API.md`](docs/API.md) for the full list and error-code table.

## Usage examples

### rclone

```ini
# ~/.config/rclone/rclone.conf
[objbox]
type = s3
provider = Other
access_key_id = AKEXAMPLE...
secret_access_key = SKEXAMPLE...
endpoint = https://s3.example.com
region = us-east-1
force_path_style = true
```

```bash
rclone lsd objbox:                                   # list buckets
rclone mkdir objbox:my-bucket                        # create bucket
rclone copy ./local-dir objbox:my-bucket/remote-dir  # upload (large files auto-multipart)
rclone ls objbox:my-bucket                           # list objects
rclone copy objbox:my-bucket/remote-dir ./local-dir  # download
rclone delete objbox:my-bucket/remote-dir            # delete objects
rclone rmdir objbox:my-bucket                        # remove empty bucket
```

### aws cli

```bash
export AWS_ACCESS_KEY_ID=AKEXAMPLE...
export AWS_SECRET_ACCESS_KEY=SKEXAMPLE...
export AWS_DEFAULT_REGION=us-east-1
ENDPOINT=https://s3.example.com

aws --endpoint-url "$ENDPOINT" s3api list-buckets
aws --endpoint-url "$ENDPOINT" s3api create-bucket --bucket my-bucket
aws --endpoint-url "$ENDPOINT" s3 cp ./big.bin s3://my-bucket/big.bin        # auto multipart
aws --endpoint-url "$ENDPOINT" s3 ls s3://my-bucket/
aws --endpoint-url "$ENDPOINT" s3api get-object --bucket my-bucket --key big.bin out.bin
aws --endpoint-url "$ENDPOINT" s3 rm s3://my-bucket/big.bin
```

Presigned (generate with the objbox CLI, then open with curl/browser):

```bash
URL=$(./objbox presign -account demo -bucket my-bucket -key big.bin \
  -endpoint https://s3.example.com)
curl -o big.bin "$URL"
```

## Configuration

Environment variables:

| Variable | Values | Default | Description |
|---|---|---|---|
| `AUTH_MODE` | `builtin` \| `sso` | `builtin` | **Admin-plane only** auth; S3 endpoints always use AK/SK |

Common command-line flags (process startup flags, not environment variables):

| Flag | Default | Description |
|---|---|---|
| `serve -addr` | `127.0.0.1:18930` | HTTP listen address |
| `-data` | `/var/lib/objbox` | Data directory (account table, metadata DB, object bytes) |
| `serve -access-log` | `false` | Print access log, `Authorization` is always masked |

`AUTH_MODE` details:

- `builtin`: on first start a random admin password is generated and written in plaintext to
  `<data>/admin-password.txt` (0600); its bcrypt hash is stored in `<data>/admin.json`. The session
  cookie `objbox_admin` (`HttpOnly; SameSite=Lax; Path=/`, plus `Secure` over HTTPS, TTL 12 hours).
  Failed logins are rate-limited per source IP: at most 10 per minute.
- `sso`: no self-managed login; the admin plane only trusts the gateway-injected `X-Auth-User` header,
  and a missing header is always 401.

When `-data` is not given explicitly, the CLI prints "using default data directory X", and `account add`
also prints the directory actually used.

## Security model

- **Isolation rule**: each account has its own root; every object path passes through the single entry
  point `SafeJoin`, which rejects `..`, absolute paths and escaping symlinks. Cross-account and
  non-existent buckets both return 403, leaking no existence information.
- **Unified 403 semantics**: unknown AK, disabled account, cross-account, read-only write attempts and
  expired presigned URLs all return `403 AccessDenied` with a byte-for-byte identical body (no random
  fields), so they cannot be used to enumerate accounts or objects.
- **Plaintext SK trade-off**: SigV4 verification requires the server to hold the plaintext SK to
  recompute the HMAC chain, so the SK cannot be stored hashed only; it is written in plaintext to
  `accounts.json` (0600) and held in memory. Keep the data directory permissions tight and do not share
  that file in backups. Rotating an SK invalidates the old one immediately; the admin page masks SKs by
  default and only returns plaintext for an explicit, authenticated `?reveal=<name>`.
- **Recommendations**: use it only on your own machine or intranet; when exposed to the internet,
  always put it behind HTTPS (TLS terminated by a reverse proxy) and keep the data directory readable
  only by the service account.
- **`X-Forwarded-For` trust boundary**: login rate limiting prefers `X-Forwarded-For`/`X-Real-IP`
  (designed for reverse proxies). **When exposed to the internet directly, a front proxy must overwrite
  that header**, otherwise clients can forge an IP and bypass the login rate limit. nginx example:

  ```nginx
  location / {
      proxy_pass http://127.0.0.1:18930;
      proxy_set_header Host $host;
      proxy_set_header X-Forwarded-For $remote_addr;   # overwrite, do not append the client value
      proxy_set_header X-Forwarded-Proto $scheme;
  }
  ```

## License and contributing

This project is released under the [MIT License](LICENSE),
`Copyright (c) 2026 YLing2024`. Issues and PRs are welcome; see
[`CONTRIBUTING.md`](CONTRIBUTING.md) for development and commit conventions.
