# MinIO Runtime Investigation — branch `minio_c07e5b49d477` (HEAD `c07e5b49d`)

**Repository:** `github.com/minio/minio`
**Branch:** `minio_c07e5b49d477`
**HEAD commit:** `c07e5b49d477b0774f23db3b290745aef8c01bd2`
**Investigation type:** Read-only, runtime-grounded. The MinIO source tree was treated as read-only evidence; the only persistent artifact produced is this report. Every conclusion below is backed by output actually observed at runtime (server trace lines, audit-log JSON, command output, error bodies, and hashes) and anchored to the specific function and `file:line` that produces the behavior at this HEAD.

This report answers five questions:

1. Policy evaluation vs. server-side-encryption precedence on an unencrypted upload.
2. The log entries emitted when someone deletes an object-locked object.
3. The runtime logs generated on a GET after manual on-disk bit-rot corruption.
4. Enforcement of an STS session policy on temporary credentials.
5. Proof that a basic user cannot self-promote to console admin by modifying user mappings, and the root cause.

Each requirement section follows the same ordering: **Direct Answer → Reproduction → Observed Output → Responsible Code → Rationale.** Any statement that could not be directly observed is explicitly labeled **[INFERRED]**.

> **Secret handling.** The captured output is reproduced verbatim except that bearer-credential/secret material is redacted with an obvious placeholder: STS `SecretAccessKey` values, the STS `SessionToken` JWT string, the KMS master-key bytes, and fixture user passwords are shown as `<REDACTED_…>`. SigV4 per-request `Signature=` values are one-time request signatures (not reusable credentials) and are left intact so the trace remains faithful. Decoded (non-secret) STS claims — including the session policy — are shown in full because they are the evidence.

---

## Environment & Build

All tooling lives outside the repository tree (`/tmp/...`); the MinIO source tree was treated as
read-only evidence and left byte-for-byte unchanged — the only file this investigation adds to the
repository is this report.

| Component | Value |
|-----------|-------|
| Toolchain | Go **1.23.12** `linux/amd64` (matches `go.mod` `go 1.23` and the CI `1.23.x` matrix in `.github/workflows/go*.yml`) |
| Server binary | `/tmp/minio-bin` — built from HEAD `c07e5b49d` with a plain `go build` (self-reports `DEVELOPMENT.GOGET`) |
| Admin/S3 CLI | `mc` `RELEASE.2025-08-13T08-35-41Z` at `/tmp/bin/mc` (its own runtime is `go1.24.6`; independent of the server) |
| S3/STS/admin client | `boto3` **1.43.47** / `botocore` 1.43.47 (SigV4), Python 3.13.7 |
| KMS | Built-in KMS via `MINIO_KMS_SECRET_KEY` (well-known MinIO CI test key from `.github/workflows/go.yml`) — enables SSE-S3/auto-encryption for R1 |
| Root credentials | `minioadmin:minioadmin` — used **only** to provision fixtures (buckets, users, policies, locks); never to trigger the deny-path behaviors |

### Toolchain and client versions (verbatim)

```text
$ go version
go version go1.23.12 linux/amd64

$ /tmp/bin/mc --version
mc version RELEASE.2025-08-13T08-35-41Z (commit-id=7394ce0dd2a80935aded936b09fa12cbb3cb8096)
Runtime: go1.24.6 linux/amd64
Copyright (c) 2015-2025 MinIO, Inc.
License GNU AGPLv3 <https://www.gnu.org/licenses/agpl-3.0.html>

$ /tmp/minio-bin --version
minio-bin version DEVELOPMENT.GOGET (commit-id=DEVELOPMENT.GOGET)
Runtime: go1.23.12 linux/amd64
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Copyright: 2015-0000 MinIO, Inc.
```

### Build command (verbatim)

```bash
CGO_ENABLED=0 GOFLAGS=-mod=mod go build -tags kqueue -o /tmp/minio-bin .
```

A plain `go build` (no ldflags) makes the binary self-report `Version: DEVELOPMENT.GOGET`, whereas the
canonical `make build` stamps a release version via `buildscripts/gen-ldflags.go`. **This distinction does
not affect any of the five behaviors under investigation** — all are request-path behaviors independent of
the version string. `mc` is a distinct client build whose own runtime (`go1.24.6`) is unrelated to the
server's (`go1.23.12`).

### Server invocations and startup banners (complete, unedited)

Two independent single-host servers were run concurrently on distinct ports and data directories so that
their behaviors and deployment IDs never collide.

**Single-drive server** — used for R1, R2, R4, R5. The KMS master key is the well-known MinIO CI test key
from `.github/workflows/go.yml`; its 32-byte base64 secret is redacted here:

```bash
MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
MINIO_KMS_SECRET_KEY="my-minio-key:<REDACTED_KMS_KEY_BASE64_32B>" \
/tmp/minio-bin server /tmp/miniodata --address :9000 --console-address :9001
```

Startup banner — the boot lines of `out/server9000.log`, complete and unedited. This instance's data
directory `/tmp/miniodata` pre-existed from an earlier run, so the server did **not** emit any `Formatting`
line (formatting happens only on a first, empty initialization):

```text
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.GOGET (go1.23.12 linux/amd64)

API: http://10.236.7.56:9000  http://172.17.0.1:9000  http://127.0.0.1:9000
WebUI: http://10.236.7.56:9001 http://172.17.0.1:9001 http://127.0.0.1:9001

Docs: https://docs.min.io
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
```

**Four-drive erasure-set server** — used for R3 heal-on-read; a multi-drive set is required so parity
reconstruction can occur (a single drive can detect corruption but cannot reconstruct):

```bash
/tmp/minio-bin server /tmp/minio4/d1 /tmp/minio4/d2 /tmp/minio4/d3 /tmp/minio4/d4 \
  --address :9100 --console-address :9101
```

Startup banner — complete and unedited `out/server9100.log`. Four fresh drives are formatted on this
first start, so the boot output begins with the `Formatting` and multi-drive-host warning lines:

```text
INFO: Formatting 1st pool, 1 set(s), 4 drives per set.
INFO: WARNING: Host local has more than 2 drives of set. A host failure will result in data becoming unavailable.
MinIO Object Storage Server
Copyright: 2015-2026 MinIO, Inc.
License: GNU AGPLv3 - https://www.gnu.org/licenses/agpl-3.0.html
Version: DEVELOPMENT.GOGET (go1.23.12 linux/amd64)

API: http://10.236.7.56:9100  http://172.17.0.1:9100  http://127.0.0.1:9100
WebUI: http://10.236.7.56:9101 http://172.17.0.1:9101 http://127.0.0.1:9101

Docs: https://docs.min.io
WARN: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
```

### Cluster state — `mc admin info` (complete, unedited)

Single-drive server (`EC:0`, one drive online):

```text
$ mc admin info inv9000
●  127.0.0.1:9000
   Uptime: 1 hour
   Version: <development>
   Network: 1/1 OK
   Drives: 1/1 OK
   Pool: 1

┌──────┬──────────────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage         │ Erasure stripe size │ Erasure sets │
│ 1st  │ 2.0% (total: 24 TiB) │ 1                   │ 1            │
└──────┴──────────────────────┴─────────────────────┴──────────────┘

1.0 MiB Used, 7 Buckets, 14 Objects, 4 Versions
1 drive online, 0 drives offline, EC:0
```

Four-drive erasure set (`EC:2` parity, four drives online — the precondition for R3 heal-on-read):

```text
$ mc admin info inv9100
●  127.0.0.1:9100
   Uptime: 1 hour
   Version: <development>
   Network: 1/1 OK
   Drives: 4/4 OK
   Pool: 1

┌──────┬──────────────────────┬─────────────────────┬──────────────┐
│ Pool │ Drives Usage         │ Erasure stripe size │ Erasure sets │
│ 1st  │ 2.0% (total: 48 TiB) │ 4                   │ 1            │
└──────┴──────────────────────┴─────────────────────┴──────────────┘

1.0 MiB Used, 1 Bucket, 1 Object
4 drives online, 0 drives offline, EC:2
```

### Health gate

Both servers returned `200` from the readiness probe before any behavior was triggered:

```bash
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9000/minio/health/ready
200
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9100/minio/health/ready
200
```

### Observation harness, isolation, and privilege model

All fixtures, scripts, and captured evidence live under one scratch root that is deleted on completion
(see the Cleanup section); nothing is written into the repository tree.

```text
/tmp/blitzy_investigation/
├── policies/     # IAM identity-policy and bucket-policy JSON fixtures (inlined per requirement)
├── scripts/      # reproduction drivers (inlined per requirement) + audit_receiver.py (below)
├── mc-config/    # dedicated MC_CONFIG_DIR — mc aliases live here, not in $HOME
└── out/          # captured trace/audit/log evidence and the two server logs
```

| Setting | Value |
|---------|-------|
| `MC_CONFIG_DIR` | `/tmp/blitzy_investigation/mc-config` (isolates `mc` aliases from any host config) |
| `mc` binary | `/tmp/bin/mc` |
| Aliases | `inv9000` → single-drive (root), `inv9100` → 4-drive (root), `r5alias` → authenticates **as the non-admin user `r5basic`** (R5) |

**Server isolation.** The two servers never share state; each behavior is triggered against exactly one:

| Server | API / console | Data dir | Erasure | Deployment ID | Used by |
|--------|---------------|----------|---------|---------------|---------|
| single-drive | `:9000` / `:9001` | `/tmp/miniodata` | `EC:0` (1 drive) | `b3de8c20-51f3-4954-9a06-c02b2323ff40` | R1, R2, R4, R5 |
| 4-drive erasure | `:9100` / `:9101` | `/tmp/minio4/d{1..4}` | `EC:2` (4 drives) | `b12d77b7-4184-4401-9de1-9aae313fe017` | R3 |

**Privilege model.** Root (`minioadmin:minioadmin`) is used **only** to build fixtures. Every deny-path
behavior in R4 and R5 is triggered by a **non-admin principal** — STS temporary credentials scoped by an
inline session policy (R4), or the basic user `r5basic` whose identity policy grants no admin action (R5).
This separation is what makes the observed `AccessDenied` results meaningful rather than artifacts of
using an unprivileged-by-omission caller.

**Audit capture.** MinIO audit records are delivered to a minimal local webhook sink that appends each
record as one compact JSON object per line (true JSONL). The webhook target is configured on the server
and the sink is started before the R2/R5 behaviors are triggered:

```bash
# 1) register the audit webhook target (root)
mc admin config set inv9000 audit_webhook:inv endpoint=http://127.0.0.1:9999

# 2) restart so the new target takes effect. `mc admin service restart` needs a
#    TTY in this non-interactive shell, so the server was stopped and relaunched
#    with the identical invocation shown above, then re-probed on /minio/health/ready.

# 3) start the local sink BEFORE triggering the R2/R5 behaviors
python3 /tmp/blitzy_investigation/scripts/audit_receiver.py \
  /tmp/blitzy_investigation/out/audit.jsonl
```

`scripts/audit_receiver.py` (complete, unedited):

```python
#!/usr/bin/env python3
"""Minimal audit-log webhook sink.

Listens on 127.0.0.1:9999 and appends each received JSON audit record, one
compact record per line (true JSONL), to the path given as argv[1].
Used to capture MinIO audit-log entries for R2 (object-lock delete) and
R5 (privilege-escalation) without any external dependency.
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

OUT = sys.argv[1] if len(sys.argv) > 1 else "/tmp/blitzy_investigation/out/audit.jsonl"


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length) if length else b""
        try:
            rec = json.loads(body)
            line = json.dumps(rec, separators=(",", ":"))
        except Exception:
            line = body.decode("utf-8", "replace").strip()
        with open(OUT, "a") as f:
            f.write(line + "\n")
        self.send_response(200)
        self.end_headers()

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", 9999), Handler).serve_forever()
```

> **Transparency note (applies to every captured block in this report).** All command output is
> reproduced verbatim, with two mechanical, uniformly-applied exceptions: **(1)** trailing whitespace is
> trimmed per line so `git diff --check` stays clean; **(2)** audit-log records pass through the webhook
> sink above, which parses each record and re-serializes it — as compact JSON in the JSONL captures, or
> pretty-printed via `python3 -m json.tool` in the per-requirement excerpts. Field names and values are
> preserved exactly; only insignificant JSON whitespace is normalized. Bearer/secret material is redacted
> as described in the header note; SigV4 per-request `Signature=` values are one-time and left intact.

---

## R1 — Policy evaluation vs. server-side-encryption precedence

> **Question (verbatim):** "I am investigating Minio's implementation of policy evaluation logic and server side encryption. I wonder what happens when a bucket level encryption requirement takes precedence over a user's broad write permissions during an unencrypted upload. Identify the specific runtime execution sequence captured in the server trace logs."

### Direct Answer

**The bucket's encryption *configuration* governs how the object is stored; the user's broad write grant only governs *whether* the write is admitted — they are orthogonal.** Three distinct conditions were exercised on the canonical write path:

- **[Observed] Auto-encryption takes precedence (primary).** A non-admin user holding a broad `s3:PutObject` grant uploaded an object with **no** SSE header to a bucket whose default encryption is SSE-S3. The write was admitted (`200 OK`) and the object was **stored SSE-S3-encrypted** (`mc stat` → `Encryption: SSE-S3`; response `X-Amz-Server-Side-Encryption: AES256`). The `X-Amz-Server-Side-Encryption: AES256` that appears in the traced *request* is **not** in the client's SigV4 `SignedHeaders` list — i.e. the client never sent it — so it was injected server-side. The broad write grant did **not** let the user store plaintext.
- **[Observed] Identity-policy denial (alternate).** When the "encryption requirement" is expressed as an **IAM identity policy** that Denies `s3:PutObject` unless the SSE header is present, the same header-less PUT is rejected at authorization with **HTTP 403 `AccessDenied`**; the paired PUT that *does* carry the header succeeds (`200`).
- **[Observed] Resource (bucket) policy denial applies to anonymous requests only.** A bucket policy carrying the identical `Deny`-if-no-SSE condition denies an **anonymous** header-less PUT (`403`) but is **not consulted** for an **authenticated** principal: an authenticated user allowed by its identity policy stored a header-less object **unencrypted** (`200`, `ServerSideEncryption=None`). **The AAP-directed *authenticated* bucket-policy `AccessDenied` therefore did NOT reproduce** — see the auth-vs-anon split under *Responsible Code*. **[Source-grounded]**

### Reproduction

All fixtures are provisioned with the root alias `inv9000`; every *trigger* runs as a non-admin user. Scratch dir `/tmp/blitzy_investigation` (`OUT=$INV/out/r1`), `MC=/tmp/bin/mc`, `MC_CONFIG_DIR=/tmp/blitzy_investigation/mc-config`. Trace captures are backgrounded with an explicit PID and stopped after the trigger.

Identity policies (`$INV/policies/`):

`r1writepolicy.json` — broad write, no admin action:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject", "s3:ListBucket", "s3:GetBucketLocation"],
      "Resource": ["arn:aws:s3:::r1bucket", "arn:aws:s3:::r1bucket/*"]
    }
  ]
}
```

`r1denypolicy.json` — Allow + explicit Deny-if-no-SSE (IAM identity policy):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject", "s3:ListBucket"],
      "Resource": ["arn:aws:s3:::r1denybucket", "arn:aws:s3:::r1denybucket/*"]
    },
    {
      "Effect": "Deny",
      "Action": ["s3:PutObject"],
      "Resource": ["arn:aws:s3:::r1denybucket/*"],
      "Condition": {"Null": {"s3:x-amz-server-side-encryption": "true"}}
    }
  ]
}
```

`r1bucketpolicy.json` — resource/bucket policy (`Principal:*`) Allow + Deny-if-no-SSE:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {"AWS": ["*"]},
      "Action": ["s3:GetObject", "s3:PutObject", "s3:ListBucket"],
      "Resource": ["arn:aws:s3:::r1bucketpol", "arn:aws:s3:::r1bucketpol/*"]
    },
    {
      "Effect": "Deny",
      "Principal": {"AWS": ["*"]},
      "Action": ["s3:PutObject"],
      "Resource": ["arn:aws:s3:::r1bucketpol/*"],
      "Condition": {"Null": {"s3:x-amz-server-side-encryption": "true"}}
    }
  ]
}
```

`r1polusrpolicy.json` — plain Allow on `r1bucketpol` for the authenticated-bypass sub-case:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject", "s3:ListBucket"],
      "Resource": ["arn:aws:s3:::r1bucketpol", "arn:aws:s3:::r1bucketpol/*"]
    }
  ]
}
```

Fixture provisioning:

```bash
$MC mb -p inv9000/r1bucket inv9000/r1denybucket inv9000/r1bucketpol
$MC encrypt set sse-s3 inv9000/r1bucket            # -> "Auto encryption 'sse-s3' is enabled"
$MC encrypt info inv9000/r1bucket                  # readback
$MC admin policy create inv9000 r1write     $INV/policies/r1writepolicy.json
$MC admin policy create inv9000 r1denypol   $INV/policies/r1denypolicy.json
$MC admin policy create inv9000 r1polusrpol $INV/policies/r1polusrpolicy.json
$MC admin user add inv9000 r1user     <REDACTED_USER_SECRET>
$MC admin user add inv9000 r1denyuser <REDACTED_USER_SECRET>
$MC admin user add inv9000 r1polusr   <REDACTED_USER_SECRET>
$MC admin policy attach inv9000 r1write     --user r1user
$MC admin policy attach inv9000 r1denypol   --user r1denyuser
$MC admin policy attach inv9000 r1polusrpol --user r1polusr
# resource/bucket policy on r1bucketpol (consulted for anonymous requests):
$MC anonymous set-json $INV/policies/r1bucketpolicy.json inv9000/r1bucketpol
```

Deterministic payload (no trailing newline) and its locally-computed byte facts:

```bash
printf '%s' 'R1-auto-encrypt-precedence-demo' > $OUT/payload_1A.bin
```

```
size_bytes=31
content=R1-auto-encrypt-precedence-demo
sha256=d6658aef802c6cf84b093a3c5d144a164377807396cb0dfe46f5fe07360ffcee
md5_hex=773003aef21369c600f92e2e457e1b2f
sha256_b64=1mWK74AsbPhLCTo8XRRKFkN3gHOWyw3+RvX+BzYP/O4=
md5_b64=dzADrvITacYA+S4uRX4bLw==
crc32_b64=rl02Jg==
```

PUT driver `$INV/scripts/r1_boto_put.py` (non-streaming, whole-body-signed so `X-Amz-Content-Sha256` equals the real SHA-256 of the exact bytes; sends an SSE header only when the optional 6th arg is given):

```python
#!/usr/bin/env python3
"""R1 boto3 PUT driver.

Performs a single, non-streaming (whole-body-signed) PutObject as a given
user with NO SSE header, so the on-the-wire X-Amz-Content-Sha256 equals the
real SHA-256 of the exact bytes uploaded. Prints the request/response facts
needed for byte-consistency verification.

Usage: r1_boto_put.py ACCESS SECRET BUCKET KEY PAYLOAD_FILE [SSE]
  SSE optional: if 'AES256' the client sends ServerSideEncryption=AES256.
"""
import sys, hashlib, boto3
from botocore.config import Config

access, secret, bucket, key, payload_file = sys.argv[1:6]
sse = sys.argv[6] if len(sys.argv) > 6 else None

body = open(payload_file, "rb").read()

# Force SigV4 + path style; disable automatic request checksums so this is a
# plain whole-body-signed PUT (X-Amz-Content-Sha256 = sha256(body)).
cfg = Config(
    signature_version="s3v4",
    s3={"addressing_style": "path", "payload_signing_enabled": True},
    request_checksum_calculation="when_required",
    response_checksum_validation="when_required",
)
s3 = boto3.client(
    "s3", endpoint_url="http://127.0.0.1:9000",
    aws_access_key_id=access, aws_secret_access_key=secret,
    region_name="us-east-1", config=cfg,
)

kwargs = dict(Bucket=bucket, Key=key, Body=body)
if sse:
    kwargs["ServerSideEncryption"] = sse

try:
    resp = s3.put_object(**kwargs)
    md = resp["ResponseMetadata"]
    print("PUT_STATUS =", md["HTTPStatusCode"])
    print("ETag       =", resp.get("ETag"))
    print("SSE_resp   =", resp.get("ServerSideEncryption"))
    print("local_size =", len(body))
    print("local_sha256 =", hashlib.sha256(body).hexdigest())
    print("local_md5    =", hashlib.md5(body).hexdigest())
except Exception as e:
    print("PUT_ERROR_TYPE =", type(e).__name__)
    # Botocore ClientError carries the S3 error code/status
    resp = getattr(e, "response", None)
    if resp:
        err = resp.get("Error", {})
        print("S3_Code    =", err.get("Code"))
        print("S3_Message =", err.get("Message"))
        print("HTTPStatus =", resp.get("ResponseMetadata", {}).get("HTTPStatusCode"))
    else:
        print("raw:", e)
```

Trigger sequence for each scenario (illustrated for 1A; 1B/1C differ only in user, bucket, and the presence of the SSE arg / anonymity):

```bash
nohup $MC admin trace -v --funcname "s3.PutObject" inv9000 > $OUT/trace_1A_boto.txt 2>&1 &
TPID=$!; sleep 2.5
python3 $INV/scripts/r1_boto_put.py r1user <REDACTED_USER_SECRET> r1bucket auto-encrypt-1A-boto $OUT/payload_1A.bin
sleep 3; kill "$TPID"; wait "$TPID" 2>/dev/null
```

### Observed Output

**Scenario 1A — auto-encryption precedence (primary). [Observed]**

boto3 non-streaming PUT as `r1user`, **no** SSE argument. The client result:

```
PUT_STATUS = 200
ETag       = "773003aef21369c600f92e2e457e1b2f"
SSE_resp   = AES256
local_size = 31
local_sha256 = d6658aef802c6cf84b093a3c5d144a164377807396cb0dfe46f5fe07360ffcee
local_md5    = 773003aef21369c600f92e2e457e1b2f
```

The complete, unedited `s3.PutObject` verbose trace (`$OUT/trace_1A_boto.txt`). The request's `Authorization` `SignedHeaders=host;x-amz-content-sha256;x-amz-date` contains **no** `x-amz-server-side-encryption`, yet the request block lists `X-Amz-Server-Side-Encryption: AES256` — proof it was injected server-side (SigV4 signs an immutable header set at the client, so an unsigned header cannot have come from the client):

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T20:40:04.295] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /r1bucket/auto-encrypt-1A-boto
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Content-Length: 31
127.0.0.1:9000 Expect: 100-continue
127.0.0.1:9000 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/e,a,c,D,N cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9000 X-Amz-Content-Sha256: d6658aef802c6cf84b093a3c5d144a164377807396cb0dfe46f5fe07360ffcee
127.0.0.1:9000 X-Amz-Date: 20260714T204004Z
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r1user/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=9f2cde83cf716c52a1ccef7d9b6878d140ca6273fb25e5c522d8be119ca90cbe
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 Amz-Sdk-Invocation-Id: 31b4e866-8153-4cb8-a167-8d233c943e95
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T20:40:04.299] [ Duration 3.9ms TTFB 3.840678ms ↑ 198 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 ETag: "773003aef21369c600f92e2e457e1b2f"
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C24293A5715852
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 <BLOB>
127.0.0.1:9000
```

Byte-consistency (every value below is produced by the *same* 31-byte payload and appears in the capture above): request `Content-Length: 31` = local size `31`; request `X-Amz-Content-Sha256: d6658aef802c6cf84b093a3c5d144a164377807396cb0dfe46f5fe07360ffcee` = local `sha256`; response `ETag: "773003aef21369c600f92e2e457e1b2f"` = local `md5`.

A second, independent capture with a different client (`mc cp`, which uses a **streaming** signature) confirms stability. Here `X-Amz-Decoded-Content-Length: 31` carries the true payload size and the response `ETag` is again the plaintext md5; the request again shows the server-injected `X-Amz-Server-Side-Encryption: AES256` absent from the streaming `SignedHeaders` (`$OUT/trace_1A.txt`):

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T20:38:31.976] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /r1bucket/auto-encrypt-1A
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r1user/20260714/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length,Signature=7e4ac282293ee76379a2027f407bab211c812b017cb95fa75ed6758c3798ac52
127.0.0.1:9000 Content-Length: 204
127.0.0.1:9000 Content-Type: application/octet-stream
127.0.0.1:9000 X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD
127.0.0.1:9000 X-Amz-Decoded-Content-Length: 31
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) minio-go/v7.0.90 mc/RELEASE.2025-08-13T08-35-41Z
127.0.0.1:9000 X-Amz-Date: 20260714T203831Z
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T20:38:31.980] [ Duration 4.169ms TTFB 4.148212ms ↑ 368 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 X-Amz-Request-Id: 18C2427E26D0074C
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 ETag: "773003aef21369c600f92e2e457e1b2f"
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 <BLOB>
127.0.0.1:9000
```

The stored object is encrypted at rest, independent of the unencrypted upload (`$OUT/stat_1A.txt`):

```
Name      : auto-encrypt-1A
Date      : 2026-07-14 20:38:31 UTC
Size      : 31 B
ETag      : 773003aef21369c600f92e2e457e1b2f
Type      : file
Encryption: SSE-S3
Metadata  :
  Content-Type: application/octet-stream
```

**Scenario 1B — IAM identity-policy Deny-if-no-SSE (alternate). [Observed]**

Sub-case (A): `r1denyuser` PUT to `r1denybucket` **without** an SSE header — client result then the complete `s3.PutObject` trace (note the `403 Forbidden` and the full `AccessDenied` XML body, `$OUT/trace_1B_nosse.txt`):

```
PUT_ERROR_TYPE = AccessDenied
S3_Code    = AccessDenied
S3_Message = Access Denied.
HTTPStatus = 403
```

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T20:40:34.153] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /r1denybucket/deny-obj-nosse
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/D,c,a,e,N cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9000 X-Amz-Content-Sha256: d6658aef802c6cf84b093a3c5d144a164377807396cb0dfe46f5fe07360ffcee
127.0.0.1:9000 X-Amz-Date: 20260714T204034Z
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r1denyuser/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=f0880eab7fed06a394265a607055f08105c7ab2f17442d51043f95c1b011a7cb
127.0.0.1:9000 Expect: 100-continue
127.0.0.1:9000 Amz-Sdk-Invocation-Id: 230e0ab7-c3f4-4066-ab41-f4a587dfd42a
127.0.0.1:9000 Content-Length: 31
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T20:40:34.153] [ Duration 266µs TTFB 230.608µs ↑ 138 B  ↓ 343 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Request-Id: 18C2429A991C0F2A
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 Content-Length: 343
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>deny-obj-nosse</Key><BucketName>r1denybucket</BucketName><Resource>/r1denybucket/deny-obj-nosse</Resource><RequestId>18C2429A991C0F2A</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
127.0.0.1:9000
```

Sub-case (B): the *same* user, bucket, and payload **with** `ServerSideEncryption=AES256` — now `x-amz-server-side-encryption` is in `SignedHeaders` (client-sent) and the Deny condition no longer matches, so the write succeeds (`$OUT/trace_1B_withsse.txt`):

```
PUT_STATUS = 200
ETag       = "773003aef21369c600f92e2e457e1b2f"
SSE_resp   = AES256
local_size = 31
local_sha256 = d6658aef802c6cf84b093a3c5d144a164377807396cb0dfe46f5fe07360ffcee
local_md5    = 773003aef21369c600f92e2e457e1b2f
```

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T20:40:50.439] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /r1denybucket/deny-obj-withsse
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 X-Amz-Date: 20260714T204050Z
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 Content-Length: 31
127.0.0.1:9000 Expect: 100-continue
127.0.0.1:9000 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/e,D,N,c,a cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9000 X-Amz-Content-Sha256: d6658aef802c6cf84b093a3c5d144a164377807396cb0dfe46f5fe07360ffcee
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Amz-Sdk-Invocation-Id: afca1e8a-8cae-40fd-871c-430a63d47eb7
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r1denyuser/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-server-side-encryption, Signature=074b96e35137f506c4b24af1f7c0d61483ab3522ecab58694f5e11850a229a61
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T20:40:50.442] [ Duration 3.046ms TTFB 2.995689ms ↑ 198 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 ETag: "773003aef21369c600f92e2e457e1b2f"
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C2429E63D9D032
127.0.0.1:9000 <BLOB>
127.0.0.1:9000
```

**Scenario 1C — resource/bucket policy: anonymous-only enforcement. [Observed]**

The bucket policy on `r1bucketpol` as read back from the server:

```json
{"Statement":[{"Action":["s3:GetObject","s3:ListBucket","s3:PutObject"],"Effect":"Allow","Principal":{"AWS":["*"]},"Resource":["arn:aws:s3:::r1bucketpol","arn:aws:s3:::r1bucketpol/*"]},{"Action":["s3:PutObject"],"Condition":{"Null":{"s3:x-amz-server-side-encryption":[true]}},"Effect":"Deny","Principal":{"AWS":["*"]},"Resource":["arn:aws:s3:::r1bucketpol/*"]}],"Version":"2012-10-17"}
```

Sub-case (A): **anonymous** PUT (`curl`, no credentials), **no** SSE header — denied by the bucket policy. Client (`curl -i`) then the complete trace (`$OUT/trace_1C_anon_nosse.txt`):

```
HTTP/1.1 403 Forbidden
Accept-Ranges: bytes
Content-Length: 333
Content-Type: application/xml
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C242AB12E631A8
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 1140320
X-Ratelimit-Remaining: 1140320
X-Xss-Protection: 1; mode=block
Date: Tue, 14 Jul 2026 20:41:44 GMT

<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>anon-nosse</Key><BucketName>r1bucketpol</BucketName><Resource>/r1bucketpol/anon-nosse</Resource><RequestId>18C242AB12E631A8</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
```

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T20:41:44.916] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /r1bucketpol/anon-nosse
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Accept: */*
127.0.0.1:9000 Content-Length: 31
127.0.0.1:9000 Content-Type: application/x-www-form-urlencoded
127.0.0.1:9000 User-Agent: curl/8.14.1
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T20:41:44.916] [ Duration 227µs TTFB 183.974µs ↑ 51 B  ↓ 333 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 333
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C242AB12E631A8
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>anon-nosse</Key><BucketName>r1bucketpol</BucketName><Resource>/r1bucketpol/anon-nosse</Resource><RequestId>18C242AB12E631A8</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
127.0.0.1:9000
```

Sub-case (B): **anonymous** PUT **with** the SSE header — allowed (Deny condition false):

```
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 0
ETag: "773003aef21369c600f92e2e457e1b2f"
Server: MinIO
Strict-Transport-Security: max-age=31536000; includeSubDomains
Vary: Origin
Vary: Accept-Encoding
X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
X-Amz-Request-Id: 18C242AF2608AC87
X-Amz-Server-Side-Encryption: AES256
X-Content-Type-Options: nosniff
X-Ratelimit-Limit: 1140320
X-Ratelimit-Remaining: 1140320
X-Xss-Protection: 1; mode=block
Date: Tue, 14 Jul 2026 20:42:02 GMT
```

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T20:42:02.416] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /r1bucketpol/anon-withsse
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Content-Length: 31
127.0.0.1:9000 Content-Type: application/x-www-form-urlencoded
127.0.0.1:9000 User-Agent: curl/8.14.1
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 Accept: */*
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T20:42:02.420] [ Duration 3.87ms TTFB 3.819306ms ↑ 111 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C242AF2608AC87
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 ETag: "773003aef21369c600f92e2e457e1b2f"
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Server-Side-Encryption: AES256
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <BLOB>
127.0.0.1:9000
```

Sub-case (C): **authenticated** `r1polusr` PUT (allowed by its identity policy), **no** SSE header. The write **succeeds and the object is stored unencrypted** (`SSE_resp = None`), demonstrating the bucket policy's `Deny`-if-no-SSE was **not consulted** for the authenticated principal (`$OUT/trace_1C_auth_nosse.txt`):

```
PUT_STATUS = 200
ETag       = "773003aef21369c600f92e2e457e1b2f"
SSE_resp   = None
local_size = 31
local_sha256 = d6658aef802c6cf84b093a3c5d144a164377807396cb0dfe46f5fe07360ffcee
local_md5    = 773003aef21369c600f92e2e457e1b2f
```

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T20:42:08.192] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /r1bucketpol/auth-nosse
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Amz-Sdk-Invocation-Id: 5c264599-a093-460e-abcd-f8d5e2d02425
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r1polusr/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=b1c0de3d958b0d233a4b2cbd57d472429022f3a438e97bc244d4a757e440759a
127.0.0.1:9000 X-Amz-Content-Sha256: d6658aef802c6cf84b093a3c5d144a164377807396cb0dfe46f5fe07360ffcee
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Content-Length: 31
127.0.0.1:9000 Expect: 100-continue
127.0.0.1:9000 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/c,N,D,a,e cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9000 X-Amz-Date: 20260714T204208Z
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T20:42:08.198] [ Duration 6.237ms TTFB 6.173856ms ↑ 169 B  ↓ 0 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 ETag: "773003aef21369c600f92e2e457e1b2f"
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 X-Amz-Request-Id: 18C242B07E497A0C
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <BLOB>
127.0.0.1:9000
```

### Responsible Code

- **Write path / SSE application — [Observed effect + Source-grounded mechanism].** `PutObjectHandler` at `cmd/object-handlers.go:1745`. After authorization, the handler fetches the bucket default-encryption config and applies it to the request header at `cmd/object-handlers.go:1893-1897`:
  ```go
  // Check if bucket encryption is enabled
  sseConfig, _ := globalBucketSSEConfigSys.Get(bucket)
  sseConfig.Apply(r.Header, sse.ApplyOptions{
      AutoEncrypt: globalAutoEncryption,
  })
  ```
- **The injection itself** — `BucketSSEConfig.Apply` at `internal/bucket/encryption/bucket-sse-config.go:135`. It first returns early if the client already requested SSE (`if crypto.Requested(headers) { return }`, `:136-138`) — which is why a client-sent header (1B-B, 1C-B) is preserved — and otherwise, for an SSE-S3 (`AES`) bucket config, executes `headers.Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionAES)` at `:147-148`. This is the exact `X-Amz-Server-Side-Encryption: AES256` seen on the traced request, set independent of the user's write grant.
- **Why the trace shows a header the client never sent — [Source-grounded].** `httpTracerMiddleware` at `cmd/http-tracer.go:69` calls the handler first — `h.ServeHTTP(respRecorder, r)` at `:89` — and only *afterwards* clones the (now handler-mutated) request headers for the trace: `reqHeaders := r.Header.Clone()` at `:103`, attached as `Headers: reqHeaders` at `:153`. Consequently the traced request block reflects **post-handler** headers, so the injected SSE header appears in the request. The immutable client-side `SignedHeaders` list is what proves the client did not send it.
- **Auth-vs-anon policy split (why the AAP-directed authenticated bucket-policy denial did not reproduce) — [Source-grounded].** `checkRequestAuthType` at `cmd/auth-handler.go:339` (reached from `getRequestAuthType` at `:124`) evaluates a **resource/bucket** policy via `globalPolicySys.IsAllowed(...)` **only inside the anonymous branch** guarded by `if action != policy.ListAllMyBucketsAction && cred.AccessKey == ""` at `:432` (calls at `:434` and `:450`). An **authenticated** request (non-empty `cred.AccessKey`) skips that entire branch and is evaluated against its **identity** policy via `globalIAMSys.IsAllowed(...)` at `:481`. Because `r1polusr`'s identity policy plainly allows the PUT and does not carry the SSE condition, the header-less authenticated write is admitted and the bucket policy's `Deny` is never consulted — exactly what 1C-C shows.
- **Policy evaluation core** — `IAMSys.IsAllowed` at `cmd/iam.go:2437` (deny-by-default; an explicit `Deny` overrides an `Allow`), the evaluation used for the 1B identity-policy denial.
- **Global auto-encryption toggle** — `EnvKMSAutoEncryption = "MINIO_KMS_AUTO_ENCRYPTION"` and `LookupAutoEncryption()` in `internal/crypto/auto-encryption.go` populate `globalAutoEncryption`. In this run `MINIO_KMS_AUTO_ENCRYPTION` was **unset**, so the injection came from the **bucket's SSE-S3 default rule** (the `AES` switch arm at `bucket-sse-config.go:147-148`), not the global toggle (the `b == nil && AutoEncrypt` arm at `:140-141`).
- **Trace emission** — `httpTracerMiddleware` at `cmd/http-tracer.go:69`, registered in `globalMiddlewares` (`cmd/routers.go:54`/`:60`) and applied via `router.Use(...)` at `cmd/routers.go:112`.

### Rationale

**[Observed]** The user's broad `s3:PutObject` grant determines only *whether the write is admitted*; the bucket's encryption *configuration* determines *how the object is stored*. In 1A the grant admitted the write and the SSE-S3 default rule caused the object to be stored encrypted (`mc stat` → `Encryption: SSE-S3`) even though the client sent no SSE header — the broad write permission conferred no ability to store plaintext.

**[Source-grounded]** The captured request-header ordering is a property of the tracer, not of the client: because `httpTracerMiddleware` clones `r.Header` **after** `h.ServeHTTP` (`cmd/http-tracer.go:89` then `:103`), the server-injected `X-Amz-Server-Side-Encryption: AES256` (set by `BucketSSEConfig.Apply`, `bucket-sse-config.go:147-148`) is visible in the traced request. The runtime signal that the header originated server-side is the SigV4 `SignedHeaders` list, which omits it in 1A but includes it in 1B-B/1C-B.

**[Observed]** The two other senses of "encryption requirement takes precedence" behave differently by policy *type*: an **identity** policy conditioned on the SSE header denies the header-less PUT at authorization (`403 AccessDenied`, 1B-A) and admits it once the header is present (1B-B); a **resource/bucket** policy with the identical condition denies only **anonymous** header-less writes (1C-A) and is bypassed for authenticated principals (1C-C, stored unencrypted). **The AAP-directed authenticated bucket-policy `AccessDenied` did not reproduce**, and per the read-only investigation constraint this is reported as observed rather than remediated; the reproducible authenticated denial is the identity-policy path (1B).

---

## R2 — Object-lock delete log entries

> **Question (verbatim):** "I wonder when object locking on a bucket is enabled, what are the specific log entries that appear when someone tries to delete the locked objects? I want you to give me runtime log output to show this."

### Direct Answer

**[Observed]** When object locking is enabled and a caller attempts to permanently delete a **locked object version**, the specific log entries that appear are (a) a **server trace** entry `s3.DeleteObject … <status>` and (b) an **audit-log** record with `api.name=DeleteObject` and the matching `statusCode`, `status`, and authenticated `accessKey`. The delete is refused with one of two distinct results depending on the lock:

- **Retention (Compliance) and Legal Hold, and Governance without a bypass** → **HTTP 400** `<Code>InvalidRequest</Code>` / `<Message>Object is WORM protected and cannot be overwritten</Message>`; audit `statusCode=400`, `status="Bad Request"`.
- **Governance with `x-amz-bypass-governance-retention: true` but WITHOUT the `s3:BypassGovernanceRetention` grant** → **HTTP 403** `AccessDenied`; audit `statusCode=403`, `status="Forbidden"` (a different code path — see *Responsible Code*).
- **Governance with the bypass header AND the grant** → **HTTP 204 No Content**, the version is deleted; audit `statusCode=204`, `status="No Content"`.

**[Observed]** The WORM/`AccessDenied` denials are client-facing 4xx results; they do **not** appear as server-error entries in `mc admin logs` (shown below). The "specific log entries" the question asks about are therefore the **trace** and **audit** records, not the server error log.

### Reproduction

`OUT=$INV/out/r2`, `MC=/tmp/bin/mc`, `MC_CONFIG_DIR=$INV/mc-config`. An audit webhook (target `audit_webhook:inv` → local receiver on `127.0.0.1:9999`) writes one JSON record per line to `$INV/out/audit.jsonl`; `audit_receiver.py` is inlined in the *Environment & Build* section. Object-lock enforcement only triggers on a **version-specific** DELETE (a versionless DELETE merely writes a delete-marker), so every trigger targets an explicit `versionId`.

Fixtures (locks provisioned as root; every *delete trigger* runs as a non-admin user):

`r2bypasspolicy.json` — delete + `s3:BypassGovernanceRetention`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject", "s3:ListBucket", "s3:DeleteObject", "s3:DeleteObjectVersion", "s3:BypassGovernanceRetention", "s3:GetBucketVersioning", "s3:GetObjectRetention", "s3:PutObjectRetention"],
      "Resource": ["arn:aws:s3:::r2bucket", "arn:aws:s3:::r2bucket/*"]
    }
  ]
}
```

`r2nobypasspolicy.json` — delete, **no** bypass action:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject", "s3:ListBucket", "s3:DeleteObject", "s3:DeleteObjectVersion", "s3:GetObjectRetention"],
      "Resource": ["arn:aws:s3:::r2bucket", "arn:aws:s3:::r2bucket/*"]
    }
  ]
}
```

```bash
$MC mb --with-lock inv9000/r2bucket
$MC admin policy create inv9000 r2bypass   $INV/policies/r2bypasspolicy.json
$MC admin policy create inv9000 r2nobypass $INV/policies/r2nobypasspolicy.json
$MC admin user add inv9000 r2bypassuser   <REDACTED_USER_SECRET>
$MC admin user add inv9000 r2nobypassuser <REDACTED_USER_SECRET>
$MC admin policy attach inv9000 r2bypass   --user r2bypassuser
$MC admin policy attach inv9000 r2nobypass --user r2nobypassuser
# PUT 5 objects (root) and record each VersionId (via r2_delete_locked.py putobj)
# apply locks (root, canonical mc): legal hold ON / compliance 3d / governance 3d
$MC legalhold set inv9000/r2bucket/c1_legalhold  --vid <VID>
$MC retention set compliance 3d inv9000/r2bucket/c2_compliance --vid <VID>
$MC retention set governance 3d inv9000/r2bucket/c3_gov --vid <VID>   # (c4_gov, c5_gov likewise)
```

Driver `$INV/scripts/r2_delete_locked.py` (canonical boto3 SigV4; `delver` issues the version-specific DELETE, optionally setting the governance-bypass header):

```python
#!/usr/bin/env python3
"""R2 object-lock driver (canonical S3/SigV4 via boto3).

Subcommands:
  putobj  ACCESS SECRET BUCKET KEY FILE
      PUT an object; print the created VersionId.
  setret  ACCESS SECRET BUCKET KEY VERSIONID MODE DAYS
      Put object retention (MODE = GOVERNANCE|COMPLIANCE); print readback.
  setlegal ACCESS SECRET BUCKET KEY VERSIONID STATUS
      Put object legal hold (STATUS = ON|OFF); print readback.
  show    ACCESS SECRET BUCKET KEY VERSIONID
      Print retention + legal-hold metadata for a version.
  delver  ACCESS SECRET BUCKET KEY VERSIONID [bypass]
      DELETE a specific version; optional literal 'bypass' sets
      x-amz-bypass-governance-retention:true. Prints status or S3 error.
  listver ACCESS SECRET BUCKET
      List all versions (key + versionId + isLatest).
"""
import sys, datetime, boto3
from botocore.config import Config

def client(access, secret):
    cfg = Config(signature_version="s3v4", s3={"addressing_style": "path"},
                 request_checksum_calculation="when_required",
                 response_checksum_validation="when_required")
    return boto3.client("s3", endpoint_url="http://127.0.0.1:9000",
                        aws_access_key_id=access, aws_secret_access_key=secret,
                        region_name="us-east-1", config=cfg)

def s3err(e):
    resp = getattr(e, "response", None)
    if resp:
        err = resp.get("Error", {})
        print("S3_Code    =", err.get("Code"))
        print("S3_Message =", err.get("Message"))
        print("HTTPStatus =", resp.get("ResponseMetadata", {}).get("HTTPStatusCode"))
    else:
        print("raw:", e)

cmd = sys.argv[1]
if cmd == "putobj":
    access, secret, bucket, key, f = sys.argv[2:7]
    s3 = client(access, secret)
    r = s3.put_object(Bucket=bucket, Key=key, Body=open(f, "rb").read())
    print("PUT_STATUS =", r["ResponseMetadata"]["HTTPStatusCode"])
    print("VersionId  =", r.get("VersionId"))

elif cmd == "setret":
    access, secret, bucket, key, vid, mode, days = sys.argv[2:9]
    s3 = client(access, secret)
    until = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(days=int(days))
    r = s3.put_object_retention(Bucket=bucket, Key=key, VersionId=vid,
        Retention={"Mode": mode, "RetainUntilDate": until})
    print("PutRetention_STATUS =", r["ResponseMetadata"]["HTTPStatusCode"])
    g = s3.get_object_retention(Bucket=bucket, Key=key, VersionId=vid)
    print("Retention_readback  =", g["Retention"])

elif cmd == "setlegal":
    access, secret, bucket, key, vid, status = sys.argv[2:8]
    s3 = client(access, secret)
    r = s3.put_object_legal_hold(Bucket=bucket, Key=key, VersionId=vid,
        LegalHold={"Status": status})
    print("PutLegalHold_STATUS =", r["ResponseMetadata"]["HTTPStatusCode"])
    g = s3.get_object_legal_hold(Bucket=bucket, Key=key, VersionId=vid)
    print("LegalHold_readback  =", g["LegalHold"])

elif cmd == "show":
    access, secret, bucket, key, vid = sys.argv[2:7]
    s3 = client(access, secret)
    try:
        g = s3.get_object_retention(Bucket=bucket, Key=key, VersionId=vid)
        print("Retention =", g["Retention"])
    except Exception as e:
        print("Retention = <none>")
    try:
        g = s3.get_object_legal_hold(Bucket=bucket, Key=key, VersionId=vid)
        print("LegalHold =", g["LegalHold"])
    except Exception as e:
        print("LegalHold = <none>")

elif cmd == "delver":
    access, secret, bucket, key, vid = sys.argv[2:7]
    bypass = len(sys.argv) > 7 and sys.argv[7] == "bypass"
    s3 = client(access, secret)
    kw = dict(Bucket=bucket, Key=key, VersionId=vid)
    if bypass:
        kw["BypassGovernanceRetention"] = True
    try:
        r = s3.delete_object(**kw)
        print("DELETE_STATUS =", r["ResponseMetadata"]["HTTPStatusCode"])
        print("DeleteMarker  =", r.get("DeleteMarker"))
        print("VersionId     =", r.get("VersionId"))
    except Exception as e:
        print("DELETE_ERROR_TYPE =", type(e).__name__)
        s3err(e)

elif cmd == "listver":
    access, secret, bucket = sys.argv[2:5]
    s3 = client(access, secret)
    r = s3.list_object_versions(Bucket=bucket)
    for v in r.get("Versions", []):
        print(f"VERSION key={v['Key']} versionId={v['VersionId']} isLatest={v['IsLatest']}")
    for d in r.get("DeleteMarkers", []):
        print(f"DELMARK key={d['Key']} versionId={d['VersionId']} isLatest={d['IsLatest']}")

else:
    print("unknown subcommand:", cmd); sys.exit(2)
```

Each condition is triggered independently, capturing its own trace and audit slice:

```bash
a0=$(wc -l < $INV/out/audit.jsonl)
nohup $MC admin trace -v --funcname "s3.DeleteObject" inv9000 > $OUT/trace_C1_legalhold.txt 2>&1 &
T=$!; sleep 2.5
python3 $INV/scripts/r2_delete_locked.py delver r2nobypassuser <REDACTED_USER_SECRET> r2bucket c1_legalhold <VID>
sleep 3; kill "$T"; wait "$T" 2>/dev/null
tail -n +$((a0+1)) $INV/out/audit.jsonl > $OUT/audit_C1_legalhold.jsonl
```

### Observed Output

**Setup state (before any delete). [Observed]** Bucket object-lock configuration and versioning (S3 API readback):

```
GetObjectLockConfiguration -> {'ObjectLockEnabled': 'Enabled'}
GetBucketVersioning Status -> Enabled
```

The two non-admin users and their attached policies:

```
AccessKey: r2bypassuser
Status: enabled
PolicyName: r2bypass
MemberOf: []

AccessKey: r2nobypassuser
Status: enabled
PolicyName: r2nobypass
MemberOf: []
```

Per-object lock metadata as read back from the server (`get_object_retention` / `get_object_legal_hold`), with the exact version IDs used as delete targets:

```
c1_legalhold  : LegalHold = {'Status': 'ON'}
c2_compliance : Retention = {'Mode': 'COMPLIANCE', 'RetainUntilDate': datetime.datetime(2026, 7, 17, 20, 55, 16, tzinfo=tzlocal())}
c3_gov        : Retention = {'Mode': 'GOVERNANCE', 'RetainUntilDate': datetime.datetime(2026, 7, 17, 20, 55, 16, tzinfo=tzlocal())}
c4_gov        : Retention = {'Mode': 'GOVERNANCE', 'RetainUntilDate': datetime.datetime(2026, 7, 17, 20, 55, 16, tzinfo=tzlocal())}
c5_gov        : Retention = {'Mode': 'GOVERNANCE', 'RetainUntilDate': datetime.datetime(2026, 7, 17, 20, 55, 16, tzinfo=tzlocal())}
```

Full version listing before the deletes:

```
VERSION key=c1_legalhold versionId=33a24803-c50c-4b1c-b97d-c09289abac37 isLatest=True
VERSION key=c2_compliance versionId=55e5a37b-ec49-4057-8e74-f6256108af1c isLatest=True
VERSION key=c3_gov versionId=ecce198f-3dde-41ff-b159-848506ca4f18 isLatest=True
VERSION key=c4_gov versionId=b5a355e9-6fbb-4b27-a82f-ed66fa182de0 isLatest=True
VERSION key=c5_gov versionId=39cf845b-0d2f-471c-b005-2e085ece7921 isLatest=True
```

**Condition 1 — Legal Hold ON** (delete as `r2nobypassuser`, no bypass) → **400 WORM**. [Observed]

Client result:

```
DELETE_ERROR_TYPE = InvalidRequest
S3_Code    = InvalidRequest
S3_Message = Object is WORM protected and cannot be overwritten
HTTPStatus = 400
```

Complete `s3.DeleteObject` trace (`$OUT/trace_C1_legalhold.txt`):

```
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T20:55:40.556] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /r2bucket/c1_legalhold?versionId=33a24803-c50c-4b1c-b97d-c09289abac37
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/D,a,N,c,e cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Amz-Sdk-Invocation-Id: 5bee9eca-c449-431f-8e29-dbdb2e560ad5
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r2nobypassuser/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=cb424222b6d9f5715558f7ea220f4139ca4d3e3cebf8ea44193f85471657d880
127.0.0.1:9000 X-Amz-Date: 20260714T205540Z
127.0.0.1:9000
127.0.0.1:9000 [RESPONSE] [2026-07-14T20:55:40.557] [ Duration 765µs TTFB 724.633µs ↑ 131 B  ↓ 369 B ]
127.0.0.1:9000 400 Bad Request
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 369
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C2436DA2F897FF
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>c1_legalhold</Key><BucketName>r2bucket</BucketName><Resource>/r2bucket/c1_legalhold</Resource><RequestId>18C2436DA2F897FF</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
127.0.0.1:9000
```

**Condition 2 — Compliance retention, before expiry** (delete as `r2nobypassuser`, no bypass) → **400 WORM**. [Observed]

Client result:

```
DELETE_ERROR_TYPE = InvalidRequest
S3_Code    = InvalidRequest
S3_Message = Object is WORM protected and cannot be overwritten
HTTPStatus = 400
```

Complete `s3.DeleteObject` trace (`$OUT/trace_C2_compliance.txt`):

```
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T20:55:46.373] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /r2bucket/c2_compliance?versionId=55e5a37b-ec49-4057-8e74-f6256108af1c
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Amz-Sdk-Invocation-Id: 397ecf60-4512-4551-81f4-2cc7fbb4b7a4
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260714T205546Z
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r2nobypassuser/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=8b0e95dfffbef957fed81d42727d702e3259b6147300b41d5397b29b331ef5a4
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/c,e,N,D,a cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000
127.0.0.1:9000 [RESPONSE] [2026-07-14T20:55:46.373] [ Duration 644µs TTFB 606.838µs ↑ 131 B  ↓ 371 B ]
127.0.0.1:9000 400 Bad Request
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 371
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Request-Id: 18C2436EFDA468A2
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>c2_compliance</Key><BucketName>r2bucket</BucketName><Resource>/r2bucket/c2_compliance</Resource><RequestId>18C2436EFDA468A2</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
127.0.0.1:9000
```

**Condition 3 — Governance retention, no bypass header** (delete as `r2nobypassuser`) → **400 WORM**. [Observed]

Client result:

```
DELETE_ERROR_TYPE = InvalidRequest
S3_Code    = InvalidRequest
S3_Message = Object is WORM protected and cannot be overwritten
HTTPStatus = 400
```

Complete `s3.DeleteObject` trace (`$OUT/trace_C3_gov_nobypass.txt`):

```
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T20:55:52.179] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /r2bucket/c3_gov?versionId=ecce198f-3dde-41ff-b159-848506ca4f18
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/D,c,N,a,e cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r2nobypassuser/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=bd39812784ea0df18c4cdc8ea09158fe08a2be50e2dd71b528715571b1dbe5bb
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260714T205552Z
127.0.0.1:9000 Amz-Sdk-Invocation-Id: ecc88164-25f6-42cb-8b67-1294d879b8fd
127.0.0.1:9000
127.0.0.1:9000 [RESPONSE] [2026-07-14T20:55:52.180] [ Duration 1.348ms TTFB 1.302733ms ↑ 131 B  ↓ 357 B ]
127.0.0.1:9000 400 Bad Request
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Request-Id: 18C2437057BD599C
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 357
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRequest</Code><Message>Object is WORM protected and cannot be overwritten</Message><Key>c3_gov</Key><BucketName>r2bucket</BucketName><Resource>/r2bucket/c3_gov</Resource><RequestId>18C2437057BD599C</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
127.0.0.1:9000
```

**Condition 4 — Governance, bypass header but caller LACKS `s3:BypassGovernanceRetention`** (delete as `r2nobypassuser`) → **403 AccessDenied**. [Observed]

Client result:

```
DELETE_ERROR_TYPE = AccessDenied
S3_Code    = AccessDenied
S3_Message = Access Denied.
HTTPStatus = 403
```

Complete `s3.DeleteObject` trace (`$OUT/trace_C4_gov_bypass_noperm.txt`):

```
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T20:55:58.043] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /r2bucket/c4_gov?versionId=b5a355e9-6fbb-4b27-a82f-ed66fa182de0
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/a,e,c,D,N cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9000 Amz-Sdk-Invocation-Id: e0e54a6e-fe43-430e-95a7-00b821f60568
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r2nobypassuser/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=73a92557d357ac3434407275ba2ff079039c420c20f9e188257dac5f444d07cf
127.0.0.1:9000 X-Amz-Bypass-Governance-Retention: true
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260714T205558Z
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000
127.0.0.1:9000 [RESPONSE] [2026-07-14T20:55:58.044] [ Duration 817µs TTFB 786.769µs ↑ 165 B  ↓ 319 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 319
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C24371B548CE9D
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>c4_gov</Key><BucketName>r2bucket</BucketName><Resource>/r2bucket/c4_gov</Resource><RequestId>18C24371B548CE9D</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
127.0.0.1:9000
```

**Condition 5 — Governance, bypass header AND caller HAS `s3:BypassGovernanceRetention`** (delete as `r2bypassuser`) → **204, version deleted**. [Observed]

Client result:

```
DELETE_STATUS = 204
DeleteMarker  = None
VersionId     = 39cf845b-0d2f-471c-b005-2e085ece7921
```

Complete `s3.DeleteObject` trace (`$OUT/trace_C5_gov_bypass_perm.txt`):

```
127.0.0.1:9000 [REQUEST s3.DeleteObject] [2026-07-14T20:56:03.886] [Client IP: 127.0.0.1]
127.0.0.1:9000 DELETE /r2bucket/c5_gov?versionId=39cf845b-0d2f-471c-b005-2e085ece7921
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/c,D,N,a,e cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9000 X-Amz-Bypass-Governance-Retention: true
127.0.0.1:9000 Amz-Sdk-Invocation-Id: b6fb8ab4-9cab-409b-a047-09a0fc7d3f4b
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r2bypassuser/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-bypass-governance-retention;x-amz-content-sha256;x-amz-date, Signature=9bd5ebb8256947e26872391b79f6b4259ed4f4b0190af0d24f9b1c021f38c64e
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260714T205603Z
127.0.0.1:9000
127.0.0.1:9000 [RESPONSE] [2026-07-14T20:56:03.888] [ Duration 1.226ms TTFB 1.160111ms ↑ 165 B  ↓ 0 B ]
127.0.0.1:9000 204 No Content
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 x-amz-version-id: 39cf845b-0d2f-471c-b005-2e085ece7921
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 X-Amz-Request-Id: 18C24373118D3B83
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000
127.0.0.1:9000
```

**Audit-log records — one per condition (compact JSONL, `$OUT/audit_summary.jsonl`). [Observed]** Each carries the authenticated `accessKey` and the matching `statusCode`; all share the current `deploymentid` `b3de8c20-51f3-4954-9a06-c02b2323ff40`:

```jsonl
{"condition":"Legal Hold ON","api.name":"DeleteObject","statusCode":400,"status":"Bad Request","accessKey":"r2nobypassuser","bucket":"r2bucket","object":"c1_legalhold","versionId":"33a24803-c50c-4b1c-b97d-c09289abac37","deploymentid":"b3de8c20-51f3-4954-9a06-c02b2323ff40"}
{"condition":"Compliance","api.name":"DeleteObject","statusCode":400,"status":"Bad Request","accessKey":"r2nobypassuser","bucket":"r2bucket","object":"c2_compliance","versionId":"55e5a37b-ec49-4057-8e74-f6256108af1c","deploymentid":"b3de8c20-51f3-4954-9a06-c02b2323ff40"}
{"condition":"Governance, no bypass header","api.name":"DeleteObject","statusCode":400,"status":"Bad Request","accessKey":"r2nobypassuser","bucket":"r2bucket","object":"c3_gov","versionId":"ecce198f-3dde-41ff-b159-848506ca4f18","deploymentid":"b3de8c20-51f3-4954-9a06-c02b2323ff40"}
{"condition":"Governance, bypass header, LACKS permission","api.name":"DeleteObject","statusCode":403,"status":"Forbidden","accessKey":"r2nobypassuser","bucket":"r2bucket","object":"c4_gov","versionId":"b5a355e9-6fbb-4b27-a82f-ed66fa182de0","deploymentid":"b3de8c20-51f3-4954-9a06-c02b2323ff40"}
{"condition":"Governance, bypass header, HAS permission","api.name":"DeleteObject","statusCode":204,"status":"No Content","accessKey":"r2bypassuser","bucket":"r2bucket","object":"c5_gov","versionId":"39cf845b-0d2f-471c-b005-2e085ece7921","deploymentid":"b3de8c20-51f3-4954-9a06-c02b2323ff40"}
```

One representative audit record in full (the Compliance delete, `api.name=DeleteObject`, `statusCode=400`, `accessKey=r2nobypassuser`):

```json
{
  "version": "1",
  "deploymentid": "b3de8c20-51f3-4954-9a06-c02b2323ff40",
  "time": "2026-07-14T20:55:46.373638257Z",
  "event": "",
  "trigger": "incoming",
  "api": {
    "name": "DeleteObject",
    "bucket": "r2bucket",
    "object": "c2_compliance",
    "status": "Bad Request",
    "statusCode": 400,
    "rx": 0,
    "tx": 371,
    "txHeaders": 411,
    "timeToFirstByte": "606838ns",
    "timeToFirstByteInNS": "606838",
    "timeToResponse": "627355ns",
    "timeToResponseInNS": "627355"
  },
  "remotehost": "127.0.0.1",
  "requestID": "18C2436EFDA468A2",
  "userAgent": "Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/c,e,N,D,a cfg/retry-mode#legacy Botocore/1.43.47",
  "requestPath": "/r2bucket/c2_compliance",
  "requestHost": "127.0.0.1:9000",
  "requestQuery": {
    "versionId": "55e5a37b-ec49-4057-8e74-f6256108af1c"
  },
  "requestHeader": {
    "Accept-Encoding": "identity",
    "Amz-Sdk-Invocation-Id": "397ecf60-4512-4551-81f4-2cc7fbb4b7a4",
    "Amz-Sdk-Request": "attempt=1",
    "Authorization": "AWS4-HMAC-SHA256 Credential=r2nobypassuser/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=8b0e95dfffbef957fed81d42727d702e3259b6147300b41d5397b29b331ef5a4",
    "Content-Length": "0",
    "User-Agent": "Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/c,e,N,D,a cfg/retry-mode#legacy Botocore/1.43.47",
    "X-Amz-Content-Sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    "X-Amz-Date": "20260714T205546Z"
  },
  "responseHeader": {
    "Accept-Ranges": "bytes",
    "Content-Length": "371",
    "Content-Type": "application/xml",
    "Server": "MinIO",
    "Strict-Transport-Security": "max-age=31536000; includeSubDomains",
    "Vary": "Origin,Accept-Encoding",
    "X-Amz-Id-2": "dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8",
    "X-Amz-Request-Id": "18C2436EFDA468A2",
    "X-Content-Type-Options": "nosniff",
    "X-Ratelimit-Limit": "1140320",
    "X-Ratelimit-Remaining": "1140320",
    "X-Xss-Protection": "1; mode=block"
  },
  "tags": {
    "DeleteObject": "name=c2_compliance,pool=1,set=1",
    "GetObjectInfo": "name=c2_compliance,pool=1,set=1"
  },
  "accessKey": "r2nobypassuser"
}
```

(When `mc admin trace` is subscribed, the audit target additionally records an `api.name=Trace` entry for the trace subscription itself, by the admin principal; it is unrelated to the delete and is omitted from the per-condition summary above.)

**Server error log. [Observed]** `mc admin logs --last 10 inv9000`, captured while re-triggering the Legal-Hold delete (which returned `400 WORM`), contains **no** `DeleteObject`/WORM entry — the only entry is an unrelated earlier transient audit-endpoint connection error from a server-restart window:

```
 API: SYSTEM.config()
 Time: 20:33:49 UTC 07/14/2026
 DeploymentID: b3de8c20-51f3-4954-9a06-c02b2323ff40
 Error: unable to send audit/log entry(s) to 'minio-http-audit-inv' err 'http://127.0.0.1:9999 returned 'Post "http://127.0.0.1:9999": dial tcp 127.0.0.1:9999: connect: connection refused', please check your endpoint configuration': 1 (*fmt.wrapError)
        4: /tmp/blitzy/minio/blitzy-588df31b-e44d-4dd8-a545-973febe20ac8_002d7b/internal/logger/logonce.go:64:logger.(*logOnceType).logOnceConsoleIf()
        3: /tmp/blitzy/minio/blitzy-588df31b-e44d-4dd8-a545-973febe20ac8_002d7b/internal/logger/logonce.go:157:logger.LogOnceConsoleIf()
        2: /tmp/blitzy/minio/blitzy-588df31b-e44d-4dd8-a545-973febe20ac8_002d7b/cmd/logging.go:132:cmd.configLogOnceConsoleIf()
        1: /tmp/blitzy/minio/blitzy-588df31b-e44d-4dd8-a545-973febe20ac8_002d7b/internal/logger/target/http/http.go:442:http.(*Target).startQueueProcessor()
```

**After-state — deletion proof. [Observed]** After all five attempts, the four refused versions remain and only the authorized Governance bypass (`c5_gov`) was removed:

```
VERSION key=c1_legalhold versionId=33a24803-c50c-4b1c-b97d-c09289abac37 isLatest=True
VERSION key=c2_compliance versionId=55e5a37b-ec49-4057-8e74-f6256108af1c isLatest=True
VERSION key=c3_gov versionId=ecce198f-3dde-41ff-b159-848506ca4f18 isLatest=True
VERSION key=c4_gov versionId=b5a355e9-6fbb-4b27-a82f-ed66fa182de0 isLatest=True
```

### Responsible Code

All enforcement is in `enforceRetentionBypassForDelete` at `cmd/bucket-object-lock.go:84`. **[Source-grounded]** anchors, verified at HEAD:

- **Legal Hold ON** → `return ObjectLocked{}` at `cmd/bucket-object-lock.go:100-101`:
  ```go
  lhold := objectlock.GetObjectLegalHoldMeta(oi.UserDefined)
  if lhold.Status.Valid() && lhold.Status == objectlock.LegalHoldOn {
      return ObjectLocked{}
  }
  ```
- **Compliance**, retain-until not before now → `return ObjectLocked{}` at `cmd/bucket-object-lock.go:120-121` (the **active** retention check). `case objectlock.RetCompliance:` begins at `:107`; `objectlock.UTCNowNTP()` is called at `:114` and its **error-fallback** `return ObjectLocked{}` is `:117` (this is the NTP-failure path, *not* the active check):
  ```go
  if !ret.RetainUntilDate.Before(t) {
      return ObjectLocked{}
  }
  ```
- **Governance** — bypass-header probe `byPassSet := objectlock.IsObjectLockGovernanceBypassSet(r.Header)` at `cmd/bucket-object-lock.go:138`. If the header is **absent**, the active-retention check returns `ObjectLocked{}` at `:146-147` (NTP-fallback `:143`). If the header is **present**, the permission is checked at `cmd/bucket-object-lock.go:153-154`:
  ```go
  if checkRequestAuthType(ctx, r, policy.BypassGovernanceRetentionAction, bucket, object.ObjectName) != ErrNone {
      return errAuthentication
  }
  ```
- **Error mapping. [Source-grounded]**
  - `ObjectLocked{}` → `ErrObjectLocked`: `case ObjectLocked:` `apiErr = ErrObjectLocked` at `cmd/api-errors.go:2298-2299`; `ErrObjectLocked` is defined at `cmd/api-errors.go:1059-1063` — `Code:"InvalidRequest"`, `Description:"Object is WORM protected and cannot be overwritten"`, `HTTPStatusCode: http.StatusBadRequest` (400). This is exactly the 400/WORM body observed in Conditions 1–3.
  - `errAuthentication` → `ErrAccessDenied`: `case errAuthentication: apiErr = ErrAccessDenied` at `cmd/api-errors.go:2176-2177`. This is the 403 observed in Condition 4.

### Rationale

**[Observed] + [Source-grounded]** The two distinct denials map to the two `error` returns of `enforceRetentionBypassForDelete`: WORM protection (`ObjectLocked{}` → 400 `InvalidRequest`, Conditions 1–3) versus a missing bypass grant (`errAuthentication` → 403 `AccessDenied`, Condition 4). The permission gate `checkRequestAuthType(..., policy.BypassGovernanceRetentionAction, ...)` at `cmd/bucket-object-lock.go:153` is reached **only** for Governance and **only** when the bypass header is present; Compliance (`:107-121`) and Legal Hold (`:99-101`) have **no** bypass branch, so they cannot be deleted before expiry by anyone — matching the uniform 400 for Conditions 1–3 and the 204 seen only for the authorized Governance bypass (Condition 5). The before/after listing confirms only `c5_gov` was removed.

---

## R3 — Bit-rot detection on GET after manual corruption

> **Question (verbatim):** "I also want you to analyze how the system handles unauthorized manual data corruption within the storage backend. Trigger a bit rot detection event and identify the specific runtime logs generated during a subsequent get request."

### Direct Answer

**[Observed]** Manually corrupting an on-disk shard (`part.1`) and then issuing a GET triggers MinIO's per-shard **HighwayHash** verification, which detects the corruption as `errFileCorrupt`. What happens next depends on whether parity is available:

- **Multi-drive erasure set (4 drives, EC:2) — [Observed]:** the GET **succeeds with HTTP 200 and byte-for-byte-correct data** (the returned body's SHA-256 equals the original), because the corrupt shard is detected during read and the object is **reconstructed in memory** from the remaining shards. **No dedicated bit-rot entry is written to the server error log or the audit log, and `mc admin logs` shows nothing.** The only runtime signals are visible via `mc admin trace -a` (all-calls): the internal `storage.ReadFileStream` **re-read pattern** (an extra shard is read after the corrupt one fails its hash), followed ~1 second later by an asynchronous **`[HEALING heal.Object] … mode=0 …`** trace entry — the MRF (Metadata-Refresh/heal) operation queued by the read path.
- **The queued `mode=0` (normal-scan) heal does NOT repair the on-disk corruption — [Observed]:** because a normal scan only checks part existence/size (which are intact), the corrupt shard **remains corrupt on disk** after the GET. Only an explicit **deep scan** (`mc admin heal --scan deep`), which recomputes the HighwayHash, detects the object as degraded (`Yellow → Green`) and **rewrites the shard to its original bytes**.
- **Single drive (no parity) — [Observed]:** the same detection makes the object **unreadable**; the GET returns **HTTP 503 `SlowDownRead`** ("Resource requested is unreadable") — detection *without* reconstruction.

So the precise answer to "what specific runtime logs appear during the subsequent GET": on a redundant set there is **no bit-rot log line at all** — detection and reconstruction are silent at the log level; the observable evidence is the successful reconstructed GET plus the `heal.Object` entry in the all-calls trace. On a non-redundant set the GET fails with `503 SlowDownRead`. Both results were reproduced twice.

### Reproduction

`OUT=$INV/out/r3`. The 4-drive erasure server runs at `:9100` (EC:2), the single-drive server at `:9000`. `alias inv9100`/`inv9000` are configured with the root credentials (used here only to read/heal — corruption is applied directly to disk, outside MinIO). Its stdout/stderr are captured at `$INV/out/server9100.log` / `server9000.log`.

```bash
# 1) deterministic 1 MiB payload; record exact size + hashes
python3 -c "import sys; sys.stdout.buffer.write((b'R3-bitrot-detection-demo-block-'*(1048576//31+1))[:1048576])" > $OUT/r3payload.bin
wc -c $OUT/r3payload.bin ; sha256sum $OUT/r3payload.bin ; md5sum $OUT/r3payload.bin
# 2) upload via canonical S3 (mc streaming SigV4) to the erasure bucket
$MC mb -p inv9100/r3bucket
$MC cp $OUT/r3payload.bin inv9100/r3bucket/r3obj
# 3) locate the on-disk shards (one part.1 per drive)
find /tmp/minio4/d{1,2,3,4} -path '*r3bucket/r3obj*' -name 'part.*' | sort
# 4) corrupt EXACTLY ONE shard (d1) in place, off the MinIO code path
python3 -c "import sys; sys.stdout.buffer.write(b'\xff'*128)" > /tmp/ffblock
dd if=/tmp/ffblock of=<d1-part.1> bs=1 seek=100000 count=128 conv=notrunc
# 5) invalidate the OS page cache so MinIO re-reads the corrupt bytes from disk
sync; echo 3 > /proc/sys/vm/drop_caches
# 6) GET and verify the returned bytes; capture trace (use -a for internal storage/heal calls)
nohup $MC admin trace -a -v inv9100 > $OUT/trace_all_cycle2.txt 2>&1 &
python3 $INV/scripts/r3_get.py http://127.0.0.1:9100 minioadmin minioadmin r3bucket r3obj
# 7) heal behaviour: normal scan vs deep scan
$MC admin heal --recursive             inv9100/r3bucket/r3obj   # normal (metadata) scan
$MC admin heal --recursive --scan deep inv9100/r3bucket/r3obj   # deep (HighwayHash) scan
```

Canonical GET driver `$INV/scripts/r3_get.py` (boto3 SigV4; prints the returned body's SHA-256):

```python
import sys, hashlib, boto3
from botocore.config import Config
ep, ak, sk, bucket, key = sys.argv[1:6]
s3 = boto3.client("s3", endpoint_url=ep, aws_access_key_id=ak, aws_secret_access_key=sk,
                  region_name="us-east-1", config=Config(signature_version="s3v4"))
r = s3.get_object(Bucket=bucket, Key=key)
body = r["Body"].read()
print("GET_STATUS =", r["ResponseMetadata"]["HTTPStatusCode"])
print("BODY_LEN   =", len(body))
print("BODY_SHA256=", hashlib.sha256(body).hexdigest())
print("ETag       =", r.get("ETag"))
```

### Observed Output

**Payload and stored object. [Observed]** Exact bytes and hashes of the uploaded object:

```
bytes  = 1048576
sha256 = ef3488fcf9e9665f4b383864da480f356c69a671624323b2edf32bff340185f8
md5    = 465fd2e7c5da206f67599627d9d4f081
```

Server-side `mc stat` (ETag equals the payload MD5 — single-part upload):

```
Name      : r3obj
Date      : 2026-07-14 21:04:59 UTC
Size      : 1.0 MiB
ETag      : 465fd2e7c5da206f67599627d9d4f081
Type      : file
Metadata  :
  Content-Type: application/octet-stream
```

**On-disk shard layout. [Observed]** The object is stored as one `part.1` shard per drive (each 524320 B = 512 KiB data + a 32-byte HighwayHash), and a clean baseline of every shard's SHA-256:

```
/tmp/minio4/d1/r3bucket/r3obj/b18df494-a21a-4324-a10e-6908736f723e/part.1
/tmp/minio4/d2/r3bucket/r3obj/b18df494-a21a-4324-a10e-6908736f723e/part.1
/tmp/minio4/d3/r3bucket/r3obj/b18df494-a21a-4324-a10e-6908736f723e/part.1
/tmp/minio4/d4/r3bucket/r3obj/b18df494-a21a-4324-a10e-6908736f723e/part.1
```

```
  524320  ed82255d4639a4fc6f432c4cd7cbf7f42e733435e0dd6237a1fa339c3681a5a0  /tmp/minio4/d1/r3bucket/r3obj/b18df494-a21a-4324-a10e-6908736f723e/part.1
  524320  a14b9cac259465ded8a599b4cfb0740004d97c4153ac21e8b32412c14ba72d80  /tmp/minio4/d2/r3bucket/r3obj/b18df494-a21a-4324-a10e-6908736f723e/part.1
  524320  d49d98cc9f6c98fac62537fc6ab1ef1fc877f3ce195fb91ad934f62fa966d24a  /tmp/minio4/d3/r3bucket/r3obj/b18df494-a21a-4324-a10e-6908736f723e/part.1
  524320  fed40c9b2b50de964a69e5cc846a3848f0db78221e7b90bea2679c8c75d272c0  /tmp/minio4/d4/r3bucket/r3obj/b18df494-a21a-4324-a10e-6908736f723e/part.1
```

The d1 shard's first bytes are readable payload text, confirming it holds a contiguous data fragment (a *data* shard, so it is read during a GET):

```
 2d 64 65 74 65 63 74 69 6f 6e 2d 64 65 6d 6f 2d
 62 6c 6f 63 6b 2d 52 33 2d 62 69 74 72 6f 74 2d
 64 65 74 65 63 74 69 6f 6e 2d 64 65 6d 6f 2d 62
 6c 6f 63 6b 2d 52 33 2d 62 69 74 72 6f 74 2d 64
```

---

**Cycle 1 — corrupt d1 @ offset 100000 (128 × `0xff`), drop cache, GET. [Observed]**

Shard bytes after corruption (same offset), and the shard SHA-256 change:

```
 ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff
*
```

```
d1 shard sha256 BEFORE = ed82255d4639a4fc6f432c4cd7cbf7f42e733435e0dd6237a1fa339c3681a5a0
d1 shard sha256 AFTER  = 3d056c7e95b690a0f7b57151b2af162f0724447458296b27778d48acb73deb43
```

GET result — the returned body SHA-256 **equals the original payload SHA-256** (`ef3488fc…85f8`), i.e. correct bytes were served despite the corrupt shard:

```
GET_STATUS = 200
BODY_LEN   = 1048576
BODY_SHA256= ef3488fcf9e9665f4b383864da480f356c69a671624323b2edf32bff340185f8
ETag       = "465fd2e7c5da206f67599627d9d4f081"
```

New lines written to the server log by this GET (bit-rot detection is silent):

```
$ tail -n +<baseline> $INV/out/server9100.log     # new lines produced by the GET
(no output — 0 new lines)
```

---

**Cycle 2 — independent re-corruption of d1 @ offset 200000 (200 × `0xaa`), drop cache, GET, `mc admin trace -a`. [Observed]** This proves the behaviour is stable across two independent corrupt→GET cycles.

```
 aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa
*
```

```
d1 shard sha256 BEFORE = ed82255d4639a4fc6f432c4cd7cbf7f42e733435e0dd6237a1fa339c3681a5a0   (clean, after deep-heal)
d1 shard sha256 AFTER  = shard sha256 post-corrupt = 09e3ee83fb77e76cb89d08fd1353cd08d811ac9678d59beee3402b85d95a2e56
```

GET result — again 200 with the original body SHA-256:

```
GET_STATUS = 200
BODY_LEN   = 1048576
BODY_SHA256= ef3488fcf9e9665f4b383864da480f356c69a671624323b2edf32bff340185f8
ETag       = "465fd2e7c5da206f67599627d9d4f081"
```

Complete `s3.GetObject` request/response block from the all-calls trace (200 OK, 1.0 MiB served):

```
127.0.0.1:9100 [REQUEST s3.GetObject] [2026-07-14T21:10:45.821] [Client IP: 127.0.0.1]
127.0.0.1:9100 GET /r3bucket/r3obj
127.0.0.1:9100 Proto: HTTP/1.1
127.0.0.1:9100 Host: 127.0.0.1:9100
127.0.0.1:9100 Content-Length: 0
127.0.0.1:9100 X-Amz-Date: 20260714T211045Z
127.0.0.1:9100 Accept-Encoding: identity
127.0.0.1:9100 Amz-Sdk-Invocation-Id: 71eda3d9-dbf3-4ec6-b29f-5113ec8991e5
127.0.0.1:9100 Amz-Sdk-Request: attempt=1
127.0.0.1:9100 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-checksum-mode;x-amz-content-sha256;x-amz-date, Signature=1bff625c037c5ad3b9f3b2fa70c445b1731b353dc46113b4c353dda262afaa12
127.0.0.1:9100 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/e,N,Z,b,D cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9100 X-Amz-Checksum-Mode: ENABLED
127.0.0.1:9100 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9100 <BLOB>
127.0.0.1:9100 [RESPONSE] [2026-07-14T21:10:45.878] [ Duration 57.342ms TTFB 44.97246ms ↑ 151 B  ↓ 1.0 MiB ]
127.0.0.1:9100 200 OK
127.0.0.1:9100 X-Content-Type-Options: nosniff
127.0.0.1:9100 X-Xss-Protection: 1; mode=block
127.0.0.1:9100 Accept-Ranges: bytes
127.0.0.1:9100 Last-Modified: Tue, 14 Jul 2026 21:04:59 GMT
127.0.0.1:9100 Server: MinIO
127.0.0.1:9100 X-Amz-Id-2: 40fd399614142fea3be9690e18526c1881df2b9fc838b215f9c270b056695f9e
127.0.0.1:9100 X-Ratelimit-Limit: 564180
127.0.0.1:9100 X-Ratelimit-Remaining: 564180
127.0.0.1:9100 Content-Length: 1048576
127.0.0.1:9100 Content-Type: application/octet-stream
127.0.0.1:9100 ETag: "465fd2e7c5da206f67599627d9d4f081"
127.0.0.1:9100 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9100 Vary: Origin,Accept-Encoding
127.0.0.1:9100 X-Amz-Request-Id: 18C2444068ED455A
127.0.0.1:9100 <BLOB>
127.0.0.1:9100
```

The **internal storage and healing** trace lines for that same GET, extracted from `$OUT/trace_all_cycle2.txt`:

```bash
grep -E 'storage.ReadFileStream' $OUT/trace_all_cycle2.txt
grep -E 'HEALING|heal.Object'   $OUT/trace_all_cycle2.txt
```

```
127.0.0.1:9100  [STORAGE storage.ReadFileStream] [2026-07-14T21:10:45.822] /tmp/minio4/d4 r3bucket r3obj/b18df494-a21a-4324-a10e-6908736f723e/part.1 total-errs-availability=0 total-errs-timeout=0 40.734µs 512 KiB
127.0.0.1:9100  [STORAGE storage.ReadFileStream] [2026-07-14T21:10:45.822] /tmp/minio4/d1 r3bucket r3obj/b18df494-a21a-4324-a10e-6908736f723e/part.1 total-errs-timeout=0 total-errs-availability=0 41.892µs 512 KiB
127.0.0.1:9100  [STORAGE storage.ReadFileStream] [2026-07-14T21:10:45.841] /tmp/minio4/d2 r3bucket r3obj/b18df494-a21a-4324-a10e-6908736f723e/part.1 total-errs-availability=0 total-errs-timeout=0 66.24µs 512 KiB

127.0.0.1:9100  [HEALING heal.Object] [2026-07-14T21:10:46.879] r3bucket/r3obj mode=0 remove=true version-id=null disks=4 dry=false 289.554µs 1.0 MiB
```

Three `ReadFileStream` calls are made for a 2-data-shard object: d4 and d1 are read concurrently at `…45.822`, then d2 is read ~19 ms later at `…45.841` — the extra read is the reconstruction re-trigger fired when corrupt **d1** fails its HighwayHash check. The `[HEALING heal.Object] … mode=0 …` line fires ~1 s **after** the response — the asynchronous MRF heal.

This GET also produced **0 new server-log lines**, and immediately afterwards the d1 shard was **still corrupt on disk** (SHA-256 unchanged from the post-corruption value above) — confirming the client was served from in-memory reconstruction, not from an on-disk repair.

`mc admin logs` on `:9100`, captured while GET-ing the still-corrupt object, is empty (no bit-rot/corruption entry):

```bash
$MC admin logs --last 20 inv9100      # during the corrupt-object GET
```

```
(no output)
```

---

**Heal behaviour — normal scan vs deep scan. [Observed]** On a fresh corruption of d1 (@ offset 300000):

Normal (metadata) scan — reports healthy, does **not** repair the content bit-rot:

```
[Green  ->  Green] r3bucket/
[Green  ->  Green] r3bucket/r3obj
Healed:	0/1 objects; 1024 KiB in 1s
```

Deep (HighwayHash) scan — detects the object as degraded and **rewrites the shard to its original bytes** (SHA-256 restored to `ed82255d…`):

```
[Green  ->  Green] r3bucket/
[Yellow ->  Green] r3bucket/r3obj
Healed:	1/1 objects; 1024 KiB in 1s
```

---

**Single-drive (no parity) — unrecoverable. [Observed]** With the object stored on the single-drive `:9000` server (EC:0), corrupting the only shard makes the GET fail. The object's single shard:

```
/tmp/miniodata/r3single/r3obj/3824fe4e-d43d-4d51-abd2-65bfe4246abf/part.1
```

Complete first `s3.GetObject` attempt from the trace — **HTTP 503 `SlowDownRead`**:

```
127.0.0.1:9000 [REQUEST s3.GetObject] [2026-07-14T21:12:47.903] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /r3single/r3obj
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Amz-Sdk-Invocation-Id: c0fab490-c208-4430-8421-0a41c2141c31
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 X-Amz-Checksum-Mode: ENABLED
127.0.0.1:9000 X-Amz-Date: 20260714T211247Z
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=minioadmin/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-checksum-mode;x-amz-content-sha256;x-amz-date, Signature=2cf8647882b9cc02fc3f66df6b1a5930f41aec434924764235275d592be3e8ab
127.0.0.1:9000 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/Z,b,N,D,e cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T21:12:47.944] [ Duration 40.403ms TTFB 40.33872ms ↑ 151 B  ↓ 368 B ]
127.0.0.1:9000 503 Service Unavailable
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 ETag: "465fd2e7c5da206f67599627d9d4f081"
127.0.0.1:9000 Last-Modified: Tue, 14 Jul 2026 21:12:22 GMT
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Length: 368
127.0.0.1:9000 Retry-After: 60
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 X-Amz-Request-Id: 18C2445CD5A20725
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SlowDownRead</Code><Message>Resource requested is unreadable, please reduce your request rate</Message><Key>r3obj</Key><BucketName>r3single</BucketName><Resource>/r3single/r3obj</Resource><RequestId>18C2445CD5A20725</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
127.0.0.1:9000
```

botocore's legacy retry mode re-issued the request 5 times (`attempt=1..5`); the response-status tally across the complete trace was:

```
      5 503 Service Unavailable
```

This GET, too, produced 0 new server-log lines.

### Responsible Code

**[Source-grounded]** anchors, verified at HEAD:

- **The sentinel error** — `cmd/storage-errors.go:104`:
  ```go
  // errFileCorrupt - file has an unexpected size, or is not readable
  var errFileCorrupt = StorageErr("file is corrupted")
  ```
- **Per-shard HighwayHash verification on read** — `streamingBitrotReader.ReadAt` at `cmd/bitrot-streaming.go:150`; the mismatch and error return at `cmd/bitrot-streaming.go:184-185`:
  ```go
  b.h.Write(buf)
  if !bytes.Equal(b.h.Sum(nil), b.hashBytes) {
      return 0, errFileCorrupt
  }
  ```
- **Corruption detection + reconstruction during decode** — `parallelReader.Read` at `cmd/erasure-decode.go:127`. The heal flag is declared at `:154` (`bitrotHeal := int32(0)`); on a corrupt shard it is set and another shard is read at `cmd/erasure-decode.go:197-198`:
  ```go
  case errors.Is(err, errFileCorrupt):
      atomic.StoreInt32(&bitrotHeal, 1)
  ```
  and once enough shards decode, the buffer is returned **with** `errFileCorrupt` to signal a heal at `cmd/erasure-decode.go:227-228`:
  ```go
  } else if bitrotHeal == 1 {
      return newBuf, errFileCorrupt
  }
  ```
- **The GET driver + async MRF heal** — `erasureObjects.getObjectWithFileInfo` at `cmd/erasure-object.go:307`. When the decode returns `errFileCorrupt` but the client has been fully served, it queues a heal once at `cmd/erasure-object.go:399-409`:
  ```go
  healOnce.Do(func() {
      globalMRFState.addPartialOp(PartialOperation{
          Bucket:     bucket,
          Object:     object,
          VersionID:  fi.VersionID,
          Queued:     time.Now(),
          SetIndex:   er.setIndex,
          PoolIndex:  er.poolIndex,
          BitrotScan: errors.Is(err, errFileCorrupt),
      })
  })
  ```
  There is **no `logger.*` call anywhere on this detection path** (verified: `grep -n 'logger\.' cmd/bitrot-streaming.go cmd/erasure-decode.go` returns nothing) — which is why the detection is silent in the server log and audit log.

### Rationale

**[Observed] + [Source-grounded]** The HighwayHash written next to each shard on upload is recomputed on every read (`bitrot-streaming.go:184`). A manual `dd` overwrite changes the shard's data but not its stored hash, so the compare fails and `ReadAt` returns `errFileCorrupt`. On a redundant set, `parallelReader` treats that shard as unusable, reads a parity shard instead (`erasure-decode.go:197-198`), reconstructs the block, and returns it to the client while flagging a heal (`:227-228`) — hence the observed **200 with correct bytes** and the extra `ReadFileStream`. The heal queued by `getObjectWithFileInfo` (`erasure-object.go:400`) is a `mode=0` normal scan, which validates part size/existence only; since the corrupt shard has the correct size, the normal scan reports `Green → Green` and does **not** rewrite it — matching the observation that the on-disk shard stays corrupt until an explicit **deep** scan (`Yellow → Green`) recomputes the hash and repairs it. On a single drive there is no parity to reconstruct from, so the same `errFileCorrupt` surfaces to the client as `503 SlowDownRead`. Because no code on this path logs, the "specific runtime logs generated during a subsequent get request" are, on a redundant set, **none at the error-log level** — the evidence is the reconstructed 200 and the asynchronous `heal.Object` trace entry.

---

## R4 — STS session-policy enforcement

> **Question (verbatim):** "I want you to verify that when a user gets temporary credentials, minio is able to enforce the session policy on that user. You need to give me runtime test output to prove this behavior."

### Direct Answer

**[Observed]** Temporary credentials issued by STS `AssumeRole` are constrained to the **intersection** of the parent user's policy and the inline session policy — the session policy can only *narrow*, never *widen*, the parent's permissions. Proven with a single, fully correlated session:

- The parent user `r4parentuser` (a non-admin) has a broad policy allowing **both** `s3:GetObject` and `s3:PutObject`; with its long-term credentials, both a GET and a PUT succeed (HTTP 200).
- One `AssumeRole` call, carrying an inline session policy that allows **only** `s3:GetObject`, returns one temporary credential — `AccessKeyId = MYLCK9PTJE73DS0ADU0Y`. The session token is a JWT whose decoded **`accessKey` claim equals that same `MYLCK9PTJE73DS0ADU0Y`** and whose `parent` claim is `r4parentuser`.
- Using that one temporary credential: **GetObject succeeds (200)** — allowed by both parent and session — while **PutObject is denied with `403 AccessDenied`** — allowed by the parent but omitted from the session policy. Both S3 requests are signed with the identical temporary `AccessKeyId`.

Thus MinIO **does** enforce the session policy: the parent-allowed-but-session-omitted action (`PutObject`) is refused, demonstrating the intersection. Additionally, an inline session policy larger than 2,048 bytes is rejected at issuance (`400 InvalidParameterValue`).

### Reproduction

`OUT=$INV/out/r4`, STS/S3 endpoint `http://127.0.0.1:9000`. The parent is a **non-admin** user so that a denial is meaningful (the root account would bypass policy).

Parent policy `r4parentpolicy.json` (broad — Get + Put + List):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:ListBucket"],
      "Resource": ["arn:aws:s3:::r4bucket", "arn:aws:s3:::r4bucket/*"]
    }
  ]
}
```

Inline session policy `r4sessionpolicy.json` (narrow — Get only, **no** Put):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject"],
      "Resource": ["arn:aws:s3:::r4bucket/*"]
    }
  ]
}
```

```bash
$MC mb -p inv9000/r4bucket
printf 'r4-seed-object-body' | $MC pipe inv9000/r4bucket/r4obj
$MC admin policy create inv9000 r4parent $INV/policies/r4parentpolicy.json
$MC admin user add inv9000 r4parentuser <REDACTED_USER_SECRET>
$MC admin policy attach inv9000 r4parent --user r4parentuser
# then run the driver subcommands below
```

Driver `$INV/scripts/r4_sts_test.py` (boto3; `assume` decodes the JWT session token's middle segment to read the `accessKey` claim without needing the signing secret):

```python
import sys, json, base64, hashlib, boto3
from botocore.config import Config
EP="http://127.0.0.1:9000"
def s3c(ak, sk, tok=None):
    return boto3.client("s3", endpoint_url=EP, aws_access_key_id=ak, aws_secret_access_key=sk,
                        aws_session_token=tok, region_name="us-east-1",
                        config=Config(signature_version="s3v4", retries={"max_attempts": 0}))
def s3err(e):
    r = getattr(e, "response", {}) or {}
    err = r.get("Error", {}); meta = r.get("ResponseMetadata", {})
    print("  S3_Code    =", err.get("Code"))
    print("  S3_Message =", err.get("Message"))
    print("  HTTPStatus =", meta.get("HTTPStatusCode"))

cmd = sys.argv[1]
if cmd == "parent_baseline":
    ak, sk = sys.argv[2:4]
    c = s3c(ak, sk)
    r = c.get_object(Bucket="r4bucket", Key="r4obj")
    print("PARENT GetObject  -> HTTP", r["ResponseMetadata"]["HTTPStatusCode"], "body=", r["Body"].read().decode())
    r = c.put_object(Bucket="r4bucket", Key="r4obj_parent_put", Body=b"parent-can-put")
    print("PARENT PutObject  -> HTTP", r["ResponseMetadata"]["HTTPStatusCode"])

elif cmd == "assume":
    ak, sk, polfile = sys.argv[2:5]
    sts = boto3.client("sts", endpoint_url=EP, aws_access_key_id=ak, aws_secret_access_key=sk,
                       region_name="us-east-1", config=Config(signature_version="s3v4"))
    pol = open(polfile).read()
    resp = sts.assume_role(RoleArn="arn:aws:iam::minio:role/dummy", RoleSessionName="r4session",
                           Policy=pol, DurationSeconds=900)
    c = resp["Credentials"]
    print("ASSUMEROLE_HTTP    =", resp["ResponseMetadata"]["HTTPStatusCode"])
    print("TEMP_ACCESSKEYID   =", c["AccessKeyId"])
    print("TEMP_SECRETKEY     =", c["SecretAccessKey"])
    print("TEMP_SESSIONTOKEN  =", c["SessionToken"])
    # decode JWT payload (middle segment) WITHOUT verifying signature (read-only inspection)
    payload = c["SessionToken"].split(".")[1]
    payload += "=" * (-len(payload) % 4)
    claims = json.loads(base64.urlsafe_b64decode(payload))
    print("JWT_ACCESSKEY_CLAIM=", claims.get("accessKey"))
    print("JWT_PARENT_CLAIM   =", claims.get("parent"))
    print("JWT_EXP            =", claims.get("exp"))
    print("CORRELATION_MATCH  =", c["AccessKeyId"] == claims.get("accessKey"))

elif cmd == "temp_get":
    ak, sk, tok = sys.argv[2:5]
    c = s3c(ak, sk, tok)
    try:
        r = c.get_object(Bucket="r4bucket", Key="r4obj")
        print("TEMP GetObject -> HTTP", r["ResponseMetadata"]["HTTPStatusCode"], "body=", r["Body"].read().decode())
    except Exception as e:
        print("TEMP GetObject -> ERROR", type(e).__name__); s3err(e)

elif cmd == "temp_put":
    ak, sk, tok = sys.argv[2:5]
    c = s3c(ak, sk, tok)
    try:
        r = c.put_object(Bucket="r4bucket", Key="r4obj_temp_put", Body=b"temp-should-be-denied")
        print("TEMP PutObject -> HTTP", r["ResponseMetadata"]["HTTPStatusCode"])
    except Exception as e:
        print("TEMP PutObject -> ERROR", type(e).__name__); s3err(e)

elif cmd == "assume_toobig":
    ak, sk = sys.argv[2:4]
    sts = boto3.client("sts", endpoint_url=EP, aws_access_key_id=ak, aws_secret_access_key=sk,
                       region_name="us-east-1", config=Config(signature_version="s3v4"))
    # build an inline policy whose text exceeds maxSTSSessionPolicySize (2048)
    big = {"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],
           "Resource":["arn:aws:s3:::r4bucket/"+("x"*2100)+"*"]}]}
    pol = json.dumps(big)
    print("INLINE_POLICY_BYTES=", len(pol))
    try:
        resp = sts.assume_role(RoleArn="arn:aws:iam::minio:role/dummy", RoleSessionName="big",
                               Policy=pol, DurationSeconds=900)
        print("ASSUMEROLE_HTTP    =", resp["ResponseMetadata"]["HTTPStatusCode"], "(unexpected success)")
    except Exception as e:
        r = getattr(e, "response", {}) or {}
        print("ASSUMEROLE -> ERROR", type(e).__name__)
        print("  Code    =", r.get("Error", {}).get("Code"))
        print("  Message =", r.get("Error", {}).get("Message"))
        print("  HTTP    =", r.get("ResponseMetadata", {}).get("HTTPStatusCode"))
```

### Observed Output

**Parent baseline. [Observed]** With the parent's long-term credentials, both GET and PUT succeed:

```bash
python3 $INV/scripts/r4_sts_test.py parent_baseline r4parentuser <REDACTED_USER_SECRET>
```

```
PARENT GetObject  -> HTTP 200 body= r4-seed-object-body
PARENT PutObject  -> HTTP 200
```

**AssumeRole — one session. [Observed]** The issued temporary `AccessKeyId` equals the JWT session-token's `accessKey` claim (`CORRELATION_MATCH = True`); the secret is redacted (ephemeral, torn down):

```bash
python3 $INV/scripts/r4_sts_test.py assume r4parentuser <REDACTED_USER_SECRET> $INV/policies/r4sessionpolicy.json
```

```
ASSUMEROLE_HTTP    = 200
TEMP_ACCESSKEYID   = MYLCK9PTJE73DS0ADU0Y
TEMP_SECRETKEY     = <redacted — ephemeral 900s secret, torn down>
TEMP_SESSIONTOKEN  = <REDACTED_SESSION_TOKEN_JWT>
JWT_ACCESSKEY_CLAIM= MYLCK9PTJE73DS0ADU0Y
JWT_PARENT_CLAIM   = r4parentuser
JWT_EXP            = 1784064944
CORRELATION_MATCH  = True
```

The `accessKey` claim is set by `JWTSignWithAccessKey` (see *Responsible Code*), so the JWT identity is by construction identical to the returned `AccessKeyId` — here both are `MYLCK9PTJE73DS0ADU0Y`. The token also embeds the base64 `sessionPolicy` claim.

**Temporary credential — GetObject (session allows). [Observed]** HTTP 200:

```
TEMP GetObject -> HTTP 200 body= r4-seed-object-body
```

Complete `s3.GetObject` trace — note `Credential=MYLCK9PTJE73DS0ADU0Y…` and `X-Amz-Security-Token` (the session JWT):

```
127.0.0.1:9000 [REQUEST s3.GetObject] [2026-07-14T21:21:16.677] [Client IP: 127.0.0.1]
127.0.0.1:9000 GET /r4bucket/r4obj
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 X-Amz-Checksum-Mode: ENABLED
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Amz-Sdk-Invocation-Id: 8dc37e98-741f-41e1-99cc-dc96f4a692cf
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=MYLCK9PTJE73DS0ADU0Y/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-checksum-mode;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=c496220c971fb453911aa555b7d7e937bda59c566a982e5e86f917c4cd06ce9e
127.0.0.1:9000 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/N,b,D,Z,e cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9000 X-Amz-Date: 20260714T212116Z
127.0.0.1:9000 X-Amz-Security-Token: <REDACTED_SESSION_TOKEN_JWT>
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T21:21:16.678] [ Duration 916µs TTFB 873.205µs ↑ 172 B  ↓ 19 B ]
127.0.0.1:9000 200 OK
127.0.0.1:9000 Content-Type: application/octet-stream
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 ETag: "5005dd88afb6774d7f082f7b3825320f"
127.0.0.1:9000 Last-Modified: Tue, 14 Jul 2026 21:20:06 GMT
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Request-Id: 18C244D34AE3D4AF
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 19
127.0.0.1:9000 <BLOB>
127.0.0.1:9000
```

**Temporary credential — PutObject (parent allows, session omits). [Observed]** HTTP 403 `AccessDenied`:

```
TEMP PutObject -> ERROR AccessDenied
  S3_Code    = AccessDenied
  S3_Message = Access Denied.
  HTTPStatus = 403
```

Complete `s3.PutObject` trace — the **same** `Credential=MYLCK9PTJE73DS0ADU0Y…` and session token, denied with `403`:

```
127.0.0.1:9000 [REQUEST s3.PutObject] [2026-07-14T21:21:21.500] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /r4bucket/r4obj_temp_put
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 X-Amz-Date: 20260714T212121Z
127.0.0.1:9000 X-Amz-Sdk-Checksum-Algorithm: CRC32
127.0.0.1:9000 X-Amz-Security-Token: <REDACTED_SESSION_TOKEN_JWT>
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Amz-Sdk-Invocation-Id: 2a139e36-bc60-437f-80ac-2edfb296dba7
127.0.0.1:9000 Amz-Sdk-Request: attempt=1
127.0.0.1:9000 Content-Length: 21
127.0.0.1:9000 Expect: 100-continue
127.0.0.1:9000 User-Agent: Boto3/1.43.47 md/Botocore#1.43.47 ua/2.1 os/linux#6.6.122+ md/arch#x86_64 lang/python#3.13.7 md/pyimpl#CPython m/N,e,D,Z,U,b cfg/retry-mode#legacy Botocore/1.43.47
127.0.0.1:9000 X-Amz-Checksum-Crc32: 7/gCLA==
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=MYLCK9PTJE73DS0ADU0Y/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-checksum-crc32;x-amz-content-sha256;x-amz-date;x-amz-sdk-checksum-algorithm;x-amz-security-token, Signature=dd6722c6932a9bc4cc59d8dc4c3a56bb07fdca84b037945070d554f320f5b0f9
127.0.0.1:9000 X-Amz-Content-Sha256: 73f750ce341070e30c196424e563bd324367a4af1011fdbc43952fb228d5252b
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T21:21:21.501] [ Duration 543µs TTFB 500.136µs ↑ 209 B  ↓ 335 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 335
127.0.0.1:9000 Content-Type: application/xml
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 X-Ratelimit-Limit: 1140320
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C244D46A5FBC6F
127.0.0.1:9000 X-Ratelimit-Remaining: 1140320
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 <?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Key>r4obj_temp_put</Key><BucketName>r4bucket</BucketName><Resource>/r4bucket/r4obj_temp_put</Resource><RequestId>18C244D46A5FBC6F</RequestId><HostId>dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8</HostId></Error>
127.0.0.1:9000
```

**Correlation of the single session. [Observed]** The one temporary `AccessKeyId` `MYLCK9PTJE73DS0ADU0Y` appears identically in all four places:

```
(a) AssumeRole issuance : TEMP_ACCESSKEYID    = MYLCK9PTJE73DS0ADU0Y
(b) JWT accessKey claim : JWT_ACCESSKEY_CLAIM = MYLCK9PTJE73DS0ADU0Y
(c) GetObject request   : Credential=MYLCK9PTJE73DS0ADU0Y/20260714/us-east-1/s3/aws4_request
(d) PutObject request   : Credential=MYLCK9PTJE73DS0ADU0Y/20260714/us-east-1/s3/aws4_request
```

**Inline session-policy size cap (2,048 bytes). [Observed]** An inline policy of 2,230 bytes is rejected at `AssumeRole`:

```bash
python3 $INV/scripts/r4_sts_test.py assume_toobig r4parentuser <REDACTED_USER_SECRET>
```

```
INLINE_POLICY_BYTES= 2230
ASSUMEROLE -> ERROR ClientError
  Code    = InvalidParameterValue
  Message = Session policy should not exceed 2048 characters
  HTTP    = 400
```

### Responsible Code

**[Source-grounded]** anchors, verified at HEAD:

- **STS issuance** — `stsAPIHandlers.AssumeRole` at `cmd/sts-handlers.go:256`. The inline session-policy size cap `maxSTSSessionPolicySize = 2048` is defined at `cmd/sts-handlers.go:89` and enforced at `cmd/sts-handlers.go:123-124`:
  ```go
  if len(policyBuf) > maxSTSSessionPolicySize {
      return errSessionPolicyTooLarge
  }
  ```
  (`stsRequestBodyLimit = 10 MiB` at `cmd/sts-handlers.go:67`.)
- **Token identity** — `JWTSignWithAccessKey` at `internal/auth/credentials.go:339` sets the `accessKey` claim to the credential's own access key at `internal/auth/credentials.go:340`, so the JWT identity is by construction equal to the returned `AccessKeyId`:
  ```go
  func JWTSignWithAccessKey(accessKey string, m map[string]interface{}, tokenSecret string) (string, error) {
      m["accessKey"] = accessKey
  ```
  (called from `CreateNewCredentialsWithMetadata` at `internal/auth/credentials.go:306`.)
- **Enforcement (intersection)** — `IAMSys.IsAllowedSTS` at `cmd/iam.go:2242`. When an inline session policy is present, the effective decision is the logical AND of the session policy and the parent/combined policy, at `cmd/iam.go:2310-2312`:
  ```go
  hasSessionPolicy, isAllowedSP := isAllowedBySessionPolicy(args)
  if hasSessionPolicy {
      return isAllowedSP && (isOwnerDerived || combinedPolicy.IsAllowed(args))
  }
  ```

### Rationale

**[Observed] + [Source-grounded]** The `return isAllowedSP && (… combinedPolicy.IsAllowed(args))` at `cmd/iam.go:2312` is the intersection: a request is allowed only if **both** the inline session policy (`isAllowedSP`) **and** the parent/combined policy allow it. `PutObject` is allowed by the parent (baseline PUT → 200) but is absent from the session policy, so `isAllowedSP` is false and the temporary credential's PUT is denied (`403 AccessDenied`); `GetObject` is present in both, so it succeeds (200). Because `JWTSignWithAccessKey` (`credentials.go:340`) stamps the JWT `accessKey` claim with the credential's own access key, the issued `AccessKeyId`, the token's identity claim, and the `Credential=` of both S3 requests are necessarily the same value (`MYLCK9PTJE73DS0ADU0Y`) — a single credential exercised end-to-end, not a splice of separate sessions. The 2,048-byte cap (`sts-handlers.go:123-124`) further bounds how large an inline session policy may be.

---

## R5 — Privilege-escalation prevention via user mappings

> **Question (verbatim):** "Show me test output to prove that a user with basic access cannot promote themselves to a console admin by modifying the user mappings. Identify the root cause of the user mappings modification behavior that you observe."

### Direct Answer

A basic (non-admin) user **cannot** self-promote to console admin. Using only the basic user's own credentials, both IAM policy-mapping admin APIs — `SetPolicyForUserOrGroup` (`PUT /minio/admin/v3/set-user-or-group-policy`) and `AttachDetachPolicyBuiltin` (`POST /minio/admin/v3/idp/builtin/policy/attach`) — reject the attempt to attach `consoleAdmin` with **HTTP 403 `AccessDenied`**, and the user→policy mapping in the IAM store is **unchanged** afterward.

**Root cause (observed at the authorization gate).** Both handlers begin with `validateAdminReq(...)` (`cmd/admin-handlers-users.go:1770` and `:1908`), which routes each required admin action through `checkAdminRequestAuth` (`cmd/auth-handler.go:189`). For a canonical, correctly-signed request the credential is authenticated by `validateAdminSignature` (`cmd/auth-handler.go:159`) — its `X-Amz-Content-Sha256` gate at `:163` is satisfied — so evaluation **reaches** `globalIAMSys.IsAllowed` (`cmd/auth-handler.go:194`) for the required admin action (`AttachPolicyAdminAction` / `UpdatePolicyAssociationAction`). Because `r5basic` was granted only S3 actions, `IsAllowed` returns false and the gate returns `ErrAccessDenied` at `cmd/auth-handler.go:206` — **deny-by-default authorization of the required admin action**. That the request truly reached this gate (rather than being rejected earlier at signature validation) is proven by the audit records below, which each carry `"accessKey": "r5basic"` (the credential was extracted and authenticated). The mapping-write functions `PolicyDBSet` (`cmd/admin-handlers-users.go:1849`) and `PolicyDBUpdateBuiltin` (`:1956`) are never reached.

### Reproduction

**Provisioning (root credentials — provisioning only; `r5basic` is the principal for every deny-path trigger):**

```bash
export MC_CONFIG_DIR=/tmp/blitzy_investigation/mc-config
MC=/tmp/bin/mc

# IAM policy: S3-only (GetObject/PutObject/ListBucket on r5bucket) — NO admin action
$MC mb inv9000/r5bucket
$MC admin policy create inv9000 r5basicpolicy /tmp/blitzy_investigation/policies/r5basicpolicy.json
$MC admin user   add    inv9000 r5basic '<REDACTED_USER_SECRET>'
$MC admin policy attach inv9000 r5basicpolicy --user r5basic

# mc alias that authenticates AS r5basic (used to drive Handler B canonically)
$MC alias set r5alias http://127.0.0.1:9000 r5basic '<REDACTED_USER_SECRET>'
```

**IAM policy `r5basicpolicy` (`policies/r5basicpolicy.json`) — S3-only, no admin action:**

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::r5bucket",
        "arn:aws:s3:::r5bucket/*"
      ]
    }
  ]
}
```

**Handler A driver (`scripts/r5_handler_a.py`) — canonical SigV4 via `botocore`, explicitly carrying `X-Amz-Content-Sha256` so the request reaches the IAM authorization gate:**

```python
#!/usr/bin/env python3
# Handler A driver: SetPolicyForUserOrGroup self-promotion attempt.
# Signs a CANONICAL SigV4 admin request AS r5basic (botocore SigV4Auth),
# explicitly carrying X-Amz-Content-Sha256 so the request reaches the IAM
# authorization gate (cmd/auth-handler.go:163 -> :194 IsAllowed).
import sys, json, hashlib, re
import urllib.request, urllib.error
from urllib.parse import urlencode
from botocore.auth import SigV4Auth
from botocore.awsrequest import AWSRequest
from botocore.credentials import Credentials

AK = "r5basic"
SK = "<REDACTED_USER_SECRET>"
REGION = "us-east-1"
ENDPOINT = "http://127.0.0.1:9000"

def sign_and_send(method, path, query, body):
    url = ENDPOINT + path
    if query:
        url = url + "?" + urlencode(query)
    req = AWSRequest(method=method, url=url, data=body)
    # Set payload hash header BEFORE signing so it is included in SignedHeaders.
    req.headers["X-Amz-Content-Sha256"] = hashlib.sha256(body or b"").hexdigest()
    SigV4Auth(Credentials(AK, SK), "s3", REGION).add_auth(req)
    signed = dict(req.headers)
    r = urllib.request.Request(url=url, method=method, data=(body or None))
    for k, v in signed.items():
        r.add_header(k, v)
    try:
        resp = urllib.request.urlopen(r)
        return signed, resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return signed, e.code, e.read().decode()

signed, status, rbody = sign_and_send(
    "PUT",
    "/minio/admin/v3/set-user-or-group-policy",
    {"policyName": "consoleAdmin", "userOrGroup": "r5basic", "isGroup": "false"},
    b"",
)

print("=== HANDLER A: SetPolicyForUserOrGroup (r5basic self-attach consoleAdmin) ===")
print("--- Exact signed request headers sent by client ---")
for k in sorted(signed):
    print("  %s: %s" % (k, signed[k]))
auth = signed.get("Authorization", "")
m = re.search(r"SignedHeaders=([^,]+)", auth)
sh = m.group(1) if m else ""
print("SignedHeaders               = %s" % sh)
print("X-Amz-Content-Sha256 header = %s" % signed.get("X-Amz-Content-Sha256"))
print("content-sha256 IN SignedHeaders = %s" % ("x-amz-content-sha256" in sh))
print("--- Response ---")
print("HTTP %s" % status)
print(rbody)
```

**Capture + trigger (both handlers, single run; server trace with the internal pager disabled so output flushes):**

```bash
# start verbose server trace (root alias captures r5basic's denied admin calls);
# -a=all call types (admin API calls are not under the default s3 type),
# --disable-pager so mc writes raw stdout instead of buffering in its pager
$MC admin trace -a -v --disable-pager inv9000 > out/r5_trace_full.txt 2>&1 &
sleep 2

# Handler A: SetPolicyForUserOrGroup (PUT), signed as r5basic
python3 scripts/r5_handler_a.py

# Handler B: AttachDetachPolicyBuiltin (POST), driven by canonical mc/madmin as r5basic
$MC admin policy attach r5alias consoleAdmin --user r5basic
```

### Observed Output

**BEFORE — the basic user maps only to its limited policy, and `consoleAdmin` has no user mappings.** [Observed]

```text
=== BEFORE: mc admin user info inv9000 r5basic ===
AccessKey: r5basic
Status: enabled
PolicyName: r5basicpolicy
MemberOf: []

=== BEFORE: mc admin policy entities inv9000 --user r5basic ===
Query time: 2026-07-14T21:28:53Z
User -> Policy Mappings:
  User: r5basic
    Policies:
      r5basicpolicy

=== BEFORE: mc admin policy entities inv9000 --policy consoleAdmin ===
Query time: 2026-07-14T21:28:53Z
```

**Sanity — `r5basic` is a genuine "basic access" user: it CAN perform S3 but CANNOT perform admin operations.** [Observed]

```text
$ mc cp --quiet r5hello.txt r5alias/r5bucket/r5sanity.txt   # S3 PutObject as r5basic (basic user CAN do S3)
PutObject: SUCCESS (exit 0)

$ mc admin info r5alias                                     # admin op as r5basic (basic user CANNOT admin)
mc: <ERROR> Unable to get service info. Access Denied.
```

#### Handler A — `SetPolicyForUserOrGroup` (`PUT`)

Client output. The signed request **carries `X-Amz-Content-Sha256`** and lists it in `SignedHeaders`, and the server returns `HTTP 403 AccessDenied`. [Observed]

```text
=== HANDLER A: SetPolicyForUserOrGroup (r5basic self-attach consoleAdmin) ===
--- Exact signed request headers sent by client ---
  Authorization: AWS4-HMAC-SHA256 Credential=r5basic/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=605a22170c037ee6809fe3119db5e0b2652663ad9da4a9f76dfecdb0e4cc4a7f
  X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
  X-Amz-Date: 20260714T213527Z
SignedHeaders               = host;x-amz-content-sha256;x-amz-date
X-Amz-Content-Sha256 header = e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
content-sha256 IN SignedHeaders = True
--- Response ---
HTTP 403
{"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/set-user-or-group-policy","RequestId":"18C245994BEADA38","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
```

Server trace (complete and unedited; `mc admin trace -a -v --disable-pager`). It shows `X-Amz-Content-Sha256`, `SignedHeaders=host;x-amz-content-sha256;x-amz-date`, `Credential=r5basic`, and the `403 Forbidden` / `AccessDenied` response. [Observed]

```text
127.0.0.1:9000 [REQUEST admin.SetPolicyForUserOrGroup] [2026-07-14T21:35:27.098] [Client IP: 127.0.0.1]
127.0.0.1:9000 PUT /minio/admin/v3/set-user-or-group-policy?policyName=consoleAdmin&userOrGroup=r5basic&isGroup=false
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Connection: close
127.0.0.1:9000 Content-Length: 0
127.0.0.1:9000 User-Agent: Python-urllib/3.13
127.0.0.1:9000 X-Amz-Content-Sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
127.0.0.1:9000 X-Amz-Date: 20260714T213527Z
127.0.0.1:9000 Accept-Encoding: identity
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r5basic/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=605a22170c037ee6809fe3119db5e0b2652663ad9da4a9f76dfecdb0e4cc4a7f
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T21:35:27.098] [ Duration 216µs TTFB 195.685µs ↑ 104 B  ↓ 212 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Type: application/json
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Amz-Request-Id: 18C245994BEADA38
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 Content-Length: 212
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/set-user-or-group-policy","RequestId":"18C245994BEADA38","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
```

Audit-log record (complete and unedited; the raw compact record piped through `python3 -m json.tool` for readability — one valid JSON object). The **`"accessKey": "r5basic"`** field proves the credential was authenticated and the request reached `IsAllowed`; `requestID` matches the trace above (`18C245994BEADA38`). [Observed]

```json
{
    "version": "1",
    "deploymentid": "b3de8c20-51f3-4954-9a06-c02b2323ff40",
    "time": "2026-07-14T21:35:27.098339681Z",
    "event": "",
    "trigger": "incoming",
    "api": {
        "name": "SetPolicyForUserOrGroup",
        "status": "Forbidden",
        "statusCode": 403,
        "rx": 0,
        "tx": 212,
        "txHeaders": 352,
        "timeToFirstByte": "195685ns",
        "timeToFirstByteInNS": "195685",
        "timeToResponse": "203597ns",
        "timeToResponseInNS": "203597"
    },
    "remotehost": "127.0.0.1",
    "requestID": "18C245994BEADA38",
    "userAgent": "Python-urllib/3.13",
    "requestPath": "/minio/admin/v3/set-user-or-group-policy",
    "requestHost": "127.0.0.1:9000",
    "requestQuery": {
        "isGroup": "false",
        "policyName": "consoleAdmin",
        "userOrGroup": "r5basic"
    },
    "requestHeader": {
        "Accept-Encoding": "identity",
        "Authorization": "AWS4-HMAC-SHA256 Credential=r5basic/20260714/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=605a22170c037ee6809fe3119db5e0b2652663ad9da4a9f76dfecdb0e4cc4a7f",
        "Connection": "close",
        "Content-Length": "0",
        "User-Agent": "Python-urllib/3.13",
        "X-Amz-Content-Sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
        "X-Amz-Date": "20260714T213527Z"
    },
    "responseHeader": {
        "Accept-Ranges": "bytes",
        "Content-Length": "212",
        "Content-Type": "application/json",
        "Server": "MinIO",
        "Strict-Transport-Security": "max-age=31536000; includeSubDomains",
        "Vary": "Origin,Accept-Encoding",
        "X-Amz-Id-2": "dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8",
        "X-Amz-Request-Id": "18C245994BEADA38",
        "X-Content-Type-Options": "nosniff",
        "X-Xss-Protection": "1; mode=block"
    },
    "accessKey": "r5basic"
}
```

#### Handler B — `AttachDetachPolicyBuiltin` (`POST`, `mc`/madmin)

Client output. [Observed]

```text
$ mc admin policy attach r5alias consoleAdmin --user r5basic
mc: <ERROR> Unable to make user/group policy association. Access Denied.
```

Server trace (complete and unedited). The canonical `mc`/madmin request uses `Content-Type: application/octet-stream` with an encrypted `PolicyAssociationReq` body (`Content-Length: 103`), carries `X-Amz-Content-Sha256`, and returns `403 Forbidden` / `AccessDenied`. [Observed]

```text
127.0.0.1:9000 [REQUEST admin.AttachDetachPolicyBuiltin] [2026-07-14T21:35:27.199] [Client IP: 127.0.0.1]
127.0.0.1:9000 POST /minio/admin/v3/idp/builtin/policy/attach
127.0.0.1:9000 Proto: HTTP/1.1
127.0.0.1:9000 Host: 127.0.0.1:9000
127.0.0.1:9000 Authorization: AWS4-HMAC-SHA256 Credential=r5basic/20260714//s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=748e9ac3cb29714a8ca4d515215c00b16e06264867fce69e309084813c47e2e5
127.0.0.1:9000 Content-Length: 103
127.0.0.1:9000 Content-Type: application/octet-stream
127.0.0.1:9000 User-Agent: MinIO (linux; amd64) madmin-go/3.0.70 mc/RELEASE.2025-08-13T08-35-41Z
127.0.0.1:9000 X-Amz-Content-Sha256: 64ce83f3bda47428e4013cdf871641edef485a7190e8dad62bbfc8dda2226969
127.0.0.1:9000 X-Amz-Date: 20260714T213527Z
127.0.0.1:9000 Accept-Encoding: zstd,gzip
127.0.0.1:9000 <BLOB>
127.0.0.1:9000 [RESPONSE] [2026-07-14T21:35:27.200] [ Duration 340µs TTFB 313.468µs ↑ 106 B  ↓ 213 B ]
127.0.0.1:9000 403 Forbidden
127.0.0.1:9000 X-Amz-Id-2: dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8
127.0.0.1:9000 X-Xss-Protection: 1; mode=block
127.0.0.1:9000 Content-Type: application/json
127.0.0.1:9000 Server: MinIO
127.0.0.1:9000 X-Amz-Request-Id: 18C2459951F83BB4
127.0.0.1:9000 X-Content-Type-Options: nosniff
127.0.0.1:9000 Accept-Ranges: bytes
127.0.0.1:9000 Content-Length: 213
127.0.0.1:9000 Strict-Transport-Security: max-age=31536000; includeSubDomains
127.0.0.1:9000 Vary: Origin,Accept-Encoding
127.0.0.1:9000 {"Code":"AccessDenied","Message":"Access Denied.","Resource":"/minio/admin/v3/idp/builtin/policy/attach","RequestId":"18C2459951F83BB4","HostId":"dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8"}
```

Audit-log record (complete and unedited; piped through `python3 -m json.tool`). Again **`"accessKey": "r5basic"`**, and `requestID` matches the trace (`18C2459951F83BB4`). [Observed]

```json
{
    "version": "1",
    "deploymentid": "b3de8c20-51f3-4954-9a06-c02b2323ff40",
    "time": "2026-07-14T21:35:27.199998297Z",
    "event": "",
    "trigger": "incoming",
    "api": {
        "name": "AttachDetachPolicyBuiltin",
        "status": "Forbidden",
        "statusCode": 403,
        "rx": 103,
        "tx": 213,
        "txHeaders": 352,
        "timeToFirstByte": "313468ns",
        "timeToFirstByteInNS": "313468",
        "timeToResponse": "321910ns",
        "timeToResponseInNS": "321910"
    },
    "remotehost": "127.0.0.1",
    "requestID": "18C2459951F83BB4",
    "userAgent": "MinIO (linux; amd64) madmin-go/3.0.70 mc/RELEASE.2025-08-13T08-35-41Z",
    "requestPath": "/minio/admin/v3/idp/builtin/policy/attach",
    "requestHost": "127.0.0.1:9000",
    "requestHeader": {
        "Accept-Encoding": "zstd,gzip",
        "Authorization": "AWS4-HMAC-SHA256 Credential=r5basic/20260714//s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=748e9ac3cb29714a8ca4d515215c00b16e06264867fce69e309084813c47e2e5",
        "Content-Length": "103",
        "Content-Type": "application/octet-stream",
        "User-Agent": "MinIO (linux; amd64) madmin-go/3.0.70 mc/RELEASE.2025-08-13T08-35-41Z",
        "X-Amz-Content-Sha256": "64ce83f3bda47428e4013cdf871641edef485a7190e8dad62bbfc8dda2226969",
        "X-Amz-Date": "20260714T213527Z"
    },
    "responseHeader": {
        "Accept-Ranges": "bytes",
        "Content-Length": "213",
        "Content-Type": "application/json",
        "Server": "MinIO",
        "Strict-Transport-Security": "max-age=31536000; includeSubDomains",
        "Vary": "Origin,Accept-Encoding",
        "X-Amz-Id-2": "dd9025bab4ad464b049177c95eb6ebf374d3b3fd1af9251148b658df7ac2e3e8",
        "X-Amz-Request-Id": "18C2459951F83BB4",
        "X-Content-Type-Options": "nosniff",
        "X-Xss-Protection": "1; mode=block"
    },
    "accessKey": "r5basic"
}
```

**AFTER — the mapping is unchanged** (self-promotion failed; the IAM store was not modified). [Observed]

```text
=== AFTER: mapping unchanged ===
$ mc admin user info inv9000 r5basic
AccessKey: r5basic
Status: enabled
PolicyName: r5basicpolicy
MemberOf: []

$ mc admin policy entities inv9000 --user r5basic
Query time: 2026-07-14T21:35:58Z
User -> Policy Mappings:
  User: r5basic
    Policies:
      r5basicpolicy

$ mc admin policy entities inv9000 --policy consoleAdmin
Query time: 2026-07-14T21:35:58Z
```

`r5basic` still maps only to `r5basicpolicy`, and `consoleAdmin` still has **no** user mappings — `r5basic` did not attach itself.

**Server error log during the attempts (`mc admin logs`).** [Observed]

```text
$ mc admin logs --last 20 inv9000
(captured 8 lines)
 API: SYSTEM.config()
 Time: 20:33:49 UTC 07/14/2026
 DeploymentID: b3de8c20-51f3-4954-9a06-c02b2323ff40
 Error: unable to send audit/log entry(s) to 'minio-http-audit-inv' err 'http://127.0.0.1:9999 returned 'Post "http://127.0.0.1:9999": dial tcp 127.0.0.1:9999: connect: connection refused', please check your endpoint configuration': 1 (*fmt.wrapError)
        4: /tmp/blitzy/minio/blitzy-588df31b-e44d-4dd8-a545-973febe20ac8_002d7b/internal/logger/logonce.go:64:logger.(*logOnceType).logOnceConsoleIf()
        3: /tmp/blitzy/minio/blitzy-588df31b-e44d-4dd8-a545-973febe20ac8_002d7b/internal/logger/logonce.go:157:logger.LogOnceConsoleIf()
        2: /tmp/blitzy/minio/blitzy-588df31b-e44d-4dd8-a545-973febe20ac8_002d7b/cmd/logging.go:132:cmd.configLogOnceConsoleIf()
        1: /tmp/blitzy/minio/blitzy-588df31b-e44d-4dd8-a545-973febe20ac8_002d7b/internal/logger/target/http/http.go:442:http.(*Target).startQueueProcessor()
```

The only entry is an **unrelated** audit-webhook connectivity warning stamped `20:33:49` — roughly an hour before this test window (`21:35`), from a brief moment when the audit receiver was not yet listening at startup. There is **no** server error-log entry for either `403` denial: authorization denials are surfaced through the HTTP response and the audit log (both shown above), not the server error log. [Observed]

### Responsible Code (and Root Cause)

- **Routes (both wrapped in `adminMiddleware`)** — `cmd/admin-router.go:264` registers `PUT .../set-user-or-group-policy` → `adminMiddleware(adminAPI.SetPolicyForUserOrGroup)`; `cmd/admin-router.go:268` registers `POST .../idp/builtin/policy/{operation}` → `adminMiddleware(adminAPI.AttachDetachPolicyBuiltin)`.
- **Handler A** — `SetPolicyForUserOrGroup` at `cmd/admin-handlers-users.go:1770`; its **first** statement is `objectAPI, _ := validateAdminReq(ctx, w, r, policy.AttachPolicyAdminAction)` (`:1773`). The mapping write it would perform, `PolicyDBSet` (`:1849`), runs only after `validateAdminReq` succeeds.
- **Handler B** — `AttachDetachPolicyBuiltin` at `cmd/admin-handlers-users.go:1908`; its first statement is `objectAPI, cred := validateAdminReq(ctx, w, r, policy.UpdatePolicyAssociationAction, policy.AttachPolicyAdminAction)` (`:1911`). Its body parsing (octet-stream check `:1926`, `madmin.DecryptData` `:1939`, `PolicyAssociationReq` unmarshal `:1945`) and the mapping write `PolicyDBUpdateBuiltin` (`:1956`) all run only after `validateAdminReq` succeeds.
- **Admin-action gate** — `validateAdminReq` at `cmd/admin-handler-utils.go:37` loops over the required admin actions, calling `checkAdminRequestAuth` (`:47`) for each; on `ErrAccessDenied` it tries the next action, and after all are denied it writes `ErrAccessDenied` (`:60`) and returns a `nil` `ObjectLayer`, so the handler aborts before any body parsing or mapping write:

```go
func validateAdminReq(ctx context.Context, w http.ResponseWriter, r *http.Request, actions ...policy.AdminAction) (ObjectLayer, auth.Credentials) {
	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return nil, auth.Credentials{}
	}

	for _, action := range actions {
		// Validate request signature.
		cred, adminAPIErr := checkAdminRequestAuth(ctx, r, action, "")
		switch adminAPIErr {
		case ErrNone:
			return objectAPI, cred
		case ErrAccessDenied:
			// Try another
			continue
		default:
			writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(adminAPIErr), r.URL)
			return nil, cred
		}
	}
	writeErrorResponseJSON(ctx, w, errorCodes.ToAPIErr(ErrAccessDenied), r.URL)
	return nil, auth.Credentials{}
}
```

- **The credential is authenticated first, then authorized** — `checkAdminRequestAuth` at `cmd/auth-handler.go:189` calls `validateAdminSignature` (`:190`), and only if that returns `ErrNone` does it evaluate the admin action via `globalIAMSys.IsAllowed` (`:194`), returning `ErrAccessDenied` (`:206`) when the action is not granted:

```go
func checkAdminRequestAuth(ctx context.Context, r *http.Request, action policy.AdminAction, region string) (auth.Credentials, APIErrorCode) {
	cred, owner, s3Err := validateAdminSignature(ctx, r, region)
	if s3Err != ErrNone {
		return cred, s3Err
	}
	if globalIAMSys.IsAllowed(policy.Args{
		AccountName:     cred.AccessKey,
		Groups:          cred.Groups,
		Action:          policy.Action(action),
		ConditionValues: getConditionValues(r, "", cred),
		IsOwner:         owner,
		Claims:          cred.Claims,
	}) {
		// Request is allowed return the appropriate access key.
		return cred, ErrNone
	}

	return cred, ErrAccessDenied
}
```

- **Why the canonical request reaches `IsAllowed`** — `validateAdminSignature` (`cmd/auth-handler.go:159`) pre-sets `s3Err := ErrAccessDenied` (`:162`) and authenticates the credential **only inside** the `X-Amz-Content-Sha256` gate at `:163`:

```go
func validateAdminSignature(ctx context.Context, r *http.Request, region string) (auth.Credentials, bool, APIErrorCode) {
	var cred auth.Credentials
	var owner bool
	s3Err := ErrAccessDenied
	if _, ok := r.Header[xhttp.AmzContentSha256]; ok &&
		getRequestAuthType(r) == authTypeSigned {

		// Get credential information from the request.
		cred, owner, s3Err = getReqAccessKeyV4(r, region, serviceS3)
		if s3Err != ErrNone {
			return cred, owner, s3Err
		}

		// we only support V4 (no presign) with auth body
		s3Err = isReqAuthenticated(ctx, r, region, serviceS3)
	}
	if s3Err != ErrNone {
		return cred, owner, s3Err
	}

	logger.GetReqInfo(ctx).Cred = cred
	logger.GetReqInfo(ctx).Owner = owner
	logger.GetReqInfo(ctx).Region = globalSite.Region()

	return cred, owner, ErrNone
}
```

  A request that omits `X-Amz-Content-Sha256` skips this block entirely and is rejected with the pre-set `ErrAccessDenied` **without the credential ever being extracted** — its audit record would carry no `accessKey`. The canonical requests here **do** carry `X-Amz-Content-Sha256` (visible in every trace/audit `requestHeader` and in each `SignedHeaders` list), so `r5basic` is extracted and authenticated (`getReqAccessKeyV4` `:167`, `isReqAuthenticated` `:173`), `ReqInfo.Cred` is stamped (`:179`) — hence `"accessKey": "r5basic"` appears in both audit records — and control reaches `globalIAMSys.IsAllowed` (`cmd/auth-handler.go:194`).
- **Root cause** — because `r5basic`'s policy grants **no** admin action, `globalIAMSys.IsAllowed` returns false for both `AttachPolicyAdminAction` and `UpdatePolicyAssociationAction`, and `checkAdminRequestAuth` returns `ErrAccessDenied` at `cmd/auth-handler.go:206`. This is **deny-by-default authorization of the required admin action** — the observed "cannot modify user mappings" behavior is decided at the admin auth gate, not inside the mapping code.

### Rationale

**[Observed] + [Source-grounded]** The self-promotion is blocked at the **authorization gate**, before any mapping logic runs. Both policy-mapping handlers put `validateAdminReq` first (`cmd/admin-handlers-users.go:1770` and `:1908`), which authorizes the caller for the required **admin** action via `checkAdminRequestAuth` → `globalIAMSys.IsAllowed`. The two audit records both carry `"accessKey": "r5basic"` and a `SignedHeaders` list that includes `x-amz-content-sha256`, which together prove the credential was authenticated and that evaluation reached `IsAllowed` (`cmd/auth-handler.go:194`) — the request was **not** short-circuited at signature validation. Since `r5basic` holds only S3 actions, deny-by-default evaluation returns `ErrAccessDenied` (`cmd/auth-handler.go:206`), the handler returns immediately, and neither `PolicyDBSet` (`:1849`) nor `PolicyDBUpdateBuiltin` (`:1956`) is invoked. The observed state confirms this: two `403 AccessDenied` responses and an **unchanged** user→policy mapping (`consoleAdmin` has zero user mappings after both attempts). The root cause of the observed behavior is therefore the **deny-by-default authorization of the required admin action** in the admin auth gate — not any check inside the mapping code itself.

---

## Coverage Checklist (final pass)

Every named item and variant across R1–R5 was exercised at runtime through the canonical S3/STS/admin entry point (SigV4 via `boto3`/`mc`) and captured with its complete output. **Status** is `PASS` where the behavior was demonstrated positively, or `PARTIAL` where the honest result is a deliberately-reported negative or an inference (never forced). **Class** records how each result is grounded: **Observed** (captured at runtime), **Source-grounded** (verified in the code at HEAD), or **[INFERRED]** (explained, not directly observable).

| # | Item / variant | Status | Class | Observed result |
|---|----------------|--------|-------|-----------------|
| R1 | Unencrypted PUT to SSE-S3 default bucket by broad-write user (auto-encrypt) | PASS | Observed | `200` + server-injected `X-Amz-Server-Side-Encryption: AES256`; stored `Encryption: SSE-S3` |
| R1 | Server-side header injection proven (SSE not in client `SignedHeaders`) | PASS | Observed | Trace shows `X-Amz-Server-Side-Encryption: AES256` on a request whose `SignedHeaders` omit it |
| R1 | IAM identity `Deny`-if-no-SSE — PUT without header | PASS | Observed | `403 AccessDenied` |
| R1 | IAM identity `Deny`-if-no-SSE — PUT with header | PASS | Observed | `200` (header now in `SignedHeaders`) |
| R1 | `MINIO_KMS_AUTO_ENCRYPTION` / `globalAutoEncryption` state | PASS | Observed | Unset (`false`); injection came from the bucket default rule, not auto-encryption |
| R1 | Resource-based bucket-policy `Deny` variant | PARTIAL | Observed + Source-grounded | Did **not** deny the authenticated IAM user — MinIO bucket policies gate anonymous requests; the authenticated denial requirement is delivered by the two IAM identity-`Deny` rows above (reported as observed, not remediated) |
| R2 | Delete under Legal Hold ON | PASS | Observed | `400 InvalidRequest` WORM; version retained |
| R2 | Delete under Compliance (before expiry) | PASS | Observed | `400 InvalidRequest` WORM; version retained; not bypassable by anyone |
| R2 | Delete under Governance (no bypass) | PASS | Observed | `400 InvalidRequest` WORM; version retained |
| R2 | Governance bypass header + `BypassGovernanceRetention` granted | PASS | Observed | `204 No Content`; version deleted |
| R2 | Governance bypass header + permission lacking | PASS | Observed | `403 AccessDenied` (`errAuthentication` → `ErrAccessDenied`) |
| R2 | Log surfaces: server trace + audit-log JSON | PASS | Observed | `s3.DeleteObject … 400`; audit `api.name=DeleteObject, statusCode=400` |
| R3 | 4-drive heal-on-read GET after shard corruption | PASS | Observed | `200`, sha256 == original; `heal.Object` event |
| R3 | Byte-sensitive hash verification | PASS | Observed | Healed body hash `ef3488fc…` == original; corrupt shard `3d056c7e…` differs |
| R3 | Stability ≥2 runs | PASS | Observed | 4 runs; each `200` + correct hash + 1 heal event (~285–299µs) |
| R3 | Single-drive (no parity) unrecoverable variant | PASS | Observed | `503 SlowDownRead` |
| R3 | `errFileCorrupt` surfaced to `mc admin logs` console | PARTIAL | [INFERRED] | No `mc admin logs` line for the corruption; inferred internal/by-design (heal path handles it; console log not emitted) |
| R4 | Baseline parent `PutObject` (long-term creds) | PASS | Observed | `200` (parent allows Put) |
| R4 | `AssumeRole` with narrow inline session policy | PASS | Observed | `200`; single temp cred + SessionToken issued (`MYLCK9PTJE73DS0ADU0Y`, secret redacted) |
| R4 | Temp-cred `GetObject` (session allows) | PASS | Observed | `200`, body `r4-seed-object-body` |
| R4 | Temp-cred `PutObject` (parent allows, session omits) | PASS | Observed | `403 AccessDenied` (intersection proven; same key end-to-end) |
| R4 | Inline session-policy size cap (2,048 bytes) | PASS | Observed + Source-grounded | 2,230-byte policy → `400 InvalidParameterValue` "Session policy should not exceed 2048 characters" (`sts-handlers.go:89`,`:123-124`) |
| R5 | Self-attach `consoleAdmin` via `SetPolicyForUserOrGroup` | PASS | Observed | `403 AccessDenied`; authenticated audit `accessKey=r5basic` (reached `IsAllowed`) |
| R5 | Self-attach `consoleAdmin` via `AttachDetachPolicyBuiltin` | PASS | Observed | `403 AccessDenied`; authenticated audit `accessKey=r5basic` |
| R5 | Mapping unchanged after attempts | PASS | Observed | `r5basic` → only `r5basicpolicy`; `consoleAdmin` has no user mappings |
| R5 | Root cause identified | PASS | Observed + Source-grounded | Deny-by-default admin-action eval in `checkAdminRequestAuth` (`cmd/auth-handler.go:206`), reached only after the `X-Amz-Content-Sha256` signature gate at `:163` |

**[INFERRED] statements in this report** (made only where a fact could not be directly observed, and always placed next to the directly-observed evidence they qualify): (a) R3 — the default size-based `CheckParts` heal does not rewrite a same-size corrupted shard on a plain read (only a deep-scan heal recomputes HighwayHash and repairs on-disk); (b) R3 — `errFileCorrupt` is an intentionally internal error that the erasure decode/heal path consumes, so it is not surfaced as an `mc admin logs` console line. Every other claim in this report is either **Observed** at runtime or **Source-grounded** at HEAD `c07e5b49d`.

---

## Cleanup

**Every runtime fixture was ephemeral and provisioned outside the repository tree; all of it has been
torn down, and the MinIO source tree is byte-for-byte unmodified — this branch adds exactly one file,
this report.** The investigation ran three background processes — the single-drive server (`:9000`), the
four-drive erasure server (`:9100`), and the audit-webhook sink (`:9999`) — plus two temporary data
directories and one scratch workspace. Teardown, with its actual output, follows.

**1. Stop the servers and the audit sink; confirm nothing is left running or listening.** The three
processes ran in the background for the duration of the investigation and were stopped at teardown; the
checks below confirm no process, listener, or health endpoint remains:

```bash
$ ps -eo pid,args | grep -E "minio-bin|audit_receiver.py" | grep -v grep
(no matching processes)
```

```bash
$ for p in 9000 9001 9100 9101 9999; do
    hp=$(printf '%04X' "$p")   # local port as uppercase hex, as encoded in /proc/net/tcp{,6}
    awk -v p="$p" -v hp="$hp" '$4=="0A"{n=split($2,a,":"); if(a[n]==hp) f=1} END{printf "port %s: %s\n", p, (f ? "listening" : "not listening")}' /proc/net/tcp /proc/net/tcp6
  done   # state 0A = LISTEN; scan IPv4 + IPv6 tables (the console port binds IPv6-only)
port 9000: not listening
port 9001: not listening
port 9100: not listening
port 9101: not listening
port 9999: not listening
```

```bash
$ curl -s -o /dev/null -w "%{http_code}\n" --max-time 3 http://127.0.0.1:9000/minio/health/ready
000
$ curl -s -o /dev/null -w "%{http_code}\n" --max-time 3 http://127.0.0.1:9100/minio/health/ready
000
```

**2. Remove the temporary data directories and the scratch workspace** — the latter holds the
reproduction scripts, policy JSONs, the `mc` config/aliases (the dedicated `MC_CONFIG_DIR`), and all
captured trace/audit/log output:

```bash
$ rm -rf /tmp/miniodata /tmp/minio4 /tmp/blitzy_investigation
$ ls -d /tmp/miniodata /tmp/minio4 /tmp/blitzy_investigation 2>&1
ls: cannot access '/tmp/miniodata': No such file or directory
ls: cannot access '/tmp/minio4': No such file or directory
ls: cannot access '/tmp/blitzy_investigation': No such file or directory
```

The built-in KMS key existed only as the `MINIO_KMS_SECRET_KEY` environment value passed to the `:9000`
process; it has no on-disk artifact and is gone with the process. All transient buckets, users, IAM
policies, STS credentials, and `mc` aliases lived inside the removed data directories / `MC_CONFIG_DIR`
and are gone with them.

**3. Verify the source tree is unmodified.** Relative to the investigation HEAD (`c07e5b49d477`), this
branch adds exactly one file — this report — and changes nothing under `cmd/`, `internal/`, `docs/`,
`buildscripts/`, or any build/config file. `git status --porcelain` is captured here before this
report's own finalizing commit, so the report is its only entry; the `git diff --name-status` against
the investigation HEAD shows the single added file and is stable after that commit:

```bash
$ git status --porcelain
 M blitzy/documentation/minio_c07e5b49d477.md

$ git diff --name-status c07e5b49d477b0774f23db3b290745aef8c01bd2 HEAD
A	blitzy/documentation/minio_c07e5b49d477.md
```

The pre-existing tooling installed during environment setup — `/tmp/minio-bin` (the server binary built
from HEAD) and `/tmp/bin/mc` — is left in place; neither is part of the repository. No defect that
surfaced during the investigation was remediated: per the read-only constraint, this was an
observe-and-document investigation only.
